"""Synthetic Kubernetes state interleavings, no live calls or operating permits.
Controller-produced counterpart is TestHeartbeatControllerProgress (envtest).
"""
import base64
import copy
import datetime as dt
import json
import unittest
from unittest.mock import patch
from tests import test_exhibition_heartbeat as hb
from tools import exhibition_demo_b_trial as core
from tools.exhibition_heartbeat_progress import Progress, conditions


class ProgressTests(hb.HeartbeatTests):
 def setUp(self):
  super().setUp()
  from tests.policy_snapshot_fixture import policy_spec
  self.obj["spec"]=policy_spec();self.p["enforcePolicySpec"]=copy.deepcopy(self.obj["spec"])
  self.scope.update(namespace='opsloop-demo-b', policy='engineering-demo', deployment='engineering-demo', deploymentUID='dep-uid',policyGeneration=12,policyHash='policy-hash',incident='engineering-demo-test-pol-00003')
  self.fixed['scope']=copy.deepcopy(self.scope)
  self.grant['scope']=self.scope
  self.wire['annotation']=json.dumps(self.grant);self.obj['metadata']['annotations'][core.GRANT]=self.wire['annotation']
  self.image='registry/app@sha256:'+'a'*64
  self.baseline={'metadata':{'labels':{'app':'demo'}},'spec':{'containers':[{'name':'app','image':self.image,'env':[{'name':'FAULT','value':'off'}]}]}}
  self.fault=copy.deepcopy(self.baseline);self.fault['spec']['containers'][0]['env'][0]['value']='on'
  self.payload={'schema':'opsloop-exhibition-permit/v3','grant':self.fixed,'baseIncidentSequence':2,'approvedReproduction':{'policyUID':'test-policy','deploymentUID':'dep-uid','containerName':'app','workloadImage':self.image,'baselineTemplate':self.baseline,'template':self.fault,'classification':{'class':'Application','reason':'POD_NOT_READY','ruleVersion':'classify-readiness-v1'}}}
  self.envelope={'payload':base64.b64encode(core.canonical(self.payload)).decode(),'signature':'synthetic-not-operating-signature'}
  self.obj['metadata']['annotations'][core.EXHIBITION_PERMIT]=core.canonical(self.envelope).decode()
  patch.object(core,'exhibition_permit',side_effect=lambda p:(self.envelope,self.payload)).start()
  self.deployment={'metadata':{'namespace':core.NS,'name':core.APP,'uid':'dep-uid','annotations':{'deployment.kubernetes.io/revision':'2','opsloop.io/execution-fence':json.dumps({'version':1,'policyUID':'test-policy','policyName':core.APP,'deploymentUID':'dep-uid','closedThroughGeneration':10})}},'spec':{'replicas':1,'template':copy.deepcopy(self.fault)}}
  self.p['sourceDeploymentSpec']={'replicas':1,'template':copy.deepcopy(self.baseline)}
  self.rs={'metadata':{'name':'rs','uid':'rs-uid','namespace':core.NS,'annotations':{'deployment.kubernetes.io/revision':'2'},'ownerReferences':[{'controller':True,'kind':'Deployment','name':core.APP,'uid':'dep-uid'}]},'spec':{'template':copy.deepcopy(self.fault)}}
  self.pod={'metadata':{'name':'source-pod','uid':'source-uid','namespace':core.NS,'ownerReferences':[{'controller':True,'kind':'ReplicaSet','name':'rs','uid':'rs-uid'}]},'spec':{'containers':copy.deepcopy(self.fault['spec']['containers']),'nodeName':'node'},'status':{'phase':'Running','conditions':[{'type':'Ready','status':'False'}],'containerStatuses':[{'name':'app','containerID':'containerd://runtime','imageID':self.image,'restartCount':0,'ready':False,'state':{'running':{}}}]}}
  self.node={'metadata':{'name':'node','uid':'node-uid'},'status':{'conditions':[{'type':'Ready','status':'True'}]}}
  self.obj['status']['observedGeneration']=12
  at=self.now.isoformat()
  self.obj['status']['conditions']=[{'type':'Monitoring','status':'True','reason':'Active','message':'','lastTransitionTime':at}, {'type':'Quiesced','status':'False','reason':'ExecutionEnabled','observedGeneration':12,'message':'','lastTransitionTime':at}, {'type':'ReproductionSource','status':'True','reason':'SourceBound','observedGeneration':12,'message':'Live approved source identity bound; readiness classification remains independently required','lastTransitionTime':at}, {'type':'ReadinessObserved','status':'False','reason':'AwaitingHealthyPod','observedGeneration':12,'message':'Prior healthy sample for this Policy generation/template only; not execution authorization','lastTransitionTime':at}]
  self.incident=None
  def get(path,absent=False):
   if path==core.POLICY:return copy.deepcopy(self.obj)
   if path==core.DEPLOY:return copy.deepcopy(self.deployment)
   if path=='/api/v1/nodes/node':return copy.deepcopy(self.node)
   if '/recoveryincidents/' in path:return copy.deepcopy(self.incident)
   raise AssertionError(path)
  self.c.get=get
  self.c.listing=lambda path:{'items':copy.deepcopy([self.rs] if path.endswith('/replicasets') else [self.pod])}
 def benign(self):
  self.obj["metadata"]["resourceVersion"]=str(int(self.obj["metadata"]["resourceVersion"])+1)
  next(x for x in self.obj["status"]["conditions"] if x["type"]=="Monitoring")["lastTransitionTime"]=(self.now+dt.timedelta(seconds=1)).isoformat()
 def readiness(self):
  self.benign()
  v=next(x for x in self.obj['status']['conditions'] if x['type']=='ReadinessObserved')
  v.update(status='True',reason='HealthyPodObserved')
 def test_actual_pattern_readiness_progress_two_requests_same_notready_source(self):
  before=copy.deepcopy(self.obj);self.hooks=[self.readiness]
  _,grant,_=self.renew();self.assertEqual(len(self.sent),2)
  self.assertNotEqual(core.heartbeat_state(before),core.heartbeat_state(self.obj))
  self.assertEqual(grant['heartbeatAt'],self.now.isoformat())
  custody=json.loads((self.root/'heartbeat-progress-binding.json').read_bytes())
  self.assertEqual(custody['source']['sourcePodUID'],'source-uid')
  self.assertFalse(self.pod['status']['containerStatuses'][0]['ready'])
 def test_condition_array_reorder_is_not_identity_change(self):
  self.hooks=[lambda:(self.readiness(),self.obj['status']['conditions'].reverse())]
  self.renew();self.assertEqual(len(self.sent),2)
 def test_readiness_change_and_permit_withdrawal(self):
  self.changed_on_conflict(lambda:(self.readiness(),self.obj['metadata']['annotations'].pop(core.EXHIBITION_PERMIT)))
 def test_unknown_condition_even_without_conflict(self):
  self.obj['status']['conditions'].append({'type':'Unspecified','status':'True'})
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])
 def test_duplicate_condition_even_unchanged(self):
  self.obj['status']['conditions'].append(copy.deepcopy(self.obj['status']['conditions'][3]))
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])
 def test_readiness_generation_mismatch(self):self.changed_on_conflict(lambda:self.obj['status']['conditions'][3].update(observedGeneration=11))
 def test_source_pod_replacement(self):self.changed_on_conflict(lambda:self.pod['metadata'].update(uid='other-source'))
 def test_source_runtime_replacement(self):self.changed_on_conflict(lambda:self.pod['status']['containerStatuses'][0].update(containerID='different'))
 def test_source_rs_replacement(self):self.changed_on_conflict(lambda:self.rs['metadata'].update(uid='other-rs'))
 def test_source_rejected(self):self.changed_on_conflict(lambda:self.obj['status']['conditions'][2].update(status='False',reason='SourceRejected'))
 def test_node_not_ready(self):self.changed_on_conflict(lambda:self.node['status']['conditions'][0].update(status='False'))
 def test_fence_closed(self):
  def change():
   f=json.loads(self.deployment['metadata']['annotations']['opsloop.io/execution-fence']);f['closedThroughGeneration']=12;self.deployment['metadata']['annotations']['opsloop.io/execution-fence']=json.dumps(f)
  self.changed_on_conflict(change)
 def test_template_env_changed(self):self.changed_on_conflict(lambda:self.deployment['spec']['template']['spec']['containers'][0]['env'][0].update(value='different'))
 def test_template_container_image_changed(self):self.changed_on_conflict(lambda:self.pod['spec']['containers'][0].update(image='different'))
 def test_fresh_read_failure_blocks_retry(self):
  original=self.c.get
  def change():
   self.c.get=lambda path,absent=False:(_ for _ in ()).throw(core.Stop('read unavailable')) if path==core.DEPLOY else original(path,absent)
  self.changed_on_conflict(change)
 def test_custody_survives_next_heartbeat_process(self):
  self.renew();self.pod['metadata']['uid']='other'
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  with self.assertRaisesRegex(core.Stop,'source changed'):progress.observe(self.obj)
 def test_pending_to_bound_without_authority_reissue(self):
  self.obj['status']['conditions'][2].update(status='Unknown',reason='SourcePending',message='Approved source runtime identity pending; no Incident or recovery action authorized')
  self.obj['status']['conditions'][0].update(status='False',reason='REPRODUCTION_SOURCE_PENDING',message='Awaiting approved source runtime identity; no Incident or recovery action authorized')
  status=copy.deepcopy(self.pod['status']);self.pod['status']['containerStatuses']=[]
  def ready():
   self.benign();self.pod['status']=status
   self.obj['status']['conditions'][2].update(status='True',reason='SourceBound',message='Live approved source identity bound; readiness classification remains independently required')
   self.obj['status']['conditions'][0].update(status='True',reason='Active',message='')
  self.hooks=[ready];self.renew();self.assertEqual(len(self.sent),2)
 def test_bound_to_pending_rejected(self):self.changed_on_conflict(lambda:self.obj['status']['conditions'][2].update(status='Unknown',reason='SourcePending'))

 def incident_progress(self, terminal=False):
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope);progress.observe(self.obj)
  occurrence={**progress.custody['source'],**{k:self.scope[k] for k in ('runID','epoch','policyUID','deploymentUID')},'healthyObservedAt':(self.now-dt.timedelta(seconds=1)).isoformat(),'signature':'b'*64}
  trig={'class':'Application','reason':'POD_NOT_READY','ruleVersion':'classify-readiness-v1','podUID':occurrence['sourcePodUID'],'podName':occurrence['sourcePodName'],'nodeUID':occurrence['nodeUID'],'nodeName':occurrence['nodeName'],'signature':occurrence['signature'],'observedAt':self.now.isoformat(),'evidence':['POD_NOT_READY podUID='+occurrence['sourcePodUID']+' containerIdentitySHA256='+occurrence['containerIdentitySHA256']]}
  spec={'policyRef':{'name':core.APP,'uid':'test-policy'},'workload':{'name':core.APP,'uid':'dep-uid'},'policyGeneration':12,'policyHash':'policy-hash','policy':copy.deepcopy(self.obj['spec']),'sequence':3,'trigger':trig,'baseline':{'image':self.image,'replicas':1},'detectedAt':self.now.isoformat()}
  self.incident={'metadata':{'name':self.scope['incident'],'namespace':core.NS,'uid':'inc-uid','annotations':{'opsloop.io/execution-grant-binding':self.wire['binding'],'opsloop.io/exhibition-run':self.p['runID'],'opsloop.io/exhibition-epoch':self.p['epoch'],'opsloop.io/exhibition-permit-sha256':core.sha(core.canonical(self.payload)),'opsloop.io/current-exhibition-occurrence':json.dumps(occurrence)}},'spec':spec,'status':{'runtime':{'phase':'WAITING'}}}
  self.obj['status']['incidentSequence']=3
  self.obj['status']['activeIncident']={'name':self.scope['incident'],'sequence':3,'workloadUID':'dep-uid','reservedAt':spec['detectedAt'],'trigger':trig,'baseline':spec['baseline']}
  self.obj['status']['conditions'][0].update(reason='IncidentActive',message=self.scope['incident'])
  if terminal:
   self.incident['status']['runtime'].update(phase='FAILED',completedAt=self.now.isoformat())
   self.obj['status'].pop('activeIncident');self.obj['status']['conditions'][0].update(reason='Active',message='')
   self.obj['status']['circuits']=[{'signature':trig['signature'],'class':trig['class'],'incident':self.scope['incident'],'phase':'FAILED','openedAt':self.now.isoformat()}]
 def test_incident_duration_semantics_after_realistic_serialization(self):
  self.incident_progress()
  self.incident['spec']['policy']['budget']['cooldown']='2m0s'
  self.assertNotEqual(self.incident['spec']['policy'],self.obj['spec'])
  self.renew();self.assertEqual(len(self.sent),1)
 def test_snapshot_duration_or_unknown_rejected_without_patch(self):
  self.incident_progress();original=copy.deepcopy(self.incident)
  for field,value in [('cooldown','121s'),('cooldown',None),('newDuration','120s'),('maxActionsPerIncident',2)]:
   with self.subTest(field=field,value=value):
    self.incident=copy.deepcopy(original);self.incident['spec']['policy']['budget'][field]=value
    with self.assertRaises(core.Stop):self.renew()
    self.assertEqual(self.sent,[])
 def test_duration_equivalence_does_not_admit_aborted_run(self):
  self.incident_progress();self.incident['spec']['policy']['budget']['cooldown']='2m0s'
  self.incident['status']['runtime'].update(phase='ABORTED',reason='POLICY_DISABLED')
  with self.assertRaisesRegex(core.Stop,'stopped or unknown'):self.renew()
  self.assertEqual(self.sent,[])
 def test_reservation_then_uid_confirmation(self):
  def reserve():self.benign();self.incident_progress()
  self.hooks=[reserve];self.renew();self.assertEqual(len(self.sent),2)
 def test_terminal_release_proved_circuit_entry(self):
  self.incident_progress(True)
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  progress.observe(self.obj)
  self.assertEqual(self.sent,[])
 def test_unproved_circuit_entry_rejected(self):
  self.changed_on_conflict(lambda:self.obj['status'].update(circuits=[{'signature':'other'}]))
 def test_incident_other_run_rejected(self):
  def change():
   self.incident_progress();self.incident['metadata']['annotations']['opsloop.io/exhibition-run']='other'
  self.changed_on_conflict(change)
 def test_incident_other_source_rejected(self):
  def change():
   self.incident_progress();v=json.loads(self.incident['metadata']['annotations']['opsloop.io/current-exhibition-occurrence']);v['sourcePodUID']='other';self.incident['metadata']['annotations']['opsloop.io/current-exhibition-occurrence']=json.dumps(v)
  self.changed_on_conflict(change)
 def test_source_binding_regression_across_heartbeats(self):
  self.renew();self.obj['status']['conditions'][2].update(status='Unknown',reason='SourcePending',message='Approved source runtime identity pending; no Incident or recovery action authorized')
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  with self.assertRaises(core.Stop):progress.observe(self.obj)

 def test_success_response_then_source_rejection_not_confirmed(self):
  original=self.send
  def send(*args):
   value=original(*args)
   self.obj['status']['conditions'][2].update(status='False',reason='SourceRejected')
   return value
  with patch('tools.exhibition_heartbeat.send',side_effect=send),self.assertRaises(core.Stop):self.renew()
  self.assertEqual(len(self.sent),1)
 def test_duplicate_fence_json_rejected(self):
  raw=self.deployment['metadata']['annotations']['opsloop.io/execution-fence']
  self.deployment['metadata']['annotations']['opsloop.io/execution-fence']=raw[:-1]+',"version":1}'
  with self.assertRaisesRegex(core.Stop,'duplicate JSON'):self.renew()
  self.assertEqual(self.sent,[])
 def test_duplicate_node_ready_rejected(self):
  self.node['status']['conditions'].append({'type':'Ready','status':'False'})
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])

 def test_baseline_armed_history_reset_on_first_source_binding(self):
  self.obj['status']['conditions'][2].update(status='Unknown',reason='SourcePending',message='Approved source runtime identity pending; no Incident or recovery action authorized')
  self.obj['status']['conditions'][0].update(status='False',reason='REPRODUCTION_SOURCE_PENDING',message='Awaiting approved source runtime identity; no Incident or recovery action authorized')
  self.obj['status']['conditions'][3].update(status='True',reason='HealthyPodObserved')
  statuses=copy.deepcopy(self.pod['status']['containerStatuses']);self.pod['status']['containerStatuses']=[]
  def bound():
   self.benign();self.pod['status']['containerStatuses']=statuses
   self.obj['status']['conditions'][2].update(status='True',reason='SourceBound',message='Live approved source identity bound; readiness classification remains independently required')
   self.obj['status']['conditions'][0].update(status='True',reason='Active',message='')
   self.obj['status']['conditions'][3].update(status='False',reason='AwaitingHealthyPod')
  self.hooks=[bound];self.renew();self.assertEqual(len(self.sent),2)
 def test_bound_healthy_history_regression_rejected(self):
  self.obj['status']['conditions'][3].update(status='True',reason='HealthyPodObserved')
  self.changed_on_conflict(lambda:self.obj['status']['conditions'][3].update(status='False',reason='AwaitingHealthyPod'))

 def test_missing_initial_condition_blocks_all_dispatch(self):
  for typ in ('Monitoring','Quiesced','ReadinessObserved','ReproductionSource'):
   with self.subTest(condition=typ):
    original=copy.deepcopy(self.obj['status']['conditions'])
    self.obj['status']['conditions']=[v for v in original if v['type']!=typ]
    with self.assertRaisesRegex(core.Stop,'missing or unknown'):self.renew()
    self.assertEqual(self.sent,[])
    self.obj['status']['conditions']=original
 def test_condition_deleted_on_conflict_blocks_retry(self):
  self.changed_on_conflict(lambda:self.obj['status'].update(conditions=[v for v in self.obj['status']['conditions'] if v['type']!='ReadinessObserved']))

 def history_changed(self, mutation):
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  progress.observe(self.obj);mutation()
  with self.assertRaises(core.Stop):progress.observe(self.obj)
  self.assertEqual(self.sent,[])
 def test_terminal_current_circuit_removal_rejected(self):
  self.incident_progress(True)
  self.history_changed(lambda:self.obj['status'].pop('circuits'))
 def test_terminal_current_circuit_time_mutation_rejected(self):
  self.incident_progress(True)
  self.history_changed(lambda:self.obj['status']['circuits'][0].update(openedAt=(self.now+dt.timedelta(seconds=1)).isoformat()))
 def test_terminal_current_circuit_duplicate_rejected(self):
  self.incident_progress(True)
  self.history_changed(lambda:self.obj['status']['circuits'].append(copy.deepcopy(self.obj['status']['circuits'][0])))
 def failure_history(self):
  self.incident_progress(True)
  at={'actionID':self.scope['incident']+'-a1','nodeName':'node','nodeUID':'node-uid','failureReason':'UNCHANGED','completedAt':self.now.isoformat(),'outcome':'Failed'}
  self.incident['status']['runtime']['attempts']=[at]
  self.obj['status']['recentFailures']=[{'actionID':at['actionID'],'nodeName':'node','nodeUID':'node-uid','reason':'UNCHANGED','at':at['completedAt'],'incident':self.scope['incident']}]
 def test_terminal_current_failure_removal_rejected(self):
  self.failure_history();self.history_changed(lambda:self.obj['status'].pop('recentFailures'))
 def test_terminal_current_failure_mutation_rejected(self):
  self.failure_history();self.history_changed(lambda:self.obj['status']['recentFailures'][0].update(reason='changed'))
 def test_terminal_current_failure_duplicate_rejected(self):
  self.failure_history();self.history_changed(lambda:self.obj['status']['recentFailures'].append(copy.deepcopy(self.obj['status']['recentFailures'][0])))
 def test_current_history_preserved_across_process_restart(self):
  self.incident_progress(True);Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope).observe(self.obj);self.obj['status'].pop('circuits')
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  with self.assertRaisesRegex(core.Stop,'history removed/changed'):progress.observe(self.obj)
 def test_reservation_create_gap_confirmed_by_second_get(self):
  self.incident_progress();original=self.c.get;reads=[]
  def get(path,absent=False):
   if '/recoveryincidents/' in path:
    reads.append(path)
    if len(reads)==1:return None
   return original(path,absent)
  self.c.get=get;self.renew();self.assertEqual(len(self.sent),1);self.assertGreaterEqual(len(reads),2)
 def test_reservation_create_gap_unresolved_no_dispatch(self):
  self.incident_progress();original=self.c.get;reads=[]
  def get(path,absent=False):
   if '/recoveryincidents/' in path:reads.append(path);return None
   return original(path,absent)
  self.c.get=get
  with self.assertRaisesRegex(core.Stop,'Incident unconfirmed'):self.renew()
  self.assertEqual(len(reads),2);self.assertEqual(self.sent,[])

 def test_bound_source_pending_regression_during_action_restart(self):
  self.incident_progress()
  at={'actionID':self.scope['incident']+'-a1','hostname':'target-host','outcome':'Patched'}
  self.incident['status']['runtime']['attempts']=[at]
  self.deployment['spec']['template']['metadata']['annotations']={'opsloop.io/attempt-id':at['actionID']}
  self.deployment['spec']['template']['spec']['nodeSelector']={'kubernetes.io/hostname':'target-host'}
  self.obj['status']['conditions'][2].update(status='Unknown',reason='SourcePending',message='Approved source runtime identity pending; no Incident or recovery action authorized')
  progress=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope)
  with self.assertRaisesRegex(core.Stop,'bound source regressed'):progress.observe(self.obj)


if __name__=='__main__':unittest.main()
