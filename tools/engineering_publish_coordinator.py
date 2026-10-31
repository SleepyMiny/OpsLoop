"""Host-only execution of an exact Publisher plan; no model, merge or deployment.

GitHub credentials remain in the host's existing gh config. The Runner's existing
network/filesystem isolation, not a Python caller label, protects those credentials.
Every write is journaled before dispatch. An uncertain write is reconciled by GET
only, including after restart; it is never automatically sent again.
"""
from __future__ import annotations
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
from urllib.parse import quote

from tools.engineering_publisher import (REPOSITORY, REPOSITORY_ID, instant, prepare,
    read_json, record_pr, require, secret_free, sha, validate_target)
from tools.engineering_runner.contracts import ALLOWED_PATHS, Rejected, canonical, digest, regular_file


class TransportError(Rejected):
    """Request failed or response is uncertain; never includes credentials/body."""


class GitHubHostTransport:
    """Restricted GitHub REST transport using existing host gh authentication."""
    def __init__(self, *, gh_config_dir, gh_path=None, context='host-coordinator'):
        require(context == 'host-coordinator', 'GitHub credentials require host coordinator')
        config = Path(gh_config_dir)
        require(config.is_absolute() and not config.is_symlink(), 'unsafe host credential directory')
        st = config.stat()
        require(st.st_uid == os.getuid() and stat.S_ISDIR(st.st_mode) and not st.st_mode & 0o077,
                'host credential directory must be private')
        require(Path.home() != Path('/home/runner'), 'Runner cannot access host GitHub transport')
        self.gh = gh_path or shutil.which('gh')
        require(self.gh is not None and Path(self.gh).is_absolute(), 'host gh executable missing')
        self.env = {'PATH': '/usr/bin:/bin', 'HOME': str(Path.home()), 'GH_CONFIG_DIR': str(config),
                    'GH_HOST': 'github.com', 'GH_PROMPT_DISABLED': '1', 'GIT_TERMINAL_PROMPT': '0'}

    def request(self, method, path, body=None):
        api = '/repos/' + REPOSITORY
        require(method in ('GET', 'POST') and (path == api or path.startswith(api + '/')),
                'GitHub transport scope rejected')
        require(method == 'GET' or path in {api + p for p in ('/git/blobs', '/git/trees', '/git/commits', '/git/refs', '/pulls')},
                'GitHub write endpoint rejected')
        # No tokens in arguments, inherited environment, stdout logs, or audit files.
        args = [self.gh, 'api', '--hostname', 'github.com', '--method', method, path]
        if body is not None:
            args += ['--input', '-']
        try:
            p = subprocess.run(args, input=canonical(body) if body is not None else None,
                               capture_output=True, env=self.env, timeout=30)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise TransportError('GitHub response unavailable') from exc
        if p.returncode:
            # 404 is absence only for an explicit ref GET; other errors remain errors.
            if method == 'GET' and '/git/ref/heads/' in path:
                try:
                    status = json.loads(p.stdout)
                    if status.get('status') == '404' and status.get('message') == 'Not Found':
                        return None
                except (ValueError, AttributeError):
                    pass
            raise TransportError('GitHub request rejected; remote reconciliation required')
        try:
            return json.loads(p.stdout)
        except ValueError as exc:
            raise TransportError('GitHub response malformed') from exc


def _private_dir(path):
    path = Path(path)
    if not path.exists():
        path.mkdir(mode=0o700, parents=True)
    require(not path.is_symlink(), 'unsafe publication state directory')
    st = path.stat()
    require(st.st_uid == os.getuid() and stat.S_ISDIR(st.st_mode) and not st.st_mode & 0o077,
            'publication state must be owner-only')
    return path


def _save(path, value):
    data = canonical(value)
    fd, tmp = tempfile.mkstemp(prefix='.journal-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data); stream.flush(); os.fsync(stream.fileno())
        os.replace(tmp, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try: os.fsync(directory)
        finally: os.close(directory)
    finally:
        if os.path.exists(tmp): os.unlink(tmp)


def _approval(plan, plan_sha, a, now):
    require(a.get('schema_version') == 'opsloop-publish-approval/v1' and a.get('approved') is True and
            a.get('plan_sha256') == plan_sha, 'exact publication approval missing')
    require(instant(a.get('issued_at')) <= now < instant(a.get('expires_at')), 'publication approval expired')
    for key in ('repository', 'repository_id', 'base_ref', 'head_ref', 'base_commit', 'candidate_source_sha256'):
        require(a.get(key) == plan.get(key), 'publication approval binding mismatch')


def publish(*, plan_path, plan_sha256, approval_path, approval_sha256, prepare_arguments,
            state_root, transport, clock=None, context='host-coordinator'):
    """Execute/reconcile one approved publication using an injected host transport.

    prepare_arguments supplies the original pinned result/review/target, not new
    assertions about candidate content. Tests inject a fake transport and clock.
    """
    require(context == 'host-coordinator', 'publication is host-only')
    clock = clock or (lambda: dt.datetime.now(dt.timezone.utc))
    plan = read_json(plan_path, plan_sha256)
    root = _private_dir(state_root)
    identity = {k: plan.get(k) for k in ('repository', 'bundle_sha256', 'patch_sha256')}
    identity['incident'] = plan.get('incident')
    key = digest(canonical({'repository': identity['repository'], 'patch': identity['patch_sha256'],
                            'run': plan.get('incident', {}).get('run_id'), 'epoch': plan.get('incident', {}).get('epoch')}))
    # One host ledger lock also serializes different plan revisions of a run.
    lockfd = os.open(root / 'publication.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lockfd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        path = root / (key + '.json')
        require(not path.is_symlink(), 'unsafe publication journal')
        journal = json.loads(regular_file(path, 4 * 1024 * 1024)) if path.exists() else {
            'schema_version': 'opsloop-publication-intent/v1', 'identity': identity,
            'plan_sha256': plan_sha256, 'operations': {}, 'state': 'INTENT'}
        require(journal.get('identity') == identity and journal.get('plan_sha256') == plan_sha256,
                'publication already belongs to another exact plan; reconcile original intent')
        _save(path, journal)
        args = dict(prepare_arguments); args.pop('now', None)
        def valid():
            require(prepare(**args, now=clock()) == plan, 'prepared plan differs from original reviewed evidence')
            approval = read_json(approval_path, approval_sha256)
            review = read_json(args['review_path'], args['review_sha256'])
            # All local artifact I/O precedes one fresh clock observation. No
            # further I/O is allowed between these pure checks and dispatch.
            now = clock()
            _approval(plan, plan_sha256, approval, now)
            require(dt.timedelta(0) <= now - instant(review['reviewed_at']) <= dt.timedelta(hours=24),
                    'independent review expired during preparation')
            validate_target(plan['target'], now)
        valid()
        for f in plan['files']:
            require(f['path'] in ALLOWED_PATHS and digest(f['content'].encode()) == f['sha256'], 'publication file changed')
            secret_free(f['content'])
        require(len(plan['files']) == 2 and len({f['path'] for f in plan['files']}) == 2, 'publication file scope changed')
        for text in (plan['pull_request']['title'], plan['pull_request']['body'], plan['commit']['message']): secret_free(text)
        api = '/repos/' + REPOSITORY
        def get(suffix): return transport.request('GET', api + suffix)
        def listing(suffix):
            out = []
            for page in range(1, 101):
                batch = get(suffix + ('&' if '?' in suffix else '?') + f'per_page=100&page={page}')
                require(isinstance(batch, list), 'incomplete GitHub list')
                out.extend(batch)
                if len(batch) < 100: return out
            raise Rejected('GitHub list exceeds bound')
        repo = get('')
        require(repo.get('id') == REPOSITORY_ID and repo.get('full_name') == REPOSITORY, 'remote repository changed')
        base_ref = get('/git/ref/heads/' + quote(plan['base_ref'], safe=''))
        require(base_ref and base_ref.get('object', {}).get('sha') == plan['base_commit'], 'remote base moved')
        base_commit = get('/git/commits/' + plan['base_commit'])
        require(base_commit.get('sha') == plan['base_commit'] and base_commit.get('tree', {}).get('sha') == plan['base_tree'], 'remote base tree mismatch')
        base_tree = get('/git/trees/' + plan['base_tree'] + '?recursive=1')
        require(base_tree.get('sha') == plan['base_tree'] and base_tree.get('truncated') is False, 'base tree incomplete')
        def leaves(tree):
            entries = tree.get('tree', [])
            require(isinstance(entries, list) and len({e['path'] for e in entries}) == len(entries), 'invalid tree paths')
            return {e['path']: (e['mode'], e['type'], e['sha']) for e in entries if e['type'] != 'tree'}
        expected = leaves(base_tree)
        source = Path(args['run_dir']) / 'baseline'
        for name, source_sha in read_json(source.parent / 'result.json', args['result_sha256'])['baseline_source_files'].items():
            data = regular_file(source / name)
            require(digest(data) == source_sha and expected.get(name) ==
                    ('100644', 'blob', hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()),
                    'remote baseline bytes differ from verified source')
        for f in plan['files']:
            require(f['path'] in expected and expected[f['path']][0:2] == ('100644', 'blob'), 'base file type changed')
            expected[f['path']] = ('100644', 'blob', f['git_blob_sha'])
        head_path = '/git/ref/heads/' + quote(plan['head_ref'], safe='')
        head = get(head_path)
        prs = listing('/pulls?state=all')
        binding = plan['incident']
        matching = [pr for pr in prs if pr.get('head', {}).get('ref') == plan['head_ref'] or
                    (plan['patch_sha256'] in (pr.get('body') or '') and
                     (f"`{binding['run_id']}`" in (pr.get('body') or '') or f"`{binding['epoch']}`" in (pr.get('body') or '')))]
        ops = journal['operations']
        known_commit = ops.get('commit', {}).get('response', {}).get('sha')
        require(not matching or 'pr' in ops, 'existing publication must not be duplicated')
        require(head is None or ('ref' in ops and known_commit and head.get('object', {}).get('sha') == known_commit), 'head exists outside this intent')
        uncertain = any(o['state'] != 'CONFIRMED' for o in ops.values())
        def write(name, suffix, body):
            if name in ops:
                require(ops[name]['state'] == 'CONFIRMED', 'uncertain write requires GET reconciliation')
                return ops[name]['response']
            valid()  # after the final preflight I/O, immediately before dispatch
            ops[name] = {'state': 'DISPATCHED', 'method': 'POST', 'path': api + suffix,
                         'input_sha256': digest(canonical(body)), 'dispatched_at': clock().isoformat()}
            _save(path, journal)
            try:
                valid()  # durable journal I/O must not consume approval freshness
                response = transport.request('POST', api + suffix, body)
            except Exception:
                journal['state'] = 'RECONCILIATION_REQUIRED'; _save(path, journal)
                raise TransportError('publication write outcome uncertain; GET only') from None
            ops[name].update(state='CONFIRMED', response=response)
            _save(path, journal)
            return response
        if not uncertain:
            for i, f in enumerate(plan['files']):
                blob = write('blob-' + str(i), '/git/blobs', {'content': f['content'], 'encoding': 'utf-8'})
                require(blob.get('sha') == f['git_blob_sha'], 'remote blob differs')
            tree = write('tree', '/git/trees', {'base_tree': plan['base_tree'], 'tree': [
                {k: f[k] for k in ('path', 'mode', 'type')} | {'sha': f['git_blob_sha']} for f in plan['files']]})
            sha(tree.get('sha'), 40)
            commit = write('commit', '/git/commits', {'message': plan['commit']['message'], 'tree': tree['sha'], 'parents': [plan['base_commit']]})
            known_commit = sha(commit.get('sha'), 40)
            remote_commit = get('/git/commits/' + known_commit)
            require(remote_commit.get('sha') == known_commit and
                    [x.get('sha') for x in remote_commit.get('parents', [])] == [plan['base_commit']] and
                    remote_commit.get('tree', {}).get('sha') == tree['sha'], 'new commit binding mismatch')
            remote_tree = get('/git/trees/' + tree['sha'] + '?recursive=1')
            require(remote_tree.get('sha') == tree['sha'] and remote_tree.get('truncated') is False and
                    leaves(remote_tree) == expected, 'unapproved remote tree before branch publication')
            write('ref', '/git/refs', {'ref': 'refs/heads/' + plan['head_ref'], 'sha': known_commit})
            # Recheck base after branch publication; never open PR against a moved base.
            require(get('/git/ref/heads/' + quote(plan['base_ref'], safe=''))['object']['sha'] == plan['base_commit'], 'base moved before PR')
            write('pr', '/pulls', plan['pull_request'])
        # All final proof is newly observed GET data, never POST assertions.
        head = get(head_path)
        prs = listing('/pulls?state=all')
        matches = [pr for pr in prs if pr.get('head', {}).get('ref') == plan['head_ref']]
        require(head is not None and len(matches) == 1, 'remote publication unconfirmed')
        observed_head = sha(head['object']['sha'], 40)
        require(known_commit == observed_head, 'remote head commit mismatch')
        number = matches[0]['number']; pr = get('/pulls/' + str(number))
        commit = get('/git/commits/' + observed_head)
        tree_sha = sha(commit['tree']['sha'], 40)
        tree = get('/git/trees/' + tree_sha + '?recursive=1')
        require(tree.get('truncated') is False and leaves(tree) == expected, 'remote tree contains unapproved changes')
        files = listing('/pulls/' + str(number) + '/files')
        receipt = {'schema_version': 'opsloop-github-pr-observation/v1', 'evidence_kind': 'observed',
                   'observed_at': clock().isoformat(), 'method': 'GET', 'api_origin': 'https://api.github.com',
                   'path': api + '/pulls/' + str(number), 'response': pr,
                   'head_commit_path': api + '/git/commits/' + observed_head,
                   'head_commit': {'sha': commit['sha'], 'parents': [{'sha': v['sha']} for v in commit['parents']], 'tree_sha': tree_sha},
                   'files_path': api + '/pulls/' + str(number) + '/files', 'files_complete': True, 'files': files,
                   'head_tree_path': api + '/git/trees/' + tree_sha + '?recursive=1', 'head_tree': tree}
        valid()
        receipt_path = root / (key + '-remote.json'); _save(receipt_path, receipt)
        result = record_pr(plan_path, plan_sha256=plan_sha256, approval_path=approval_path,
                           approval_sha256=approval_sha256, remote_path=receipt_path,
                           remote_sha256=digest(canonical(receipt)), expected_head_sha=observed_head, now=clock())
        journal['state'] = 'PR_CREATED'; journal['result'] = result; _save(path, journal)
        return result
    except BlockingIOError as exc:
        raise Rejected('publication coordinator already active') from exc
    finally:
        os.close(lockfd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ('plan-path', 'plan-sha256', 'approval-path', 'approval-sha256', 'inputs-path', 'inputs-sha256', 'state-root', 'gh-config-dir'):
        parser.add_argument('--' + arg, required=True)
    a = vars(parser.parse_args())
    inputs = read_json(a.pop('inputs_path'), a.pop('inputs_sha256'))
    transport = GitHubHostTransport(gh_config_dir=a.pop('gh_config_dir'))
    try:
        result = publish(**a, prepare_arguments=inputs, transport=transport)
    except (Rejected, OSError, ValueError, KeyError) as exc:
        print(json.dumps({'state': 'PUBLISH_BLOCKED', 'error_type': type(exc).__name__})); return 2
    print(json.dumps({'state': result['state'], 'pr_url': result['pr_url']})); return 0


if __name__ == '__main__':
    raise SystemExit(main())
