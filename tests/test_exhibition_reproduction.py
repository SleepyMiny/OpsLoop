"""Synthetic same-ledger B1 -> failed B2 -> explicit v3 reproduction. No live authority."""
import copy,json,sqlite3,tempfile,unittest
from pathlib import Path
from tools.exhibition_control import canonical,ControlError
from tools.exhibition_inputs import pin
from tools.exhibition_hashes import describe
from tools.exhibition_permit import validate_payload,verify
from tests.test_exhibition_evidence_roles import V2Fixture
from tests import test_exhibition_evidence_roles as roles

def prepare_readiness(f):
 # Synthetic historical classification is sealed BEFORE this completion is consumed.
 inc=json.loads(Path(f.refs['incident']['path']).read_text())
 inc['spec']['trigger'].update(reason='POD_NOT_READY',ruleVersion='classify-readiness-v1')
 old=f.refs['incident']['sha256'];Path(f.refs['incident']['path']).write_text(canonical(inc));f.refs['incident']=pin(f.refs['incident']['path'])
 done=json.loads(Path(f.refs['completion']['path']).read_text());archive=Path(done['archiveManifest']);m=json.loads(archive.read_text())
 for obj in m['objects']:
  if obj['sha256']==old:obj['sha256']=f.refs['incident']['sha256']
 (archive.parent/f.refs['incident']['sha256']).write_text(canonical(inc));archive.write_text(canonical(m));done['archiveSHA256']=pin(archive)['sha256'];Path(f.refs['completion']['path']).write_text(canonical(done));f.refs['completion']=pin(f.refs['completion']['path'])
 f.plan['newInterval']['continuation']['completion']=f.refs['completion'];f.intent['evidence']=f.refs;f.intent['previousCompletionSHA256']=f.refs['completion']['sha256'];f.args['previous_completion_sha']=f.refs['completion']['sha256']
 dep=f.objects['b-deployment'];c=dep['spec']['template']['spec']['containers'][0];c['env'][5]={'name':'DEMO_B_REPLAY','value':''};c['readinessProbe']={'httpGet':{'path':'/ready','port':8080}}
 f.objects['b-replicasets']['items'][0]['spec']['template']=copy.deepcopy(dep['spec']['template']);f.target['baselineSpecHash']=describe(dep,'/spec')

class V3Fixture(V2Fixture):
 def __init__(self,root,policy_hash='a'*64):
  super().__init__(root,policy_hash,prepare_readiness)
  baseline=copy.deepcopy(self.objects['b-deployment']['spec']['template']);fault=copy.deepcopy(baseline);fault['spec']['containers'][0]['env'][5]['value']='empty-order-after-30s'
  s=self.intent['scope'];self.intent['schema']='opsloop-circuit-replay-intent/v3';self.intent['approvedReproduction']={'schema':'opsloop-approved-reproduction/v1','kind':'DEMO_B_EMPTY_ORDER_READINESS','policyUID':s['policyUID'],'deploymentUID':s['deploymentUID'],'workloadImage':self.image,'containerName':'app','input':{'name':'DEMO_B_REPLAY','value':'empty-order-after-30s'},'classification':{'class':'APPLICATION_FAILURE','reason':'POD_NOT_READY','ruleVersion':'classify-readiness-v1'},'baselineTemplate':baseline,'template':fault};self.approve()

class ReproductionTests(unittest.TestCase):
 def setUp(self):self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.f=V3Fixture(Path(self.tmp.name))
 def test_input_issuer_exact_three_roles_same_ledger(self):
  f=self.f;p=f.payload();self.assertEqual(p['schema'],'opsloop-exhibition-permit/v3');self.assertEqual(verify(f.issue_v2(p),f.public),p)
  self.assertNotEqual(p['circuitEvidence']['completionSHA256'],p['predecessorCompletion']['completionSHA256'])
  self.assertEqual(p['approvedReproduction']['template'],f.intent['approvedReproduction']['template'])
  with sqlite3.connect(f.root/'issuer/issuance.sqlite3') as db:self.assertEqual(db.execute('select count(*) from permits').fetchone()[0],3)
  with self.assertRaises(ControlError):f.issue_v2(p,'again')
 def test_concurrent_v3_issuance_one_winner(self):
  roles.EvidenceRoleTests.test_concurrent_consumption_has_only_one_winner(self)
 def test_v3_old_predecessor_and_retirement_rejected(self):
  roles.EvidenceRoleTests.test_old_b1_predecessor_reuse_denied(self)
 def test_bad_scope_and_legacy_projection_denied(self):
  p=self.f.payload()
  for mode in ('policy','deployment','image','container','input','class','probe','env','v1','v2','hash'):
   bad=copy.deepcopy(p);v=bad['approvedReproduction']
   if mode in ('policy','deployment'):v[mode+'UID']='other'
   elif mode=='image':v['workloadImage']='other'
   elif mode=='container':v['containerName']='other'
   elif mode=='input':v['input']['value']='other'
   elif mode=='class':v['classification']['reason']='HTTP_500'
   elif mode=='probe':v['template']['spec']['containers'][0]['readinessProbe']['httpGet']['path']='/other'
   elif mode=='env':v['template']['spec']['containers'][0]['env'][0]['value']='other'
   elif mode in ('v1','v2'):bad['schema']='opsloop-exhibition-permit/'+mode
   else:bad['newExecutionAuthorization']['approvedReproductionSHA256']='0'*64
   with self.subTest(mode=mode),self.assertRaises(ControlError):validate_payload(bad)
 def test_baseline_changed_and_no_explicit_approval_denied(self):
  f=self.f;f.objects['b-deployment']['spec']['template']['spec']['containers'][0]['env'][0]['value']='other'
  with self.assertRaises(ControlError):f.payload()
  from tools.exhibition_replay import pretransition_replay
  with self.assertRaisesRegex(ControlError,'REPRODUCTION_BASELINE_TEMPLATE_CHANGED'):pretransition_replay(f.plan,'b',f.objects,{'instance':f.receipt['priorInstance'],'podUID':f.receipt['priorPodUID']},f.args['policy_hash'])
  f.intent['approved']=False;f.approve()
  with self.assertRaises(ControlError):f.payload()

if __name__=='__main__':unittest.main()
