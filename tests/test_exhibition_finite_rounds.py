"""Synthetic finite same-instance reproductions; no operational approvals or APIs.

The host approval files here are fabricated test inputs, not operator consent.
"""
import concurrent.futures
from contextlib import closing
import copy
import datetime as dt
import json
import sqlite3
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from tools.exhibition_control import ControlError, canonical
from tools.exhibition_inputs import pin
from tools.exhibition_permit import issue, verify
from tools.exhibition_replay import pretransition_replay, projection_hash
from tests.test_exhibition_reproduction import V3Fixture


class FiniteFixture(V3Fixture):
    def approve(self):
        # Keep all historical host pins immutable, including the inherited B2.
        self.intent_count = getattr(self, 'intent_count', 0)+1
        self.ref = pin(self.put('synthetic-intent-'+str(self.intent_count), self.intent))
        self.plan['executionInputs']['circuitReplayApproval'] = {'b':self.ref}
        return self.ref

    def __init__(self, root, policy_hash='a'*64):
        super().__init__(root, policy_hash)
        self.intent['schema'] = 'opsloop-circuit-replay-intent/v4'
        # Preserve the already-issued B2's real fixture receipt and instance.
        self.receipt = json.loads((root/'controller-receipt.json').read_text())
        self.intent['controller'] = {k:self.receipt[k] for k in ('deploymentUID','imageID','instance','podUID')}
        # The new boundary binds current identity, independent of an old rollout.
        self.receipt['priorInstance'] = self.receipt['instance']
        self.receipt['priorPodUID'] = self.receipt['podUID']
        predecessor = json.loads(Path(self.predecessor['path']).read_text())['binding']
        slots = []
        for index in range(2):
            scope = copy.deepcopy(self.intent['scope'])
            scope.update(runID='synthetic-finite-run-'+str(index+1), epoch='synthetic-finite-epoch-'+str(index+1),
                         policyGeneration=scope['policyGeneration']+index*3)
            sequence = self.intent['baseIncidentSequence']+index
            scope['incident'] = scope['policy']+'-'+scope['policyUID'][:8]+'-'+str(sequence+1).zfill(5)
            slots.append({'authorizationID':'synthetic-finite-authorization-'+str(index+1), 'ordinal':5+index,
                          'baseIncidentSequence':sequence, 'scope':scope,
                          'predecessor':{k:predecessor[k] for k in ('runID','epoch')}})
            predecessor = scope
        self.bounded = {'schema':'opsloop-finite-round-approval/v1', 'approved':True,
                        'purpose':'FINITE_SAME_INSTANCE_REPRODUCTION', 'controller':copy.deepcopy(self.intent['controller']),
                        'issuerLedger':str(root/'issuer'), 'notBefore':self.intent['notBefore'], 'notAfter':self.intent['notAfter'],
                        'workloadImage':self.image, 'approvedReproductionSHA256':projection_hash(self.intent['approvedReproduction']),
                        'maxIncidents':1, 'maxActions':1,
                        'modelBudget':{'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt','maxModelCalls':1,'totalSeconds':900},
                        'rounds':slots}
        self.bound_ref = pin(self.put('synthetic-finite-approval', self.bounded))
        self.select(0)

    def select(self, index):
        slot = self.bounded['rounds'][index]
        self.intent.update({k:copy.deepcopy(slot[k]) for k in ('authorizationID','ordinal','baseIncidentSequence','scope')})
        self.intent['finiteRoundsApproval'] = self.bound_ref
        self.intent['predecessorCompletion'] = self.predecessor
        self.plan['newInterval']['continuation']['predecessorCompletion'] = self.predecessor
        self.args.update(run_id=slot['scope']['runID'], epoch=slot['scope']['epoch'], authorization_id=slot['authorizationID'],
                         ordinal=slot['ordinal'], previous_completion_sha=self.predecessor['sha256'])
        self.approve()

    def complete(self, payload, **overrides):
        """Explicitly synthetic safety completion and terminal retirement."""
        binding = {k:payload['grant']['scope'][k] for k in ('runID','epoch','policyUID','deploymentUID')}
        path = self.chain.completion(binding)
        done = json.loads(path.read_text())
        authority = path.parent/'authority-retirement.json'
        authority.write_text(canonical({'state':'RETIRED','binding':binding}))
        done['retiredAuthoritySHA256'] = pin(authority)['sha256']
        done.update(overrides)
        path.write_text(canonical(done))
        self.predecessor = pin(path)
        pol = self.objects['b-policy']
        pol['metadata']['generation'] += 3
        pol['status']['conditions'][0]['observedGeneration'] = pol['metadata']['generation']
        pol['status']['incidentSequence'] += 1

    def request(self, payload, label):
        pref = pin(self.put(label+'-payload', payload))
        approval = {'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],
                    'authorizationID':payload['authorizationID'],'expiresAt':payload['grant']['notAfter'],
                    'previousCompletion':self.predecessor['path'],'previousCompletionSHA256':self.predecessor['sha256'],
                    'circuitReplayApproval':self.ref,'controllerReceipt':pin(self.put(label+'-controller',self.receipt))}
        ref = pin(self.put(label+'-issuance-approval', approval))
        return (pref['path'],pref['sha256'],ref['path'],ref['sha256'],self.key,self.root/'issuer',self.root/(label+'-issued.json'))

    def mint(self, payload, label):
        request = self.request(payload, label)
        issue(*request)
        return json.loads(request[-1].read_text())


def export_controller_fixture(root, policy_hash):
    """Only temporary-key synthetic signed permits; exercised by the Go test."""
    f = FiniteFixture(root, policy_hash)
    result = {'public':f.public, 'rounds':[]}
    for index in range(2):
        f.select(index)
        p = f.payload()
        envelope = f.mint(p, 'synthetic-round-'+str(index+1))
        result['rounds'].append({'permit':p,'envelope':envelope,'deployment':copy.deepcopy(f.objects['b-deployment'])})
        f.complete(p)
    return result


class FiniteRoundTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.f = FiniteFixture(Path(self.tmp.name))

    def test_two_exact_slots_same_controller_policy_and_ledger(self):
        f = self.f
        with closing(sqlite3.connect(f.root/'issuer/issuance.sqlite3')) as db:
            historical = db.execute('SELECT * FROM permits ORDER BY rowid').fetchall()
        first = f.payload()
        pretransition_replay(f.plan,'b',f.objects,{'instance':f.receipt['instance'],'podUID':f.receipt['podUID']},f.args['policy_hash'])
        self.assertEqual(verify(f.mint(first,'first'),f.public),first)
        f.complete(first)
        f.select(1)
        second = f.payload()
        self.assertEqual(verify(f.mint(second,'second'),f.public),second)
        self.assertEqual(f.b2['grant']['instance'],first['grant']['instance'])
        self.assertEqual(first['grant']['instance'],second['grant']['instance'])
        self.assertEqual(first['grant']['scope']['policyUID'],second['grant']['scope']['policyUID'])
        self.assertEqual(second['previousCompletionSHA256'],f.predecessor['sha256'])
        self.assertEqual((first['ordinal'],second['ordinal']),(5,6))
        self.assertEqual(second['baseIncidentSequence'],first['baseIncidentSequence']+1)
        with closing(sqlite3.connect(f.root/'issuer/issuance.sqlite3')) as db:
            self.assertEqual(db.execute('SELECT * FROM permits ORDER BY rowid LIMIT 2').fetchall(),historical)
            self.assertEqual(db.execute('SELECT COUNT(*) FROM finite_rounds').fetchone()[0],2)
            self.assertEqual(db.execute("SELECT COUNT(*) FROM permits WHERE state='ISSUED'").fetchone()[0],4)
        f.complete(second)
        f.intent['authorizationID'] = 'synthetic-unlisted-third'
        f.args['authorization_id'] = f.intent['authorizationID']
        f.approve()
        with self.assertRaisesRegex(ControlError,'ALLOWLIST_EXHAUSTED_OR_UNKNOWN'):
            f.payload()

    def test_legacy_approvals_do_not_expand(self):
        f = self.f
        f.intent.pop('finiteRoundsApproval')
        f.intent['schema'] = 'opsloop-circuit-replay-intent/v3'
        f.intent['controller'] = {k:f.receipt[k] for k in ('deploymentUID','imageID','priorInstance','priorPodUID')}
        f.approve()
        with self.assertRaisesRegex(ControlError,'REPLAY_ONE_B2_ONLY'):
            f.payload()
        f.plan['executionInputs'].pop('circuitReplayApproval')
        with self.assertRaisesRegex(ValueError,'SESSION_ORDER_CHANGED'):
            f.payload()

    def test_each_slot_scope_identity_and_model_are_exact(self):
        f = self.f
        original = copy.deepcopy(f.intent)
        for field in ('policyUID','deploymentUID','policyGeneration','policyHash','incident','runID','epoch'):
            f.intent = copy.deepcopy(original)
            f.intent['scope'][field] = 999 if field == 'policyGeneration' else 'changed'
            f.approve()
            with self.subTest(field=field),self.assertRaises(ControlError):
                f.payload()
        f.intent = original; f.approve()
        from tools.exhibition_replay import approved_replay
        p = f.payload()
        for field,value in [('maxModelCalls',2),('totalSeconds',899),('expiresAt',(f.now+dt.timedelta(seconds=899)).isoformat())]:
            bad = copy.deepcopy(p); bad['modelBudget'][field] = value
            with self.subTest(model=field),self.assertRaises(ControlError):
                approved_replay(f.ref,bad,f.image,f.receipt)

    def test_current_instance_pod_image_and_target_cannot_change(self):
        f = self.f; original = copy.deepcopy(f.receipt)
        for field in ('instance','podUID','deploymentUID','imageID'):
            f.receipt = copy.deepcopy(original); f.receipt[field] = 'changed'
            with self.subTest(field=field),self.assertRaises((ControlError,ValueError)):
                f.payload()
        f.receipt = original
        f.objects['b-deployment']['metadata']['uid'] = 'changed'
        with self.assertRaises((ControlError,ValueError)):
            f.payload()

    def test_no_fence_or_baseline_means_no_new_input(self):
        f = self.f; original = copy.deepcopy(f.objects)
        for mode in ('running','fence','restore'):
            f.objects = copy.deepcopy(original)
            if mode == 'running': f.objects['b-policy']['spec']['enabled'] = True
            if mode == 'fence': f.objects['b-policy']['status']['conditions'] = []
            if mode == 'restore': f.objects['b-deployment']['status']['readyReplicas'] = 0
            with self.subTest(mode=mode),self.assertRaises((ControlError,ValueError)):
                f.payload()

    def test_duplicate_allowlist_identities_and_nonmonotone_order_rejected(self):
        f = self.f; original = copy.deepcopy(f.bounded)
        for field in ('authorizationID','ordinal','runID','epoch','baseIncidentSequence','policyGeneration'):
            bad = copy.deepcopy(original)
            if field in ('runID','epoch','policyGeneration'):
                bad['rounds'][1]['scope'][field] = bad['rounds'][0]['scope'][field]
            else: bad['rounds'][1][field] = bad['rounds'][0][field]
            f.intent['finiteRoundsApproval'] = pin(f.put('bad-allowlist-'+field,bad)); f.approve()
            with self.subTest(field=field),self.assertRaises(ControlError):
                f.payload()

    def test_unknown_or_expired_or_unapproved_allowlist_rejected(self):
        f = self.f; original = copy.deepcopy(f.bounded)
        for mode in ('expiry','approval','empty','too-many','ledger','image','recipe'):
            bad = copy.deepcopy(original)
            if mode == 'expiry': bad['notAfter'] = '2000-01-01T00:00:00Z'
            if mode == 'approval': bad['approved'] = False
            if mode == 'empty': bad['rounds'] = []
            if mode == 'too-many': bad['rounds'].append(copy.deepcopy(bad['rounds'][-1]))
            if mode == 'ledger': bad['issuerLedger'] = str(f.root/'other-ledger')
            if mode == 'image': bad['workloadImage'] = 'other'
            if mode == 'recipe': bad['approvedReproductionSHA256'] = '0'*64
            f.intent['finiteRoundsApproval'] = pin(f.put('bad-allowlist-'+mode,bad)); f.approve()
            with self.subTest(mode=mode),self.assertRaises(ControlError):
                if mode == 'ledger': f.mint(f.payload(),'wrong-ledger')
                else: f.payload()

    def test_second_slot_requires_first_issued_and_completed(self):
        f = self.f; first = f.payload(); f.select(1)
        with self.assertRaisesRegex(ControlError,'FINITE_PREDECESSOR_CHANGED'):
            f.payload()
        # A fabricated safe receipt does not replace the issuer's actual history.
        f.complete(first); f.select(1); second = f.payload()
        with self.assertRaisesRegex(ControlError,'ISSUANCE_PREDECESSOR_NOT_COMPLETE'):
            f.mint(second,'skipped-first')

    def test_unconfirmed_completion_or_retirement_blocks_next_round(self):
        f = self.f; first = f.payload(); f.mint(first,'first'); f.complete(first)
        path = Path(f.predecessor['path']); original = json.loads(path.read_text())
        for field in ('nextRunAllowed','stopConfirmed','baselineRestored','modelStopped','evidenceArchived','objectsCleaned'):
            done = copy.deepcopy(original); done[field] = False; path.write_text(canonical(done)); f.predecessor = pin(path); f.select(1)
            with self.subTest(field=field),self.assertRaises(ControlError):
                f.payload()
        path.write_text(canonical(original)); f.predecessor = pin(path); f.select(1)
        retirement = path.parent/'authority-retirement.json'
        retirement.write_text(canonical({'state':'ISSUED','binding':original['binding']}))
        original['retiredAuthoritySHA256'] = pin(retirement)['sha256']; path.write_text(canonical(original)); f.predecessor = pin(path); f.select(1)
        with self.assertRaisesRegex(ControlError,'PREDECESSOR_AUTHORITY_NOT_RETIRED'):
            f.payload()

    def test_archive_corruption_blocks_next_round(self):
        f = self.f; first = f.payload(); f.mint(first,'first'); f.complete(first); f.select(1)
        done = json.loads(Path(f.predecessor['path']).read_text()); archive = Path(done['archiveManifest'])
        blob = json.loads(archive.read_text())['files'][0]['sha256']; (archive.parent/blob).write_text('corrupt')
        with self.assertRaisesRegex(ControlError,'ARCHIVE_INTEGRITY_FAILED'):
            f.payload()

    def test_concurrent_issuance_only_one_consumes_slot(self):
        f = self.f; p = f.payload(); requests = [f.request(p,'concurrent-'+str(i)) for i in range(2)]
        def attempt(request):
            try: issue(*request); return 'ISSUED'
            except ControlError: return 'REJECTED'
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(attempt,requests))
        self.assertEqual(sorted(results),['ISSUED','REJECTED'])
        with self.assertRaises(ControlError): f.mint(p,'retry-after-restart')

    def test_signing_and_durable_output_failures_burn_slot_and_block_restart(self):
        for failure in ('signing','file-fsync','directory-fsync','output-exists'):
            with self.subTest(failure=failure),tempfile.TemporaryDirectory() as directory:
                f = FiniteFixture(Path(directory)); first = f.payload(); request = f.request(first,'failed')
                real_run = subprocess.run
                def signing_fail(*args,**kwargs):
                    if '-sign' in args[0]: return subprocess.CompletedProcess(args[0],1,b'',b'synthetic failure')
                    return real_run(*args,**kwargs)
                if failure == 'output-exists': request[-1].write_text('preserve-existing-output')
                context = mock.patch('tools.exhibition_permit.subprocess.run',side_effect=signing_fail) if failure == 'signing' else (
                    mock.patch('tools.exhibition_permit.os.fsync',side_effect=[None,OSError('synthetic directory durability failure')]) if failure == 'directory-fsync' else (
                    mock.patch('tools.exhibition_permit.os.fsync',side_effect=OSError('synthetic file durability failure')) if failure == 'file-fsync' else mock.patch('tools.exhibition_permit.time.time',wraps=__import__('time').time)))
                with context,self.assertRaises((ControlError,OSError)):
                    issue(*request)
                with closing(sqlite3.connect(f.root/'issuer/issuance.sqlite3')) as db:
                    self.assertEqual(db.execute('SELECT state FROM permits WHERE authorization=?',(first['authorizationID'],)).fetchone()[0],'RESERVED_NO_RETRY')
                    self.assertEqual(db.execute('SELECT count(*) FROM finite_rounds').fetchone()[0],1)
                with self.assertRaises(ControlError): f.mint(first,'restart-retry')
                f.complete(first); f.select(1)
                with self.assertRaisesRegex(ControlError,'ISSUANCE_PREDECESSOR_NOT_COMPLETE'):
                    f.mint(f.payload(),'restart-next')

    def test_actual_session_accepts_both_v3_permits_with_synthetic_execution(self):
        from tools.exhibition_control import Coordinator
        from tools.exhibition_session import run_once
        from tests.test_exhibition_control import Adapter,SyntheticLifecycle
        f = self.f
        for index in range(2):
            f.select(index); p = f.payload(); label = 'session-'+str(index); f.mint(p,label)
            scope = p['grant']['scope']; adapter = Adapter(); adapter.release.set()
            plan = {'schema':'opsloop-exhibition-plan/v1','planId':label,'authorizationId':p['authorizationID'],
                    'track':'engineering','source':'measured','binding':{k:scope[k] for k in ('runID','epoch','policyUID','deploymentUID')},
                    'expiresAt':p['modelBudget']['expiresAt'],'totalSeconds':900,'approvalReference':'SYNTHETIC_ONLY',
                    'exhibitionPermit':{**pin(f.root/(label+'-issued.json')),'publicKey':f.public}}
            cfg = {'schema':'opsloop-exhibition-control/v1','plans':[pin(f.put(label+'-plan',plan))]}
            ref = pin(f.put(label+'-config',cfg))
            def coordinator(state,config,adapters):
                return Coordinator(state,config,{'engineering':adapter},lifecycle=SyntheticLifecycle())
            with mock.patch('tools.exhibition_session.Coordinator',side_effect=coordinator):
                result = run_once(ref,f.root/'coordinator',label,label+'-request',stop_at=f.intent['notAfter'])
            self.assertTrue(result['stopConfirmed']); self.assertTrue(result['lifecycleComplete'])
            f.complete(p)


class FiniteExpiryTests(unittest.TestCase):
    def delayed_issue(self, boundary, expires='issuance'):
        with tempfile.TemporaryDirectory() as directory:
            f = FiniteFixture(Path(directory))
            start = f.now.timestamp()
            end = start + (1 if expires in ('issuance','grant') else 900)
            deadline = dt.datetime.fromtimestamp(end,dt.timezone.utc).isoformat()
            if expires in ('intent','finite'):
                f.intent['notAfter'] = deadline
                if expires == 'finite':
                    f.bounded['notAfter'] = deadline
                    f.bound_ref = pin(f.put('synthetic-short-finite-approval',f.bounded))
                    f.intent['finiteRoundsApproval'] = f.bound_ref
                f.approve()
            payload = f.payload()
            if expires == 'grant': payload['grant']['notAfter'] = deadline
            request = list(f.request(payload,'expiry-'+boundary))
            approval = json.loads(Path(request[2]).read_text())
            approval['expiresAt'] = deadline if expires == 'issuance' else f.bounded['notAfter']
            approval_ref = pin(f.put('expiry-issuance-approval',approval))
            request[2:4] = [approval_ref['path'],approval_ref['sha256']]
            clock = [start + 0.25]
            real_connect,real_run,real_fsync = sqlite3.connect,subprocess.run,__import__('os').fsync
            triggered = []
            counts = {'begin':0,'commit':0,'fsync':0}
            def advance(name):
                if name == boundary and not triggered:
                    clock[0] = end + 1
                    triggered.append(name)

            class Connection:
                def __init__(self,*args,**kwargs):
                    self.db = real_connect(*args,**kwargs)
                    advance('connect')
                def execute(self,sql,*args):
                    result = self.db.execute(sql,*args)
                    if sql == 'BEGIN IMMEDIATE':
                        counts['begin'] += 1
                        advance('reservation-lock' if counts['begin'] == 1 else 'issued-lock')
                    if sql.startswith('SELECT COALESCE(MAX(ordinal)'):
                        advance('before-reservation')
                    if sql.startswith('INSERT INTO permits VALUES'):
                        advance('reservation-insert')
                    if sql.startswith('UPDATE permits SET state'):
                        advance('issued-update')
                    if sql == 'COMMIT':
                        counts['commit'] += 1
                        advance('reservation-commit' if counts['commit'] == 1 else 'issued-commit')
                    return result
                def close(self):
                    self.db.close()
                    advance('close')

            def signing(*args,**kwargs):
                result = real_run(*args,**kwargs)
                if '-sign' in args[0]: advance('signing')
                return result
            def fsync(fd):
                real_fsync(fd)
                counts['fsync'] += 1
                advance('file-fsync' if counts['fsync'] == 1 else 'directory-fsync')
            with mock.patch('tools.exhibition_permit.time.time',side_effect=lambda:clock[0]), \
                 mock.patch('tools.exhibition_permit.sqlite3.connect',side_effect=Connection), \
                 mock.patch('tools.exhibition_permit.subprocess.run',side_effect=signing), \
                 mock.patch('tools.exhibition_permit.os.fsync',side_effect=fsync):
                with self.assertRaisesRegex(ControlError,'ISSUANCE_AUTHORIZATION_EXPIRED_OR_FUTURE'):
                    issue(*request)
            self.assertEqual(triggered,[boundary])
            with closing(real_connect(f.root/'issuer/issuance.sqlite3')) as db:
                row = db.execute('SELECT state FROM permits WHERE authorization=?',(payload['authorizationID'],)).fetchone()
            if boundary in ('connect','reservation-lock','before-reservation','reservation-insert'):
                self.assertIsNone(row)
            elif boundary in ('issued-commit','close'):
                # A commit already confirmed is immutable history, even when
                # its late response cannot be reported as successful issuance.
                self.assertEqual(row,('ISSUED',))
                f.select(1)
                with self.assertRaises(ControlError): f.payload()
            else:
                self.assertEqual(row,('RESERVED_NO_RETRY',))
                f.complete(payload); f.select(1)
                with self.assertRaisesRegex(ControlError,'ISSUANCE_PREDECESSOR_NOT_COMPLETE'):
                    f.mint(f.payload(),'no-next-after-expired-issuance')
            if boundary in ('connect','reservation-lock','before-reservation','reservation-insert','reservation-commit','signing'):
                self.assertFalse(request[-1].exists())
            else:
                self.assertTrue(request[-1].exists())

    def test_short_issuance_approval_all_io_boundaries(self):
        for boundary in ('connect','reservation-lock','before-reservation','reservation-insert','reservation-commit','signing',
                         'file-fsync','directory-fsync','issued-lock','issued-update','issued-commit','close'):
            with self.subTest(boundary=boundary): self.delayed_issue(boundary)

    def test_grant_intent_and_finite_expiry_during_signing(self):
        # Valid intent/finite windows necessarily enclose the shorter grant;
        # crossing either outer bound must also reject the expired grant.
        for expires in ('grant','intent','finite'):
            with self.subTest(expires=expires): self.delayed_issue('signing',expires)


if __name__ == '__main__':
    unittest.main()
