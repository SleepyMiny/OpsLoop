"""Validate externally pinned observations before producing an AI input bundle.

Trust starts at the coordinator's separately approved capture/provenance/authorization
hashes, never at a boolean supplied inside a log or an Incident. This module does not
establish that an arbitrary JSON document came from Kubernetes. The coordinator must
collect it through its authenticated, approved read path and pin that receipt first.
Raw objects remain outside the model bundle; only allowlisted facts are exported.
"""

from __future__ import annotations

import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
from fractions import Fraction


PROFILE = "demo-b-discount-v1"
ALLOWED_PATHS = ["apps/engineering-demo/discount.go", "apps/engineering-demo/discount_test.go"]
FIXTURE = b'{"items":[]}'
FIXTURE_SHA256 = hashlib.sha256(FIXTURE).hexdigest()
ERROR = "QUOTE_DIVISION_BY_ZERO"
MAX_FILE = 2 * 1024 * 1024
MAX_BUNDLE = 512 * 1024
FILES = (
    "incident.json", "policy.json", "deployment-before.json", "deployment-after.json",
    "pod-before.json", "pod-after.json", "replicasets.json", "events.json",
    "observations.json", "logs.json",
)


class EvidenceError(ValueError):
    """Evidence is insufficient; do not run Engineering."""


def require(ok, reason):
    if not ok:
        raise EvidenceError(reason)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode()


def timestamp(value):
    require(isinstance(value, str), "missing timestamp")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EvidenceError("invalid timestamp") from exc
    require(parsed.tzinfo is not None, "timestamp has no timezone")
    return parsed.astimezone(dt.timezone.utc)


def text(value, label, limit=253):
    require(isinstance(value, str) and 0 < len(value) <= limit, "invalid " + label)
    require(re.fullmatch(r"[A-Za-z0-9_.:/@+-]+", value) is not None, "unsafe " + label)
    return value


def regular_bytes(path: Path, limit=MAX_FILE):
    """Reject symlinks, special files and hardlinks before opening any input."""
    try:
        st = path.lstat()
        require(stat.S_ISREG(st.st_mode) and st.st_nlink == 1, "input is not a private regular file")
        require(st.st_size <= limit, "input exceeds size limit")
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as stream:
            opened = os.fstat(stream.fileno())
            require((opened.st_dev, opened.st_ino) == (st.st_dev, st.st_ino), "input changed while opening")
            data = stream.read(limit + 1)
        require(len(data) <= limit, "input exceeds size limit")
        return data
    except OSError as exc:
        raise EvidenceError("input file unavailable") from exc


def pinned_json(path: Path, expected):
    require(isinstance(expected, str) and re.fullmatch(r"[0-9a-f]{64}", expected), "invalid external hash pin")
    data = regular_bytes(path)
    require(digest(data) == expected, "input hash mismatch")
    try:
        return json.loads(data)
    except (ValueError, UnicodeDecodeError) as exc:
        raise EvidenceError("invalid JSON input") from exc


def metadata(obj, kind, namespace):
    require(obj.get("kind") == kind, "wrong Kubernetes kind")
    m = obj.get("metadata", {})
    require(m.get("namespace") == namespace, "namespace mismatch")
    for key in ("name", "uid", "resourceVersion"):
        text(m.get(key), kind + " " + key)
    require(not m.get("deletionTimestamp"), "object is deleting")
    return m


def owner(obj, kind, uid):
    refs = [x for x in obj.get("metadata", {}).get("ownerReferences", []) if x.get("controller") is True]
    require(len(refs) == 1 and refs[0].get("kind") == kind and refs[0].get("uid") == uid,
            "controller owner lineage mismatch")


def capture_inputs(root, expected, authorization, now):
    require(not root.is_symlink() and root.is_dir(), "capture directory is not regular")
    manifest = pinned_json(root / "capture-manifest.json", expected)
    require(manifest.get("schema_version") == "opsloop-engineering-capture/v1", "unsupported capture schema")
    require(manifest.get("evidence_kind") == "observed", "synthetic evidence cannot start Engineering")
    for key in ("run_id", "epoch", "cluster_uid", "api_origin"):
        require(manifest.get(key) == authorization.get(key) and bool(manifest.get(key)), "capture authority mismatch: " + key)
    captured = timestamp(manifest.get("captured_at"))
    require(now - dt.timedelta(minutes=10) <= captured <= now + dt.timedelta(seconds=30), "capture is stale or future dated")
    entries = manifest.get("files", {})
    require(set(entries) == set(FILES), "incomplete capture file manifest")
    raw = {}
    for name in FILES:
        entry = entries[name]
        require(isinstance(entry, dict) and entry.get("evidence_kind") == "observed", "unobserved capture entry")
        require(timestamp(entry.get("observed_at")) <= captured, "capture receipt time mismatch")
        raw[name] = pinned_json(root / name, entry.get("sha256"))
    return raw, manifest


def validate_image_digests(provenance):
    """Index + two OCI manifests + up to two runtime config IDs, pinned by build receipt."""
    digests = provenance.get("platform_image_digests", [])
    require(isinstance(digests, list) and 3 <= len(digests) <= 5 and len(set(digests)) == len(digests),
            "missing, duplicated or oversized image digest set")
    require(all(isinstance(p, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", p) for p in digests), "invalid platform image digest")
    descriptors = provenance.get("platforms", [])
    require(isinstance(descriptors, list) and len(descriptors) == 2, "expected AMD64 and ARM64 OCI descriptors")
    architectures = set()
    manifests = set()
    for entry in descriptors:
        require(isinstance(entry, dict) and entry.get("mediaType") == "application/vnd.oci.image.manifest.v1+json" and
                type(entry.get("size")) is int and 0 < entry["size"] <= MAX_FILE, "invalid OCI manifest descriptor")
        platform = entry.get("platform", {})
        require(platform.get("os") == "linux" and platform.get("architecture") in {"amd64", "arm64"}, "unsupported OCI platform")
        architectures.add(platform["architecture"])
        require(entry.get("digest") in digests, "OCI manifest digest absent from runtime digest set")
        manifests.add(entry["digest"])
    require(architectures == {"amd64", "arm64"} and len(manifests) == 2 and
            provenance.get("image_digest") in digests and provenance["image_digest"] not in manifests,
            "image index/platform provenance inconsistent")
    return digests


def normalized_policy(policy):
    """Normalize only typed metav1.Duration fields; all other bytes of meaning remain strict."""
    require(isinstance(policy, dict), "missing policy snapshot")
    result = copy.deepcopy(policy)
    known = {
        "detection": ("podUnreadyFor", "nodeUnreadyFor", "restartWindow"),
        "filter": ("maxSampleAge",), "score": ("recentFailureWindow",),
        "budget": ("cooldown", "placementTimeout", "verificationTimeout", "incidentDeadline"),
        "verification": ("stabilityWindow",),
    }
    units = {"ns": 1, "us": 1000, "µs": 1000, "μs": 1000, "ms": 10**6, "s": 10**9, "m": 60 * 10**9, "h": 3600 * 10**9}
    token = re.compile(r"([0-9]+(?:\.[0-9]{1,9})?|\.[0-9]{1,9})(ns|us|µs|μs|ms|s|m|h)")
    for section, fields in known.items():
        values = result.get(section, {})
        require(isinstance(values, dict), "policy section is not an object")
        for field in fields:
            if field not in values:
                continue
            value = values[field]
            require(isinstance(value, str) and 0 < len(value) <= 128, "invalid policy duration")
            remaining = value[1:] if value.startswith("+") else value
            total = Fraction(0)
            while remaining:
                match = token.match(remaining)
                require(match is not None, "invalid policy duration syntax")
                segment = Fraction(match[1]) * units[match[2]]
                # Go truncates sub-ns parts per segment. Reject these rather
                # than summing them into a different duration. No float math.
                require(segment.denominator == 1, "policy duration segment loses nanosecond precision")
                total += segment
                remaining = remaining[match.end():]
            require(total.denominator == 1 and 0 < total <= 2**63 - 1, "policy duration outside positive nanosecond range")
            values[field] = int(total)
    return result



def policy_snapshots_equal(left, right):
    """Semantic comparison of complete v1alpha1 snapshots, never a raw hash/signature.

    Reject unknown/missing typed fields before duration normalization. In particular,
    Go Unmarshal/PolicyHash alone drops unknown fields and supplies zero values.
    Optional members retain their presence (absent != null != empty/default).
    """
    shapes = {
        "targetRef": {"name": str},
        "detection": {"nodeUnreadyFor": str, "restartThreshold": int, "restartWindow": str},
        "filter": {"maxCPUUtilizationPercent": int, "maxMemoryUtilizationPercent": int,
                   "minimumAvailableMemory": str, "startupMemoryBudget": str,
                   "requestedCapacityReservePercent": int, "maxSampleAge": str},
        "score": {"cpuWeight": int, "memoryWeight": int, "requestsWeight": int,
                  "recentFailurePenalty": int, "maxRecentFailurePenalty": int, "recentFailureWindow": str},
        "budget": {"maxActionsPerIncident": int, "cooldown": str, "placementTimeout": str,
                   "verificationTimeout": str, "incidentDeadline": str},
        "verification": {"serviceName": str, "port": int, "consecutiveSuccesses": int, "stabilityWindow": str},
    }

    def fields(value, required, optional=None):
        optional = optional or {}
        require(type(value) is dict and set(required) <= set(value) <= set(required) | set(optional),
                "policy snapshot missing/unknown fields")
        for key, item in value.items():
            require(type(item) is (required | optional)[key], "policy snapshot field type invalid")

    def validate(value):
        fields(value, {**{k: dict for k in shapes}, "candidateSelector": dict,
                       "enabled": bool, "mode": str, "requiredPlatforms": list})
        for section, shape in shapes.items():
            fields(value[section], shape, {"podUnreadyFor": str} if section == "detection" else {})
        require(all(type(v) is str for v in value["requiredPlatforms"]), "policy platforms malformed")
        selector = value["candidateSelector"]
        fields(selector, {}, {"matchLabels": dict, "matchExpressions": list})
        require(all(type(k) is str and type(v) is str for k, v in selector.get("matchLabels", {}).items()),
                "policy selector labels malformed")
        for expression in selector.get("matchExpressions", []):
            fields(expression, {"key": str, "operator": str}, {"values": list})
            require(all(type(v) is str for v in expression.get("values", [])), "policy selector values malformed")
        return normalized_policy(value)

    # JSON typed equality avoids Python's True == 1 and does not default fields.
    return canonical(validate(left)) == canonical(validate(right))

def verify_source(repo_root, provenance, authorization):
    require(provenance.get("schema_version") == "opsloop-image-source-provenance/v1", "unsupported provenance schema")
    require(provenance.get("evidence_kind") == "observed-build", "missing actual build provenance")
    for key in ("repository", "base_commit", "image_digest"):
        require(provenance.get(key) == authorization.get(key), "source authorization mismatch: " + key)
    sha = provenance.get("base_commit", "")
    require(re.fullmatch(r"[0-9a-f]{40}", sha), "source revision is not a full commit SHA")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", provenance.get("image_digest", "")), "invalid image digest")
    require(re.fullmatch(r"https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:\.git)?", provenance.get("repository", "")),
            "repository must be an explicit credential-free GitHub HTTPS URL")
    env = {"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_OPTIONAL_LOCKS": "0", "GIT_NO_REPLACE_OBJECTS": "1"}

    def git(*args):
        try:
            result = subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-C", str(repo_root), *args],
                                    stdin=subprocess.DEVNULL, capture_output=True, timeout=15, env=env)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise EvidenceError("source Git object unavailable") from exc
        require(result.returncode == 0 and len(result.stdout) <= MAX_FILE, "source Git object unavailable or oversized")
        return result.stdout

    require(git("cat-file", "-t", sha).strip() == b"commit", "source revision is not a commit object")
    files = provenance.get("source_files", {})
    required = set(ALLOWED_PATHS + ["apps/engineering-demo/main.go"])
    require(required <= set(files) and len(files) <= 100, "missing build source hashes")
    tree_paths = git("ls-tree", "-r", "--name-only", sha, "--", "apps/engineering-demo/").decode().splitlines()
    require(set(tree_paths) == set(files), "build provenance does not cover the entire application source directory")
    for path, expected in files.items():
        require(isinstance(path, str) and re.fullmatch(r"apps/engineering-demo/[a-zA-Z0-9_.-]+", path), "unexpected build source path")
        entry = git("ls-tree", sha, "--", path).decode().strip()
        require(entry.startswith("100644 blob ") or entry.startswith("100755 blob "), "source is not a regular Git blob")
        size = git("cat-file", "-s", sha + ":" + path).strip()
        require(size.isdigit() and int(size) <= MAX_FILE, "source blob exceeds size limit")
        require(digest(git("show", sha + ":" + path)) == expected, "image build source differs from pinned commit")
    validate_image_digests(provenance)
    return files


def validate_objects(raw, authorization, provenance, now):
    ns = text(authorization.get("namespace"), "namespace")
    inc, pol = raw["incident.json"], raw["policy.json"]
    before, after = raw["deployment-before.json"], raw["deployment-after.json"]
    old, new = raw["pod-before.json"], raw["pod-after.json"]
    im = metadata(inc, "RecoveryIncident", ns)
    pm = metadata(pol, "RecoveryPolicy", ns)
    bm, am = metadata(before, "Deployment", ns), metadata(after, "Deployment", ns)
    om, nm = metadata(old, "Pod", ns), metadata(new, "Pod", ns)
    for key, actual in (("incident_uid", im["uid"]), ("policy_uid", pm["uid"]), ("deployment_uid", am["uid"]),
                        ("deployment_name", am["name"]), ("policy_name", pm["name"])):
        require(actual == authorization.get(key), "authorized identity mismatch: " + key)
    require(bm["uid"] == am["uid"] and bm["name"] == am["name"] and om["uid"] != nm["uid"], "no confirmed replacement")
    spec = inc.get("spec", {})
    runtime = inc.get("status", {}).get("runtime", {})
    require(spec.get("workload") == {"name": am["name"], "uid": am["uid"]}, "Incident Deployment mismatch")
    require(spec.get("policyRef") == {"name": pm["name"], "uid": pm["uid"]}, "Incident Policy mismatch")
    policy = pol.get("spec", {})
    require(policy_snapshots_equal(policy, spec.get("policy")) and policy.get("enabled") is True and policy.get("mode") == "Enforce", "policy changed or runtime not enabled")
    require(policy.get("targetRef", {}).get("name") == am["name"], "policy target mismatch")
    require(pm.get("generation") == spec.get("policyGeneration"), "policy generation changed")
    grant_text = pm.get("annotations", {}).get("opsloop.io/execution-grant", "")
    require(isinstance(grant_text, str) and len(grant_text) <= 8192, "runtime grant missing or oversized")
    try:
        grant = json.loads(grant_text)
    except ValueError as exc:
        raise EvidenceError("runtime grant missing or invalid") from exc
    scope = grant.get("scope", {})
    for key, expected in (("runID", authorization["run_id"]), ("epoch", authorization["epoch"]),
                          ("clusterUID", authorization["cluster_uid"]), ("namespace", ns),
                          ("policyUID", pm["uid"]), ("deploymentUID", am["uid"]),
                          ("policyGeneration", pm["generation"]), ("policyHash", spec.get("policyHash")),
                          ("incident", im["name"])):
        require(scope.get(key) == expected, "Runtime execution scope mismatch: " + key)
    require(policy.get("budget", {}).get("maxActionsPerIncident") == 1, "unapproved runtime budget")
    trigger = spec.get("trigger", {})
    require(trigger.get("class") == "APPLICATION_FAILURE" and trigger.get("reason") == "POD_NOT_READY", "not the approved application trigger")
    text(trigger.get("signature"), "Runtime signature")
    require(trigger.get("podUID") == om["uid"] and trigger.get("podName") == om["name"], "initial Pod trigger mismatch")
    require(runtime.get("phase") == "FAILED" and runtime.get("reason") == "RUNTIME_BUDGET_EXHAUSTED", "runtime has not exhausted actual attempts")
    require(not runtime.get("workloadDispatchActionID") and not runtime.get("safetyHaltReason"), "runtime settlement incomplete")
    attempts = runtime.get("attempts", [])
    require(len(attempts) == 1, "actual attempt count mismatch")
    attempt = attempts[0]
    require(attempt.get("outcome") == "Failed", "attempt did not fail")
    reserved, patched, completed = (timestamp(attempt.get(k)) for k in ("reservedAt", "patchedAt", "completedAt"))
    require(reserved <= patched <= completed <= timestamp(runtime.get("completedAt")) <= now, "attempt chronology invalid")
    require(timestamp(grant.get("notBefore")) <= patched < timestamp(grant.get("notAfter")), "Runtime action outside its grant")
    action = text(attempt.get("actionID"), "action ID")
    require(attempt.get("podUID") == nm["uid"] and attempt.get("podName") == nm["name"], "runtime attempt Pod mismatch")
    decision = attempt.get("decision", {})
    require(decision.get("actionID") == action and decision.get("workloadUID") == am["uid"] and
            decision.get("policyHash") == spec.get("policyHash") and decision.get("policyGeneration") == pm.get("generation"), "runtime decision mismatch")
    pre, post = copy.deepcopy(before["spec"]), copy.deepcopy(after["spec"])
    pre_t, post_t = pre.pop("template"), post.pop("template")
    require(pre == post, "Deployment changed outside its template")
    require(post_t.get("metadata", {}).get("annotations", {}).get("opsloop.io/attempt-id") == action, "Deployment action marker missing")
    require(new.get("metadata", {}).get("annotations", {}).get("opsloop.io/attempt-id") == action, "Pod action marker missing")
    require(pre_t != post_t, "Deployment template was not changed")
    for template in (pre_t, post_t):
        template.get("metadata", {}).get("annotations", {}).pop("opsloop.io/attempt-id", None)
        template.get("spec", {}).get("nodeSelector", {}).pop("kubernetes.io/hostname", None)
        if not template.get("metadata", {}).get("annotations"): template.get("metadata", {}).pop("annotations", None)
        if not template.get("spec", {}).get("nodeSelector"): template.get("spec", {}).pop("nodeSelector", None)
    require(pre_t == post_t, "unexpected Runtime template changes")
    rs_items = raw["replicasets.json"].get("items", [])
    require(len(rs_items) <= 20, "too many ReplicaSets")
    for pod in (old, new):
        refs = [r for r in pod.get("metadata", {}).get("ownerReferences", []) if r.get("controller") is True]
        require(len(refs) == 1 and refs[0].get("kind") == "ReplicaSet", "Pod owner missing")
        matches = [r for r in rs_items if r.get("metadata", {}).get("uid") == refs[0].get("uid")]
        require(len(matches) == 1, "ReplicaSet evidence missing or duplicated")
        metadata(matches[0], "ReplicaSet", ns)
        owner(matches[0], "Deployment", am["uid"])
        owner(pod, "ReplicaSet", matches[0]["metadata"]["uid"])
        containers = pod.get("spec", {}).get("containers", [])
        statuses = pod.get("status", {}).get("containerStatuses", [])
        require(len(containers) == len(statuses) == 1, "ambiguous application container")
        require(containers[0].get("image", "").endswith("@" + provenance["image_digest"]), "Pod image is not pinned source image")
        image_id = statuses[0].get("imageID", "").split("@")[-1]
        require(image_id in provenance["platform_image_digests"], "running image digest lacks build provenance")
    require(spec.get("baseline", {}).get("imageDigest") == provenance["image_digest"], "Incident image provenance mismatch")
    return im, am, pm, om, nm, attempt, trigger, patched, completed


def build_bundle(capture_dir, *, capture_sha256, authorization_path, authorization_sha256,
                 provenance_path, provenance_sha256, repository_root, now=None):
    """No I/O side effects beyond reading pinned input files and Git objects."""
    now = now or dt.datetime.now(dt.timezone.utc)
    auth = pinned_json(Path(authorization_path), authorization_sha256)
    require(auth.get("schema_version") == "opsloop-engineering-authorization/v1", "unsupported authorization")
    require(auth.get("engineering_enabled") is True and auth.get("profile_id") == PROFILE, "Engineering is not authorized")
    require(timestamp(auth.get("issued_at")) <= now < timestamp(auth.get("expires_at")), "Engineering authorization expired or future dated")
    require(auth.get("max_runtime_attempts") == 1 and auth.get("allowed_paths") == ALLOWED_PATHS, "authorization exceeds demo scope")
    for key in ("run_id", "epoch", "cluster_uid"):
        text(auth.get(key), key)
    provenance = pinned_json(Path(provenance_path), provenance_sha256)
    files = verify_source(repository_root, provenance, auth)
    raw, capture = capture_inputs(Path(capture_dir), capture_sha256, auth, now)
    im, dm, pm, old, new, attempt, trigger, patched, completed = validate_objects(raw, auth, provenance, now)
    observations = raw["observations.json"]
    require(isinstance(observations, list) and len(observations) == 2, "need initial and replacement application failure observations")
    reproduction = []
    for pod, phase in ((old, "before_runtime"), (new, "after_runtime")):
        matches = [o for o in observations if o.get("phase") == phase]
        require(len(matches) == 1, "missing application failure phase")
        o = matches[0]
        require(o.get("pod_uid") == pod["uid"] and o.get("pod_name") == pod["name"] and o.get("namespace") == auth["namespace"], "HTTP observation Pod mismatch")
        require(o.get("method") == "POST" and o.get("path") == "/quote" and o.get("request_sha256") == FIXTURE_SHA256,
                "reproduction input mismatch")
        require(o.get("evidence_kind") == "observed" and o.get("transport") == "kubernetes-pod-proxy", "application failure not directly observed")
        require(o.get("http_status") == 500 and o.get("error_code") == ERROR, "application defect was not reproduced")
        observed = timestamp(o.get("observed_at"))
        require(observed <= now and (observed <= patched if phase == "before_runtime" else observed >= completed), "HTTP observation chronology mismatch")
        require(re.fullmatch(r"[0-9a-f]{64}", o.get("response_sha256", "")), "HTTP response hash missing")
        reproduction.append({k: o[k] for k in ("phase", "pod_uid", "pod_name", "namespace", "method", "path", "request_sha256", "response_sha256", "http_status", "error_code", "observed_at")})
    signature = digest(canonical({"error": ERROR, "fixture": FIXTURE_SHA256, "image": provenance["image_digest"], "deployment_uid": dm["uid"]}))
    events = []
    items = raw["events.json"].get("items", [])
    require(isinstance(items, list) and len(items) <= 1000, "event limit exceeded")
    for e in items:
        ref = e.get("involvedObject", e.get("regarding", {}))
        if ref.get("uid") in (old["uid"], new["uid"], dm["uid"]) and e.get("reason") in {"Scheduled", "Started", "Created", "Pulled", "Unhealthy", "ScalingReplicaSet"}:
            events.append({"object_uid": ref["uid"], "reason": e["reason"]})
    require(events, "no related Kubernetes Event evidence")
    # Logs are not copied. Only a known diagnostic token bound to a Pod is exported;
    # malicious instructions, environment dumps and arbitrary error text disappear.
    logs = raw["logs.json"]
    require(isinstance(logs, list) and len(logs) <= 10, "invalid bounded logs")
    safe_logs = []
    for item in logs:
        if item.get("pod_uid") in (old["uid"], new["uid"]) and isinstance(item.get("text"), str) and ERROR in item["text"]:
            safe_logs.append({"pod_uid": item["pod_uid"], "error_code": ERROR, "raw_sha256": digest(item["text"].encode()), "projection": "known-error-code-only"})
    require({x["pod_uid"] for x in safe_logs} == {old["uid"], new["uid"]}, "missing initial or replacement error logs")
    bundle = {
        "schema_version": "opsloop-engineering-input/v1", "evidence_kind": "observed",
        "incident": {"uid": im["uid"], "run_id": auth["run_id"], "epoch": auth["epoch"], "signature": signature, "runtime_signature": trigger["signature"]},
        "source": {"repository": provenance["repository"], "base_commit": provenance["base_commit"], "image_digest": provenance["image_digest"], "source_files": files},
        "workload": {"namespace": auth["namespace"], "deployment_name": dm["name"], "deployment_uid": dm["uid"], "policy_uid": pm["uid"], "pod_name": new["name"], "pod_uid": new["uid"]},
        "runtime": {"classification": "APPLICATION_FAILURE", "outcome": "EXHAUSTED", "reason": "RUNTIME_BUDGET_EXHAUSTED", "max_attempts": 1,
                    "attempts": [{"action_id": attempt["actionID"], "patched_at": timestamp(attempt["patchedAt"]).isoformat().replace("+00:00", "Z"), "completed_at": timestamp(attempt["completedAt"]).isoformat().replace("+00:00", "Z"), "outcome": "Failed", "signature": signature}]},
        "authorization": {"engineering_enabled": True, "profile_id": PROFILE, "expires_at": timestamp(auth["expires_at"]).isoformat().replace("+00:00", "Z")},
        "evidence": {"reproduction": reproduction, "fixture": {"body": FIXTURE.decode(), "sha256": FIXTURE_SHA256, "expected_status": 200, "expected_result": {"total": 0, "average": 0}}, "events": events[:100], "logs": safe_logs},
        "repair_constraints": {"allowed_paths": ALLOWED_PATHS, "verification_profile_id": PROFILE},
        "collection": {"capture_manifest_sha256": capture_sha256, "authorization_sha256": authorization_sha256, "provenance_sha256": provenance_sha256,
                       "raw_files_sha256": {name: capture["files"][name]["sha256"] for name in FILES}, "redaction": "allowlisted-facts-only", "complete": True},
    }
    require(len(canonical(bundle)) <= MAX_BUNDLE, "bundle too large")
    return bundle


def seal_bundle(bundle, destination):
    """Exclusive create: a sealed run is never overwritten, even on a retry."""
    dest = Path(destination)
    require(not dest.exists(), "sealed destination already exists")
    dest.mkdir(mode=0o700, parents=False)
    data = canonical(bundle)
    require(len(data) <= MAX_BUNDLE, "bundle too large")
    manifest = {"schema_version": "opsloop-engineering-seal/v1", "files": {"bundle.json": digest(data)}}
    for name, content in (("bundle.json", data), ("manifest.json", canonical(manifest))):
        fd = os.open(dest / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o400)
        with os.fdopen(fd, "wb") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
    fd = os.open(dest, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    # chmod prevents accidental edits, not malicious owner writes. The returned
    # external manifest pin is the integrity authority on every subsequent read.
    dest.chmod(0o500)
    fd = os.open(dest.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    return {"bundle_sha256": digest(data), "manifest_sha256": digest(canonical(manifest))}


def verify_sealed(directory, manifest_sha256):
    root = Path(directory)
    require(not root.is_symlink(), "sealed directory symlink rejected")
    m = pinned_json(root / "manifest.json", manifest_sha256)
    require(m.get("schema_version") == "opsloop-engineering-seal/v1" and set(m.get("files", {})) == {"bundle.json"}, "invalid sealed manifest")
    return pinned_json(root / "bundle.json", m["files"]["bundle.json"])
