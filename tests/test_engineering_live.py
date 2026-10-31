"""Synthetic/local orchestration tests; no model or runtime success evidence."""
import copy
import datetime as dt
import json
from pathlib import Path
import shutil
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from tests.test_engineering_runner import bundle, ROOT
from tools.engineering_runner.contracts import ALLOWED_PATHS, Rejected, canonical, digest, inventory
from tools.engineering_runner.live import PublicEvents, comparison, execution_plan, public_checks
from tools.engineering_runner.runner import run
from tools.engineering_runner.sandbox import Sandbox, Limits, execute


class LiveRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        (self.source / 'apps').mkdir(parents=True)
        shutil.copytree(ROOT / 'apps/engineering-demo', self.source / 'apps/engineering-demo')
        self.bundle = bundle()
        self.bundle['source']['source_files'] = inventory(self.source)
        self.bundle['authorization']['expires_at'] = (dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=30)).strftime('%Y-%m-%dT%H:%M:%SZ')
        data = canonical(self.bundle); (self.root / 'bundle.json').write_bytes(data)
        harness = ROOT / 'tests/engineering'
        self.args = SimpleNamespace(approve_model_execution=True, model='gpt-6-luna', reasoning_effort='medium',
            auth_mode='chatgpt', bundle=self.root / 'bundle.json', bundle_sha256=digest(data),
            repository=self.bundle['source']['repository'], base_commit='b' * 40,
            state=self.root / 'state', repository_path=self.root, harness=harness,
            harness_sha256=digest(canonical(inventory(harness))), go_root=self.root,
            codex_binary=self.root / 'codex', auth_file=self.root / 'auth')

    def plan(self, calls=2):
        p = {'schema':'opsloop-engineering-plan/v1', 'bundle_sha256':self.args.bundle_sha256,
            'run_id':self.bundle['incident']['run_id'], 'epoch':self.bundle['incident']['epoch'],
            'policy_uid':self.bundle['workload']['policy_uid'], 'deployment_uid':self.bundle['workload']['deployment_uid'],
            'model':self.args.model, 'reasoning_effort':self.args.reasoning_effort,
            'max_model_calls':calls, 'total_seconds':900, 'expires_at':self.bundle['authorization']['expires_at']}
        self.args.execution_plan = self.root / 'plan.json'
        self.args.execution_plan.write_bytes(canonical(p))
        self.args.execution_plan_sha256 = digest(canonical(p))
        return p

    @staticmethod
    def verification(accepted, baseline=False):
        return {'accepted':accepted, 'report':{'checks':[{'name':'empty-order-regression',
            'passed':not baseline and accepted, 'status':500 if baseline else 200,
            'fixture_sha256':digest(b'{"items":[]}'), 'output':'secret provider log'}]}}

    def invoke(self, results, *, cancel=None, model_failure=False, dispatched=True):
        def export(_, __, destination):
            shutil.copytree(self.source, destination)
            return inventory(destination)
        def model(sandbox, command, output, limits, input_bytes=b'', **kwargs):
            proposed = {'schema':'opsloop-code-proposal/v1', 'base_commit':'b'*40,
                        'source_sha256':digest(canonical(inventory(self.source))), 'rationale':'SYNTHETIC TEST',
                        'files':[{'path':p, 'content':(self.source/p).read_text()+'\n// synthetic boundary fixture\n'} for p in ALLOWED_PATHS]}
            output.write_bytes(canonical({'opsloop_model_proposal':proposed}))
            return {'exit_code':1 if model_failure else 0, 'termination':None}
        broker = Mock(); broker.__enter__ = Mock(return_value=broker); broker.__exit__ = Mock(return_value=None)
        broker.capability = 'private-capability'; broker.summary.return_value = {'requests':1, 'dispatched_requests':int(dispatched)}
        emitted = []
        with patch('tools.engineering_runner.runner.pinned_source', side_effect=export), \
             patch('tools.engineering_runner.runner.preflight', return_value={}), \
             patch('tools.engineering_runner.runner.ModelBroker', return_value=broker) as factory, \
             patch('tools.engineering_runner.runner.execute', side_effect=model), \
             patch('tools.engineering_runner.runner.verify', side_effect=results):
            result = run(self.args, event_sink=emitted.append, cancel_event=cancel)
        return result, emitted, factory

    def test_retry_only_after_verified_failure_and_same_input_success(self):
        self.plan(2)
        result, events, factory = self.invoke([self.verification(True, True), self.verification(False), self.verification(True)])
        self.assertEqual(result['state'], 'VERIFIED_PENDING_REVIEW')
        self.assertEqual(factory.call_count, 2)
        self.assertEqual(len(result['attempts']), 2)
        self.assertTrue(result['comparison']['same_input_confirmed'])
        self.assertEqual([e['sequence'] for e in events], list(range(1, len(events)+1)))
        stages = [e['stage'] for e in events]
        self.assertLess(stages.index('VERIFICATION_RESULT'), stages.index('REVISION_REQUESTED'))
        self.assertNotIn('secret provider log', json.dumps(events))
        self.assertNotIn('private-capability', json.dumps(events))
        self.assertTrue((self.args.state/'runs'/result['run_id']/'candidate.patch').is_file())
        self.assertTrue(Path(result['patch_path']).parent.name == 'attempt-2')
        # Ledger remains durable: new HTTP requests cannot start the same failure again.
        with self.assertRaisesRegex(Rejected, 'duplicate'):
            self.invoke([])

    def test_budget_exhaustion_and_transport_failure_never_retry(self):
        self.plan(3)
        result, events, factory = self.invoke([self.verification(True, True)], model_failure=True)
        self.assertEqual(result['state'], 'ENGINEERING_FAILED')
        self.assertEqual(factory.call_count, 1)
        self.assertNotIn('REVISION_REQUESTED', [e['stage'] for e in events])

    def test_default_is_one_call_and_failure_does_not_auto_escalate(self):
        result, _, factory = self.invoke([self.verification(True, True), self.verification(False)])
        self.assertEqual(result['state'], 'ENGINEERING_FAILED')
        self.assertEqual(factory.call_count, 1)

    def test_success_with_different_input_is_rejected(self):
        candidate = self.verification(True)
        candidate['report']['checks'][0]['fixture_sha256'] = '0' * 64
        result, _, factory = self.invoke([self.verification(True, True), candidate])
        self.assertEqual(result['state'], 'ENGINEERING_FAILED')
        self.assertIn('same-input', result['failure'])
        self.assertEqual(factory.call_count, 1)

    def test_reserved_request_without_dispatch_cannot_claim_real_model_execution(self):
        result, _, factory = self.invoke([self.verification(True, True)], dispatched=False)
        self.assertEqual(result['state'], 'ENGINEERING_FAILED')
        self.assertFalse(result['real_model_execution'])
        self.assertEqual(factory.call_count, 1)

    def test_plan_digest_binding_budget_and_expiry(self):
        base = self.plan()
        for key, value in [('model','gpt-6-astra'), ('epoch','other'), ('policy_uid','other'),
                           ('deployment_uid','other'), ('max_model_calls',4), ('max_model_calls',True),
                           ('total_seconds',901), ('expires_at','2020-01-01T00:00:00Z')]:
            p = dict(base); p[key] = value
            self.args.execution_plan.write_bytes(canonical(p)); self.args.execution_plan_sha256 = digest(canonical(p))
            with self.subTest(key=key, value=value), self.assertRaises(Rejected):
                execution_plan(self.args, self.bundle)
        self.plan(); self.args.execution_plan_sha256 = '0'*64
        with self.assertRaises(Rejected): execution_plan(self.args, self.bundle)

    def test_cancel_before_claim_never_starts_model(self):
        cancelled = threading.Event(); cancelled.set()
        with self.assertRaisesRegex(Rejected, 'CANCELLED'):
            self.invoke([], cancel=cancelled)
        self.assertFalse(self.args.state.exists())

    def test_public_projection_ignores_arbitrary_body_log_and_unknown_checks(self):
        v = self.verification(True)
        v['report']['checks'][0]['body'] = {'token':'never-public'}
        v['report']['checks'].append({'name':'secret-identifier', 'passed':True})
        public = public_checks(v)
        self.assertEqual(len(public), 1)
        self.assertNotIn('never-public', json.dumps(public))
        wrong = copy.deepcopy(v); wrong['report']['checks'][0]['fixture_sha256'] = '0'*64
        self.assertFalse(comparison(v, wrong)['same_input_confirmed'])


class CancellationTests(unittest.TestCase):
    def test_cancel_before_sandbox_launch_has_zero_commands(self):
        stop = threading.Event(); stop.set()
        with tempfile.TemporaryDirectory() as d, patch('tools.engineering_runner.sandbox.subprocess.Popen') as spawn:
            with self.assertRaisesRegex(Rejected, 'CANCELLED'):
                execute(Sandbox(Path(d),Path(d)), ['/usr/bin/true'], Path(d)/'log', Limits(), cancel_event=stop)
            spawn.assert_not_called()

    def test_cancel_before_release_settles_and_does_not_release(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); stop = threading.Event()
            process = Mock(); process.poll.return_value = None
            def confirm(*_): stop.set(); return root/'cgroup', {}
            with patch('tools.engineering_runner.sandbox.subprocess.Popen', return_value=process), \
                 patch('tools.engineering_runner.sandbox._confirm_cgroup', side_effect=confirm), \
                 patch('tools.engineering_runner.sandbox._stop_and_settle') as settle:
                with self.assertRaisesRegex(Rejected, 'CANCELLED'):
                    execute(Sandbox(root,root), ['/usr/bin/true'], root/'log', Limits(), cancel_event=stop)
                settle.assert_called_once()
            self.assertFalse((root/'log.release').exists())

    def test_cancel_after_release_settles_whole_cgroup(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); stop = threading.Event()
            process = Mock(); process.poll.return_value = None; process.wait.return_value = 0
            selector = Mock()
            def wait(**_): stop.set(); return []
            selector.select.side_effect = wait
            with patch('tools.engineering_runner.sandbox.subprocess.Popen', return_value=process), \
                 patch('tools.engineering_runner.sandbox._confirm_cgroup', return_value=(root/'cgroup',{})), \
                 patch('tools.engineering_runner.sandbox.selectors.DefaultSelector', return_value=selector), \
                 patch('tools.engineering_runner.sandbox._stop_and_settle') as settle:
                result = execute(Sandbox(root,root), ['/usr/bin/true'], root/'log', Limits(), cancel_event=stop)
                self.assertEqual(result['termination'], 'CANCELLED')
                settle.assert_called_once_with(unittest.mock.ANY, process, root/'cgroup')
            self.assertTrue((root/'log.release').exists())

    def test_cancel_cleanup_failure_never_claims_confirmed_stop(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); stop = threading.Event(); process = Mock()
            def confirm(*_): stop.set(); return root/'cgroup',{}
            with patch('tools.engineering_runner.sandbox.subprocess.Popen', return_value=process), \
                 patch('tools.engineering_runner.sandbox._confirm_cgroup', side_effect=confirm), \
                 patch('tools.engineering_runner.sandbox._stop_and_settle', side_effect=Rejected('UNCONFIRMED_TERMINATION')):
                with self.assertRaisesRegex(Rejected,'UNCONFIRMED_TERMINATION'):
                    execute(Sandbox(root,root), ['/usr/bin/true'], root/'log', Limits(), cancel_event=stop)

    def test_broker_stop_or_expiry_during_connect_never_sends_post(self):
        from tools.engineering_runner.broker import ModelBroker
        for expired in (False, True):
            with self.subTest(expired=expired), tempfile.TemporaryDirectory() as d:
                auth = Path(d)/'auth'; auth.write_text(json.dumps({'tokens':{'access_token':'fixture', 'account_id':'fixture'}}))
                stop = threading.Event()
                sent_event = Mock()
                broker = ModelBroker(Path(d)/'broker',auth,'gpt-6-luna','chatgpt',cancel_event=stop,on_request=sent_event)
                connection = Mock()
                def connect():
                    if expired: broker.started -= 1000
                    else: stop.set()
                connection.connect.side_effect = connect
                with patch('tools.engineering_runner.broker.http.client.HTTPSConnection',return_value=connection):
                    with self.assertRaises(Rejected): broker._forward(Mock(), b'{}')
                connection.request.assert_not_called()
                self.assertEqual(broker.dispatched_requests, 0)
                sent_event.assert_not_called()
                connection.close.assert_called_once()

    def test_dispatch_event_occurs_only_after_post_and_callback_expiry_stops_response(self):
        from tools.engineering_runner.broker import ModelBroker
        with tempfile.TemporaryDirectory() as d:
            auth = Path(d)/'auth'; auth.write_text(json.dumps({'tokens':{'access_token':'fixture', 'account_id':'fixture'}}))
            broker = ModelBroker(Path(d)/'broker',auth,'gpt-6-luna','chatgpt')
            broker.on_request = lambda: setattr(broker, 'started', broker.started - 1000)
            connection = Mock()
            with patch('tools.engineering_runner.broker.http.client.HTTPSConnection',return_value=connection):
                with self.assertRaises(Rejected): broker._forward(Mock(), b'{}')
            connection.request.assert_called_once()
            connection.getresponse.assert_not_called()
            self.assertEqual(broker.dispatched_requests, 1)

    def test_cancelled_broker_denies_request_before_forward(self):
        from tools.engineering_runner.broker import ModelBroker
        with tempfile.TemporaryDirectory() as d:
            auth = Path(d)/'auth'; auth.write_text(json.dumps({'tokens':{'access_token':'fixture', 'account_id':'fixture'}}))
            stop = threading.Event(); stop.set()
            broker = ModelBroker(Path(d)/'broker', auth, 'gpt-6-luna', 'chatgpt', cancel_event=stop)
            handler = Mock()
            with patch.object(broker, '_forward') as forward:
                broker.handle(handler)
                forward.assert_not_called(); self.assertEqual(broker.requests, 0)


if __name__ == '__main__': unittest.main()
