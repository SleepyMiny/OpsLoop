"""Synthetic projection tests: no Kubernetes, permit, or model requests."""
import copy,json,unittest
from tools.exhibition_handoff import source_status_event
from tools.exhibition_control import public_data
class SourceEventTests(unittest.TestCase):
 def setUp(self):
  self.binding={'runID':'new-run','epoch':'new-epoch','policyUID':'policy','deploymentUID':'deployment'}
  self.policy={'metadata':{'uid':'policy','generation':21,'annotations':{'opsloop.io/execution-grant':json.dumps({'scope':self.binding})}},'status':{'conditions':[{'type':'ReproductionSource','reason':'SourcePending','status':'Unknown','observedGeneration':21,'message':'private credential omitted'}]}}
 def test_controller_pending_bound_rejected_only(self):
  for reason,status,phase in [('SourcePending','Unknown','PENDING'),('SourceBound','True','BOUND'),('SourceRejected','False','REJECTED')]:
   self.policy['status']['conditions'][0].update(reason=reason,status=status)
   event=source_status_event(self.policy,self.binding,21)
   self.assertEqual(event[0],'SOURCE_'+phase);self.assertEqual(public_data(event[1]),event[1]);self.assertNotIn('credential',json.dumps(event));self.assertNotIn('COMPLETED',event[0])
 def test_b1_success_cannot_be_relabelled(self):
  for key in self.binding:
   with self.subTest(key=key):
    p=copy.deepcopy(self.policy);b=copy.deepcopy(self.binding);b[key]='old-b1';p['metadata']['annotations']['opsloop.io/execution-grant']=json.dumps({'scope':b})
    self.assertIsNone(source_status_event(p,self.binding,21))
 def test_stale_or_ambiguous_condition_rejected(self):
  for mutate in [lambda p:p['metadata'].update(uid='other'),lambda p:p['metadata'].update(generation=20),lambda p:p['status']['conditions'][0].update(observedGeneration=20),lambda p:p['status']['conditions'].append(copy.deepcopy(p['status']['conditions'][0])),lambda p:p['status']['conditions'][0].update(status='True'),lambda p:p['metadata']['annotations'].clear()]:
   p=copy.deepcopy(self.policy);mutate(p);self.assertIsNone(source_status_event(p,self.binding,21))
if __name__=='__main__':unittest.main()
