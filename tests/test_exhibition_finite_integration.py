"""Synthetic transports around real finite CLI/session/Coordinator/Lifecycle.

No cluster, model, operational key, or Publisher is used. The child adapter
models Runtime/Engineering termination; it is NOT evidence of live recovery or
of a model invocation. Issuance, child ownership, durable Coordinator state,
baseline restore, archive, retirement and predecessor validation remain real.
"""
import copy
from contextlib import closing, redirect_stdout
import datetime as dt
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch


def synthetic_child(state_file, args):
    """Child-only transport injection, never installed in an operator entrypoint."""
    from tests.test_exhibition_lifecycle import API
    from tools import exhibition_session as session
    from tools.exhibition_control import Coordinator
    from tools.exhibition_hashes import private_read
    from tools.exhibition_inputs import new
    from tools.exhibition_lifecycle import Lifecycle, RUN, EPOCH
    from tools.exhibition_handoff import preflight

    file = Path(state_file)
    shared = json.loads(file.read_bytes())
    objects = shared['objects']
    pol, dep = objects['b-policy'], objects['b-deployment']
    mode = shared.get('mode', 'success')
    calls = shared.setdefault('childCalls', [])

    class Transport(API):
        def close(self):
            shared['deleted'] = self.deleted
            shared['patches'] = self.patch_calls
            file.write_text(json.dumps(shared))

    api = Transport(pol, dep, {})

    class SyntheticAdapter:
        def prepare(self, plan):
            preflight(plan)  # actual freshly materialized plan and signed permit

        def run(self, plan, emit, cancel):
            b = plan['binding']
            calls.append({'runID': b['runID'], 'pid': os.getpid(),
                          'kind': 'SYNTHETIC_RUNTIME_ENGINEERING', 'modelCalls': 0})
            cfg = private_read(plan['lifecycle']['path'], plan['lifecycle']['sha256'])
            root = Path(cfg['evidenceRoot'])
            self.evidence = new(root, 'synthetic-boundary-evidence.json', {
                'binding': b, 'synthetic': True, 'runtimeActionIntents': 1,
                'runtimeTerminal': 'FAILED/RUNTIME_BUDGET_EXHAUSTED',
                'postActionDefect': 'synthetic-only', 'stop': 'STOP_CONFIRMED',
                'engineering': 'SYNTHETIC_VERIFIED_PENDING_REVIEW',
                'realModelCalls': 0, 'deployed': False})
            pol['metadata']['generation'] += 3
            pol['status']['incidentSequence'] += 1
            pol['status']['conditions'] = [{'type': 'Quiesced', 'status': 'True',
                'reason': 'FenceClosed', 'observedGeneration': pol['metadata']['generation']}]
            pol['metadata'].setdefault('annotations', {})['opsloop.io/exhibition-permit'] = Path(plan['exhibitionPermit']['path']).read_text()
            dep['spec']['template']['spec']['containers'][0]['env'][5]['value'] = 'empty-order-after-30s'
            obj = {'apiVersion': 'opsloop.io/v1alpha1', 'kind': 'RecoveryIncident',
                'metadata': {'name': b['runID'], 'namespace': cfg['namespace'],
                    'uid': b['runID']+'-incident', 'resourceVersion': '1',
                    'annotations': {RUN: b['runID'], EPOCH: b['epoch']}},
                'spec': {'policyRef': {'uid': b['policyUID']}, 'workload': {'uid': b['deploymentUID']}},
                'status': {'runtime': {'phase': 'FAILED', 'reason': 'RUNTIME_BUDGET_EXHAUSTED',
                    'completedAt': dt.datetime.now(dt.timezone.utc).isoformat()}}}
            api.objects[b['runID']] = obj
            self.inventory = [{'kind': 'RecoveryIncident', **{k: obj['metadata'][k] for k in ('name', 'uid', 'resourceVersion')}}]
            emit('observer', 'SYNTHETIC_TERMINAL', {'runID': b['runID'], 'status': 'LOCAL_SYNTHETIC'})
            if mode == 'stop-unconfirmed':
                return {'state': 'UNCONFIRMED_TERMINATION', 'stopConfirmed': False}
            return {'state': 'COMPLETED', 'stopConfirmed': True}

        def reconcile(self, plan, emit):
            if mode == 'baseline-drift':
                dep['spec']['unexpected'] = 'unapproved'
            return {'binding': plan['binding'], 'stopConfirmed': True,
                'modelStopped': mode != 'model-unconfirmed',
                'evidence': [self.evidence], 'objects': self.inventory}

    def coordinator(state, cfg, adapters):
        state.mkdir(mode=0o700, parents=True, exist_ok=True)
        return Coordinator(state, cfg, adapters, lifecycle=Lifecycle(Path(state)/'lifecycle', lambda cfg: api))

    sys.argv = ['synthetic-session-child', *args]
    try:
        with patch.object(session, 'EngineeringAdapter', SyntheticAdapter), patch.object(session, 'Coordinator', coordinator):
            session.main()
    finally:
        api.close()


def child_transport(real_run, state_file, dispatches):
    """Keep real subprocess ownership while injecting only child transports."""
    def run(args, **kwargs):
        if len(args) > 2 and args[1:3] == ['-m', 'tools.exhibition_session']:
            dispatches.append(copy.deepcopy(args))
            return real_run([sys.executable, '-B', '-c',
                'import sys; from tests.test_exhibition_finite_integration import synthetic_child; synthetic_child(sys.argv[1], sys.argv[2:])',
                str(state_file), *args[3:]], **kwargs)
        return real_run(args, **kwargs)
    return run


class FiniteChildIntegrationTests(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.configure(Path(self.tmp.name))

    def configure(self, root, policy_hash='a'*64):
        from tests.test_exhibition_finite_cli import CLIFixture
        self.f = CLIFixture(root, policy_hash)
        for key, target in self.f.plan['fixedTargets'].items():
            self.f.objects[key+'-policy']['metadata'].update(name=target['policy'], namespace=target['namespace'], resourceVersion='1')
            self.f.objects[key+'-deployment']['metadata']['resourceVersion'] = '1'
        self.state = self.f.root/'synthetic-api-state.json'
        self.dispatches = []
        self.results = []

    def invoke(self, mode='success', close=False):
        from tools import exhibition_finite_cli as cli, exhibition_resume as resume
        from tools.exhibition_inputs import pin
        self.state.write_text(json.dumps({'objects': self.f.objects, 'mode': mode}))
        self.state.chmod(0o600)
        plan = pin(self.f.put('cli-input-'+str(len(self.dispatches))+('-close' if close else ''), self.f.plan))
        outer = self
        class Reader:
            def __init__(self, *a): pass
            def snapshot(self): return json.loads(outer.state.read_bytes())['objects']
        args = ['exhibition_resume', '--plan', plan['path'], '--sha256', plan['sha256']]
        args += ['--finite-close'] if close else ['--finite-slot', self.f.intent['authorizationID']]
        original = subprocess.run
        with patch.object(cli, 'Reader', Reader), patch('subprocess.run', side_effect=child_transport(original, self.state, self.dispatches)), patch.object(sys, 'argv', args):
            try:
                output = io.StringIO()
                with redirect_stdout(output):
                    resume.main()  # actual argparse and exact pinned-plan dispatch
                self.results.append(json.loads(output.getvalue()))
            finally:
                self.f.objects = json.loads(self.state.read_bytes())['objects']

    def test_cli_two_rounds_real_child_coordinator_lifecycle(self):
        from tools.exhibition_inputs import pin
        from tools.exhibition_lifecycle import verify_completion
        import sqlite3
        initial_controller = copy.deepcopy(self.f.objects['controller'])
        initial_history = copy.deepcopy(self.f.objects['b-policy']['status']['circuits'])
        initial_sequence = self.f.objects['b-policy']['status']['incidentSequence']
        completions = []
        for index in (0, 1):
            if index:
                self.f.predecessor = completions[-1]
                self.f.select(index)
            self.invoke()
            run = self.f.intent['scope']['runID']
            completion = pin(self.f.root/'control/lifecycle'/run/'completion.json')
            done = verify_completion(completion['path'], completion['sha256'])
            self.assertTrue(all(done[k] for k in ('stopConfirmed', 'modelStopped', 'baselineRestored', 'evidenceArchived', 'objectsCleaned', 'nextRunAllowed')))
            completions.append(completion)
            self.assertEqual(self.f.objects['controller'], initial_controller)
            self.assertEqual(self.f.objects['b-policy']['status']['circuits'], initial_history)
            self.assertEqual(self.f.objects['b-policy']['status']['incidentSequence'], initial_sequence+index+1)
            self.assertNotIn('opsloop.io/exhibition-permit', self.f.objects['b-policy']['metadata']['annotations'])
            readback = json.loads(self.state.read_bytes())
            self.assertEqual(len(readback['childCalls']), 1)
            self.assertNotEqual(readback['childCalls'][0]['pid'], os.getpid())
            self.assertEqual(readback['childCalls'][0]['modelCalls'], 0)
            self.assertEqual(len(readback['deleted']), 1)
        self.assertNotEqual(completions[0]['sha256'], completions[1]['sha256'])
        with closing(sqlite3.connect(self.f.root/'issuer/finite-cli.sqlite3')) as db:
            self.assertEqual(db.execute('SELECT ordinal,state FROM slots ORDER BY ordinal').fetchall(), [(5, 'COMPLETED'), (6, 'COMPLETED')])
        with closing(sqlite3.connect(self.f.root/'control/ledger.sqlite3')) as db:
            runs = [json.loads(r[0]) for r in db.execute('SELECT value FROM runs')]
            self.assertEqual(len(runs), 2)
            self.assertTrue(all(r['lifecycleComplete'] and r['state']=='COMPLETED' for r in runs))
        self.assertEqual(len(self.dispatches), 2)
        self.invoke(close=True)
        self.assertEqual(len(self.dispatches), 2)
        self.assertEqual(self.f.objects['controller'], initial_controller)
        self.assertEqual([r['state'] for r in self.results], ['FINITE_SLOT_COMPLETE', 'FINITE_SLOT_COMPLETE', 'FINITE_SESSION_CLOSED_CONTROLLER_UNCHANGED'])
        self.assertTrue(all(r['nextRunIsAuthorized'] is False for r in self.results))

    def test_unconfirmed_stop_blocks_completion_restart_and_next(self):
        from tools import exhibition_finite_cli as cli
        with self.assertRaisesRegex(ValueError, 'FINITE_RUN_OR_CLEANUP_UNCONFIRMED'):
            self.invoke('stop-unconfirmed')
        self.assertEqual(len(self.dispatches), 1)
        self.assertFalse(list((self.f.root/'control/lifecycle').glob('*/completion.json')))
        with patch.object(cli, 'Reader', side_effect=AssertionError('no observation after uncertain claim')):
            with self.assertRaisesRegex(ValueError, 'FINITE_PRIOR_DISPATCH_UNCONFIRMED'):
                cli.execute(self.f.plan, 'synthetic', authorization_id=self.f.intent['authorizationID'])
        self.assertEqual(len(self.dispatches), 1)

    def test_partial_restore_does_not_unlock_next_round(self):
        with self.assertRaisesRegex(ValueError, 'ROUND_CHILD_RESULT_UNCONFIRMED'):
            self.invoke('baseline-drift')
        self.assertEqual(len(self.dispatches), 1)
        self.assertFalse(list((self.f.root/'control/lifecycle').glob('*/completion.json')))
        self.assertEqual(json.loads(self.state.read_bytes())['deleted'], [])


def export_cli_controller_fixture(root, policy_hash):
    """Feed permits emitted by the top CLI/real child chain to Go authority."""
    case = FiniteChildIntegrationTests('test_cli_two_rounds_real_child_coordinator_lifecycle')
    case.configure(root, policy_hash)
    case.test_cli_two_rounds_real_child_coordinator_lifecycle()
    result = {'public': case.f.public, 'rounds': []}
    for slot in case.f.bounded['rounds']:
        runroot = root/'session'/slot['scope']['runID']
        result['rounds'].append({
            'permit': json.loads((runroot/'permit-payload.json').read_bytes()),
            'envelope': json.loads((runroot/'permit.json').read_bytes()),
            'deployment': json.loads((runroot/'runtime-snapshot.json').read_bytes())['objects']['b-deployment']})
    return result


if __name__ == '__main__':
    unittest.main()
