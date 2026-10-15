"""Execute production reset functions with stateful mocks; never contact a cluster.

The marked function block is extracted verbatim so these tests also reproduce the
original early-exit bug. The separate preflight test runs the entire script with
mock executables. Shell state lives in files because kubectl reads use subshells.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / 'demos/node-failure/run.sh'
HARNESS = r'''
set -euo pipefail
PROBE_PID=""; PRIMARY=mock; NS=mock; POLICY=mock; P_UID=uid
LOAD_PODS=(load1 load2); REPO_ROOT=/mock
printf true > "$RUN_DIR/enabled"
printf 'load1\nload2\n' > "$RUN_DIR/pods"
log() { echo "$*" >&2; }
sleep() { :; }
fails() { [[ ",$FAIL," == *",$1,"* ]]; }
psh() {
 echo "psh $*" >> "$RUN_DIR/calls"
 if [[ "$*" == *' start '* ]]; then ! fails start; else ! fails service; fi
}
k() {
 echo "k $*" >> "$RUN_DIR/calls"
 case "$*" in
 *'wait --for=condition=Ready'*) ! fails ready ;;
 *'delete pod '*)
  local pod=$5
  fails "delete_$pod" && return 1
  if ! fails "remain_$pod"; then
   grep -vx "$pod" "$RUN_DIR/pods" > "$RUN_DIR/remaining" || true
   mv "$RUN_DIR/remaining" "$RUN_DIR/pods"
  fi ;;
 *'get pod '*)
  local pod=$5
  fails "get_$pod" && return 1
  grep -x "$pod" "$RUN_DIR/pods" || true ;;
 *'"enabled":false'*)
  fails disable && return 1
  printf false > "$RUN_DIR/enabled" ;;
 *'"enabled":true'*)
  # Model a remote success followed by an ambiguous transport failure.
  printf true > "$RUN_DIR/enabled"
  ! fails enable ;;
 *'"circuits":null'*) ! fails ledger ;;
 *'get recoverypolicy '*)
  fails get_policy && return 1
  local uid=uid enabled
  enabled=$(cat "$RUN_DIR/enabled")
  if fails replaced; then uid=other; fi
  if fails disable_unconfirmed && [[ "$enabled" == false ]]; then enabled=true; fi
  if fails enable_read && [[ "$enabled" == true ]]; then return 1; fi
  printf '%s|%s' "$uid" "$enabled"
  if [[ "$*" == *activeIncident* ]]; then
   printf '|'
   if fails active; then printf incident; fi
  fi ;;
 'apply '*) ! fails apply ;;
 *) echo "unexpected mock command: $*" >&2; return 98 ;;
 esac
}
single_primary_pod_ready() { echo pod_ready >> "$RUN_DIR/calls"; ! fails pod_ready; }
node_ready() { echo node_ready >> "$RUN_DIR/calls"; ! fails final_ready; }
'''


class ResetTest(unittest.TestCase):
    def run_reset(self, failure='', original=0, extra=''):
        source = SCRIPT.read_text().split('# ---- 6. recovery and reset, armed only after preflight passed ----\n', 1)[1].split('trap reset_all EXIT', 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            if failure == 'evidence':
                Path(tmp, 'reset.tsv').mkdir()
            script = HARNESS + source + extra + '\ntrap reset_all EXIT\nexit "$ORIGINAL"\n'
            p = subprocess.run(['bash', '-c', script], env={**os.environ, 'FAIL': failure, 'RUN_DIR': tmp, 'ORIGINAL': str(original)}, capture_output=True, text=True, timeout=10)
            calls = Path(tmp, 'calls').read_text().splitlines()
            report = Path(tmp, 'reset.tsv')
            evidence = report.read_text() if report.is_file() else ''
            enabled = Path(tmp, 'enabled').read_text()
            return p, calls, evidence, enabled

    def assert_cleanup_attempts(self, calls):
        self.assertEqual(calls[0], 'psh sudo -n systemctl start k3s-agent')
        self.assertTrue(any('"enabled":false' in c for c in calls), calls)
        for pod in ('load1', 'load2'):
            self.assertTrue(any('delete pod ' + pod in c for c in calls), calls)

    def test_initial_failure_still_cleans(self):
        for failure in ('start', 'ready'):
            with self.subTest(failure=failure):
                p, calls, report, enabled = self.run_reset(failure)
                self.assertNotEqual(p.returncode, 0, p.stderr)
                self.assert_cleanup_attempts(calls)
                self.assertFalse(any('"enabled":true' in c for c in calls), calls)
                self.assertEqual(enabled, 'false')
                self.assertIn('cleanup_exit\t1', report)

    def test_each_failure_path(self):
        for failure in ('service', 'disable', 'disable_unconfirmed', 'get_policy', 'replaced',
                        'delete_load1', 'delete_load2', 'remain_load1', 'remain_load2',
                        'get_load1', 'get_load2', 'active', 'apply', 'pod_ready', 'ledger',
                        'final_ready', 'enable', 'enable_read', 'start,disable,delete_load1', 'evidence'):
            with self.subTest(failure=failure):
                p, calls, report, enabled = self.run_reset(failure)
                self.assertNotEqual(p.returncode, 0, p.stderr)
                self.assert_cleanup_attempts(calls)
                if failure not in ('enable', 'enable_read'):
                    self.assertFalse(any('"enabled":true' in c for c in calls), calls)
                if 'disable' not in failure or failure == 'disable_unconfirmed':
                    self.assertEqual(enabled, 'false', calls)
                if failure != 'evidence':
                    self.assertIn('cleanup_exit\t1', report)
                self.assertIn('reset:', p.stderr)

    def test_success_and_original_exit_preserved(self):
        for original in (0, 7, 130, 143):
            with self.subTest(original=original):
                p, calls, report, enabled = self.run_reset(original=original)
                self.assertEqual(p.returncode, original, p.stderr)
                self.assert_cleanup_attempts(calls)
                self.assertEqual(enabled, 'true')
                self.assertIn('cleanup_exit\t0', report)
                self.assertIn('original_exit\t' + str(original), report)
                self.assertTrue(any('is-active' in c for c in calls))
                self.assertLess(next(i for i,c in enumerate(calls) if '"enabled":false' in c), next(i for i,c in enumerate(calls) if 'wait --for=condition=Ready' in c))

    def test_failed_cleanup_preserves_original_exit(self):
        p, _, _, _ = self.run_reset('start', original=143)
        self.assertEqual(p.returncode, 143)

    def test_stuck_probe_cannot_block_cleanup(self):
        # Mock only process primitives, keeping the actual stop_probe loop. A
        # permanently live probe must receive INT, TERM, KILL without any wait.
        extra = r'''
PROBE_PID=12345
kill() { echo "kill $*" >> "$RUN_DIR/calls"; return 0; }
wait() { echo UNBOUNDED_WAIT >> "$RUN_DIR/calls"; return 99; }
'''
        p, calls, report, enabled = self.run_reset(extra=extra)
        self.assertNotEqual(p.returncode, 0)
        self.assert_cleanup_attempts(calls)
        for signal in ('INT', 'TERM', 'KILL'):
            self.assertIn('kill -' + signal + ' 12345', calls)
        self.assertNotIn('UNBOUNDED_WAIT', calls)
        self.assertIn('stop_probe\t1', report)
        self.assertEqual(enabled, 'false')
        self.assertFalse(any('"enabled":true' in c for c in calls))

    def test_ssh_timeout_still_cleans(self):
        # Use the exact production psh wrapper and a mock timeout transport;
        # exit 124 models GNU timeout, including ambiguous remote completion.
        psh = next(line for line in SCRIPT.read_text().splitlines() if line.startswith('psh()'))
        extra = r'''
OPSLOOP_PRIMARY_SSH=mock
 timeout() {
  echo "timeout $*" >> "$RUN_DIR/calls"
  [[ "$*" != *'systemctl start'* ]] || return 124
 }
''' + psh + '\n'
        p, calls, report, enabled = self.run_reset(extra=extra)
        self.assertNotEqual(p.returncode, 0)
        self.assertTrue(calls[0].startswith('timeout --kill-after=5s 45s ssh '))
        self.assertIn('start_primary\t124', report)
        self.assertTrue(any('delete pod load1' in c for c in calls))
        self.assertTrue(any('delete pod load2' in c for c in calls))
        self.assertEqual(enabled, 'false')
        self.assertFalse(any('"enabled":true' in c for c in calls))

    def test_preflight_failure_is_read_only(self):
        # Run a full copy in a fake owner checkout; all reachable remote commands
        # are mocks. Blank kubectl reads fail the second (cluster) preflight.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            demo = root / 'demos/node-failure'
            demo.mkdir(parents=True)
            shutil.copyfile(SCRIPT, demo / 'run.sh')
            owner = root / 'deploy/owner'
            owner.mkdir(parents=True)
            (owner / 'kustomization.yaml').touch()
            bindir = root / 'bin'
            bindir.mkdir()
            for name in ('kubectl', 'ssh', 'curl', 'timedatectl', 'driver'):
                p = bindir / name
                p.write_text('#!/bin/bash\necho "' + name + ' $*" >> "$MOCK_CALLS"\nexit 0\n')
                p.chmod(0o755)
            env = {k:v for k,v in os.environ.items() if not k.startswith('OPSLOOP_')}
            env.update(PATH=str(bindir) + os.pathsep + os.environ['PATH'], MOCK_CALLS=str(root / 'calls'),
                       OPSLOOP_KUBECTL='kubectl', OPSLOOP_URL='http://mock', OPSLOOP_PRIMARY_SSH='mock',
                       OPSLOOP_TIME_SSH='mock', OPSLOOP_SCENARIO='primary-only', OPSLOOP_PLATFORM_DIGESTS='mock',
                       OPSLOOP_SOURCE_SHA='mock', OPSLOOP_IMAGE_CACHE='warm', OPSLOOP_NETPATH_CMD='true',
                       OPSLOOP_DRIVER=str(bindir / 'driver'), OPSLOOP_ARTIFACT_DIR=str(root / 'artifacts'))
            p = subprocess.run(['bash', str(demo / 'run.sh')], env=env, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(p.returncode, 0)
            self.assertIn('preflight FAILED (cluster untouched)', p.stderr)
            calls = (root / 'calls').read_text()
            self.assertIn('kubectl get node', calls)
            for write in (' patch ', ' delete ', ' apply ', 'systemctl start', 'systemctl stop'):
                self.assertNotIn(write, calls)
            self.assertFalse(list(root.rglob('reset.tsv')))


if __name__ == '__main__':
    unittest.main()
