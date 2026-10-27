"""Local integration tests: synthetic processes + real wrapper and validator.
No scanner, DB, cluster or cloud access; no historical stream is a gate input.
"""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('run_security', Path(__file__).parents[1] / 'scripts/run-security.py')
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class SecurityWrapperTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.env = gate.clean_environment(self.directory, Path('/usr/bin/go'), self.directory, self.directory)
        self.runner = gate.Runner(self.directory, self.env)
        self.events = [
            {'config': {'protocol_version': 'v1.0.0', 'scanner_name': 'govulncheck',
                        'scanner_version': 'v1.9.0', 'db': gate.DB,
                        'db_last_modified': '2000-01-01T00:00:00Z', 'go_version': 'go1.27.3',
                        'scan_mode': 'source', 'scan_level': 'symbol'}},
            {'SBOM': {'go_version': 'go1.27.3', 'modules': [{'path': 'stdlib', 'version': 'v1.27.3'}],
                      'roots': ['example/main']}},
            {'progress': {'message': 'Checking the code against the vulnerabilities...'}},
        ]

    def synthetic(self, events=None, scanner_code=0, scanner_stderr='', load_code=0,
                  packages=None, load_stderr='', raw=None):
        # Executable fixture emits controlled process output; it cannot access a network.
        tool = self.directory / 'synthetic-scanner'
        body = raw if raw is not None else '\n'.join(json.dumps(e) for e in (events or self.events))
        tool.write_text('#!/usr/bin/python3\nimport sys\nprint(' + repr(body) + ')\n'
                        'print(' + repr(scanner_stderr) + ', file=sys.stderr,end="")\n'
                        'sys.exit(' + str(scanner_code) + ')\n')
        tool.chmod(0o700)
        packages = [{'ImportPath': 'example/main'}] if packages is None else packages
        load = self.runner.run('synthetic-package-load', [sys.executable, '-I', '-c',
            f'import sys;print({json.dumps(packages[0])!r})' if len(packages) == 1 else
            'import sys;print(' + repr('\n'.join(json.dumps(p) for p in packages)) + ')'])
        # Use an actual second process for failure cases to retain genuine statuses/stderr.
        if load_code or load_stderr:
            load = self.runner.run('synthetic-load-failure', [sys.executable, '-I', '-c',
                'import sys;print(' + repr('\n'.join(json.dumps(p) for p in packages)) + ');'
                'print(' + repr(load_stderr) + ',file=sys.stderr,end="");sys.exit(' + str(load_code) + ')'])
        result = gate.scan(self.runner, tool, 'source', './synthetic', ['example/main'], 'amd64', load)
        self.assertTrue((self.directory / 'commands.json').is_file())
        self.assertTrue(list(self.directory.glob('*.result.json')))
        for record in self.runner.records:
            for field in ('stdout', 'stderr'):
                self.assertTrue((self.directory / record[field]).is_file())
        return result

    def test_normal_analysis_and_old_modified_db_succeed(self):
        self.assertEqual(self.synthetic()['exit_code'], 0)

    def test_scanner_nonzero_and_stderr_fail(self):
        for args in [{'scanner_code': 17}, {'scanner_stderr': 'synthetic analysis failure'}]:
            with self.subTest(args=args):
                self.assertEqual(self.synthetic(**args)['exit_code'], 2)

    def test_package_load_exit_stderr_errors_and_missing_target_fail(self):
        for args in [{'load_code': 7}, {'load_stderr': 'load error'}, {'packages': []},
                     {'packages': [{'ImportPath': 'wrong'}]},
                     {'packages': [{'ImportPath': 'example/main', 'Incomplete': True}]}]:
            with self.subTest(args=args):
                self.assertEqual(self.synthetic(**args)['exit_code'], 2)

    def test_truncation_missing_stdlib_finding_and_unknown_event_fail(self):
        self.assertEqual(self.synthetic(raw='{"config":')['exit_code'], 2)
        self.events[1]['SBOM']['modules'] = []
        self.assertEqual(self.synthetic()['exit_code'], 2)
        self.events[1]['SBOM']['modules'] = [{'path': 'stdlib', 'version': 'v1.27.3'}]
        self.events.extend([{'osv': {'id': 'GO-SYNTHETIC'}},
                            {'finding': {'osv': 'GO-SYNTHETIC', 'trace': [{'module': 'example'}]}}])
        self.assertEqual(self.synthetic()['exit_code'], 3)
        self.events.append({'error': {'message': 'unsupported'}})
        self.assertEqual(self.synthetic()['exit_code'], 2)

    def test_validator_process_failure_is_not_masked(self):
        original = self.runner.run
        def run(label, argv, arch='amd64'):
            if label.startswith('validate-'):
                argv = [sys.executable, '-I', '-c', 'import sys;sys.exit(19)']
            return original(label, argv, arch)
        with patch.object(self.runner, 'run', side_effect=run):
            self.assertEqual(self.synthetic()['exit_code'], 2)

    def test_clean_environment_and_db_override_rejection(self):
        for key in ('AWS_SECRET_ACCESS_KEY', 'AZURE_CLIENT_SECRET', 'GOVULNDB', 'HTTPS_PROXY', 'SSL_CERT_FILE'):
            self.assertNotIn(key, self.env)
        self.assertEqual(self.env['GOENV'], 'off')
        with patch.dict(os.environ, {'GOVULNDB': 'https://unapproved.invalid'}):
            self.assertEqual(gate.execute(self.runner, Path('/not-used'), Path('/not-used')), 2)
        self.assertIn('override', json.loads((self.directory / 'summary.json').read_text())['errors'][0])

    def test_online_failure_never_uses_past_clean_stream(self):
        old = self.directory / 'past-clean.json'
        old.write_text('\n'.join(json.dumps(e) for e in self.events))
        # Fail the receipt after synthetic preflight; no scanner invocation follows.
        info = self.directory / 'preflight'
        def required(label, argv, arch='amd64'):
            info.write_text('go1.27.3' if label == 'go-version' else '\tmod\tgolang.org/x/vuln\tv1.9.0\thash')
            return info
        with patch.object(self.runner, 'required', side_effect=required), \
             patch.object(gate, 'sha', return_value='0'*64), \
             patch.object(gate, 'source_manifest', return_value={}), \
             patch.object(gate, 'receipt', side_effect=ValueError('online DB unavailable')):
            self.assertEqual(gate.execute(self.runner, Path('/unused'), Path('/unused')), 2)
        report = json.loads((self.directory / 'summary.json').read_text())
        self.assertEqual(report['status'], 'TOOL_ERROR')
        self.assertEqual(report['scans'], [])
        self.assertTrue(old.is_file())

    def test_full_source_discovery_rejects_package_errors(self):
        for packages in [[], [{'ImportPath': 'x', 'Error': {'Err': 'load failed'}}]]:
            with self.assertRaises(ValueError):
                gate.package_roots(packages)

    def test_complete_gate_propagates_synthetic_process_results(self):
        # Exercise orchestration and its final exit, not just a validator call.
        for failure in (None, 'scanner', 'load', 'discovery', 'validator', 'finding'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                directory = Path(tmp)
                runner = gate.Runner(directory, gate.clean_environment(directory, Path('/usr/bin/go'), directory, directory))
                real_run = runner.run
                def run(label, argv, arch='amd64'):
                    argv = [str(a) for a in argv]
                    code, stderr = 0, ''
                    if label == 'go-version':
                        output = 'go1.27.3'
                    elif label == 'scanner-buildinfo':
                        output = '\tmod\tgolang.org/x/vuln\tv1.9.0\thash'
                    elif label == 'python-security-regressions':
                        output = 'synthetic regression subprocess'
                    elif label.startswith(('discover-', 'load-')):
                        roots = [gate.MODULE + t[1:] for n,t in gate.TARGETS.items() if n != 'all'] if argv[-1] == './...' else [gate.MODULE + argv[-1][1:]]
                        output = '\n'.join(json.dumps({'ImportPath': r}) for r in roots)
                        if label.startswith('load-') and failure == 'load': code = 9
                        if label.startswith('discover-') and failure == 'discovery': output = ''
                    elif label.startswith('buildinfo-'):
                        name = label.removeprefix('buildinfo-')
                        output = ': go1.27.3\n\tpath\t' + gate.MODULE + gate.TARGETS[name][1:] + '\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=' + arch + '\n\tbuild\tCGO_ENABLED=0'
                    elif label.startswith('build-'):
                        Path(argv[argv.index('-o') + 1]).write_bytes(b'synthetic binary')
                        output = ''
                    elif label.startswith(('source-', 'binary-')):
                        self.assertIn('-db=https://vuln.go.dev', argv)
                        binary = label.startswith('binary-')
                        roots = [gate.MODULE] if binary else ([gate.MODULE + t[1:] for n,t in gate.TARGETS.items() if n != 'all'] if argv[-1] == './...' else [gate.MODULE + argv[-1][1:]])
                        events = json.loads(json.dumps(self.events))
                        events[1]['SBOM']['roots'] = roots
                        if binary:
                            events[0]['config']['scan_mode'] = 'binary'
                            del events[0]['config']['go_version']
                            events[1]['SBOM']['modules'][0]['version'] = 'go1.27.3'
                        if failure == 'finding':
                            events.extend([{'osv': {'id': 'GO-SYNTHETIC'}}, {'finding': {'osv': 'GO-SYNTHETIC', 'trace': [{'module': 'example'}]}}])
                        output = '\n'.join(json.dumps(e) for e in events)
                        if failure == 'scanner': code, stderr = 12, 'synthetic scanner failure'
                    elif label.startswith('validate-'):
                        if failure != 'validator': return real_run(label, argv, arch)
                        output, code = '{}', 21
                    else:
                        raise AssertionError(label)
                    return real_run(label, [sys.executable, '-I', '-c',
                        'import sys;print(' + repr(output) + ');print(' + repr(stderr) + ',file=sys.stderr,end="");sys.exit(' + str(code) + ')'], arch)
                with patch.object(runner, 'run', side_effect=run), \
                     patch.object(gate, 'sha', return_value='0'*64), \
                     patch.object(gate, 'source_manifest', return_value={}), \
                     patch.object(gate, 'receipt', return_value={'synthetic': True}):
                    code = gate.execute(runner, Path('/synthetic-go'), Path('/synthetic-scanner'))
                report = json.loads((directory / 'summary.json').read_text())
                self.assertEqual(code, 0 if failure is None else 3 if failure == 'finding' else 2)
                self.assertEqual(report['status'], 'PASS' if failure is None else 'FINDINGS' if failure == 'finding' else 'TOOL_ERROR')
                if failure != 'discovery': self.assertEqual(len(report['scans']), 10)
                self.assertTrue((directory / 'commands.json').exists())


if __name__ == '__main__':
    unittest.main()
