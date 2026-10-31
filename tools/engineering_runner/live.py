"""Bounded public projection. Private provider output is never an event payload."""
from __future__ import annotations

import datetime as dt
import json
import os
from pathlib import Path
import re

from .contracts import Rejected, canonical, digest, regular_file, timestamp, _unique_object

CHECKS = frozenset({'source-security', 'unit', 'static', 'build-amd64', 'cross-build-arm64',
                   'startup', 'empty-order-regression', 'normal-order', 'zero-price',
                   'fraction-truncation', 'negative-price', 'zero-quantity', 'malformed-json',
                   'oversize-request', 'health-after-reproduction'})
STAGES = {'VALIDATING_INPUT': 'coordinator', 'BASELINE_VERIFYING': 'verifier',
          'BASELINE_CONFIRMED': 'verifier', 'ANALYZING': 'coordinator',
          'MODEL_REQUESTED': 'coordinator', 'PROPOSAL_RECEIVED': 'model', 'PATCH_APPLIED': 'coordinator', 'VERIFYING': 'verifier',
          'VERIFICATION_RESULT': 'verifier', 'REVISION_REQUESTED': 'coordinator',
          'VERIFIED_PENDING_REVIEW': 'coordinator', 'ENGINEERING_FAILED': 'coordinator',
          'CANCELLED': 'coordinator', 'UNCONFIRMED_TERMINATION': 'coordinator'}


def execution_plan(args, bundle):
    """A server-owned exact file plus external digest is required to expand defaults."""
    path = getattr(args, 'execution_plan', None)
    if path is None:
        return {'max_model_calls': 1, 'total_seconds': 900, 'model': args.model,
                'reasoning_effort': args.reasoning_effort, 'expires_at': bundle['authorization']['expires_at']}
    raw = regular_file(Path(path), 16384)
    if digest(raw) != getattr(args, 'execution_plan_sha256', None):
        raise Rejected('execution plan digest mismatch')
    try:
        p = json.loads(raw, object_pairs_hook=_unique_object)
        expected = {'schema', 'bundle_sha256', 'run_id', 'epoch', 'policy_uid', 'deployment_uid',
                    'model', 'reasoning_effort', 'max_model_calls', 'total_seconds', 'expires_at'}
        is_exhibition = p.get('schema') == 'opsloop-engineering-plan/v2'
        if set(p) != expected | ({'exhibition'} if is_exhibition else set()) or p['schema'] not in ('opsloop-engineering-plan/v1','opsloop-engineering-plan/v2'):
            raise Rejected('execution plan schema mismatch')
        bindings = {'bundle_sha256': args.bundle_sha256, 'run_id': bundle['incident']['run_id'],
                    'epoch': bundle['incident']['epoch'], 'policy_uid': bundle['workload']['policy_uid'],
                    'deployment_uid': bundle['workload']['deployment_uid'], 'model': args.model,
                    'reasoning_effort': args.reasoning_effort}
        if any(p[k] != v for k, v in bindings.items()):
            raise Rejected('execution plan binding mismatch')
        if (type(p['max_model_calls']) is not int or not 1 <= p['max_model_calls'] <= 3
                or type(p['total_seconds']) is not int or not 1 <= p['total_seconds'] <= 900):
            raise Rejected('execution plan budget outside trusted cap')
        if timestamp(p['expires_at']) > timestamp(bundle['authorization']['expires_at']):
            raise Rejected('execution plan outlives authorization')
        if timestamp(p['expires_at']) <= dt.datetime.now(dt.timezone.utc):
            raise Rejected('execution plan expired')
        if is_exhibition:
            exhibition_authority(p, bundle)
        return p
    except (KeyError, TypeError, ValueError) as exc:
        raise Rejected('execution plan rejected') from exc


def exhibition_authority(plan, bundle):
    """A signed deliberate exhibition cannot authorize an automatic operational retry."""
    from tools.exhibition_permit import verify
    from tools.exhibition_control import pinned, ControlError, instant
    try:
        e=plan['exhibition']
        if set(e)!={'authorization_id','permit','previous_completion_sha256'} or set(e['permit'])!={'path','sha256','publicKey'}:
            raise Rejected('exhibition authority schema mismatch')
        config=e['permit']; envelope=pinned(config['path'],config['sha256'])
        authority=verify(envelope,config['publicKey'],model=True)
        scope=authority['grant']['scope']; budget=authority['modelBudget']
        expected={'runID':bundle['incident']['run_id'],'epoch':bundle['incident']['epoch'],
                  'policyUID':bundle['workload']['policy_uid'],'deploymentUID':bundle['workload']['deployment_uid']}
        if any(scope[k]!=v for k,v in expected.items()) or e['authorization_id']!=authority['authorizationID'] or e['previous_completion_sha256']!=authority['previousCompletionSHA256']:
            raise Rejected('exhibition authority binding mismatch')
        if budget['model']!=plan['model'] or budget['reasoningEffort']!=plan['reasoning_effort'] or budget['maxModelCalls']!=plan['max_model_calls'] or budget['totalSeconds']!=plan['total_seconds'] or instant(budget['expiresAt'])!=instant(plan['expires_at']):
            raise Rejected('exhibition model budget changed')
        for a in bundle['runtime']['attempts']:
            if not instant(authority['grant']['notBefore']) <= instant(a['patched_at']) <= instant(a['completed_at']) <= instant(authority['grant']['notAfter']):
                raise Rejected('exhibition evidence outside signed run window')
        return authority
    except (ControlError,KeyError,TypeError,ValueError) as exc:
        raise Rejected('signed exhibition authority rejected') from exc


def public_checks(verification):
    """Never forward verifier stdout, arbitrary response bodies or exception strings."""
    out = []
    for c in verification.get('report', {}).get('checks', []):
        if c.get('name') not in CHECKS or type(c.get('passed')) is not bool:
            continue
        item = {'name': c['name'], 'passed': c['passed']}
        if type(c.get('status')) is int and 100 <= c['status'] <= 599:
            item['status'] = c['status']
        for field in ('fixture_sha256', 'output_sha256'):
            if isinstance(c.get(field), str) and re.fullmatch('[0-9a-f]{64}', c[field]):
                item[field] = c[field]
        out.append(item)
    return out


def comparison(baseline, candidate):
    before = {c['name']: c for c in public_checks(baseline)}
    after = {c['name']: c for c in public_checks(candidate)}
    a, b = before.get('empty-order-regression', {}), after.get('empty-order-regression', {})
    same = bool(a.get('fixture_sha256')) and a.get('fixture_sha256') == b.get('fixture_sha256')
    return {'same_input_confirmed': same, 'baseline': list(before.values()), 'candidate': list(after.values())}


class PublicEvents:
    def __init__(self, work, run_id, bundle, sink=None):
        self.path = work / 'public-events.jsonl'
        self.run_id = run_id
        self.binding = {k: bundle['incident'][k] for k in ('run_id', 'epoch')}
        self.sequence = 0
        self.sink = sink

    def emit(self, stage, *, attempt=0, verification=None, compare=None):
        if stage not in STAGES or type(attempt) is not int or not 0 <= attempt <= 3:
            raise Rejected('public event contract rejected')
        detail = {'attempt': attempt}
        if verification is not None:
            detail['checks'] = public_checks(verification)
        if compare is not None:
            # Comparison is derived here, not accepted as a freeform data envelope.
            detail['comparison'] = comparison(*compare)
        self.sequence += 1
        event = {'schema': 'opsloop-engineering-event/v1', 'run_id': self.run_id,
                 'incident_run_id': self.binding['run_id'], 'epoch': self.binding['epoch'],
                 'sequence': self.sequence, 'at': dt.datetime.now(dt.timezone.utc).isoformat(),
                 'actor': STAGES[stage], 'stage': stage, 'detail': detail}
        fd = os.open(self.path, os.O_CREAT | os.O_APPEND | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'ab') as f:
            f.write(canonical(event)); f.flush(); os.fsync(f.fileno())
        if self.sink:
            self.sink(json.loads(canonical(event)))
        return event
