"""Signed fixed-Policy Runtime transitions, synthetic API/no injection."""
import copy,json,hashlib,tempfile,threading,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_runtime import canonical,RuntimeRejected
from tools.exhibition_lifecycle import patched
from tests.test_exhibition_runtime import fixture as runtime_fixture,FakeAPI,FakeAdapter,NOW,iso
from tests.test_exhibition_permit import fixture as permit_fixture

class API(FakeAPI):
    def request(self,path,patch=None,absent=False):
        if patch is None:return super().request(path,absent=absent)
        self.mutations.append(copy.deepcopy(patch));before=copy.deepcopy(self.policy['spec'])
        self.policy=patched(self.policy,patch)
        if before!=self.policy['spec']:self.policy['metadata']['generation']+=1
        self.policy['metadata']['resourceVersion']=str(int(self.policy['metadata']['resourceVersion'])+1)
        return copy.deepcopy(self.policy)

class RepeatRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.plan=runtime_fixture(self.root);r=self.plan['runtime']
        r['policyBefore'].update(generation=10,incidentSequence=7);r['policyBefore']['spec'].update(enabled=False,mode='Enforce')
        r['executionGrant']['scope'].update(policyGeneration=12,incident='demo-api-policy-f-00008')
        guard=json.loads(Path(r['guardConfig']['path']).read_bytes());guard.update(policyGeneration=12);guard['policySpec'].update(enabled=True,mode='Enforce')
        Path(r['guardConfig']['path']).write_bytes(canonical(guard));r['guardConfig']['sha256']=hashlib.sha256(canonical(guard)).hexdigest()
        payload,_,pin,_,_,_,sign=permit_fixture(self.root)
        payload.update(grant=copy.deepcopy(r['executionGrant']),baseIncidentSequence=7,authorizationID=self.plan['authorizationId'],allowedCircuitSignatures=[]);payload.pop('modelBudget')
        envelope=sign(payload);Path(pin['path']).write_bytes(canonical(envelope));pin['sha256']=hashlib.sha256(canonical(envelope)).hexdigest();self.plan['exhibitionPermit']=pin
        self.api=API(self.plan);self.api.policy['metadata']['generation']=10
        self.api.policy['status']={'incidentSequence':7,'failureHistory':['preserved'],'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':10}]}
        self.adapter=FakeAdapter(self.api)
        self.clock=patch('tools.exhibition_permit.time.time',return_value=NOW);self.clock.start();self.addCleanup(self.clock.stop)
    def test_fixed_policy_observe_then_enforce_preserves_lifetime_state(self):
        status=copy.deepcopy(self.api.policy['status']);uid=self.api.policy['metadata']['uid']
        self.adapter.prepare(self.plan);self.assertEqual(self.api.mutations,[])
        cancel=threading.Event();self.adapter.observe_only(self.plan,self.api,cancel);self.adapter.phase='ARMED'
        grant=copy.deepcopy(self.plan['runtime']['executionGrant']);grant.update(heartbeatSequence=1,heartbeatAt=iso())
        self.adapter.grant_patch(self.plan,self.api,cancel,grant,activate=True)
        self.assertEqual(self.api.policy['metadata']['generation'],12);self.assertEqual(self.api.policy['metadata']['uid'],uid)
        self.assertEqual(self.api.policy['status'],status);self.assertEqual(self.api.faults,0)
        self.assertIn('opsloop.io/exhibition-permit',self.api.policy['metadata']['annotations'])
        grant.update(heartbeatSequence=2,heartbeatAt=iso(1));self.adapter.grant_patch(self.plan,self.api,cancel,grant)
        self.assertEqual(self.api.policy['status'],status)
    def test_sequence_and_stale_fence_or_changed_resource_input_block_preparation(self):
        self.api.policy['status']['incidentSequence']=8
        with self.assertRaisesRegex(RuntimeRejected,'BASELINE_POLICY_CHANGED'):self.adapter.prepare(self.plan)
        self.api.policy['status']['incidentSequence']=7;self.api.policy['status']['conditions'][0]['observedGeneration']=9
        with self.assertRaisesRegex(RuntimeRejected,'FENCE_UNCONFIRMED'):self.adapter.prepare(self.plan)
        self.assertEqual(self.api.mutations,[])

if __name__=='__main__':unittest.main()
