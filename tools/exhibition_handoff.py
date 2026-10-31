"""Same-run trusted Runtime capture -> existing seal -> isolated Runner handoff.

No browser filenames, capture upload, prior-bundle adoption, setup or publication.
The immutable outer plan approves the Runtime plan and all build/source authority.
Generated digests bind newly collected evidence, never extend that authority.
"""
from __future__ import annotations
import copy
import datetime as dt
import importlib.util
import json
import os
from pathlib import Path
import time

from tools.exhibition_control import ControlError, pinned, validate_plan, instant
from tools.engineering_evidence.seal import build_bundle, seal_bundle, canonical, digest


def require(ok, code):
    if not ok: raise ControlError(code)


def write_new(path, value):
    data = canonical(value)
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'wb') as f:
        f.write(data); f.flush(); os.fsync(f.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    return digest(data)


def source_status_event(policy, binding, generation):
    """Project only this live run's fresh Controller condition; never authority."""
    meta=policy.get('metadata', {})
    if meta.get('uid') != binding.get('policyUID') or meta.get('generation') != generation:
        return None
    try:
        grant=json.loads(meta.get('annotations', {}).get('opsloop.io/execution-grant', '{}'))
    except (ValueError, TypeError):
        return None
    if any(grant.get('scope', {}).get(k) != v for k,v in binding.items()):
        return None
    conditions=[c for c in policy.get('status', {}).get('conditions', []) if c.get('type')=='ReproductionSource' and c.get('observedGeneration')==generation]
    if len(conditions)!=1:
        return None
    c=conditions[0]
    states={('SourcePending','Unknown'):'PENDING', ('SourceBound','True'):'BOUND', ('SourceRejected','False'):'REJECTED'}
    phase=states.get((c.get('reason'),c.get('status')))
    if phase is None:
        return None
    return ('SOURCE_'+phase, {'phase':phase, 'reasonCode':c['reason'], 'policyUID':binding['policyUID']})


def preflight(plan):
    validate_plan(plan)
    cfg = plan['runtimeCapture']
    require(set(cfg) == {'root', 'plan', 'planSHA256', 'setupReceipt', 'setupReceiptSHA256', 'provenance', 'provenanceSHA256'}, 'CAPTURE_CONFIG_INVALID')
    root = Path(cfg['root'])
    require(root.is_absolute() and not root.is_symlink() and root.is_dir() and root.stat().st_mode & 0o077 == 0, 'CAPTURE_PRIVATE_ROOT_REQUIRED')
    p = pinned(cfg['plan'], cfg['planSHA256']); setup = pinned(cfg['setupReceipt'], cfg['setupReceiptSHA256'])
    require(p.get('runID') == plan['binding']['runID'] and p.get('epoch') == plan['binding']['epoch'], 'CAPTURE_RUN_MISMATCH')
    require(all(p['executionScope'][key] == value for key, value in plan['binding'].items()), 'CAPTURE_UID_MISMATCH')
    repeated = 'exhibitionPermit' in plan or 'exhibitionPermit' in p
    expected_setup = 'STATIC_CONTROLLER_READY_POLICY_DISABLED' if repeated else 'SCOPE_READY_POLICY_DISABLED'
    require(setup.get('state') == expected_setup and setup.get('planSHA256') == cfg['planSHA256'], 'CAPTURE_SETUP_MISMATCH')
    if repeated:
        from tools.exhibition_permit import bound_permit
        from tools.exhibition_demo_b_trial import exhibition_permit, policy_generations
        require(p.get('exhibitionPermit') == plan.get('exhibitionPermit') and p.get('authorizationId') == plan.get('authorizationId'), 'CAPTURE_PERMIT_MISMATCH')
        _, payload = bound_permit(plan, model=True)
        policy_generations(p)
        _, runtime_payload = exhibition_permit(p, setup)
        require(runtime_payload == payload, 'CAPTURE_PERMIT_MISMATCH')
        budget = payload['modelBudget']
        require(budget['model'] == plan.get('model') and budget['maxModelCalls'] == plan.get('maxModelCalls')
                and budget['totalSeconds'] == plan['totalSeconds'] and instant(budget['expiresAt']) == instant(plan['expiresAt']), 'CAPTURE_MODEL_BUDGET_MISMATCH')
    require(instant(p['expiresAt']) >= instant(plan['expiresAt']) and plan.get('model') == 'gpt-6-luna' and plan.get('maxModelCalls') == 1, 'CAPTURE_AUTHORITY_MISMATCH')
    prov = pinned(cfg['provenance'], cfg['provenanceSHA256'])
    require(prov['repository'] == plan['engineering']['repository'] and prov['base_commit'] == plan['engineering']['base_commit'], 'CAPTURE_SOURCE_MISMATCH')
    require(Path(cfg['provenance']) == root/'verification/image-provenance.json', 'CAPTURE_PROVENANCE_LOCATION')
    # No previous execution/capture/seal/derived plan can be adopted, even if valid.
    for path in ('runtime-state', 'incident-raw', 'sealed', 'execution-plan.json', 'handoff-start.json', 'runtime-receipt.json', 'incident-identity.json'):
        require(not (root/path).exists(), 'CAPTURE_ALREADY_CONSUMED')
    return cfg, root, p, setup


def engineering_budget(plan, bundle_sha256):
    """Bind the new seal to the already approved run/model budget, never expand it."""
    budget = {'schema':'opsloop-engineering-plan/v1', 'bundle_sha256':bundle_sha256,
              'run_id':plan['binding']['runID'], 'epoch':plan['binding']['epoch'],
              'policy_uid':plan['binding']['policyUID'], 'deployment_uid':plan['binding']['deploymentUID'],
              'model':plan['model'], 'reasoning_effort':'medium', 'max_model_calls':plan['maxModelCalls'],
              'total_seconds':plan['totalSeconds'], 'expires_at':dt.datetime.fromtimestamp(instant(plan['expiresAt']), dt.timezone.utc).isoformat().replace('+00:00','Z')}
    if 'exhibitionPermit' in plan:
        from tools.exhibition_permit import bound_permit
        _, payload = bound_permit(plan, model=True)
        approved = payload['modelBudget']
        require(approved['model'] == plan['model'] and approved['maxModelCalls'] == plan['maxModelCalls']
                and approved['totalSeconds'] == plan['totalSeconds'] and instant(approved['expiresAt']) == instant(plan['expiresAt']), 'CAPTURE_MODEL_BUDGET_MISMATCH')
        budget['schema'] = 'opsloop-engineering-plan/v2'
        budget['exhibition'] = {'authorization_id':plan['authorizationId'], 'permit':copy.deepcopy(plan['exhibitionPermit']),
                                'previous_completion_sha256':payload['previousCompletionSHA256']}
    return budget


def collect(plan, emit, cancel):
    cfg, root, p, setup = preflight(plan)
    started = time.time()
    require(not cancel.is_set(), 'CAPTURE_CANCELLED')
    write_new(root/'handoff-start.json', {'binding':plan['binding'], 'startedAt':dt.datetime.now(dt.timezone.utc).isoformat(), 'planSHA256':cfg['planSHA256']})
    # Private module instance: no mutation of global historical-module state.
    source = Path(__file__).with_name('exhibition_demo_b_trial.py')
    spec = importlib.util.spec_from_file_location('_opsloop_current_trial', source)
    core = importlib.util.module_from_spec(spec); spec.loader.exec_module(core); core.ROOT = root
    from tools.exhibition_control import identifier
    require(identifier(p['executionScope']['policy']), 'CAPTURE_POLICY_NAME_INVALID')
    core.POLICY_NAME=p['executionScope']['policy']
    core.POLICY='/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo-b/recoverypolicies/'+core.POLICY_NAME
    cleanup = False
    original_finish, original_native = core.finish_guard, core.native
    def check():
        require(not cancel.is_set() and time.time() < min(started+plan['totalSeconds'], instant(plan['expiresAt'])), 'CAPTURE_CANCELLED_OR_EXPIRED')
    def finish(*args):
        nonlocal cleanup
        cleanup = True
        emit('coordinator', 'CLEANUP_PENDING', {'phase':'RUNTIME_STRONG_STOP'})
        return original_finish(*args)
    core.finish_guard = finish
    def native(*args, **kwargs):
        if not cleanup: check()
        at=time.monotonic()
        result=original_native(*args, **kwargs)
        if len(args)>1 and args[1]=='quoteProbe':
            emit('workload','SERVICE_PROBE',{'httpStatus':result.get('http_status'), 'podUID':result.get('pod_uid'), 'latencyMs':round((time.monotonic()-at)*1000)})
        return result
    core.native = native
    # load verifies exact core/helper/source/build hashes before authenticated reads.
    p, kube, context = core.load(Path(cfg['plan']), cfg['planSHA256'])
    record = {'requests':[], 'planSHA256':cfg['planSHA256'], 'state':'STARTING', 'binding':copy.deepcopy(plan['binding'])}
    last = None
    last_source = None
    def save():
        nonlocal last
        core.atomic(root/'runtime-receipt.json', record)
        state = record['state']
        if state != last:
            emit('coordinator', 'RUNTIME_STAGE', {'phase':state}); last = state
    class Client(core.Client):
        def request(self, *args, **kwargs):
            nonlocal last_source
            if not cleanup: check()
            value=super().request(*args, **kwargs)
            if isinstance(value,dict) and value.get('kind')=='RecoveryPolicy':
                # Private bounded last response survives a subsequent Guard stop.
                core.atomic(root/'last-runtime-policy.json',value)
                record['lastPolicyConditions']=value.get('status',{}).get('conditions',[])
                source=source_status_event(value, plan['binding'], p.get('executionScope',{}).get('policyGeneration'))
                if source is not None and source != last_source:
                    emit('controller', source[0], source[1]); last_source=source
                save()
            if isinstance(value,dict) and value.get('kind')=='RecoveryIncident':
                runtime=value.get('status',{}).get('runtime',{})
                emit('controller','CONTROLLER_ACTION',{'incidentUID':value['metadata']['uid'], 'phase':runtime.get('phase'), 'reasonCode':runtime.get('reason')})
                decision=runtime.get('decision') or {}
                from tools.exhibition_runtime import public_candidate
                emit('controller','CANDIDATES',{'candidates':[public_candidate(c) for c in decision.get('candidates',[])[:16]]})
            return value
    emit('coordinator', 'RUNTIME_CAPTURE_STARTED', {'runID':p['runID'], 'epoch':p['epoch']})
    try:
        core.trial(Client(kube, context, record, save), p, setup, record, save)
    except Exception as exc:
        # Keep the original exception separate from any later cleanup outcome.
        write_new(root/'runtime-error.json', {'binding':plan['binding'],'errorType':type(exc).__name__,
                    'reason':str(exc) if type(exc).__name__ in ('Stop','ControlError') else 'UNEXPECTED_RUNTIME_ERROR','diagnostic':getattr(exc,'diagnostic',None),'at':dt.datetime.now(dt.timezone.utc).isoformat()})
        raise
    require(record.get('stopReceipt', {}).get('state', {}).get('phase') == 'STOP_CONFIRMED', 'CAPTURE_STOP_UNCONFIRMED')
    check()
    capture = root/'incident-raw'; manifest_path=capture/'capture-manifest.json'
    manifest = pinned(manifest_path, record['captureManifestSHA256'])
    require(instant(manifest['captured_at']) >= started, 'CAPTURE_NOT_CURRENT')
    auth = capture/'engineering-authorization.json'
    bundle = build_bundle(capture, capture_sha256=record['captureManifestSHA256'], authorization_path=auth,
                          authorization_sha256=record['engineeringAuthorizationSHA256'], provenance_path=Path(cfg['provenance']),
                          provenance_sha256=cfg['provenanceSHA256'], repository_root=Path(plan['engineering']['repository_path']))
    require(bundle['incident']['uid'] == record['incidentUID'], 'CAPTURE_INCIDENT_CHANGED')
    require(all(instant(o['observed_at']) >= started for o in bundle['evidence']['reproduction'])
            and all(instant(a['patched_at']) >= started for a in bundle['runtime']['attempts']), 'CAPTURE_PREVIOUS_EXECUTION_REJECTED')
    pins = seal_bundle(bundle, root/'sealed')
    bound = copy.deepcopy(plan); args=bound['engineering']
    args['bundle'] = str(root/'sealed/bundle.json'); args['bundle_sha256'] = pins['bundle_sha256']
    budget = engineering_budget(plan, pins['bundle_sha256'])
    args['execution_plan']=str(root/'execution-plan.json'); args['execution_plan_sha256']=write_new(root/'execution-plan.json',budget)
    emit('observer','EVIDENCE_SEALED',{'sourceSHA256':pins['bundle_sha256'], 'incidentUID':record['incidentUID'], 'phase':'CURRENT_RUN_RUNTIME_EVIDENCE', 'stopConfirmed':True})
    return bound
