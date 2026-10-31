"""Local/synthetic Runtime adapter checks; no cluster or credential access."""
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from tools.exhibition_runtime import (BINDING, GRANT, RuntimeAdapter, RuntimeRejected, Transport,
                                     canonical, pinned, process_json, public_candidate)


NOW = dt.datetime(2026, 10, 28, 0, 0, tzinfo=dt.timezone.utc).timestamp()
def iso(seconds=0):
    return dt.datetime.fromtimestamp(NOW + seconds, dt.timezone.utc).isoformat()


def fixture(root):
    def pin(name, content):
        path = root / name; path.write_bytes(content)
        return {'path': str(path), 'sha256': hashlib.sha256(content).hexdigest()}
    binding = {'runID': 'run-fresh', 'epoch': 'epoch-fresh', 'policyUID': 'policy-fresh', 'deploymentUID': 'deployment-current'}
    spec = {'mode': 'ObserveOnly', 'enabled': True, 'targetRef': {'name': 'demo-api'}}
    grant = {'version': 'opsloop-execution-grant/v1', 'scope': {**binding, 'clusterUID': 'cluster', 'namespaceUID': 'namespace', 'namespace': 'demo-a', 'policy': 'demo-api', 'deployment': 'demo-api', 'policyGeneration': 2, 'policyHash': 'a'*64, 'incident': 'demo-api-policy-f-00001'}, 'instance': 'b'*64, 'notBefore': iso(-1), 'notAfter': iso(400)}
    kubeconfig = pin('kubeconfig.json', b'{}')
    guard = {'version': 'opsloop-workload-safety/v1', **binding, 'namespace': 'demo-a', 'policy': 'demo-api', 'workload': 'demo-api', 'apiOrigin': 'https://cluster.invalid', 'mode': 'hybrid-live', 'evidenceKind': 'observed', 'kubeconfigFile': kubeconfig['path'], 'hardDeadline': iso(500), 'authorityExpires': iso(550), 'heartbeatMs': 10000, 'policyGeneration': 2, 'policySpec': {**spec, 'mode': 'Enforce'}}
    runtime = {'namespace': 'demo-a', 'policy': 'demo-api', 'workload': 'demo-api', 'service': 'demo-api', 'serviceUID': 'service', 'clusterUID': 'cluster', 'namespaceUID': 'namespace', 'container': 'demo-api', 'sourcePod': {'name': 'source-pod', 'uid': 'pod-old'}, 'nodes': [{'name': 'node-a', 'uid': 'node-uid'}], 'faultSeconds': 30, 'baselineSamples': 3, 'recoverySamples': 3, 'stableSeconds': 2, 'serviceURL': 'http://service.invalid/', 'apiOrigin': 'https://cluster.invalid', 'guardConfig': pin('guard.json', canonical(guard)), 'guardBinding': 'c'*64, 'grantBinding': 'd'*64, 'driver': pin('driver', b'offline-fixture-only'), 'kubectl': pin('kubectl', b'offline-fixture-only'), 'kubeconfig': kubeconfig, 'executionGrant': grant, 'policyBefore': {'generation': 1, 'spec': spec}, 'sourceImageID': 'sha256:'+'e'*64, 'sourcePodSpecSHA256': hashlib.sha256(canonical({'nodeName': 'node-a'})).hexdigest(), 'deploymentSpecSHA256': hashlib.sha256(canonical({'replicas': 1})).hexdigest()}
    return {'schema': 'opsloop-exhibition-plan/v1', 'planId': 'plan', 'authorizationId': 'authorization-fresh', 'approvalReference': 'synthetic-local-test-only', 'track': 'runtime', 'source': 'measured', 'binding': binding, 'expiresAt': iso(600), 'totalSeconds': 300, 'runtime': runtime}


class FakeAPI:
    def __init__(self, plan):
        self.plan = plan; self.r = plan['runtime']; self.paths = RuntimeAdapter.paths(self.r)
        self.mutations = []; self.faults = 0; self.closed = False; self.recovered = False
        self.policy = {'metadata': {'uid': plan['binding']['policyUID'], 'resourceVersion': '1', 'generation': 1, 'annotations': {}}, 'spec': copy.deepcopy(self.r['policyBefore']['spec']), 'status': {}}
        self.incident_uid = 'incident-fresh'
    def request(self, path, patch=None, absent=False):
        if patch is not None:
            self.mutations.append(patch)
            for op in patch:
                if op['op'] == 'replace' and op['path'] == '/spec/mode':
                    self.policy['spec']['mode'] = op['value']; self.policy['metadata']['generation'] = 2
                if op['op'] == 'add' and op['path'] == '/metadata/annotations/opsloop.io~1execution-grant':
                    self.policy['metadata']['annotations'][GRANT] = op['value']
            return copy.deepcopy(self.policy)
        if path == self.paths['policy']: return copy.deepcopy(self.policy)
        if path == self.paths['cluster']: return {'metadata': {'uid': 'cluster'}}
        if path == self.paths['namespace']: return {'metadata': {'uid': 'namespace'}}
        if path == self.paths['service']: return {'metadata': {'uid': 'service'}}
        if path == self.paths['deployment']: return {'metadata': {'uid': self.plan['binding']['deploymentUID'], 'generation': 1}, 'spec': {'replicas': 1}, 'status': {'readyReplicas': 1, 'observedGeneration': 1}}
        if path == self.paths['pod'] and absent and self.recovered: return None
        if path == self.paths['pod']: return {'metadata': {'uid': 'pod-old'}, 'spec': {'nodeName': 'node-a'}, 'status': {'containerStatuses': [{'name': 'demo-api', 'imageID': 'sha256:'+'e'*64, 'restartCount': 0, 'ready': True}], 'conditions': [{'type': 'Ready', 'status': 'True'}]}}
        if path.startswith('/api/v1/nodes/'): return {'metadata': {'uid': 'node-uid'}, 'status': {'conditions': [{'type': k, 'status': 'True' if k == 'Ready' else 'False'} for k in ('Ready', 'MemoryPressure', 'DiskPressure', 'PIDPressure')]}}
        if path == self.paths['incident']:
            return {'metadata': {'uid': self.incident_uid, 'annotations': {BINDING: 'd'*64}}, 'spec': {'policyRef': {'uid': self.plan['binding']['policyUID']}, 'workload': {'uid': self.plan['binding']['deploymentUID']}, 'policyGeneration': 2, 'policyHash': 'a'*64}, 'status': {'runtime': {'phase': 'RECOVERED', 'decision': {'actionID': 'action-1', 'selectedNodeName': 'node-a', 'candidates': [{'nodeName': 'node-a', 'nodeUID': 'node-uid', 'eligible': True, 'measurements': {'cpuUtilizationPermille': 200, 'secret': 'PRIVATE_SENTINEL'}}]}}}}
        if path == '/api/v1/namespaces/demo-a/pods/new-pod':
            return {'metadata': {'uid': 'pod-new', 'ownerReferences': [{'kind': 'ReplicaSet', 'name': 'new-rs', 'uid': 'rs-uid', 'controller': True}]}, 'spec': {'nodeName': 'node-a'}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]}}
        if path == '/apis/apps/v1/namespaces/demo-a/replicasets/new-rs':
            return {'metadata': {'uid': 'rs-uid', 'ownerReferences': [{'kind': 'Deployment', 'uid': self.plan['binding']['deploymentUID'], 'controller': True}]}}
        if path == self.paths['endpoints']:
            return {'items': [{'metadata': {'ownerReferences': [{'kind': 'Service', 'uid': 'service'}]}, 'endpoints': [{'conditions': {'ready': True}, 'targetRef': {'kind': 'Pod', 'namespace': 'demo-a', 'uid': 'pod-new'}}]}]}
        raise AssertionError(path)
    def probe(self): return {'httpStatus': 200, 'latencyMs': 1, 'podUID': 'pod-new' if self.recovered else 'pod-old', 'pod': 'new-pod' if self.recovered else 'source-pod', 'namespace': 'demo-a'}
    def fault(self, cancel):
        if cancel.is_set(): raise RuntimeRejected('CANCELLED')
        self.faults += 1; self.recovered = True
        self.policy['status']['activeIncident'] = {'name': self.r['executionGrant']['scope']['incident'], 'incidentUID': self.incident_uid}
        return {'podUID': 'pod-old', 'durationSeconds': 30}
    def close(self): self.closed = True


class FakeWatcher:
    def poll(self): return None
    def wait(self, timeout): return 0


class FakeAdapter(RuntimeAdapter):
    def reconcile(self, plan, emit):
        return {'binding':plan['binding'],'stopConfirmed':True,'modelStopped':True}
    def __init__(self, api):
        super().__init__(lambda _: api, clock=lambda: NOW)
        self.calls = []; self.phase = 'UNARMED'; self.fail_stop = False; self.cancel_on_activation = None
    def cli(self, r, action, sequence=None, cancel=None):
        if cancel and cancel.is_set(): raise RuntimeRejected('CANCELLED')
        self.calls.append(action)
        if action == 'arm': self.phase = 'ARMED'
        if action == 'stop':
            if self.fail_stop: raise RuntimeRejected('STOP_UNCONFIRMED')
            self.phase = 'STOP_CONFIRMED'
        return {'runID': 'run-fresh', 'state': {'phase': self.phase, 'binding': 'c'*64}}
    def watcher(self, r): return FakeWatcher()
    def grant_patch(self, *args, **kwargs):
        super().grant_patch(*args, **kwargs)
        if kwargs.get('activate') and self.cancel_on_activation: self.cancel_on_activation.set()


class RuntimeBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.plan = fixture(Path(self.temp.name)); self.api = FakeAPI(self.plan); self.adapter = FakeAdapter(self.api)
    def test_preparation_only_reads_and_rechecks_current_baseline(self):
        self.adapter.prepare(self.plan)
        self.assertEqual(self.api.mutations, []); self.assertEqual(self.api.faults, 0); self.assertEqual(self.adapter.calls, [])
        self.api.policy['metadata']['uid'] = 'replaced'
        with self.assertRaisesRegex(RuntimeRejected, 'IDENTITY'): self.adapter.prepare(self.plan)
    def test_expired_changed_binding_and_policy_bypass_rejected(self):
        for mutate in (lambda p: p.update(expiresAt=iso(-1)), lambda p: p['binding'].update(epoch='old'), lambda p: p['runtime']['policyBefore']['spec'].update(enabled=False), lambda p: p['runtime'].update(faultSeconds=601), lambda p: p['runtime']['executionGrant'].update(notAfter=iso(1000))):
            p = copy.deepcopy(self.plan); mutate(p)
            with self.subTest(mutate=mutate), self.assertRaises(RuntimeRejected): self.adapter.settings(p)
    def test_cancel_before_arm_launches_no_mutation(self):
        cancel = threading.Event(); cancel.set()
        result = self.adapter.run(self.plan, lambda *x: None, cancel)
        self.assertEqual(result['state'], 'STOPPED'); self.assertTrue(result['stopConfirmed'])
        self.assertEqual(self.adapter.calls, []); self.assertFalse(self.api.mutations); self.assertEqual(self.api.faults, 0)
    def test_cancel_after_activation_blocks_fault_and_finishes(self):
        cancel = threading.Event(); self.adapter.cancel_on_activation = cancel
        result = self.adapter.run(self.plan, lambda *x: None, cancel)
        self.assertEqual(result['state'], 'STOPPED'); self.assertEqual(self.api.faults, 0)
        self.assertIn('stop', self.adapter.calls); self.assertEqual(len(self.api.mutations), 1)
    def test_lost_stop_keeps_run_blocked(self):
        self.adapter.fail_stop = True
        cancel = threading.Event(); self.adapter.cancel_on_activation = cancel
        result = self.adapter.run(self.plan, lambda *x: None, cancel)
        self.assertEqual(result['state'], 'UNCONFIRMED_TERMINATION'); self.assertFalse(result['stopConfirmed'])
    def test_real_schema_shaped_recovery_events_no_private_fields(self):
        events = []
        result = self.adapter.run(self.plan, lambda actor, kind, data: events.append((actor, kind, data)), threading.Event())
        self.assertEqual(result['state'], 'COMPLETED'); self.assertTrue(result['stopConfirmed']); self.assertEqual(self.api.faults, 1)
        kinds = [x[1] for x in events]
        self.assertIn('SERVICE_RECOVERED', kinds); self.assertIn('CANDIDATES', kinds); self.assertIn('ENDPOINT', kinds)
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(events))
        self.assertEqual(self.adapter.calls.count('arm'), 1); self.assertEqual(self.adapter.calls.count('stop'), 1)
    def test_observed_incident_cannot_be_replaced(self):
        self.api.recovered = True; self.api.policy['status']['activeIncident'] = {'name': self.plan['runtime']['executionGrant']['scope']['incident'], 'incidentUID': self.api.incident_uid}
        seen = {}; self.adapter.observe(self.plan, self.api, lambda *x: None, seen)
        self.api.incident_uid = 'another-incident'
        with self.assertRaisesRegex(RuntimeRejected, 'INCIDENT_UID'): self.adapter.observe(self.plan, self.api, lambda *x: None, seen)
    def test_pin_changed_or_symlink_rejected(self):
        ref = self.plan['runtime']['driver']; Path(ref['path']).write_bytes(b'changed')
        with self.assertRaisesRegex(RuntimeRejected, 'PIN_CHANGED'): pinned(ref)
        target = Path(ref['path']); target.unlink(); target.symlink_to('/etc/passwd')
        with self.assertRaisesRegex(RuntimeRejected, 'PIN_INVALID'): pinned(ref)
    def test_static_credentials_reject_exec_auth_before_process(self):
        r = self.plan['runtime']; k = {'current-context': 'ctx', 'contexts': [{'name': 'ctx', 'context': {'cluster': 'cluster', 'user': 'user'}}], 'clusters': [{'name': 'cluster', 'cluster': {'server': r['apiOrigin'], 'certificate-authority-data': 'AA=='}}], 'users': [{'name': 'user', 'user': {'exec': {'command': 'credential-exfiltrator'}}}]}
        raw = canonical(k); Path(r['kubeconfig']['path']).write_bytes(raw); r['kubeconfig']['sha256'] = hashlib.sha256(raw).hexdigest()
        with patch('subprocess.Popen') as popen, self.assertRaisesRegex(RuntimeRejected, 'STATIC_AUTH'):
            Transport(r)
        popen.assert_not_called()
    def test_policy_stop_alone_does_not_release_remaining_fault(self):
        self.api.recovered = False
        self.assertFalse(self.adapter.settle_fault(self.plan, self.api, None))
        self.adapter.monotonic = lambda: 31
        self.assertTrue(self.adapter.settle_fault(self.plan, self.api, 0))
        original = self.api.request
        def unready(path, patch=None, absent=False):
            value = original(path, patch, absent)
            if path == self.api.paths['pod']: value['status']['conditions'][0]['status'] = 'False'
            return value
        self.api.request = unready
        moment = [31.]
        self.adapter.monotonic = lambda: moment[0]
        self.adapter.sleep = lambda seconds: moment.__setitem__(0, moment[0] + seconds)
        self.assertFalse(self.adapter.settle_fault(self.plan, self.api, 0))

    def test_cancelled_fault_waits_read_only_for_actual_ready_then_stops(self):
        clock = [0.]
        self.adapter.clock = lambda: NOW + clock[0]
        self.adapter.monotonic = lambda: clock[0]
        self.adapter.sleep = lambda seconds: clock.__setitem__(0, clock[0] + seconds)
        cancel = threading.Event(); events = []
        def fault(_):
            self.api.faults += 1
            return {'podUID': 'pod-old', 'durationSeconds': 30}
        self.api.fault = fault
        original = self.api.request
        def request(path, patch=None, absent=False):
            value = original(path, patch, absent)
            if path == self.api.paths['pod'] and absent:
                value['status']['conditions'][0]['status'] = 'True' if clock[0] >= 32 else 'False'
            return value
        self.api.request = request
        def emit(actor, kind, data):
            events.append((actor, kind, data))
            if kind == 'READINESS_FAULT': cancel.set()
        result = self.adapter.run(self.plan, emit, cancel)
        self.assertEqual(result['state'], 'STOPPED'); self.assertTrue(result['stopConfirmed'])
        self.assertGreaterEqual(clock[0], 32); self.assertLess(clock[0], 35)
        self.assertEqual(self.api.faults, 1); self.assertEqual(len(self.api.mutations), 1)
        self.assertNotIn('heartbeat', self.adapter.calls)
        kinds = [e[1] for e in events]
        self.assertEqual(kinds.count('CLEANUP_PENDING'), 1)
        self.assertLess(kinds.index('CLEANUP_PENDING'), kinds.index('FAULT_SETTLED'))

    def test_cleanup_read_horizon_expires_without_releasing_run(self):
        clock = [0.]
        self.adapter.clock = lambda: NOW + clock[0]
        self.adapter.monotonic = lambda: clock[0]
        self.adapter.sleep = lambda seconds: clock.__setitem__(0, clock[0] + seconds)
        cancel = threading.Event(); events = []
        def fault(_):
            self.api.faults += 1
            return {'podUID': 'pod-old', 'durationSeconds': 30}
        self.api.fault = fault
        def emit(actor, kind, data):
            events.append(kind)
            if kind == 'READINESS_FAULT':
                # Simulate a cancelled run near its original absolute horizon.
                cancel.set(); clock[0] = 545
        result = self.adapter.run(self.plan, emit, cancel)
        self.assertEqual(result['state'], 'UNCONFIRMED_TERMINATION')
        self.assertFalse(result['stopConfirmed']); self.assertNotIn('FAULT_SETTLED', events)
        self.assertEqual(self.api.faults, 1); self.assertEqual(len(self.api.mutations), 1)

    def test_cleanup_uses_earliest_guard_or_plan_read_horizon(self):
        for horizon in ('guard', 'plan'):
            clock = [0.]
            self.adapter.clock = lambda: NOW + clock[0]
            self.adapter.monotonic = lambda: clock[0]
            self.adapter.sleep = lambda seconds: clock.__setitem__(0, clock[0] + seconds)
            plan = copy.deepcopy(self.plan)
            if horizon == 'plan': plan['expiresAt'] = iso(2)
            else:
                ref = plan['runtime']['guardConfig']; guard = json.loads(Path(ref['path']).read_bytes())
                guard['authorityExpires'] = iso(2)
                path = Path(self.temp.name) / 'short-guard.json'; raw = canonical(guard); path.write_bytes(raw)
                plan['runtime']['guardConfig'] = {'path': str(path), 'sha256': hashlib.sha256(raw).hexdigest()}
            self.assertFalse(self.adapter.settle_fault(plan, self.api, 0))
            self.assertGreaterEqual(clock[0], 2); self.assertLess(clock[0], 2.11)

    def test_coordinator_runtime_event_contract_and_durable_reconnect(self):
        from tools.exhibition_control import Coordinator
        from tests.test_exhibition_control import SyntheticLifecycle
        root = Path(self.temp.name)
        path = root / 'plan.json'; raw = canonical(self.plan); path.write_bytes(raw)
        config = {'schema': 'opsloop-exhibition-control/v1', 'plans': [{'path': str(path), 'sha256': hashlib.sha256(raw).hexdigest()}]}
        with patch('tools.exhibition_control.time.time', return_value=NOW):
            control = Coordinator(root / 'control', config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
            try:
                control.prepare('plan')
                run = control.start('plan', 'request-one')
                control.worker.join(timeout=10)
                self.assertFalse(control.worker.is_alive())
                state = control.state()['runs'][0]
                self.assertEqual(state['state'], 'COMPLETED')
                events = control.events(run['runId'])['events']
                self.assertIn('SERVICE_RECOVERED', [e['kind'] for e in events])
                self.assertIn('workload', [e['actor'] for e in events])
                self.assertEqual([e['seq'] for e in events], list(range(1, len(events)+1)))
                self.assertNotIn('PRIVATE_SENTINEL', json.dumps(events))
                self.assertEqual(control.start('plan', 'request-one')['runId'], run['runId'])
                self.assertEqual(self.api.faults, 1)
            finally: control.close()
            restored = Coordinator(root / 'control', config, {'runtime': self.adapter}, lifecycle=SyntheticLifecycle())
            try:
                self.assertEqual(restored.state()['runs'][0]['state'], 'COMPLETED')
                self.assertEqual(restored.events(run['runId'])['events'], events)
            finally: restored.close()

    def test_resource_condition_expiry_node_and_input_changes_rejected(self):
        root = Path(self.temp.name)
        condition = {'schema': 'opsloop-resource-conditions/v1', **self.plan['binding'], 'notBefore': iso(-1), 'expiresAt': iso(10), 'nodes': [{'nodeName': 'node-a', 'nodeUID': 'node-uid', 'cpuUtilizationFloorPermille': 950}]}
        path = root / 'condition.json'
        def set_condition(value):
            raw = canonical(value); path.write_bytes(raw)
            self.plan['runtime']['resourceConditions'] = {'path': str(path), 'sha256': hashlib.sha256(raw).hexdigest()}
        set_condition(condition); self.adapter.authority_now(self.plan)
        for mutate in (lambda c: c.update(expiresAt=iso(-1)), lambda c: c.update(epoch='old'), lambda c: c['nodes'][0].update(nodeUID='old'), lambda c: c['nodes'][0].update(cpuUtilizationFloorPermille=-1)):
            changed = copy.deepcopy(condition); mutate(changed); set_condition(changed)
            with self.assertRaises(RuntimeRejected): self.adapter.authority_now(self.plan)
        set_condition(condition); path.write_bytes(b'{}')
        with self.assertRaisesRegex(RuntimeRejected, 'PIN_CHANGED'): self.adapter.authority_now(self.plan)

    def test_authority_expiring_after_activation_prevents_fault(self):
        original = self.adapter.grant_patch
        def activate(*args, **kwargs):
            original(*args, **kwargs)
            if kwargs.get('activate'): self.adapter.clock = lambda: NOW + 401
        self.adapter.grant_patch = activate
        result = self.adapter.run(self.plan, lambda *x: None, threading.Event())
        self.assertEqual(result['state'], 'FAILED'); self.assertEqual(self.api.faults, 0)
        self.assertIn('stop', self.adapter.calls)

    def test_foreign_recovery_owner_is_not_success(self):
        self.api.recovered = True
        self.api.policy['status']['activeIncident'] = {'name': self.plan['runtime']['executionGrant']['scope']['incident'], 'incidentUID': self.api.incident_uid}
        original = self.api.request
        def request(path, patch=None, absent=False):
            result = original(path, patch, absent)
            if '/replicasets/' in path: result['metadata']['ownerReferences'][0]['uid'] = 'foreign-deployment'
            return result
        self.api.request = request
        with self.assertRaisesRegex(RuntimeRejected, 'RECOVERY_DEPLOYMENT_OWNER'): self.adapter.observe(self.plan, self.api, lambda *x: None, {})

    def test_fault_invokes_existing_image_entrypoint_and_exact_bound_uid(self):
        transport = Transport.__new__(Transport)
        transport.settings = self.plan['runtime']; transport.kubeconfig = Path(self.plan['runtime']['kubeconfig']['path'])
        with patch('tools.exhibition_runtime.process_json', return_value={'status': 'fault-active', 'podUID': 'pod-old', 'namespace': 'demo-a'}) as command:
            receipt = transport.fault(threading.Event())
        argv = command.call_args.args[0]
        self.assertEqual(argv[-6:], ['/app', 'inject-readiness-fault', '--pod-uid', 'pod-old', '--seconds', '30'])
        self.assertEqual(receipt['podUID'], 'pod-old')
        self.assertNotIn('token', ' '.join(argv).lower())

    def test_pending_subprocess_cancel_terminates_process_group_promptly(self):
        cancel = threading.Event()
        timer = threading.Timer(.1, cancel.set); timer.start()
        start = time.monotonic()
        try:
            with self.assertRaisesRegex(RuntimeRejected, 'CANCELLED_DISPATCH'):
                process_json(['/usr/bin/python3', '-c', 'import time; time.sleep(20)'], 7, 4096, cancel)
        finally: timer.join()
        self.assertLess(time.monotonic() - start, .5)
    def test_pre_cancel_never_launches_subprocess(self):
        cancel = threading.Event(); cancel.set()
        with patch('subprocess.Popen') as popen, self.assertRaisesRegex(RuntimeRejected, 'CANCELLED'):
            process_json(['/no/command'], 7, 4096, cancel)
        popen.assert_not_called()


if __name__ == '__main__': unittest.main()
