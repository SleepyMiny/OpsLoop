import datetime as dt,json,os,sqlite3,subprocess,sys,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_session import run_in_process
from tools.exhibition_execution import host_idle
from tools.exhibition_hashes import ControlError
class RoundProcess(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name);self.control=self.root/'control';self.model=self.root/'model'
  for p in (self.control,self.model):p.mkdir()
  db=sqlite3.connect(self.control/'ledger.sqlite3');db.execute('CREATE TABLE runs (value TEXT)');db.close()
  db=sqlite3.connect(self.model/'ledger.sqlite3');db.execute('CREATE TABLE jobs(state TEXT,detail TEXT)');db.close()
  self.stop=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(hours=2)).isoformat()
 def tearDown(self):self.tmp.cleanup()
 def invoke(self):return run_in_process({'path':'sealed.json','sha256':'a'*64},self.control,'run-b','request-b',stop_at=self.stop,output_root=self.root/'round',total_seconds=900)
 def insert_run(self,state):
  db=sqlite3.connect(self.control/'ledger.sqlite3');db.execute('INSERT INTO runs VALUES (?)',(json.dumps({'planId':'run-b','runId':'run-b','state':state}),));db.commit();db.close()
 def test_real_child_exits_before_owner_gate(self):
  original=subprocess.run
  def child(argv,**kwargs):
   self.assertEqual(argv[1:3],['-m','tools.exhibition_session']);self.assertIn('--approve-execution',argv)
   code="import os,json,sqlite3;from pathlib import Path;p=os.getpid();d=sqlite3.connect("+repr(str(self.model/'ledger.sqlite3'))+");d.execute('INSERT INTO jobs VALUES (?,?)',('VERIFIED_PENDING_REVIEW',json.dumps({'owner':{'pid':p,'startTicks':Path('/proc/self/stat').read_text().rsplit(')',1)[1].split()[19]}})));d.commit();d.close()"
   self.insert_run('COMPLETED');return original([sys.executable,'-c',code],**kwargs)
  with patch('subprocess.run',side_effect=child) as called:
   result=self.invoke();self.assertEqual(called.call_count,1)
  self.assertEqual(result['state'],'COMPLETED')
  # Host-only gate with no incomplete control claims; same real exited owner.
  empty=self.root/'empty';empty.mkdir()
  self.assertEqual(host_idle({'controlState':str(empty),'engineering':{'state':str(self.model)}})['state'],'LOCAL_CLAIMS_TERMINAL')
 def test_live_owner_still_rejected(self):
  db=sqlite3.connect(self.model/'ledger.sqlite3');db.execute('INSERT INTO jobs VALUES (?,?)',('VERIFIED_PENDING_REVIEW',json.dumps({'owner':{'pid':os.getpid(),'startTicks':Path('/proc/self/stat').read_text().rsplit(')',1)[1].split()[19]}})));db.commit();db.close()
  with self.assertRaisesRegex(ControlError,'HOST_OWNER_ACTIVE'):host_idle({'controlState':str(self.control),'engineering':{'state':str(self.model)}})
 def test_failed_child_no_retry(self):
  with patch('subprocess.run',return_value=subprocess.CompletedProcess([],1)) as called,self.assertRaisesRegex(ControlError,'ROUND_CHILD_FAILED'):self.invoke()
  self.assertEqual(called.call_count,1)
 def test_unconfirmed_result_rejected(self):
  self.insert_run('RUNNING')
  with patch('subprocess.run',return_value=subprocess.CompletedProcess([],0)),self.assertRaisesRegex(ControlError,'ROUND_CHILD_RESULT_UNCONFIRMED'):self.invoke()
 def test_insufficient_window_no_dispatch(self):
  self.stop=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(seconds=900)).isoformat()
  with patch('subprocess.run') as called,self.assertRaisesRegex(ControlError,'CHILD_ROUND_WINDOW_INSUFFICIENT'):self.invoke()
  called.assert_not_called()
 def test_lost_child_no_retry(self):
  with patch('subprocess.run',side_effect=subprocess.TimeoutExpired('child',1080)) as called,self.assertRaises(subprocess.TimeoutExpired):self.invoke()
  self.assertEqual(called.call_count,1);self.assertFalse(json.loads((self.root/'round/child-result.json').read_bytes())['replayed'])
