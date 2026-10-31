import io,json,tempfile,time,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_resume import require_live_view
from tools.exhibition_hashes import ControlError
class Response(io.StringIO):
 status=200
class ViewGate(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name);self.urls=[];self.cap={'control':False,'observationConfigured':True};self.active=None
 def get(self,url,timeout):
  self.urls.append(url)
  v=self.cap if url.endswith('capabilities') else {'active':self.active} if url.endswith('state') else {'observed':'fixture-local-only'}
  return Response(json.dumps(v))
 def run_gate(self):
  with patch('urllib.request.urlopen',self.get):return require_live_view(self.root,time.time()+10)
 def test_missing_connection_before_transition(self):
  with self.assertRaises(ControlError):self.run_gate()
  self.assertEqual(self.urls,[])
 def test_verified_get_only(self):
  (self.root/'view-ready.json').write_text('{}');self.assertEqual(self.run_gate()['GETs'],4);self.assertTrue(all(u.startswith('http://127.0.0.1:4174/api/') for u in self.urls))
 def test_wrong_mode_and_active_run_rejected(self):
  (self.root/'view-ready.json').write_text('{}')
  for cap,active in [({'control':True,'observationConfigured':True},None),({'control':False,'observationConfigured':False},None),(self.cap,{'runId':'other'})]:
   self.cap=cap;self.active=active
   with self.assertRaises(ControlError):self.run_gate()
 def test_expiry_blocks_before_get(self):
  with patch('urllib.request.urlopen',self.get),self.assertRaises(ControlError):require_live_view(self.root,time.time()-1)
  self.assertEqual(self.urls,[])
