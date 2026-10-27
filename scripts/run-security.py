#!/usr/bin/env python3
"""Fresh online security gate. No stream/snapshot input or DB override is supported.

Use the same command locally and in CI. All subprocess environments are built
from scratch; package downloads are disabled (populate the module cache first).
Exit 2 = incomplete/tool error, 3 = findings, 0 = complete without findings.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('security', ROOT / 'scripts/verify-security.py')
security = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(security)
DB = security.OFFICIAL_DB
MODULE = 'github.com/SleepyMiny/OpsLoop'
TARGETS = {'collector': './cmd/status-collector', 'controller': './cmd/opsloop-controller', 'all': './...'}


def now():
    return datetime.now(timezone.utc).isoformat()


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def save(path, value):
    Path(path).write_text(json.dumps(value, indent=2) + '\n')


def clean_environment(directory, go, modcache, cache):
    # No caller GOENV, GOFLAGS, GOVERSION, DB/proxy, TLS, cloud or kube settings.
    home = directory / 'home'
    home.mkdir(mode=0o700)
    return {'PATH': str(go.parent) + ':/usr/bin:/bin', 'HOME': str(home),
            'TMPDIR': str(directory), 'LANG': 'C.UTF-8', 'TZ': 'UTC',
            'GOENV': 'off', 'GOWORK': 'off', 'GOTOOLCHAIN': 'local',
            'GOTELEMETRY': 'off', 'GOEXPERIMENT': '', 'GOFLAGS': '-mod=readonly -p=4',
            'GOPATH': str(directory / 'gopath'), 'GOMODCACHE': str(modcache),
            'GOCACHE': str(cache), 'GOPROXY': 'off', 'GOSUMDB': 'off',
            'GOMAXPROCS': '4', 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0',
            'KUBECONFIG': str(directory / 'NO_KUBECONFIG'), 'USE_EXISTING_CLUSTER': 'false',
            'PYTHONDONTWRITEBYTECODE': '1'}


class Runner:
    def __init__(self, directory, env):
        self.directory, self.env, self.records = directory, env, []

    def run(self, label, argv, arch='amd64'):
        run_id = label + '-' + uuid.uuid4().hex
        out, err = [self.directory / (run_id + suffix) for suffix in ('.stdout', '.stderr')]
        env = self.env | {'GOARCH': arch}
        start = now()
        with out.open('xb') as stdout, err.open('xb') as stderr:
            try:
                proc = subprocess.run([str(a) for a in argv], cwd=ROOT, env=env,
                                      stdout=stdout, stderr=stderr, timeout=1800)
                code = proc.returncode
            except (OSError, subprocess.TimeoutExpired) as error:
                stderr.write((str(error) + '\n').encode())
                code = 124 if isinstance(error, subprocess.TimeoutExpired) else 127
        record = {'id': run_id, 'command': [str(a) for a in argv], 'started_at': start,
                  'completed_at': now(), 'exit_code': code, 'GOARCH': arch,
                  'stdout': out.name, 'stderr': err.name,
                  'stdout_sha256': sha(out), 'stderr_sha256': sha(err)}
        self.records.append(record)
        save(self.directory / 'commands.json', self.records)
        return record, out, err

    def required(self, label, argv, arch='amd64'):
        record, out, err = self.run(label, argv, arch)
        if record['exit_code'] or err.read_text().strip():
            raise ValueError(label + ' failed; see command stdout/stderr')
        return out


def receipt(runner):
    # A separate direct, verified-TLS request. This is an observable receipt,
    # not proof that all scanner responses came from one atomic DB snapshot.
    code = '''import hashlib,json,urllib.request,datetime
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args,**kwargs): raise ValueError('DB redirect refused')
url='https://vuln.go.dev/index/db.json'
start=datetime.datetime.now(datetime.timezone.utc).isoformat()
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
with opener.open(url,timeout=30) as r:
 data=r.read()
 result={'requested_url':url,'final_url':r.geturl(),'http_status':r.status,
 'requested_at':start,'received_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),
 'sha256':hashlib.sha256(data).hexdigest(),'bytes':len(data),
 'last_modified_header':r.headers.get('Last-Modified'),'date_header':r.headers.get('Date'),
 'body':json.loads(data)}
 print(json.dumps(result))
'''
    out = runner.required('official-db-index-receipt', [sys.executable, '-I', '-c', code])
    result = json.loads(out.read_text())
    if result['http_status'] != 200 or result['final_url'] != DB + '/index/db.json':
        raise ValueError('unexpected DB receipt')
    security.timestamp(result['requested_at'])
    security.timestamp(result['received_at'])
    save(runner.directory / 'db-receipt.json', result)
    return result


def package_roots(packages):
    if not packages or any(p.get('Error') or p.get('DepsErrors') or p.get('Incomplete') for p in packages):
        raise ValueError('independent package discovery failed')
    roots = sorted(p['ImportPath'] for p in packages if not p.get('DepOnly'))
    if not roots or any(not isinstance(r, str) for r in roots):
        raise ValueError('empty/malformed package roots')
    return roots


def scan(runner, scanner, mode, target, roots, arch, load=None):
    rec, stream, stderr = runner.run(mode + '-' + Path(target).name,
        [scanner, '-db=' + DB, '-json', '-mode=' + mode, '-scan=symbol', target], arch)
    command = [sys.executable, '-B', ROOT / 'scripts/verify-security.py', stream,
               '--go-version', 'go1.27.3', '--mode', mode, '--exit-code', str(rec['exit_code']),
               '--stderr', stderr, '--started-at', rec['started_at'], '--completed-at', rec['completed_at']]
    for root in roots:
        command.extend(['--root', root])
    if load:
        lr, lo, le = load
        command.extend(['--packages', lo, '--package-exit-code', str(lr['exit_code']), '--package-stderr', le])
    vr, vo, ve = runner.run('validate-' + mode + '-' + Path(target).name, command, arch)
    result = {'scan_id': rec['id'], 'validation_id': vr['id'], 'mode': mode,
              'target': str(target), 'roots': roots, 'arch': arch,
              'raw_stream_sha256': sha(stream), 'scan_started_at': rec['started_at'],
              'scan_completed_at': rec['completed_at'], 'validator_exit': vr['exit_code']}
    # Check both the process status and validator's structured result.
    try:
        result['validation'] = json.loads(vo.read_text())
    except (ValueError, OSError):
        result['validation'] = {'status': 'TOOL_ERROR', 'errors': ['invalid validator output']}
    code = vr['exit_code']
    if rec['exit_code'] or stderr.read_text().strip() or ve.read_text().strip():
        code = 2
    if load and (load[0]['exit_code'] or load[2].read_text().strip()):
        code = 2
    if code not in (0, 3) or result['validation'].get('status') != ('PASS' if code == 0 else 'FINDINGS'):
        code = 2
    result['exit_code'] = code
    save(runner.directory / (rec['id'] + '.result.json'), result)
    return result


def source_manifest(runner):
    out = runner.required('source-paths', ['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'])
    files = [p for p in out.read_bytes().decode().split('\0') if p]
    return {p: sha(ROOT / p) for p in sorted(set(files)) if (ROOT / p).is_file()}


def execute(runner, go, scanner):
    report = {'id': runner.directory.name, 'evidence_kind': 'fresh_online_analysis',
              'db': DB, 'started_at': now(), 'GOOS': 'linux', 'CGO_ENABLED': '0',
              'architectures': ['amd64', 'arm64'], 'arm64_execution': False,
              'scans': [], 'binaries': [], 'status': 'TOOL_ERROR', 'errors': []}
    code = 2
    try:
        if any(os.environ.get(key) for key in ('GOVULNDB', 'GOVULNCHECK_DB')):
            raise ValueError('DB environment override is unsupported')
        version = runner.required('go-version', [go, 'env', 'GOVERSION']).read_text().strip()
        if version != 'go1.27.3':
            raise ValueError('Go 1.27.3 required')
        info = runner.required('scanner-buildinfo', [go, 'version', '-m', scanner]).read_text()
        if '\tmod\tgolang.org/x/vuln\tv1.9.0\t' not in info:
            raise ValueError('govulncheck v1.9.0 buildinfo required')
        report['tools'] = {'go': {'path': str(go), 'sha256': sha(go), 'version': version},
                           'scanner': {'path': str(scanner), 'sha256': sha(scanner), 'buildinfo': info}}
        report['source_before'] = source_manifest(runner)
        report['db_receipt'] = receipt(runner)
        # Preflight ensures regressions participate in the same local/CI gate.
        test, _, _ = runner.run('python-security-regressions', [sys.executable, '-B', '-m', 'unittest',
                        'discover', '-s', 'tests', '-p', 'test_*security*.py'])
        if test['exit_code']:
            raise ValueError('security regressions failed')
        for arch in ('amd64', 'arm64'):
            for name, target in TARGETS.items():
                discovered = runner.required('discover-' + name, [go, 'list', '-json', target], arch)
                roots = package_roots(security.decode_stream(discovered.read_text()))
                required = [MODULE + target[1:]] if name != 'all' else [MODULE + t[1:] for n, t in TARGETS.items() if n != 'all']
                if (name != 'all' and roots != required) or not set(required) <= set(roots):
                    raise ValueError('explicit target missing from discovery')
                load = runner.run('load-' + name, [go, 'list', '-deps', '-e', '-json', target], arch)
                report['scans'].append(scan(runner, scanner, 'source', target, roots, arch, load))
            for name in ('collector', 'controller'):
                target = TARGETS[name]
                binary = runner.directory / (name + '-' + arch)
                runner.required('build-' + name, [go, 'build', '-trimpath', '-ldflags',
                    '-s -w -X main.version=security-local', '-o', binary, target], arch)
                info = runner.required('buildinfo-' + name, [go, 'version', '-m', binary], arch).read_text()
                for expected in (': go1.27.3', '\tpath\t' + MODULE + target[1:],
                                 '\tbuild\tGOOS=linux', '\tbuild\tGOARCH=' + arch, '\tbuild\tCGO_ENABLED=0'):
                    if expected not in info:
                        raise ValueError('binary buildinfo mismatch: ' + name)
                report['binaries'].append({'target': target, 'arch': arch, 'sha256': sha(binary), 'buildinfo': info})
                # Pinned scanner v1.9.0 emits the module as binary SBOM root;
                # derive it from the explicit project target, never that SBOM.
                report['scans'].append(scan(runner, scanner, 'binary', str(binary), [MODULE], arch))
        report['source_after'] = source_manifest(runner)
        if report['source_before'] != report['source_after']:
            raise ValueError('source changed during analysis')
        codes = [r['exit_code'] for r in report['scans']]
        code = 2 if len(codes) != 10 or 2 in codes else 3 if 3 in codes else 0
        report['status'] = {0: 'PASS', 2: 'TOOL_ERROR', 3: 'FINDINGS'}[code]
    except (ValueError, OSError, KeyError, TypeError, AttributeError) as error:
        report['errors'].append(str(error))
    finally:
        report['completed_at'] = now()
        report['exit_code'] = code
        save(runner.directory / 'summary.json', report)
    return code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', type=Path, required=True)
    parser.add_argument('--scanner', type=Path, required=True)
    parser.add_argument('--gomodcache', type=Path, required=True)
    parser.add_argument('--gocache', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    directory = args.output.resolve() / (datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ-') + uuid.uuid4().hex)
    directory.mkdir(parents=True, mode=0o700)
    go, scanner = args.go.resolve(), args.scanner.resolve()
    runner = Runner(directory, clean_environment(directory, go, args.gomodcache.resolve(), args.gocache.resolve()))
    print('Security evidence directory:', directory, flush=True)
    return execute(runner, go, scanner)


if __name__ == '__main__':
    sys.exit(main())
