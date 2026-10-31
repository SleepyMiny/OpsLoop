import copy,unittest
from tools.exhibition_inputs import approved_circuit_signatures
from tools.exhibition_execution import trial_gate
from tools.exhibition_hashes import ControlError

def fixture(signatures):
 return {'fixedTargets':{'b':{'policyUID':'p'}},'executionInputs':{'expectedCircuitHistory':{'b':{'initialSignatures':signatures,'additions':[]}}}}
def policy(signatures):return {'metadata':{'uid':'p'},'status':{'circuits':[{'signature':s} for s in signatures]}}
class CircuitPreflight(unittest.TestCase):
 def test_known_preserved(self):
  p=fixture(['a'*64,'b'*64]);pol=policy(['a'*64,'b'*64]);before=copy.deepcopy(pol)
  self.assertEqual(approved_circuit_signatures(p,'b',pol),['a'*64,'b'*64]);self.assertEqual(pol,before)
 def test_new_circuit_rejected_before_other_preflight(self):
  with self.assertRaisesRegex(ControlError,'UNEXPECTED_CIRCUIT_HISTORY'):trial_gate(fixture(['a'*64]),'b',{'b-policy':policy(['b'*64])},{})
 def test_unknown_not_added(self):
  p=fixture(['a'*64]);before=copy.deepcopy(p)
  with self.assertRaises(ControlError):approved_circuit_signatures(p,'b',policy(['b'*64]))
  self.assertEqual(p,before)
 def test_missing_contract_rejected(self):
  with self.assertRaises(KeyError):approved_circuit_signatures({},'b',{})
