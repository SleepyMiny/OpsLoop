#!/usr/bin/env python3
"""Run local M3 checks using installed tools/caches; never installs, deploys or calls AI."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]


def now():
    return datetime.now(timezone.utc).isoformat()


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def stop_group(process, graceful=False):
    """Terminate the entire check, including test servers and helper children."""
    try:
        os.killpg(process.pid, signal.SIGTERM if graceful else signal.SIGKILL)
    except ProcessLookupError:
        pass
    if graceful:
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            pass
        # The group may still contain children even after its leader exits.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait()


def execute(name, argv, cwd, env, output, timeout):
    started, tick = now(), time.monotonic()
    path = output / (name + ".log")
    with path.open("wb") as log:
        try:
            process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                       stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                code = process.wait(timeout=timeout)
                status = "PASS" if code == 0 else "FAIL"
            except subprocess.TimeoutExpired:
                stop_group(process)
                code, status = None, "TIMEOUT"
            except KeyboardInterrupt:
                stop_group(process, graceful=True)
                code, status = None, "INTERRUPTED"
        except OSError as error:
            log.write((type(error).__name__ + ": executable unavailable\n").encode())
            code, status = None, "NOT_RUN"
    data = path.read_text(errors="replace")
    # The runner does not inherit auth variables. Redact recognizable credentials
    # defensively before retaining local diagnostics; never print their values.
    data = re.sub(r"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{70,})\b", "[REDACTED]", data)
    path.write_text(data)
    result = {"name": name, "command": argv, "cwd": str(cwd), "status": status,
              "exitCode": code, "startedAt": started, "completedAt": now(),
              "durationSeconds": round(time.monotonic() - tick, 3), "log": path.name,
              "logSHA256": sha(path), "scope": "local"}
    if name.startswith("go-test") or name.startswith("go-race"):
        events = []
        for line in data.splitlines():
            try:
                event = json.loads(line)
                if isinstance(event, dict):
                    events.append(event)
            except ValueError:
                pass
        result["testEvents"] = {key: sum(e.get("Action") == key and "Test" in e for e in events)
                                for key in ["pass", "fail", "skip"]}
        result["skippedTests"] = [{"package": e.get("Package"), "test": e["Test"]}
                                  for e in events if e.get("Action") == "skip" and "Test" in e]
    print(name + ": " + status, flush=True)
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--go", default=shutil.which("go") or "go")
    p.add_argument("--node", default=shutil.which("node") or "node")
    p.add_argument("--gomodcache", type=Path, required=True)
    p.add_argument("--gocache", type=Path)
    p.add_argument("--envtest-assets", type=Path)
    p.add_argument("--output", type=Path, default=ROOT / "reports/m3/checks" / datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
    p.add_argument("--only", action="append", default=[], help="Exact check name; repeat to select several")
    p.add_argument("--go-package", action="append", default=[], help="Go package selector for vet/test/race; repeat; default ./...")
    p.add_argument("--list", action="store_true")
    p.add_argument("--timeout", type=int, default=3600, help="Per-check seconds")
    a = p.parse_args()
    go, node = str(Path(a.go).resolve()), str(Path(a.node).resolve())
    output = a.output.resolve()
    build = output / "build"
    frontend = ROOT / "frontend/dashboard"
    cli = build / "demo-driver"
    packages = a.go_package or ["./..."]
    if any(not package.startswith("./") or any(part == ".." for part in Path(package).parts) for package in packages):
        p.error("--go-package must select packages within this repository, such as ./cmd/demo-driver")
    tasks = [
        ("source-scan", [sys.executable, str(ROOT / "scripts/m3-source-scan.py"), "--output", str(output / "source-scan.json")], ROOT, {}),
        ("build-cli", [go, "build", "-o", str(cli), "./cmd/demo-driver"], ROOT, {}),
        ("go-vet", [go, "vet", *packages], ROOT, {}),
        ("go-test", [go, "test", "-json", "-timeout=45m", "-count=1", *packages], ROOT, {}),
        ("go-race", [go, "test", "-json", "-race", "-timeout=45m", "-count=1", *packages], ROOT, {}),
        ("python", [sys.executable, "-B", "-m", "unittest", "discover", "-s", "tests", "-v"], ROOT, {}),
        ("frontend-typecheck", [node, "node_modules/typescript/bin/tsc", "--noEmit", "-p", "tsconfig.json"], frontend, {}),
        ("frontend-test", [node, "node_modules/vitest/vitest.mjs", "run"], frontend, {}),
        ("frontend-build", [node, "node_modules/vite/bin/vite.js", "build"], frontend, {}),
    ]
    for arch in ["amd64", "arm64"]:
        for app in ["cmd/opsloop-controller", "cmd/dashboard-bff", "apps/demo-api", "apps/engineering-demo"]:
            name = "build-" + arch + "-" + app.split("/")[-1]
            tasks.append((name, [go, "build", "-trimpath", "-o", str(build / name), "./" + app], ROOT,
                          {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}))
    if a.only:
        unknown = set(a.only) - {t[0] for t in tasks}
        if unknown:
            p.error("unknown checks: " + ", ".join(sorted(unknown)))
        tasks = [t for t in tasks if t[0] in a.only or (t[0] == "build-cli" and any(x.startswith("go-test") or x.startswith("go-race") for x in a.only))]
    if a.list:
        print(json.dumps([{"name": t[0], "command": t[1]} for t in tasks], indent=2))
        return 0
    if output.exists() and (output / "verification.json").exists():
        p.error("output already contains verification.json; use a fresh output directory")
    for directory in [output, build, output / "tmp", output / "gopath", a.gocache or output / "gocache"]:
        directory.mkdir(parents=True, exist_ok=True)
    env = {"PATH": str(Path(go).parent) + os.pathsep + str(Path(node).parent) + ":/usr/bin:/bin",
           "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TMPDIR": str(output / "tmp"),
           "GOTOOLCHAIN": "local", "GOENV": "off", "GOTELEMETRY": "off", "GOPROXY": "off", "GOSUMDB": "off",
           "GOCACHE": str((a.gocache or output / "gocache").resolve()), "GOMODCACHE": str(a.gomodcache.resolve()),
           "GOPATH": str(output / "gopath"), "GOFLAGS": "-mod=readonly -p=4 -buildvcs=false",
           "GOMAXPROCS": "4", "CGO_ENABLED": "1", "GIT_OPTIONAL_LOCKS": "0",
           "KUBECONFIG": str(output / "NO_KUBECONFIG"), "USE_EXISTING_CLUSTER": "false",
           "OPSLOOP_VERIFY_GO": go, "PYTHONDONTWRITEBYTECODE": "1", "CI": "true"}
    for key in ["P2C_CLI", "P2D_CLI", "P2E_CLI", "P2F_CLI", "P3B_CLI", "WORKLOAD_SAFETY_CLI"]:
        env[key] = str(cli)
    if a.envtest_assets:
        env["KUBEBUILDER_ASSETS"] = str(a.envtest_assets.resolve())
        env["OPSLOOP_REQUIRE_ENVTEST"] = "1"
    receipt = {"schema": "opsloop-m3-local-verification/v1", "startedAt": now(), "checks": [],
               "goPackages": packages,
               "environment": env, "remoteCI": "NOT_RUN", "deployment": "NOT_RUN", "nativeARM64": "NOT_RUN",
               "networkBoundary": "offline dependency resolution; tests may bind loopback; no kernel egress isolation claimed",
               "envtest": "REQUESTED" if a.envtest_assets else "NOT_RUN", "toolchain": {}}
    for name, argv in [("go", [go, "version"]), ("node", [node, "--version"]), ("python", [sys.executable, "--version"])]:
        r = execute("version-" + name, argv, ROOT, env, output, 30)
        receipt["toolchain"][name] = {"path": argv[0], "version": (output / r["log"]).read_text().strip(), "status": r["status"]}
    for name, argv, cwd, additions in tasks:
        receipt["checks"].append(execute(name, argv, cwd, {**env, **additions}, output, a.timeout))
        receipt["status"] = "PASS" if all(c["status"] == "PASS" for c in receipt["checks"]) else "FAIL"
        receipt["completedAt"] = now()
        (output / "verification.json").write_text(json.dumps(receipt, indent=2) + "\n")
        if receipt["checks"][-1]["status"] == "INTERRUPTED":
            break
    receipt["binaries"] = {f.name: sha(f) for f in build.iterdir() if f.is_file()}
    (output / "verification.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(str(output / "verification.json"))
    if any(c["status"] == "INTERRUPTED" for c in receipt["checks"]):
        return 130
    return 0 if receipt["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
