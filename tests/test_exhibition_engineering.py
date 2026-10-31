"""Local adapter contract tests; mocked Runner, no model/network/process execution."""
import copy
from pathlib import Path
import threading
import unittest
from unittest.mock import patch

from tests import test_engineering_live as fixtures
from tools.engineering_runner.contracts import canonical, digest
from tools.exhibition_control import ControlError
from tools.exhibition_engineering import EngineeringAdapter


class EngineeringAdapterTests(unittest.TestCase):
    plan = fixtures.LiveRunnerTests.plan
    def setUp(self):
        fixtures.LiveRunnerTests.setUp(self)
        self.plan()
        self.args.auth_file.write_text('{}'); self.args.auth_file.chmod(0o600)
        self.args.codex_binary.write_bytes(b'local synthetic executable')
        (self.root/'bin').mkdir(); (self.root/'bin/go').write_bytes(b'local synthetic go')
        b = self.bundle
        self.public_plan = {'schema':'opsloop-exhibition-plan/v1','planId':'plan-1','authorizationId':'auth-1',
            'track':'engineering','source':'measured','binding':{'runID':b['incident']['run_id'],'epoch':b['incident']['epoch'],
            'policyUID':b['workload']['policy_uid'],'deploymentUID':b['workload']['deployment_uid']},
            'expiresAt':b['authorization']['expires_at'],'totalSeconds':900,'approvalReference':'local-test-only',
            'model':'gpt-6-luna','maxModelCalls':2,
            'engineering':{k:str(getattr(self.args,k)) if isinstance(getattr(self.args,k),Path) else getattr(self.args,k)
                for k in ('bundle','bundle_sha256','state','repository_path','repository','base_commit','harness',
                          'harness_sha256','go_root','codex_binary','auth_file','execution_plan','execution_plan_sha256')}}
        self.public_plan['engineering'].update(go_binary_sha256=digest((self.root/'bin/go').read_bytes()),
                                               codex_binary_sha256=digest(self.args.codex_binary.read_bytes()))
        self.adapter = EngineeringAdapter()

    def verified_result(self):
        from tools.engineering_runner.live import public_checks, CHECKS
        baseline={'accepted':True,'source_sha256':'c'*64,'report':{'baseline_defect_reproduced':True,'checks':[{'name':'empty-order-regression','passed':False,'status':500,'fixture_sha256':'d'*64}]}}
        candidate={'accepted':True,'source_sha256':'a'*64,'report':{'all_passed':True,'checks':[{'name':'empty-order-regression','passed':True,'status':200,'fixture_sha256':'d'*64}]}}
        for name in CHECKS:
            if name!='empty-order-regression':
                baseline['report']['checks'].append({'name':name,'passed':True})
                candidate['report']['checks'].append({'name':name,'passed':True})
        return {'state':'VERIFIED_PENDING_REVIEW','candidate':{'source_sha256':'a'*64},'patch_sha256':'b'*64,
            'incident':{'run_id':self.bundle['incident']['run_id'],'epoch':self.bundle['incident']['epoch']},
            'bundle_sha256':self.args.bundle_sha256,'real_model_execution':True,'broker':{'dispatched_requests':1},
            'baseline_verification':baseline,'candidate_verification':candidate,
            'comparison':{'same_input_confirmed':True,'baseline':public_checks(baseline),'candidate':public_checks(candidate)}}

    def test_success_label_without_same_run_evidence_rejected(self):
        for change in ('missing','run','epoch','model','checks','comparison','fixture'):
            result=self.verified_result()
            if change=='missing':result={'state':'VERIFIED_PENDING_REVIEW'}
            elif change in ('run','epoch'):result['incident']['run_id' if change=='run' else 'epoch']='foreign'
            elif change=='model':result['broker']['dispatched_requests']=0
            elif change=='checks':result['candidate_verification']['report']['all_passed']=False
            elif change=='fixture':
                result['candidate_verification']['report']['checks'][0]['fixture_sha256']='e'*64
                result['comparison']['candidate'][0]['fixture_sha256']='e'*64
            else:result['comparison']['same_input_confirmed']=False
            events=[]
            with patch('tools.exhibition_engineering.run_engineering',return_value=result),self.assertRaisesRegex(ControlError,'VERIFIED_RESULT'):
                self.adapter.run(self.public_plan,lambda *e:events.append(e),threading.Event())
            self.assertFalse(any(e[1]=='VERIFIED_PENDING_REVIEW' for e in events))

    def test_prepare_validates_local_pins_without_starting_runner(self):
        with patch('tools.exhibition_engineering.run_engineering') as run:
            self.adapter.prepare(self.public_plan)
            run.assert_not_called()
        for field, value in [('maxModelCalls',3),('model','gpt-6-astra'),('totalSeconds',800)]:
            p=copy.deepcopy(self.public_plan); p[field]=value
            with self.subTest(field=field), self.assertRaises(ControlError): self.adapter.prepare(p)
        self.args.codex_binary.write_bytes(b'changed')
        with self.assertRaisesRegex(ControlError,'CHANGED'): self.adapter.prepare(self.public_plan)

    def test_never_claims_deployment_and_unknown_termination_stays_blocked(self):
        result=self.verified_result()
        emitted=[]
        with patch('tools.exhibition_engineering.run_engineering', return_value=result):
            outcome=self.adapter.run(self.public_plan,lambda *e: emitted.append(e),threading.Event())
        self.assertEqual(outcome,{'state':'COMPLETED','stopConfirmed':True})
        self.assertFalse(emitted[-1][2]['deployed']); self.assertFalse(emitted[-1][2]['serviceRecovered'])
        for state in ('UNCONFIRMED_TERMINATION','unknown'):
            with patch('tools.exhibition_engineering.run_engineering',return_value={'state':state}):
                self.assertEqual(self.adapter.run(self.public_plan,lambda *e:None,threading.Event()),
                                 {'state':'UNCONFIRMED_TERMINATION','stopConfirmed':False})

    def test_current_cancellation_never_calls_runner(self):
        stopped=threading.Event(); stopped.set()
        with patch('tools.exhibition_engineering.run_engineering') as run:
            self.assertEqual(self.adapter.run(self.public_plan,lambda *e:None,stopped),{'state':'STOPPED','stopConfirmed':True})
            run.assert_not_called()

    def test_adapter_event_has_exact_binding_and_public_fields(self):
        def invoke(args, event_sink, cancel_event):
            event_sink({'incident_run_id':self.bundle['incident']['run_id'],'epoch':self.bundle['incident']['epoch'],
                        'actor':'verifier','stage':'VERIFICATION_RESULT','detail':{'attempt':1,'checks':[
                            {'name':'empty-order-regression','passed':True,'status':200,
                             'fixture_sha256':'a'*64,'output':'private arbitrary output'}]}})
            return {'state':'ENGINEERING_FAILED'}
        emitted=[]
        with patch('tools.exhibition_engineering.run_engineering',side_effect=invoke):
            outcome=self.adapter.run(self.public_plan,lambda *e:emitted.append(e),threading.Event())
        self.assertEqual(outcome,{'state':'FAILED','stopConfirmed':True})
        self.assertEqual(emitted[-1][2]['checks'][0],{'name':'empty-order-regression','passed':True,'httpStatus':200,'inputSHA256':'a'*64})


if __name__ == '__main__': unittest.main()
