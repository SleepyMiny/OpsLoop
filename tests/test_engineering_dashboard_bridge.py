import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from tools.engineering_dashboard_bridge import projection, join


def sha(raw): return hashlib.sha256(raw).hexdigest()


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.manifest = {'schema': 'opsloop-engineering-dashboard-artifacts/v1', 'files': {}}
        self.inc = {'kind': 'RecoveryIncident', 'metadata': {'name': 'incident', 'namespace': 'demo', 'uid': 'incident-uid', 'annotations': {'opsloop.io/execution-grant-binding': 'f'*64}}, 'runtimeBinding': {'runID': 'runtime-run', 'epoch': 'epoch-1', 'policyUID': 'policy-uid', 'workloadUID': 'deployment', 'grantSha256': 'f'*64}, 'spec': {'workload': {'uid': 'deployment'}, 'policyRef': {'uid': 'policy-uid'}}, 'status': {'runtime': {'phase': 'FAILED', 'reason': 'RUNTIME_BUDGET_EXHAUSTED'}}}
        b = {'incident': {'uid': 'incident-uid', 'run_id': 'runtime-run', 'epoch': 'epoch-1'}, 'workload': {'policy_uid': 'policy-uid', 'namespace': 'demo', 'deployment_uid': 'deployment', 'pod_uid': 'replacement-pod', 'pod_name': 'replacement'}}
        self.put('bundle', b); self.put('incident', self.inc)
        r = {'state': 'VERIFIED_PENDING_REVIEW', 'real_model_execution': True, 'model_process': {'exit_code': 0, 'descendants_terminated': True}, 'broker': {'requests': 1, 'model': 'approved-model'}, 'baseline_verification': {'accepted': True, 'report': {'baseline_defect_reproduced': True}}, 'candidate_verification': {'accepted': True, 'report': {'all_passed': True, 'checks': [{'passed': True}]}, 'remote_ci': 'NOT_RUN'}, 'bundle_sha256': self.manifest['files']['bundle']['sha256'], 'incident': b['incident'], 'source': {'repository': 'https://github.com/owner/repo', 'base_commit': 'a'*40}, 'candidate': {'source_sha256': 'c'*64}, 'patch_sha256': 'd'*64, 'run_id': 'run', 'execution_mode': 'DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR'}
        self.put('result', r)
        self.put('review', {'decision': 'APPROVED', 'result_sha256': self.manifest['files']['result']['sha256'], 'candidate_source_sha256': 'c'*64, 'patch_sha256': 'd'*64, 'bundle_sha256': r['bundle_sha256'], 'base_commit': 'a'*40})
        response = {'number': 6, 'html_url': 'https://github.com/owner/repo/pull/6', 'title': 'Fix defect', 'merged': False, 'state': 'open', **{side: {'sha': letter*40, 'ref': side, 'repo': {'full_name': 'owner/repo', 'id': 1}} for side, letter in [('base','a'), ('head','b')]}}
        self.put('receipt', {'schema_version': 'opsloop-github-pr-observation/v1', 'evidence_kind': 'observed', 'method': 'GET', 'api_origin': 'https://api.github.com', 'path': '/repos/owner/repo/pulls/6', 'response': response, 'observed_at': '2026-10-27T00:00:00Z'})
        self.put('pr', {'schema_version': 'opsloop-pr-result/v1', 'state': 'PR_CREATED', 'bundle_sha256': r['bundle_sha256'], 'candidate_source_sha256': 'c'*64, 'base_commit': 'a'*40, 'head_commit': 'b'*40, 'base_ref': 'base', 'head_ref': 'head', 'repository': 'owner/repo', 'repository_id': 1, 'pr_number': 6, 'pr_url': response['html_url'], 'remote_receipt_sha256': self.manifest['files']['receipt']['sha256'], 'merged': False, 'deployed': False, 'remote_ci': 'NOT_RUN'})

    def put(self, key, data):
        path = self.root/key; raw = json.dumps(data).encode(); path.write_bytes(raw)
        self.manifest['files'][key] = {'path': str(path), 'sha256': sha(raw)}

    def change(self, key, mutate):
        data = json.loads((self.root/key).read_bytes()); mutate(data); self.put(key, data)

    def test_verified_pr_projects_without_raw_logs(self):
        item = projection(self.manifest)
        self.assertEqual(item['status']['phase'], 'PR_CREATED')
        self.assertEqual(item['runtimeRunID'], 'runtime-run')
        self.assertEqual(item['status']['jobId'], 'run')
        self.assertEqual(item['epoch'], 'epoch-1')
        self.assertEqual(item['status']['evidence']['remoteCI'], 'NOT_RUN')
        self.assertNotIn('broker', json.dumps(item))
        self.assertNotIn('report', json.dumps(item))

    def test_hash_mismatch_rejected(self):
        (self.root/'result').write_text('{}')
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_same_name_replacement_does_not_join(self):
        item = projection(self.manifest)
        original = {'activeIncident': None, 'recentIncidents': [self.inc], 'workload': {'uid': 'deployment', 'namespace': 'demo'}}
        good = join(original, item)
        self.assertIsNone(good['activeIncident']); self.assertEqual(len(good['engineeringRuns']), 1)
        self.assertNotIn('engineeringRuns', original)
        for section, field in [('metadata','uid'),('metadata','namespace'),('metadata','name')]:
            wrong = copy.deepcopy(original); wrong['recentIncidents'][0][section][field] = 'other'
            self.assertEqual(join(wrong,item)['engineeringRuns'], [])
        wrong = copy.deepcopy(original); wrong['recentIncidents'][0]['spec']['workload']['uid'] = 'other'
        self.assertEqual(join(wrong,item)['engineeringRuns'], [])
        wrong = copy.deepcopy(original); wrong['workload']['uid'] = 'other'
        self.assertEqual(join(wrong,item)['engineeringRuns'], [])

    def test_other_run_epoch_or_policy_does_not_join(self):
        item = projection(self.manifest)
        original = {'activeIncident': self.inc, 'workload': {'uid': 'deployment', 'namespace': 'demo'}}
        for field in ('runID', 'epoch', 'policyUID', 'workloadUID', 'grantSha256'):
            wrong = copy.deepcopy(original)
            wrong['activeIncident']['runtimeBinding'][field] = 'other'
            self.assertEqual(join(wrong, item)['engineeringRuns'], [])
        wrong = copy.deepcopy(original)
        del wrong['activeIncident']['runtimeBinding']
        self.assertEqual(join(wrong, item)['engineeringRuns'], [])
        wrong = copy.deepcopy(original)
        wrong['activeIncident']['spec']['policyRef']['uid'] = 'other'
        self.assertEqual(join(wrong, item)['engineeringRuns'], [])

    def test_symlink_pinned_artifact_rejected(self):
        path = self.root/'result'
        real = self.root/'result-real'
        path.rename(real)
        path.symlink_to(real)
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_remote_head_mismatch_rejected(self):
        self.change('receipt', lambda d: d['response']['head'].update(sha='e'*40))
        self.change('pr', lambda d: d.update(remote_receipt_sha256=self.manifest['files']['receipt']['sha256']))
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_historical_join_preserves_current_topology(self):
        item = projection(self.manifest)
        pods = [{'uid': 'replacement-pod', 'namespace': 'demo', 'name': 'replacement', 'role': 'system', 'ready': False, 'phase': 'Running'},
                {'uid': 'other-pod', 'namespace': 'other', 'name': 'demo-api', 'role': 'demo-api', 'ready': True},
                {'uid': 'old-pod', 'namespace': 'demo', 'name': 'old', 'role': 'demo-api', 'ready': True}]
        snapshot = {'activeIncident': None, 'recentIncidents': [self.inc], 'workload': {'uid': 'deployment', 'namespace': 'demo'}, 'cluster': {'nodes': [{'name': 'azure', 'pods': pods}]}}
        projected = join(snapshot, item)['cluster']['nodes'][0]['pods']
        self.assertEqual(projected, pods)
        self.assertFalse(projected[0]['ready']); self.assertEqual(projected[0]['phase'], 'Running')
        self.assertEqual(snapshot['cluster']['nodes'][0]['pods'][0]['role'], 'system')
        for field in ('uid', 'namespace', 'name'):
            wrong = copy.deepcopy(snapshot); wrong['cluster']['nodes'][0]['pods'][0][field] = 'wrong'
            self.assertEqual(join(wrong,item)['cluster'], wrong['cluster'])
        wrong = copy.deepcopy(snapshot); wrong['cluster']['nodes'][0]['pods'].append(copy.deepcopy(pods[0]))
        self.assertEqual(join(wrong,item)['cluster'], wrong['cluster'])

    def test_later_live_rollout_is_not_replaced_by_evidence_pod(self):
        item = projection(self.manifest)
        current = {'uid': 'later-pod', 'namespace': 'demo', 'name': 'later', 'role': 'demo-api', 'ready': True, 'phase': 'Running'}
        snapshot = {'activeIncident': None, 'recentIncidents': [self.inc], 'workload': {'uid': 'deployment', 'namespace': 'demo'}, 'cluster': {'nodes': [{'name': 'physical', 'pods': [current]}]}}
        joined = join(snapshot, item)
        self.assertEqual(len(joined['engineeringRuns']), 1)
        self.assertEqual(joined['cluster'], snapshot['cluster'])
        self.assertEqual(joined['engineeringRuns'][0]['podUID'], 'replacement-pod')

    def test_review_wrong_candidate_rejected(self):
        self.change('review', lambda d: d.update(candidate_source_sha256='e'*64))
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_fake_model_or_failed_verification_rejected(self):
        self.change('result', lambda d: d.update(real_model_execution=False))
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_unverified_pr_rejected(self):
        self.change('pr', lambda d: d.update(state='PR_READY'))
        with self.assertRaises(ValueError): projection(self.manifest)

    def test_external_pr_link_rejected(self):
        self.change('pr', lambda d: d.update(pr_url='https://evil.invalid/pull/6'))
        with self.assertRaises(ValueError): projection(self.manifest)

if __name__ == '__main__': unittest.main()
