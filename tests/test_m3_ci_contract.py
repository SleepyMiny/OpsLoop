"""CI receipt and dependency wiring; synthetic events, no cluster or provider."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


contract = load('ci_contract_test', 'scripts/m3-ci-contract.py')
runner = load('ci_runner_test', 'scripts/m3-verify.py')


class CIContract(unittest.TestCase):
    def events(self):
        return [{'Package': p, 'Test': n, 'Action': 'pass'}
                for p, names in contract.REQUIRED_CONNECTIONS.items() for n in names]

    def result(self, events):
        return contract.connection_results('\n'.join(map(json.dumps, events)))['status']

    def test_exact_five_connections_pass(self):
        self.assertEqual(len(self.events()), 5)
        self.assertEqual(self.result(self.events()), 'PASS')

    def test_missing_skip_duplicate_wrong_package_and_failed_package_rejected(self):
        good = self.events()
        cases = [good[:-1], good + [good[0]],
                 [{**good[0], 'Action': 'skip'}, *good[1:]],
                 [{**good[0], 'Package': 'unrelated'}, *good[1:]],
                 [*good, {'Action': 'fail', 'Package': 'compiler-error'}]]
        for events in cases:
            with self.subTest(events=events):
                self.assertEqual(self.result(events), 'FAIL')
        self.assertEqual(contract.connection_results('not-json\n{}\n[]')['status'], 'FAIL')

    def test_actual_runner_preserves_exit_failure_and_rejects_missing_passes(self):
        for code, data, expected in [(0, self.events(), 'PASS'), (1, self.events(), 'FAIL'),
                                     (0, self.events()[:-1], 'FAIL')]:
            with self.subTest(code=code, count=len(data)), tempfile.TemporaryDirectory() as d:
                program = 'import sys;print(' + repr('\n'.join(map(json.dumps, data))) + ');sys.exit(' + str(code) + ')'
                result = runner.execute('go-connections', [sys.executable, '-c', program], ROOT,
                                        {'PATH': '/usr/bin:/bin'}, Path(d), 10, require_connections=True)
                self.assertEqual(result['exitCode'], code)
                self.assertEqual(result['status'], expected)

    def test_missing_yaml_fails_environment_without_installing(self):
        result = subprocess.run([sys.executable, '-S', str(ROOT / 'scripts/m3-ci-contract.py'), 'environment'],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)['status'], 'FAIL')

    def test_missing_envtest_rejected_before_go_execution(self):
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / 'no-output'
            result = subprocess.run([sys.executable, str(ROOT / 'scripts/m3-verify.py'),
                                     '--gomodcache', d, '--only', 'go-connections', '--output', str(target)],
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('require --envtest-assets', result.stderr)
            self.assertFalse(target.exists())

    def test_environment_wrong_tool_and_incomplete_assets_rejected(self):
        with patch.object(contract.subprocess, 'check_output', return_value='go version go0.0.0 linux/amd64'):
            with self.assertRaises(ValueError):
                contract.environment(go='/synthetic/go')
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(ValueError):
                contract.environment(assets=d)

    def test_environment_failure_stops_runner_before_required_tests(self):
        seen = []
        def check(name, argv, cwd, env, output, timeout, **kwargs):
            seen.append(name)
            (output / (name + '.log')).write_text('synthetic missing prerequisite')
            return {'name': name, 'status': 'FAIL' if name == 'ci-environment' else 'PASS', 'log': name + '.log'}
        with tempfile.TemporaryDirectory() as d, patch.object(runner, 'execute', side_effect=check), \
             patch.object(sys, 'argv', ['m3-verify.py', '--gomodcache', d, '--envtest-assets', d,
                                       '--output', str(Path(d) / 'out'), '--only', 'go-connections']):
            self.assertEqual(runner.main(), 1)
            receipt = json.loads((Path(d) / 'out/verification.json').read_text())
        self.assertEqual(receipt['status'], 'FAIL')
        self.assertIn('go-connections', receipt['notRunChecks'])
        self.assertNotIn('build-cli', seen)
        self.assertNotIn('go-connections', seen)

    def test_runner_path_contains_invoking_python_without_credentials(self):
        seen = []
        def check(name, argv, cwd, env, output, timeout, **kwargs):
            seen.append(env)
            (output / (name + '.log')).write_text('synthetic tool result')
            return {'name': name, 'status': 'PASS', 'log': name + '.log'}
        with tempfile.TemporaryDirectory() as d, patch.object(runner, 'execute', side_effect=check), \
             patch.dict(runner.os.environ, {'OPERATING_AUTH_SENTINEL': 'synthetic-not-inherited'}), \
             patch.object(sys, 'argv', ['m3-verify.py', '--gomodcache', d, '--output', str(Path(d) / 'out'), '--only', 'ci-environment']):
            self.assertEqual(runner.main(), 0)
        self.assertTrue(seen)
        for env in seen:
            self.assertIn(str(Path(sys.executable).absolute().parent), env['PATH'].split(':'))
            self.assertNotIn('OPERATING_AUTH_SENTINEL', env)
            self.assertEqual(env['USE_EXISTING_CLUSTER'], 'false')

    def test_workflow_tools_required_connections_and_publish_boundary(self):
        workflows = {n: yaml.safe_load((ROOT / '.github/workflows' / n).read_text())
                     for n in ('ci.yaml', 'm3-local.yaml')}
        ci, m3 = workflows['ci.yaml'], workflows['m3-local.yaml']
        for workflow, job in ((ci, 'verify'), (m3, 'offline-checks')):
            self.assertEqual(workflow['permissions'], {})
            self.assertEqual(workflow['jobs'][job]['permissions'], {'contents': 'read'})
            steps = workflow['jobs'][job]['steps']
            setup = next(s for s in steps if s.get('uses', '').startswith('actions/setup-python@'))
            self.assertEqual(setup['uses'].split('@')[1], 'e797f83bcb11b83ae66e0230d6156d7c80228e7c')
            self.assertEqual(setup['with']['python-version-file'], '.python-version')
            install = next(s['run'] for s in steps if '--require-hashes' in s.get('run', ''))
            self.assertIn('--only-binary=:all: -r requirements-ci.txt', install)
            self.assertFalse(any(s.get('continue-on-error') for s in steps))
        run = next(s['run'] for s in ci['jobs']['verify']['steps'] if s.get('name') == 'go test (envtest)')
        self.assertIn('OPSLOOP_REQUIRE_ENVTEST=1 go test -json -count=1', run)
        self.assertIn('m3-ci-contract.py go-log', run)
        self.assertEqual(ci['defaults']['run']['shell'], 'bash')
        publish = ci['jobs']['publish']
        self.assertEqual(publish['if'], "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'")
        self.assertEqual(publish['needs'], ['verify', 'image', 'dashboard'])
        self.assertEqual(publish['environment'], 'acr-publish')
        self.assertEqual(publish['permissions'], {'contents': 'read', 'id-token': 'write'})
        self.assertEqual(publish['concurrency'], {'group': 'publish-main', 'cancel-in-progress': False})
        for workflow in workflows.values():
            for name, job in workflow['jobs'].items():
                if name != 'publish':
                    self.assertNotIn('id-token', job.get('permissions', {}))


if __name__ == '__main__':
    unittest.main()
