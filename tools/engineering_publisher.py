"""Prepare exact GitHub requests from reviewed results; never call GitHub or run code.

This module is a credential-free publisher gate, not the authenticated publisher.
External SHA pins are supplied by the trusted coordinator, outside Runner control.
Only a separately approved, freshly fetched GitHub response can yield PR_CREATED.
"""
from __future__ import annotations

import argparse
import datetime as dt
import difflib
import hashlib
import json
import os
from pathlib import Path
import re

from tools.engineering_runner.contracts import (
    ALLOWED_PATHS, Rejected, _unique_object, canonical, check_candidate, digest,
    inventory, regular_file,
)


REPOSITORY = "SleepyMiny/OpsLoop"
REPOSITORY_ID = 1408542597
BASE_REF = "demo/demo-b-baseline-20261027"
HEAD_REF = "opsloop/demo-b-empty-order-20261027"
REQUIRED_CHECKS = {
    "source-security", "unit", "static", "build-amd64", "cross-build-arm64", "startup",
    "empty-order-regression", "normal-order", "zero-price", "fraction-truncation",
    "negative-price", "zero-quantity", "malformed-json", "oversize-request", "health-after-reproduction",
}


def require(ok, reason):
    if not ok:
        raise Rejected(reason)


def sha(value, length=64):
    require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{" + str(length) + r"}", value), "invalid SHA pin")
    return value


def instant(value):
    try:
        result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (ValueError, AttributeError) as exc:
        raise Rejected("invalid timestamp") from exc
    require(result.tzinfo is not None, "timestamp requires timezone")
    return result.astimezone(dt.timezone.utc)


def safe_id(value):
    require(isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", value), "invalid identifier")
    return value


def read_json(path, expected, limit=2 * 1024 * 1024):
    data = regular_file(Path(path), limit)
    require(digest(data) == sha(expected), "artifact hash differs from externally pinned evidence")
    try:
        return json.loads(data, object_pairs_hook=_unique_object)
    except (ValueError, UnicodeDecodeError) as exc:
        raise Rejected("invalid JSON artifact") from exc


def check_process(process, exit_code):
    require(process.get("exit_code") == exit_code and not process.get("termination") and
            process.get("descendants_terminated") is True, "process did not finish with confirmed descendant termination")
    sha(process.get("log_sha256"))
    require(process.get("isolation") == "bubblewrap-unshare-all+systemd-cgroup", "unexpected execution isolation")
    caps, limits = process.get("effective_cgroup", {}), process.get("limits", {})
    require(isinstance(caps, dict) and isinstance(limits, dict), "effective process resource limits missing")
    try:
        quota, period = (int(v) for v in caps["cpu.max"].split())
        memory, processes = int(caps["memory.max"]), int(caps["pids.max"])
        require(0 < memory <= 4 * 1024**3 and memory == limits["memory_bytes"] and caps["memory.swap.max"] == "0",
                "effective memory limit missing or too permissive")
        require(0 < processes <= 256 and processes == limits["processes"] and quota > 0 and period > 0 and
                quota * 100 <= period * limits["cpu_percent"] <= period * 200, "effective process/CPU limits invalid")
        require(0 < limits["wall_seconds"] <= 900 and 0 < limits["output_bytes"] <= 8 * 1024**2,
                "execution time/output limits invalid")
    except (KeyError, TypeError, ValueError) as exc:
        raise Rejected("effective process resource limits missing") from exc


def check_verification(value, source_sha, mode, log_file):
    require(value.get("mode") == mode and value.get("accepted") is True and value.get("source_sha256") == source_sha,
            "verification does not cover exact source")
    require(value.get("remote_ci") == "NOT_RUN", "local result must not claim remote CI success")
    process = value.get("process", {})
    check_process(process, 1 if mode == "baseline" else 0)
    log = regular_file(log_file, 4 * 1024 * 1024)
    require(digest(log) == process["log_sha256"], "independent verifier log changed")
    try:
        report = json.loads(log.splitlines()[-1], object_pairs_hook=_unique_object)
    except (ValueError, IndexError) as exc:
        raise Rejected("verifier has no final report") from exc
    require(report == value.get("report") and report.get("schema") == "opsloop-independent-verification/v1", "verifier report mismatch")
    checks = report.get("checks", [])
    require(isinstance(checks, list) and len(checks) == len(REQUIRED_CHECKS) and {c.get("name") for c in checks} == REQUIRED_CHECKS,
            "required independent checks missing or duplicated")
    if mode == "baseline":
        require(report.get("baseline_defect_reproduced") is True and report.get("all_passed") is False and
                all(c.get("passed") is (c["name"] != "empty-order-regression") for c in checks), "baseline must fail only the unchanged defect reproduction")
    else:
        require(report.get("all_passed") is True and all(c.get("passed") is True for c in checks), "candidate did not pass all independent checks")


def secret_free(content):
    require(not any(re.search(pattern, content) for pattern in (
        r"-----BEGIN [A-Z ]*PRIVATE KEY-----", r"\b(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}",
        r"\bsk-[A-Za-z0-9_-]{20,}", r"(?i)authorization\s*[:=]\s*bearer\s+\S+",
        r"(?i)(?:api_key|access_token|refresh_token|client_secret)\s*[:=]\s*[\"']?[A-Za-z0-9_./+-]{16,}",
    )), "possible secret in publishing content")


def validate_target(target, now=None):
    version = target.get("schema_version")
    require(version in ("opsloop-publish-target/v1", "opsloop-publish-target/v2"), "invalid publishing target schema")
    require(target.get("repository") == REPOSITORY and target.get("repository_id") == REPOSITORY_ID,
            "publishing repository is outside approved scope")
    if version == "opsloop-publish-target/v1":
        require(target.get("base_ref") == BASE_REF and target.get("head_ref") == HEAD_REF, "publishing branch is outside approved scope")
    else:
        require(set(target) == {"schema_version", "repository", "repository_id", "base_ref", "head_ref",
                               "base_commit", "base_tree", "run_id", "epoch", "head_ref_exists", "observed_at"},
                "incomplete fresh publishing target")
        run_id = target.get("run_id")
        require(isinstance(run_id, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,95}", run_id), "unsafe exhibition branch run")
        safe_id(target.get("epoch"))
        require(target.get("base_ref") == "main" and target.get("head_ref") == "opsloop/exhibition/" + run_id,
                "exhibition publishing branch is outside approved scope")
        require(target.get("head_ref_exists") is False, "fresh exhibition branch must not exist")
        now = now or dt.datetime.now(dt.timezone.utc)
        observed = instant(target.get("observed_at"))
        require(now - dt.timedelta(minutes=10) <= observed <= now, "publishing target observation stale or future dated")
    sha(target.get("base_commit"), 40)
    sha(target.get("base_tree"), 40)


def winning_attempt(root, result):
    """Keep legacy single-attempt evidence valid; retries never overwrite it."""
    attempts = result.get("attempts")
    if attempts is None:
        require("candidate_directory" not in result and "patch_path" not in result, "unbound candidate path")
        return root, 1
    plan = result.get("execution_plan", {})
    require(isinstance(attempts, list) and 1 <= len(attempts) <= 3, "invalid attempt evidence")
    require(type(plan.get("max_model_calls")) is int and len(attempts) <= plan["max_model_calls"] <= 3,
            "attempt count exceeds approved plan")
    require(plan.get("model") == "gpt-6-luna" and plan.get("reasoning_effort") == "medium", "attempt plan model changed")
    for number, entry in enumerate(attempts, 1):
        require(isinstance(entry, dict), "invalid attempt entry")
        require(entry.get("attempt") == number and type(entry.get("attempt")) is int, "attempt sequence changed")
        where = root if number == 1 else root / ("attempt-" + str(number))
        require(where.is_dir() and not where.is_symlink(), "attempt directory rejected")
        broker = entry.get("broker", {})
        require(type(broker.get("requests")) is int and broker["requests"] == 1 and broker.get("dispatched_requests") == 1 and not broker.get("errors")
                and broker.get("model") == "gpt-6-luna" and broker.get("reasoning_effort") == "medium"
                and broker.get("mode") == "chatgpt" and broker.get("provider_credentials_in_sandbox") is False,
                "attempt broker evidence changed")
        process = entry.get("model_process", {})
        check_process(process, 0)
        require(digest(regular_file(where / "codex.jsonl", 4 * 1024 * 1024)) == process["log_sha256"], "attempt model log changed")
        require(digest(regular_file(where / "candidate.patch", 256 * 1024)) == entry.get("patch_sha256"), "attempt patch changed")
        verification = entry.get("verification", {})
        require(verification.get("accepted") is (number == len(attempts)), "retry did not follow completed rejection")
        verification_log = regular_file(where / "candidate.log", 4 * 1024 * 1024)
        require(digest(verification_log) == verification.get("process", {}).get("log_sha256"), "attempt verification log changed")
        if number < len(attempts):
            check_process(verification.get("process", {}), 1)
            try:
                report = json.loads(verification_log.splitlines()[-1], object_pairs_hook=_unique_object)
            except (ValueError, IndexError) as exc:
                raise Rejected("failed attempt report missing") from exc
            checks = report.get("checks", [])
            require(report == verification.get("report") and report.get("all_passed") is False
                    and len(checks) == len(REQUIRED_CHECKS) and {c.get("name") for c in checks} == REQUIRED_CHECKS
                    and all(type(c.get("passed")) is bool for c in checks) and any(not c["passed"] for c in checks),
                    "retry lacks completed failed checks")
            info = check_candidate(root / "baseline", where / "candidate")
            require(info == entry.get("candidate") and inventory(where / "verification-source") == info["source_files"]
                    and verification.get("source_sha256") == info["source_sha256"], "failed attempt source changed")
    winner = root if len(attempts) == 1 else root / ("attempt-" + str(len(attempts)))
    for key, expected in (("candidate_directory", winner / "candidate"), ("patch_path", winner / "candidate.patch")):
        require(result.get(key) == str(expected.resolve()), "winner path is not the exact bound attempt")
    last = attempts[-1]
    for result_key, attempt_key in (("candidate", "candidate"), ("broker", "broker"), ("model_process", "model_process"),
                                    ("candidate_verification", "verification"), ("patch_sha256", "patch_sha256"),
                                    ("proposal_sha256", "proposal_sha256")):
        require(result.get(result_key) == last.get(attempt_key), "winning result differs from attempt evidence")
    return winner, len(attempts)


def prepare(run_dir, *, result_sha256, review_path, review_sha256, target_path, target_sha256, now=None):
    """Return a reviewable request artifact. No Git writes, hooks or network calls."""
    now = now or dt.datetime.now(dt.timezone.utc)
    root = Path(run_dir)
    require(root.is_dir() and not root.is_symlink(), "run directory rejected")
    result = read_json(root / "result.json", result_sha256)
    review = read_json(review_path, review_sha256)
    target = read_json(target_path, target_sha256)
    validate_target(target, now)
    if target["schema_version"] == "opsloop-publish-target/v2":
        require(target["run_id"] == result.get("incident", {}).get("run_id") and
                target["epoch"] == result.get("incident", {}).get("epoch"), "fresh target belongs to a different run or epoch")
    require(result.get("state") == "VERIFIED_PENDING_REVIEW" and result.get("real_model_execution") is True,
            "real AI candidate has not completed independent validation")
    require(result.get("execution_mode") == "DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR", "unrecognized real model execution path")
    require(result.get("pr_url") is None and result.get("pr_number") is None, "runner must not publish PRs")
    source = result.get("source", {})
    require(source.get("repository") in {"https://github.com/" + REPOSITORY, "https://github.com/" + REPOSITORY + ".git"} and
            source.get("base_commit") == target["base_commit"], "source base is stale or targets a different repository")
    require(result.get("independent_review") == "REQUIRED_FOR_EXACT_SOURCE_SHA256", "unexpected review state")
    winner, provider_requests = winning_attempt(root, result)
    baseline, candidate = root / "baseline", winner / "candidate"
    baseline_files = inventory(baseline)
    require(baseline_files == result.get("baseline_source_files") == source.get("source_files"), "baseline source changed")
    candidate_info = check_candidate(baseline, candidate)
    require(candidate_info == result.get("candidate"), "candidate differs from independently verified result")
    require(inventory(winner / "verification-source") == candidate_info["source_files"], "frozen verifier source changed")
    require(inventory(root / "harness") == result.get("harness_files") and set(result["harness_files"]) == {"audit.go", "verify.py"}, "trusted verifier changed")
    preflight = result.get("preflight", {})
    check_process(preflight, 0)
    require(digest(regular_file(root / "preflight.log", 128 * 1024)) == preflight["log_sha256"], "sandbox boundary preflight changed")
    bundle = read_json(root / "incident.json", result.get("bundle_sha256"), 512 * 1024)
    require(bundle.get("schema_version") == "opsloop-engineering-input/v1" and bundle.get("evidence_kind") == "observed" and
            bundle.get("source") == source and bundle.get("incident") == result.get("incident"), "incident/source evidence mismatch")
    for key in ("capture_manifest_sha256", "authorization_sha256", "provenance_sha256"):
        sha(bundle.get("collection", {}).get(key))
    require(bundle.get("collection", {}).get("complete") is True, "incident evidence incomplete")
    check_verification(result.get("baseline_verification", {}), digest(canonical(baseline_files)), "baseline", root / "baseline.log")
    check_verification(result.get("candidate_verification", {}), candidate_info["source_sha256"], "candidate", winner / "candidate.log")
    model = result.get("model_process", {})
    check_process(model, 0)
    codex_log = regular_file(winner / "codex.jsonl", 4 * 1024 * 1024)
    require(digest(codex_log) == model["log_sha256"], "real Codex execution log changed")
    broker = result.get("broker", {})
    require(type(broker.get("requests")) is int and broker["requests"] == 1 and not broker.get("errors") and
            broker.get("input_bytes", 0) > 0 and broker.get("output_bytes", 0) > 0 and
            broker.get("provider_credentials_in_sandbox") is False, "model broker did not complete the approved credential-isolated request")
    safe_id(broker.get("model"))
    require(broker.get("model") == "gpt-6-luna" and broker.get("reasoning_effort") == "medium" and
            broker.get("mode") == "chatgpt", "result is not the approved Luna Medium ChatGPT execution")
    proposal = read_json(winner / "model-proposal.json", result.get("proposal_sha256"), 256 * 1024)
    try:
        completion = json.loads(codex_log.splitlines()[-1], object_pairs_hook=_unique_object)
    except (ValueError, IndexError) as exc:
        raise Rejected("Codex log lacks a structured proposal") from exc
    require(completion.get("opsloop_model_proposal") == proposal and proposal.get("base_commit") == target["base_commit"] and
            proposal.get("source_sha256") == digest(canonical(baseline_files)), "proposal is not bound to actual Codex output and base")
    proposed = proposal.get("files", [])
    require(isinstance(proposed, list) and len(proposed) == 2 and {p.get("path") for p in proposed} == set(ALLOWED_PATHS), "proposal file scope changed")
    for entry in proposed:
        require(entry.get("content") == regular_file(candidate / entry["path"]).decode(), "published code is not the real model proposal")
    expected_patch = "".join("".join(difflib.unified_diff(regular_file(baseline / p).decode().splitlines(keepends=True),
                       regular_file(candidate / p).decode().splitlines(keepends=True), fromfile="a/" + p, tofile="b/" + p))
                             for p in candidate_info["changed_paths"]).encode()
    patch_data = regular_file(winner / "candidate.patch", 256 * 1024)
    require(patch_data == expected_patch and digest(patch_data) == sha(result.get("patch_sha256")), "candidate patch changed")
    require(review.get("schema_version") == "opsloop-independent-review/v1" and review.get("decision") == "APPROVED" and
            review.get("findings") == [], "independent review has unresolved findings")
    safe_id(review.get("reviewer"))
    reviewed = instant(review.get("reviewed_at"))
    require(reviewed <= now and now - reviewed <= dt.timedelta(hours=24), "independent review is stale or future dated")
    for key, value in (("result_sha256", result_sha256), ("candidate_source_sha256", candidate_info["source_sha256"]),
                       ("patch_sha256", result["patch_sha256"]), ("bundle_sha256", result["bundle_sha256"]), ("base_commit", target["base_commit"])):
        require(review.get(key) == value, "independent review does not cover exact evidence: " + key)
    incident = result["incident"]
    for key in ("uid", "run_id", "epoch"):
        safe_id(incident.get(key))
    files = []
    for path in candidate_info["changed_paths"]:
        content = regular_file(candidate / path).decode()
        secret_free(content)
        data = content.encode()
        files.append({"path": path, "mode": "100644", "type": "blob", "encoding": "utf-8", "content": content,
                      "sha256": digest(data), "git_blob_sha": hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest()})
    request_summary = ("Exactly one provider request succeeded, with no retry or model fallback. " if provider_requests == 1
                       else f"{provider_requests} approved provider requests completed with a fixed model and bounded verification-driven revisions; no transport retry or model fallback. ")
    # Fixed rationale is derived from trusted reproduction, not arbitrary model text.
    body = (
        "An empty order caused the application quote calculation to divide by zero. "
        "The same defect remained after the actual Runtime recovery attempt failed.\n\n"
        f"Incident: `{incident['uid']}`; run: `{incident['run_id']}`; epoch: `{incident['epoch']}`.\n\n"
        "A real Codex run returned the application source and regression-test changes in this PR. "
        "The coordinator applied those exact returned bytes; the independent verifier tested a frozen copy.\n\n"
        "Model: `gpt-6-luna`; reasoning: `medium`; authentication: existing ChatGPT account. "
        + request_summary +
        "The reviewed model proposal changes the two files listed below. "
        "The exact patch and candidate hashes identify its implementation and regression tests.\n\n"
        "Changed paths:\n" + "".join(f"- `{p}`\n" for p in candidate_info["changed_paths"]) + "\n"
        "Reviewed candidate files (SHA-256):\n" + "".join(
            f"- `{path}`: `{candidate_info['source_files'][path]}`\n" for path in candidate_info["changed_paths"]) + "\n"
        "Passed independent checks:\n" + "".join(
            f"- `{check['name']}`\n" for check in result["candidate_verification"]["report"]["checks"]) + "\n"
        "Before the change, the fixed empty-order reproduction failed while the other required checks passed. "
        "After the change, all 15 independent checks passed: reproduction, normal and boundary inputs, unit tests, "
        "static analysis, AMD64 build, ARM64 cross-build, startup and health. ARM64 cross-build is not native ARM64 execution.\n\n"
        "Security checks passed for source scope, preserved existing tests, allowed imports, no new dependencies, "
        "compiler/test hooks, and the independent source audit. The verifier had no provider credentials or network egress.\n\n"
        f"Bundle SHA-256: `{result['bundle_sha256']}`\n\n"
        f"Patch SHA-256: `{result['patch_sha256']}`\n\n"
        f"Candidate inventory SHA-256: `{candidate_info['source_sha256']}`\n\n"
        f"Independent review SHA-256: `{review_sha256}`\n\n"
        "Remote CI: NOT_RUN. Manual review is still required before merge or deployment. "
        "This PR does not restore the running service, activate Enforce, merge itself, or deploy the patch.\n"
    )
    secret_free(body)
    plan_version = "opsloop-publish-plan/v2" if target["schema_version"] == "opsloop-publish-target/v2" else "opsloop-publish-plan/v1"
    return {"schema_version": plan_version, "state": "PREPARED_PENDING_APPROVAL", "requires_user_approval": True,
            "target": target,
            "repository": REPOSITORY, "repository_id": REPOSITORY_ID, "base_ref": target["base_ref"], "head_ref": target["head_ref"],
            "base_commit": target["base_commit"], "base_tree": target["base_tree"], "incident": incident,
            "result_sha256": result_sha256, "review_sha256": review_sha256, "target_sha256": target_sha256,
            "bundle_sha256": result["bundle_sha256"], "patch_sha256": result["patch_sha256"],
            "candidate_source_sha256": candidate_info["source_sha256"], "files": files,
            "commit": {"parents": [target["base_commit"]], "message": "fix(demo): handle empty order quotes"},
            "preconditions": {"base_ref_must_equal": target["base_commit"], "head_ref_must_not_exist": True, "force_push": False},
            "pull_request": {"title": "Fix empty order quotes from verified OpsLoop incident", "body": body,
                             "base": target["base_ref"], "head": target["head_ref"], "draft": True},
            "remote_ci": "NOT_RUN", "pr_number": None, "pr_url": None}


def record_pr(plan_path, *, plan_sha256, approval_path, approval_sha256, remote_path,
              remote_sha256, expected_head_sha, now=None):
    """Validate a trusted coordinator's actual GitHub PR GET receipt; no API call."""
    now = now or dt.datetime.now(dt.timezone.utc)
    plan = read_json(plan_path, plan_sha256)
    require(plan.get("schema_version") in ("opsloop-publish-plan/v1", "opsloop-publish-plan/v2") and plan.get("state") == "PREPARED_PENDING_APPROVAL", "invalid prepared plan")
    target_fields = ("repository", "repository_id", "base_ref", "head_ref", "base_commit", "base_tree")
    if plan["schema_version"] == "opsloop-publish-plan/v2":
        target = plan.get("target", {})
        require(target.get("schema_version") == "opsloop-publish-target/v2" and
                all(target.get(k) == plan.get(k) for k in target_fields) and
                target.get("run_id") == plan.get("incident", {}).get("run_id") and
                target.get("epoch") == plan.get("incident", {}).get("epoch"), "fresh plan target binding changed")
        validate_target(target, now)
    else:
        validate_target({"schema_version": "opsloop-publish-target/v1", **{k: plan.get(k) for k in target_fields}}, now)
    approval = read_json(approval_path, approval_sha256)
    require(approval.get("schema_version") == "opsloop-publish-approval/v1" and approval.get("approved") is True and
            approval.get("plan_sha256") == plan_sha256, "publishing approval does not authorize exact plan")
    require(instant(approval.get("issued_at")) <= now < instant(approval.get("expires_at")), "publishing approval expired or future dated")
    for key in ("repository", "repository_id", "base_ref", "head_ref", "base_commit", "candidate_source_sha256"):
        require(approval.get(key) == plan.get(key), "publishing approval target mismatch")
    receipt = read_json(remote_path, remote_sha256)
    require(receipt.get("schema_version") == "opsloop-github-pr-observation/v1" and receipt.get("evidence_kind") == "observed" and
            receipt.get("method") == "GET" and receipt.get("api_origin") == "https://api.github.com", "PR must come from the actual GitHub read path")
    fetched = instant(receipt.get("observed_at"))
    require(now - dt.timedelta(minutes=10) <= fetched <= now, "GitHub PR observation stale or future dated")
    pr = receipt.get("response", {})
    number = pr.get("number")
    require(type(number) is int and number > 0, "actual PR number missing")
    if plan["schema_version"] == "opsloop-publish-plan/v2":
        require(number != 6, "historical PR 6 cannot be an exhibition publication")
    require(receipt.get("path") == f"/repos/{REPOSITORY}/pulls/{number}", "GitHub PR GET path mismatch")
    require(pr.get("html_url") == f"https://github.com/{REPOSITORY}/pull/{number}" and pr.get("state") == "open" and
            pr.get("merged") is not True, "actual PR URL or state invalid")
    require(pr.get("title") == plan["pull_request"]["title"] and pr.get("body") == plan["pull_request"]["body"] and
            pr.get("draft") is True and pr.get("auto_merge") is None, "PR content, draft or merge policy differs from prepared request")
    sha(expected_head_sha, 40)
    for side, ref, commit in (("base", plan["base_ref"], plan["base_commit"]), ("head", plan["head_ref"], expected_head_sha)):
        value = pr.get(side, {})
        require(value.get("ref") == ref and value.get("sha") == commit, "actual PR branch or commit mismatch")
        require(value.get("repo", {}).get("full_name") == REPOSITORY and value.get("repo", {}).get("id") == REPOSITORY_ID,
                "actual PR repository mismatch")
    require(pr.get("base", {}).get("sha") != pr.get("head", {}).get("sha"), "PR has no repair commit")
    # These are authenticated GET projections supplied by the coordinator, not
    # assertions made by the model. Blob object IDs bind GitHub's patch to the
    # exact reviewed bytes, and the sole parent prevents unrelated commits.
    commit = receipt.get("head_commit", {})
    require(receipt.get("head_commit_path") == f"/repos/{REPOSITORY}/git/commits/{expected_head_sha}" and
            commit.get("sha") == expected_head_sha and commit.get("parents") == [{"sha": plan["base_commit"]}],
            "remote repair commit parent is not the approved base")
    sha(commit.get("tree_sha"), 40)
    require(receipt.get("files_path") == f"/repos/{REPOSITORY}/pulls/{number}/files" and receipt.get("files_complete") is True,
            "complete remote changed-file observation missing")
    changed = receipt.get("files", [])
    expected_files = {f["path"]: f["git_blob_sha"] for f in plan["files"]}
    require(isinstance(changed, list) and len(changed) == len(expected_files), "remote PR changes unexpected files")
    require(all(f.get("status") == "modified" and f.get("filename") in expected_files and
                f.get("sha") == expected_files[f["filename"]] for f in changed) and
            {f.get("filename") for f in changed} == set(expected_files), "remote PR file bytes differ from reviewed source")
    tree = receipt.get("head_tree", {})
    require(receipt.get("head_tree_path") == f"/repos/{REPOSITORY}/git/trees/{commit['tree_sha']}?recursive=1" and
            tree.get("sha") == commit["tree_sha"] and tree.get("truncated") is False, "complete remote commit tree missing")
    entries = tree.get("tree", [])
    require(isinstance(entries, list) and len(entries) <= 10000, "remote tree exceeds supported bound")
    for path, expected in expected_files.items():
        matches = [e for e in entries if e.get("path") == path]
        require(len(matches) == 1 and matches[0].get("mode") == "100644" and matches[0].get("type") == "blob" and
                matches[0].get("sha") == expected, "remote commit tree has unexpected file mode or content")
    return {"schema_version": "opsloop-pr-result/v1", "state": "PR_CREATED", "repository": REPOSITORY, "repository_id": REPOSITORY_ID,
            "pr_number": number, "pr_url": pr["html_url"], "base_ref": plan["base_ref"], "head_ref": plan["head_ref"],
            "base_commit": plan["base_commit"], "head_commit": expected_head_sha, "plan_sha256": plan_sha256,
            "approval_sha256": approval_sha256, "remote_receipt_sha256": remote_sha256, "bundle_sha256": plan["bundle_sha256"],
            "candidate_source_sha256": plan["candidate_source_sha256"], "remote_ci": "NOT_RUN", "merged": False, "deployed": False}


def write_exclusive(path, value):
    data = canonical(value)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    return digest(data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    p = commands.add_parser("prepare")
    for name in ("run-dir", "result-sha256", "review-path", "review-sha256", "target-path", "target-sha256", "output"):
        p.add_argument("--" + name, required=True)
    p = commands.add_parser("record-pr")
    for name in ("plan-path", "plan-sha256", "approval-path", "approval-sha256", "remote-path", "remote-sha256", "expected-head-sha", "output"):
        p.add_argument("--" + name, required=True)
    args = vars(parser.parse_args())
    command, output = args.pop("command"), args.pop("output")
    try:
        result = prepare(**args) if command == "prepare" else record_pr(**args)
        checksum = write_exclusive(output, result)
    except (Rejected, OSError, ValueError, TypeError, KeyError) as exc:
        print(json.dumps({"state": "PUBLISH_BLOCKED", "error_type": type(exc).__name__}))
        return 2
    print(json.dumps({"state": result["state"], "artifact_sha256": checksum}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
