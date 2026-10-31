"""Synthetic local B1 -> failed B2 -> newly approved run, one durable issuer ledger.
Runtime/cleanup fixtures are explicitly synthetic; real stored evidence is read only.
"""
import copy,json,sqlite3,tempfile,unittest,concurrent.futures
from pathlib import Path
from tools.exhibition_control import ControlError,canonical
from tools.exhibition_inputs import pin,fresh_payload
from tools.exhibition_permit import issue,verify,validate_payload
from tools.exhibition_replay import evidence,verified_predecessor,pretransition_replay
from tests.test_exhibition_replay import ReplayFixture

class V2Fixture(ReplayFixture):
 def __init__(self,root,policy_hash='a'*64,prepare=None):
  super().__init__(root)
  if prepare:prepare(self)
  self.args['policy_hash']=policy_hash;self.intent['scope']['policyHash']=policy_hash
  # B1 source completion predates B2 issuance; make its terminal state explicit.
  b1done=json.loads(Path(self.refs['completion']['path']).read_text());b1marker=Path(self.refs['completion']['path']).parent/'authority-retirement.json'
  b1marker.write_text(canonical({'state':'RETIRED','binding':b1done['binding']}));b1done['retiredAuthoritySHA256']=pin(b1marker)['sha256'];Path(self.refs['completion']['path']).write_text(canonical(b1done))
  self.refs['completion']=pin(self.refs['completion']['path']);self.plan['newInterval']['continuation']['completion']=self.refs['completion'];self.intent['previousCompletionSHA256']=self.refs['completion']['sha256'];self.args['previous_completion_sha']=self.refs['completion']['sha256']
  self.approve();self.b2=self.payload();self.b2env=self.issue(self.b2)
  binding={k:self.b2['grant']['scope'][k] for k in ('runID','epoch','policyUID','deploymentUID')}
  completion=self.chain.completion(binding);done=json.loads(completion.read_text());authority=Path(done['archiveManifest']).parent/'authority-retirement.json'
  authority.write_text(canonical({'state':'RETIRED','binding':binding}));done['retiredAuthoritySHA256']=pin(authority)['sha256'];completion.write_text(canonical(done))
  self.predecessor=pin(completion);self.b1source=copy.deepcopy(self.refs)
  self.args.update(run_id='local-b2-revalidated',epoch='local-epoch-revalidated',authorization_id='local-approval-revalidated',previous_completion_sha=self.predecessor['sha256'])
  self.objects['b-policy']['metadata']['generation']+=3
  self.objects['b-policy']['status']['conditions'][0]['observedGeneration']=self.objects['b-policy']['metadata']['generation']
  self.receipt.update(priorInstance=self.receipt['instance'],priorPodUID=self.receipt['podUID'],instance='d'*64,expectedInstance='d'*64,podUID='revalidation-pod')
  self.plan['newInterval']['continuation']['predecessorCompletion']=self.predecessor
  self.intent=copy.deepcopy(self.intent);self.intent['schema']='opsloop-circuit-replay-intent/v2';self.intent['circuitEvidence']=self.intent.pop('evidence');self.intent.pop('previousCompletionSHA256');self.intent['predecessorCompletion']=self.predecessor
  self.intent['scope'].update(runID=self.args['run_id'],epoch=self.args['epoch'],policyGeneration=self.objects['b-policy']['metadata']['generation']+2)
  self.intent['authorizationID']=self.args['authorization_id'];self.intent['controller']={k:self.receipt[k] for k in ('deploymentUID','imageID','priorInstance','priorPodUID')}
  self.approve()
 def issue_v2(self,p,label='v2'):
  pref=pin(self.put(label+'-payload',p));a={'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],'authorizationID':p['authorizationID'],'expiresAt':p['grant']['notAfter'],'previousCompletion':self.predecessor['path'],'previousCompletionSHA256':self.predecessor['sha256'],'circuitReplayApproval':self.ref,'controllerReceipt':pin(self.put(label+'-controller',self.receipt))}
  ap=pin(self.put(label+'-approval',a));out=self.root/(label+'-issued.json');issue(pref['path'],pref['sha256'],ap['path'],ap['sha256'],self.key,self.root/'issuer',out);return json.loads(out.read_text())

class EvidenceRoleTests(unittest.TestCase):
 def setUp(self):self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.f=V2Fixture(Path(self.tmp.name))
 def test_actual_input_issuer_same_ledger_after_failed_predecessor(self):
  f=self.f;pretransition_replay(f.plan,'b',f.objects,{'instance':f.receipt['priorInstance'],'podUID':f.receipt['priorPodUID']},f.args['policy_hash'])
  p=f.payload();self.assertNotEqual(p['circuitEvidence']['completionSHA256'],p['predecessorCompletion']['completionSHA256']);self.assertEqual(p['circuitEvidence']['completionSHA256'],f.b1source['completion']['sha256'])
  self.assertEqual(verify(f.issue_v2(p),f.public),p)
  with sqlite3.connect(f.root/'issuer/issuance.sqlite3') as db:
   self.assertEqual(db.execute('select count(*) from permits').fetchone()[0],3);self.assertEqual(db.execute('select count(*) from completion_chain where completion=?',(f.b1source['completion']['sha256'],)).fetchone()[0],1)
  with self.assertRaises(ControlError):f.issue_v2(p,'duplicate')
 def test_old_b1_predecessor_reuse_denied(self):
  f=self.f;f.intent['predecessorCompletion']=f.b1source['completion'];f.plan['newInterval']['continuation']['predecessorCompletion']=f.b1source['completion'];f.approve()
  with self.assertRaisesRegex(ControlError,'PREDECESSOR_ALREADY_CONSUMED'):pretransition_replay(f.plan,'b',f.objects,{'instance':f.receipt['priorInstance'],'podUID':f.receipt['priorPodUID']},f.args['policy_hash'])
 def test_retirement_and_safe_completion_required(self):
  f=self.f;original=json.loads(Path(f.predecessor['path']).read_text());authority=Path(original['archiveManifest']).parent/'authority-retirement.json'
  for mode in ('missing-cleanup','not-retired','wrong-binding'):
   done=copy.deepcopy(original);value={'state':'RETIRED','binding':done['binding']}
   if mode=='missing-cleanup':done['objectsCleaned']=False
   if mode=='not-retired':value['state']='ISSUED'
   if mode=='wrong-binding':value['binding']={**done['binding'],'epoch':'wrong'}
   authority.write_text(canonical(value));done['retiredAuthoritySHA256']=pin(authority)['sha256'];ref=pin(f.put('bad-'+mode,done));(f.root/'authority-retirement.json').write_text(canonical(value))
   with self.subTest(mode=mode),self.assertRaises(ControlError):verified_predecessor(ref,f.intent['scope'])
 def test_missing_terminal_marker_denied(self):
  f=self.f;done=json.loads(Path(f.predecessor['path']).read_text());ref=pin(f.put('missing-terminal-marker',done))
  with self.assertRaisesRegex(ControlError,'RETIREMENT_MISSING'):verified_predecessor(ref,f.intent['scope'])
 def test_other_target_and_tampered_roles_denied(self):
  f=self.f;p=f.payload()
  for role,field,value in [('circuitEvidence','policyUID','other'),('predecessorCompletion','deploymentUID','other'),('newExecutionAuthorization','approvalSHA256','0'*64),('predecessorCompletion','completionSHA256','f'*64)]:
   bad=copy.deepcopy(p);bad[role][field]=value
   with self.subTest(role=role,field=field),self.assertRaises(ControlError):validate_payload(bad)
  bad=copy.deepcopy(p);bad['schema']='opsloop-exhibition-permit/v1'
  with self.assertRaises(ControlError):validate_payload(bad)
 def test_no_new_approval_and_source_tamper_denied(self):
  f=self.f;f.intent['approved']=False;f.approve()
  with self.assertRaises(ControlError):f.payload()
  f.intent['approved']=True;f.approve();Path(f.b1source['bundle']['path']).write_text('{}')
  with self.assertRaises(ControlError):f.payload()
 def test_concurrent_consumption_has_only_one_winner(self):
  f=self.f;requests=[]
  for n in (1,2):
   f.args.update(run_id='parallel-run-'+str(n),epoch='parallel-epoch-'+str(n),authorization_id='parallel-approval-'+str(n))
   f.intent['scope'].update(runID=f.args['run_id'],epoch=f.args['epoch']);f.intent['authorizationID']=f.args['authorization_id'];f.approve();p=f.payload()
   pref=pin(f.put('parallel-payload-'+str(n),p));a={'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],'authorizationID':p['authorizationID'],'expiresAt':p['grant']['notAfter'],'previousCompletion':f.predecessor['path'],'previousCompletionSHA256':f.predecessor['sha256'],'circuitReplayApproval':copy.deepcopy(f.ref),'controllerReceipt':pin(f.put('parallel-controller-'+str(n),f.receipt))}
   # Persist each approval's exact intent independently before the next one.
   refpath=f.put('parallel-intent-'+str(n),copy.deepcopy(f.intent));a['circuitReplayApproval']=pin(refpath)
   # Same canonical intent bytes => identical pin hash; path is not signed.
   ap=pin(f.put('parallel-approval-'+str(n),a));requests.append((pref,ap,n))
  def attempt(req):
   pref,ap,n=req
   try:issue(pref['path'],pref['sha256'],ap['path'],ap['sha256'],f.key,f.root/'issuer',f.root/('parallel-issued-'+str(n)));return 'ISSUED'
   except ControlError:return 'REJECTED'
  with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:results=list(pool.map(attempt,requests))
  self.assertEqual(sorted(results),['ISSUED','REJECTED'])

if __name__=='__main__':unittest.main()
