"""Parse-only security regressions for the immutable Demo B verifier."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class RepairLanguageTests(unittest.TestCase):
    def test_forbidden_references_and_initializers(self):
        go = os.environ.get("OPSLOOP_VERIFY_GO", "go")
        with tempfile.TemporaryDirectory() as temp:
            app = Path(temp) / "app"
            app.mkdir()
            for source in (ROOT / "apps/engineering-demo").glob("*.go"):
                shutil.copyfile(source, app / source.name)
            env = {"PATH": "/usr/bin:/bin", "HOME": temp, "GO111MODULE": "off",
                   "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
                   "GOTELEMETRY": "off", "GOCACHE": os.environ.get("GOCACHE", temp + "/cache"),
                   "CGO_ENABLED": "0", "GOMAXPROCS": "2"}
            audit = Path(temp) / "audit"
            built = subprocess.run([go, "build", "-o", str(audit), str(ROOT / "tests/engineering/audit.go")],
                                   env=env, capture_output=True, timeout=90)
            self.assertEqual(built.returncode, 0, built.stderr.decode())
            baseline = (app / "discount_test.go").read_text()
            valid = subprocess.run([str(audit), str(app)], capture_output=True, timeout=5)
            self.assertEqual(valid.returncode, 0)
            for source in [
                'func TestAlias(t *testing.T) { skip := t.Skip; skip("ignore") }',
                'func TestParen(t *testing.T) { (panic)("stop") }',
                'var hidden = func() int { return 1 }()',
                '//go:linkname hidden syscall.exec\nfunc hidden()',
                'func TestConcurrent(t *testing.T) { go func(){}() }',
                'func TestDefer(t *testing.T) { defer func(){}() }',
            ]:
                with self.subTest(source=source):
                    (app / "discount_test.go").write_text(baseline + "\n" + source + "\n")
                    result = subprocess.run([str(audit), str(app)], capture_output=True, timeout=5)
                    self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
