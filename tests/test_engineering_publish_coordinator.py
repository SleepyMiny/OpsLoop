"""Offline real Publisher -> host Coordinator -> GitHub GET proof contract tests."""
import copy
import datetime as dt
import json
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import test_engineering_publisher as fixtures
from test_engineering_publisher import NOW, when
from tools.engineering_publish_coordinator import publish, GitHubHostTransport, TransportError
from tools.engineering_publisher import REPOSITORY, REPOSITORY_ID, Rejected
from tools.engineering_runner.contracts import canonical, digest


class FakeGitHub:
    def __init__(self, plan, baseline):
        self.plan = plan; self.posts = []; self.head = None; self.pr = None
        self.fail = None; self.lose_after = None; self.modify_tree = False; self.other_prs = []
        self.commit_sha = 'c' * 40; self.tree_sha = 'd' * 40
        import hashlib
        self.original = []
        for name in ('discount.go', 'discount_test.go', 'main.go'):
            path = 'apps/engineering-demo/' + name; data = (baseline / path).read_bytes()
            self.original.append({'path': path, 'mode': '100644', 'type': 'blob',
                                  'sha': hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()})
        self.original += [{'path': 'unrelated.txt', 'mode': '100644', 'type': 'blob', 'sha': '1' * 40}]
    def request(self, method, path, body=None):
        path = path.removeprefix('/repos/' + REPOSITORY)
        if method == 'POST':
            self.posts.append(path)
            if self.fail == path: raise TransportError('synthetic timeout before response')
            if path == '/git/blobs':
                import hashlib
                data = body['content'].encode(); result = {'sha': hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()}
            elif path == '/git/trees': result = {'sha': self.tree_sha}
            elif path == '/git/commits': result = {'sha': self.commit_sha}
            elif path == '/git/refs': self.head = {'object': {'sha': self.commit_sha}}; result = self.head
            elif path == '/pulls':
                repo = {'full_name': REPOSITORY, 'id': REPOSITORY_ID}
                self.pr = {**body, 'number': 123, 'html_url': f'https://github.com/{REPOSITORY}/pull/123', 'state': 'open', 'merged': False, 'auto_merge': None,
                           'base': {'ref': self.plan['base_ref'], 'sha': self.plan['base_commit'], 'repo': repo},
                           'head': {'ref': self.plan['head_ref'], 'sha': self.commit_sha, 'repo': repo}}
                result = self.pr
            else: raise AssertionError(path)
            if self.lose_after == path: raise TransportError('synthetic lost successful response')
            return copy.deepcopy(result)
        if not path: return {'full_name': REPOSITORY, 'id': REPOSITORY_ID}
        if path.startswith('/git/ref/heads/'):
            from urllib.parse import unquote
            name = unquote(path.removeprefix('/git/ref/heads/'))
            return {'object': {'sha': self.plan['base_commit']}} if name == self.plan['base_ref'] else self.head
        if path == '/git/commits/' + self.plan['base_commit']:
            return {'sha': self.plan['base_commit'], 'tree': {'sha': self.plan['base_tree']}}
        if path == '/git/commits/' + self.commit_sha:
            return {'sha': self.commit_sha, 'tree': {'sha': self.tree_sha}, 'parents': [{'sha': self.plan['base_commit']}]}
        if path.startswith('/git/trees/'):
            base = self.plan['base_tree'] in path
            entries = copy.deepcopy(self.original)
            if not base:
                for f in self.plan['files']:
                    next(e for e in entries if e['path'] == f['path'])['sha'] = f['git_blob_sha']
                if self.modify_tree: entries[-1]['sha'] = 'f' * 40
            return {'sha': self.plan['base_tree'] if base else self.tree_sha, 'truncated': False, 'tree': entries}
        if path.startswith('/pulls?'): return copy.deepcopy(self.other_prs + ([self.pr] if self.pr else []))
        if path.startswith('/pulls/123/files'): return [{'filename': f['path'], 'status': 'modified', 'sha': f['git_blob_sha']} for f in self.plan['files']]
        if path == '/pulls/123': return copy.deepcopy(self.pr)
        raise AssertionError((method, path))


class CoordinatorTests(unittest.TestCase):
    def setUp(self):
        self.f = fixtures.PublisherTests(); self.f.setUp(); self.addCleanup(self.f.tearDown)
        self.prep = self.f.arguments(); self.plan = self.f.plan()
        args, self.approval, _ = self.f.receipt_arguments()
        self.api = FakeGitHub(self.plan, self.f.run/"baseline")
        self.args = {k: args[k] for k in ('plan_path', 'plan_sha256', 'approval_path', 'approval_sha256')}
        self.args.update(prepare_arguments=self.prep, state_root=self.f.root/'state', transport=self.api, clock=lambda: NOW)
    def run_publish(self): return publish(**self.args)
    def test_real_prepare_to_remote_get_proof(self):
        result = self.run_publish()
        self.assertEqual(result['state'], 'PR_CREATED')
        self.assertFalse(result['merged']); self.assertFalse(result['deployed'])
        self.assertEqual(self.api.posts, ['/git/blobs', '/git/blobs', '/git/trees', '/git/commits', '/git/refs', '/pulls'])
        self.assertTrue(all((p.stat().st_mode & 0o077) == 0 for p in self.args['state_root'].iterdir()))
        self.assertNotIn('nil/empty-slice', self.plan['pull_request']['body'])
    def test_restart_does_not_send_again(self):
        self.run_publish(); before = list(self.api.posts)
        self.assertEqual(self.run_publish()['state'], 'PR_CREATED'); self.assertEqual(before, self.api.posts)
    def test_lost_pr_response_reconciles_get_only_after_restart(self):
        self.api.lose_after = '/pulls'
        with self.assertRaises(TransportError): self.run_publish()
        before = list(self.api.posts)
        self.assertEqual(self.run_publish()['state'], 'PR_CREATED'); self.assertEqual(before, self.api.posts)
    def test_timeout_without_remote_pr_never_retries(self):
        for path in ('/git/blobs', '/git/trees', '/git/commits', '/git/refs', '/pulls'):
            with self.subTest(path=path):
                self.setUp(); self.api.fail = path
                with self.assertRaises(TransportError): self.run_publish()
                before = list(self.api.posts)
                with self.assertRaises(Rejected): self.run_publish()
                self.assertEqual(before, self.api.posts)
    def test_duplicate_pr_different_head_is_blocked(self):
        self.api.other_prs = [{'head': {'ref': 'other'}, 'body': self.plan['pull_request']['body']}]
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_existing_unowned_head_is_blocked(self):
        self.api.head = {'object': {'sha': '9'*40}}
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_base_and_tree_mismatch_before_writes(self):
        for field in ('base_commit', 'base_tree'):
            with self.subTest(field=field):
                self.setUp(); self.api.plan = copy.deepcopy(self.plan); self.api.plan[field] = 'f'*40
                with self.assertRaises(Rejected): self.run_publish()
                self.assertEqual(self.api.posts, [])
    def test_full_remote_tree_rejects_unrelated_change(self):
        self.api.modify_tree = True
        with self.assertRaises(Rejected): self.run_publish()
        self.assertNotIn('"state":"PR_CREATED"', ''.join(p.read_text() for p in self.args['state_root'].glob('*.json')))
    def test_changed_candidate_review_or_approval_blocks_all_writes(self):
        for kind in ('candidate', 'review', 'approval', 'run', 'epoch', 'patch'):
            with self.subTest(kind=kind):
                self.setUp()
                if kind == 'candidate': (self.f.run/'candidate/apps/engineering-demo/discount.go').write_text('changed')
                elif kind == 'review': self.args['clock'] = lambda: NOW + dt.timedelta(days=2)
                elif kind == 'approval':
                    self.approval['expires_at'] = when(-1); self.args['approval_sha256'] = self.f.write(self.args['approval_path'], self.approval)
                else:
                    plan = copy.deepcopy(self.plan)
                    if kind in ('run', 'epoch'): plan['incident']['run_id' if kind=='run' else 'epoch'] = 'changed'
                    else: plan['patch_sha256'] = 'f'*64
                    self.args['plan_sha256'] = self.f.write(self.args['plan_path'], plan)
                with self.assertRaises(Rejected): self.run_publish()
                self.assertEqual(self.api.posts, [])
    def test_unapproved_extra_file_is_rejected(self):
        (self.f.run/'candidate/extra.txt').write_text('unexpected')
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_new_approval_plan_cannot_reset_uncertain_operation(self):
        self.api.fail = '/git/blobs'
        with self.assertRaises(TransportError): self.run_publish()
        before = list(self.api.posts)
        changed = copy.deepcopy(self.plan); changed['pull_request']['title'] += ' changed'
        self.args['plan_sha256'] = self.f.write(self.args['plan_path'], changed)
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(before, self.api.posts)
    def test_expiry_during_journal_fsync_prevents_dispatch(self):
        from tools import engineering_publish_coordinator as coordinator
        actual = coordinator._save
        current = [NOW]
        def save(path, value):
            actual(path, value)
            if value.get('operations'):
                current[0] = NOW + dt.timedelta(seconds=100)
        self.args['clock'] = lambda: current[0]
        with patch.object(coordinator, '_save', side_effect=save):
            with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_approval_expiry_during_last_artifact_read_prevents_dispatch(self):
        from tools import engineering_publish_coordinator as coordinator
        actual = coordinator.read_json; current = [NOW]; reads = [0]
        def read(path, expected, *args):
            value = actual(path, expected, *args)
            if Path(path) == self.f.root/'review.json':
                reads[0] += 1
                if reads[0] == 2: current[0] = NOW + dt.timedelta(seconds=100)
            return value
        self.args['clock'] = lambda: current[0]
        with patch.object(coordinator, 'read_json', side_effect=read):
            with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_remote_baseline_bytes_must_match_source(self):
        self.api.original[0]['sha'] = '0'*40
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_expired_review_with_long_approval_is_rejected(self):
        self.approval['expires_at'] = when(300000)
        self.args['approval_sha256'] = self.f.write(self.args['approval_path'], self.approval)
        self.args['clock'] = lambda: NOW + dt.timedelta(days=2)
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
    def test_remote_parent_blob_file_and_pr_bindings_are_required(self):
        for kind in ('parent', 'blob', 'file', 'pr-head', 'pr-base', 'draft'):
            with self.subTest(kind=kind):
                self.setUp(); actual = self.api.request
                def request(method, path, body=None):
                    value = actual(method, path, body)
                    if method == 'POST' and path.endswith('/git/blobs') and kind == 'blob': value['sha'] = 'f'*40
                    if method == 'GET' and path.endswith('/git/commits/'+'c'*40) and kind == 'parent': value['parents'] = [{'sha': 'f'*40}]
                    if method == 'GET' and '/pulls/123/files?' in path and kind == 'file': value[0]['sha'] = 'f'*40
                    if method == 'GET' and path.endswith('/pulls/123'):
                        if kind == 'pr-head': value['head']['sha'] = 'f'*40
                        if kind == 'pr-base': value['base']['sha'] = 'f'*40
                        if kind == 'draft': value['draft'] = False
                    return value
                self.api.request = request
                with self.assertRaises(Rejected): self.run_publish()
                self.assertNotIn('"state":"PR_CREATED"', ''.join(p.read_text() for p in self.args['state_root'].glob('*.json')))
                if kind in ('parent', 'blob'): self.assertNotIn('/git/refs', self.api.posts)
    def test_actual_gh_subprocess_adapter_connects_to_full_pipeline(self):
        config = self.f.root/'gh'; config.mkdir(mode=0o700)
        transport = GitHubHostTransport(gh_config_dir=config, gh_path='/usr/bin/gh')
        def run(args, **kwargs):
            method = args[args.index('--method')+1]
            path = args[args.index('--method')+2]
            body = json.loads(kwargs['input']) if kwargs.get('input') else None
            value = self.api.request(method, path, body)
            self.assertEqual(set(kwargs['env']), {'PATH','HOME','GH_CONFIG_DIR','GH_HOST','GH_PROMPT_DISABLED','GIT_TERMINAL_PROMPT'})
            if value is None: return subprocess.CompletedProcess(args, 1, canonical({'message':'Not Found','status':'404'}), b'')
            return subprocess.CompletedProcess(args, 0, canonical(value), b'')
        self.args['transport'] = transport
        with patch('subprocess.run', side_effect=run):
            self.assertEqual(self.run_publish()['state'], 'PR_CREATED')
    def test_auth_failure_is_not_branch_absence_or_success(self):
        config = self.f.root/'gh'; config.mkdir(mode=0o700)
        transport = GitHubHostTransport(gh_config_dir=config, gh_path='/usr/bin/gh')
        for code in ('401','403','500'):
            with self.subTest(code=code), patch('subprocess.run', return_value=subprocess.CompletedProcess([],1,canonical({'message':'Not Found','status':code}),b'private diagnostics')):
                with self.assertRaises(TransportError): transport.request('GET','/repos/'+REPOSITORY+'/git/ref/heads/example')

    def test_host_context_is_required(self):
        self.args['context'] = 'model'
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, [])
        with self.assertRaises(Rejected): GitHubHostTransport(gh_config_dir='/nonexistent', context='sandbox')
    def test_existing_runner_boundary_excludes_host_credentials(self):
        from tools.engineering_runner.sandbox import Sandbox
        go = self.f.root/'go'; go.mkdir()
        config = self.f.root/'host-gh'; config.mkdir(mode=0o700)
        argv = Sandbox(self.f.run/'candidate', go).argv(['/usr/bin/true'])
        self.assertIn('--unshare-all', argv); self.assertIn('--clearenv', argv)
        self.assertNotIn(str(config), argv)
        home = next(argv[i+2] for i, v in enumerate(argv) if v == '--setenv' and argv[i+1] == 'HOME')
        self.assertEqual(home, '/home/runner')
        with patch.object(Path, 'home', return_value=Path(home)):
            with self.assertRaises(Rejected): GitHubHostTransport(gh_config_dir=config, gh_path='/usr/bin/gh')

    def test_host_transport_removes_credential_environment_and_uses_stdin(self):
        config = self.f.root/'gh'; config.mkdir(mode=0o700)
        with patch.dict(os.environ, {'GH_TOKEN':'do-not-inherit','GITHUB_TOKEN':'do-not-inherit'}):
            transport = GitHubHostTransport(gh_config_dir=config, gh_path='/usr/bin/gh')
        with patch('subprocess.run', return_value=subprocess.CompletedProcess([], 0, b'{}', b'')) as call:
            transport.request('POST', '/repos/'+REPOSITORY+'/git/blobs', {'content':'source'})
            args, kwargs = call.call_args
            self.assertNotIn('GH_TOKEN', kwargs['env']); self.assertNotIn('GITHUB_TOKEN', kwargs['env'])
            self.assertNotIn('source', args[0]); self.assertIn(b'source', kwargs['input'])
        with self.assertRaises(Rejected): transport.request('POST', '/repos/'+REPOSITORY+'/pulls/123/merge', {})


if __name__ == '__main__': unittest.main()
