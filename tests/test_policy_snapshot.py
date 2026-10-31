"""Offline duration semantics; original bytes and authority are never normalized."""
import copy
import json
import unittest
from tools.engineering_evidence.seal import EvidenceError, policy_snapshots_equal, normalized_policy
from tests.policy_snapshot_fixture import policy_spec


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.raw = policy_spec()
        self.typed = copy.deepcopy(self.raw)
        for section, field, text in (
            ('budget','cooldown','2m0s'), ('budget','incidentDeadline','5m0s'),
            ('budget','placementTimeout','1m30s'), ('budget','verificationTimeout','1m0s'),
            ('detection','restartWindow','2m0s'), ('filter','maxSampleAge','1m30s'),
            ('score','recentFailureWindow','30m0s')):
            self.typed[section][field] = text

    def test_seven_different_strings_equal_without_mutation(self):
        before = json.dumps([self.raw, self.typed])
        self.assertNotEqual(self.raw, self.typed)
        self.assertTrue(policy_snapshots_equal(self.raw, self.typed))
        self.assertEqual(json.dumps([self.raw, self.typed]), before)

    def test_each_duration_and_non_duration_change_rejected(self):
        for section, fields in self.raw.items():
            if not isinstance(fields, dict): continue
            for key, value in fields.items():
                if type(value) not in (str, int): continue
                altered = copy.deepcopy(self.typed)
                altered[section][key] = value+1 if type(value) is int else value+'1ns'
                with self.subTest(path=(section,key)):
                    try: self.assertFalse(policy_snapshots_equal(self.raw, altered))
                    except EvidenceError: pass

    def test_missing_null_unknown_rejected_even_both_sides(self):
        mutations = [lambda p:p.pop('budget'),lambda p:p['budget'].pop('cooldown'),
                     lambda p:p['budget'].update(cooldown=None),lambda p:p.update(unknown='x'),
                     lambda p:p['budget'].update(unknown='x'),lambda p:p['candidateSelector'].update(unknown={}),
                     lambda p:p['targetRef'].update(extra='x'),lambda p:p.update(enabled=1),
                     lambda p:p['budget'].update(maxActionsPerIncident=True)]
        for mutate in mutations:
            altered=copy.deepcopy(self.raw); mutate(altered)
            with self.subTest(value=altered),self.assertRaises(EvidenceError):
                policy_snapshots_equal(altered,copy.deepcopy(altered))

    def test_optional_absence_not_defaulted(self):
        p=copy.deepcopy(self.raw);p['detection'].pop('podUnreadyFor')
        self.assertFalse(policy_snapshots_equal(self.raw,p))
        self.assertTrue(policy_snapshots_equal(p,copy.deepcopy(p)))
        p['candidateSelector'].update(matchExpressions=[])
        q=copy.deepcopy(p);q['candidateSelector'].pop('matchExpressions')
        self.assertFalse(policy_snapshots_equal(p,q))

    def test_invalid_or_lossy_duration_never_truncated(self):
        for value in ('', '+', '1d', 'nan', '1s trailing', '-1s', '0s', '9223372036854775808ns',
                      '0.1ns', '1s0.5ns0.5ns', '١٢٠s', '１２０s', '1.0000000001s', None, 120, True):
            changed=copy.deepcopy(self.typed);changed['budget']['cooldown']=value
            with self.subTest(value=value), self.assertRaises(EvidenceError):
                policy_snapshots_equal(changed,copy.deepcopy(changed))

    def test_exact_nanoseconds_and_upper_boundary(self):
        for left,right,equal in [('9223372036854775807ns','2562047h47m16.854775807s',True),
                                 ('120000000001ns','2m0s',False),('120.000000001s','120000000001ns',True)]:
            a=copy.deepcopy(self.raw);b=copy.deepcopy(self.raw)
            a['budget']['cooldown']=left;b['budget']['cooldown']=right
            self.assertEqual(policy_snapshots_equal(a,b),equal)
        self.assertEqual(normalized_policy({'budget':{'cooldown':'120.000000001s'}})['budget']['cooldown'],120000000001)

if __name__=='__main__': unittest.main()
