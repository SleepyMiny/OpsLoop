"""Offline real Publisher -> host Coordinator -> GitHub GET proof contract tests."""
import copy
import datetime as dt
import json
import os
from pathlib import Path
import stat
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
        self.base_sha = plan['base_commit']; self.base_reads = 0
        self.before_final_base = None; self.after_final_base = None; self.requests = []
        import hashlib
        self.original = []
        for name in ('discount.go', 'discount_test.go', 'main.go'):
            path = 'apps/engineering-demo/' + name; data = (baseline / path).read_bytes()
            self.original.append({'path': path, 'mode': '100644', 'type': 'blob',
                                  'sha': hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()})
        self.original += [{'path': 'unrelated.txt', 'mode': '100644', 'type': 'blob', 'sha': '1' * 40}]
    def request(self, method, path, body=None):
        path = path.removeprefix('/repos/' + REPOSITORY)
        self.requests.append((method, path))
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
                           'base': {'ref': self.plan['base_ref'], 'sha': self.base_sha, 'repo': repo},
                           'head': {'ref': self.plan['head_ref'], 'sha': self.commit_sha, 'repo': repo}}
                result = self.pr
            else: raise AssertionError(path)
            if self.lose_after == path: raise TransportError('synthetic lost successful response')
            return copy.deepcopy(result)
        if not path: return {'full_name': REPOSITORY, 'id': REPOSITORY_ID}
        if path.startswith('/git/ref/heads/'):
            from urllib.parse import unquote
            name = unquote(path.removeprefix('/git/ref/heads/'))
            if name != self.plan['base_ref']: return self.head
            self.base_reads += 1
            if self.base_reads == 2 and self.before_final_base: self.before_final_base()
            response = {'object': {'sha': self.base_sha}}
            if self.base_reads == 2 and self.after_final_base: self.after_final_base()
            return response
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
        # CI's host also uses /home/runner. Positive transport tests model a
        # separate host explicitly; the sandbox-denial test overrides it below.
        host_home = self.f.root / 'host-home'; host_home.mkdir()
        home = patch.object(Path, 'home', return_value=host_home)
        home.start(); self.addCleanup(home.stop)
        self.prep = self.f.arguments(); self.plan = self.f.plan()
        args, self.approval, _ = self.f.receipt_arguments()
        self.api = FakeGitHub(self.plan, self.f.run/"baseline")
        self.args = {k: args[k] for k in ('plan_path', 'plan_sha256', 'approval_path', 'approval_sha256')}
        self.args.update(prepare_arguments=self.prep, state_root=self.f.root/'state', transport=self.api, clock=lambda: NOW)
        # Synthetic explicit user decision, separate from unchanged B1 evidence
        # and the legacy publication approval. No production approval is issued.
        self.risk = {k: self.plan[k] for k in ('repository', 'repository_id', 'base_ref', 'base_commit',
            'base_tree', 'head_ref', 'patch_sha256', 'candidate_source_sha256')}
        self.risk.update(schema_version='opsloop-nonatomic-base-risk-approval/v1', approved=True,
            approved_by='synthetic-test-user', plan_sha256=self.args['plan_sha256'],
            publication_approval_sha256=self.args['approval_sha256'],
            run_id=self.plan['incident']['run_id'], epoch=self.plan['incident']['epoch'],
            issued_at=when(-1), expires_at=when(60), allowed_remote_side_effects={
                'reviewed_git_objects': True, 'approved_head_ref': True, 'draft_pr_count': 1,
                'base_ref_drift_draft': True, 'automatic_close_delete_retarget_merge': False})
        self.args['risk_approval_path'] = self.f.root/'risk-approval.json'
        self.args['risk_approval_sha256'] = self.f.write(self.args['risk_approval_path'], self.risk)
    def run_publish(self): return publish(**self.args)
    def test_real_prepare_to_remote_get_proof(self):
        result = self.run_publish()
        self.assertEqual(result['state'], 'PR_CREATED')
        self.assertFalse(result['merged']); self.assertFalse(result['deployed'])
        self.assertEqual(self.api.posts, ['/git/blobs', '/git/blobs', '/git/trees', '/git/commits', '/git/refs', '/pulls'])
        self.assertTrue(all((p.stat().st_mode & 0o077) == 0 for p in self.args['state_root'].iterdir()))
        self.assertNotIn('nil/empty-slice', self.plan['pull_request']['body'])
    def test_restart_does_not_send_again(self):
        result = self.run_publish(); before = list(self.api.posts)
        receipt = next(self.args['state_root'].glob('*-remote.json'))
        receipt_bytes = receipt.read_bytes()
        self.assertTrue(self.journal()['remote_observation_started'])
        with self.assertRaisesRegex(Rejected, 'prior PR observation requires manual review'):
            self.run_publish()
        self.assertEqual(before, self.api.posts)
        self.assertEqual(self.journal()['state'], 'RECONCILIATION_REQUIRED')
        self.assertEqual(self.journal()['result'], result)
        self.assertEqual(receipt.read_bytes(), receipt_bytes)
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
                if field == 'base_commit': self.api.base_sha = 'f'*40
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
    def journal(self):
        return next(json.loads(p.read_bytes()) for p in self.args['state_root'].glob('*.json')
                    if 'operations' in json.loads(p.read_bytes()))

    def journal_path(self):
        return next(p for p in self.args['state_root'].glob('*.json')
                    if 'operations' in json.loads(p.read_bytes()))

    def fail_save(self, actual, path, value, failure):
        """Inject process exceptions or syscall errors, not real power loss."""
        from tools import engineering_publish_coordinator as coordinator
        if failure == 'before-save': raise OSError('synthetic save failure')
        if failure == 'exit-before-save': raise SystemExit('synthetic termination')
        if failure == 'exit-after-save':
            actual(path, value)
            raise SystemExit('synthetic termination')
        if failure == 'replace':
            with patch.object(coordinator.os, 'replace', side_effect=OSError('synthetic replace failure')):
                return actual(path, value)
        real_fsync = coordinator.os.fsync
        def fsync(fd):
            directory = stat.S_ISDIR(os.fstat(fd).st_mode)
            if directory == (failure == 'directory-fsync'):
                raise OSError('synthetic fsync failure')
            return real_fsync(fd)
        self.assertIn(failure, ('file-fsync', 'directory-fsync'))
        with patch.object(coordinator.os, 'fsync', side_effect=fsync):
            return actual(path, value)

    def assert_diagnostic_restart(self):
        before = list(self.api.posts); start = len(self.api.requests)
        self.api.base_sha = self.plan['base_commit']
        self.api.pr['base']['sha'] = self.plan['base_commit']
        with self.assertRaisesRegex(Rejected, 'manual review'):
            self.run_publish()
        self.assertEqual(self.api.posts, before)
        self.assertTrue(self.api.requests[start:])
        self.assertTrue(all(method == 'GET' for method, _ in self.api.requests[start:]))
        self.assertNotEqual(self.journal()['state'], 'PR_CREATED')

    def test_lost_quarantine_save_never_upgrades_restored_base(self):
        from tools import engineering_publish_coordinator as coordinator
        for failure in ('before-save', 'file-fsync', 'replace', 'directory-fsync',
                        'exit-before-save', 'exit-after-save'):
            with self.subTest(failure=failure):
                self.setUp(); actual = coordinator._save; seen = []
                self.api.after_final_base = lambda: setattr(self.api, 'base_sha', 'f'*40)
                def save(path, value):
                    if value.get('state') == 'QUARANTINED_REMOTE_PR':
                        seen.append(copy.deepcopy(value['remote_pr']))
                        return self.fail_save(actual, path, value, failure)
                    return actual(path, value)
                with patch.object(coordinator, '_save', side_effect=save):
                    with self.assertRaises((OSError, SystemExit)): self.run_publish()
                self.assertEqual(seen[0]['observed']['base']['sha'], 'f'*40)
                self.assertTrue(self.journal()['remote_observation_started'])
                self.assertEqual(self.journal()['operations']['pr']['state'], 'CONFIRMED')
                self.assertNotIn('result', self.journal())
                self.assertEqual(self.api.posts.count('/pulls'), 1)
                self.assert_diagnostic_restart()
                self.assert_diagnostic_restart()  # The diagnostic itself cannot clear the latch.

    def test_directory_fsync_uncertainty_allows_either_surviving_journal(self):
        from tools import engineering_publish_coordinator as coordinator
        for phase in ('quarantine', 'final-success'):
            for survivor in ('before-replace', 'after-replace'):
                with self.subTest(phase=phase, survivor=survivor):
                    self.setUp(); actual = coordinator._save; previous = []
                    target = 'QUARANTINED_REMOTE_PR' if phase == 'quarantine' else 'PR_CREATED'
                    if phase == 'quarantine':
                        self.api.after_final_base = lambda: setattr(self.api, 'base_sha', 'f'*40)
                    def save(path, value):
                        if value.get('state') == target:
                            previous.append(path.read_bytes())
                            return self.fail_save(actual, path, value, 'directory-fsync')
                        return actual(path, value)
                    with patch.object(coordinator, '_save', side_effect=save):
                        with self.assertRaises(OSError): self.run_publish()
                    self.assertEqual(self.journal()['state'], target)  # replace was visible.
                    if survivor == 'before-replace':
                        # Model the older surviving directory entry; this is not
                        # an actual filesystem crash/power-loss experiment.
                        self.journal_path().write_bytes(previous[0])
                    self.assertTrue(self.journal()['remote_observation_started'])
                    self.assert_diagnostic_restart()

    def test_observation_barrier_precedes_get_and_interrupted_start_is_safe(self):
        from tools import engineering_publish_coordinator as coordinator
        for failure in ('before-save', 'file-fsync', 'replace', 'directory-fsync',
                        'exit-before-save', 'exit-after-save'):
            with self.subTest(failure=failure):
                self.setUp(); actual = coordinator._save; hit = []
                def save(path, value):
                    if value.get('remote_observation_started') and not hit:
                        hit.append(True)
                        self.assertFalse(any(p.endswith('/pulls/123') for _, p in self.api.requests))
                        return self.fail_save(actual, path, value, failure)
                    return actual(path, value)
                with patch.object(coordinator, '_save', side_effect=save):
                    with self.assertRaises((OSError, SystemExit)): self.run_publish()
                self.assertTrue(hit)
                self.assertFalse(any(p.endswith('/pulls/123') for _, p in self.api.requests))
                before = list(self.api.posts)
                if failure in ('directory-fsync', 'exit-after-save'):
                    self.assert_diagnostic_restart()
                else:
                    # No PR observation was started, so GET-only completion is
                    # safe, just like a failed CONFIRMED save after the POST.
                    self.assertFalse(self.journal()['remote_observation_started'])
                    self.assertEqual(self.run_publish()['state'], 'PR_CREATED')
                self.assertEqual(self.api.posts, before)

    def test_final_success_save_failure_cannot_be_reconfirmed_after_restart(self):
        from tools import engineering_publish_coordinator as coordinator
        for failure in ('before-save', 'file-fsync', 'replace', 'directory-fsync',
                        'exit-before-save', 'exit-after-save'):
            with self.subTest(failure=failure):
                self.setUp(); actual = coordinator._save
                def save(path, value):
                    if value.get('state') == 'PR_CREATED':
                        return self.fail_save(actual, path, value, failure)
                    return actual(path, value)
                with patch.object(coordinator, '_save', side_effect=save):
                    with self.assertRaises((OSError, SystemExit)): self.run_publish()
                self.assertTrue(self.journal()['remote_observation_started'])
                receipt = next(self.args['state_root'].glob('*-remote.json'))
                receipt_bytes = receipt.read_bytes()
                self.assert_diagnostic_restart()
                self.assertEqual(receipt.read_bytes(), receipt_bytes)

    def test_completed_intent_reobservation_cannot_lose_mismatch(self):
        from tools import engineering_publish_coordinator as coordinator
        result = self.run_publish(); actual = coordinator._save
        receipt = next(self.args['state_root'].glob('*-remote.json'))
        receipt_bytes = receipt.read_bytes()
        self.api.pr['base']['sha'] = 'f'*40
        def save(path, value):
            if value.get('state') == 'QUARANTINED_REMOTE_PR':
                raise OSError('synthetic lost quarantine')
            return actual(path, value)
        with patch.object(coordinator, '_save', side_effect=save):
            with self.assertRaises(OSError): self.run_publish()
        self.assertEqual(self.journal()['state'], 'PR_CREATED')
        self.assert_diagnostic_restart()
        self.assertEqual(self.journal()['result'], result)
        self.assertEqual(receipt.read_bytes(), receipt_bytes)

    def test_legacy_journal_is_diagnostic_only_and_original_bytes_are_preserved(self):
        for state, op_state in (('INTENT', 'CONFIRMED'), ('INTENT', 'DISPATCHED'),
                               ('RECONCILIATION_REQUIRED', 'DISPATCHED'),
                               ('BLOCKED_BEFORE_DISPATCH', 'NOT_SENT'),
                               ('QUARANTINED_REMOTE_PR', 'CONFIRMED'),
                               ('PR_CREATED', 'CONFIRMED')):
            with self.subTest(state=state, op_state=op_state):
                self.setUp(); self.run_publish()
                path = self.journal_path(); old = self.journal()
                del old['observation_protocol']; del old['remote_observation_started']
                old['state'] = state; old['operations']['pr']['state'] = op_state
                original = (json.dumps(old, indent=2) + '\n').encode()
                path.write_bytes(original); before = list(self.api.posts)
                start = len(self.api.requests)
                with self.assertRaisesRegex(Rejected, 'manual review'): self.run_publish()
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(self.api.posts, before)
                self.assertTrue(all(m == 'GET' for m, _ in self.api.requests[start:]))

    def test_legacy_intent_without_pr_is_never_upgraded(self):
        self.api.fail = '/git/blobs'
        with self.assertRaises(TransportError): self.run_publish()
        old = self.journal(); del old['observation_protocol']; del old['remote_observation_started']
        path = self.journal_path(); original = canonical(old); path.write_bytes(original)
        before = list(self.api.requests)
        with self.assertRaisesRegex(Rejected, 'legacy publication journal'): self.run_publish()
        self.assertEqual(path.read_bytes(), original)
        self.assertEqual(self.api.requests, before)

    def test_expiry_after_observation_stays_diagnostic_even_if_clock_recovers(self):
        for kind in ('approval', 'risk'):
            with self.subTest(kind=kind):
                self.setUp(); current = [NOW]; actual = self.api.request
                self.args['clock'] = lambda: current[0]
                if kind == 'approval':
                    self.approval['expires_at'] = when(1)
                    self.args['approval_sha256'] = self.f.write(self.args['approval_path'], self.approval)
                    self.risk['publication_approval_sha256'] = self.args['approval_sha256']
                else: self.risk['expires_at'] = when(1)
                self.args['risk_approval_sha256'] = self.f.write(self.args['risk_approval_path'], self.risk)
                def request(method, path, body=None):
                    response = actual(method, path, body)
                    if method == 'GET' and path.endswith('/pulls/123'):
                        current[0] = NOW + dt.timedelta(seconds=2)
                    return response
                self.api.request = request
                with self.assertRaisesRegex(Rejected, 'expired'): self.run_publish()
                current[0] = NOW; self.api.request = actual
                self.assert_diagnostic_restart()

    def test_legacy_approval_without_separate_risk_decision_blocks_all_writes(self):
        self.args.pop('risk_approval_path'); self.args.pop('risk_approval_sha256')
        with self.assertRaisesRegex(Rejected, 'separate non-atomic'):
            self.run_publish()
        self.assertEqual(self.api.posts, [])
        self.assertIsNone(self.api.pr)

    def test_risk_approval_requires_exact_scope_identity_and_time(self):
        for field in ('repository', 'repository_id', 'base_ref', 'base_commit', 'base_tree',
                      'head_ref', 'run_id', 'epoch', 'patch_sha256', 'candidate_source_sha256',
                      'publication_approval_sha256', 'plan_sha256', 'approved', 'approved_by',
                      'expires_at', 'allowed_remote_side_effects'):
            with self.subTest(field=field):
                self.setUp()
                self.risk[field] = {'approved': False, 'approved_by': '', 'expires_at': when(0),
                    'allowed_remote_side_effects': {}}.get(field, 'mismatch')
                self.args['risk_approval_sha256'] = self.f.write(self.args['risk_approval_path'], self.risk)
                with self.assertRaises(Rejected): self.run_publish()
                self.assertEqual(self.api.posts, [])

    def test_base_moves_before_final_get_blocks_pr_and_records_not_sent(self):
        self.api.before_final_base = lambda: setattr(self.api, 'base_sha', 'f'*40)
        with self.assertRaisesRegex(Rejected, 'base moved before PR'): self.run_publish()
        self.assertEqual(self.api.posts.count('/pulls'), 0)
        self.assertIsNone(self.api.pr)
        self.assertEqual(self.journal()['state'], 'BLOCKED_BEFORE_DISPATCH')
        self.assertEqual(self.journal()['operations']['pr']['state'], 'NOT_SENT')
        before = list(self.api.posts)
        with self.assertRaises(Rejected): self.run_publish()
        self.assertEqual(self.api.posts, before)

    def test_base_moves_after_final_get_is_get_proven_and_quarantined(self):
        self.api.after_final_base = lambda: setattr(self.api, 'base_sha', 'f'*40)
        with self.assertRaisesRegex(Rejected, 'quarantined'): self.run_publish()
        self.assertEqual(self.api.posts.count('/pulls'), 1)
        self.assertTrue(self.api.pr['draft'])
        journal = self.journal()
        self.assertEqual(journal['state'], 'QUARANTINED_REMOTE_PR')
        self.assertNotIn('result', journal)
        observation = journal['quarantined_pr']
        self.assertEqual(observation['method'], 'GET')
        self.assertEqual(observation['number'], 123)
        self.assertEqual(observation['url'], f'https://github.com/{REPOSITORY}/pull/123')
        self.assertEqual(observation['observed']['base']['sha'], 'f'*40)
        self.assertEqual(observation['expected']['base_commit'], self.plan['base_commit'])
        self.assertEqual(observation['observed']['head']['sha'], self.api.commit_sha)
        before = list(self.api.posts)
        # Even a later manual retarget cannot silently promote this intent.
        self.api.pr['base']['sha'] = self.plan['base_commit']
        with self.assertRaisesRegex(Rejected, 'quarantined'): self.run_publish()
        self.assertEqual(self.api.posts, before)
        self.assertEqual(self.journal()['quarantined_pr'], observation)

    def test_final_get_has_no_artifact_or_journal_io_before_post(self):
        from tools import engineering_publish_coordinator as coordinator
        final_get = [False]; actual_read = coordinator.read_json; actual_save = coordinator._save
        def read(*args, **kwargs):
            self.assertFalse(final_get[0], 'artifact read in final GET/POST gap')
            return actual_read(*args, **kwargs)
        def save(*args, **kwargs):
            self.assertFalse(final_get[0], 'journal write in final GET/POST gap')
            return actual_save(*args, **kwargs)
        self.api.after_final_base = lambda: final_get.__setitem__(0, True)
        actual_request = self.api.request
        def request(method, path, body=None):
            if method == 'POST' and path.endswith('/pulls'):
                self.assertTrue(final_get[0]); final_get[0] = False
            return actual_request(method, path, body)
        self.api.request = request
        with patch.object(coordinator, 'read_json', side_effect=read), patch.object(coordinator, '_save', side_effect=save):
            self.assertEqual(self.run_publish()['state'], 'PR_CREATED')

    def test_expiry_during_final_base_get_blocks_pr_post(self):
        for kind in ('approval', 'risk', 'review'):
            with self.subTest(kind=kind):
                self.setUp(); current = [NOW]
                if kind == 'review':
                    review = json.loads(self.prep['review_path'].read_bytes())
                    review['reviewed_at'] = when(-86400 + 1)
                    self.prep['review_sha256'] = self.f.write(self.prep['review_path'], review)
                if kind == 'approval':
                    self.approval['expires_at'] = when(1)
                    self.args['approval_sha256'] = self.f.write(self.args['approval_path'], self.approval)
                    self.risk['publication_approval_sha256'] = self.args['approval_sha256']
                if kind == 'risk': self.risk['expires_at'] = when(1)
                self.args['risk_approval_sha256'] = self.f.write(self.args['risk_approval_path'], self.risk)
                self.args['clock'] = lambda: current[0]
                self.api.after_final_base = lambda: current.__setitem__(0, NOW + dt.timedelta(seconds=2))
                with self.assertRaises(Rejected): self.run_publish()
                self.assertEqual(self.api.posts.count('/pulls'), 0)

    def test_journal_failure_before_post_never_dispatches(self):
        from tools import engineering_publish_coordinator as coordinator
        actual = coordinator._save
        def save(path, value):
            if value.get('operations', {}).get('pr', {}).get('state') == 'DISPATCHED':
                raise OSError('synthetic durable storage failure')
            actual(path, value)
        with patch.object(coordinator, '_save', side_effect=save):
            with self.assertRaises(OSError): self.run_publish()
        self.assertEqual(self.api.posts.count('/pulls'), 0)

    def test_confirmed_save_failure_restarts_with_get_only(self):
        from tools import engineering_publish_coordinator as coordinator
        for failure in ('before-save', 'file-fsync', 'replace', 'directory-fsync',
                        'exit-before-save', 'exit-after-save'):
            with self.subTest(failure=failure):
                self.setUp(); actual = coordinator._save
                def save(path, value):
                    if value.get('operations', {}).get('pr', {}).get('state') == 'CONFIRMED':
                        return self.fail_save(actual, path, value, failure)
                    return actual(path, value)
                with patch.object(coordinator, '_save', side_effect=save):
                    with self.assertRaises((OSError, SystemExit)): self.run_publish()
                self.assertEqual(self.journal()['operations']['pr']['state'],
                    'CONFIRMED' if failure in ('directory-fsync', 'exit-after-save') else 'DISPATCHED')
                self.assertFalse(self.journal()['remote_observation_started'])
                self.assertFalse(any(p.endswith('/pulls/123') for _, p in self.api.requests))
                before = list(self.api.posts); start = len(self.api.requests)
                self.assertEqual(self.run_publish()['state'], 'PR_CREATED')
                self.assertEqual(self.api.posts, before)
                self.assertTrue(all(m == 'GET' for m, _ in self.api.requests[start:]))

    def test_lost_response_moved_base_reconciles_after_approval_and_review_expiry(self):
        self.api.after_final_base = lambda: setattr(self.api, 'base_sha', 'f'*40)
        self.api.lose_after = '/pulls'
        with self.assertRaises(TransportError): self.run_publish()
        before = list(self.api.posts)
        self.args['clock'] = lambda: NOW + dt.timedelta(days=2)
        with self.assertRaisesRegex(Rejected, 'quarantined'): self.run_publish()
        self.assertEqual(self.journal()['state'], 'QUARANTINED_REMOTE_PR')
        self.assertEqual(self.api.posts, before)

    def test_post_response_without_get_proof_is_unknown_not_success(self):
        actual = self.api.request
        def request(method, path, body=None):
            if method == 'GET' and path.endswith('/pulls/123'):
                raise TransportError('synthetic GET response lost')
            return actual(method, path, body)
        self.api.request = request
        with self.assertRaises(TransportError): self.run_publish()
        before = list(self.api.posts)
        self.assertEqual(self.journal()['state'], 'RECONCILIATION_REQUIRED')
        self.assertNotIn('remote_pr', self.journal())
        with self.assertRaises(TransportError): self.run_publish()
        self.assertEqual(self.api.posts, before)

    def test_risk_approval_cannot_be_replaced_after_uncertain_post(self):
        self.api.lose_after = '/pulls'
        with self.assertRaises(TransportError): self.run_publish()
        before = list(self.api.posts)
        self.risk['expires_at'] = when(120)
        self.args['risk_approval_sha256'] = self.f.write(self.args['risk_approval_path'], self.risk)
        with self.assertRaisesRegex(Rejected, 'risk approval differs'): self.run_publish()
        self.assertEqual(self.api.posts, before)

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
