import tempfile,unittest
from pathlib import Path
from tests.test_exhibition_runtime import fixture,FakeAPI,FakeAdapter,NOW
class LateSettle(unittest.TestCase):
 def setup_case(self):
  t=tempfile.TemporaryDirectory();self.addCleanup(t.cleanup);p=fixture(Path(t.name));api=FakeAPI(p);a=FakeAdapter(api);a.monotonic=lambda:100.;a.clock=lambda:NOW+100;return p,api,a
 def test_late_source_absence_still_requires_current_owner_and_service(self):
  p,api,a=self.setup_case();api.recovered=True;self.assertTrue(a.settle_fault(p,api,0));self.assertEqual(api.mutations,[]);self.assertEqual(api.faults,0)
 def test_late_present_unready_does_not_extend_propagation_wait(self):
  p,api,a=self.setup_case();base=api.request
  def read(*args,**kw):
   v=base(*args,**kw)
   if args[0]==api.paths['pod']:v['status']['conditions'][0]['status']='False'
   return v
  api.request=read;a.sleep=lambda _:self.fail('Late unready must not wait again');self.assertFalse(a.settle_fault(p,api,0))
 def test_original_authority_expiry_still_blocks_all_reads(self):
  p,api,a=self.setup_case();a.clock=lambda:NOW+600;api.request=lambda *x,**y:self.fail('No reads after original horizon');self.assertFalse(a.settle_fault(p,api,0))
 def test_late_absent_source_is_not_success_with_bad_service(self):
  p,api,a=self.setup_case();api.recovered=True;api.probe=lambda:{'httpStatus':503}
  from tools.exhibition_runtime import RuntimeRejected
  with self.assertRaises(RuntimeRejected):a.settle_fault(p,api,0)
