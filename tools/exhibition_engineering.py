"""Exhibition adapter for the existing isolated Runner; never a Publisher.

Only server-owned pinned plans reach this adapter. All preparation is local.
"""
from __future__ import annotations

import hashlib
import re
import os
from pathlib import Path
import stat
from types import SimpleNamespace

from tools.engineering_runner.contracts import Rejected, canonical, digest, inventory, regular_file, validate_bundle
from tools.engineering_runner.live import execution_plan
from tools.engineering_runner.runner import run as run_engineering
from tools.exhibition_control import ControlError, instant, validate_plan


FIELDS = {'bundle', 'bundle_sha256', 'state', 'repository_path', 'repository', 'base_commit',
          'harness', 'harness_sha256', 'go_root', 'go_binary_sha256', 'codex_binary',
          'codex_binary_sha256', 'auth_file', 'execution_plan', 'execution_plan_sha256'}
PATHS = {'bundle', 'state', 'repository_path', 'harness', 'go_root', 'codex_binary', 'auth_file', 'execution_plan'}


def _file_hash(path, expected):
    if not isinstance(expected, str) or len(expected) != 64:
        raise ControlError('INVALID_ENGINEERING_PIN')
    h = hashlib.sha256()
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as f:
        st = os.fstat(f.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_mode & 0o022:
            raise ControlError('UNSAFE_ENGINEERING_EXECUTABLE')
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    if h.hexdigest() != expected:
        raise ControlError('ENGINEERING_EXECUTABLE_CHANGED')


def require_verified_result(result,plan,args):
    """Do not promote a state string or a different run's successful result."""
    b=plan['binding'];inc=result.get('incident',{});compare=result.get('comparison',{})
    baseline=result.get('baseline_verification',{});candidate=result.get('candidate_verification',{})
    from tools.engineering_runner.live import public_checks, CHECKS
    if (inc.get('run_id')!=b['runID'] or inc.get('epoch')!=b['epoch']
            or result.get('bundle_sha256')!=args.bundle_sha256
            or type(result.get('broker',{}).get('dispatched_requests')) is not int
            or not 1<=result['broker']['dispatched_requests']<=plan['maxModelCalls']
            or result.get('real_model_execution') is not True
            or baseline.get('accepted') is not True or candidate.get('accepted') is not True
            or baseline.get('report',{}).get('baseline_defect_reproduced') is not True
            or candidate.get('report',{}).get('all_passed') is not True
            or compare.get('same_input_confirmed') is not True
            or compare.get('baseline')!=public_checks(baseline) or compare.get('candidate')!=public_checks(candidate)
            or {c.get('name') for c in compare.get('baseline',[])}!=set(CHECKS)
            or {c.get('name') for c in compare.get('candidate',[])}!=set(CHECKS)
            or len(compare.get('baseline',[]))!=len(CHECKS) or len(compare.get('candidate',[]))!=len(CHECKS)
            or not all(c.get('passed') is True for c in compare['candidate'])
            or not any(c.get('name')=='empty-order-regression' and c.get('passed') is False for c in compare['baseline'])
            or not re.fullmatch('[a-f0-9]{64}',result.get('candidate',{}).get('source_sha256',''))
            or not re.fullmatch('[a-f0-9]{64}',result.get('patch_sha256',''))
            or candidate.get('source_sha256')!=result.get('candidate',{}).get('source_sha256')):
        raise ControlError('VERIFIED_RESULT_EVIDENCE_UNCONFIRMED')
    original=next(c for c in compare['baseline'] if c['name']=='empty-order-regression')
    revised=next(c for c in compare['candidate'] if c['name']=='empty-order-regression')
    if (not re.fullmatch('[a-f0-9]{64}',original.get('fixture_sha256',''))
            or original['fixture_sha256']!=revised.get('fixture_sha256')
            or original.get('status')!=500 or revised.get('status')!=200):
        raise ControlError('VERIFIED_RESULT_INPUT_COMPARISON_UNCONFIRMED')


class EngineeringAdapter:
    def _args(self, plan, *, pending=False):
        validate_plan(plan)
        if plan['track'] != 'engineering' or plan.get('model') != 'gpt-6-luna':
            raise ControlError('ENGINEERING_MODEL_NOT_APPROVED')
        config = plan.get('engineering')
        if not isinstance(config, dict) or set(config) != FIELDS:
            raise ControlError('INVALID_ENGINEERING_CONFIG')
        args = SimpleNamespace(**config, approve_model_execution=True, model='gpt-6-luna',
                               reasoning_effort='medium', auth_mode='chatgpt')
        for name in PATHS:
            value = getattr(args, name)
            if not isinstance(value, str):
                raise ControlError('INVALID_ENGINEERING_PATH')
            path = Path(value)
            if not path.is_absolute() or path.is_symlink() or '..' in path.parts:
                raise ControlError('INVALID_ENGINEERING_PATH')
            setattr(args, name, path)
        hashes = inventory(args.harness)
        if set(hashes) != {'verify.py', 'audit.go'} or digest(canonical(hashes)) != args.harness_sha256:
            raise ControlError('ENGINEERING_HARNESS_CHANGED')
        _file_hash(args.codex_binary, args.codex_binary_sha256)
        _file_hash(args.go_root / 'bin/go', args.go_binary_sha256)
        # Credential contents stay broker-only, never inspected or projected here.
        auth = args.auth_file.stat(follow_symlinks=False)
        if not stat.S_ISREG(auth.st_mode) or auth.st_uid != os.getuid() or auth.st_mode & 0o077:
            raise ControlError('PRIVATE_ENGINEERING_AUTH_REQUIRED')
        if args.state.exists() and (not args.state.is_dir() or args.state.stat().st_mode & 0o077):
            raise ControlError('PRIVATE_ENGINEERING_STATE_REQUIRED')
        if pending:
            return args
        data = regular_file(args.bundle, 512 * 1024)
        bundle = validate_bundle(data, args.bundle_sha256, args.repository, args.base_commit)
        binding = {'runID': bundle['incident']['run_id'], 'epoch': bundle['incident']['epoch'],
                   'policyUID': bundle['workload']['policy_uid'], 'deploymentUID': bundle['workload']['deployment_uid']}
        if binding != plan['binding']:
            raise ControlError('ENGINEERING_BINDING_MISMATCH')
        budget = execution_plan(args, bundle)
        if (budget['max_model_calls'] != plan.get('maxModelCalls')
                or type(plan.get('maxModelCalls')) is not int
                or budget['total_seconds'] != plan['totalSeconds']
                or instant(budget['expires_at']) != instant(plan['expiresAt'])):
            raise ControlError('ENGINEERING_BUDGET_MISMATCH')
        args.bound_bundle = bundle
        return args

    def prepare(self, plan):
        if "runtimeCapture" in plan:
            from tools.exhibition_handoff import preflight
            preflight(plan)
            self._args(plan, pending=True)
        else:
            self._args(plan)

    def reconcile(self, plan, emit):
        from tools.exhibition_reconcile import reconcile_engineering
        return reconcile_engineering(plan, emit)

    def run(self, plan, emit, cancel_event):
        from tools.exhibition_reconcile import record_start, record_outcome
        record_start(plan)
        result = self._run(plan, emit, cancel_event)
        record_outcome(plan, result)
        return result

    def _run(self, plan, emit, cancel_event):
        current_capture = 'runtimeCapture' in plan
        if current_capture:
            if cancel_event.is_set():
                return {'state': 'STOPPED', 'stopConfirmed': True}
            self._args(plan, pending=True)
            from tools.exhibition_handoff import collect
            try:
                plan = collect(plan, emit, cancel_event)
            except Exception:
                # Guard/Finish owns external cleanup. No speculative release/retry.
                return {'state': 'UNCONFIRMED_TERMINATION', 'stopConfirmed': False}
        args = self._args(plan)  # Recheck exact files, current expiry and all run/UID bindings.
        if cancel_event.is_set():
            return {'state': 'STOPPED', 'stopConfirmed': True}

        bundle = args.bound_bundle
        emit('observer', 'EVIDENCE_COLLECTED', {
            'phase': 'CURRENT_RUN_RUNTIME_EVIDENCE' if current_capture else 'BOUND_PRIOR_RUNTIME_EVIDENCE', 'sourceSHA256': args.bundle_sha256,
            'runID': bundle['incident']['run_id'], 'epoch': bundle['incident']['epoch'],
            'incidentUID': bundle['incident']['uid']})
        emit('coordinator', 'RUNTIME_LIMIT_CONFIRMED', {
            'phase': 'CURRENT_RUN_RUNTIME_EVIDENCE' if current_capture else 'BOUND_PRIOR_RUNTIME_EVIDENCE', 'state': 'EXHAUSTED',
            'attempt': len(bundle['runtime']['attempts']), 'serviceRecovered': False,
            'observedAt': bundle['runtime']['attempts'][-1]['completed_at']})

        def event(value):
            if (value['incident_run_id'] != plan['binding']['runID'] or value['epoch'] != plan['binding']['epoch']):
                raise Rejected('engineering event binding mismatch')
            detail = value['detail']
            public = {'attempt': detail['attempt']}
            def checks(items):
                return [{('httpStatus' if k == 'status' else 'inputSHA256' if k == 'fixture_sha256' else k): v
                         for k, v in c.items() if k in ('name', 'passed', 'status', 'fixture_sha256')} for c in items]
            if 'checks' in detail:
                public['checks'] = checks(detail['checks'])
            if 'comparison' in detail:
                compare = detail['comparison']
                public.update(baseline=checks(compare['baseline']), candidate=checks(compare['candidate']),
                              accepted=compare['same_input_confirmed'])
            emit(value['actor'], value['stage'], public)

        result = run_engineering(args, event_sink=event, cancel_event=cancel_event)
        state = result['state']
        if state == 'UNCONFIRMED_TERMINATION':
            return {'state': state, 'stopConfirmed': False}
        if state == 'VERIFIED_PENDING_REVIEW':
            require_verified_result(result,plan,args)
            emit('coordinator', 'VERIFIED_PENDING_REVIEW', {
                'sourceSHA256': result['candidate']['source_sha256'], 'patchSHA256': result['patch_sha256'],
                'deployed': False, 'serviceRecovered': False})
            return {'state': 'COMPLETED', 'stopConfirmed': True}
        if state == 'CANCELLED':
            return {'state': 'STOPPED', 'stopConfirmed': True}
        if state == 'ENGINEERING_FAILED':
            return {'state': 'FAILED', 'stopConfirmed': True}
        return {'state': 'UNCONFIRMED_TERMINATION', 'stopConfirmed': False}
