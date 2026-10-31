import datetime as dt,hashlib,importlib.util,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_hashes import encoded
from tools.exhibition_preparation import authority
class BConnections(unittest.TestCase):
 def test_actual_preparation_authority_accepts_new_b_window_only(self):
  ref={'path':'p','sha256':'h'};p={'status':'SEALED_EXECUTION_CANDIDATE','authority':{'order':['A1','A2','B1','B2'],'maxNewPermits':4},'newInterval':{'authorization':ref}}
  s={'approvedPlanSHA256':'plan','planContentSHA256':hashlib.sha256(encoded(p)).hexdigest(),'livePreflightState':'LIVE_CHECKS_PASS','livePreflightScope':'CURRENT_GET_ONLY','delegatedWork':ref,'approvedAt':'2026-10-29T14:17:46+00:00','startedAt':'2026-10-29T15:00:00+00:00','preparationDeadline':'2026-10-29T15:20:00+00:00','deadline':'2026-10-29T20:17:46+00:00'}
  with patch('tools.exhibition_b_work.work_auth',return_value={'receivedAt':s['approvedAt']}):authority(p,'plan',s,now=dt.datetime.fromisoformat('2026-10-29T15:05:00+00:00'))
  s['delegatedWork']={'path':'other','sha256':'h'}
  from tools.exhibition_control import ControlError
  with self.assertRaises(ControlError):authority(p,'plan',s,now=dt.datetime.fromisoformat('2026-10-29T15:05:00+00:00'))
 def test_ledger_only_artifacts_still_bound_to_known_run(self):
  spec=importlib.util.spec_from_file_location('b_readonly_server',Path(__file__).resolve().parents[1]/'scripts/m3-serve.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  class View:
   def state(self):return {'runs':[{'runId':'run-b1'}]}
  h=object.__new__(m.Handler);h.coordinator=None;h.ledger_view=View();h.public_artifacts=('catalog','hash');h.path='/api/exhibition/artifacts?runId=run-b1';out=[];h.json_response=lambda *v:out.append(v)
  with patch('tools.exhibition_disclosure.load_public_artifacts',return_value={'artifacts':['reviewed']}) as loader:
   h.do_GET();loader.assert_called_once_with('catalog','hash','run-b1')
  self.assertEqual(out[-1],(200,{'artifacts':['reviewed']}));h.path='/api/exhibition/artifacts?runId=foreign'
  with patch('tools.exhibition_disclosure.load_public_artifacts') as loader:h.do_GET();loader.assert_not_called()
  self.assertEqual(out[-1],(200,{'artifacts':[]}))
