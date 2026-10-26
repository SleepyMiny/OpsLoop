"""Regression checks for false PASS conditions observed in P1-B."""
import copy
import importlib.util
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('verify_security', Path(__file__).parents[1] / 'scripts/verify-security.py')
security = importlib.util.module_from_spec(spec)
spec.loader.exec_module(security)


class SecurityEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.events = [
            {'config': {'scanner_name': 'govulncheck', 'scanner_version': 'v1.9.0',
                        'scan_mode': 'source', 'scan_level': 'symbol', 'go_version': 'go1.27.3',
                        'db': 'https://vuln.go.dev', 'protocol_version': 'v1.0.0',
                        'db_last_modified': '2026-10-21T00:27:29Z'}},
            {'SBOM': {'go_version': 'go1.27.3', 'modules': [{'path': 'stdlib', 'version': 'v1.27.3'}], 'roots': ['example/main']}},
            {'progress': {'message': 'Checking the code against the vulnerabilities...'}},
        ]
        self.args = dict(go_version='go1.27.3', mode='source', roots=['example/main'], exit_code=0,
                         stderr='', packages=[{'ImportPath': 'example/main'}],
                         package_exit_code=0, package_stderr='',
                         started_at='2026-10-22T00:00:00Z', completed_at='2026-10-22T00:01:00Z')

    def test_complete_empty_findings_is_pass(self):
        self.assertEqual(security.validate(self.events, **self.args)[1], 0)

    def test_missing_stdlib_version_cannot_pass_json_exit_zero(self):
        del self.events[1]['SBOM']['modules'][0]['version']
        self.assertEqual(security.validate(self.events, **self.args)[1], 2)

    def test_version_mismatch_is_error(self):
        self.events[1]['SBOM']['go_version'] = 'go1.27.2'
        self.assertEqual(security.validate(self.events, **self.args)[1], 2)

    def test_zero_or_wrong_targets_is_error(self):
        for roots in [[], ['example/other']]:
            with self.subTest(roots=roots):
                events = copy.deepcopy(self.events)
                events[1]['SBOM']['roots'] = roots
                self.assertEqual(security.validate(events, **self.args)[1], 2)

    def test_nonzero_exit_or_stderr_is_error(self):
        for override in [{'exit_code': 1}, {'stderr': 'package load failed'}]:
            with self.subTest(override=override):
                self.assertEqual(security.validate(self.events, **(self.args | override))[1], 2)

    def test_package_load_error_or_empty_load_is_error(self):
        for packages in [[], [{'ImportPath': 'example/main', 'Error': {'Err': 'missing dependency'}}]]:
            with self.subTest(packages=packages):
                self.assertEqual(security.validate(self.events, **(self.args | {'packages': packages}))[1], 2)

    def test_analysis_not_started_is_error(self):
        self.events.pop()
        self.assertEqual(security.validate(self.events, **self.args)[1], 2)

    def test_truncated_or_trailing_garbage_is_rejected(self):
        for text in ['{"config":', '{} garbage', '[]']:
            with self.subTest(text=text), self.assertRaises(ValueError):
                security.decode_stream(text)

    def test_finding_levels_and_ids_are_distinct(self):
        self.events.append({'osv': {'id': 'GO-EXAMPLE'}})
        for trace in [{'module': 'example'}, {'module': 'example', 'package': 'example/pkg'},
                      {'module': 'example', 'package': 'example/pkg', 'function': 'Call'}]:
            self.events.append({'finding': {'osv': 'GO-EXAMPLE', 'trace': [trace]}})
        result, code = security.validate(self.events, **self.args)
        self.assertEqual(code, 3)
        self.assertEqual(result['unique_advisory_ids'], ['GO-EXAMPLE'])
        self.assertEqual(result['finding_events'], {'module': 1, 'package': 1, 'symbol': 1})

    def test_unresolved_finding_is_error(self):
        self.events.append({'finding': {'osv': 'missing', 'trace': []}})
        self.assertEqual(security.validate(self.events, **self.args)[1], 2)

    def test_repeated_advisory_metadata_is_not_an_extra_advisory(self):
        self.events.extend([{'osv': {'id': 'GO-EXAMPLE'}}] * 2)
        result, code = security.validate(self.events, **self.args)
        self.assertEqual(code, 0)
        self.assertEqual(result['osv_event_count'], 2)
        self.assertEqual(result['osv_count'], 1)
        self.assertEqual(result['unique_advisory_ids'], [])

    def test_binary_compiler_version_uses_sbom_and_buildinfo(self):
        self.events[0]['config']['scan_mode'] = 'binary'
        del self.events[0]['config']['go_version']
        self.events[1]['SBOM']['modules'][0]['version'] = 'go1.27.3'
        args = self.args | {'mode': 'binary', 'packages': None}
        self.assertEqual(security.validate(self.events, **args)[1], 0)
        self.events[1]['SBOM']['modules'][0]['version'] = 'go1.27.2'
        self.assertEqual(security.validate(self.events, **args)[1], 2)

    def test_online_db_and_protocol_required(self):
        for change in [{'db': None}, {'db': 'https://example.invalid'},
                       {'protocol_version': 'v999.0.0'}, {'protocol_version': None}]:
            with self.subTest(change=change):
                events = copy.deepcopy(self.events)
                events[0]['config'].update(change)
                self.assertEqual(security.validate(events, **self.args)[1], 2)

    def test_timestamp_format_future_and_old_modification(self):
        for value in ['not-a-time', '2026-02-30T00:00:00Z', '2026-01-01',
                      '2026-01-01T00:00:00+00:60']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                events = copy.deepcopy(self.events)
                events[0]['config']['db_last_modified'] = value
                security.validate(events, **self.args)
        for value, expected in [('2999-01-01T00:00:00Z', 2),
                                ('2026-10-22T00:06:01Z', 2),
                                ('2026-10-22T00:06:00Z', 0),
                                ('2000-01-01T00:00:00Z', 0),
                                ('2026-10-21T09:00:00.123456789+09:00', 0)]:
            with self.subTest(value=value):
                events = copy.deepcopy(self.events)
                events[0]['config']['db_last_modified'] = value
                self.assertEqual(security.validate(events, **self.args)[1], expected)

    def test_unknown_and_malformed_events_fail_closed(self):
        for event in [{'error': {'message': 'failure'}}, {'findings': []}, {},
                      {'progress': []}, {'progress': {'message': 1}},
                      {'osv': {}}, {'finding': {'osv': 'GO-X', 'trace': [None]}},
                      {'progress': {}, 'finding': {}}, {'SBOM': None}]:
            with self.subTest(event=event), self.assertRaises(ValueError):
                security.validate(self.events + [event], **self.args)

    def test_duplicate_keys_are_not_silently_replaced(self):
        with self.assertRaises(ValueError):
            security.decode_stream('{"config":{},"config":{}}')

    def test_package_process_failure_even_with_good_json(self):
        for change in [{'package_exit_code': 1}, {'package_stderr': 'load failure'}]:
            self.assertEqual(security.validate(self.events, **(self.args | change))[1], 2)

    def test_scan_times_required_and_progress_time_checked(self):
        with self.assertRaises(ValueError):
            security.validate(self.events, **(self.args | {'started_at': None}))
        self.events[-1]['progress']['time'] = '2999-01-01T00:00:00Z'
        self.assertEqual(security.validate(self.events, **self.args)[1], 2)


if __name__ == '__main__':
    unittest.main()
