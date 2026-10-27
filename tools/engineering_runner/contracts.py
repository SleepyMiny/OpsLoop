"""Trusted coordinator contracts. Incident data never supplies executable commands."""
from __future__ import annotations

import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat


class Rejected(ValueError):
    """A boundary check failed; no AI or publisher action is authorized."""


PROFILE = "demo-b-discount-v1"
ALLOWED_PATHS = (
    "apps/engineering-demo/discount.go",
    "apps/engineering-demo/discount_test.go",
)
SOURCE_PREFIX = "apps/engineering-demo/"
PROFILE_SOURCE_FILES = {*ALLOWED_PATHS, SOURCE_PREFIX + "main.go"}
MAX_FILE_BYTES = 128 * 1024
MAX_TREE_BYTES = 2 * 1024 * 1024


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n").encode()


def safe_relative(value: str) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise Rejected("unsafe relative path")
    p = PurePosixPath(value)
    if p.is_absolute() or any(x in ("", ".", "..") for x in value.split("/")):
        raise Rejected("unsafe relative path")
    return value


def regular_file(path: Path, max_bytes: int = MAX_FILE_BYTES) -> bytes:
    # O_NOFOLLOW also protects the final component during a concurrent replacement.
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as exc:
        raise Rejected("regular file could not be opened safely") from exc
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_nlink != 1 or s.st_size > max_bytes:
            raise Rejected("file type, hardlink or size rejected")
        data = os.read(fd, max_bytes + 1)
        if len(data) > max_bytes:
            raise Rejected("file size rejected")
        return data
    finally:
        os.close(fd)


def inventory(root: Path) -> dict[str, str]:
    if root.is_symlink() or not root.is_dir():
        raise Rejected("source directory rejected")
    result = {}
    total = 0
    for current, dirs, files in os.walk(root, followlinks=False):
        for directory in dirs:
            if (Path(current) / directory).is_symlink():
                raise Rejected("symlink directory rejected")
        for name in files:
            path = Path(current) / name
            relative = safe_relative(path.relative_to(root).as_posix())
            data = regular_file(path)
            total += len(data)
            if total > MAX_TREE_BYTES:
                raise Rejected("source tree too large")
            result[relative] = digest(data)
    return dict(sorted(result.items()))


def _required_strings(obj: dict, fields: tuple[str, ...]) -> None:
    for key in fields:
        if not isinstance(obj.get(key), str) or not obj[key] or len(obj[key]) > 512:
            raise Rejected(f"missing or invalid {key}")


def validate_bundle(data: bytes, expected_sha256: str, repository: str, base_commit: str,
                    now: dt.datetime | None = None) -> dict:
    if len(data) > 512 * 1024 or not re.fullmatch(r"[0-9a-f]{64}", expected_sha256):
        raise Rejected("bundle size or external digest invalid")
    if digest(data) != expected_sha256:
        raise Rejected("bundle digest mismatch")
    try:
        b = json.loads(data, object_pairs_hook=_unique_object)
        if b["schema_version"] != "opsloop-engineering-input/v1":
            raise Rejected("unsupported bundle schema")
        if b["evidence_kind"] != "observed":
            raise Rejected("synthetic evidence cannot start real engineering")
        for key in ("capture_manifest_sha256", "authorization_sha256", "provenance_sha256"):
            if not re.fullmatch(r"[0-9a-f]{64}", b["collection"][key]):
                raise Rejected("evidence provenance hash missing")
        _required_strings(b["incident"], ("uid", "run_id", "epoch", "signature"))
        _required_strings(b["workload"], ("namespace", "deployment_name", "deployment_uid", "policy_uid", "pod_name", "pod_uid"))
        source = b["source"]
        if source["repository"] != repository or source["base_commit"] != base_commit:
            raise Rejected("source authority mismatch")
        if not re.fullmatch(r"[0-9a-f]{40}", base_commit):
            raise Rejected("source base must be a full commit SHA")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", source["image_digest"]):
            raise Rejected("image digest missing")
        source_files = source["source_files"]
        if not isinstance(source_files, dict) or not source_files:
            raise Rejected("source file hashes missing")
        for path, checksum in source_files.items():
            if (not safe_relative(path).startswith(SOURCE_PREFIX) or not path.endswith(".go")
                    or not re.fullmatch(r"[0-9a-f]{64}", checksum)):
                raise Rejected("source file identity invalid")
        runtime = b["runtime"]
        if runtime["classification"] != "APPLICATION_FAILURE" or runtime["outcome"] != "EXHAUSTED":
            raise Rejected("runtime is not an exhausted application failure")
        if type(runtime["max_attempts"]) is not int or runtime["max_attempts"] != 1 or not isinstance(runtime["attempts"], list) or len(runtime["attempts"]) != 1:
            raise Rejected("Demo B requires exactly one actual exhausted runtime attempt")
        attempt = runtime["attempts"][0]
        _required_strings(attempt, ("action_id", "patched_at", "completed_at", "signature"))
        if attempt["outcome"] != "Failed" or attempt["signature"] != b["incident"]["signature"]:
            raise Rejected("attempt did not fail with the same signature")
        if timestamp(attempt["completed_at"]) < timestamp(attempt["patched_at"]):
            raise Rejected("runtime timestamps reversed")
        auth = b["authorization"]
        if auth["engineering_enabled"] is not True or auth["profile_id"] != PROFILE:
            raise Rejected("engineering policy disabled or unsupported")
        if timestamp(auth["expires_at"]) <= (now or dt.datetime.now(dt.timezone.utc)):
            raise Rejected("engineering authorization expired")
        constraints = b["repair_constraints"]
        if constraints["allowed_paths"] != list(ALLOWED_PATHS) or constraints["verification_profile_id"] != PROFILE:
            raise Rejected("repair constraints do not match trusted profile")
        evidence = b["evidence"]
        if not evidence["reproduction"] or not evidence["events"] or not evidence["logs"]:
            raise Rejected("missing reproduction, events or logs")
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise Rejected("invalid or incomplete incident bundle") from exc
    return b


def _unique_object(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise Rejected("duplicate JSON key")
        obj[key] = value
    return obj


def timestamp(value: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise Rejected("timestamp must be UTC Z")
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise Rejected("invalid timestamp") from exc


def check_candidate(baseline: Path, candidate: Path) -> dict:
    before, after = inventory(baseline), inventory(candidate)
    if before.keys() != after.keys():
        raise Rejected("source file creation or deletion is not allowed")
    if any(stat.S_IMODE((baseline / p).stat().st_mode) != stat.S_IMODE((candidate / p).stat().st_mode) for p in before):
        raise Rejected("source file permissions changed")
    changed = sorted(p for p in before if before[p] != after[p])
    if not changed or any(p not in ALLOWED_PATHS for p in changed):
        raise Rejected("empty patch or modification outside allowlist")
    total = sum(len(regular_file(candidate / p)) for p in changed)
    if total > 64 * 1024:
        raise Rejected("candidate patch exceeds profile size budget")
    # Existing test bytes must remain intact. Codex may append tests, not alter expectations.
    original_tests = regular_file(baseline / ALLOWED_PATHS[1])
    modified_tests = regular_file(candidate / ALLOWED_PATHS[1])
    if not modified_tests.startswith(original_tests):
        raise Rejected("existing regression tests were changed")
    for p in changed:
        text = regular_file(candidate / p).decode("utf-8")
        if any(x in text for x in ("//go:", "/*go:", "import \"C\"", "func init(", "func TestMain(")):
            raise Rejected("unsafe Go compiler directive or initialization/test hook")
        imports = re.findall(r'(?m)^\s*(?:import\s+)?(?:[\w.]+\s+)?"([^"\n]+)"', text)
        allowed_imports = {"errors", "fmt", "math", "testing", "reflect"}
        if any(value not in allowed_imports for value in imports):
            raise Rejected("new dependency or risky Go import")
    return {"changed_paths": changed, "source_files": after, "source_sha256": digest(canonical(after)),
            "security": {"scope": "PASS", "existing_tests_preserved": True,
                         "imports": "PASS", "compiler_hooks": "PASS", "dependencies_added": False}}
