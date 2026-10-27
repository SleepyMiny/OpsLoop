#!/usr/bin/env python3
"""Validate a pinned govulncheck JSON stream and its independent package load.

Exit 0: complete analysis, no reported findings; 3: complete with findings;
2: incomplete or inconsistent evidence. This does not certify deployed software.
"""
import argparse
import collections
from datetime import datetime, timedelta, timezone
import json
import re
from pathlib import Path


OFFICIAL_DB = 'https://vuln.go.dev'
PROTOCOL = 'v1.0.0'  # x/vuln v1.9.0 internal/govulncheck/govulncheck.go
CLOCK_SKEW_SECONDS = 300


def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(
            r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})', value):
        raise ValueError('invalid RFC3339 timestamp')
    # fromisoformat checks calendar and time ranges; explicitly check offset
    # ranges because it otherwise normalizes an invalid +00:60 offset.
    if value[-1] != 'Z' and (int(value[-5:-3]) > 23 or int(value[-2:]) > 59):
        raise ValueError('invalid RFC3339 offset')
    return datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(timezone.utc)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON key')
        result[key] = value
    return result


def decode_stream(text):
    decoder = json.JSONDecoder(object_pairs_hook=unique_object,
                               parse_constant=lambda _: (_ for _ in ()).throw(ValueError('non-JSON number')))
    values = []
    pos = 0
    while pos < len(text):
        if text[pos].isspace():
            pos += 1
            continue
        value, pos = decoder.raw_decode(text, pos)
        if not isinstance(value, dict):
            raise ValueError("expected JSON object stream")
        values.append(value)
    return values


def validate(events, *, go_version, mode, roots, exit_code, stderr, packages=None,
             started_at=None, completed_at=None, package_exit_code=None, package_stderr=None):
    errors = []
    # This is evidence validation, not authentication. Only the online wrapper
    # supplies trusted process times/status and binds them to a fresh invocation.
    try:
        start, end = timestamp(started_at), timestamp(completed_at)
        if end < start:
            raise ValueError('scan completion precedes start')
    except (ValueError, TypeError):
        raise ValueError('valid scan start/completion timestamps required')
    future_limit = end + timedelta(seconds=CLOCK_SKEW_SECONDS)
    if not events or list(events[0]) != ['config']:
        raise ValueError('config must be the first event')
    for event in events:
        if not isinstance(event, dict) or len(event) != 1:
            raise ValueError('exactly one supported top-level event required')
        kind, value = next(iter(event.items()))
        if kind not in {'config', 'SBOM', 'progress', 'osv', 'finding'} or not isinstance(value, dict):
            raise ValueError('unsupported or malformed event')
        if kind == 'progress' and not isinstance(value.get('message'), str):
            raise ValueError('malformed progress')
        if kind == 'osv' and not isinstance(value.get('id'), str):
            raise ValueError('malformed advisory')
        if kind == 'finding' and (not isinstance(value.get('osv'), str) or
                                 not isinstance(value.get('trace'), list) or
                                 any(not isinstance(f, dict) or not isinstance(f.get('module'), str)
                                     for f in value['trace'])):
            raise ValueError('malformed finding')
        for field in {'config': ['db_last_modified'], 'progress': ['time'],
                      'osv': ['modified', 'published', 'withdrawn']}.get(kind, []):
            if field in value:
                if timestamp(value[field]) > future_limit:
                    errors.append('timestamp exceeds scan completion plus clock skew')
    configs = [e['config'] for e in events if 'config' in e]
    sboms = [e['SBOM'] for e in events if 'SBOM' in e]
    if exit_code != 0:
        errors.append(f"scanner exit {exit_code}")
    # Successful JSON scans in the pinned version use stdout for progress.
    # Preserve and investigate any stderr instead of silently declaring PASS.
    if stderr.strip():
        errors.append("scanner stderr is not empty")
    if len(configs) != 1 or len(sboms) != 1:
        raise ValueError("exactly one config and SBOM required")
    config, sbom = configs[0], sboms[0]
    if go_version != 'go1.27.3':
        errors.append("expected actual stable Go version")
    if (config.get('scanner_name'), config.get('scanner_version')) != ('govulncheck', 'v1.9.0'):
        errors.append("unexpected scanner")
    if config.get('scan_mode') != mode or config.get('scan_level') != 'symbol':
        errors.append("unexpected analysis mode/level")
    # Binary mode reports the compiler in SBOM, and has no config.go_version.
    if ((mode == 'source' or 'go_version' in config) and config.get('go_version') != go_version) or sbom.get('go_version') != go_version:
        errors.append("actual Go/config/SBOM mismatch")
    if not isinstance(sbom.get('modules'), list) or any(not isinstance(m, dict) or
            not isinstance(m.get('path'), str) for m in sbom['modules']):
        raise ValueError('malformed SBOM modules')
    if not isinstance(sbom.get('roots'), list) or any(not isinstance(r, str) for r in sbom['roots']):
        raise ValueError('malformed SBOM roots')
    stdlib = [m.get('version') for m in sbom.get('modules', []) if m.get('path') == 'stdlib']
    expected_stdlib = go_version if mode == 'binary' else 'v' + go_version.removeprefix('go')
    if stdlib != [expected_stdlib]:
        errors.append("stdlib version missing or mismatched")
    if config.get('db') != OFFICIAL_DB:
        errors.append('missing or unapproved online DB')
    if config.get('protocol_version') != PROTOCOL:
        errors.append('unsupported protocol')
    if not config.get('db_last_modified'):
        errors.append('DB timestamp missing')
    package_summary = None
    actual_roots = sbom.get('roots', [])
    if not roots or set(actual_roots) != set(roots):
        errors.append("analysis targets empty, missing or unexpected")
    if mode == 'source':
        if package_exit_code != 0 or package_stderr is None or package_stderr.strip():
            errors.append('independent package load exit/stderr failure')
        if not packages:
            errors.append("independent package load empty")
        else:
            load_errors = [p.get('ImportPath') for p in packages if p.get('Error') or p.get('DepsErrors') or p.get('Incomplete')]
            loaded_roots = [p.get('ImportPath') for p in packages if not p.get('DepOnly')]
            if load_errors or set(loaded_roots) != set(roots):
                errors.append("package load errors or root mismatch")
            package_summary = {'count': len(packages), 'roots': loaded_roots, 'errors': load_errors}
        if not any('Checking the code' in e.get('progress', {}).get('message', '') for e in events):
            errors.append("symbol analysis stage missing")
    osvs = {e['osv']['id']: e['osv'] for e in events if 'osv' in e}
    counts = collections.Counter()
    ids = {level: set() for level in ['module', 'package', 'symbol']}
    findings = []
    for event in events:
        if 'finding' not in event:
            continue
        finding = event['finding']
        trace = finding.get('trace', [])
        if not trace or finding.get('osv') not in osvs:
            errors.append("finding without trace or advisory metadata")
            continue
        first = trace[0]
        level = 'symbol' if first.get('function') else 'package' if first.get('package') else 'module'
        counts[level] += 1
        ids[level].add(finding['osv'])
        findings.append(finding)
    status = 'TOOL_ERROR' if errors else 'FINDINGS' if findings else 'PASS'
    return {'status': status, 'evidence_kind': 'stream_revalidation',
            'started_at': started_at, 'completed_at': completed_at,
            'clock_skew_seconds': CLOCK_SKEW_SECONDS, 'errors': errors, 'config': config, 'sbom': sbom,
            'package_load': package_summary, 'event_count': len(events), 'osv_count': len(osvs),
            'osv_event_count': sum('osv' in event for event in events),
            'finding_events': dict(counts), 'advisory_ids_by_level': {k: sorted(v) for k, v in ids.items()},
            'unique_advisory_ids': sorted(set().union(*ids.values())), 'findings': findings}, (2 if errors else 3 if findings else 0)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('stream', type=Path)
    parser.add_argument('--go-version', required=True, help='actual go env GOVERSION; never an override')
    parser.add_argument('--mode', choices=['source', 'binary'], required=True)
    parser.add_argument('--root', action='append', default=[])
    parser.add_argument('--exit-code', type=int, required=True)
    parser.add_argument('--stderr', type=Path, required=True)
    parser.add_argument('--packages', type=Path)
    parser.add_argument('--started-at', required=True)
    parser.add_argument('--completed-at', required=True)
    parser.add_argument('--package-exit-code', type=int)
    parser.add_argument('--package-stderr', type=Path)
    args = parser.parse_args()
    try:
        result, code = validate(decode_stream(args.stream.read_text()), go_version=args.go_version,
                                mode=args.mode, roots=args.root, exit_code=args.exit_code,
                                stderr=args.stderr.read_text(),
                                packages=decode_stream(args.packages.read_text()) if args.packages else None,
                                started_at=args.started_at, completed_at=args.completed_at,
                                package_exit_code=args.package_exit_code,
                                package_stderr=args.package_stderr.read_text() if args.package_stderr else None)
    except (ValueError, KeyError, TypeError, AttributeError, OSError) as error:
        result, code = {'status': 'TOOL_ERROR', 'errors': [str(error)]}, 2
    print(json.dumps(result, indent=2))
    return code


if __name__ == '__main__':
    raise SystemExit(main())
