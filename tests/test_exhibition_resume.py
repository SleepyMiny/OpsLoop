import tempfile,sqlite3,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_resume import can_start,unused_budget
from tools.exhibition_control import ControlError
class ResumeTests(unittest.TestCase):
 def test_next_round_budget_not_all_four_rounds(self):
  self.assertTrue(can_start('A1',0,1800));self.assertFalse(can_start('B1',0,1800))
  self.assertFalse(can_start('A2',0,1680));self.assertTrue(can_start('B2',0,2281))
 def test_consumed_issuance_not_reset(self):
  with tempfile.TemporaryDirectory() as d:
   root=Path(d);(root/'issuer').mkdir();db=sqlite3.connect(root/'issuer/issuance.sqlite3');db.execute('CREATE TABLE permits(x TEXT)');db.execute("INSERT INTO permits VALUES('already-consumed')");db.commit();db.close()
   with patch('tools.exhibition_resume.host_idle'),self.assertRaises(ControlError):unused_budget({'controlState':str(root/'control'),'issuanceState':str(root/'issuer')})
   db=sqlite3.connect(root/'issuer/issuance.sqlite3');self.assertEqual(db.execute('SELECT COUNT(*) FROM permits').fetchone()[0],1);db.close()
