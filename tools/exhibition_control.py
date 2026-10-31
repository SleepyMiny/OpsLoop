"""Durable, single-run exhibition coordinator. No browser-supplied execution authority.

Only hash-pinned server-side plans can be prepared/consumed. A restarted active
run stays blocked until cleanup-only reconciliation proves its full lifecycle.
"""
from __future__ import annotations
import base64
import copy
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import threading
import time
import uuid


class ControlError(ValueError):
    pass


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)


def timestamp():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def instant(value):
    try:
        result = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
        if result.tzinfo is None:
            raise ValueError()
        return result.timestamp()
    except (ValueError, TypeError, AttributeError):
        raise ControlError('INVALID_DEADLINE') from None


def pinned(path, sha):
    path = Path(path)
    if not path.is_absolute() or path.is_symlink() or not re.fullmatch(r'[a-f0-9]{64}', sha):
        raise ControlError('INVALID_PIN')
    flags = os.O_RDONLY | os.O_NOFOLLOW
    fd = os.open(path, flags)
    try:
        st = os.fstat(fd)
        import stat
        if not stat.S_ISREG(st.st_mode) or st.st_size > 1024 * 1024 or st.st_uid != os.getuid() or st.st_mode & 0o022:
            raise ControlError('UNSAFE_PLAN_FILE')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            raw = stream.read(1024 * 1024 + 1)
    finally:
        os.close(fd)
    if hashlib.sha256(raw).hexdigest() != sha:
        raise ControlError('PLAN_CHANGED')
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ControlError('DUPLICATE_JSON_KEY')
            result[key] = value
        return result
    try:
        return json.loads(raw, object_pairs_hook=unique, parse_constant=lambda _: (_ for _ in ()).throw(ControlError('INVALID_JSON')))
    except (ValueError, UnicodeError):
        raise ControlError('INVALID_PLAN_JSON') from None


def identifier(value):
    return isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}', value) is not None


def validate_plan(plan, *, current=True):
    if not isinstance(plan, dict) or plan.get('schema') != 'opsloop-exhibition-plan/v1':
        raise ControlError('INVALID_PLAN')
    if any(not identifier(plan.get(k)) for k in ('planId', 'authorizationId')):
        raise ControlError('INVALID_PLAN_ID')
    if plan.get('source') != 'measured' or plan.get('track') not in ('runtime', 'engineering'):
        raise ControlError('INVALID_EXECUTION_SOURCE')
    binding = plan.get('binding')
    if not isinstance(binding, dict) or set(binding) != {'runID', 'epoch', 'policyUID', 'deploymentUID'} or not all(identifier(v) for v in binding.values()):
        raise ControlError('INVALID_BINDING')
    if not isinstance(plan.get('approvalReference'), str) or not plan['approvalReference'].strip():
        raise ControlError('APPROVAL_REQUIRED')
    if type(plan.get('totalSeconds')) is not int or not 1 <= plan['totalSeconds'] <= 900:
        raise ControlError('INVALID_EXECUTION_LIMIT')
    expires = instant(plan.get('expiresAt'))
    if current and expires <= time.time():
        raise ControlError('PLAN_EXPIRED')
    return plan


# Structured public facts only. Free text, subprocess output, model prompts and
# arbitrary exception messages never cross this boundary.
ENUM = re.compile(r'[A-Za-z0-9_.:/ -]{1,160}\Z')
FACTS = {'status', 'phase', 'reasonCode', 'node', 'pod', 'podUID', 'nodeUID', 'policyUID',
         'deploymentUID', 'incidentUID', 'actionID', 'selectedNode', 'epoch', 'runID',
         'httpStatus', 'observedAt', 'latencyMs', 'ok', 'ready', 'accepted', 'passed', 'name', 'attempt',
         'model', 'reasoningEffort', 'checksPassed', 'checksTotal', 'maxModelCalls',
         'sourceSHA256', 'patchSHA256', 'requests', 'stopConfirmed', 'measuredCPU',
         'appliedCPU', 'measuredMemory', 'appliedMemory', 'expiresAt', 'eligible',
         'scoreMilli', 'stableSamples', 'stableElapsedMS', 'durationSeconds', 'cpuUtilizationPermille', 'memAvailableBytes', 'memTotalBytes',
         'measuredCPUUtilizationPermille', 'appliedCPUUtilizationPermille',
         'measuredMemAvailableBytes', 'appliedMemAvailableBytes', 'baseline', 'candidate',
         'checks', 'candidates', 'nodeName', 'podUIDs', 'namespace', 'arch', 'measurements', 'appliedMeasurements', 'configSHA256', 'resourceCondition', 'rejectReasons', 'serviceRecovered',
         'deployed', 'prNumber', 'prURL', 'state', 'verification', 'inputSHA256',
         'baselineRestored', 'modelStopped', 'evidenceArchived', 'objectsCleaned', 'lifecycleComplete', 'lifecycleReceiptSHA256'}


def public_data(data, depth=0):
    if depth > 5:
        return None
    if isinstance(data, dict):
        return {k: public_data(v, depth + 1) for k, v in data.items() if k in FACTS}
    if isinstance(data, list):
        return [public_data(v, depth + 1) for v in data[:64]]
    if type(data) in (bool, int) or data is None:
        return data
    if isinstance(data, float):
        return data if abs(data) < 1e18 else None
    if isinstance(data, str) and ENUM.fullmatch(data) and not re.search(r'(?i)(bearer|token|password|secret|credential|/home/|/root/|\.kube|\.azure)', data):
        return data
    return None


class Coordinator:
    def __init__(self, state: Path, config: dict, adapters: dict, *, lifecycle=None):
        state.mkdir(mode=0o700, parents=True, exist_ok=True)
        if state.is_symlink() or state.stat().st_uid != os.getuid() or state.stat().st_mode & 0o077:
            raise ControlError('PRIVATE_STATE_REQUIRED')
        for name in ('owner.lock', 'ledger.sqlite3', 'ledger.sqlite3-journal'):
            if (state / name).is_symlink():
                raise ControlError('UNSAFE_STATE_FILE')
        self.lockfile = (state / 'owner.lock').open('a+')
        try:
            fcntl.flock(self.lockfile, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.lockfile.close()
            raise ControlError('COORDINATOR_ALREADY_RUNNING') from None
        self.mutex = threading.RLock()
        self.adapters = adapters
        if lifecycle is None:
            from tools.exhibition_lifecycle import Lifecycle
            lifecycle = Lifecycle(state / 'lifecycle')
        self.lifecycle = lifecycle
        self.pins = {}
        if config.get('schema') != 'opsloop-exhibition-control/v1' or not isinstance(config.get('plans'), list):
            raise ControlError('INVALID_CONTROL_CONFIG')
        for entry in config['plans']:
            plan = validate_plan(pinned(entry['path'], entry['sha256']), current=False)
            if plan['planId'] in self.pins:
                raise ControlError('DUPLICATE_PLAN')
            self.pins[plan['planId']] = entry
        self.plan_inbox = config.get('planInbox')
        self.inbox_error = None
        self.pin_origins = {}
        if self.plan_inbox is not None:
            if not isinstance(self.plan_inbox, dict) or set(self.plan_inbox) != {'path', 'publicKey'}:
                raise ControlError('INVALID_PLAN_INBOX')
            pointer = Path(self.plan_inbox['path'])
            try:
                key = base64.b64decode(self.plan_inbox['publicKey'], validate=True)
            except (ValueError, TypeError):
                raise ControlError('INVALID_PLAN_INBOX_KEY') from None
            if not pointer.is_absolute() or len(key) != 32:
                raise ControlError('INVALID_PLAN_INBOX')
        self.cleanup_pins = {}
        for entry in config.get('cleanupAuthorities', []):
            if not isinstance(entry, dict) or set(entry) != {'runId', 'path', 'sha256'} or not identifier(entry['runId']) or entry['runId'] in self.cleanup_pins:
                raise ControlError('INVALID_CLEANUP_CATALOG')
            cleanup = pinned(entry['path'], entry['sha256'])
            if cleanup.get('schema') != 'opsloop-exhibition-lifecycle/v1' or cleanup.get('binding', {}).get('runID') != entry['runId']:
                raise ControlError('CLEANUP_BINDING_MISMATCH')
            self.cleanup_pins[entry['runId']] = {k: entry[k] for k in ('path', 'sha256')}
        self.db = sqlite3.connect(state / 'ledger.sqlite3', check_same_thread=False, isolation_level=None)
        os.chmod(state / 'ledger.sqlite3', 0o600)
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.execute('CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, request TEXT UNIQUE, plan TEXT UNIQUE, authority TEXT UNIQUE, binding TEXT UNIQUE, value TEXT)')
        self.db.execute('CREATE TABLE IF NOT EXISTS events (run TEXT, seq INTEGER, value TEXT, PRIMARY KEY(run,seq))')
        self.db.execute('CREATE TABLE IF NOT EXISTS plan_pins (id TEXT PRIMARY KEY, path TEXT, sha256 TEXT, origin TEXT, public_key TEXT)')
        configured = dict(self.pins)
        self.startup_plan_ids = set(configured)
        for key, path, checksum, origin, public_key in self.db.execute('SELECT id,path,sha256,origin,public_key FROM plan_pins').fetchall():
            ref = {'path': path, 'sha256': checksum}
            if key in configured and any(configured[key].get(k) != ref[k] for k in ref):
                raise ControlError('PLAN_PIN_CONFLICT')
            plan = validate_plan(pinned(path, checksum), current=False)
            if plan['planId'] != key:
                raise ControlError('PLAN_PIN_CONFLICT')
            if origin == 'inbox':
                self._private_inbox_ref(path)
                self._verify_inbox_plan(plan, public_key)
            elif origin != 'startup':
                raise ControlError('PLAN_PIN_ORIGIN_INVALID')
            self.pins[key] = ref
            self.pin_origins[key] = (origin, public_key)
        for key, ref in configured.items():
            if key not in self.pin_origins:
                self._remember_pin(key, ref, 'startup', '')

        self.prepared = set()
        self.cancel = None
        self.worker = None
        for row in self.db.execute('SELECT id,value FROM runs').fetchall():
            value = json.loads(row[1])
            if not value.get('lifecycleComplete', False):
                value.setdefault('executionState', value['state'])
                value['state'] = 'RECONCILIATION_REQUIRED' if value.get('stopConfirmed') else 'UNCONFIRMED_TERMINATION'
                value['lifecycleComplete'] = False
                value.setdefault('lifecycle', {'stopConfirmed': bool(value.get('stopConfirmed')), 'baselineRestored': False, 'modelStopped': False, 'evidenceArchived': False, 'objectsCleaned': False, 'reasonCode': 'RESTART_RECONCILIATION_REQUIRED'})
                self._save(value)
                self.emit(row[0], 'coordinator', 'RESTART_RECONCILIATION_REQUIRED', {})

    def _remember_pin(self, plan_id, ref, origin, public_key):
        ref = {k: ref[k] for k in ('path', 'sha256')}
        if plan_id in self.pins and plan_id in self.pin_origins:
            if self.pins[plan_id] != ref or self.pin_origins[plan_id] != (origin, public_key):
                raise ControlError('PLAN_PIN_CONFLICT')
            return
        self.db.execute('INSERT INTO plan_pins VALUES (?,?,?,?,?)', (plan_id, ref['path'], ref['sha256'], origin, public_key))
        self.pins[plan_id] = ref
        self.pin_origins[plan_id] = (origin, public_key)

    @staticmethod
    def _private_inbox_ref(path):
        file = Path(path)
        if not file.is_absolute() or any(p.is_symlink() for p in (file, *file.parents)):
            raise ControlError('PLAN_INBOX_UNSAFE_REFERENCE')
        info = file.stat(follow_symlinks=False); parent = file.parent.stat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600
                or parent.st_uid != os.getuid() or parent.st_mode & 0o077):
            raise ControlError('PLAN_INBOX_PRIVATE_REFERENCE_REQUIRED')

    @staticmethod
    def _verify_inbox_plan(plan, public_key):
        if plan.get('exhibitionPermit', {}).get('publicKey') != public_key:
            raise ControlError('PLAN_INBOX_UNTRUSTED_KEY')
        Coordinator._private_inbox_ref(plan['exhibitionPermit']['path'])
        from tools.exhibition_permit import bound_permit
        bound_permit(plan, current=False, model=plan['track'] == 'engineering')

    def _read_inbox_pointer(self):
        path = Path(self.plan_inbox['path'])
        if any(p.is_symlink() for p in (path, *path.parents)):
            raise ControlError('PLAN_INBOX_SYMLINK')
        parent = path.parent.stat()
        if parent.st_uid != os.getuid() or parent.st_mode & 0o077:
            raise ControlError('PLAN_INBOX_PRIVATE_DIRECTORY_REQUIRED')
        try:
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        except FileNotFoundError:
            return None
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4096:
                raise ControlError('PLAN_INBOX_PRIVATE_FILE_REQUIRED')
            with os.fdopen(fd, 'rb', closefd=False) as stream:
                raw = stream.read(4097)
        finally:
            os.close(fd)
        if len(raw) > 4096:
            raise ControlError('PLAN_INBOX_SIZE_LIMIT')
        def unique(pairs):
            value = {}
            for key, item in pairs:
                if key in value:
                    raise ControlError('PLAN_INBOX_DUPLICATE_FIELD')
                value[key] = item
            return value
        try:
            ref = json.loads(raw, object_pairs_hook=unique)
        except (ValueError, UnicodeError):
            raise ControlError('PLAN_INBOX_INVALID_JSON') from None
        if not isinstance(ref, dict) or set(ref) != {'path', 'sha256'}:
            raise ControlError('PLAN_INBOX_INVALID_POINTER')
        return ref

    def _refresh_inbox(self, *, strict=False):
        if self.plan_inbox is None or (self.worker and self.worker.is_alive()) or any(not r.get('lifecycleComplete', False) for r in self._runs()):
            return
        try:
            ref = self._read_inbox_pointer()
            if ref is not None:
                self._private_inbox_ref(ref['path'])
                plan = validate_plan(pinned(ref['path'], ref['sha256']), current=False)
                self._verify_inbox_plan(plan, self.plan_inbox['publicKey'])
                self._remember_pin(plan['planId'], ref, 'inbox', self.plan_inbox['publicKey'])
            self.inbox_error = None
        except Exception as exc:
            self.inbox_error = str(exc) if isinstance(exc, ControlError) else 'PLAN_INBOX_REJECTED'
            if strict:
                raise ControlError(self.inbox_error) from None

    def _plan(self, plan_id, *, current=True):
        if plan_id not in self.pins:
            raise ControlError('UNKNOWN_PLAN')
        entry = self.pins[plan_id]
        origin, key = self.pin_origins.get(plan_id, ('startup', ''))
        if current and origin == 'startup' and plan_id not in self.startup_plan_ids:
            raise ControlError('PLAN_NOT_CURRENTLY_APPROVED')
        if current and origin == 'inbox' and (self.plan_inbox is None or self.plan_inbox['publicKey'] != key):
            raise ControlError('PLAN_INBOX_TRUST_CHANGED')
        if current and origin == 'inbox':
            # A durable pin retains cleanup history, not a withdrawn execution approval.
            # Recheck here as start may follow prepare after the host changes the pointer.
            if self._read_inbox_pointer() != entry:
                raise ControlError('PLAN_INBOX_APPROVAL_WITHDRAWN')
            self._private_inbox_ref(entry['path'])
        plan = validate_plan(pinned(entry['path'], entry['sha256']), current=current)
        if current and origin == 'inbox':
            self._verify_inbox_plan(plan, key)
        return plan

    def _consumed(self, plan):
        return any(r['planId'] == plan['planId'] or r['binding']['runID'] == plan['binding']['runID']
                   or r['binding']['epoch'] == plan['binding']['epoch'] for r in self._runs()) or bool(
                   self.db.execute('SELECT 1 FROM runs WHERE authority=?', (plan['authorizationId'],)).fetchone())

    def _save(self, run):
        self.db.execute('UPDATE runs SET value=? WHERE id=?', (canonical(run), run['runId']))

    def _runs(self):
        return [json.loads(r[0]) for r in self.db.execute('SELECT value FROM runs ORDER BY rowid DESC')]

    def _ready(self, run):
        if not run.get('lifecycleComplete', False):
            return False
        try:
            self.lifecycle.verify_run(run)
            return True
        except Exception:
            run['lifecycleComplete'] = False
            run['state'] = 'RECONCILIATION_REQUIRED'
            run['lifecycle']['reasonCode'] = 'ARCHIVE_INTEGRITY_RECONCILIATION_REQUIRED'
            self._save(run)
            return False

    def _idle(self):
        if any(not self._ready(r) for r in self._runs()):
            raise ControlError('ACTIVE_OR_UNCONFIRMED_RUN')

    def state(self):
        with self.mutex:
            runs = self._runs()
            for run in runs:
                self._ready(run)
            self._refresh_inbox()
            plans = []
            for key in self.pins:
                try:
                    p = self._plan(key)
                except (ControlError, OSError):
                    continue
                if self._consumed(p):
                    continue
                item = {k: p[k] for k in ('planId', 'track', 'source', 'binding', 'expiresAt', 'totalSeconds')}
                item['ready'] = key in self.prepared
                if p['track'] == 'engineering':
                    item.update({k: p.get(k) for k in ('model', 'maxModelCalls')})
                plans.append(item)
            return {'active': next((r for r in runs if not r.get('lifecycleComplete', False)), None), 'runs': runs, 'plans': plans,
                    'nextRunAllowed': all(r.get('lifecycleComplete', False) for r in runs), 'planInboxError': self.inbox_error}

    def prepare(self, plan_id):
        with self.mutex:
            self._idle()
            self._refresh_inbox(strict=True)
            plan = self._plan(plan_id)
            if self._consumed(plan):
                raise ControlError('AUTHORITY_CONSUMED')
            self.lifecycle.config(plan)  # Cleanup authority must exist before any experiment.
            self.adapters[plan['track']].prepare(copy.deepcopy(plan))
            self.prepared.add(plan_id)
        return {'prepared': plan_id}

    def start(self, plan_id, request_id):
        if not identifier(request_id):
            raise ControlError('INVALID_REQUEST_ID')
        with self.mutex:
            previous = self.db.execute('SELECT value FROM runs WHERE request=?', (request_id,)).fetchone()
            if previous:
                run = json.loads(previous[0])
                if run['planId'] != plan_id:
                    raise ControlError('REQUEST_CONFLICT')
                return run
            self._idle()
            plan = self._plan(plan_id)
            if self._consumed(plan):
                raise ControlError('AUTHORITY_CONSUMED')
            if plan_id not in self.prepared:
                raise ControlError('PREPARE_REQUIRED')
            replay=None
            if 'exhibitionPermit' in plan:
                from tools.exhibition_permit import bound_permit
                _,verified=bound_permit(plan,model=plan['track']=='engineering')
                replay=verified.get('circuitReplay')
            run = {'runId': plan['binding']['runID'], 'planId': plan_id, 'track': plan['track'],
                   'state': 'RUNNING', 'source': plan['source'], 'binding': plan['binding'],
                   'lastSeq': 0, 'stopConfirmed': False, 'lifecycleComplete': False,
                   'lifecycle': {'stopConfirmed': False, 'baselineRestored': False, 'modelStopped': False,
                                 'evidenceArchived': False, 'objectsCleaned': False, 'reasonCode': 'EXECUTION_ACTIVE'}}
            try:
                self.db.execute('INSERT INTO runs VALUES (?,?,?,?,?,?)', (run['runId'], request_id, plan_id,
                    plan['authorizationId'], plan['binding']['runID'] + ':' + plan['binding']['epoch'], canonical(run)))
            except sqlite3.IntegrityError:
                raise ControlError('AUTHORITY_CONSUMED') from None
            self.prepared.discard(plan_id)
            self.cancel = threading.Event()
            self.emit(run['runId'], 'coordinator', 'RUN_STARTED', {})
            if replay is not None:
                self.emit(run['runId'],'operator','EXHIBITION_REPLAY_APPROVED',{'phase':'ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION','sourceSHA256':replay['approvalSHA256'],'reasonCode':'NOT_OPERATIONAL_CIRCUIT_RECOVERY'})
            self.worker = threading.Thread(target=self._execute, args=(plan, run, self.cancel), daemon=True)
            self.worker.start()
            return run

    def _execute(self, plan, run, cancel):
        timer = threading.Timer(min(plan['totalSeconds'], max(0, instant(plan['expiresAt']) - time.time())), cancel.set)
        timer.daemon = True
        timer.start()
        try:
            result = self.adapters[plan['track']].run(copy.deepcopy(plan),
                lambda actor, kind, data: self.emit(run['runId'], actor, kind, data), cancel)
            if result.get('state') not in ('COMPLETED', 'FAILED', 'STOPPED', 'UNCONFIRMED_TERMINATION') or type(result.get('stopConfirmed')) is not bool:
                raise ControlError('INVALID_TERMINATION_RECEIPT')
            if not result['stopConfirmed']:
                result = {'state': 'UNCONFIRMED_TERMINATION', 'stopConfirmed': False}
        except Exception:
            # Never turn an exception into permission to run another experiment.
            result = {'state': 'UNCONFIRMED_TERMINATION', 'stopConfirmed': False}
        finally:
            timer.cancel()
        with self.mutex:
            current = json.loads(self.db.execute('SELECT value FROM runs WHERE id=?', (run['runId'],)).fetchone()[0])
            current.update({k: result[k] for k in ('state', 'stopConfirmed')})
            self._save(current)
            current['executionState'] = current['state']
            current['lifecycle']['stopConfirmed'] = current['stopConfirmed']
            self._save(current)
            self.emit(run['runId'], 'coordinator', current['state'], {'stopConfirmed': current['stopConfirmed']})
        if result['stopConfirmed']:
            self._reconcile_run(plan, run['runId'])

    def _reconcile_run(self, plan, run_id):
        def emit(actor, kind, data):
            self.emit(run_id, actor, kind, data)
        with self.mutex:
            run = json.loads(self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()[0])
            run['state'] = 'RECONCILING'
            run['lifecycle']['reasonCode'] = 'CLEANUP_IN_PROGRESS'
            self._save(run)
        try:
            # Current cleanup authority is distinct from expired execution authority.
            self.lifecycle.config(plan)
            adapter = self.adapters[plan['track']]
            if not hasattr(adapter, 'reconcile'):
                raise ControlError('CLEANUP_ADAPTER_REQUIRED')
            receipt = adapter.reconcile(copy.deepcopy(plan), emit)
            result = self.lifecycle.reconcile(plan, receipt, emit)
            flags = ('stopConfirmed', 'baselineRestored', 'modelStopped', 'evidenceArchived', 'objectsCleaned')
            if result.get('binding') != plan['binding'] or not all(result.get(k) is True for k in flags) or not re.fullmatch(r'[a-f0-9]{64}', result.get('lifecycleReceiptSHA256', '')):
                raise ControlError('LIFECYCLE_COMPLETION_INVALID')
            with self.mutex:
                run = json.loads(self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()[0])
                run.update(stopConfirmed=True, lifecycleComplete=True, lifecycleReceiptSHA256=result['lifecycleReceiptSHA256'])
                run['lifecycle'] = {**{k: True for k in flags}, 'reasonCode': 'LIFECYCLE_COMPLETE'}
                run['state'] = run.get('executionState', 'STOPPED')
                if run['state'] in ('RUNNING', 'STOPPING', 'UNCONFIRMED_TERMINATION', 'RECONCILING', 'RECONCILIATION_REQUIRED'):
                    run['state'] = 'STOPPED'
                self._save(run)
                emit('coordinator', 'NEXT_RUN_ALLOWED', {'lifecycleComplete': True, 'lifecycleReceiptSHA256': result['lifecycleReceiptSHA256']})
        except Exception as exc:
            with self.mutex:
                run = json.loads(self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()[0])
                run['state'] = 'RECONCILIATION_REQUIRED'
                run['lifecycleComplete'] = False
                run['lifecycle']['reasonCode'] = str(exc) if isinstance(exc, ControlError) else 'CLEANUP_UNCONFIRMED'
                self._save(run)
                emit('coordinator', 'RECONCILIATION_REQUIRED', {'reasonCode': run['lifecycle']['reasonCode']})

    def reconcile(self, run_id):
        with self.mutex:
            row = self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()
            if not row:
                raise ControlError('UNKNOWN_RUN')
            run = json.loads(row[0])
            if run.get('lifecycleComplete'):
                return run
            if self.worker and self.worker.is_alive():
                raise ControlError('EXECUTION_OR_CLEANUP_ACTIVE')
            plan = self._plan(run['planId'], current=False)
            if run_id in self.cleanup_pins:
                plan = copy.deepcopy(plan)
                plan['lifecycle'] = self.cleanup_pins[run_id]
            self.lifecycle.config(plan)
            run['state'] = 'RECONCILING'
            self._save(run)
            self.worker = threading.Thread(target=self._reconcile_run, args=(plan, run_id), daemon=True)
            self.worker.start()
            return run

    def stop(self, run_id):
        with self.mutex:
            row = self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()
            if not row:
                raise ControlError('UNKNOWN_RUN')
            run = json.loads(row[0])
            if run['stopConfirmed']:
                return run
            if run['state'] == 'UNCONFIRMED_TERMINATION':
                raise ControlError('EXTERNAL_RECONCILIATION_REQUIRED')
            run['state'] = 'STOPPING'
            self._save(run)
            self.cancel.set()
            self.emit(run_id, 'operator', 'STOP_REQUESTED', {})
            return run

    def emit(self, run_id, actor, kind, data):
        if actor not in ('operator', 'coordinator', 'model', 'verifier', 'controller', 'scheduler', 'guard', 'observer', 'workload') or not re.fullmatch(r'[A-Z_]{1,64}', kind):
            raise ControlError('INVALID_PUBLIC_EVENT')
        with self.mutex:
            seq = self.db.execute('SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE run=?', (run_id,)).fetchone()[0]
            row = self.db.execute('SELECT value FROM runs WHERE id=?', (run_id,)).fetchone()
            if not row:
                raise ControlError('UNKNOWN_RUN')
            run = json.loads(row[0])
            event = {'runId': run_id, 'seq': seq, 'at': timestamp(), 'actor': actor, 'kind': kind,
                     'source': run['source'], 'data': public_data(data)}
            self.db.execute('BEGIN IMMEDIATE')
            try:
                self.db.execute('INSERT INTO events VALUES (?,?,?)', (run_id, seq, canonical(event)))
                run['lastSeq'] = seq
                self._save(run)
                self.db.execute('COMMIT')
            except Exception:
                self.db.execute('ROLLBACK')
                raise

    def events(self, run_id, after=0):
        if not identifier(run_id) or type(after) is not int or after < 0:
            raise ControlError('INVALID_CURSOR')
        with self.mutex:
            return {'events': [json.loads(r[0]) for r in self.db.execute('SELECT value FROM events WHERE run=? AND seq>? ORDER BY seq LIMIT 200', (run_id, after))]}

    def close(self):
        if self.cancel:
            self.cancel.set()
        if self.worker:
            self.worker.join(timeout=5)
            if self.worker.is_alive():
                return  # Keep ownership and fail closed; Guard remains responsible for bounded cleanup.
        with self.mutex:
            self.db.close()
            self.lockfile.close()
