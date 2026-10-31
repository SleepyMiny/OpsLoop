"""Cleanup-only exhibition lifecycle. No execution, grant issuance or Policy deletion.

The host pins cleanup authority; adapter receipts are never HTTP request fields.
A durable archive precedes exact UID/resourceVersion deletion. Ambiguous writes
remain blocked until a later cleanup-only reconciliation observes their result.
"""
from __future__ import annotations
import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import time
import urllib.error
import urllib.request
import uuid

from tools.exhibition_control import ControlError, canonical, identifier, instant, pinned
from tools.exhibition_runtime import Transport
from tools.exhibition_deletion import pending_record, wait_deleted, confirmed_deletion
from tools.exhibition_hashes import bound_pin, matches as hash_matches, verify as verify_hash, describe as describe_hash, encoded as object_bytes

RUN = 'opsloop.io/exhibition-run'
EPOCH = 'opsloop.io/exhibition-epoch'
KINDS = {'RecoveryIncident': '/apis/opsloop.io/v1alpha1/namespaces/{ns}/recoveryincidents',
         'ConfigMap': '/api/v1/namespaces/{ns}/configmaps',
         'Job': '/apis/batch/v1/namespaces/{ns}/jobs'}
TERMINAL = {'RECOVERED', 'FAILED', 'ABORTED', 'SUPERSEDED', 'UNSUPPORTED_WORKLOAD', 'OBSERVED'}
SHA = re.compile(r'[a-f0-9]{64}\Z')
NAME = re.compile(r'[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?\Z')


def require(ok, code):
    if not ok:
        raise ControlError(code)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def private_dir(path):
    p = Path(path)
    require(p.is_absolute() and not any(x.is_symlink() for x in (p, *p.parents)), 'UNSAFE_LIFECYCLE_DIRECTORY')
    p.mkdir(mode=0o700, parents=True, exist_ok=True)
    require(p.stat().st_uid == os.getuid() and not p.stat().st_mode & 0o077, 'PRIVATE_LIFECYCLE_DIRECTORY_REQUIRED')
    return p


def write(path, raw, *, exclusive=False):
    path = Path(path)
    require(not path.is_symlink(), 'UNSAFE_LIFECYCLE_FILE')
    if exclusive:
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'wb') as f:
            f.write(raw); f.flush(); os.fsync(f.fileno())
    else:
        temp = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
        with os.fdopen(os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as f:
            f.write(raw); f.flush(); os.fsync(f.fileno())
        os.replace(temp, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def safe_bytes(path, root):
    path = Path(path)
    require(path.is_absolute() and path.is_relative_to(root) and path != root,
            'EVIDENCE_OUTSIDE_RUN_ROOT')
    require(not any(p.is_symlink() for p in (path, *path.parents)), 'EVIDENCE_SYMLINK')
    require(not re.search(r'(?i)(kubeconfig|(^|/)(auth|credentials|secret)\.(json|ya?ml)|\.pem$|id_rsa)', str(path.relative_to(root))), 'PRIVATE_AUTH_EVIDENCE_REJECTED')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        st = os.fstat(fd)
        require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and st.st_size <= 16*1024*1024,
                'EVIDENCE_FILE_REJECTED')
        with os.fdopen(fd, 'rb', closefd=False) as f:
            raw = f.read(16*1024*1024+1)
    finally:
        os.close(fd)
    require(len(raw) <= 16*1024*1024, 'EVIDENCE_SIZE_LIMIT')
    # Credential-bearing documents must never be archived even in a private run.
    require(not re.search(rb'(?i)(-----BEGIN [A-Z ]*PRIVATE KEY|\bBearer [A-Za-z0-9._~-]+|"(?:token|secret|access_token|refresh_token|client-key-data|password)"\s*:)', raw), 'CREDENTIAL_EVIDENCE_REJECTED')
    return raw


def patched(document, ops):
    value = copy.deepcopy(document)
    for op in ops:
        parts = [p.replace('~1', '/').replace('~0', '~') for p in op['path'].split('/')[1:]]
        require(parts and all(p not in ('', '.', '..') for p in parts), 'RESTORE_POINTER_INVALID')
        target = value
        try:
            for key in parts[:-1]:
                target = target[int(key)] if isinstance(target, list) else target[key]
            key = int(parts[-1]) if isinstance(target, list) else parts[-1]
            if op['op'] == 'test':
                require(target[key] == op.get('value'), 'RESTORE_TEST_FAILED')
            elif op['op'] == 'remove':
                del target[key]
            elif op['op'] == 'add' and isinstance(target, list):
                require(0 <= key <= len(target), 'RESTORE_POINTER_INVALID')
                target.insert(key, copy.deepcopy(op['value']))
            else:
                if op['op'] == 'replace':
                    require(key in target if isinstance(target, dict) else 0 <= key < len(target), 'RESTORE_POINTER_INVALID')
                target[key] = copy.deepcopy(op['value'])
        except (KeyError, IndexError, ValueError, TypeError):
            raise ControlError('RESTORE_POINTER_INVALID') from None
    return value


def verify_completion(path, expected_sha256):
    """Recheck immutable archive before host issuer mints the next run permit."""
    result = pinned(str(path), expected_sha256)
    require(result.get('schema') in ('opsloop-exhibition-completion/v1', 'opsloop-exhibition-migration-completion/v1') and result.get('nextRunAllowed') is True,
            'COMPLETION_INVALID')
    require(all(result.get(k) is True for k in ('stopConfirmed', 'baselineRestored', 'modelStopped', 'evidenceArchived', 'objectsCleaned')), 'COMPLETION_INVALID')
    manifest_path = Path(result['archiveManifest'])
    manifest = pinned(str(manifest_path), result['archiveSHA256'])
    require(manifest.get('binding') == result.get('binding'), 'ARCHIVE_BINDING_MISMATCH')
    if result['schema'] == 'opsloop-exhibition-migration-completion/v1':
        require(manifest.get('schema') == 'opsloop-exhibition-migration-archive/v1'
                and manifest.get('migrationID') == result.get('migrationID')
                and manifest.get('planSHA256') == result.get('planSHA256'), 'MIGRATION_ARCHIVE_BINDING_MISMATCH')
    else:
        require(manifest.get('schema') == 'opsloop-exhibition-archive/v1', 'ARCHIVE_SCHEMA_MISMATCH')
    for item in [*manifest['files'], *manifest['objects']]:
        blob = manifest_path.parent / item['sha256']
        require(SHA.fullmatch(item['sha256']) and not blob.is_symlink() and sha(blob.read_bytes()) == item['sha256'], 'ARCHIVE_INTEGRITY_FAILED')
    if 'executionPlanSHA256' in result:
        require(SHA.fullmatch(result['executionPlanSHA256']) and len(result.get('deletionConfirmations',[]))==len(manifest['objects']),'MIGRATION_CONTINUATION_PROOF_REQUIRED')
        for ref,record in zip(result['deletionConfirmations'],manifest['objects']):
            file=Path(ref['path']);require(file.name.endswith('.confirmed.json'),'DELETE_CONFIRMATION_CHANGED')
            marker=file.with_name(file.name[:-len('.confirmed.json')]+'.json')
            identity=record['identity'];target=KINDS[identity['kind']].format(ns=result['binding']['namespace'])+'/'+identity['name']
            require(confirmed_deletion(marker,identity,target)==ref,'DELETE_CONFIRMATION_CHANGED')
    authority = manifest_path.parent / 'authority-retirement.json'
    require(not authority.is_symlink() and sha(authority.read_bytes()) == result['retiredAuthoritySHA256'], 'ARCHIVE_AUTHORITY_CHANGED')
    return result


class CleanupTransport(Transport):
    def delete(self, path, uid, resource_version):
        require(any(re.fullmatch(re.escape(v).replace(r'\{ns\}', r'[a-z0-9][a-z0-9.-]*') + r'/[a-z0-9][a-z0-9.-]*', path) for v in KINDS.values()), 'CLEANUP_PATH_REJECTED')
        require(isinstance(uid, str) and uid and isinstance(resource_version, str) and resource_version, 'DELETE_PRECONDITIONS_REQUIRED')
        body = {'apiVersion': 'v1', 'kind': 'DeleteOptions',
                'preconditions': {'uid': uid, 'resourceVersion': resource_version},
                'propagationPolicy': 'Foreground'}
        headers = {'Accept': 'application/json', 'Content-Type': 'application/json'}
        if self.token:
            headers['Authorization'] = 'Bearer ' + self.token
        req = urllib.request.Request(self.origin + path, data=canonical(body).encode(), headers=headers, method='DELETE')
        try:
            with self.opener.open(req, timeout=2) as response:
                response.read(1048577)
        except (OSError, ValueError, urllib.error.URLError):
            raise ControlError('DELETE_UNCONFIRMED') from None


class Lifecycle:
    def __init__(self, state, transport_factory=CleanupTransport):
        self.state = private_dir(state)
        self.transport_factory = transport_factory

    def verify_run(self, run):
        result = verify_completion(self.state / run['runId'] / 'completion.json', run['lifecycleReceiptSHA256'])
        require(result['binding'] == run['binding'], 'COMPLETION_BINDING_MISMATCH')
        return result

    def config(self, plan):
        ref = plan.get('lifecycle')
        require(isinstance(ref, dict) and set(ref) == {'path', 'sha256'}, 'CLEANUP_AUTHORITY_REQUIRED')
        cfg = pinned(ref['path'], ref['sha256'])
        require(cfg.get('schema') == 'opsloop-exhibition-lifecycle/v1' and cfg.get('binding') == plan['binding'], 'CLEANUP_BINDING_MISMATCH')
        require(set(plan['binding']) == {'runID', 'epoch', 'policyUID', 'deploymentUID'} and all(identifier(v) for v in plan['binding'].values()), 'CLEANUP_BINDING_INVALID')
        require(instant(cfg.get('expiresAt')) > time.time(), 'CLEANUP_AUTHORITY_EXPIRED')
        for name in ('namespace', 'policy', 'deployment'):
            require(isinstance(cfg.get(name), str) and NAME.fullmatch(cfg[name]), 'CLEANUP_TARGET_INVALID')
        bound_pin(cfg, 'baselineSpec', fields='/spec')
        require(isinstance(cfg.get('allowedRestorePaths', []), list), 'RESTORE_SCOPE_INVALID')
        for path in cfg.get('allowedRestorePaths', []):
            require(isinstance(path, str) and path.startswith('/spec/template/') and '..' not in path, 'RESTORE_SCOPE_INVALID')
        evidence = private_dir(cfg['evidenceRoot']); archive = private_dir(cfg['archiveRoot'])
        require(evidence != archive and not archive.is_relative_to(evidence) and not evidence.is_relative_to(archive), 'ARCHIVE_ROOT_OVERLAP')
        return cfg

    def _policy_stopped(self, cfg, policy):
        meta = policy.get('metadata', {})
        require(not meta.get('deletionTimestamp') and meta.get('uid') == cfg['binding']['policyUID'] and meta.get('namespace') == cfg['namespace']
                and meta.get('name') == cfg['policy'] and type(meta.get('generation')) is int and meta['generation'] >= 1
                and policy['spec']['enabled'] is False and not policy.get('status', {}).get('activeIncident')
                and any(c.get('type') == 'Quiesced' and c.get('status') == 'True' and c.get('reason') == 'FenceClosed'
                        and c.get('observedGeneration') == meta['generation']
                        for c in policy.get('status', {}).get('conditions', [])), 'POLICY_STOP_UNCONFIRMED')
        self._bound_authority(cfg, policy)

    def _guard(self, cfg, api):
        require(time.time() < instant(cfg['expiresAt']), 'CLEANUP_AUTHORITY_EXPIRED')
        ns = cfg['namespace']
        policy = api.request('/apis/opsloop.io/v1alpha1/namespaces/' + ns + '/recoverypolicies/' + cfg['policy'])
        self._policy_stopped(cfg, policy)
        dep = api.request('/apis/apps/v1/namespaces/' + ns + '/deployments/' + cfg['deployment'])
        require(dep['metadata']['uid'] == cfg['binding']['deploymentUID'] and not dep['metadata'].get('deletionTimestamp'), 'DEPLOYMENT_UID_CHANGED')
        return dep

    def _restore(self, cfg, api, receipt):
        dep = self._guard(cfg, api)
        baseline_pin = bound_pin(cfg, 'baselineSpec', fields='/spec')
        if not hash_matches(dep, baseline_pin, fields='/spec'):
            restore = cfg.get('restore')
            if cfg.get('restorePatch') is not None:
                restore = receipt.get('restore')
                require(isinstance(restore, dict) and restore.get('patch') == cfg['restorePatch'], 'PINNED_RESTORE_REQUIRED')
            else:
                require(isinstance(restore, dict) and (receipt.get('restore') is None or receipt['restore'] == restore), 'PINNED_RESTORE_REQUIRED')
            if restore.get('expectedSpecSHA256') is not None:
                verify_hash(dep, bound_pin(restore, 'expectedSpec', fields='/spec'), fields='/spec')
            ops = restore.get('patch')
            require(isinstance(ops, list) and 0 < len(ops) <= 32, 'RESTORE_PATCH_INVALID')
            for op in ops:
                require(op.get('op') in ('test', 'add', 'replace', 'remove') and op.get('path') in cfg.get('allowedRestorePaths', []), 'RESTORE_PATCH_OUTSIDE_SCOPE')
                require(set(op) <= {'op', 'path', 'value'}, 'RESTORE_PATCH_INVALID')
            require(hash_matches(patched(dep, ops), baseline_pin, fields='/spec'), 'RESTORE_WOULD_NOT_MATCH_BASELINE')
            patch = [{'op': 'test', 'path': '/metadata/uid', 'value': dep['metadata']['uid']},
                     {'op': 'test', 'path': '/metadata/resourceVersion', 'value': dep['metadata']['resourceVersion']},
                     {'op': 'test', 'path': '/spec', 'value': dep['spec']}, *ops]
            require(time.time() < instant(cfg['expiresAt']), 'CLEANUP_AUTHORITY_EXPIRED')
            api.request('/apis/apps/v1/namespaces/' + cfg['namespace'] + '/deployments/' + cfg['deployment'], patch=patch)
            dep = self._guard(cfg, api)
        wait = cfg.get('baselineWaitSeconds', 30)
        require(type(wait) is int and 0 <= wait <= 120, 'BASELINE_WAIT_INVALID')
        deadline = time.monotonic() + wait
        while True:
            require(hash_matches(dep, baseline_pin, fields='/spec'), 'BASELINE_SPEC_UNCONFIRMED')
            status = dep.get('status', {}); replicas = dep['spec'].get('replicas', 1)
            if (replicas > 0 and status.get('observedGeneration', 0) >= dep['metadata'].get('generation', 1)
                    and status.get('readyReplicas', 0) == replicas and status.get('updatedReplicas', 0) == replicas
                    and status.get('replicas', 0) == replicas and not status.get('unavailableReplicas', 0)):
                return dep
            require(time.monotonic() < deadline, 'BASELINE_READY_UNCONFIRMED')
            time.sleep(min(.2, max(0, deadline-time.monotonic())))
            dep = self._guard(cfg, api)  # observation only; never replay a restore write.

    def _object(self, cfg, obj, identity):
        meta = obj.get('metadata', {})
        require(meta.get('namespace') == cfg['namespace'] and meta.get('name') == identity['name']
                and meta.get('uid') == identity['uid'] and meta.get('resourceVersion') == identity['resourceVersion'], 'CLEANUP_OBJECT_REPLACED')
        annotations = meta.get('annotations', {})
        require(annotations.get(RUN) == cfg['binding']['runID'] and annotations.get(EPOCH) == cfg['binding']['epoch'], 'CLEANUP_FOREIGN_RUN')
        if identity['kind'] == 'RecoveryIncident':
            runtime = obj.get('status', {}).get('runtime', {})
            require(runtime.get('phase') in TERMINAL and runtime.get('completedAt') and not runtime.get('safetyHaltReason')
                    and not runtime.get('oldPodsOutstanding'), 'INCIDENT_NOT_TERMINAL')
            require(obj.get('spec', {}).get('policyRef', {}).get('uid') == cfg['binding']['policyUID']
                    and obj.get('spec', {}).get('workload', {}).get('uid') == cfg['binding']['deploymentUID'], 'INCIDENT_OWNER_MISMATCH')
        if identity['kind'] == 'Job':
            status = obj.get('status', {})
            require(not status.get('active') and any(c.get('type') in ('Complete', 'Failed') and c.get('status') == 'True'
                    for c in status.get('conditions', [])), 'JOB_NOT_TERMINAL')
        return obj

    def _bound_authority(self, cfg, policy):
        annotations = policy.get('metadata', {}).get('annotations', {})
        selected = {k: annotations[k] for k in ('opsloop.io/execution-grant', 'opsloop.io/exhibition-permit') if k in annotations}
        for key, raw in selected.items():
            try:
                value = json.loads(raw)
                if key.endswith('exhibition-permit'):
                    value = json.loads(base64.b64decode(value['payload'], validate=True))['grant']
                scope = value['scope']
                require(all(scope.get(k) == cfg['binding'][k] for k in ('runID', 'epoch', 'policyUID', 'deploymentUID')), 'RETIRE_FOREIGN_AUTHORITY')
            except ControlError:
                raise
            except (ValueError, KeyError, TypeError):
                raise ControlError('RETIRE_AUTHORITY_INVALID') from None
        return selected

    def _retire_authority(self, cfg, api, archived, run_dir):
        self._guard(cfg, api)
        path = '/apis/opsloop.io/v1alpha1/namespaces/' + cfg['namespace'] + '/recoverypolicies/' + cfg['policy']
        policy = api.request(path)
        self._policy_stopped(cfg, policy)
        selected = self._bound_authority(cfg, policy)
        archived_file = archived / 'authority-retirement.json'
        marker = run_dir / 'authority-retirement.json'
        require(not archived_file.is_symlink() and not marker.is_symlink(), 'UNSAFE_LIFECYCLE_FILE')
        if archived_file.exists():
            old = json.loads(archived_file.read_bytes())
            require(old.get('binding') == cfg['binding'] and (not selected or selected == old['annotations']), 'RETIRE_AUTHORITY_CHANGED')
            require(bool(selected) or not old['annotations'] or marker.exists(), 'AUTHORITY_REMOVED_WITHOUT_RECEIPT')
        else:
            write(archived_file, canonical({'binding': cfg['binding'], 'annotations': selected}).encode(), exclusive=True)
            archived_file.chmod(0o400)
        if selected:
            if marker.exists():
                require(json.loads(marker.read_bytes()).get('state') != 'DISPATCHING', 'RETIRE_AUTHORITY_UNCONFIRMED')
            patch = [{'op': 'test', 'path': '/metadata/uid', 'value': policy['metadata']['uid']},
                     {'op': 'test', 'path': '/metadata/resourceVersion', 'value': policy['metadata']['resourceVersion']},
                     {'op': 'test', 'path': '/spec/enabled', 'value': False}]
            for key, raw in selected.items():
                pointer = '/metadata/annotations/' + key.replace('~', '~0').replace('/', '~1')
                patch += [{'op': 'test', 'path': pointer, 'value': raw}, {'op': 'remove', 'path': pointer}]
            write(marker, canonical({'state': 'DISPATCHING', 'binding': cfg['binding']}).encode())
            require(time.time() < instant(cfg['expiresAt']), 'CLEANUP_AUTHORITY_EXPIRED')
            api.request(path, patch=patch)
            check = api.request(path)
            require(all(k not in check.get('metadata', {}).get('annotations', {}) for k in selected), 'RETIRE_AUTHORITY_UNCONFIRMED')
        write(marker, canonical({'state': 'RETIRED', 'binding': cfg['binding']}).encode())
        return sha(archived_file.read_bytes())

    def reconcile(self, plan, receipt, emit):
        cfg = self.config(plan)
        require(isinstance(receipt, dict) and receipt.get('binding') == plan['binding'], 'CLEANUP_RECEIPT_BINDING_MISMATCH')
        require(receipt.get('stopConfirmed') is True and receipt.get('modelStopped') is True, 'TERMINATION_UNCONFIRMED')
        root = private_dir(cfg['evidenceRoot']); archive_root = private_dir(cfg['archiveRoot'])
        run_dir = private_dir(str(self.state / plan['binding']['runID']))
        archived = private_dir(str(archive_root / plan['binding']['runID']))
        inventory = receipt.get('objects'); evidence = receipt.get('evidence')
        require(isinstance(inventory, list) and len(inventory) <= 32 and isinstance(evidence, list) and 0 < len(evidence) <= 256, 'CLEANUP_INVENTORY_REQUIRED')
        seen = set()
        for item in inventory:
            require(isinstance(item, dict) and set(item) == {'kind', 'name', 'uid', 'resourceVersion'}
                    and item.get('kind') in KINDS and isinstance(item.get('name'), str) and NAME.fullmatch(item['name'])
                    and all(isinstance(item.get(k), str) and item[k] for k in ('uid', 'resourceVersion')), 'CLEANUP_OBJECT_INVALID')
            key = (item['kind'], item['name']); require(key not in seen, 'CLEANUP_DUPLICATE_OBJECT'); seen.add(key)
        api = self.transport_factory(cfg)
        try:
            self._restore(cfg, api, receipt)
            emit('coordinator', 'LIFECYCLE_BASELINE_RESTORED', {'baselineRestored': True})
            # Full inventory prevents an adapter omitting a same-run object to unlock next execution.
            listed = set()
            for kind, base in KINDS.items():
                values = api.request(base.format(ns=cfg['namespace']) + '?limit=1000')
                require(not values.get('metadata', {}).get('continue'), 'CLEANUP_INVENTORY_TRUNCATED')
                for obj in values.get('items', []):
                    ann = obj.get('metadata', {}).get('annotations', {})
                    if ann.get(RUN) == plan['binding']['runID']:
                        require(ann.get(EPOCH) == plan['binding']['epoch'], 'CLEANUP_EPOCH_MISMATCH')
                        listed.add((kind, obj['metadata']['name']))
            require(listed <= seen, 'CLEANUP_OBJECT_OMITTED')
            payloads = {}; entries = []
            for item in evidence:
                require(isinstance(item, dict) and set(item) == {'path', 'sha256'} and SHA.fullmatch(item.get('sha256', '')), 'EVIDENCE_DESCRIPTOR_INVALID')
                raw = safe_bytes(item['path'], root)
                require(sha(raw) == item['sha256'], 'EVIDENCE_CHANGED')
                name = str(Path(item['path']).relative_to(root))
                require(not any(e['name'] == name for e in entries), 'EVIDENCE_DUPLICATE')
                entries.append({'name': name, 'sha256': sha(raw), 'bytes': len(raw)})
                payloads[sha(raw)] = raw
                require(sum(len(v) for v in payloads.values()) <= 64*1024*1024, 'ARCHIVE_SIZE_LIMIT')
            object_records = []
            for item in inventory:
                path = KINDS[item['kind']].format(ns=cfg['namespace']) + '/' + item['name']
                marker = run_dir / (sha(canonical(item).encode()) + '.json')
                require(not marker.is_symlink(), 'UNSAFE_LIFECYCLE_FILE')
                obj = api.request(path, absent=True)
                if marker.exists():
                    object_records.append(pending_record(marker,item,'archiveRecord',obj))
                    continue
                require(obj is not None,'UNARCHIVED_OBJECT_MISSING')
                self._object(cfg, obj, item)
                if item['kind'] == 'ConfigMap':
                    # A CM still referenced by a workload is not temporary garbage.
                    for resource in ('pods', 'replicationcontrollers'):
                        values = api.request('/api/v1/namespaces/' + cfg['namespace'] + '/' + resource + '?limit=1000')
                        require(not values.get('metadata', {}).get('continue'), 'CLEANUP_INVENTORY_TRUNCATED')
                        require(not any(item['name'] in canonical(v.get('spec', {})) for v in values.get('items', [])), 'CONFIGMAP_STILL_REFERENCED')
                    for resource in ('deployments', 'replicasets', 'statefulsets', 'daemonsets'):
                        values = api.request('/apis/apps/v1/namespaces/' + cfg['namespace'] + '/' + resource + '?limit=1000')
                        require(not values.get('metadata', {}).get('continue'), 'CLEANUP_INVENTORY_TRUNCATED')
                        require(not any(item['name'] in canonical(v.get('spec', {})) for v in values.get('items', [])), 'CONFIGMAP_STILL_REFERENCED')
                    for resource in ('jobs', 'cronjobs'):
                        values = api.request('/apis/batch/v1/namespaces/' + cfg['namespace'] + '/' + resource + '?limit=1000')
                        require(not values.get('metadata', {}).get('continue'), 'CLEANUP_INVENTORY_TRUNCATED')
                        require(not any(item['name'] in canonical(v.get('spec', {})) for v in values.get('items', [])), 'CONFIGMAP_STILL_REFERENCED')
                raw = object_bytes(obj)
                require(not re.search(rb'(?i)(PRIVATE KEY|"(?:token|secret|access_token|refresh_token|client-key-data|password)"\s*:)', raw), 'CREDENTIAL_OBJECT_REJECTED')
                record = {'identity': item, 'sha256': sha(raw), 'bytes': len(raw), 'hash': describe_hash(obj, '/')}
                payloads[sha(raw)] = raw; object_records.append(record)
            manifest = {'schema': 'opsloop-exhibition-archive/v1', 'binding': plan['binding'],
                        'files': sorted(entries, key=lambda x: x['name']), 'objects': object_records}
            encoded = canonical(manifest).encode(); manifest_file = archived / 'manifest.json'
            for digest, raw in payloads.items():
                file = archived / digest
                require(not file.is_symlink(), 'ARCHIVE_SYMLINK')
                if file.exists(): require(file.read_bytes() == raw, 'ARCHIVE_CHANGED')
                else: write(file, raw, exclusive=True); file.chmod(0o400)
            require(not manifest_file.is_symlink(), 'ARCHIVE_SYMLINK')
            if manifest_file.exists(): require(manifest_file.read_bytes() == encoded, 'ARCHIVE_MANIFEST_CHANGED')
            else: write(manifest_file, encoded, exclusive=True); manifest_file.chmod(0o400)
            # Verify every archived blob, including already deleted objects on resume.
            for desc in [*manifest['files'], *manifest['objects']]:
                file = archived / desc['sha256']
                require(file.is_file() and not file.is_symlink() and sha(file.read_bytes()) == desc['sha256'], 'ARCHIVE_INTEGRITY_FAILED')
            emit('coordinator', 'LIFECYCLE_EVIDENCE_ARCHIVED', {'evidenceArchived': True, 'sourceSHA256': sha(encoded)})
            for item, archived_record in zip(inventory, object_records):
                self._restore(cfg, api, receipt)
                path = KINDS[item['kind']].format(ns=cfg['namespace']) + '/' + item['name']
                marker = run_dir / (sha(canonical(item).encode()) + '.json')
                require(not marker.is_symlink(), 'UNSAFE_LIFECYCLE_FILE')
                obj = api.request(path, absent=True)
                if marker.exists():
                    require(pending_record(marker,item,'archiveRecord',obj)==archived_record,'DELETE_ARCHIVE_CHANGED')
                else:
                    require(obj is not None,'UNARCHIVED_OBJECT_MISSING')
                    self._object(cfg,obj,item)
                    verify_hash(obj,archived_record.get('hash'),fields='/')
                    require(archived_record['hash']['sha256']==archived_record['sha256'],'ARCHIVE_HASH_ALIAS_CHANGED')
                    verify_hash(json.loads((archived/archived_record['sha256']).read_bytes()),archived_record['hash'],fields='/')
                    write(marker,canonical({'identity':item,'state':'DELETE_DISPATCHED','archiveRecord':archived_record}).encode(),exclusive=True)
                    require(time.time()<instant(cfg['expiresAt']),'CLEANUP_AUTHORITY_EXPIRED')
                    api.delete(path,item['uid'],item['resourceVersion'])
                if obj is None and marker.with_suffix('.confirmed.json').exists():
                    confirmed_deletion(marker,item,path)
                else:wait_deleted(api,path,item,marker,cfg['expiresAt'],window_seconds=cfg.get('deleteWaitSeconds',30))
            self._restore(cfg, api, receipt)
            for base in KINDS.values():
                values = api.request(base.format(ns=cfg['namespace']) + '?limit=1000')
                require(not values.get('metadata', {}).get('continue') and not any(
                    v.get('metadata', {}).get('annotations', {}).get(RUN) == plan['binding']['runID']
                    for v in values.get('items', [])), 'CLEANUP_OBJECTS_REMAIN')
            retired_sha = self._retire_authority(cfg, api, archived, run_dir)
            completion = {'schema': 'opsloop-exhibition-completion/v1', 'binding': plan['binding'], 'archiveSHA256': sha(encoded), 'archiveManifest': str(manifest_file),
                          'retiredAuthoritySHA256': retired_sha, 'nextRunAllowed': True,
                          'stopConfirmed': True, 'baselineRestored': True, 'modelStopped': True, 'evidenceArchived': True, 'objectsCleaned': True}
            raw = canonical(completion).encode(); final = run_dir / 'completion.json'
            require(not final.is_symlink(), 'UNSAFE_LIFECYCLE_FILE')
            if final.exists(): require(final.read_bytes() == raw, 'COMPLETION_CHANGED')
            else: write(final, raw, exclusive=True); final.chmod(0o400)
            emit('coordinator', 'LIFECYCLE_COMPLETE', {'baselineRestored': True, 'evidenceArchived': True, 'objectsCleaned': True, 'modelStopped': True})
            return {**completion, 'lifecycleReceiptSHA256': sha(raw)}
        finally:
            api.close()
