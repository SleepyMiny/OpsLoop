"""B1 success alongside failed B2: process result, read-only HTTP and event custody."""
import copy,importlib.util,json,sqlite3,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_session import outcome_summary
from tools.exhibition_view import LedgerView

class ResultBinding(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name);self.view=LedgerView(self.root)
  db=sqlite3.connect(self.view.path);self.view.path.chmod(0o600)
  db.execute('CREATE TABLE runs(value TEXT)');db.execute('CREATE TABLE events(run TEXT,seq INTEGER,value TEXT)')
  self.b1={'runId':'b1','planId':'b1','state':'COMPLETED','executionState':'COMPLETED','stopConfirmed':True,'lifecycleComplete':True}
  self.b2={'runId':'b2','planId':'b2','state':'STOPPED','executionState':'UNCONFIRMED_TERMINATION','stopConfirmed':True,'lifecycleComplete':True}
  for row in (self.b1,self.b2):db.execute('INSERT INTO runs VALUES(?)',(json.dumps(row),))
  for run,kinds in [('b1',['VERIFIED_PENDING_REVIEW','COMPLETED']),('b2',['UNCONFIRMED_TERMINATION','LIFECYCLE_COMPLETE','NEXT_RUN_ALLOWED'])]:
   for seq,kind in enumerate(kinds,1):
    e={'runId':run,'seq':seq,'at':'local-fixture','actor':'coordinator','kind':kind,'source':'measured','data':{}}
    db.execute('INSERT INTO events VALUES(?,?,?)',(run,seq,json.dumps(e)))
  db.commit();db.close();self.before=self.view.path.read_bytes()
 def test_process_success_is_not_trial_or_cleanup_success(self):
  self.assertTrue(outcome_summary(self.b1)['trialSucceeded']);self.assertFalse(outcome_summary(self.b2)['trialSucceeded'])
  incomplete={**self.b1,'lifecycleComplete':False};self.assertFalse(outcome_summary(incomplete)['trialSucceeded'])
  self.assertFalse(outcome_summary(self.b2)['nextRunIsAuthorized'])
 def test_exact_run_http_events_do_not_mix_success(self):
  spec=importlib.util.spec_from_file_location('readonly_result_server',Path('scripts/m3-serve.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  h=object.__new__(m.Handler);h.coordinator=None;h.ledger_view=self.view;out=[];h.json_response=lambda *v:out.append(v)
  for run in ('b1','b2'):
   h.path='/api/exhibition/events?runId='+run+'&after=0';h.do_GET();code,body=out[-1];self.assertEqual(code,200)
   self.assertTrue(all(e['runId']==run for e in body['events']))
   if run=='b2':self.assertFalse(any(e['kind'] in ('VERIFIED_PENDING_REVIEW','COMPLETED') for e in body['events']))
  self.assertFalse(self.view.state()['nextRunAllowed']);self.assertEqual(self.view.path.read_bytes(),self.before)
