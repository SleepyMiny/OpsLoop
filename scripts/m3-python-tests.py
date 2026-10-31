#!/usr/bin/env python3
"""Run the local Python acceptance suite; missing/skipped checks are not PASS.

The separate Frontend Live API opt-in exclusion is unaffected. This suite needs
no operating credentials, Terraform backend/provider, cluster, or model calls.
"""
import argparse
import json
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[1]
# Match `python -m unittest` repository imports when invoked as a script.
sys.path.insert(0, str(ROOT))
TERRAFORM_TESTS = {
    'test_explicit_null_inherits_without_evaluating_floor_null',
    'test_explicit_whole_sizes_remain_supported', 'test_fractional_override_rejected',
    'test_nonnumeric_input_rejected', 'test_omitted_input_inherits',
    'test_provider_upper_limit_enforced', 'test_undersized_overrides_rejected',
}
REQUIRED = {'test_terraform_disk_contract.TerraformDiskContractTests.' + n
            for n in TERRAFORM_TESTS}


class Result(unittest.TextTestResult):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.passed_ids = []

    def addSuccess(self, test):
        self.passed_ids.append(test.id())
        super().addSuccess(test)


def acceptance(result):
    missing = sorted(n for n in REQUIRED if result.passed_ids.count(n) != 1)
    return {'status': 'PASS' if result.wasSuccessful() and not result.skipped
            and result.testsRun > 0 and not missing else 'FAIL',
            'testsRun': result.testsRun, 'passed': len(result.passed_ids),
            'skipped': [{'test': t.id(), 'reason': why} for t, why in result.skipped],
            'requiredMissingOrNotExactlyOnePass': missing}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--terraform-only', action='store_true')
    args = parser.parse_args()
    suite = unittest.defaultTestLoader.discover(str(ROOT / 'tests'),
        pattern='test_terraform_disk_contract.py' if args.terraform_only else 'test*.py')
    result = unittest.TextTestRunner(verbosity=2, resultclass=Result).run(suite)
    receipt = acceptance(result)
    print(json.dumps(receipt, sort_keys=True))
    return 0 if receipt['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
