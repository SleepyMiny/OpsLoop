"""Linux namespace and cgroup boundary. No unrestricted fallback exists."""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import time
import uuid

from .contracts import ALLOWED_PATHS, Rejected, digest, regular_file


@dataclass(frozen=True)
class Limits:
    wall_seconds: int = 180
    memory_bytes: int = 2 * 1024**3
    cpu_percent: int = 100
    processes: int = 96
    output_bytes: int = 2 * 1024**2
    file_bytes: int = 64 * 1024**2

    def validate(self):
        if not (1 <= self.wall_seconds <= 900 and 128 * 1024**2 <= self.memory_bytes <= 4 * 1024**3
                and 1 <= self.cpu_percent <= 200 and 8 <= self.processes <= 256
                and 1024 <= self.output_bytes <= 8 * 1024**2
                and 16 * 1024**2 <= self.file_bytes <= 128 * 1024**2):
            raise Rejected("sandbox limits outside trusted maximum")


@dataclass(frozen=True)
class Sandbox:
    source: Path
    go_root: Path
    harness: Path | None = None
    codex_binary: Path | None = None
    broker_directory: Path | None = None
    runtime_directory: Path | None = None
    writable_source: bool = False

    def argv(self, command: list[str]) -> list[str]:
        if not command or not command[0].startswith("/"):
            raise Rejected("sandbox command must use an absolute trusted executable")
        for p in (self.source, self.go_root):
            if not p.is_absolute() or p.is_symlink() or not p.is_dir():
                raise Rejected("invalid sandbox mount")
        args = ["/usr/bin/bwrap", "--unshare-all", "--die-with-parent", "--new-session",
                "--cap-drop", "ALL", "--clearenv", "--ro-bind", "/usr", "/usr",
                "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/home/runner",
                "--dir", "/run", "--dir", "/etc"]
        for dest in ("/bin", "/sbin", "/lib", "/lib64"):
            path = Path(dest)
            if path.is_symlink():
                args += ["--symlink", os.readlink(path), dest]
            elif path.is_dir():
                args += ["--ro-bind", dest, dest]
        args += ["--ro-bind", str(self.source), "/workspace", "--ro-bind", str(self.go_root), "/opt/go"]
        if self.writable_source:
            for relative in ALLOWED_PATHS:
                regular_file(self.source / relative)
                args += ["--bind", str(self.source / relative), "/workspace/" + relative]
        if self.harness:
            args += ["--ro-bind", str(self.harness), "/harness"]
        if self.codex_binary:
            args += ["--ro-bind", str(self.codex_binary), "/opt/codex"]
        if self.runtime_directory:
            args += ["--ro-bind", str(self.runtime_directory), "/runner"]
        if self.broker_directory:
            # Contains only this run's AF_UNIX capability socket, never credentials.
            args += ["--ro-bind", str(self.broker_directory), "/broker"]
        env = {"HOME": "/home/runner", "CODEX_HOME": "/tmp/codex", "PATH": "/opt/go/bin:/usr/bin:/bin",
               "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TMPDIR": "/tmp", "GO111MODULE": "off",
               "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOCACHE": "/tmp/go-cache",
               "GOPATH": "/tmp/go-path", "GOMAXPROCS": "2", "CGO_ENABLED": "0", "PYTHONDONTWRITEBYTECODE": "1"}
        for name, value in env.items():
            args += ["--setenv", name, value]
        args += ["--chdir", "/workspace/apps/engineering-demo", "--", *command]
        return args


def systemd_argv(unit: str, command: list[str], limits: Limits) -> list[str]:
    limits.validate()
    if not re.fullmatch(r"opsloop-eb-[a-z0-9-]{1,80}", unit):
        raise Rejected("invalid sandbox unit")
    return ["/usr/bin/systemd-run", "--user", "--quiet", "--wait", "--pipe", "--collect",
            "--unit=" + unit, "--property=MemoryMax=" + str(limits.memory_bytes),
            "--property=MemorySwapMax=0", "--property=CPUQuota=" + str(limits.cpu_percent) + "%",
            "--property=TasksMax=" + str(limits.processes),
            "--property=RuntimeMaxSec=" + str(limits.wall_seconds), "--property=KillMode=control-group",
            "--property=TimeoutStopSec=5", "--property=SendSIGKILL=yes", "--property=LimitCORE=0",
            "--property=NoNewPrivileges=yes", "--property=LimitFSIZE=" + str(limits.file_bytes), "--", *command]


def _manager_env() -> dict[str, str]:
    # Only host service-manager routing; bwrap clears this before untrusted execution.
    out = {"PATH": "/usr/bin:/bin", "LANG": "C", "HOME": str(Path.home())}
    for name in ("XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"):
        if name in os.environ:
            out[name] = os.environ[name]
    return out


def execute(sandbox: Sandbox, command: list[str], log_path: Path, limits: Limits,
            input_bytes: bytes = b"", *, cancel_event=None) -> dict:
    """Execute with hard cgroup/time caps. A host approval must precede actual use."""
    if cancel_event is not None and cancel_event.is_set():
        raise Rejected("CANCELLED")
    unit = "opsloop-eb-" + uuid.uuid4().hex
    release = log_path.with_suffix(log_path.suffix + ".release").resolve()
    if release.exists():
        raise Rejected("pre-exec release already exists")
    gated = ["/usr/bin/python3", str(Path(__file__).with_name("service_gate.py").resolve()),
             str(release), *sandbox.argv(command)]
    argv = systemd_argv(unit, gated, limits)
    log_path.parent.mkdir(parents=True, exist_ok=True)
    input_path = log_path.with_suffix(log_path.suffix + ".stdin")
    fd = os.open(input_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(input_bytes)
    receipt_path = log_path.with_suffix(log_path.suffix + '.process.json')
    receipt = {'schema':'opsloop-sandbox-process/v1','unit':unit,'state':'RESERVED',
               'hostBoot':Path('/proc/sys/kernel/random/boot_id').read_text().strip()}
    def save_process():
        tmp=receipt_path.with_name(receipt_path.name+'.new')
        with os.fdopen(os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600),'wb') as f:
            f.write(json.dumps(receipt,sort_keys=True).encode());f.flush();os.fsync(f.fileno())
        os.replace(tmp,receipt_path)
        fd=os.open(receipt_path.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    if receipt_path.exists():raise Rejected('sandbox process receipt already exists')
    save_process()  # Durable unique unit identity before any process can launch.
    started = time.monotonic()
    termination = None
    with input_path.open("rb") as stdin, log_path.open("xb") as output:
        process = subprocess.Popen(argv, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                   env=_manager_env(), start_new_session=True)
        cgroup = None
        selector = None
        settled = False
        try:
            cgroup, caps = _confirm_cgroup(unit, process, limits)
            receipt.update(state='STARTED',cgroup=str(cgroup));save_process()
            if cancel_event is not None and cancel_event.is_set():
                raise Rejected("CANCELLED")
            release.write_bytes(b"release\n")
            selector = selectors.DefaultSelector()
            selector.register(process.stdout, selectors.EVENT_READ)
            written = 0
            while True:
                if cancel_event is not None and cancel_event.is_set():
                    termination = "CANCELLED"
                    break
                if time.monotonic() - started > limits.wall_seconds + 10:
                    termination = "TIMED_OUT"
                    break
                if selector.select(timeout=0.05):
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    available = limits.output_bytes - written
                    output.write(chunk[:available])
                    written += min(len(chunk), available)
                    if len(chunk) > available:
                        termination = "OUTPUT_LIMIT"
                        break
                elif process.poll() is not None:
                    break
            if termination:
                _stop_and_settle(unit, process, cgroup)
            returncode = process.wait(timeout=10)
            if not termination:
                _confirm_empty(cgroup)
            settled = True
        finally:
            # Includes I/O errors, ENOSPC, selector failures and interruption after
            # release. The ledger must never be released while a cgroup may run.
            if not settled:
                _stop_and_settle(unit, process, cgroup)
            if selector is not None:
                selector.close()
            process.stdout.close()
            receipt.update(state='TERMINATED',descendantsTerminated=True);save_process()
    payload = log_path.read_bytes()
    if len(payload) > limits.output_bytes:
        termination = "OUTPUT_LIMIT"
    return {"unit": unit, "exit_code": returncode, "termination": termination,
            "elapsed_seconds": round(time.monotonic() - started, 3), "log_sha256": digest(payload),
            "output_bytes": len(payload), "isolation": "bubblewrap-unshare-all+systemd-cgroup",
            "limits": limits.__dict__, "effective_cgroup": caps, "descendants_terminated": True}


def _stop_and_settle(unit, process, cgroup):
    try:
        stopped = subprocess.run(["/usr/bin/systemctl", "--user", "stop", unit + ".service"],
                                 env=_manager_env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
        if stopped.returncode != 0 and process.poll() is None:
            raise Rejected("service-manager stop not acknowledged")
        process.wait(timeout=25)
        if cgroup is not None:
            _confirm_empty(cgroup)
    except BaseException as exc:
        raise Rejected("UNCONFIRMED_TERMINATION: unit=" + unit + ".service; cgroup=" + str(cgroup)
                       + "; cleanup requires reconciliation") from exc


def _confirm_cgroup(unit, process, limits):
    deadline = time.monotonic() + 10
    while process.poll() is None and time.monotonic() < deadline:
        result = subprocess.run(["/usr/bin/systemctl", "--user", "show", unit + ".service",
                                 "--property=ControlGroup", "--value"], env=_manager_env(),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=2)
        value = result.stdout.decode().strip()
        if result.returncode == 0 and value.startswith("/") and ".." not in value.split("/"):
            cgroup = Path("/sys/fs/cgroup") / value.lstrip("/")
            if cgroup.is_dir():
                caps = {p: (cgroup / p).read_text().strip()
                        for p in ("memory.max", "memory.swap.max", "cpu.max", "pids.max")}
                quota, period = caps["cpu.max"].split()
                if caps["memory.max"] != str(limits.memory_bytes) or caps["memory.swap.max"] != "0":
                    raise Rejected("effective cgroup memory cap mismatch")
                if caps["pids.max"] != str(limits.processes) or quota == "max":
                    raise Rejected("effective cgroup pids or CPU cap missing")
                if int(quota) * 100 > int(period) * limits.cpu_percent:
                    raise Rejected("effective cgroup CPU cap too permissive")
                return cgroup, caps
        time.sleep(0.02)
    raise Rejected("BLOCKED_ENVIRONMENT: cgroup enforcement was not confirmed")


def _confirm_empty(cgroup: Path):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if not cgroup.exists():
            return
        if not (cgroup / "cgroup.procs").read_text().strip():
            return
        time.sleep(0.05)
    raise Rejected("UNCONFIRMED_TERMINATION: cgroup descendants remain")


def preflight(sandbox: Sandbox, log_path: Path, *, cancel_event=None, limits=Limits(wall_seconds=20)) -> dict:
    """Check boundaries inside the same cgroup+namespace path used for workload code."""
    probe = """import os,socket,json
assert os.environ.get('OPENAI_API_KEY') is None
assert os.environ.get('KUBECONFIG') is None
assert not os.path.exists('/var/run/docker.sock')
assert not os.path.exists('/run/user')
assert not os.path.exists('/home/sleepyminy')
assert not os.path.exists('/root/.ssh')
assert not os.path.exists('/broker')
for target in ('/workspace/apps/engineering-demo/main.go','/workspace/apps/engineering-demo/forbidden-new-file'):
 try:
  f=open(target,'a'); f.close()
  raise AssertionError('source unexpectedly writable')
 except OSError: pass
s=socket.socket(); s.settimeout(0.2)
try:
 s.connect(('1.1.1.1',443))
 raise AssertionError('network unexpectedly reachable')
except OSError: pass
assert os.listdir('/sys') == [] if os.path.isdir('/sys') else True
print(json.dumps({'sandbox_probe':'PASS','network':'DENIED','host_credentials':'NOT_MOUNTED'}))
"""
    result = execute(sandbox, ["/usr/bin/python3", "-c", probe], log_path, limits, cancel_event=cancel_event)
    if result["exit_code"] != 0 or result["termination"]:
        raise Rejected("BLOCKED_ENVIRONMENT: namespace/cgroup preflight failed")
    return result
