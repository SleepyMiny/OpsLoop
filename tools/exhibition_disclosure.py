"""Exact-hash, explicitly reviewed diff disclosure; never automatic model output.

The caller must load approved_sha256 from a separate operator-reviewed server
catalog. This check is an additional filter, not a guarantee that regexes detect
all secrets. The browser must render content as text, never HTML.
"""
import re

from tools.engineering_runner.contracts import ALLOWED_PATHS, Rejected, digest, regular_file


def public_candidate_patch(path, approved_sha256):
    if not isinstance(approved_sha256, str) or not re.fullmatch('[0-9a-f]{64}', approved_sha256):
        raise Rejected('reviewed patch digest required')
    raw = regular_file(path, 256 * 1024)
    if digest(raw) != approved_sha256:
        raise Rejected('reviewed patch digest mismatch')
    try:
        text = raw.decode('utf-8')
    except UnicodeError:
        raise Rejected('public patch must be UTF-8') from None
    if re.search(r'(?i)(-----BEGIN [A-Z ]*PRIVATE KEY|Bearer\s+\S+|sk-[A-Za-z0-9_-]{16,}|'
                 r'(?:access_token|refresh_token|api[_-]?key|password|client_secret)\s*[=:]|'
                 r'/home/[^/\s]+/|/root/|\.kube/|\.azure/)', text):
        raise Rejected('reviewed patch contains protected information')
    headers = re.findall(r'(?m)^(---|\+\+\+) ([^\n]+)$', text)
    if not headers or len(headers) % 2:
        raise Rejected('invalid public patch headers')
    seen = set()
    for i in range(0, len(headers), 2):
        old, new = headers[i:i+2]
        relative = old[1].removeprefix('a/')
        if (old != ('---', 'a/'+relative) or new != ('+++', 'b/'+relative)
                or relative not in ALLOWED_PATHS or relative in seen):
            raise Rejected('public patch outside reviewed source scope')
        seen.add(relative)
    if any(ord(c) < 32 and c not in '\n\r\t' for c in text):
        raise Rejected('public patch control characters rejected')
    return {'kind': 'diff', 'sha256': approved_sha256, 'content': text}


def load_public_artifacts(catalog_path, catalog_sha256, run_id):
    """Load a separately reviewed local catalog; no browser-controlled paths."""
    import json
    from pathlib import Path
    from tools.exhibition_control import identifier, pinned
    from tools.engineering_runner.contracts import _unique_object
    from tools.engineering_runner.live import CHECKS
    if not identifier(run_id):
        raise Rejected('invalid disclosure run')
    catalog = pinned(catalog_path, catalog_sha256)
    if (not isinstance(catalog, dict) or set(catalog) != {'schema', 'approvalReference', 'entries'}
            or catalog['schema'] != 'opsloop-exhibition-disclosures/v1'
            or not isinstance(catalog['approvalReference'], str) or not catalog['approvalReference'].strip()
            or not isinstance(catalog['entries'], list) or len(catalog['entries']) > 32):
        raise Rejected('reviewed disclosure catalog required')
    artifacts = []
    seen = set()
    for item in catalog['entries']:
        if (not isinstance(item, dict) or set(item) != {'runId', 'kind', 'path', 'sha256'}
                or not identifier(item['runId']) or item['kind'] not in ('diff', 'checks')
                or not isinstance(item['path'], str) or not Path(item['path']).is_absolute()
                or not isinstance(item['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', item['sha256'])):
            raise Rejected('invalid disclosure entry')
        if item['runId'] != run_id:
            continue
        key = (item['kind'], item['sha256'])
        if key in seen:
            raise Rejected('duplicate disclosure artifact')
        seen.add(key)
        path = Path(item['path'])
        if item['kind'] == 'diff':
            artifacts.append(public_candidate_patch(path, item['sha256']))
            continue
        raw = regular_file(path, 64 * 1024)
        if digest(raw) != item['sha256']:
            raise Rejected('reviewed checks digest mismatch')
        try:
            obj = json.loads(raw, object_pairs_hook=_unique_object)
            if not isinstance(obj, dict) or set(obj) != {'checks'} or not isinstance(obj['checks'], list) or len(obj['checks']) > 15:
                raise Rejected('public checks contract rejected')
            names = set()
            for c in obj['checks']:
                if (not isinstance(c, dict) or not {'name','passed'} <= set(c)
                        or set(c) - {'name','passed','status','fixture_sha256','output_sha256'}
                        or c['name'] not in CHECKS or c['name'] in names or type(c['passed']) is not bool):
                    raise Rejected('public checks contract rejected')
                names.add(c['name'])
                if 'status' in c and (type(c['status']) is not int or not 100 <= c['status'] <= 599):
                    raise Rejected('public checks contract rejected')
                for key in ('fixture_sha256','output_sha256'):
                    if key in c and (not isinstance(c[key],str) or not re.fullmatch('[0-9a-f]{64}', c[key])):
                        raise Rejected('public checks contract rejected')
            content = raw.decode('utf-8')
        except (ValueError, TypeError, UnicodeError):
            raise Rejected('public checks contract rejected') from None
        artifacts.append({'kind':'checks', 'sha256':item['sha256'], 'content':content})
    return {'artifacts': artifacts}
