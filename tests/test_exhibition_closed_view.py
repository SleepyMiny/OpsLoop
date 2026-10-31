import importlib.util,unittest
from pathlib import Path
class ClosedViewTests(unittest.TestCase):
 def test_closed_cache_is_410_and_never_proxied(self):
  spec=importlib.util.spec_from_file_location('closed_view',Path(__file__).resolve().parents[1]/'scripts/m3-serve.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  h=object.__new__(m.Handler);h.path='/api/a/api/v1/snapshot';h.live=True;h.observation_enabled=False;h.last_snapshots={'a':{'observedAt':100,'source':'live'}};calls=[];h.json_response=lambda *a:calls.append(a)
  h.do_GET();self.assertEqual(calls,[(410,{'error':'OBSERVATION_CLOSED','snapshot':{'observedAt':100,'source':'live'}})])

 def test_cached_observation_is_advertised_without_live_polling(self):
  spec=importlib.util.spec_from_file_location('closed_caps',Path(__file__).resolve().parents[1]/'scripts/m3-serve.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  h=object.__new__(m.Handler);h.path='/api/exhibition/capabilities';h.live=True;h.observation_enabled=False;h.last_snapshots={'a':{'observedAt':100}};h.coordinator=None;h.csrf_token='';calls=[];h.json_response=lambda *a:calls.append(a)
  h.do_GET();self.assertFalse(calls[0][1]['observationConfigured']);self.assertTrue(calls[0][1]['lastObservationAvailable']);self.assertFalse(calls[0][1]['control'])
