import datetime as dt,json,sqlite3,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_resume import session_deadline,replacement_budget
from tools.exhibition_control import ControlError
class ContinuationTests(unittest.TestCase):
 def test_continuation_never_renews_ninety_minutes(self):
  prior={'startedAt':'2026-10-29T10:29:51+00:00','deadline':'2026-10-29T11:59:51+00:00'}
  with patch('tools.exhibition_resume.private_read',return_value=prior):
   result=session_deadline(dt.datetime.fromisoformat('2026-10-29T11:10:00+00:00'),{'workDeadline':'2026-10-29T12:00:00+00:00'},{'priorSession':{'path':'prior','sha256':'pin'}})
  self.assertEqual(result.isoformat(),prior['deadline'])
 def scenario(self,changed=None):
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);(root/'control').mkdir();(root/'issuer').mkdir()
   db=sqlite3.connect(root/'control/ledger.sqlite3');db.execute('CREATE TABLE runs(value TEXT)');db.execute('CREATE TABLE events(value TEXT)')
   run={'runId':'old','state':'STOPPED','lifecycleComplete':True}
   if changed=='unfinished':run['lifecycleComplete']=False
   db.execute('INSERT INTO runs VALUES(?)',(json.dumps(run),))
   if changed=='fault':db.execute('INSERT INTO events VALUES(?)',(json.dumps({'kind':'FAULT_DISPATCHED'}),))
   db.commit();db.close();db=sqlite3.connect(root/'issuer/issuance.sqlite3');db.execute('CREATE TABLE permits(run TEXT,epoch TEXT,state TEXT)');db.execute("INSERT INTO permits VALUES('old','epoch','ISSUED')");db.commit();db.close()
   i={'controlState':str(root/'control'),'issuanceState':str(root/'issuer')};c={'replacesRunID':'old','completion':{'path':'receipt','sha256':'pin'},'priorOutcome':{'path':str(root/'outcome'),'sha256':'pin'}};binding={'runID':'old','epoch':'epoch'}
   with patch('tools.exhibition_resume.verify_completion',return_value={'binding':binding}),patch('tools.exhibition_resume.private_read',return_value={'binding':binding,'result':{'faultMaximumSeconds':0}}),patch('tools.exhibition_resume.host_idle'):
    replacement_budget(i,c)
   db=sqlite3.connect(root/'issuer/issuance.sqlite3');self.assertEqual(db.execute('SELECT COUNT(*) FROM permits').fetchone()[0],1);db.close()
 def test_explicit_replacement_preserves_consumed_issuance(self):self.scenario()
 def test_fault_consumed_rejected(self):
  with self.assertRaises(ControlError):self.scenario('fault')
 def test_unfinished_rejected(self):
  with self.assertRaises(ControlError):self.scenario('unfinished')
