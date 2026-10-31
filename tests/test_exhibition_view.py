import json,sqlite3,tempfile,unittest
from pathlib import Path
from tools.exhibition_view import LedgerView
class ViewTests(unittest.TestCase):
 def test_no_ledger_created(self):
  with tempfile.TemporaryDirectory() as d:
   view=LedgerView(Path(d));self.assertEqual(view.state()['runs'],[]);self.assertFalse(view.path.exists())
 def test_read_only_no_plans_or_private_detail(self):
  with tempfile.TemporaryDirectory() as d:
   view=LedgerView(Path(d));db=sqlite3.connect(view.path);view.path.chmod(0o600)
   db.execute('CREATE TABLE runs(value TEXT)');db.execute('CREATE TABLE events(run TEXT,seq INTEGER,value TEXT)')
   db.execute('INSERT INTO runs VALUES(?)',(json.dumps({'runId':'r','state':'RUNNING','lifecycleComplete':False,'privatePlan':'secret'}),))
   db.execute('INSERT INTO events VALUES(?,?,?)',('r',1,json.dumps({'runId':'r','seq':1,'at':'now','actor':'observer','kind':'TEST','source':'measured','data':{'ready':True,'token':'private'}})))
   db.commit();before=view.path.read_bytes();state=view.state();events=view.events('r')
   self.assertEqual(state['plans'],[]);self.assertFalse(state['nextRunAllowed']);self.assertNotIn('privatePlan',str(state));self.assertNotIn('token',str(events));self.assertEqual(view.path.read_bytes(),before);db.close()
