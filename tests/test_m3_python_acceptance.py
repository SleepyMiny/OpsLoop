"""Exercise actual unittest outcomes; no Terraform provider or cloud operations."""
import importlib.util
import io
import subprocess
import sys
import tempfile
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('python_acceptance', ROOT / 'scripts/m3-python-tests.py')
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class PythonAcceptanceTests(unittest.TestCase):
    def run_cases(self, cases):
        class Case(unittest.TestCase):
            def __init__(self, name, outcome):
                super().__init__('runTest')
                self.name, self.outcome = name, outcome

            def id(self):
                return self.name

            def runTest(self):
                if self.outcome == 'skip':
                    self.skipTest('synthetic unavailable tool')
                if self.outcome == 'fail':
                    self.fail('synthetic assertion')
                if self.outcome == 'error':
                    raise RuntimeError('synthetic error')

        suite = unittest.TestSuite(Case(name, outcome) for name, outcome in cases)
        result = unittest.TextTestRunner(stream=io.StringIO(), resultclass=acceptance.Result).run(suite)
        return acceptance.acceptance(result)

    def passing(self):
        return [(name, 'pass') for name in sorted(acceptance.REQUIRED)]

    def test_all_seven_must_pass(self):
        result = self.run_cases(self.passing())
        self.assertEqual(len(acceptance.REQUIRED), 7)
        self.assertEqual(result['status'], 'PASS')
        self.assertEqual(result['passed'], 7)

    def test_missing_duplicate_or_empty_suite_fails(self):
        good = self.passing()
        for cases in [[], good[:-1], good + [good[0]]]:
            with self.subTest(cases=cases):
                self.assertEqual(self.run_cases(cases)['status'], 'FAIL')

    def test_skip_even_outside_required_group_is_not_pass(self):
        result = self.run_cases(self.passing() + [('another.local.contract', 'skip')])
        self.assertEqual(result['status'], 'FAIL')
        self.assertEqual(result['skipped'][0]['test'], 'another.local.contract')

    def test_required_failure_error_or_skip_is_rejected(self):
        good = self.passing()
        for outcome in ['fail', 'error', 'skip']:
            with self.subTest(outcome=outcome):
                result = self.run_cases([(good[0][0], outcome), *good[1:]])
                self.assertEqual(result['status'], 'FAIL')
                self.assertIn(good[0][0], result['requiredMissingOrNotExactlyOnePass'])

    def test_script_entry_restores_repository_imports_without_pythonpath(self):
        # Run from elsewhere, like the script entry point (sys.path[0]=scripts),
        # without inheriting this unittest process's repository import path.
        program = "import runpy; runpy.run_path(" + repr(str(ROOT / 'scripts/m3-python-tests.py')) + "); import tools.exhibition_session; import tests.test_engineering_evidence"
        with tempfile.TemporaryDirectory() as cwd:
            result = subprocess.run([sys.executable, '-c', program], cwd=cwd,
                env={'PATH': '/usr/bin:/bin', 'PYTHONDONTWRITEBYTECODE': '1'},
                capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_workflows_use_strict_runner_and_verified_installation(self):
        import yaml
        ci = yaml.safe_load((ROOT / '.github/workflows/ci.yaml').read_text())
        m3 = yaml.safe_load((ROOT / '.github/workflows/m3-local.yaml').read_text())
        for workflow, job in [(ci, 'verify'), (m3, 'offline-checks')]:
            steps = workflow['jobs'][job]['steps']
            installs = [s for s in steps if 'install-ci-terraform.sh' in s.get('run', '')]
            self.assertEqual(len(installs), 1)
            self.assertIn('GITHUB_PATH', installs[0]['run'])
            self.assertFalse(installs[0].get('continue-on-error', False))
        commands = '\n'.join(s.get('run', '') for s in ci['jobs']['verify']['steps'])
        self.assertIn('m3-python-tests.py --terraform-only', commands)


if __name__ == '__main__':
    unittest.main()
