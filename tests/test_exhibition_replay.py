"""Local synthetic authority only: real input builder/issuer, no live API or model."""
import copy,datetime as dt,hashlib,json,sqlite3,tempfile,unittest
from pathlib import Path
from tools.exhibition_control import ControlError,canonical
from tools.exhibition_inputs import fresh_payload,pin
from tools.exhibition_hashes import describe
from tools.exhibition_replay import expected_history,approved_replay,pretransition_replay
from tools.exhibition_permit import issue,verify
from tests.test_exhibition_permit import IssuanceChainTests,fixture

class ReplayFixture:
 def __init__(self,root):
  self.root=root;self.now=dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
  self.chain=IssuanceChainTests();self.chain.root=root
  p,_,pin0,_,_,self.key,_=fixture(root,'3');self.public=pin0['publicKey'];self.chain.payload=p;self.chain.key=self.key
  migration=self.chain.bootstrap();self.chain.mint(p,migration,'predecessor')
  s=p['grant']['scope'];self.target={k:s[k] for k in ('namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}
  binding={k:s[k] for k in ('runID','epoch','policyUID','deploymentUID')}
  complete=self.chain.completion(binding);done=json.loads(complete.read_text());archive=Path(done['archiveManifest']);m=json.loads(archive.read_text())
  self.signature='b'*64;self.image='example/app@sha256:'+'d'*64
  inc={'metadata':{'namespace':s['namespace'],'uid':'previous-incident','annotations':{'opsloop.io/exhibition-run':s['runID'],'opsloop.io/exhibition-epoch':s['epoch']}},'spec':{'policyRef':{'uid':s['policyUID']},'workload':{'uid':s['deploymentUID']},'trigger':{'class':'APPLICATION_FAILURE','signature':self.signature}},'status':{'runtime':{'phase':'FAILED'}}}
  bundle={'incident':{'uid':'previous-incident','run_id':s['runID'],'epoch':s['epoch'],'runtime_signature':self.signature},'workload':{'namespace':s['namespace'],'policy_uid':s['policyUID'],'deployment_uid':s['deploymentUID']},'source':{'image_digest':'sha256:'+'d'*64}}
  self.refs={}
  for name,value in [('incident',inc),('bundle',bundle)]:
   path=self.put(name,value);h=pin(path)['sha256'];(archive.parent/h).write_bytes(path.read_bytes());m['objects' if name=='incident' else 'files'].append({'sha256':h});self.refs[name]=pin(path)
  archive.write_text(canonical(m));done['archiveSHA256']=pin(archive)['sha256'];complete.write_text(canonical(done));self.refs['completion']=pin(complete)
  dep={'apiVersion':'apps/v1','kind':'Deployment','metadata':{'namespace':s['namespace'],'name':s['deployment'],'uid':s['deploymentUID'],'generation':1},'spec':{'replicas':1,'revisionHistoryLimit':2,'strategy':{'type':'RollingUpdate','rollingUpdate':{'maxUnavailable':0,'maxSurge':1}},'template':{'metadata':{'labels':{'app':'fixture'}},'spec':{'containers':[{'name':'app','image':self.image,'env':[{'name':'X'+str(i)} for i in range(6)]}]}}},'status':{'observedGeneration':1,'replicas':1,'readyReplicas':1,'updatedReplicas':1,'availableReplicas':1}}
  pol={'metadata':{'uid':s['policyUID'],'generation':8},'spec':{'enabled':False},'status':{'incidentSequence':4,'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':8}],'circuits':[{'signature':self.signature}]}}
  rs={'metadata':{'uid':'baseline-rs','ownerReferences':[{'kind':'Deployment','controller':True,'uid':s['deploymentUID']}]},'spec':{'template':copy.deepcopy(dep['spec']['template'])}}
  quota={'items':[{'spec':{'hard':{'count/replicasets.apps':'5'}},'status':{'hard':{'count/replicasets.apps':'5'},'used':{'count/replicasets.apps':'1'}}}]}
  self.objects={'b-deployment':dep,'b-policy':pol,'b-replicasets':{'items':[rs]},'b-quotas':quota}
  self.target['baselineSpecHash']=describe(dep,'/spec')
  self.receipt={'state':'STATIC_CONTROLLER_READY','deploymentUID':'controller-uid','podUID':'new-pod','priorPodUID':'old-pod','instance':'c'*64,'expectedInstance':'c'*64,'priorInstance':'e'*64,'imageID':'example/controller@sha256:'+'f'*64}
  self.plan={'clusterUID':s['clusterUID'],'fixedTargets':{'b':self.target},'deployment':{'controller':{'uid':'controller-uid'},'imagePublication':{'digest':'sha256:'+'f'*64}},'newInterval':{'continuation':{'completion':self.refs['completion']}},'executionInputs':{'integration':{'issuanceState':str(root/'issuer')},'expectedCircuitHistory':{'b':{'initialSignatures':[],'additions':[{'signature':self.signature,'evidence':self.refs}]}}}}
  self.args=dict(policy_hash='a'*64,run_id='local-b2',epoch='local-epoch-b2',authorization_id='local-approval-b2',ordinal=4,previous_completion_sha=self.refs['completion']['sha256'],now=self.now)
  base=fresh_payload(self.plan,'b',self.objects,self.receipt,**self.args)
  self.intent={'schema':'opsloop-circuit-replay-intent/v1','approved':True,'purpose':'ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION','scope':base['grant']['scope'],'authorizationID':base['authorizationID'],'ordinal':4,'baseIncidentSequence':4,'maxIncidents':1,'maxActions':1,'evidence':self.refs,'workloadImage':self.image,'signature':self.signature,'previousCompletionSHA256':self.refs['completion']['sha256'],'notBefore':(self.now-dt.timedelta(seconds=10)).isoformat(),'notAfter':(self.now+dt.timedelta(minutes=30)).isoformat(),'controller':{k:self.receipt[k] for k in ('deploymentUID','imageID','priorInstance','priorPodUID')}}
 def put(self,name,value):
  p=self.root/(name+'.json');p.write_text(canonical(value));return p
 def approve(self):
  self.ref=pin(self.put('explicit-operator-intent',self.intent));self.plan['executionInputs']['circuitReplayApproval']={'b':self.ref};return self.ref
 def payload(self):return fresh_payload(self.plan,'b',self.objects,self.receipt,**self.args)
 def issue(self,p):
  pref=pin(self.put('payload',p));approval={'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],'authorizationID':p['authorizationID'],'expiresAt':p['grant']['notAfter'],'previousCompletion':self.refs['completion']['path'],'previousCompletionSHA256':self.refs['completion']['sha256'],'circuitReplayApproval':self.ref,'controllerReceipt':pin(self.put('controller-receipt',self.receipt))}
  ap=pin(self.put('issuer-approval',approval));out=self.root/'issued.json';issue(pref['path'],pref['sha256'],ap['path'],ap['sha256'],self.key,self.root/'issuer',out);return json.loads(out.read_text())

class ReplayTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.f=ReplayFixture(Path(self.tmp.name))
 def test_history_only_never_issues_exception(self):
  f=self.f;p=f.payload();self.assertEqual(p['allowedCircuitSignatures'],[]);self.assertNotIn('circuitReplay',p)
  with self.assertRaisesRegex(ControlError,'EXPLICIT'):pretransition_replay(f.plan,'b',f.objects,{'instance':'e'*64,'podUID':'old-pod'},'a'*64)
 def test_actual_b2_input_issuer_and_consumption(self):
  f=self.f;f.approve();a=pretransition_replay(f.plan,'b',f.objects,{'instance':'e'*64,'podUID':'old-pod'},'a'*64);self.assertEqual(a['ordinal'],4)
  p=f.payload();self.assertEqual(p['allowedCircuitSignatures'],[f.signature]);env=f.issue(p);self.assertEqual(verify(env,f.public),p)
  with self.assertRaises(ControlError):f.issue(p)
  with self.assertRaises(ControlError):pretransition_replay(f.plan,'b',f.objects,{'instance':'e'*64,'podUID':'old-pod'},'a'*64)
  db=sqlite3.connect(f.root/'issuer/issuance.sqlite3');self.assertEqual(db.execute('select count(*) from circuit_replays').fetchone()[0],1);db.close()
 def test_scope_and_instance_image_signature_mutations(self):
  f=self.f;f.approve();p=f.payload()
  for field in ('policyUID','deploymentUID','runID','epoch'):
   bad=copy.deepcopy(p);bad['grant']['scope'][field]='other'
   with self.subTest(field=field),self.assertRaises(ControlError):approved_replay(f.ref,bad,f.image,f.receipt)
  original_intent=copy.deepcopy(f.intent)
  for mode in ('instance','image','signature','expiry','old-receipt'):
   f.intent=copy.deepcopy(original_intent);f.approve()
   bad=copy.deepcopy(p);r=copy.deepcopy(f.receipt);image=f.image
   if mode=='instance':bad['grant']['instance']='old'
   if mode=='image':image='example/other@sha256:'+'d'*64
   if mode=='signature':f.intent['signature']='a'*64;f.approve()
   if mode=='expiry':f.intent['notAfter']='2000-01-01T00:00:00Z';f.approve()
   if mode=='old-receipt':r['instance']=r['priorInstance']
   with self.subTest(mode=mode),self.assertRaises(ControlError):approved_replay(f.ref,bad,image,r)
 def test_tampered_prior_evidence_and_revoked_approval(self):
  f=self.f;f.approve();p=f.payload();Path(f.refs['bundle']['path']).write_text('{}')
  with self.assertRaises(ControlError):f.payload()
 def test_approval_revocation_and_missing_receipt(self):
  f=self.f;f.approve();p=f.payload();old=copy.deepcopy(f.ref)
  with self.assertRaises(ControlError):approved_replay(f.ref,p,f.image,None)
  f.intent['approved']=False;f.approve()
  with self.assertRaises(ControlError):approved_replay(old,p,f.image,f.receipt)
  with self.assertRaises(ControlError):approved_replay(f.ref,p,f.image,f.receipt)
 def test_public_event_requires_verified_permit_and_does_not_claim_recovery(self):
  from tools.exhibition_control import Coordinator
  from tests.test_exhibition_control import Adapter,SyntheticLifecycle
  f=self.f;f.approve();payload=f.payload();f.issue(payload)
  scope=payload['grant']['scope'];adapter=Adapter()
  plan={'schema':'opsloop-exhibition-plan/v1','planId':'local-plan','authorizationId':payload['authorizationID'],'track':'engineering','source':'measured','binding':{k:scope[k] for k in ('runID','epoch','policyUID','deploymentUID')},'expiresAt':payload['modelBudget']['expiresAt'],'totalSeconds':900,'approvalReference':'SYNTHETIC_ONLY','exhibitionPermit':{**pin(f.root/'issued.json'),'publicKey':f.public}}
  ref=pin(f.put('coordinator-plan',plan));c=Coordinator(f.root/'control',{'schema':'opsloop-exhibition-control/v1','plans':[ref]},{'engineering':adapter},lifecycle=SyntheticLifecycle())
  try:
   c.prepare('local-plan');c.start('local-plan','single-request');adapter.release.set();c.worker.join(2)
   events=c.events(scope['runID'])['events'];matches=[v for v in events if v['kind']=='EXHIBITION_REPLAY_APPROVED']
   self.assertEqual(len(matches),1);self.assertEqual(matches[0]['actor'],'operator');self.assertEqual(matches[0]['data']['sourceSHA256'],f.ref['sha256']);self.assertEqual(matches[0]['data']['reasonCode'],'NOT_OPERATIONAL_CIRCUIT_RECOVERY')
   self.assertNotIn('evidence',matches[0]['data']);self.assertNotIn('SERVICE_RECOVERED',[v['kind'] for v in events])
  finally:adapter.release.set();c.close()
 def test_no_automatic_next_signature(self):
  f=self.f;f.approve();before=copy.deepcopy(f.plan);f.objects['b-policy']['status']['circuits'].append({'signature':'a'*64})
  with self.assertRaises(ControlError):f.payload()
  self.assertEqual(f.plan,before)
 def test_false_approval_and_changed_target_before_transition(self):
  f=self.f;f.intent['approved']=False;f.approve()
  with self.assertRaises(ControlError):f.payload()
  f.intent['approved']=True;f.approve();f.objects['b-policy']['metadata']['uid']='wrong'
  with self.assertRaises(ControlError):pretransition_replay(f.plan,'b',f.objects,{'instance':'e'*64,'podUID':'old-pod'},'a'*64)
 def test_baseline_fence_quota_still_required(self):
  f=self.f;f.approve();original=copy.deepcopy(f.objects)
  for mode in ('ready','fence','quota'):
   f.objects=copy.deepcopy(original)
   if mode=='ready':f.objects['b-deployment']['status']['readyReplicas']=0
   if mode=='fence':f.objects['b-policy']['status']['conditions']=[]
   if mode=='quota':f.objects['b-quotas']['items'][0]['status']['used']['count/replicasets.apps']='5'
   with self.subTest(mode=mode),self.assertRaises(ControlError):f.payload()

if __name__=='__main__':unittest.main()
