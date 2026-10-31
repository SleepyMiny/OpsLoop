"""Explicit one-slot same-instance CLI, using existing issuance/session/lifecycle.

Host-owned exact pins remain the approval boundary; booleans are not identity.
The issuer directory owns a persistent lock/journal shared by start and close.
An uncertain dispatch is never replayed. CLOSED closes host session authority,
not the Controller: any Controller rollback needs separately approved custody.
"""
from contextlib import contextmanager
import base64
import copy
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import subprocess
import time

from tools.exhibition_control import instant
from tools.exhibition_hashes import require, private_read, encoded, verify, describe
from tools.exhibition_inputs import pin, new, fresh_payload, materialize
from tools.exhibition_lifecycle import private_dir, verify_completion
from tools.exhibition_execution import Reader, host_idle, trial_gate
from tools.exhibition_finite_rounds import APPROVAL, INTENT, finite_approval
from tools.exhibition_permit import PUBLIC_DER_PREFIX, issue
from tools.exhibition_replay import replay_intent, pretransition_replay, verified_predecessor
from tools.exhibition_stages import stopped, ready

SCHEMA = 'opsloop-finite-cli/v1'
FIELDS = {'schema', 'authorization', 'sourcePins', 'sessionRoot', 'issuerKey',
          'controllerSnapshot', 'controllerReceipt', 'authorityConfig', 'continuation'}


def reject_legacy_mix(plan):
    """Called before legacy resume writes, reads the API, or reserves anything."""
    require(plan.get('newInterval', {}).get('schema') != SCHEMA, 'FINITE_EXPLICIT_OPT_IN_REQUIRED')
    for ref in plan.get('executionInputs', {}).get('circuitReplayApproval', {}).values():
        intent = private_read(ref['path'], ref['sha256'])
        require(intent.get('schema') != INTENT, 'FINITE_EXPLICIT_OPT_IN_REQUIRED')


def inputs(plan, authorization_id, *, close=False):
    cfg = plan['newInterval']; e = plan['executionInputs']; i = e['integration']
    require(set(cfg) == FIELDS and cfg['schema'] == SCHEMA, 'FINITE_CLI_CONFIG_REQUIRED')
    require(plan.get('status') == 'SEALED_EXECUTION_CANDIDATE', 'UNSEALED_EXECUTION_PLAN')
    require(isinstance(cfg['sourcePins'], list) and bool(cfg['sourcePins']), 'FINITE_SOURCE_PINS_REQUIRED')
    for ref in cfg['sourcePins']:
        require(pin(ref['path']) == ref, 'FINITE_SOURCE_CHANGED')
    required_sources = {str(Path(__file__).resolve()), str(Path(__file__).with_name('exhibition_resume.py').resolve())}
    require(required_sources <= {str(Path(ref['path']).resolve()) for ref in cfg['sourcePins']}, 'FINITE_ENTRYPOINT_PINS_REQUIRED')
    approval = private_read(cfg['authorization']['path'], cfg['authorization']['sha256'])
    require(approval.get('schema') == APPROVAL and approval.get('approved') is True, 'FINITE_APPROVAL_REQUIRED')
    require(Path(i['issuanceState']).resolve() == Path(approval['issuerLedger']).resolve(), 'FINITE_LEDGER_CHANGED')
    require(i['sessionRoot'] == cfg['sessionRoot'], 'FINITE_SESSION_ROOT_CHANGED')
    require(set(cfg['continuation']) == {'predecessorCompletion'}, 'FINITE_CONTINUATION_REQUIRED')
    # No historical path inference: every helper used by the existing execution
    # flow is explicit and pinned in this approved plan.
    for name in ('policyHash', 'driver', 'grant', 'quoteProbe', 'kubernetes', 'runtimeB', 'provenance', 'audit', 'kubectl', 'kubeconfig'):
        ref = i[name]
        require(pin(ref['path']) == ref, 'FINITE_HELPER_CHANGED')
    key = Path(cfg['issuerKey']['path'])
    require(not key.is_symlink() and stat.S_ISREG(key.stat().st_mode)
            and key.stat().st_uid == os.getuid() and key.stat().st_mode & 0o077 == 0
            and pin(key) == cfg['issuerKey'], 'FINITE_ISSUER_KEY_CHANGED')
    public_result = subprocess.run(['/usr/bin/openssl', 'pkey', '-in', str(key), '-pubout', '-outform', 'DER'], capture_output=True, timeout=5)
    require(public_result.returncode == 0 and public_result.stdout.startswith(PUBLIC_DER_PREFIX)
            and len(public_result.stdout) == 44, 'FINITE_PUBLIC_KEY_FAILED')
    public = base64.b64encode(public_result.stdout[-32:]).decode()
    expected = {k:private_read(cfg[k]['path'], cfg[k]['sha256']) for k in ('controllerSnapshot', 'controllerReceipt', 'authorityConfig')}
    receipt = expected['controllerReceipt']
    require(all(receipt.get(k) == v for k,v in approval['controller'].items()), 'FINITE_CONTROLLER_CHANGED')
    if close:
        # Closing cannot create a grant and remains available after slot expiry.
        intent = None
    else:
        ref = e.get('circuitReplayApproval', {}).get('b')
        require(ref is not None, 'FINITE_SLOT_INTENT_REQUIRED')
        intent = replay_intent(ref)
        require(intent['schema'] == INTENT and intent['authorizationID'] == authorization_id
                and intent['finiteRoundsApproval'] == cfg['authorization'], 'FINITE_SELECTED_SLOT_CHANGED')
        checked, _ = finite_approval(intent)
        require(checked == approval, 'FINITE_APPROVAL_CHANGED')
        require(intent['predecessorCompletion'] == cfg['continuation']['predecessorCompletion'], 'FINITE_PREDECESSOR_CHANGED')
        require(time.time()+1080 < instant(intent['notAfter']), 'FINITE_ROUND_AND_CLEANUP_WINDOW_REQUIRED')
    return cfg, i, approval, intent, expected, public


def controller_receipt(plan, objects, expected, public):
    """Validate the existing static incarnation; never invoke transition/rollout."""
    from tools.exhibition_resume import current_identity
    before = expected['controllerSnapshot']; observed = objects['controller']; e = plan['executionInputs']
    c = plan['deployment']['controller']; old = expected['controllerReceipt']
    require(observed['metadata']['uid'] == before['metadata']['uid'] == c['uid'], 'FINITE_CONTROLLER_UID_CHANGED')
    verify(observed, describe(before, '/spec'), fields='/spec')
    require(observed['metadata']['generation'] == before['metadata']['generation'] and ready(observed), 'FINITE_CONTROLLER_GENERATION_OR_READY_CHANGED')
    # current_identity normally validates the legacy spec; here its exact hash is
    # the approved already-static snapshot. The approved input itself is immutable.
    same = copy.deepcopy(plan); same['deployment']['controller']['expectedSpecHash'] = describe(before, '/spec')
    identity = current_identity(same, objects)
    require(identity['restartCount'] == 0 and all(identity[k] == old[k] for k in ('podUID', 'podName', 'instance', 'containerID')), 'FINITE_CONTROLLER_RESTARTED')
    pod = next(p for p in objects['controller-pods']['items'] if p['metadata']['uid'] == identity['podUID'])
    image = pod['status']['containerStatuses'][0]['imageID']
    require(image == old['imageID'] and image.endswith('@'+plan['deployment']['imagePublication']['digest'])
            and pod['spec']['nodeName'] == old['node'], 'FINITE_CONTROLLER_IMAGE_OR_NODE_CHANGED')
    logs = [json.loads(line) for line in objects['controller-log-'+identity['podUID']].splitlines() if line.startswith('{')]
    identities = [v for v in logs if v.get('schema') == 'opsloop-controller-execution-identity/v1']
    require(len(identities) == 1 and identities[0].get('enforceDefault') == 'deny-without-fresh-bound-grant', 'FINITE_CONTROLLER_DEFAULT_CHANGED')
    cm = objects[e['authorityConfigName']]; approved_cm = expected['authorityConfig']
    require(cm['metadata']['uid'] == approved_cm['metadata']['uid'] == old['configMapUID']
            and cm.get('immutable') is True and cm.get('data') == approved_cm.get('data'), 'FINITE_STATIC_AUTHORITY_CHANGED')
    config = json.loads(cm['data']['authority.json'])
    targets = [{k:v for k,v in {'clusterUID':plan['clusterUID'], **{k:t[k] for k in ('namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}}.items()} for t in plan['fixedTargets'].values()]
    require(set(config) == {'schema','publicKey','targets'} and config['schema'] == 'opsloop-exhibition-authority/v1'
            and config['publicKey'] == public and isinstance(config['targets'], list)
            and sorted(encoded(v) for v in config['targets']) == sorted(encoded(v) for v in targets), 'FINITE_STATIC_KEY_OR_TARGET_CHANGED')
    ps = observed['spec']['template']['spec']; managers = [v for v in ps['containers'] if v['name'] == 'manager']
    require(len(managers) == 1, 'FINITE_CONTROLLER_CONTAINER_CHANGED')
    manager = managers[0]; args = manager['args']
    require(args.count('--exhibition-authority-file=/etc/opsloop/execution/authority.json') == 1
            and not any(a.startswith('--execution-scope-file') for a in args), 'FINITE_STATIC_CONTROLLER_REQUIRED')
    mounts = [v for v in manager.get('volumeMounts',[]) if v.get('mountPath') == '/etc/opsloop/execution']
    require(len(mounts) == 1 and mounts[0].get('readOnly') is True, 'FINITE_STATIC_MOUNT_CHANGED')
    volumes = [v for v in ps['volumes'] if v['name'] == mounts[0]['name']]
    require(len(volumes) == 1 and volumes[0].get('configMap') == {'name':cm['metadata']['name'], 'items':[{'key':'authority.json','path':'authority.json'}]}, 'FINITE_STATIC_VOLUME_CHANGED')
    require(objects['cluster']['metadata']['uid'] == plan['clusterUID'], 'CLUSTER_UID_CHANGED')
    return {**old, 'state':'STATIC_CONTROLLER_READY', 'expectedInstance':identity['instance'],
            'priorInstance':identity['instance'], 'priorPodUID':identity['podUID']}


def safe_baselines(plan, objects):
    for key,t in plan['fixedTargets'].items():
        pol = objects[key+'-policy']; stopped(pol, t['policyUID'])
        require(not any(k in pol['metadata'].get('annotations',{}) for k in ('opsloop.io/exhibition-permit','opsloop.io/execution-grant')), 'FINITE_AUTHORITY_NOT_RETIRED')
        dep = objects[key+'-deployment']; require(dep['metadata']['uid'] == t['deploymentUID'], 'FINITE_DEPLOYMENT_CHANGED')
        verify(dep, t['baselineSpecHash'], fields='/spec'); require(ready(dep), 'FINITE_BASELINE_NOT_READY')


@contextmanager
def journal(integration, *, deadline=None):
    """One OS lock for every invocation sharing this issuer, including close.

    SQLite FULL/DELETE commits plus fsync(directory) precede external dispatch.
    Requires a local filesystem with working flock, fsync and SQLite durability;
    restoring/deleting either journal or issuer ledger is not supported recovery.
    """
    root = private_dir(integration['issuanceState']); path = root/'finite-cli.sqlite3'
    fd = os.open(root/'finite-cli.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    db = None
    try:
        require(stat.S_ISREG(os.fstat(fd).st_mode) and os.fstat(fd).st_uid == os.getuid()
                and os.fstat(fd).st_mode & 0o077 == 0, 'FINITE_LOCK_UNSAFE')
        try: fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError: raise ValueError('FINITE_SESSION_BUSY') from None
        require(not path.is_symlink(), 'FINITE_JOURNAL_UNSAFE')
        if not path.exists():
            created = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600); os.close(created)
        require(path.stat().st_uid == os.getuid() and path.stat().st_mode & 0o077 == 0, 'FINITE_JOURNAL_UNSAFE')
        db = sqlite3.connect(path, isolation_level=None)
        db.execute('PRAGMA journal_mode=DELETE'); db.execute('PRAGMA synchronous=FULL')
        db.execute('CREATE TABLE IF NOT EXISTS session (singleton INTEGER PRIMARY KEY CHECK(singleton=1), contract TEXT NOT NULL, state TEXT NOT NULL)')
        db.execute('CREATE TABLE IF NOT EXISTS slots (authorization TEXT PRIMARY KEY, run TEXT UNIQUE, ordinal INTEGER UNIQUE, state TEXT NOT NULL, completion TEXT)')
        dfd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        try: os.fsync(dfd)
        finally: os.close(dfd)
        yield db
    finally:
        try:
            if db is not None:
                db.close()
                # Check even after the final commit, result fsync and connection
                # close. Expiry cannot turn a delayed response into readiness.
                # The same lock is still held while the tombstone is committed.
                if deadline is not None and time.time() >= deadline:
                    expired = sqlite3.connect(path, isolation_level=None)
                    try:
                        expired.execute('PRAGMA synchronous=FULL')
                        expired.execute("UPDATE session SET state='EXPIRED' WHERE singleton=1")
                    finally: expired.close()
                    raise ValueError('FINITE_SESSION_DEADLINE_PASSED')
        finally:
            os.close(fd)


def bind_session(db, cfg, integration):
    contract = json.dumps({'approval':cfg['authorization'], 'controllerSnapshot':cfg['controllerSnapshot'],
                          'controllerReceipt':cfg['controllerReceipt'], 'authorityConfig':cfg['authorityConfig'],
                          'issuerKey':cfg['issuerKey'], 'controlState':integration['controlState'],
                          'engineeringState':integration['engineering']['state'], 'sessionRoot':cfg['sessionRoot']}, sort_keys=True)
    row = db.execute('SELECT contract,state FROM session WHERE singleton=1').fetchone()
    if row is None:
        db.execute('INSERT INTO session VALUES (1,?,?)', (contract,'OPEN'))
    else:
        require(row[0] == contract, 'FINITE_SESSION_CONTRACT_CHANGED')
        require(row[1] == 'OPEN', 'FINITE_SESSION_CLOSED_OR_UNCONFIRMED')
    require(not db.execute("SELECT 1 FROM slots WHERE state!='COMPLETED'").fetchone(), 'FINITE_PRIOR_DISPATCH_UNCONFIRMED')


def execute(plan, approval_sha, *, authorization_id=None, close=False):
    require((authorization_id is not None) != close, 'FINITE_ONE_ACTION_REQUIRED')
    cfg, i, approval, intent, expected, public = inputs(plan, authorization_id, close=close)
    with journal(i, deadline=None if close else instant(intent['notAfter'])) as db:
        bind_session(db, cfg, i)
        if close:
            # Irreversible host-side tombstone FIRST; even a failed verification
            # cannot race with the next slot or reopen authority on restart.
            db.execute("UPDATE session SET state='CLOSING' WHERE singleton=1")
        else:
            require(db.execute('SELECT 1 FROM slots WHERE authorization=?', (authorization_id,)).fetchone() is None, 'FINITE_SLOT_ALREADY_CONSUMED')
        root = private_dir(cfg['sessionRoot'])
        deadline = time.time()+180 if close else instant(intent['notAfter'])
        reader = Reader(plan, root, deadline)
        objects = reader.snapshot()
        receipt = controller_receipt(plan, objects, expected, public)
        safe_baselines(plan, objects); host_idle(i)
        if close:
            db.execute("UPDATE session SET state='CLOSED' WHERE singleton=1")
            result = {'state':'FINITE_SESSION_CLOSED_CONTROLLER_UNCHANGED', 'nextRunIsAuthorized':False,
                      'controllerRollback':'NOT_REQUESTED', 'controller':receipt}
            new(root, 'finite-close.json', result)
            return result
        # Existing readiness gates and replay history remain authoritative.
        trial_gate(plan, 'b', objects, receipt)
        enforce = copy.deepcopy(objects['b-policy']['spec']); enforce.update(enabled=True, mode='Enforce')
        spec = new(root, 'enforce-spec-'+authorization_id+'.json', enforce)
        hashed = subprocess.run([i['policyHash']['path'], spec['path']], capture_output=True, timeout=10)
        policy_hash = hashed.stdout.decode().strip()
        require(hashed.returncode == 0 and len(policy_hash) == 64, 'POLICY_HASH_FAILED')
        pretransition_replay(plan, 'b', objects, receipt, policy_hash)
        slot = intent['scope']; previous = intent['predecessorCompletion']
        # Exact expected prior local slot, if any, must have completed through
        # this journal too. A different control ledger cannot stand in for it.
        index = next(n for n,s in enumerate(approval['rounds']) if s['authorizationID'] == authorization_id)
        if index:
            prior = approval['rounds'][index-1]
            row = db.execute('SELECT state,completion FROM slots WHERE authorization=?', (prior['authorizationID'],)).fetchone()
            require(row is not None and row[0] == 'COMPLETED' and json.loads(row[1]) == previous, 'FINITE_PRIOR_LOCAL_COMPLETION_REQUIRED')
        now = dt.datetime.now(dt.timezone.utc)
        payload = fresh_payload(plan, 'b', objects, receipt, policy_hash=policy_hash, run_id=slot['runID'], epoch=slot['epoch'],
                                authorization_id=authorization_id, ordinal=intent['ordinal'], previous_completion_sha=previous['sha256'], now=now)
        # Commit BEFORE issuer reservation, materialization or child dispatch.
        # Any failure beyond here burns the invocation; only explicit cleanup
        # using the existing reconciliation CLI may establish later safety.
        db.execute('INSERT INTO slots VALUES (?,?,?,?,NULL)', (authorization_id, slot['runID'], intent['ordinal'], 'DISPATCH_RESERVED'))
        runroot = private_dir(str(root/slot['runID']))
        pref = new(runroot, 'permit-payload.json', payload)
        cref = new(runroot, 'controller-receipt.json', receipt)
        request = new(runroot, 'permit-approval.json', {'schema':'opsloop-permit-issuance-approval/v1','approved':True,
                      'payloadSHA256':pref['sha256'], 'authorizationID':authorization_id, 'expiresAt':payload['grant']['notAfter'],
                      'previousCompletion':previous['path'], 'previousCompletionSHA256':previous['sha256'],
                      'circuitReplayApproval':plan['executionInputs']['circuitReplayApproval']['b'], 'controllerReceipt':cref})
        output = runroot/'permit.json'
        checksum = issue(pref['path'], pref['sha256'], request['path'], request['sha256'], cfg['issuerKey']['path'], i['issuanceState'], output)
        objects['authority-config'] = objects[plan['executionInputs']['authorityConfigName']]
        outer, ref = materialize(plan, 'b', objects, receipt, payload, {'path':str(output),'sha256':checksum,'publicKey':public}, runroot, i,
                                 approval_sha=approval_sha, session_deadline=intent['notAfter'])
        control = new(runroot, 'control.json', {'schema':'opsloop-exhibition-control/v1','plans':[ref]})
        from tools.exhibition_session import run_in_process, outcome_summary
        result = run_in_process(control, i['controlState'], outer['planId'], 'once-'+slot['runID'], stop_at=intent['notAfter'],
                                output_root=runroot, total_seconds=outer['totalSeconds'])
        require(outcome_summary(result)['trialSucceeded'] is True, 'FINITE_RUN_OR_CLEANUP_UNCONFIRMED')
        completion = {'path':str(Path(i['controlState'])/'lifecycle'/result['runId']/'completion.json'), 'sha256':result['lifecycleReceiptSHA256']}
        done = verify_completion(completion['path'], completion['sha256'])
        require(done['binding'] == outer['binding'], 'FINITE_COMPLETION_BINDING_CHANGED')
        # verified_predecessor checks retirement; use a distinct hypothetical
        # identity only to validate this run as the completed predecessor.
        verified_predecessor(completion, {**outer['binding'], 'runID':'next-'+slot['runID'], 'epoch':'next-'+slot['epoch']})
        final = reader.snapshot(); controller_receipt(plan, final, expected, public); safe_baselines(plan, final); host_idle(i)
        require(time.time() < deadline, 'FINITE_SESSION_DEADLINE_PASSED')
        db.execute("UPDATE slots SET state='COMPLETED',completion=? WHERE authorization=?", (json.dumps(completion,sort_keys=True),authorization_id))
        result = {'state':'FINITE_SLOT_COMPLETE', 'ordinal':intent['ordinal'], 'authorizationID':authorization_id,
                  'completion':completion, 'controller':receipt, 'nextRunReady':True, 'nextRunIsAuthorized':False}
        new(runroot, 'finite-result.json', result)
        return result
