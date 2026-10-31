"""Explicit host-only legacy custody migration. Never creates/deletes a Policy.

No HTTP endpoint imports this CLI. A full plan hash and --approve-migration are
required. Legacy objects keep their original identity and annotations in archive;
they are never relabelled as a new exhibition run.
"""
from __future__ import annotations
import argparse
import json
from pathlib import Path
import time

from tools.exhibition_deletion import pending_record, wait_deleted, confirmed_deletion
from tools.exhibition_control import ControlError, canonical, identifier, instant, pinned
from tools.exhibition_hashes import bound_pin, validate as validate_hash, verify as verify_hash, encoded as object_bytes
from tools.exhibition_lifecycle import (CleanupTransport, Lifecycle, KINDS, NAME, TERMINAL,
                                        private_dir, require, safe_bytes, sha, write)


class MigrationGuard(Lifecycle):
    def _bound_authority(self, cfg, policy):
        selected = {k: v for k, v in policy['metadata'].get('annotations', {}).items()
                    if k in ('opsloop.io/execution-grant', 'opsloop.io/exhibition-permit')}
        marker = self.state / 'authority-retirement.json'
        retired = not selected and marker.is_file() and not marker.is_symlink() and json.loads(marker.read_bytes()).get('state') in ('DISPATCHING', 'RETIRED')
        require(sha(canonical(selected).encode()) == cfg['legacyAuthoritySHA256'] or retired, 'MIGRATION_AUTHORITY_DRIFT')
        return selected


def migrate(plan_path, expected_sha256, *, transport_factory=CleanupTransport, model_checker=None):
    plan = pinned(str(plan_path), expected_sha256)
    require(plan.get('schema') == 'opsloop-exhibition-migration/v1' and plan.get('approved') is True
            and isinstance(plan.get('approvalReference'), str) and plan['approvalReference']
            and identifier(plan.get('migrationID')), 'MIGRATION_APPROVAL_REQUIRED')
    require(time.time() < instant(plan.get('expiresAt')), 'MIGRATION_APPROVAL_EXPIRED')
    custody_sha=expected_sha256
    if 'resumeCustody' in plan:
        original=pinned(plan['resumeCustody']['path'],plan['resumeCustody']['sha256'])
        unchanged={k:v for k,v in plan.items() if k not in ('resumeCustody','expiresAt','approvalReference')}
        require(unchanged=={k:v for k,v in original.items() if k not in ('expiresAt','approvalReference')},'MIGRATION_RESUME_SCOPE_CHANGED')
        require(plan['approvalReference']!=original['approvalReference'],'MIGRATION_FRESH_APPROVAL_REQUIRED')
        custody_sha=plan['resumeCustody']['sha256']
    target = plan['target']
    require(set(target) == {'clusterUID', 'namespaceUID', 'namespace', 'policy', 'policyUID', 'deployment', 'deploymentUID'}
            and all(identifier(v) for v in target.values()), 'MIGRATION_TARGET_INVALID')
    for field in ('namespace', 'policy', 'deployment'):
        require(NAME.fullmatch(target[field]), 'MIGRATION_TARGET_INVALID')
    require(isinstance(plan.get('modelChecks'), list) and 0 < len(plan['modelChecks']) <= 16, 'MIGRATION_MODEL_INVENTORY_REQUIRED')
    bound_pin(plan, 'baselineSpec', fields='/spec')
    bound_pin(plan, 'policySpec', fields='/spec')
    for desc in plan.get('objects', []):
        pin = validate_hash(desc.get('hash'), fields='/')
        require(pin['sha256'] == desc.get('sha256') and pin['object']['uid'] == desc.get('uid')
                and pin['object']['name'] == desc.get('name') and pin['object']['namespace'] == target['namespace']
                and pin['object']['kind'] == desc.get('kind'), 'MIGRATION_CUSTODY_HASH_MISMATCH')
    root = private_dir(plan['evidenceRoot']); archive = private_dir(plan['archiveRoot'])
    require(root != archive and not archive.is_relative_to(root) and not root.is_relative_to(archive), 'MIGRATION_ARCHIVE_OVERLAP')
    directory = private_dir(str(archive / plan['migrationID']))
    state = private_dir(plan['stateRoot']); session = private_dir(str(state / plan['migrationID']))
    cfg = {**plan, **{k: target[k] for k in ('namespace', 'policy', 'deployment')},
           'binding': {'policyUID': target['policyUID'], 'deploymentUID': target['deploymentUID']},
           'baselineSpecSHA256': plan['baselineSpecSHA256']}
    guard = MigrationGuard(session)
    if model_checker is None:
        from tools.exhibition_reconcile import model_jobs_stopped
        model_checker = model_jobs_stopped
    model_files = []
    for check_index, check in enumerate(plan['modelChecks']):
        require(set(check) == {'state', 'binding'} and Path(check['state']).is_absolute()
                and check['binding'].get('policyUID') == target['policyUID']
                and check['binding'].get('deploymentUID') == target['deploymentUID'], 'MIGRATION_MODEL_SCOPE_CHANGED')
        for source in model_checker(check['state'], check['binding']):
            raw = safe_bytes(source, Path(check['state']))
            model_files.append({'name': 'model-checks/'+str(check_index)+'/'+str(Path(source).relative_to(check['state'])), 'sha256':sha(raw), 'bytes':len(raw), 'raw':raw})
    proof = {'schema':'opsloop-exhibition-migration-model-stop/v1','binding':target,'modelStopped':True,
             'checks':plan['modelChecks'],'sources':[{k:v for k,v in item.items() if k!='raw'} for item in model_files]}
    proof_path = session/'model-stop-proof.json';proof_raw=canonical(proof).encode()
    require(not proof_path.is_symlink(),'MIGRATION_UNSAFE_RECEIPT')
    if proof_path.exists():require(proof_path.read_bytes()==proof_raw,'MIGRATION_MODEL_PROOF_CHANGED')
    else:write(proof_path,proof_raw,exclusive=True);proof_path.chmod(0o400)
    model_files.append({'name':'model-stop-proof.json','sha256':sha(proof_raw),'bytes':len(proof_raw),'raw':proof_raw})
    api = transport_factory(plan)
    try:
        cluster = api.request('/api/v1/namespaces/kube-system')
        namespace = api.request('/api/v1/namespaces/' + target['namespace'])
        require(cluster['metadata']['uid'] == target['clusterUID'] and namespace['metadata']['uid'] == target['namespaceUID'], 'MIGRATION_CLUSTER_CHANGED')
        policy_path = '/apis/opsloop.io/v1alpha1/namespaces/' + target['namespace'] + '/recoverypolicies/' + target['policy']
        policy = api.request(policy_path); guard._policy_stopped(cfg, policy)
        verify_hash(policy, bound_pin(plan, 'policySpec', fields='/spec'), fields='/spec')
        sequence = policy.get('status', {}).get('incidentSequence', 0)
        # Validate custody before any optional baseline PATCH.
        descriptors = plan.get('objects'); evidence = plan.get('evidence')
        require(isinstance(descriptors, list) and len(descriptors) <= 32 and isinstance(evidence, list)
                and 0 < len(evidence) <= 256, 'MIGRATION_CUSTODY_INVENTORY_REQUIRED')
        current_incidents = api.request(KINDS['RecoveryIncident'].format(ns=target['namespace'])+'?limit=1000')
        require(not current_incidents.get('metadata',{}).get('continue'),'MIGRATION_INVENTORY_TRUNCATED')
        approved_incidents = {(o.get('name'),o.get('uid')) for o in descriptors if o.get('kind')=='RecoveryIncident'}
        for obj in current_incidents.get('items',[]):
            ref=obj.get('spec',{}).get('policyRef',{})
            if ref.get('uid')==target['policyUID'] or ref.get('name')==target['policy']:
                require((obj['metadata']['name'],obj['metadata']['uid']) in approved_incidents,'MIGRATION_INVENTORY_INCOMPLETE')
        payloads = {item['sha256']:item['raw'] for item in model_files}; records = []
        files = [{k:v for k,v in item.items() if k!='raw'} for item in model_files]; identities = set()
        for desc in evidence:
            require(set(desc) == {'path', 'sha256'}, 'MIGRATION_EVIDENCE_INVALID')
            raw = safe_bytes(desc['path'], root); require(sha(raw) == desc['sha256'], 'MIGRATION_EVIDENCE_CHANGED')
            name = str(Path(desc['path']).relative_to(root)); require(name not in {f['name'] for f in files}, 'MIGRATION_EVIDENCE_DUPLICATE')
            files.append({'name': name, 'sha256': sha(raw), 'bytes': len(raw)}); payloads[sha(raw)] = raw
        for desc in descriptors:
            require(set(desc) <= {'kind', 'name', 'uid', 'resourceVersion', 'sha256', 'grantBinding', 'hash'}
                    and {'kind', 'name', 'uid', 'resourceVersion', 'sha256'} <= set(desc)
                    and desc['kind'] in KINDS and NAME.fullmatch(desc['name']), 'MIGRATION_OBJECT_INVALID')
            identity = {k: desc[k] for k in ('kind', 'name', 'uid', 'resourceVersion')}
            key = (desc['kind'], desc['name']); require(key not in identities, 'MIGRATION_OBJECT_DUPLICATE'); identities.add(key)
            path = KINDS[desc['kind']].format(ns=target['namespace']) + '/' + desc['name']
            marker = session / (sha(canonical(identity).encode()) + '.json')
            require(not marker.is_symlink(), 'MIGRATION_UNSAFE_RECEIPT')
            obj = api.request(path, absent=True)
            if 'resumeCustody' in plan:
                require(marker.exists() and obj is None,'MIGRATION_RESUME_ABSENCE_REQUIRED')
            if marker.exists():
                prior=pending_record(marker,identity,'record',obj)
                require(prior['sha256']==desc['sha256'] and prior.get('hash')==desc['hash'],'MIGRATION_ARCHIVE_CHANGED')
                records.append(prior);continue
            require(obj is not None,'MIGRATION_OBJECT_MISSING_WITHOUT_CUSTODY')
            meta = obj.get('metadata', {})
            require(meta.get('namespace') == target['namespace'] and all(meta.get(k) == desc[k] for k in ('name','uid','resourceVersion')), 'MIGRATION_OBJECT_CHANGED')
            verify_hash(obj, desc['hash'], fields='/')
            if desc['kind'] == 'RecoveryIncident':
                status = obj.get('status', {}).get('runtime', {})
                require(status.get('phase') in TERMINAL and status.get('completedAt') and not status.get('safetyHaltReason')
                        and not status.get('oldPodsOutstanding'), 'MIGRATION_INCIDENT_ACTIVE')
                require(obj['spec']['policyRef']['uid'] == target['policyUID'] and obj['spec']['workload']['uid'] == target['deploymentUID'], 'MIGRATION_INCIDENT_OWNER_CHANGED')
                require(desc.get('grantBinding') and meta.get('annotations', {}).get('opsloop.io/execution-grant-binding') == desc['grantBinding'], 'MIGRATION_GRANT_CUSTODY_REQUIRED')
            elif desc['kind'] == 'Job':
                require(not obj.get('status', {}).get('active') and any(c.get('type') in ('Complete','Failed') and c.get('status') == 'True'
                        for c in obj.get('status', {}).get('conditions', [])), 'MIGRATION_JOB_ACTIVE')
            elif desc['kind'] == 'ConfigMap':
                for prefix, resources in (('/api/v1', ('pods','replicationcontrollers')),
                        ('/apis/apps/v1', ('deployments','replicasets','statefulsets','daemonsets')),
                        ('/apis/batch/v1', ('jobs','cronjobs'))):
                    for resource in resources:
                        values = api.request(prefix+'/namespaces/'+target['namespace']+'/'+resource+'?limit=1000')
                        require(not values.get('metadata', {}).get('continue') and not any(desc['name'] in canonical(v.get('spec', {})) for v in values.get('items', [])), 'MIGRATION_CONFIGMAP_REFERENCED')
            raw = object_bytes(obj)
            # Reuse the evidence credential filter before archiving object bytes.
            import re
            require(not re.search(rb'(?i)(PRIVATE KEY|"(?:token|secret|access_token|refresh_token|client-key-data|password)"\s*:)', raw), 'MIGRATION_CREDENTIAL_OBJECT_REJECTED')
            payloads[sha(raw)] = raw; records.append({'identity': identity, 'sha256': sha(raw), 'bytes': len(raw), 'hash': desc['hash']})
        manifest = {'schema':'opsloop-exhibition-migration-archive/v1','migrationID':plan['migrationID'],
                    'binding':target,'planSHA256':custody_sha,'files':files,'objects':records}
        manifest_path = directory/'manifest.json'; raw = canonical(manifest).encode()
        for digest, data in payloads.items():
            file = directory/digest; require(not file.is_symlink(), 'MIGRATION_ARCHIVE_SYMLINK')
            if file.exists(): require(file.read_bytes() == data, 'MIGRATION_ARCHIVE_CHANGED')
            else: write(file,data,exclusive=True); file.chmod(0o400)
        require(not manifest_path.is_symlink(), 'MIGRATION_ARCHIVE_SYMLINK')
        if manifest_path.exists(): require(manifest_path.read_bytes() == raw, 'MIGRATION_ARCHIVE_CHANGED')
        else: write(manifest_path,raw,exclusive=True); manifest_path.chmod(0o400)
        for desc in [*files,*records]:
            file=directory/desc['sha256'];require(file.is_file() and not file.is_symlink() and sha(file.read_bytes()) == desc['sha256'], 'MIGRATION_ARCHIVE_INTEGRITY_FAILED')
        for desc, record in zip(descriptors, records):
            guard._restore(cfg,api,{'restore':plan.get('restore')})
            path=KINDS[desc['kind']].format(ns=target['namespace'])+'/'+desc['name']
            marker=session/(sha(canonical(record['identity']).encode())+'.json')
            obj=api.request(path,absent=True)
            if marker.exists():
                require(pending_record(marker,record['identity'],'record',obj)==record,'MIGRATION_ARCHIVE_CHANGED')
            else:
                require('resumeCustody' not in plan,'MIGRATION_RESUME_DELETE_FORBIDDEN')
                require(obj is not None,'MIGRATION_OBJECT_MISSING_WITHOUT_CUSTODY')
                verify_hash(obj,desc['hash'],fields='/')
                verify_hash(json.loads((directory/record['sha256']).read_bytes()),desc['hash'],fields='/')
                require(record.get('hash')==desc['hash'],'MIGRATION_ARCHIVE_CONTRACT_CHANGED')
                write(marker,canonical({'state':'DELETE_DISPATCHED','identity':record['identity'],'record':record}).encode(),exclusive=True)
                require(time.time()<instant(plan['expiresAt']),'MIGRATION_APPROVAL_EXPIRED')
                api.delete(path,desc['uid'],desc['resourceVersion'])
            if obj is None and marker.with_suffix('.confirmed.json').exists():
                confirmed_deletion(marker,record['identity'],path)
            else:wait_deleted(api,path,record['identity'],marker,plan['expiresAt'],window_seconds=plan.get('deleteWaitSeconds',30))
        guard._restore(cfg,api,{'restore':plan.get('restore')})
        # Retire only exact preapproved old authority values; never reset status/history.
        retirement=directory/'authority-retirement.json';marker=session/'authority-retirement.json'
        current=api.request(policy_path);guard._policy_stopped(cfg,current)
        selected=guard._bound_authority(cfg,current)
        require(not retirement.is_symlink() and not marker.is_symlink(),'MIGRATION_UNSAFE_RECEIPT')
        if retirement.exists():
            previous=json.loads(retirement.read_bytes())
            require(previous.get('binding')==target and (not selected or previous['annotations']==selected),'MIGRATION_AUTHORITY_DRIFT')
        else:
            write(retirement,canonical({'binding':target,'annotations':selected}).encode(),exclusive=True);retirement.chmod(0o400)
        if selected:
            require(plan.get('retireAuthority') is True,'MIGRATION_RETIRE_APPROVAL_REQUIRED')
            require(not marker.exists(),'MIGRATION_RETIRE_UNCONFIRMED')
            patch=[{'op':'test','path':'/metadata/uid','value':current['metadata']['uid']},
                   {'op':'test','path':'/metadata/resourceVersion','value':current['metadata']['resourceVersion']},
                   {'op':'test','path':'/spec/enabled','value':False}]
            for key,value in selected.items():
                pointer='/metadata/annotations/'+key.replace('~','~0').replace('/','~1')
                patch.extend([{'op':'test','path':pointer,'value':value},{'op':'remove','path':pointer}])
            write(marker,canonical({'state':'DISPATCHING'}).encode())
            require(time.time()<instant(plan['expiresAt']),'MIGRATION_APPROVAL_EXPIRED')
            api.request(policy_path,patch=patch)
            updated=api.request(policy_path)
            require(all(k not in updated.get('metadata',{}).get('annotations',{}) for k in selected),'MIGRATION_RETIRE_UNCONFIRMED')
        write(marker,canonical({'state':'RETIRED'}).encode())
        guard._restore(cfg,api,{'restore':plan.get('restore')})
        after=api.request(policy_path)
        require(after['spec'] == policy['spec'] and after.get('status', {}).get('incidentSequence',0) == sequence,'MIGRATION_POLICY_STATE_CHANGED')
        completion={'schema':'opsloop-exhibition-migration-completion/v1','migrationID':plan['migrationID'],
            'binding':target,'baseIncidentSequence':sequence,'nextRunAllowed':True,'stopConfirmed':True,
            'baselineRestored':True,'modelStopped':True,'evidenceArchived':True,'objectsCleaned':True,
            'archiveManifest':str(manifest_path),'archiveSHA256':sha(raw),'planSHA256':custody_sha,
            'retiredAuthoritySHA256':sha(retirement.read_bytes())}
        if 'resumeCustody' in plan:
            completion['executionPlanSHA256']=expected_sha256
            completion['deletionConfirmations']=[confirmed_deletion(session/(sha(canonical(r['identity']).encode())+'.json'),r['identity'],KINDS[r['identity']['kind']].format(ns=target['namespace'])+'/'+r['identity']['name']) for r in records]
        file=session/'completion.json';data=canonical(completion).encode()
        if file.exists(): require(file.read_bytes()==data,'MIGRATION_COMPLETION_CHANGED')
        else:write(file,data,exclusive=True);file.chmod(0o400)
        return {'completion':str(file),'sha256':sha(data),'objectsCleaned':len(descriptors)}
    finally:api.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan',required=True);parser.add_argument('--sha256',required=True)
    parser.add_argument('--approve-migration',action='store_true',required=True)
    args=parser.parse_args()
    print(json.dumps(migrate(Path(args.plan),args.sha256)))

if __name__=='__main__':main()
