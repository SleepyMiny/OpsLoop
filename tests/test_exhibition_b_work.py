import datetime as dt,json,tempfile,unittest,os
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_b_work import work_auth
from tools.exhibition_resume import rounds_for,session_deadline
from tools.exhibition_control import ControlError
from tools.exhibition_demo_b_trial import initial_fault_pod
class BWorkTests(unittest.TestCase):
 def setUp(self):
  self.a={'schema':'opsloop-b-work-approval/v1','receivedAt':'2026-10-29T14:17:46+00:00','workDeadline':'2026-10-29T20:17:46+00:00','allowedRounds':['B1','B2'],'candidateTransitionsMaximum':2,'modelCallsPerRound':1,'modelCallsTotalMaximum':2,'runtimeEngineeringSecondsPerRound':900,'AAdditionalInjections':0,'finalReserveSeconds':1200}
  self.now=dt.datetime.fromisoformat('2026-10-29T15:00:00+00:00').timestamp()
 def test_six_hours_only_b(self):
  with patch('tools.exhibition_b_work.private_read',return_value=self.a):self.assertEqual(work_auth({'path':'p','sha256':'h'},now=self.now),self.a)
  self.assertEqual(rounds_for({'allowedRounds':['B1','B2']},self.a),[(3,'B1'),(4,'B2')])
  self.assertEqual(session_deadline(dt.datetime.fromtimestamp(self.now,dt.timezone.utc),self.a).isoformat(),self.a['workDeadline'])
 def test_no_seven_hours_or_extra_model_or_a(self):
  for key,value in [('workDeadline','2026-10-29T21:17:46+00:00'),('modelCallsPerRound',2),('AAdditionalInjections',1),('runtimeEngineeringSecondsPerRound',901)]:
   with self.subTest(key=key),patch('tools.exhibition_b_work.private_read',return_value={**self.a,key:value}),self.assertRaises(ControlError):work_auth({'path':'p','sha256':'h'},now=self.now)
 def test_expired_blocked(self):
  with patch('tools.exhibition_b_work.private_read',return_value=self.a),self.assertRaises(ControlError):work_auth({'path':'p','sha256':'h'},now=self.now+21600)
 def test_fixed_baseline_marker_only(self):
  p={'faultTemplate':{'metadata':{'annotations':{'opsloop.io/attempt-id':'historical-1'}}}}
  self.assertTrue(initial_fault_pod({'metadata':{'annotations':{'opsloop.io/attempt-id':'historical-1'}}},p))
  for v in ('new-runtime-2',None):self.assertFalse(initial_fault_pod({'metadata':{'annotations':{'opsloop.io/attempt-id':v}}},p))
 def test_legacy_no_marker_still_allowed(self):self.assertTrue(initial_fault_pod({'metadata':{}},{'faultTemplate':{'metadata':{}}}))
