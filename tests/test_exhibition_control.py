"""Synthetic coordinator tests; no Kubernetes, provider or GitHub requests."""
import datetime as dt
import hashlib
import json
from pathlib import Path
import tempfile
import threading
import unittest
from tools.exhibition_control import Coordinator, ControlError, public_data


class SyntheticLifecycle:
    """Coordinator-only fixture. Real archive/cleanup boundaries have separate tests."""
    def verify_run(self, run):
        return {}
    def config(self, plan):
        return {}
    def reconcile(self, plan, receipt, emit):
        return {'binding': plan['binding'], 'stopConfirmed': True, 'baselineRestored': True,
                'modelStopped': True, 'evidenceArchived': True, 'objectsCleaned': True,
                'lifecycleReceiptSHA256': 'a' * 64}


class Adapter:
    def __init__(self):
        self.calls = 0
        self.release = threading.Event()
        self.entered = threading.Event()
        self.unconfirmed = False
    def prepare(self, plan):
        pass
    def reconcile(self, plan, emit):
        return {'binding': plan['binding'], 'stopConfirmed': True, 'modelStopped': True}
    def run(self, plan, emit, cancel):
        self.calls += 1
        self.entered.set()
        emit('observer', 'BASELINE_CONFIRMED', {'ok': True, 'token': 'PRIVATE', 'reasonCode': 'Bearer PRIVATE'})
        self.release.wait(2)
        return {'state': 'UNCONFIRMED_TERMINATION' if self.unconfirmed else 'STOPPED' if cancel.is_set() else 'COMPLETED', 'stopConfirmed': not self.unconfirmed}


class ControlTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.plan = {'schema': 'opsloop-exhibition-plan/v1', 'planId': 'plan-1', 'authorizationId': 'auth-1',
            'track': 'runtime', 'source': 'measured', 'binding': {'runID': 'run-1', 'epoch': 'epoch-1', 'policyUID': 'policy-1', 'deploymentUID': 'deployment-1'},
            'expiresAt': (dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=5)).isoformat(), 'totalSeconds': 30, 'approvalReference': 'synthetic-test-only'}
        self.path = self.root / 'plan.json'
        self.path.write_text(json.dumps(self.plan))
        self.config = {'schema': 'opsloop-exhibition-control/v1', 'plans': [{'path': str(self.path), 'sha256': hashlib.sha256(self.path.read_bytes()).hexdigest()}]}
        self.adapter = Adapter()
        self.coordinator = Coordinator(self.root / 'state', self.config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
    def tearDown(self):
        self.adapter.release.set()
        self.coordinator.close()
        self.tmp.cleanup()
    def start(self):
        self.coordinator.prepare('plan-1')
        return self.coordinator.start('plan-1', 'request-1')
    def test_duplicate_click_consumes_once_and_reconnects_ordered_events(self):
        run = self.start()
        self.adapter.entered.wait(1)
        duplicate = self.coordinator.start('plan-1', 'request-1')
        self.assertEqual(run['runId'], duplicate['runId'])
        self.assertEqual(self.adapter.calls, 1)
        with self.assertRaises(ControlError):
            self.coordinator.start('plan-1', 'request-2')
        self.adapter.release.set()
        self.coordinator.worker.join(2)
        events = self.coordinator.events('run-1')['events']
        self.assertEqual([e['seq'] for e in events], list(range(1, len(events)+1)))
        self.assertNotIn('PRIVATE', json.dumps(events))
        self.assertEqual(self.coordinator.events('run-1', 1)['events'], events[1:])
        self.assertIsNone(self.coordinator.state()['active'])
        self.assertEqual(self.coordinator.state()['plans'], [])
        with self.assertRaises(ControlError):
            self.coordinator.prepare('plan-1')
    def test_stop_is_not_acknowledged_before_adapter_receipt(self):
        self.start()
        self.adapter.entered.wait(1)
        result = self.coordinator.stop('run-1')
        self.assertEqual(result['state'], 'STOPPING')
        self.assertFalse(result['stopConfirmed'])
        self.adapter.release.set()
        self.coordinator.worker.join(2)
        self.assertEqual(self.coordinator.state()['runs'][0]['state'], 'STOPPED')
    def test_changed_plan_cannot_start(self):
        self.coordinator.prepare('plan-1')
        self.path.write_text(self.path.read_text() + ' ')
        with self.assertRaises(ControlError):
            self.coordinator.start('plan-1', 'request-1')
        self.assertEqual(self.adapter.calls, 0)
    def test_restart_keeps_uncertain_run_blocking_and_ledger(self):
        self.adapter.unconfirmed = True
        self.start()
        self.adapter.release.set()
        self.coordinator.worker.join(2)
        self.coordinator.close()
        self.coordinator = Coordinator(self.root / 'state', self.config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
        self.assertEqual(self.coordinator.state()['active']['state'], 'UNCONFIRMED_TERMINATION')
        with self.assertRaises(ControlError):
            self.coordinator.prepare('plan-1')
        with self.assertRaises(ControlError):
            self.coordinator.stop('run-1')
    def test_cleanup_after_restart_does_not_reexecute_experiment(self):
        self.adapter.unconfirmed = True
        self.start(); self.adapter.release.set(); self.coordinator.worker.join(2)
        self.coordinator.close()
        self.coordinator = Coordinator(self.root / 'state', self.config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
        self.assertFalse(self.coordinator.state()['nextRunAllowed'])
        with self.assertRaises(ControlError): self.coordinator.prepare('plan-1')
        before = self.coordinator.events('run-1')['events']
        self.coordinator.reconcile('run-1'); self.coordinator.worker.join(2)
        self.assertEqual(self.adapter.calls, 1)
        self.assertTrue(self.coordinator.state()['nextRunAllowed'])
        self.assertEqual(self.coordinator.events('run-1')['events'][:len(before)], before)
        self.assertTrue(self.coordinator.state()['runs'][0]['lifecycleComplete'])
        with self.assertRaises(ControlError): self.coordinator.prepare('plan-1')

    def test_legacy_stop_without_lifecycle_is_blocked_after_restart(self):
        self.start(); self.adapter.release.set(); self.coordinator.worker.join(2)
        row = self.coordinator.state()['runs'][0]
        row.pop('lifecycleComplete'); row.pop('lifecycle'); row.pop('lifecycleReceiptSHA256')
        self.coordinator._save(row); self.coordinator.close()
        self.coordinator = Coordinator(self.root / 'state', self.config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
        self.assertFalse(self.coordinator.state()['nextRunAllowed'])
        self.assertEqual(self.coordinator.state()['active']['state'], 'RECONCILIATION_REQUIRED')
        with self.assertRaises(ControlError): self.coordinator.prepare('plan-1')

    def test_completed_archive_recheck_failure_blocks_next(self):
        self.start(); self.adapter.release.set(); self.coordinator.worker.join(2)
        def fail(run): raise ControlError('ARCHIVE_CHANGED')
        self.coordinator.lifecycle.verify_run = fail
        self.assertFalse(self.coordinator.state()['nextRunAllowed'])
        self.assertEqual(self.coordinator.state()['active']['state'], 'RECONCILIATION_REQUIRED')

    def test_second_coordinator_rejected(self):
        with self.assertRaises(ControlError):
            Coordinator(self.root / 'state', self.config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
    def test_prepare_required(self):
        with self.assertRaises(ControlError):
            self.coordinator.start('plan-1', 'request-1')
    def test_unknown_fields_and_private_data_are_not_public(self):
        result = public_data({'stdout': 'PRIVATE', 'checks': [{'name': 'unit', 'passed': True, 'stderr': 'PRIVATE'}], 'pod': '/home/operator/secret', 'ok': True})
        self.assertEqual(result, {'checks': [{'name': 'unit', 'passed': True}], 'pod': None, 'ok': True})

if __name__ == '__main__':
    unittest.main()

class PlanInboxTests(unittest.TestCase):
    """Ephemeral local signing keys and synthetic adapters; never operational keys."""
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)
        from tests.test_exhibition_permit import fixture
        self.fixture=fixture
        self.first=self.make_plan('1')
        self.pointer=self.root/'inbox.json'
        self.config={'schema':'opsloop-exhibition-control/v1','plans':[],
                     'planInbox':{'path':str(self.pointer),'publicKey':self.first['exhibitionPermit']['publicKey']}}
        self.adapter=Adapter()
        self.coordinator=Coordinator(self.root/'state',self.config,{'runtime':self.adapter},lifecycle=SyntheticLifecycle())
        self.addCleanup(self.close)
    def close(self):
        self.adapter.release.set();self.coordinator.close()
    def make_plan(self,suffix,key=None):
        payload,_,permit,_,_,key,_=self.fixture(self.root,suffix,key)
        self.key=key
        Path(permit['path']).chmod(0o600)
        plan={'schema':'opsloop-exhibition-plan/v1','planId':'inbox-plan-'+suffix,
              'authorizationId':payload['authorizationID'],'track':'runtime','source':'measured',
              'binding':{k:payload['grant']['scope'][k] for k in ('runID','epoch','policyUID','deploymentUID')},
              'expiresAt':payload['grant']['notAfter'],'totalSeconds':30,'approvalReference':'LOCAL_SYNTHETIC_ONLY',
              'exhibitionPermit':permit}
        file=self.root/('inbox-plan-'+suffix+'.json');file.write_text(json.dumps(plan));file.chmod(0o600)
        return plan
    def publish(self,plan,mode=0o600):
        file=self.root/(plan['planId']+'.json');file.write_text(json.dumps(plan));file.chmod(0o600)
        ref={'path':str(file),'sha256':hashlib.sha256(file.read_bytes()).hexdigest()}
        temp=self.root/'inbox.tmp';temp.write_text(json.dumps(ref));temp.chmod(mode);temp.replace(self.pointer)
        return ref
    def test_signed_next_plan_loads_only_after_previous_cleanup(self):
        self.publish(self.first)
        self.assertEqual(self.coordinator.state()['plans'][0]['planId'],self.first['planId'])
        self.coordinator.prepare(self.first['planId']);self.coordinator.start(self.first['planId'],'click-1')
        self.adapter.entered.wait(1)
        second=self.make_plan('2',self.key);self.publish(second)
        self.assertNotIn(second['planId'],[p['planId'] for p in self.coordinator.state()['plans']])
        with self.assertRaises(ControlError):self.coordinator.prepare(second['planId'])
        self.adapter.release.set();self.coordinator.worker.join(2)
        self.assertEqual(self.coordinator.state()['plans'][0]['planId'],second['planId'])
        self.coordinator.prepare(second['planId']);self.coordinator.start(second['planId'],'click-2');self.coordinator.worker.join(2)
        self.assertEqual(self.adapter.calls,2)
        self.assertEqual(self.coordinator.db.execute('SELECT COUNT(*) FROM plan_pins').fetchone()[0],2)
    def test_inbox_plan_refs_survive_restart_without_pointer_for_cleanup(self):
        self.publish(self.first);self.adapter.unconfirmed=True
        self.coordinator.prepare(self.first['planId']);self.coordinator.start(self.first['planId'],'click-1')
        self.adapter.release.set();self.coordinator.worker.join(2);self.coordinator.close()
        self.pointer.unlink()
        self.coordinator=Coordinator(self.root/'state',self.config,{'runtime':self.adapter},lifecycle=SyntheticLifecycle())
        self.assertFalse(self.coordinator.state()['nextRunAllowed'])
        self.coordinator.reconcile('run-1');self.coordinator.worker.join(2)
        self.assertTrue(self.coordinator.state()['nextRunAllowed']);self.assertEqual(self.adapter.calls,1)
        self.assertEqual(self.coordinator.db.execute('SELECT COUNT(*) FROM plan_pins').fetchone()[0],1)
    def test_wrong_key_and_forged_signature_are_not_loaded(self):
        wrong=self.make_plan('2',self.root/'untrusted-test-key.pem');self.publish(wrong)
        state=self.coordinator.state();self.assertEqual(state['plans'],[])
        self.assertEqual(state['planInboxError'],'PLAN_INBOX_UNTRUSTED_KEY')
        with self.assertRaises(ControlError):self.coordinator.prepare(wrong['planId'])
        import base64
        envelope=Path(self.first['exhibitionPermit']['path']);value=json.loads(envelope.read_text())
        value['signature']=base64.b64encode(b'x'*64).decode();envelope.write_text(json.dumps(value))
        self.first['exhibitionPermit']['sha256']=hashlib.sha256(envelope.read_bytes()).hexdigest();self.publish(self.first)
        self.assertEqual(self.coordinator.state()['plans'],[])
        self.assertEqual(self.coordinator.db.execute('SELECT COUNT(*) FROM plan_pins').fetchone()[0],0)
        self.assertEqual(self.adapter.calls,0)

    def test_withdrawn_or_replaced_pointer_cannot_start_prepared_plan(self):
        for action in ('remove', 'replace', 'public-mode', 'alter-fields'):
            with self.subTest(action=action):
                self.publish(self.first)
                self.coordinator.prepare(self.first['planId'])
                if action == 'remove':
                    self.pointer.unlink()
                elif action == 'replace':
                    self.publish(self.make_plan('2', self.key))
                elif action == 'public-mode':
                    self.pointer.chmod(0o644)
                else:
                    self.pointer.write_text(json.dumps({'planId': self.first['planId']}))
                with self.assertRaises(ControlError):
                    self.coordinator.start(self.first['planId'], 'withdrawn-' + action)
                self.assertEqual(self.adapter.calls, 0)
                self.assertEqual(self.coordinator.db.execute('SELECT COUNT(*) FROM runs').fetchone()[0], 0)
                self.assertEqual(self.coordinator._plan(self.first['planId'], current=False), self.first)
    def test_changed_plan_id_pin_never_replaces_durable_ref(self):
        original=self.publish(self.first);self.coordinator.state()
        changed=dict(self.first);changed['approvalReference']='CHANGED_AFTER_APPROVAL'
        other=self.root/'other-plan.json';other.write_text(json.dumps(changed));other.chmod(0o600)
        self.pointer.write_text(json.dumps({'path':str(other),'sha256':hashlib.sha256(other.read_bytes()).hexdigest()}));self.pointer.chmod(0o600)
        state=self.coordinator.state();self.assertEqual(state['planInboxError'],'PLAN_PIN_CONFLICT')
        self.assertEqual(self.coordinator.pins[self.first['planId']],original)
        self.assertEqual(self.coordinator.db.execute('SELECT COUNT(*) FROM plan_pins').fetchone()[0],1)
    def test_public_mode_symlink_and_browser_like_pointer_fields_rejected(self):
        self.publish(self.first,mode=0o644)
        self.assertEqual(self.coordinator.state()['planInboxError'],'PLAN_INBOX_PRIVATE_FILE_REQUIRED')
        self.pointer.unlink();self.pointer.symlink_to(self.root/'inbox-plan-1.json')
        self.assertEqual(self.coordinator.state()['planInboxError'],'PLAN_INBOX_SYMLINK')
        self.pointer.unlink();self.pointer.write_text(json.dumps({'planId':'inbox-plan-1','publicKey':self.config['planInbox']['publicKey']}));self.pointer.chmod(0o600)
        self.assertEqual(self.coordinator.state()['planInboxError'],'PLAN_INBOX_INVALID_POINTER')
        self.assertEqual(self.coordinator.state()['plans'],[])
        self.publish(self.first)
        Path(self.first['exhibitionPermit']['path']).chmod(0o644)
        self.assertEqual(self.coordinator.state()['planInboxError'], 'PLAN_INBOX_PRIVATE_REFERENCE_REQUIRED')
        self.assertEqual(self.coordinator.state()['plans'], [])
