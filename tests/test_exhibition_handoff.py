"""Synthetic handoff contracts only. No Kubernetes/model/helper process is launched."""
import copy
import datetime as dt
import hashlib
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
from tools.exhibition_control import ControlError
from tools.exhibition_handoff import preflight, collect
from tests.test_exhibition_engineering import EngineeringAdapterTests
from tools.exhibition_engineering import EngineeringAdapter

class HandoffTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name);self.root.chmod(0o700)
        self.exp=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=20)).isoformat()
        self.binding={'runID':'fresh-run','epoch':'fresh-epoch','policyUID':'policy','deploymentUID':'deployment'}
        def pin(name,value):
            path=self.root/name;data=json.dumps(value).encode();path.write_bytes(data)
            return str(path),hashlib.sha256(data).hexdigest()
        self.runtime={'runID':'fresh-run','epoch':'fresh-epoch','executionScope':{**self.binding,'policy':'fresh-policy'},'expiresAt':self.exp}
        rp,rh=pin('runtime-plan.json',self.runtime)
        sp,sh=pin('setup.json',{'state':'SCOPE_READY_POLICY_DISABLED','planSHA256':rh})
        (self.root/'verification').mkdir()
        pp,ph=pin('verification/image-provenance.json',{'repository':'repo','base_commit':'a'*40})
        self.plan={'schema':'opsloop-exhibition-plan/v1','planId':'new-plan','authorizationId':'new-auth','approvalReference':'synthetic-test-only',
                   'track':'engineering','source':'measured','binding':self.binding,'totalSeconds':900,'expiresAt':self.exp,'model':'gpt-6-luna','maxModelCalls':1,
                   'engineering':{'repository':'repo','base_commit':'a'*40},
                   'runtimeCapture':{'root':str(self.root),'plan':rp,'planSHA256':rh,'setupReceipt':sp,'setupReceiptSHA256':sh,'provenance':pp,'provenanceSHA256':ph}}
    def tearDown(self):self.tmp.cleanup()
    def test_preflight_is_read_only_and_requires_fresh_run(self):
        before=list(self.root.rglob('*'));preflight(self.plan);self.assertEqual(before,list(self.root.rglob('*')))
        for key in self.binding:
            p=copy.deepcopy(self.plan);p['binding'][key]='different'
            with self.subTest(key=key),self.assertRaises(ControlError):preflight(p)
    def test_reused_output_root_rejected_not_deleted(self):
        for name in ['handoff-start.json','runtime-state','incident-raw','sealed','execution-plan.json']:
            path=self.root/name;path.write_text('preserved')
            with self.subTest(name=name),self.assertRaisesRegex(ControlError,'CONSUMED'):preflight(self.plan)
            self.assertEqual(path.read_text(),'preserved');path.unlink()
    def test_no_model_budget_escalation(self):
        for key,value in [('model','gpt-6-astra'),('maxModelCalls',2)]:
            p=copy.deepcopy(self.plan);p[key]=value
            with self.assertRaises(ControlError):preflight(p)
    def test_cancel_before_capture_has_no_effect(self):
        cancel=threading.Event();cancel.set()
        with self.assertRaisesRegex(ControlError,'CANCELLED'):collect(self.plan,lambda *a:None,cancel)
        self.assertFalse((self.root/'handoff-start.json').exists())
    def test_failed_capture_keeps_exclusive_intent(self):
        # Actual core load rejects this deliberately incomplete runtime authority.
        with self.assertRaises(Exception):collect(self.plan,lambda *a:None,threading.Event())
        self.assertTrue((self.root/'handoff-start.json').is_file())
        with self.assertRaisesRegex(ControlError,'CONSUMED'):preflight(self.plan)

class AdapterCurrentCaptureTests(EngineeringAdapterTests):
    def test_current_capture_is_bound_before_runner_without_manual_bundle_upload(self):
        incoming=copy.deepcopy(self.public_plan);incoming['runtimeCapture']={'synthetic':'test'}
        result=self.verified_result()
        events=[]
        with patch('tools.exhibition_handoff.collect',return_value=self.public_plan) as collect_mock, patch('tools.exhibition_engineering.run_engineering',return_value=result) as runner:
            outcome=EngineeringAdapter().run(incoming,lambda *a:events.append(a),threading.Event())
        collect_mock.assert_called_once();runner.assert_called_once()
        self.assertEqual(outcome['state'],'COMPLETED')
        self.assertEqual(events[0][2]['phase'],'CURRENT_RUN_RUNTIME_EVIDENCE')
    def test_capture_failure_never_calls_model_and_blocks_next_run(self):
        incoming=copy.deepcopy(self.public_plan);incoming['runtimeCapture']={}
        with patch('tools.exhibition_handoff.collect',side_effect=ControlError('CAPTURE_STOP_UNCONFIRMED')),patch('tools.exhibition_engineering.run_engineering') as runner:
            result=EngineeringAdapter().run(incoming,lambda *a:None,threading.Event())
        self.assertEqual(result,{'state':'UNCONFIRMED_TERMINATION','stopConfirmed':False});runner.assert_not_called()

class SealCurrentCaptureTests(HandoffTests):
    def test_current_capture_seals_and_derives_exact_budget_automatically(self):
        self.exercise(current=True)

    def test_new_manifest_cannot_relabel_prior_runtime_observations(self):
        with self.assertRaisesRegex(ControlError,'PREVIOUS_EXECUTION'):
            self.exercise(current=False)
        self.assertFalse((self.root/'sealed').exists())
        self.assertTrue((self.root/'handoff-start.json').exists())

    def exercise(self, current):
        from tests.test_engineering_evidence import fixtures
        from tools.engineering_evidence.seal import canonical,digest,FILES
        from tools.exhibition_handoff import write_new
        from types import SimpleNamespace
        from tests.test_engineering_evidence import NOW
        auth,prov,raw=fixtures()
        delta=dt.datetime.now(dt.timezone.utc)-NOW
        def fresh(value):
            if isinstance(value,dict):return {k:fresh(v) for k,v in value.items()}
            if isinstance(value,list):return [fresh(v) for v in value]
            if isinstance(value,str):
                try:return (dt.datetime.fromisoformat(value.replace('Z','+00:00'))+delta).isoformat()
                except ValueError:pass
            return value
        annotation=raw['policy.json']['metadata']['annotations']['opsloop.io/execution-grant']
        auth,raw=fresh(auth),fresh(raw)
        raw['policy.json']['metadata']['annotations']['opsloop.io/execution-grant']=json.dumps(fresh(json.loads(annotation)))
        self.plan['binding']={'runID':auth['run_id'],'epoch':auth['epoch'],'policyUID':auth['policy_uid'],'deploymentUID':auth['deployment_uid']}
        self.plan['engineering']={'repository':prov['repository'],'base_commit':prov['base_commit'],'repository_path':str(self.root)}
        self.runtime.update(runID=auth['run_id'],epoch=auth['epoch'],executionScope={**self.plan['binding'],'policy':'fresh-policy'})
        rp=Path(self.plan['runtimeCapture']['plan']);rp.write_bytes(canonical(self.runtime));rh=digest(rp.read_bytes());self.plan['runtimeCapture']['planSHA256']=rh
        sp=Path(self.plan['runtimeCapture']['setupReceipt']);sp.write_bytes(canonical({'state':'SCOPE_READY_POLICY_DISABLED','planSHA256':rh}));self.plan['runtimeCapture']['setupReceiptSHA256']=digest(sp.read_bytes())
        pp=Path(self.plan['runtimeCapture']['provenance']);pp.write_bytes(canonical(prov));self.plan['runtimeCapture']['provenanceSHA256']=digest(pp.read_bytes())
        class Client:pass
        def trial(client,p,setup,record,save):
            capture=self.root/'incident-raw';capture.mkdir()
            manifest={'schema_version':'opsloop-engineering-capture/v1','evidence_kind':'observed',**{k:auth[k] for k in ['run_id','epoch','cluster_uid','api_origin']},'files':{}}
            for name in FILES:
                sha=write_new(capture/name,raw[name]);manifest['files'][name]={'sha256':sha,'observed_at':dt.datetime.now(dt.timezone.utc).isoformat(),'evidence_kind':'observed'}
            manifest['captured_at']=dt.datetime.now(dt.timezone.utc).isoformat()
            record.update(captureManifestSHA256=write_new(capture/'capture-manifest.json',manifest),engineeringAuthorizationSHA256=write_new(capture/'engineering-authorization.json',auth),incidentUID=auth['incident_uid'],stopReceipt={'state':{'phase':'STOP_CONFIRMED'}})
        class C:
            def __init__(self,*args):pass
        core=SimpleNamespace(finish_guard=lambda *a:None,native=lambda *a:None,load=lambda *a:(self.runtime,None,None),Client=C,trial=trial)
        fake_spec=SimpleNamespace(loader=SimpleNamespace(exec_module=lambda module:None))
        events=[]
        with patch('tools.exhibition_handoff.time.time',return_value=dt.datetime.now(dt.timezone.utc).timestamp()-(500 if current else 0)), patch('tools.exhibition_handoff.importlib.util.spec_from_file_location',return_value=fake_spec),patch('tools.exhibition_handoff.importlib.util.module_from_spec',return_value=core),patch('tools.engineering_evidence.seal.verify_source',return_value=prov['source_files']):
            bound=collect(self.plan,lambda *args:events.append(args),threading.Event())
        actual=Path(bound['engineering']['bundle']).read_bytes()
        self.assertEqual(digest(actual),bound['engineering']['bundle_sha256'])
        self.assertEqual(json.loads(actual)['incident']['run_id'],auth['run_id'])
        budget=json.loads(Path(bound['engineering']['execution_plan']).read_bytes())
        self.assertEqual(budget['bundle_sha256'],digest(actual));self.assertEqual(budget['max_model_calls'],1)
        self.assertEqual(events[-1][1],'EVIDENCE_SEALED')
