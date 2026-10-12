"""Run the documented acceptance block using only a temporary kubectl stub."""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class NamespaceAcceptanceTests(unittest.TestCase):
    def run_acceptance(self, outcome):
        document = (ROOT / "docs/runbooks/azure-arm64-deployment.md").read_text()
        blocks = re.findall(r"```bash\n(.*?)\n```", document, re.S)
        block, = [b for b in blocks if 'OPSLOOP_TEST_NS="opsloop-arm64-nettest-' in b]
        with tempfile.TemporaryDirectory(prefix="opsloop-namespace-test-") as folder:
            directory = Path(folder)
            stub = directory / "kubectl_stub.py"
            log = directory / "calls.jsonl"
            stub.write_text('''import json, os, sys
args = sys.argv[1:]
with open(os.environ["OPSLOOP_TEST_LOG"], "a") as log:
    log.write(json.dumps(args) + "\\n")
if args[:2] == ["get", "namespace"]:
    outcome = os.environ["OPSLOOP_TEST_OUTCOME"]
    if outcome == "exists":
        print("namespace/" + args[2])
    elif outcome != "absent":
        if outcome == "error-with-output":
            print("partial response")
        sys.exit(1)
elif "get" in args and "pod" in args:
    print("10.42.1.2")
''')
            # No kubeconfig, Kubernetes credentials, or actual kubectl process.
            prefix = f"OPSLOOP_KUBECTL=({shlex.quote(sys.executable)} {shlex.quote(str(stub))})\n"
            environment = {k: os.environ[k] for k in ("PATH", "TMPDIR") if k in os.environ}
            environment.update(OPSLOOP_TEST_LOG=str(log), OPSLOOP_TEST_OUTCOME=outcome)
            result = subprocess.run(["bash", "-c", prefix + block], cwd=directory,
                                    env=environment, text=True, capture_output=True, timeout=30)
            calls = [json.loads(line) for line in log.read_text().splitlines()]
        return result, calls

    def test_existing_namespace_stops_without_any_resource_mutation(self):
        result, calls = self.run_acceptance("exists")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("이미 존재", result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:2], ["get", "namespace"])

    def test_absent_namespace_allows_acceptance_and_own_cleanup(self):
        result, calls = self.run_acceptance("absent")
        self.assertEqual(result.returncode, 0, result.stderr)
        namespace = calls[0][2]
        self.assertEqual(calls[1], ["create", "namespace", namespace])
        self.assertEqual(sum("run" in c for c in calls), 3)
        self.assertEqual(sum("expose" in c for c in calls), 3)
        self.assertEqual(sum("exec" in c for c in calls), 12)
        self.assertEqual(calls[-1], ["delete", "namespace", namespace, "--wait=true"])

    def test_failed_get_stops_without_any_resource_mutation(self):
        for outcome in ("error-empty-output", "error-with-output"):
            with self.subTest(outcome=outcome):
                result, calls = self.run_acceptance(outcome)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("조회 실패", result.stderr)
                self.assertEqual(len(calls), 1)
                self.assertEqual(calls[0][:2], ["get", "namespace"])


if __name__ == "__main__":
    unittest.main()
