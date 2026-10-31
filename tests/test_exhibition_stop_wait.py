import unittest
from tools.exhibition_runtime import RuntimeAdapter,RuntimeRejected
class Watcher:
 def poll(self):return None
class StopWait(unittest.TestCase):
 def scenario(self,states,*,lost=True,advance=False):
  tick=[0.];a=RuntimeAdapter(clock=lambda:0,monotonic=lambda:tick[0],sleep=lambda t:tick.__setitem__(0,tick[0]+t));calls=[];values=iter(states)
  def cli(r,action):
   calls.append(action)
   if action=='stop' and lost:raise RuntimeRejected('PENDING')
   phase=next(values,'STOP_RECONCILING');return {'runID':'r','state':{'binding':'b','phase':phase}}
  a.cli=cli;p={'binding':{'runID':'r'},'runtime':{'guardBinding':'b'}}
  try:return a.stop_and_wait(p,{'authorityExpires':'1970-01-01T00:00:05+00:00'},Watcher()),calls
  finally:self.assertEqual(calls.count('stop'),1);self.assertTrue(set(calls)<= {'stop','status'})
 def test_immediate(self):self.assertEqual(self.scenario(['STOP_CONFIRMED'],lost=False)[0]['state']['phase'],'STOP_CONFIRMED')
 def test_delayed_ack_only_status(self):self.assertEqual(self.scenario(['STOP_RECONCILING','STOP_RECONCILING','STOP_CONFIRMED'])[1],['stop','status','status','status'])
 def test_timeout_no_redispatch(self):
  with self.assertRaisesRegex(RuntimeRejected,'DEADLINE'):self.scenario([])
 def test_manual_state_never_passes(self):
  with self.assertRaises(RuntimeRejected):self.scenario(['MANUAL_REQUIRED'])
 def test_armed_without_stop_evidence_never_passes(self):
  with self.assertRaises(RuntimeRejected):self.scenario(['ARMED'])
 def test_other_run_rejected(self):
  a=RuntimeAdapter();a.cli=lambda *x:{'runID':'foreign','state':{'binding':'b','phase':'STOP_CONFIRMED'}}
  with self.assertRaisesRegex(RuntimeRejected,'IDENTITY'):a.stop_and_wait({'binding':{'runID':'r'},'runtime':{'guardBinding':'b'}},{'authorityExpires':'2099-01-01T00:00:00+00:00'},Watcher())
