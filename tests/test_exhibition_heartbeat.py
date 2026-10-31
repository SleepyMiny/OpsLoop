"""Deterministic local heartbeat conflict/authority interleavings; no real credentials."""
import copy,datetime as dt,json,subprocess,tempfile,time,unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from tools import exhibition_demo_b_trial as core
from tools.exhibition_heartbeat import resource_version_conflict

class HeartbeatTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
  self.addCleanup(patch.stopall);patch.object(core,'ROOT',self.root).start()
  self.now=dt.datetime.now(dt.timezone.utc);self.tick=100000000000;self.scope={'policyUID':'test-policy','runID':'test-run','epoch':'test-epoch'}
  self.grant={'version':'test','scope':self.scope,'instance':'test-instance','notBefore':(self.now-dt.timedelta(seconds=5)).isoformat(),'notAfter':(self.now+dt.timedelta(minutes=2)).isoformat(),'heartbeatAt':(self.now-dt.timedelta(seconds=2)).isoformat(),'heartbeatSequence':4}
  self.envelope={'testOnly':'not-a-signed-permit'};self.fixed={k:copy.deepcopy(self.grant[k]) for k in ('version','scope','instance','notBefore','notAfter')}
  self.wire={'annotation':json.dumps(self.grant),'binding':'fixed-test-binding'}
  self.obj={'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryPolicy','metadata':{'name':'engineering-demo','namespace':'opsloop-demo-b','uid':'test-policy','resourceVersion':'7','generation':12,'annotations':{core.GRANT:self.wire['annotation'],core.EXHIBITION_PERMIT:core.canonical(self.envelope).decode()}},'spec':{'enabled':True,'mode':'Enforce'},'status':{'incidentSequence':2,'conditions':[{'type':'Monitoring','status':'True','reason':'Active','lastTransitionTime':'old'}]}}
  self.p={'runID':'test-run','epoch':'test-epoch','apiOrigin':'https://127.0.0.1:1','executionScope':self.scope,'enforcePolicySpec':copy.deepcopy(self.obj['spec']),'initialIncidentSequence':2,'expiresAt':self.grant['notAfter']}
  self.sent=[];self.grant_stopped=False;self.instance_changed=False;self.hooks=[];self.responses=[];self.record={'requests':[]};self.guard_calls=0
  cfg=self.root/'config';cfg.write_text(json.dumps({'current-context':'local-envtest'}))
  self.c=core.Client(SimpleNamespace(CLIENT='synthetic-kubectl',CONFIG=str(cfg)),'local-envtest',self.record,lambda:None)
  patch.object(core,'clock',return_value={'apiServerDate':self.now.isoformat(),'beforeTick':self.tick,'afterTick':self.tick}).start()
  patch.object(core.time,'clock_gettime_ns',side_effect=lambda *a:self.tick).start()
  patch.object(core,'native',side_effect=lambda p,key,payload:{'annotation':json.dumps(payload),'binding':self.wire['binding']}).start()
  patch.object(core,'exhibition_permit',return_value=(self.envelope,{'grant':self.fixed})).start()
  patch.object(core.subprocess,'run',side_effect=lambda argv,**kw:subprocess.CompletedProcess(argv,0,json.dumps(self.obj).encode(),b'')).start()
  patch('tools.exhibition_heartbeat.send',side_effect=self.send).start()
 def guard(self,receipt):
  self.guard_calls+=1
  if self.grant_stopped:raise core.Stop('RETIRED / STOP_CONFIRMED')
 def revalidate(self):
  if self.instance_changed:raise core.Stop('Controller instance changed')
 def conflict(self):
  return {'disposition':'SERVER_REJECTION','httpStatus':409,'object':None,'status':{'kind':'Status','status':'Failure','code':409,'reason':'Conflict','details':{'group':'opsloop.io','kind':'recoverypolicies','name':'engineering-demo'},'message':'Operation cannot be fulfilled on recoverypolicies.opsloop.io "engineering-demo": the object has been modified; please apply your changes to the latest version and try again'}}
 def benign(self):
  self.obj['metadata']['resourceVersion']=str(int(self.obj['metadata']['resourceVersion'])+1);self.obj['status']['conditions'][0]['lastTransitionTime']='new'+self.obj['metadata']['resourceVersion']
 def send(self,settings,path,payload,directory,before_send):
  before_send();self.sent.append(copy.deepcopy(payload));(directory/'dispatch.json').write_text('{}')
  if self.hooks:self.hooks.pop(0)()
  if self.responses:return self.responses.pop(0)
  candidate=copy.deepcopy(self.obj)
  for op in payload:
   parts=[x.replace('~1','/') for x in op['path'].split('/')[1:]];target=candidate
   for key in parts[:-1]:target=target[key]
   key=parts[-1]
   if op['op']=='test' and target.get(key)!=op['value']:return {'disposition':'SERVER_REJECTION','httpStatus':422,'object':None,'status':{'kind':'Status','status':'Failure','code':422,'reason':'Invalid'}}
   if op['op']=='replace':target[key]=op['value']
  if candidate['metadata']['resourceVersion']!=self.obj['metadata']['resourceVersion']:return self.conflict()
  candidate['metadata']['resourceVersion']=str(int(candidate['metadata']['resourceVersion'])+1);self.obj=candidate
  return {'disposition':'APPLIED_RESPONSE','httpStatus':200,'status':None,'object':copy.deepcopy(candidate)}
 def renew(self):return core.renew_heartbeat(self.c,self.p,12,self.envelope,self.wire,self.grant,self.guard,revalidate=self.revalidate)
 def test_normal_one_attempt_and_readback(self):
  w,g,r=self.renew();self.assertEqual(g['heartbeatSequence'],5);self.assertEqual(len(self.sent),1)
  step=next((self.root/'heartbeat-diagnostics').glob('*/attempt-1'));self.assertEqual((step/'patch.json').stat().st_mode&0o777,0o600)
  self.assertEqual(self.sent[0][1],{'op':'replace','path':'/metadata/resourceVersion','value':'7'})
 def test_benign_status_conflict_revalidates_and_rebuilds_once(self):
  self.hooks=[self.benign];w,g,r=self.renew();self.assertEqual(len(self.sent),2)
  self.assertEqual(self.sent[0][1]['value'],'7');self.assertEqual(self.sent[1][1]['value'],'8')
  self.assertEqual(self.sent[0][-1],self.sent[1][-1]);self.assertEqual(g['heartbeatAt'],self.now.isoformat());self.assertEqual(g['notAfter'],self.grant['notAfter'])
 def test_second_conflict_stops_without_third(self):
  self.hooks=[self.benign,self.benign]
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(len(self.sent),2)
 def test_restart_does_not_reset_budget_or_clock(self):
  self.hooks=[self.benign,self.benign]
  with self.assertRaises(core.Stop):self.renew()
  with self.assertRaises(FileExistsError):self.renew()
  self.assertEqual(len(self.sent),2)
 def changed_on_conflict(self,mutation):
  def changed():self.benign();mutation()
  self.hooks=[changed];self.responses=[self.conflict()]
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(len(self.sent),1)
 def test_uid_change(self):self.changed_on_conflict(lambda:self.obj['metadata'].update(uid='other'))
 def test_generation_change(self):self.changed_on_conflict(lambda:self.obj['metadata'].update(generation=13))
 def test_spec_change(self):self.changed_on_conflict(lambda:self.obj['spec'].update(extra='changed'))
 def test_grant_withdrawal(self):self.changed_on_conflict(lambda:self.obj['metadata']['annotations'].pop(core.GRANT))
 def test_permit_change(self):self.changed_on_conflict(lambda:self.obj['metadata']['annotations'].update({core.EXHIBITION_PERMIT:'changed'}))
 def test_run_change(self):self.changed_on_conflict(lambda:self.p.update(runID='other'))
 def test_epoch_change(self):self.changed_on_conflict(lambda:self.p.update(epoch='other'))
 def test_instance_change(self):self.changed_on_conflict(lambda:setattr(self,'instance_changed',True))
 def test_policy_stop(self):self.changed_on_conflict(lambda:self.obj['spec'].update(enabled=False))
 def test_guard_retirement(self):self.changed_on_conflict(lambda:setattr(self,'grant_stopped',True))
 def test_fence_status_change(self):self.changed_on_conflict(lambda:self.obj['status']['conditions'].append({'type':'Quiesced','status':'True'}))
 def test_unknown_safety_status_change(self):self.changed_on_conflict(lambda:self.obj['status'].update(activeIncident={'name':'other'}))
 def test_deadline_expires_in_conflict(self):
  self.p['expiresAt']=(self.now+dt.timedelta(seconds=3)).isoformat();self.changed_on_conflict(lambda:setattr(self,'tick',self.tick+4000000000))
 def test_no_dispatch_without_response_budget(self):
  self.p['expiresAt']=(self.now+dt.timedelta(seconds=1)).isoformat()
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])
 def test_changed_source_sequence_is_not_new_budget(self):
  self.grant['heartbeatSequence']=9
  with self.assertRaisesRegex(core.Stop,'sequence/core'):self.renew()
  self.assertEqual(self.sent,[])
 def test_instance_revoked_after_transport_setup_blocks_second_send(self):
  original=self.send
  def transport(settings,path,payload,directory,before_send):
   if len(self.sent)==1:self.instance_changed=True
   return original(settings,path,payload,directory,before_send)
  self.hooks=[self.benign]
  with patch('tools.exhibition_heartbeat.send',side_effect=transport),self.assertRaisesRegex(core.Stop,'instance'):self.renew()
  self.assertEqual(len(self.sent),1)
 def test_original_freshness_not_reset(self):self.changed_on_conflict(lambda:setattr(self,'tick',self.tick+5000000000))
 def test_initial_guard_stop_is_not_sent(self):
  self.grant_stopped=True
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])
 def test_lost_response_even_if_applied_is_not_retried_or_success(self):
  self.responses=[{'disposition':'APPLY_UNKNOWN','httpStatus':None,'status':None,'object':None}]
  def applied():self.obj['metadata']['annotations'][core.GRANT]=self.sent[0][-1]['value']
  self.hooks=[applied]
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(len(self.sent),1);self.assertTrue(list((self.root/'heartbeat-diagnostics').glob('*/attempt-1/uncertain-readback.json')))
 def test_non_conflict_rejections_never_retry(self):
  for code in (401,403,422,500):
   with self.subTest(code=code),tempfile.TemporaryDirectory() as root,patch.object(core,'ROOT',Path(root)):
    self.sent=[];self.responses=[{'disposition':'SERVER_REJECTION','httpStatus':code,'object':None,'status':{'kind':'Status','status':'Failure','code':code,'reason':'Invalid'}}]
    with self.assertRaises(core.Stop):self.renew()
    self.assertEqual(len(self.sent),1)
 def test_classifier_requires_all_response_fields_and_target(self):
  self.assertTrue(resource_version_conflict(self.conflict(),'engineering-demo'))
  for invalid in (None,{}, {'status':[]},{'status':{'details':[]}}):self.assertFalse(resource_version_conflict(invalid,'engineering-demo'))
  for mutation in (lambda v:v.update(httpStatus=None),lambda v:v['status'].update(reason='Forbidden'),lambda v:v['status']['details'].update(name='other'),lambda v:v['status'].update(message='conflict'),lambda v:v['status']['details'].update(causes=[{'reason':'other'}])):
   v=self.conflict();mutation(v);self.assertFalse(resource_version_conflict(v,'engineering-demo'))

if __name__=='__main__':unittest.main()
