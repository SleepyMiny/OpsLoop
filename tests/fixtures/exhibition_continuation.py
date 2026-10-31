"""Synthetic fixed-Policy continuation inputs; never imports private live evidence.

Builds an interrupted DELETE with the real migration writer and an in-memory API.
All identities, source bytes, previous receipts and credentials are fabricated.
"""
import copy
import datetime as dt
from pathlib import Path

from tools.exhibition_control import ControlError
from tools.exhibition_hashes import describe, digest, encoded
from tools.exhibition_inputs import new, pin, migration_config
from tools.exhibition_migration import migrate
from tools.exhibition_preflight import object_path
from tests.test_exhibition_hash_contract import fixtures, build
from tests.test_exhibition_lifecycle import API
from tests.test_exhibition_readiness import history


def configured_plan(root):
    root = Path(root)
    prior, capture = fixtures()
    plan = build(prior, capture)
    objects = {k: copy.deepcopy(v.obj) for k, v in capture.items()}
    plan.update(status='SEALED_EXECUTION_CANDIDATE', clusterUID='cluster')
    plan['authority']['order'] = ['A1', 'A2', 'B1', 'B2']
    for item in plan['migration']['legacyObjectsAllowed']: item['kind'] = 'RecoveryIncident'
    plan['deployment']['imagePublication'].update(repository='registry.invalid/controller', newTag='synthetic')
    plan['readinessContract'] = {'historicalJobs': [], 'comparisons': [], 'requiredEvidence': []}
    plan['trials']['A'] = {'resourceConditions': {'cpuUtilizationFloorPermille': 900, 'nodeName': 'node-a', 'nodeUID': 'node-uid'}}
    legacy = {'opsloop.io/execution-grant': 'synthetic-expired-opaque-grant'}
    for key, target in plan['fixedTargets'].items():
        target['namespaceUID'] = 'ns-' + key
        target['legacyAuthoritySHA256'] = digest(legacy if key == 'a' else {})
        pol, dep = objects[key+'-policy'], objects[key+'-deployment']
        pol['spec']['filter'] = {'maxCPUUtilizationPercent': 80}
        pol['metadata']['annotations'] = copy.deepcopy(legacy if key == 'a' else {})
        dep['spec'].update(revisionHistoryLimit=2, strategy={'type': 'RollingUpdate', 'rollingUpdate': {'maxUnavailable': 0, 'maxSurge': 1}})
        dep['spec']['template']['metadata'] = {'labels': {'app': 'synthetic-'+key}}
        dep['status']['availableReplicas'] = 1
        if key == 'b':
            target['baselineDerivation']['responseContract'] = 'kubernetes-env-empty-omitted/v1'
        baseline = copy.deepcopy(dep)
        ops = []
        if key == 'b':
            del baseline['spec']['template']['spec']['containers'][0]['env'][5]['value']
            ops = [{'op': 'remove', 'path': '/spec/template/spec/containers/0/env/5/value'}]
        for name, obj in [('policySpec', pol), ('observedDeploymentSpec', dep)]:
            target[name+'Hash'] = describe(obj, '/spec')
            target[name+'SHA256'] = target[name+'Hash']['sha256']
        plan['objectHashContract']['objects'][key+'-policy'] = target['policySpecHash']
        plan['objectHashContract']['objects'][key+'-deployment'] = target['observedDeploymentSpecHash']
        target['baselineSpecHash'] = describe(baseline, '/spec')
        target['readyBaselineSpecSHA256'] = target['baselineSpecHash']['sha256']
        target['baselineDerivation'].update(source=target['observedDeploymentSpecHash'], patch=ops, patchSHA256=digest(ops))
        rs = {'metadata': {'uid': 'rs-'+key, 'ownerReferences': [{'kind': 'Deployment', 'uid': target['deploymentUID'], 'controller': True}]}, 'spec': {'template': copy.deepcopy(dep['spec']['template'])}}
        objects[key+'-replicasets'] = {'items': [rs]}
        objects[key+'-pods'] = {'items': [{'metadata': {'uid': 'pod-'+key, 'ownerReferences': [{'kind': 'ReplicaSet', 'uid': 'rs-'+key, 'controller': True}]}, 'status': {'phase': 'Running'}}]}
        hard = {'count/recoverypolicies.opsloop.io': '1', 'count/replicasets.apps': '4', 'count/pods': '6'}
        objects[key+'-quotas'] = {'items': [{'spec': {'hard': hard}, 'status': {'hard': copy.deepcopy(hard), 'used': {'count/recoverypolicies.opsloop.io': '1', 'count/replicasets.apps': '1', 'count/pods': '1'}}}]}
        objects[key+'-policies'] = {'items': [pol]}
        objects[key+'-service'] = {'metadata': {'uid': 'service-'+key}, 'spec': {'selector': {'app': 'synthetic-'+key}}}
        objects[key+'-incidents'] = {'items': [objects[key+'-incident']]}
        objects['namespace-'+target['namespace']] = {'metadata': {'uid': target['namespaceUID']}, 'status': {'phase': 'Active'}}
    job, pod, record = history(root)
    objects[job['metadata']['name']] = job
    objects['a-pods']['items'].append(pod)
    plan['readinessContract']['historicalJobs'].append(record)
    objects.update(cluster={'metadata': {'uid': 'cluster'}}, nodes={'items': [{'metadata': {'name': 'node-a', 'uid': 'node-uid'}}]}, **{'new-authority': {'absent': True}, 'system-quotas': {'items': []}})
    rules = [{'apiGroups': ['*'], 'resources': ['*'], 'verbs': ['*']}]
    objects['clusterroles-cluster-admin'] = {'metadata': {'uid': 'role-uid'}, 'rules': rules}
    objects['clusterrolebindings-cluster-admin'] = {'metadata': {'uid': 'binding-uid'}, 'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': 'cluster-admin'}, 'subjects': [{'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Group', 'name': 'system:masters'}]}
    credential = root/'synthetic-kubeconfig'
    credential.write_bytes(b'SYNTHETIC_NOT_A_KUBECONFIG'); credential.chmod(0o600)
    integration = {'sessionRoot': str(root/'session'), 'sessionID': 'synthetic-r6', 'archiveRoot': str(root/'archive'), 'migrationState': str(root/'migration'), 'controlState': str(root/'control'), 'engineering': {'state': str(root/'runner')}, 'apiOrigin': 'https://synthetic.invalid', 'kubeconfig': pin(credential), 'registryHelper': {}}
    evidence = root/'evidence'; evidence.mkdir(mode=0o700)
    evidence_ref = new(evidence, 'source.json', {'synthetic': True, 'purpose': 'continuation-custody-fixture'})
    e = {'integration': integration, 'captureRoot': str(evidence), 'migrationEvidence': {k: [evidence_ref] for k in ('a','b')}, 'migrationModelChecks': {k: [{'state': str(root/'old-runner'), 'binding': {'policyUID': 'pol-'+k, 'deploymentUID': 'dep-'+k}}] for k in ('a','b')}, 'services': {k: {'uid': 'service-'+k, 'spec': objects[k+'-service']['spec']} for k in ('a','b')}, 'authorityConfigName': 'new-authority', 'operatorRoleUID': 'role-uid', 'operatorBindingUID': 'binding-uid', 'operatorRules': rules}
    e['readRequests'] = [{'key': k, 'path': object_path(desc['object'])} for k, desc in plan['objectHashContract']['objects'].items()]
    e['readRequests'] += [{'key': 'cluster', 'path': '/api/v1/namespaces/kube-system'}]
    e['readRequests'] += [{'key': 'namespace-'+t['namespace'], 'path': '/api/v1/namespaces/'+t['namespace']} for t in plan['fixedTargets'].values()]
    plan['executionInputs'] = e

    # The original synthetic failure uses the same archive-before-DELETE path.
    original = migration_config(plan, 'a', approval_sha='synthetic-prior', deadline=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=5)).isoformat(), integration=integration)
    ref = new(root, 'original-migration.json', original)
    class InterruptedAPI(API):
        def request(self, path, patch=None, absent=False):
            if path == '/api/v1/namespaces/kube-system': return objects['cluster']
            if path == '/api/v1/namespaces/demo-a': return objects['namespace-demo-a']
            return super().request(path, patch, absent)
        def delete(self, *args):
            super().delete(*args)
            raise ControlError('SYNTHETIC_RESPONSE_LOSS_AFTER_DELETE')
    api = InterruptedAPI(objects['a-policy'], objects['a-deployment'], {'incident': objects['a-incident']})
    try:
        migrate(ref['path'], ref['sha256'], transport_factory=lambda _: api, model_checker=lambda *args: [])
    except ControlError as exc:
        if str(exc) != 'SYNTHETIC_RESPONSE_LOSS_AFTER_DELETE': raise
    else: raise AssertionError('synthetic interrupted DELETE did not occur')
    assert api.deleted == [('incident', 'inc-a', '9')]
    archive = root/'archive'/original['migrationID']/'manifest.json'
    state = root/'migration'/original['migrationID']
    markers = [p for p in state.glob('*.json') if p.name != 'model-stop-proof.json']
    assert len(markers) == 1
    marker = markers[0]
    identity = {'kind': 'RecoveryIncident', 'name': 'incident', 'uid': 'inc-a', 'resourceVersion': '9'}
    path = object_path(plan['objectHashContract']['objects']['a-incident']['object'])
    audit = root/'synthetic-followup'; audit.mkdir(mode=0o700)
    stdout, stderr = audit/'stdout.bin', audit/'stderr.bin'
    stdout.write_bytes(b''); stderr.write_bytes(b'Error from server (NotFound): synthetic incident not found\n')
    stdout.chmod(0o600); stderr.chmod(0o600)
    audit_ref = new(audit, 'receipt.json', {'target': path, 'argv': ['synthetic-kubectl', 'get', '--raw', path], 'stdinBytes': 0, 'exitCode': 1, 'stdoutSHA256': pin(stdout)['sha256'], 'stderrSHA256': pin(stderr)['sha256']})
    source_root = Path(__file__).resolve().parents[2]
    files = [pin(source_root/'tools'/n) for n in ('exhibition_deletion.py','exhibition_lifecycle.py','exhibition_runtime.py','exhibition_hashes.py','exhibition_control.py')]
    cfg = {'schema': 'opsloop-delete-continuation/v1', 'allowedMethods': ['GET'], 'originalMigrationPlan': ref, 'archiveManifest': pin(archive), 'dispatchMarker': pin(marker), 'followupGET': audit_ref, 'identity': identity, 'path': path, 'executionFiles': files}
    prior_plan = new(root, 'synthetic-previous-plan.json', plan)
    previous_result = new(root, 'synthetic-previous-result.json', {'state': 'SYNTHETIC_DELETE_RESPONSE_LOST', 'completed': []})
    plan['continuation'] = {'priorDeletionPlan': new(root, 'custody.json', cfg), 'previousExecutionPlan': prior_plan, 'previousExecutionResult': previous_result, 'executionFiles': files}
    objects['a-incident'] = {'absent': True}
    objects['a-incidents'] = {'items': []}
    return plan, objects, cfg, marker
