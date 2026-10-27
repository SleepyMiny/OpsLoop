#!/usr/bin/env python3
"""Offline source credential-pattern and explicit change-scope check (no secrets printed)."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

PATTERNS = {
    "private-key": rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----",
    "github-token": rb"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{70,})\b",
    "aws-access-key": rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b",
    "slack-token": rb"\bxox[baprs]-[A-Za-z0-9-]{24,}\b",
    "embedded-client-key": rb"client-key-data\s*:\s*[A-Za-z0-9+/=]{80,}",
    "bearer-token": rb"(?i)authorization[\"']?\s*[:=]\s*[\"']?bearer\s+[A-Za-z0-9._-]{32,}",
}
DENIED_PARTS = {".aws", ".azure", ".kube", "credentials", "secrets", ".opsloop", "node_modules", "reports"}


def scan(root, baseline=None, allowed=()):
    names = subprocess.check_output(
        ["git", "ls-files", "-co", "--exclude-standard", "-z"], cwd=root).decode().split("\0")
    files, findings = {}, []
    for name in sorted(set(filter(None, names))):
        path = root / name
        if path.is_symlink():
            findings.append({"path": name, "rule": "symlink-source"})
            continue
        if not path.is_file():
            continue
        data = path.read_bytes()
        files[name] = hashlib.sha256(data).hexdigest()
        if (any(p in DENIED_PARTS for p in Path(name).parts)
                or (path.name in {"auth.json", "credentials.json", "kubeconfig", ".env"})
                or (path.suffix in {".key", ".p12", ".pfx"})):
            findings.append({"path": name, "rule": "credential-or-generated-path"})
        for rule, pattern in PATTERNS.items():
            if re.search(pattern, data):
                findings.append({"path": name, "rule": rule})
    changed, unexpected = [], []
    if baseline is not None:
        old = baseline["files"]
        changed = sorted(p for p in set(old) | set(files) if old.get(p) != files.get(p))
        unexpected = [p for p in changed if not any(p == prefix or p.startswith(prefix.rstrip("/") + "/") for prefix in allowed)]
    return {"schema": "opsloop-m3-source-scan/v1", "status": "FAIL" if findings or unexpected else "PASS",
            "scope": "git tracked and nonignored untracked source; high-confidence patterns only, not a complete secret audit",
            "fileCount": len(files), "findings": findings, "changedPaths": changed,
            "unexpectedChanges": unexpected, "changeScopeChecked": baseline is not None,
            "allowedPaths": list(allowed), "files": files}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    p.add_argument("--baseline", type=Path)
    p.add_argument("--allow", action="append", default=[])
    p.add_argument("--output", type=Path)
    a = p.parse_args()
    if a.baseline and not a.allow:
        p.error("--baseline requires explicit --allow paths/prefixes")
    result = scan(a.root.resolve(), json.loads(a.baseline.read_text()) if a.baseline else None, a.allow)
    if a.output:
        a.output.parent.mkdir(parents=True, exist_ok=True)
        a.output.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: v for k, v in result.items() if k != "files"}, indent=2))
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
