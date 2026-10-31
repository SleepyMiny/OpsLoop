#!/usr/bin/env python3
"""Local CI prerequisites and required Go/Python bridge results; never installs."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
REQUIRED_CONNECTIONS = {
    'github.com/SleepyMiny/OpsLoop/api/v1alpha1': ['TestPolicySnapshotPythonSerialization'],
    'github.com/SleepyMiny/OpsLoop/internal/controller': [
        'TestHeartbeatPythonTransport',
        'TestHeartbeatControllerStartupThroughFirstAction',
        'TestHeartbeatControllerReadinessAndRevocationBlocksAction',
        'TestTerminalControllerThroughSealAndEngineeringIntent',
    ],
}


def connection_results(data):
    expected = {(package, name) for package, names in REQUIRED_CONNECTIONS.items() for name in names}
    outcomes = {key: [] for key in expected}
    failures = []
    for line in data.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            # Compiler/error output is allowed in the log, but never proves a PASS.
            continue
        if not isinstance(event, dict):
            continue
        action = event.get('Action')
        key = (event.get('Package'), event.get('Test'))
        if action == 'fail':
            failures.append({'package': key[0], 'test': key[1]})
        if key in outcomes and action in ('pass', 'fail', 'skip'):
            outcomes[key].append(action)
    results = [{'package': p, 'test': n, 'outcomes': outcomes[(p, n)]}
               for p, n in sorted(expected)]
    return {'status': 'PASS' if not failures and all(v == ['pass'] for v in outcomes.values()) else 'FAIL',
            'requiredConnections': results, 'failedEvents': failures}


def environment(go=None, node=None, assets=None):
    import yaml  # Missing dependency must fail before local API tests start.
    expected_python = (ROOT / '.python-version').read_text().strip()
    found = {'python': '.'.join(map(str, sys.version_info[:3])), 'PyYAML': yaml.__version__}
    if found != {'python': expected_python, 'PyYAML': '6.0.3'}:
        raise ValueError('Python/PyYAML does not match the pinned CI environment')
    def output(argv):
        return subprocess.check_output(argv, text=True, stderr=subprocess.STDOUT, timeout=20).strip()
    if go:
        found['go'] = output([go, 'version'])
        if found['go'].split()[2] != 'go1.27.3':
            raise ValueError('Go version mismatch')
    if node:
        found['node'] = output([node, '--version'])
        if found['node'] != 'v24.22.0':
            raise ValueError('Node version mismatch')
    if assets:
        assets = Path(assets)
        for name in ('kube-apiserver', 'etcd', 'kubectl'):
            if not (assets / name).is_file() or not os.access(assets / name, os.X_OK):
                raise ValueError('envtest executable missing: ' + name)
        found['kube-apiserver'] = output([str(assets / 'kube-apiserver'), '--version'])
        found['kubectl'] = json.loads(output([str(assets / 'kubectl'), 'version', '--client=true', '-o', 'json']))['clientVersion']['gitVersion']
        if found['kube-apiserver'] != 'Kubernetes v1.35.0' or found['kubectl'] != 'v1.35.0':
            raise ValueError('envtest Kubernetes version mismatch')
    return {'status': 'PASS', 'versions': found, 'productionAccess': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    env = commands.add_parser('environment')
    env.add_argument('--go')
    env.add_argument('--node')
    env.add_argument('--envtest-assets')
    log = commands.add_parser('go-log')
    log.add_argument('path', type=Path)
    args = parser.parse_args()
    try:
        result = environment(args.go, args.node, args.envtest_assets) if args.command == 'environment' else connection_results(args.path.read_text())
    except (ImportError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        result = {'status': 'FAIL', 'reason': type(error).__name__ + ': CI prerequisite/result contract failed'}
    print(json.dumps(result, indent=2))
    return 0 if result['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
