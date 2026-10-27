"""Read-only local projection of pinned Engineering evidence onto the existing BFF.

No credentials, Kubernetes client, model API, Git commands, or write endpoints.
Artifact manifest is a local trust root: each entry is {path, sha256}.
"""
import argparse
import copy
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import re
if __package__:
    from tools.engineering_runner.contracts import regular_file
else:
    from engineering_runner.contracts import regular_file

UPSTREAM = ("127.0.0.1", 18092)
LISTEN = ("127.0.0.1", 18093)
ORIGIN = "http://127.0.0.1:15174"
ENDPOINT = "/api/v1/snapshot"
MAX_BYTES = 4 * 1024 * 1024
KEYS = {"result", "review", "bundle", "incident", "pr", "receipt"}


def require(value, message):
    if not value:
        raise ValueError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_pinned(entry):
    path = Path(entry["path"])
    raw = regular_file(path, MAX_BYTES)
    require(len(raw) <= MAX_BYTES and digest(raw) == entry["sha256"], "artifact digest mismatch")
    return json.loads(raw)


def projection(manifest):
    require(manifest["schema"] == "opsloop-engineering-dashboard-artifacts/v1", "artifact schema")
    entries = manifest["files"]
    require(set(entries) == KEYS, "exact artifact inventory required")
    docs = {key: read_pinned(entry) for key, entry in entries.items()}
    r, review, bundle, inc, pr, receipt = (docs[k] for k in ("result", "review", "bundle", "incident", "pr", "receipt"))
    require(r["state"] == "VERIFIED_PENDING_REVIEW" and r["real_model_execution"] is True, "actual verified model result required")
    require(r["model_process"]["exit_code"] == 0 and r["model_process"]["descendants_terminated"] is True and r["broker"]["requests"] == 1, "model execution incomplete")
    require(r["baseline_verification"]["accepted"] is True and r["baseline_verification"]["report"]["baseline_defect_reproduced"] is True, "baseline failure missing")
    require(r["candidate_verification"]["accepted"] is True and r["candidate_verification"]["report"]["all_passed"] is True, "candidate verification failed")
    require(review["decision"] == "APPROVED" and review["result_sha256"] == entries["result"]["sha256"], "independent review mismatch")
    for key, expected in (("candidate_source_sha256", r["candidate"]["source_sha256"]), ("patch_sha256", r["patch_sha256"]), ("bundle_sha256", r["bundle_sha256"]), ("base_commit", r["source"]["base_commit"])):
        require(review[key] == expected, "review binding mismatch")
    require(entries["bundle"]["sha256"] == r["bundle_sha256"] and bundle["incident"] == r["incident"], "bundle binding mismatch")
    workload = bundle["workload"]
    runtime_run_id, epoch = bundle["incident"].get("run_id"), bundle["incident"].get("epoch")
    require(all(isinstance(x, str) and 0 < len(x) <= 253 for x in (runtime_run_id, epoch, workload.get("policy_uid"))), "runtime run binding missing")
    require(inc["kind"] == "RecoveryIncident" and inc["metadata"]["uid"] == bundle["incident"]["uid"] and inc["metadata"]["namespace"] == workload["namespace"] and inc["spec"]["workload"]["uid"] == workload["deployment_uid"], "Incident identity mismatch")
    require(inc["spec"].get("policyRef", {}).get("uid") == workload["policy_uid"], "Incident Policy identity mismatch")
    grant_sha = inc["metadata"].get("annotations", {}).get("opsloop.io/execution-grant-binding", "")
    require(isinstance(grant_sha, str) and re.fullmatch(r"[a-f0-9]{64}", grant_sha), "Incident grant binding missing")
    require(inc["status"]["runtime"]["phase"] == "FAILED" and inc["status"]["runtime"]["reason"] == "RUNTIME_BUDGET_EXHAUSTED", "Runtime failure missing")
    require(pr["schema_version"] == "opsloop-pr-result/v1" and pr["state"] == "PR_CREATED", "actual PR result required")
    require(pr["bundle_sha256"] == r["bundle_sha256"] and pr["candidate_source_sha256"] == r["candidate"]["source_sha256"] and pr["base_commit"] == r["source"]["base_commit"], "PR candidate binding mismatch")
    require(pr["remote_receipt_sha256"] == entries["receipt"]["sha256"], "PR receipt hash mismatch")
    repo, number = pr["repository"], pr["pr_number"]
    require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) and type(number) is int and number > 0, "invalid PR identity")
    require(r["source"]["repository"] == "https://github.com/" + repo and pr["pr_url"] == f"https://github.com/{repo}/pull/{number}", "PR URL mismatch")
    require(receipt["schema_version"] == "opsloop-github-pr-observation/v1" and receipt["evidence_kind"] == "observed" and receipt["method"] == "GET" and receipt["api_origin"] == "https://api.github.com" and receipt["path"] == f"/repos/{repo}/pulls/{number}", "actual GitHub GET receipt missing")
    response = receipt["response"]
    require(response["number"] == number and response["html_url"] == pr["pr_url"], "remote PR identity mismatch")
    for side in ("base", "head"):
        require(response[side]["sha"] == pr[side + "_commit"] and response[side]["ref"] == pr[side + "_ref"] and response[side]["repo"]["full_name"] == repo and response[side]["repo"]["id"] == pr["repository_id"], "remote commit/repository mismatch")
    require(pr["merged"] is False and pr["deployed"] is False and response.get("merged") is False and response["state"] == "open", "receipt is not an open unmerged PR")
    require(pr["remote_ci"] == r["candidate_verification"]["remote_ci"] == "NOT_RUN", "remote CI scope mismatch")
    checks = r["candidate_verification"]["report"]["checks"]
    require(checks and all(c["passed"] is True for c in checks), "independent check failure")
    stage = lambda name, summary: {"stage": name, "iteration": 1, "outcome": "pass", "summary": summary}
    return {"incidentUID": inc["metadata"]["uid"], "namespace": inc["metadata"]["namespace"], "incidentName": inc["metadata"]["name"], "workloadUID": workload["deployment_uid"], "policyUID": workload["policy_uid"], "runtimeRunID": runtime_run_id, "epoch": epoch, "grantSha256": grant_sha, "podUID": workload["pod_uid"], "podName": workload["pod_name"],
            "status": {"phase": "PR_CREATED", "jobId": r["run_id"], "iteration": 1, "maxIterations": 1, "baseCommit": pr["base_commit"], "branch": pr["head_ref"],
                       "stages": [stage("build", "로컬 AMD64 빌드·정적 검사 통과; ARM64는 교차 빌드만 확인"), stage("unit", "기존·추가 단위 테스트 통과"), stage("regression-fixture", "고정 HTTP 재현: 수정 전 500 → 수정 후 200; 정상·경계 입력 통과"), stage("independent-review", "정확한 후보 소스·패치 hash에 대한 독립 보안 검토 승인")],
                       "pullRequest": {"number": number, "title": response["title"], "url": pr["pr_url"], "headSha": pr["head_commit"]},
                       "evidence": {"bundleSha256": r["bundle_sha256"], "resultSha256": entries["result"]["sha256"], "prReceiptSha256": entries["receipt"]["sha256"], "remoteCI": "NOT_RUN", "model": r["broker"]["model"], "observedAt": receipt["observed_at"], "executionMode": r["execution_mode"]}}}


def matches(incident, item):
    if not isinstance(incident, dict):
        return False
    meta = incident.get("metadata", {})
    binding = incident.get("runtimeBinding", {})
    return incident.get("kind") == "RecoveryIncident" and meta.get("uid") == item["incidentUID"] and meta.get("namespace") == item["namespace"] and meta.get("name") == item["incidentName"] and incident.get("spec", {}).get("workload", {}).get("uid") == item["workloadUID"] and incident.get("spec", {}).get("policyRef", {}).get("uid") == item["policyUID"] and all(binding.get(key) == item[target] for key, target in (("runID", "runtimeRunID"), ("epoch", "epoch"), ("policyUID", "policyUID"), ("workloadUID", "workloadUID"), ("grantSha256", "grantSha256")))


def join(snapshot, item):
    result = copy.deepcopy(snapshot)
    result.pop("engineeringRuns", None)
    candidates = [result.get("activeIncident"), *result.get("recentIncidents", [])]
    workload = result.get("workload", {})
    result["engineeringRuns"] = [item] if workload.get("uid") == item["workloadUID"] and workload.get("namespace") == item["namespace"] and any(matches(x, item) for x in candidates) else []
    # The BFF already scopes workload presentation by the current Namespace and
    # Deployment selector. Historical evidence must not rewrite any current Pod
    # role, UID, phase, readiness or placement after a later rollout.
    return result


def serve(item):
    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(5)

        def do_GET(self):
            if self.path != ENDPOINT or self.headers.get("Host") != f"{LISTEN[0]}:{LISTEN[1]}" or self.headers.get("Origin") not in (None, ORIGIN):
                self.send_error(403)
                return
            conn = http.client.HTTPConnection(*UPSTREAM, timeout=3)
            try:
                conn.request("GET", ENDPOINT, headers={"Accept": "application/json"})
                response = conn.getresponse()
                require(response.status == 200, "upstream unavailable")
                raw = response.read(MAX_BYTES + 1)
                require(len(raw) <= MAX_BYTES, "upstream oversized")
                content = json.dumps(join(json.loads(raw), item), separators=(",", ":")).encode()
            except Exception:
                self.send_error(502, "Local snapshot unavailable")
                return
            finally:
                conn.close()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(content)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("Access-Control-Allow-Origin", ORIGIN)
            self.send_header("X-Content-Type-Options", "nosniff")
            self.end_headers()
            self.wfile.write(content)

        def log_message(self, *_):
            pass
    HTTPServer(LISTEN, Handler).serve_forever()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--snapshot", type=Path, help="Offline BFF JSON projection, no server/network")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    item = projection(json.loads(args.artifacts.read_bytes()))
    if args.snapshot:
        require(args.output is not None, "offline output path required")
        with args.output.open("x") as f:
            json.dump(join(json.loads(args.snapshot.read_bytes()), item), f, indent=2)
    else:
        require(args.output is None, "output requires offline snapshot")
        serve(item)


if __name__ == "__main__":
    main()
