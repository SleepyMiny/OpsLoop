"""Focused negative evidence/approval paths. All source/model/cluster inputs local."""
import copy,json,threading,unittest
from unittest.mock import patch
from tests.test_exhibition_handoff import SealCurrentCaptureTests
from tests.test_engineering_evidence import fixtures
from tests.test_exhibition_engineering import EngineeringAdapterTests
from tools.exhibition_engineering import EngineeringAdapter
from tools.engineering_runner.contracts import canonical,digest

class TerminalSealNegatives(SealCurrentCaptureTests):
 def test_terminal_evidence_rejection_never_reaches_runner(self):
  for kind in ('other-run','other-incident','aborted','no-action','no-post-defect','different-input','missing-raw','tampered-raw'):
   with self.subTest(kind=kind):
    case=SealCurrentCaptureTests();case.setUp()
    adapter_fixture=EngineeringAdapterTests();adapter_fixture.setUp()
    incoming=copy.deepcopy(adapter_fixture.public_plan);incoming['runtimeCapture']={'local':'fixture'}
    failures=[]
    auth,prov,raw=fixtures()
    if kind=='other-run':auth['run_id']='different-run'
    if kind=='other-incident':raw['incident.json']['metadata']['uid']='other'
    if kind=='aborted':raw['incident.json']['status']['runtime'].update(phase='ABORTED',reason='POLICY_DISABLED')
    if kind=='no-action':raw['incident.json']['status']['runtime']['attempts']=[]
    if kind=='no-post-defect':raw['observations.json'][1]['http_status']=200
    if kind=='different-input':raw['observations.json'][1]['request_sha256']='0'*64
    from tools import exhibition_handoff as handoff
    original=handoff.write_new
    def writer(path,value):
     result=original(path,value)
     if path.name=='pod-after.json':
      if kind=='missing-raw':path.unlink()
      if kind=='tampered-raw':path.write_bytes(path.read_bytes()+b' ')
     return result
    def capture(*args):
     try:return case.exercise(current=True)
     except Exception as exc:failures.append(str(exc));raise
    try:
     with patch('tools.exhibition_handoff.collect',side_effect=capture),patch('tests.test_engineering_evidence.fixtures',return_value=(auth,prov,raw)),patch.object(handoff,'write_new',side_effect=writer),patch('tools.exhibition_engineering.run_engineering') as model:
      outcome=EngineeringAdapter().run(incoming,lambda *event:None,threading.Event())
      self.assertEqual(outcome,{'state':'UNCONFIRMED_TERMINATION','stopConfirmed':False})
      self.assertEqual(len(failures),1)
      expected={'other-run':'run','other-incident':'Incident','aborted':'exhausted','no-action':'attempt','no-post-defect':'application defect was not reproduced','different-input':'reproduction input mismatch','missing-raw':'input file unavailable','tampered-raw':'input hash mismatch'}
      self.assertIn(expected[kind].lower(),failures[0].lower())
      model.assert_not_called()
      self.assertFalse((case.root/'sealed').exists())
      self.assertTrue((case.root/'handoff-start.json').is_file())
    finally:case.tearDown();adapter_fixture.doCleanups()

class TerminalModelApprovalNegatives(EngineeringAdapterTests):
 def test_expired_or_exhausted_approval_never_enters_runner(self):
  for kind in ('expired','exhausted'):
   with self.subTest(kind=kind):
    plan=copy.deepcopy(self.public_plan)
    budget=json.loads(self.args.execution_plan.read_bytes())
    if kind=='expired':plan['expiresAt']=budget['expires_at']='2000-01-01T00:00:00Z'
    else:plan['maxModelCalls']=budget['max_model_calls']=0
    self.args.execution_plan.write_bytes(canonical(budget));plan['engineering']['execution_plan_sha256']=digest(canonical(budget))
    with patch('tools.exhibition_engineering.run_engineering') as model,self.assertRaises(Exception):
     EngineeringAdapter().run(plan,lambda *e:None,threading.Event())
    model.assert_not_called()

def load_tests(loader,tests,pattern):
 return unittest.TestSuite([TerminalSealNegatives('test_terminal_evidence_rejection_never_reaches_runner'),TerminalModelApprovalNegatives('test_expired_or_exhausted_approval_never_enters_runner')])
