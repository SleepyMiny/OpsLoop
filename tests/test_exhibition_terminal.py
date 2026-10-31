"""Synthetic deterministic terminal races through actual heartbeat transport.
No live API, model or operating permit is used.
"""
import copy
import datetime as dt
import json
import unittest
from unittest.mock import patch
from tests.test_exhibition_heartbeat_progress import ProgressTests
from tools import exhibition_demo_b_trial as core
from tools.exhibition_heartbeat_progress import Progress
from tools.exhibition_terminal import TerminalObserved


class TerminalTests(ProgressTests):
 setUp=ProgressTests.setUp
 incident_progress=ProgressTests.incident_progress
 benign=ProgressTests.benign
 send=ProgressTests.send
 conflict=ProgressTests.conflict
 guard=ProgressTests.guard
 revalidate=ProgressTests.revalidate
 renew=ProgressTests.renew
 def prepare(self):
  self.incident_progress()
  Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope).observe(self.obj)
  oldget=self.c.get
  self.replacement=copy.deepcopy(self.pod)
  self.replacement['metadata'].update(name='replacement',uid='replacement-uid',annotations={'opsloop.io/attempt-id':self.scope['incident']+'-a1'},ownerReferences=[{'controller':True,'kind':'ReplicaSet','name':'rs-new','uid':'rs-new-uid'}])
  self.replacement['status']['containerStatuses'][0]['containerID']='containerd://replacement'
  self.replacement_rs=copy.deepcopy(self.rs);self.replacement_rs['metadata'].update(name='rs-new',uid='rs-new-uid')
  template=copy.deepcopy(self.fault);template['metadata']['annotations']={'opsloop.io/attempt-id':self.scope['incident']+'-a1'};template['spec']['nodeSelector']={'kubernetes.io/hostname':'target-host'}
  self.replacement_rs['spec']['template']=template
  def get(path,absent=False):
   if path.endswith('/pods/source-pod'):return copy.deepcopy(self.pod)
   if path.endswith('/pods/replacement'):return copy.deepcopy(self.replacement)
   if path.endswith('/replicasets/rs-new'):return copy.deepcopy(self.replacement_rs)
   return oldget(path,absent)
  self.c.get=get
 def terminal(self):
  self.benign();self.deployment['spec']['template']=copy.deepcopy(self.replacement_rs['spec']['template'])
  at=self.now.isoformat();action=self.scope['incident']+'-a1'
  self.incident['status']['runtime']={'phase':'FAILED','reason':'RUNTIME_BUDGET_EXHAUSTED','completedAt':at,'attempts':[{'actionID':action,'outcome':'Failed','reservedAt':at,'patchedAt':at,'completedAt':at,'nodeName':'node','nodeUID':'node-uid','hostname':'target-host','podName':'replacement','podUID':'replacement-uid','decision':{'actionID':action,'workloadUID':'dep-uid','policyHash':'policy-hash','policyGeneration':12}}]}
  self.obj['status'].pop('activeIncident',None)
  self.obj['status']['conditions'][0].update(status='False',reason='EXECUTION_AUTHORITY_UNAVAILABLE',message='execution authority: single incident already consumed')
  self.obj['status']['conditions'][2].update(status='False',reason='SourceRejected',message='Source authorization unavailable; execution blocked')
 def closed(self,count):
  with self.assertRaises(TerminalObserved) as caught:self.renew()
  self.assertEqual(len(self.sent),count)
  self.assertEqual(caught.exception.proof['incident']['metadata']['uid'],'inc-uid')
  self.assertTrue((self.root/'runtime-terminal.json').is_file())
 def test_initial_terminal_no_patch(self):
  self.prepare();self.terminal();self.closed(0)
 def test_terminal_before_send_no_patch(self):
  self.prepare();original=self.send
  def send(*args):self.terminal();return original(*args)
  with patch('tools.exhibition_heartbeat.send',side_effect=send):self.closed(0)
 def test_terminal_after_applied_response_one_patch(self):
  self.prepare();original=self.send
  def send(*args):value=original(*args);self.terminal();return value
  with patch('tools.exhibition_heartbeat.send',side_effect=send):self.closed(1)
 def test_terminal_on_conflict_no_second_patch(self):
  self.prepare();self.hooks=[self.terminal];self.responses=[self.conflict()];self.closed(1)
 def test_terminal_latch_cannot_restart_heartbeat(self):
  self.prepare();self.terminal();self.closed(0)
  with self.assertRaisesRegex(core.Stop,'already latched'):self.renew()
  self.assertEqual(self.sent,[])
 def test_unknown_response_is_not_terminal_success(self):
  self.prepare();self.hooks=[self.terminal];self.responses=[{'disposition':'APPLY_UNKNOWN','httpStatus':None,'status':None,'object':None}]
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(len(self.sent),1);self.assertFalse((self.root/'runtime-terminal.json').exists())
 def test_terminal_with_authority_or_source_changes_rejected(self):
  for change in ('permit','grant','uid','generation','instance','guard','source-runtime','source-owner','replacement','replacement-runtime','reason','other-run','other-incident','fence'):
   with self.subTest(change=change):
    # A fresh isolated test case, not an operating budget reset.
    case=TerminalTests();case.setUp()
    try:
     case.prepare();case.terminal()
     if change=='permit':case.obj['metadata']['annotations'].pop(core.EXHIBITION_PERMIT)
     elif change=='grant':case.obj['metadata']['annotations'].pop(core.GRANT)
     elif change=='uid':case.obj['metadata']['uid']='other'
     elif change=='generation':case.obj['metadata']['generation']=13
     elif change=='instance':case.instance_changed=True
     elif change=='guard':case.grant_stopped=True
     elif change=='source-runtime':case.pod['status']['containerStatuses'][0]['containerID']='other'
     elif change=='source-owner':case.pod['metadata']['ownerReferences'][0]['uid']='other'
     elif change=='replacement':case.replacement['metadata']['uid']='other'
     elif change=='replacement-runtime':case.replacement['status']['containerStatuses'][0]['imageID']='other'
     elif change=='reason':case.incident['status']['runtime']['reason']='POLICY_DISABLED'
     elif change=='other-run':case.incident['metadata']['annotations']['opsloop.io/exhibition-run']='other'
     elif change=='other-incident':case.incident['metadata']['uid']='other'
     elif change=='fence':case.obj['status']['conditions'][1].update(status='True',reason='FenceClosed')
     with case.assertRaises(core.Stop):case.renew()
     case.assertEqual(case.sent,[])
    finally:case.doCleanups()
 def test_original_absence_only_after_proved_action(self):
  self.prepare();self.terminal();original=self.c.get
  self.c.get=lambda path,absent=False:None if path.endswith('/pods/source-pod') and absent else original(path,absent)
  self.closed(0)
 def test_original_lookup_failure_is_not_absence(self):
  self.prepare();self.terminal();original=self.c.get
  def get(path,absent=False):
   if path.endswith('/pods/source-pod'):raise core.Stop('read unavailable')
   return original(path,absent)
  self.c.get=get
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])
 def test_terminal_source_rejected_without_consumed_reason(self):
  self.prepare();self.terminal();self.obj['status']['conditions'][0]['message']='permit revoked'
  with self.assertRaises(core.Stop):self.renew()
  self.assertEqual(self.sent,[])


 def test_history_change_with_terminal_never_closes(self):
  for phase in ('before-send','after-applied','conflict'):
   for field in ('circuits','recentFailures'):
    for mutation in ('remove','change'):
     with self.subTest(phase=phase,field=field,mutation=mutation):
      case=TerminalTests();case.setUp()
      try:
       case.prepare();identity='signature' if field=='circuits' else 'actionID'
       case.obj['status'][field]=[{identity:'old','incident':'older','reason':'preserve'}]
       original=case.send
       def change():
        case.terminal()
        if mutation=='remove':case.obj['status'][field]=[]
        else:case.obj['status'][field][0]['reason']='changed'
       def send(*args):
        if phase=='before-send':change();return original(*args)
        value=original(*args)
        if phase=='after-applied':change()
        return value
       if phase=='conflict':case.hooks=[change];case.responses=[case.conflict()]
       with patch('tools.exhibition_heartbeat.send',side_effect=send),case.assertRaises(core.Stop):case.renew()
       case.assertEqual(len(case.sent),0 if phase=='before-send' else 1)
       case.assertFalse((case.root/'runtime-terminal.json').exists())
      finally:case.doCleanups()
 def test_own_terminal_history_append_is_proved(self):
  self.prepare();self.obj['status']['circuits']=[{'signature':'old','incident':'older'}]
  original=self.send
  def send(*args):
   value=original(*args);self.terminal()
   self.obj['status']['circuits'].append({'signature':'b'*64,'class':'Application','incident':self.scope['incident'],'phase':'FAILED','openedAt':self.now.isoformat()})
   return value
  with patch('tools.exhibition_heartbeat.send',side_effect=send):self.closed(1)

 def capture_fixture(self):
  self.wire['binding']='c'*64
  self.prepare();self.terminal()
  proof=Progress(self.c,self.p,self.payload,self.wire['binding'],self.root,self.envelope).terminal(self.obj)
  self.p.update(exhibitionPermit={'testOnly':True},initialPolicyGeneration=10,clusterUID='cluster',expiresAt=(self.now+dt.timedelta(minutes=10)).isoformat())
  self.record.update(guardBinding='guard-binding',planSHA256='d'*64)
  raw=self.root/'capture';raw.mkdir()
  before={'deployment':copy.deepcopy(self.deployment),'pod':copy.deepcopy(self.pod),'probe':{'synthetic':True},'log':{'synthetic':True}}
  core.private_new(raw/'deployment-before.json',before['deployment']);core.private_new(raw/'pod-before.json',before['pod'])
  (self.root/'verification').mkdir();core.private_new(self.root/'verification/image-provenance.json',{'repository':'local-fixture','base_commit':'e'*40,'image_digest':self.image.split('@')[1]})
  previous=self.c.get
  self.c.get=lambda path,absent=False:copy.deepcopy(self.rs) if path.endswith('/replicasets/rs') else previous(path,absent)
  self.c.listing=lambda path:{'items':[]}
  self.c.log=lambda *a:'synthetic defect observation'
  def stop():
   self.obj['spec']['enabled']=False;self.obj['metadata']['generation']=13
   self.obj['status']['conditions'][1].update(status='True',reason='FenceClosed',observedGeneration=13)
   self.record['stopReceipt']={'runID':self.p['runID'],'state':{'binding':'guard-binding','phase':'STOP_CONFIRMED'}}
  return proof,raw,before,stop
 def capture(self,proof,raw,before,stop,**kwargs):
  from contextlib import ExitStack
  with ExitStack() as stack:
   def permit(p,setup=None,*,current=True):
    self.assertFalse(current,'capture must not reactivate current Runtime authority')
    return self.envelope,self.payload
   def bind(c,p,setup,*,current_authority=True):self.assertFalse(current_authority)
   stack.enter_context(patch.object(core,'exhibition_permit',side_effect=permit))
   stack.enter_context(patch.object(core,'bind_controller',side_effect=bind))
   stack.enter_context(patch.object(core,'native',side_effect=kwargs.pop('probe',lambda *a,**k:{'synthetic':True})))
   return core.capture_terminal(self.c,self.p,{},self.record,lambda:None,proof,raw,before,stop=stop,started=self.now,read_deadline=kwargs.pop('read_deadline',self.now+dt.timedelta(minutes=5)))
 def test_capture_after_runtime_expiry_within_stop_only_window(self):
  proof,raw,before,stop=self.capture_fixture()
  with patch.object(core,'now',return_value=self.now+dt.timedelta(minutes=3)):
   self.capture(proof,raw,before,stop)
  self.assertTrue((raw/'capture-manifest.json').exists());self.assertEqual(self.sent,[])
  self.assertTrue(json.loads((raw/'policy.json').read_bytes())['spec']['enabled'])
  self.assertFalse(self.obj['spec']['enabled'])
 def test_capture_strong_stop_missing_or_wrong_binding_rejected(self):
  proof,raw,before,stop=self.capture_fixture()
  with self.assertRaisesRegex(core.Stop,'strong stop binding'):
   self.capture(proof,raw,before,lambda:None)
  self.assertFalse((raw/'capture-manifest.json').exists());self.assertEqual(self.sent,[])
 def test_capture_read_window_expiry_rejected(self):
  proof,raw,before,stop=self.capture_fixture()
  with self.assertRaisesRegex(core.Stop,'capture deadline'):
   self.capture(proof,raw,before,stop,read_deadline=self.now-dt.timedelta(seconds=1))
  self.assertFalse((raw/'capture-manifest.json').exists())
 def test_capture_plan_model_window_expiry_rejected(self):
  proof,raw,before,stop=self.capture_fixture();self.p['expiresAt']=(self.now-dt.timedelta(seconds=1)).isoformat()
  with self.assertRaisesRegex(core.Stop,'capture deadline'):self.capture(proof,raw,before,stop)
  self.assertFalse((raw/'capture-manifest.json').exists())
 def test_capture_runtime_changes_during_probe_rejected(self):
  proof,raw,before,stop=self.capture_fixture()
  def probe(*a,**kw):
   self.replacement['status']['containerStatuses'][0]['containerID']='replacement-changed'
   return {'synthetic':True}
  with self.assertRaisesRegex(core.Stop,'custody changed'):self.capture(proof,raw,before,stop,probe=probe)
  self.assertFalse((raw/'capture-manifest.json').exists())
 def test_capture_original_changes_during_probe_rejected(self):
  proof,raw,before,stop=self.capture_fixture()
  def probe(*a,**kw):
   self.pod['status']['containerStatuses'][0]['containerID']='source-changed'
   return {'synthetic':True}
  with self.assertRaisesRegex(core.Stop,'original runtime changed'):self.capture(proof,raw,before,stop,probe=probe)
  self.assertFalse((raw/'capture-manifest.json').exists())
 def test_capture_permit_change_during_probe_rejected(self):
  proof,raw,before,stop=self.capture_fixture()
  def probe(*a,**kw):self.obj['metadata']['annotations'].pop(core.EXHIBITION_PERMIT);return {'synthetic':True}
  with self.assertRaisesRegex(core.Stop,'grant/permit changed'):self.capture(proof,raw,before,stop,probe=probe)
  self.assertFalse((raw/'capture-manifest.json').exists())


def load_tests(loader, tests, pattern):
 return unittest.TestSuite(TerminalTests(name) for name in TerminalTests.__dict__ if name.startswith('test_'))

if __name__=='__main__':unittest.main()
