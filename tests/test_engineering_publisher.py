"""Synthetic publisher-boundary tests; these never contact GitHub or prove a PR."""
import copy
import datetime as dt
import difflib
import json
from pathlib import Path
import shutil
import tempfile
import unittest

from tools.engineering_publisher import (
    BASE_REF, HEAD_REF, REPOSITORY, REPOSITORY_ID, REQUIRED_CHECKS,
    Rejected, prepare, record_pr,
)
from tools.engineering_runner.contracts import ALLOWED_PATHS, canonical, check_candidate, digest, inventory


NOW = dt.datetime(2026, 10, 27, tzinfo=dt.timezone.utc)


def when(seconds):
    return (NOW + dt.timedelta(seconds=seconds)).isoformat().replace("+00:00", "Z")


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.run = self.root / "run"
        self.run.mkdir()
        self.base = "a" * 40
        for name in ("baseline", "candidate", "verification-source"):
            (self.run / name / "apps/engineering-demo").mkdir(parents=True)
        original = {ALLOWED_PATHS[0]: "package main\nfunc Answer() int { return 1 }\n",
                    ALLOWED_PATHS[1]: "package main\n// synthetic fixture\n",
                    "apps/engineering-demo/main.go": "package main\nfunc main() {}\n"}
        updated = {**original, ALLOWED_PATHS[0]: "package main\nfunc Answer() int { return 2 }\n",
                   ALLOWED_PATHS[1]: original[ALLOWED_PATHS[1]] + "// extra synthetic check\n"}
        for name, files in (("baseline", original), ("candidate", updated), ("verification-source", updated)):
            for path, content in files.items():
                (self.run / name / path).write_text(content)
        (self.run / "harness").mkdir()
        for file in ("audit.go", "verify.py"):
            (self.run / "harness" / file).write_text("offline fixture only\n")
        before = inventory(self.run / "baseline")
        candidate = check_candidate(self.run / "baseline", self.run / "candidate")
        incident = {"uid": "incident-uid", "run_id": "runtime-one", "epoch": "epoch-one", "signature": "1" * 64}
        source = {"repository": "https://github.com/" + REPOSITORY, "base_commit": self.base,
                  "image_digest": "sha256:" + "2" * 64, "source_files": before}
        bundle = {"schema_version": "opsloop-engineering-input/v1", "evidence_kind": "observed", "source": source, "incident": incident,
                  "collection": {"complete": True, **{k: "3" * 64 for k in ("capture_manifest_sha256", "authorization_sha256", "provenance_sha256")}}}
        proposal = {"schema": "opsloop-code-proposal/v1", "base_commit": self.base, "source_sha256": digest(canonical(before)),
                    "rationale": "synthetic test proposal", "files": [{"path": p, "content": updated[p]} for p in ALLOWED_PATHS]}
        self.write(self.run / "incident.json", bundle)
        self.write(self.run / "model-proposal.json", proposal)
        codex = canonical({"opsloop_model_proposal": proposal})
        (self.run / "codex.jsonl").write_bytes(codex)
        patch = "".join("".join(difflib.unified_diff(original[p].splitlines(keepends=True), updated[p].splitlines(keepends=True),
                        fromfile="a/" + p, tofile="b/" + p)) for p in candidate["changed_paths"]).encode()
        (self.run / "candidate.patch").write_bytes(patch)
        self.result = {"run_id": "runner-one", "incident": incident, "source": source, "state": "VERIFIED_PENDING_REVIEW",
                       "real_model_execution": True, "execution_mode": "DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR",
                       "pr_url": None, "pr_number": None, "independent_review": "REQUIRED_FOR_EXACT_SOURCE_SHA256",
                       "baseline_source_files": before, "candidate": candidate, "harness_files": inventory(self.run / "harness"),
                       "bundle_sha256": digest(canonical(bundle)), "proposal_sha256": digest(canonical(proposal)), "patch_sha256": digest(patch),
                       "model_process": self.process(0, codex),
                       "broker": {"requests": 1, "errors": {}, "input_bytes": 10, "output_bytes": 10, "model": "gpt-6-luna",
                                  "reasoning_effort": "medium", "mode": "chatgpt",
                                  "provider_credentials_in_sandbox": False}}
        (self.run / "preflight.log").write_bytes(b"synthetic sandbox preflight fixture\n")
        self.result["preflight"] = self.process(0, b"synthetic sandbox preflight fixture\n")
        for mode in ("baseline", "candidate"):
            report = {"schema": "opsloop-independent-verification/v1", "all_passed": mode == "candidate",
                      "baseline_defect_reproduced": mode == "baseline",
                      "checks": [{"name": n, "passed": not (mode == "baseline" and n == "empty-order-regression")} for n in sorted(REQUIRED_CHECKS)]}
            log = canonical(report)
            (self.run / (mode + ".log")).write_bytes(log)
            self.result[mode + "_verification"] = {"mode": mode, "accepted": True,
                "source_sha256": digest(canonical(before)) if mode == "baseline" else candidate["source_sha256"],
                "process": self.process(1 if mode == "baseline" else 0, log), "report": report, "remote_ci": "NOT_RUN"}
        self.target = {"schema_version": "opsloop-publish-target/v1", "repository": REPOSITORY, "repository_id": REPOSITORY_ID,
                       "base_ref": BASE_REF, "head_ref": HEAD_REF, "base_commit": self.base, "base_tree": "b" * 40}
        self.review = {"schema_version": "opsloop-independent-review/v1", "reviewer": "independent-reviewer", "reviewed_at": when(-60),
                       "decision": "APPROVED", "findings": [], "result_sha256": digest(canonical(self.result)),
                       "candidate_source_sha256": candidate["source_sha256"], "patch_sha256": digest(patch),
                       "bundle_sha256": self.result["bundle_sha256"], "base_commit": self.base}

    def tearDown(self):
        self.tmp.cleanup()

    def write(self, path, obj):
        path.write_bytes(canonical(obj))
        return digest(canonical(obj))

    def process(self, exit_code, log):
        return {"exit_code": exit_code, "termination": None, "descendants_terminated": True,
                "log_sha256": digest(log), "isolation": "bubblewrap-unshare-all+systemd-cgroup",
                "effective_cgroup": {"memory.max": "1073741824", "memory.swap.max": "0", "cpu.max": "100000 100000", "pids.max": "96"},
                "limits": {"memory_bytes": 1073741824, "cpu_percent": 100, "processes": 96, "wall_seconds": 180, "output_bytes": 2097152}}

    def arguments(self):
        return {"run_dir": self.run, "result_sha256": self.write(self.run / "result.json", self.result),
                "review_path": self.root / "review.json", "review_sha256": self.write(self.root / "review.json", self.review),
                "target_path": self.root / "target.json", "target_sha256": self.write(self.root / "target.json", self.target), "now": NOW}

    def plan(self):
        return prepare(**self.arguments())

    def receipt_arguments(self):
        plan = self.plan()
        ph = self.write(self.root / "plan.json", plan)
        approval = {"schema_version": "opsloop-publish-approval/v1", "approved": True, "plan_sha256": ph,
                    "issued_at": when(-10), "expires_at": when(100),
                    **{k: plan[k] for k in ("repository", "repository_id", "base_ref", "head_ref", "base_commit", "candidate_source_sha256")}}
        pr = {"number": 123, "html_url": f"https://github.com/{REPOSITORY}/pull/123", "state": "open", "merged": False,
              "title": plan["pull_request"]["title"], "body": plan["pull_request"]["body"], "draft": True, "auto_merge": None,
              "base": {"ref": BASE_REF, "sha": self.base, "repo": {"full_name": REPOSITORY, "id": REPOSITORY_ID}},
              "head": {"ref": HEAD_REF, "sha": "c" * 40, "repo": {"full_name": REPOSITORY, "id": REPOSITORY_ID}}}
        remote = {"schema_version": "opsloop-github-pr-observation/v1", "evidence_kind": "observed", "observed_at": when(-1),
                  "method": "GET", "api_origin": "https://api.github.com", "path": f"/repos/{REPOSITORY}/pulls/123", "response": pr,
                  "head_commit_path": f"/repos/{REPOSITORY}/git/commits/" + "c" * 40,
                  "head_commit": {"sha": "c" * 40, "parents": [{"sha": self.base}], "tree_sha": "d" * 40},
                  "files_path": f"/repos/{REPOSITORY}/pulls/123/files", "files_complete": True,
                  "files": [{"filename": f["path"], "status": "modified", "sha": f["git_blob_sha"]} for f in plan["files"]],
                  "head_tree_path": f"/repos/{REPOSITORY}/git/trees/" + "d" * 40 + "?recursive=1",
                  "head_tree": {"sha": "d" * 40, "truncated": False, "tree": [{"path": f["path"], "mode": "100644", "type": "blob", "sha": f["git_blob_sha"]} for f in plan["files"]]}}
        args = {"plan_path": self.root / "plan.json", "plan_sha256": ph, "approval_path": self.root / "approval.json",
                "approval_sha256": self.write(self.root / "approval.json", approval), "remote_path": self.root / "remote.json",
                "remote_sha256": self.write(self.root / "remote.json", remote), "expected_head_sha": "c" * 40, "now": NOW}
        return args, approval, remote

    def test_prepared_requests_have_exact_paths_and_never_claim_pr(self):
        p = self.plan()
        self.assertEqual(p["state"], "PREPARED_PENDING_APPROVAL")
        self.assertIsNone(p["pr_url"])
        self.assertIsNone(p["pr_number"])
        self.assertEqual({f["path"] for f in p["files"]}, set(ALLOWED_PATHS))
        self.assertEqual(p["commit"]["parents"], [self.base])
        self.assertEqual(p["remote_ci"], "NOT_RUN")
        self.assertNotIn("synthetic test proposal", p["pull_request"]["body"])

    def test_changed_candidate_and_stale_review_are_rejected(self):
        p = self.run / "candidate" / ALLOWED_PATHS[0]
        original = p.read_bytes()
        p.write_text("package main\n// changed after verification\n")
        with self.assertRaises(Rejected): self.plan()
        p.write_bytes(original)
        self.review["reviewed_at"] = when(-90000)
        with self.assertRaises(Rejected): self.plan()

    def test_wrong_target_base_ref_and_remote_repo_are_rejected(self):
        for key, value in [("base_commit", "d" * 40), ("base_ref", "main"), ("head_ref", "../main"), ("repository", "attacker/OpsLoop")]:
            original = self.target[key]
            self.target[key] = value
            with self.subTest(key=key), self.assertRaises(Rejected): self.plan()
            self.target[key] = original

    def test_missing_real_model_or_required_test_blocks_prepare(self):
        self.result["real_model_execution"] = False
        with self.assertRaises(Rejected): self.plan()
        self.result["real_model_execution"] = True
        self.result["candidate_verification"]["report"]["checks"].pop()
        with self.assertRaises(Rejected): self.plan()

    def test_external_pin_or_review_source_mismatch_rejected(self):
        args = self.arguments()
        args["result_sha256"] = "0" * 64
        with self.assertRaises(Rejected): prepare(**args)
        self.review["candidate_source_sha256"] = "f" * 64
        with self.assertRaises(Rejected): self.plan()

    def test_record_requires_external_approval_and_actual_shaped_receipt(self):
        args, _, _ = self.receipt_arguments()
        result = record_pr(**args)
        self.assertEqual(result["state"], "PR_CREATED")
        self.assertEqual(result["pr_number"], 123)
        self.assertFalse(result["merged"])
        self.assertEqual(result["remote_ci"], "NOT_RUN")

    def test_wrong_remote_url_head_repo_base_sha_rejected(self):
        args, _, remote = self.receipt_arguments()
        for path, value in [(('html_url',), 'https://evil.example/pull/123'), (('head', 'sha'), 'e' * 40),
                            (('base', 'sha'), 'e' * 40), (('head', 'repo', 'full_name'), 'attacker/OpsLoop')]:
            modified = copy.deepcopy(remote)
            node = modified["response"]
            for key in path[:-1]: node = node[key]
            node[path[-1]] = value
            args["remote_sha256"] = self.write(args["remote_path"], modified)
            with self.subTest(path=path), self.assertRaises(Rejected): record_pr(**args)

    def test_missing_or_expired_approval_and_synthetic_receipt_rejected(self):
        args, approval, remote = self.receipt_arguments()
        approval["approved"] = False
        args["approval_sha256"] = self.write(args["approval_path"], approval)
        with self.assertRaises(Rejected): record_pr(**args)
        approval["approved"] = True
        approval["expires_at"] = when(-1)
        args["approval_sha256"] = self.write(args["approval_path"], approval)
        with self.assertRaises(Rejected): record_pr(**args)
        approval["expires_at"] = when(100)
        args["approval_sha256"] = self.write(args["approval_path"], approval)
        remote["evidence_kind"] = "synthetic"
        args["remote_sha256"] = self.write(args["remote_path"], remote)
        with self.assertRaises(Rejected): record_pr(**args)

    def test_remote_blob_content_and_commit_parent_must_match_review(self):
        args, _, remote = self.receipt_arguments()
        remote["files"][0]["sha"] = "e" * 40
        args["remote_sha256"] = self.write(args["remote_path"], remote)
        with self.assertRaises(Rejected): record_pr(**args)
        args, _, remote = self.receipt_arguments()
        remote["head_commit"]["parents"].append({"sha": "e" * 40})
        args["remote_sha256"] = self.write(args["remote_path"], remote)
        with self.assertRaises(Rejected): record_pr(**args)


if __name__ == "__main__":
    unittest.main()
