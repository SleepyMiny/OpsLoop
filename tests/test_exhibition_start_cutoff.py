import unittest,datetime as dt
from tools.exhibition_resume import require_start_window
from tools.exhibition_hashes import ControlError
class StartCutoff(unittest.TestCase):
 def test_exact_boundary(self):
  a={'noNewStartsAfter':'2026-10-30T04:45:00+09:00'};t=dt.datetime.fromisoformat(a['noNewStartsAfter']).timestamp()
  require_start_window(a,t-1)
  for now in (t,t+1):
   with self.assertRaises(ControlError):require_start_window(a,now)
 def test_existing_without_cutoff(self):require_start_window({},0)
