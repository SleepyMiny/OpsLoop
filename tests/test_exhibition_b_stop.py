import tempfile,time,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_reconcile import wait_engineering_stop,ControlError
class Stop(unittest.TestCase):
 def run_case(self,phases,receipt,foreign=False,expired=False,raise_finish=False):
  calls=[]
  def call(args,timeout):
   calls.append(args[0])
   if args[0]=='finish-workload':
    if raise_finish:raise RuntimeError('lost response')
    return {}
   return {'runID':'other' if foreign else 'run','state':{'binding':'guard','phase':phases.pop(0) if len(phases)>1 else phases[0]}}
  with tempfile.TemporaryDirectory() as d,patch('tools.exhibition_reconcile.time.sleep'):
   try:wait_engineering_stop(call,[],{'runID':'run'},'guard',receipt,Path(d),time.time()+(-1 if expired else 10))
   except ControlError:
    if not(foreign or expired):raise
   else:self.assertFalse(foreign or expired)
  return calls
 def test_already(self):self.assertEqual(self.run_case(['STOP_CONFIRMED'],{}),['guard-workload'])
 def test_prior_dispatch_waits(self):self.assertNotIn('finish-workload',self.run_case(['STOP_RECONCILING','STOP_CONFIRMED'],{'finishDispatched':True}))
 def test_prior_attempt_waits(self):self.assertNotIn('finish-workload',self.run_case(['STOP_RECONCILING','STOP_CONFIRMED'],{'finishAttempt':{}}))
 def test_lost_response_once(self):self.assertEqual(self.run_case(['ACTIVE','STOP_RECONCILING','STOP_CONFIRMED'],{},raise_finish=True).count('finish-workload'),1)
 def test_foreign(self):self.assertNotIn('finish-workload',self.run_case(['ACTIVE'],{},foreign=True))
 def test_expired_prior(self):self.assertNotIn('finish-workload',self.run_case(['STOP_RECONCILING'],{'finishDispatched':True},expired=True))

class TrialStrongStop(unittest.TestCase):
 def run_trial_stop(self,foreign=False,prior=False):
  from tools import exhibition_demo_b_trial as core
  calls=[];record={'guardBinding':'exact-binding'}
  if prior:record['finishDispatched']=True
  def native(p,key,args,**kw):
   calls.append(args[0])
   if args[0]=='finish-workload':raise RuntimeError('lost finish response')
   return {'runID':'foreign' if foreign else 'run','state':{'binding':'exact-binding','phase':'STOP_CONFIRMED'}}
  with patch.object(core,'native',side_effect=native):
   if foreign:
    with self.assertRaises(core.Stop):core.finish_guard({'runID':'run'},[],None,record,lambda:None)
   else:
    core.finish_guard({'runID':'run'},[],None,record,lambda:None)
    core.finish_guard({'runID':'run'},[],None,record,lambda:None)
  self.assertEqual(calls.count('finish-workload'),0 if prior else 1)
  if not foreign:self.assertEqual(record['stopReceipt']['state']['phase'],'STOP_CONFIRMED')
  return record
 def test_one_dispatch_and_lost_response_reconciled(self):self.run_trial_stop()
 def test_prior_dispatch_not_repeated(self):self.run_trial_stop(prior=True)
 def test_foreign_run_cannot_confirm(self):self.assertNotIn('stopReceipt',self.run_trial_stop(foreign=True))
 def test_unconfirmed_stop_wait_is_not_replenished(self):
  from tools import exhibition_demo_b_trial as core
  record={'guardBinding':'binding'};calls=[];tick=[10.0]
  def native(p,key,args,**kw):
   calls.append(args[0]);return {} if args[0]=='finish-workload' else {'runID':'run','state':{'binding':'binding','phase':'STOP_RECONCILING'}}
  with patch.object(core,'native',side_effect=native),patch.object(core.time,'monotonic',side_effect=lambda:tick[0]):
   with self.assertRaises(core.Stop):core.finish_guard({'runID':'run'},[],None,record,lambda:None)
   first=dict(record['stopWait']);tick[0]=56.0
   with self.assertRaises(core.Stop):core.finish_guard({'runID':'run'},[],None,record,lambda:None)
  self.assertEqual(record['stopWait'],first);self.assertEqual(calls,['finish-workload','guard-workload'])
