"""Immutable independent HTTP oracle, build, regression and source security gate.

Invoked inside a credential-free, network-isolated verifier sandbox. The only
permitted network is its private loopback; no inherited proxy/environment.
This file must be mounted read-only, outside the AI's source allowlist.
"""
import hashlib
import http.client
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time


def main():
    app = Path.cwd()
    go = Path(os.environ.get("GOROOT", "/opt/go")) / "bin/go"
    harness = Path(__file__).resolve().parent
    env = {"PATH": str(go.parent) + ":/usr/bin:/bin", "HOME": "/tmp",
           "GO111MODULE": "off", "GOROOT": str(go.parent.parent),
           "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
           "GOTELEMETRY": "off", "GOENV": "off", "GOMAXPROCS": "2",
           "CGO_ENABLED": "0", "GOCACHE": os.environ.get("GOCACHE", "/tmp/go-cache")}
    checks = []
    reproduced = False

    def command(name, argv):
        try:
            r = subprocess.run(argv, cwd=app, env=env, stdin=subprocess.DEVNULL,
                               capture_output=True, timeout=90)
            checks.append({"name": name, "passed": r.returncode == 0,
                           "exit_code": r.returncode,
                           "output_sha256": hashlib.sha256(r.stdout + r.stderr).hexdigest(),
                           "output": (r.stdout + r.stderr).decode(errors="replace")[-8000:]})
            return r.returncode == 0
        except subprocess.TimeoutExpired:
            checks.append({"name": name, "passed": False, "error": "timeout"})
            return False

    def request(method, path, data=None):
        c = http.client.HTTPConnection("127.0.0.1", 18080, timeout=2)
        try:
            c.request(method, path, data, {"Content-Type": "application/json"})
            r = c.getresponse()
            raw = r.read(16385)
            if len(raw) > 16384: raise ValueError("oversized response")
            return r.status, json.loads(raw)
        finally:
            c.close()

    security = command("source-security", [str(go), "run", str(harness / "audit.go"), str(app)])
    if security:
        command("unit", [str(go), "test", "-count=1", "-timeout=20s", "."])
        command("static", [str(go), "vet", "."])
        with tempfile.TemporaryDirectory(prefix="opsloop-verifier-") as work:
            binary = Path(work) / "app"
            built = command("build-amd64", [str(go), "build", "-trimpath", "-o", str(binary), "."])
            # Cross-build is recorded as cross-build, never as ARM native E2E.
            saved = env.get("GOARCH")
            env["GOARCH"] = "arm64"
            command("cross-build-arm64", [str(go), "build", "-trimpath", "-o", str(Path(work) / "app-arm64"), "."])
            if saved is None: env.pop("GOARCH")
            else: env["GOARCH"] = saved
            if built:
                log = open(Path(work) / "server.log", "wb")
                p = subprocess.Popen([str(binary)], env=env, stdin=subprocess.DEVNULL,
                                     stdout=log, stderr=log)
                try:
                    started = False
                    startup_result = None
                    for _ in range(60):
                        try:
                            startup_result = request("GET", "/health")
                            started = startup_result == (200, {"status": "ok"})
                            if started: break
                        except (OSError, ValueError, http.client.HTTPException) as e: startup_result = str(e)
                        time.sleep(0.05)
                    checks.append({"name": "startup", "passed": started, "process_exit":p.poll(), "observed":startup_result})
                    if started:
                        fixture = '{"items":[]}'
                        status, body = request("POST", "/quote", fixture)
                        reproduced = status == 500 and body.get("error") == "QUOTE_DIVISION_BY_ZERO"
                        checks.append({"name": "empty-order-regression", "passed": (status, body) == (200, {"total": 0, "average": 0}),
                                       "status": status, "body": body,
                                       "fixture_sha256": hashlib.sha256(fixture.encode()).hexdigest()})
                        for name, payload, code, want in [
                            ("normal-order", '{"items":[{"unitPrice":150,"quantity":2},{"unitPrice":300,"quantity":1}]}', 200, {"total":600,"average":200}),
                            ("zero-price", '{"items":[{"unitPrice":0,"quantity":1}]}', 200, {"total":0,"average":0}),
                            ("fraction-truncation", '{"items":[{"unitPrice":1,"quantity":2},{"unitPrice":2,"quantity":1}]}', 200, {"total":4,"average":1}),
                            ("negative-price", '{"items":[{"unitPrice":-1,"quantity":1}]}', 400, None),
                            ("zero-quantity", '{"items":[{"unitPrice":1,"quantity":0}]}', 400, None),
                            ("malformed-json", '{', 400, {"error":"INVALID_ORDER"}),
                            ("oversize-request", 'x'*8193, 400, {"error":"INVALID_ORDER"}),
                        ]:
                            actual, result = request("POST", "/quote", payload)
                            checks.append({"name": name, "passed": actual == code and (want is None or result == want), "status": actual})
                        checks.append({"name":"health-after-reproduction", "passed":request("GET", "/health") == (200,{"status":"ok"})})
                except (OSError, ValueError, http.client.HTTPException) as e:
                    checks.append({"name":"http-harness", "passed":False, "error":type(e).__name__})
                finally:
                    p.terminate()
                    try: p.wait(timeout=3)
                    except subprocess.TimeoutExpired: p.kill(); p.wait()
                    log.close()
                    if not started:
                        checks.append({"name":"server-startup-log", "passed":False, "output":(Path(work) / "server.log").read_text()[-2000:]})
    # A random build failure cannot be counted as the expected baseline failure.
    baseline = reproduced and all(c["passed"] for c in checks if c["name"] != "empty-order-regression")
    result = {"schema":"opsloop-independent-verification/v1", "baseline_defect_reproduced":baseline,
              "all_passed": bool(checks) and all(c["passed"] for c in checks), "checks":checks,
              "scope":"local native amd64 HTTP; arm64 cross-build only; remote CI not executed"}
    print(json.dumps(result, sort_keys=True))
    return 0 if result["all_passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
