import tempfile,unittest,stat,json
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_runtime import process_json,RuntimeRejected
from tools.exhibition_session import run_once
from tools.exhibition_resume import ready,stopped
class Connections(unittest.TestCase):
 def test_failed_guard_output_retained_private(self):
  with tempfile.TemporaryDirectory() as d:
   sink=Path(d)/'diagnostics'
   with self.assertRaises(RuntimeRejected):process_json(['/usr/bin/python3','-c','import sys; print("receipt"); print("specific-failure",file=sys.stderr); sys.exit(7)'],2,4096,diagnostics=sink)
   entry=next(sink.iterdir());r=json.loads((entry/'receipt.json').read_text());self.assertEqual(r['exitCode'],7)
   self.assertEqual((entry/'stderr').read_text(),'specific-failure\n');self.assertEqual((entry/'stdout').read_text(),'receipt\n')
   self.assertEqual(stat.S_IMODE((entry/'stderr').stat().st_mode),0o600);self.assertNotIn('argv',r)
 def test_restore_symbols_available(self):
  self.assertTrue(callable(ready));self.assertTrue(callable(stopped))
