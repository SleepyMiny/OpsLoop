#!/usr/bin/env python3
"""Verify sealed Demo A/B archives and export a read-only, historical UI projection.

No Kubernetes, network, model, Git or shell operations are performed. Archive
hash pins are deliberate: an unrecognized archive must be reviewed, never used
to silently manufacture a replacement successful run.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import zipfile

ARCHIVES = {
    "demo-a": ("OpsLoop_DemoA_HybridE2E_20261027_030110_KST.zip", "c396116dd0670a82c78c2ac9574c0e7a66521739e9f9862b02abd549c0a04ce0"),
    "demo-b": ("OpsLoop_DemoB_EngineeringE2E_20261027_064911_KST.zip", "43dae1309ebb2b22f29ec48cc8bfef3beebfad51355e2aa92e40d7a0e93e2bee"),
}
ROOT = Path(__file__).resolve().parents[1]


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def safe_path(name: str) -> None:
    p = PurePosixPath(name)
    require(bool(name) and not p.is_absolute() and ".." not in p.parts and "\\" not in name, "unsafe archive path")


def check_files(z: zipfile.ZipFile, entries: dict, prefix: str = "", allow_missing: bool = False) -> dict:
    checked, missing = 0, []
    for name, digest in entries.items():
        safe_path(name)
        path = prefix + name
        require(isinstance(digest, str) and re.fullmatch(r"[a-f0-9]{64}", digest) is not None, "invalid manifest digest")
        if path not in z.namelist():
            require(allow_missing, "manifest member missing: " + path)
            missing.append(path)
            continue
        require(sha(z.read(path)) == digest, "manifest hash mismatch: " + path)
        checked += 1
    return {"checked": checked, "missing": len(missing), "status": "PARTIAL" if missing else "PASS"}


def open_verified(path: Path, expected: str, archive_id: str):
    require(path.is_file(), "original archive missing: " + path.name)
    require(sha(path.read_bytes()) == expected, "archive SHA-256 mismatch: " + path.name)
    z = zipfile.ZipFile(path)
    names = z.namelist()
    require(len(names) == len(set(names)), "duplicate archive entries")
    for name in names:
        safe_path(name)
    require(z.testzip() is None, "archive CRC failure")
    manifest_name = "source-manifest.json" if archive_id == "demo-a" else "artifact-manifest.json"
    manifest = json.loads(z.read(manifest_name))
    entries = manifest["packageFilesSHA256"] if archive_id == "demo-a" else manifest["files"]
    require(set(entries) == set(names) - {manifest_name}, "package manifest coverage mismatch")
    checks = [{"path": manifest_name, **check_files(z, entries)}]
    if archive_id == "demo-a":
        observer = json.loads(z.read("runtime-evidence/observer/manifest.json"))
        checks.append({"path": "runtime-evidence/observer/manifest.json", **check_files(z, {f["ref"]: f["sha256"] for f in observer["files"]}, "runtime-evidence/observer/", True), "scope": "Original observer manifest lists a larger collection; this sealed archive intentionally contains selected files only."})
    else:
        checks.append({"path": "incident-evidence/manifest.json", **check_files(z, json.loads(z.read("incident-evidence/manifest.json"))["files"], "incident-evidence/")})
        source = json.loads(z.read("source-manifest.json"))["files"]
        checks.append({"path": "source-manifest.json", **check_files(z, source, "patch-and-tests/platform-source/", True), "scope": "Source snapshot coverage in the archive; source paths absent from the package remain unverified by this check."})
    return z, {"id": archive_id, "file": path.name, "sha256": expected, "crc": "PASS", "manifestChecks": checks}


def binding_equal(actual: dict, expected: dict, fields: tuple[str, ...]) -> None:
    for field in fields:
        require(bool(expected.get(field)) and actual.get(field) == expected[field], "binding mismatch: " + field)


def verify_b_binding(bundle: dict, result: dict, pr: dict, stop: dict, bundle_hash: str) -> None:
    binding_equal(result["incident"], bundle["incident"], ("uid", "run_id", "epoch", "signature"))
    require(result["bundle_sha256"] == bundle_hash == pr["bundle_sha256"], "bundle hash binding mismatch")
    require(stop["incidentUID"] == bundle["incident"]["uid"], "stop incident binding mismatch")
    require(stop["stopReceipt"]["runID"] == bundle["incident"]["run_id"], "stop run binding mismatch")
    proof = stop["stopReceipt"]["state"]["proof"]
    require(proof["policyUID"] == bundle["workload"]["policy_uid"], "stop policy binding mismatch")
    require(proof["liveFence"]["deploymentUID"] == bundle["workload"]["deployment_uid"], "stop deployment binding mismatch")
    require(pr["candidate_source_sha256"] == result["candidate"]["source_sha256"], "candidate hash binding mismatch")
    require(pr["base_commit"] == bundle["source"]["base_commit"], "base commit binding mismatch")
    require(pr["state"] == "PR_CREATED" and pr["deployed"] is False and pr["merged"] is False, "PR is not deployment")


def source(z: zipfile.ZipFile, archive: str, path: str) -> dict:
    raw = z.read(path)
    download, transformation = public_copy(archive, path, raw)
    return {"archive": archive, "path": path, "sha256": sha(raw), "href": f"m3-evidence/{archive}/{path}", "downloadSha256": sha(download), "transformation": transformation}


def public_copy(archive: str, path: str, raw: bytes) -> tuple[bytes, str]:
    """Separate original provenance from a privacy-minimized browser download."""
    if (archive, path) == ("demo-a", "verification.json"):
        value = json.loads(raw)
        for field in ("service", "directEndpoint"):
            value["lastServiceObservation"][field].pop("url", None)
        transformation = "internal-endpoints-removed"
    elif (archive, path) == ("demo-b", "patch-and-tests/receipts/github-pr-observation.json"):
        original = json.loads(raw)
        response = original["response"]
        value = {"schema_version": original["schema_version"], "evidence_kind": original["evidence_kind"], "observed_at": original["observed_at"], "method": original["method"], "api_origin": original["api_origin"], "path": original["path"], "response": {k: copy.deepcopy(response[k]) for k in ("number", "state", "draft", "merged", "created_at", "html_url")}}
        value["response"]["head"] = {"sha": response["head"]["sha"]}
        value["response"]["base"] = {"sha": response["base"]["sha"], "ref": response["base"]["ref"]}
        transformation = "allowlisted-pr-receipt"
    else:
        return raw, "none"
    value["_exportProvenance"] = {"archive": archive, "path": path, "originalSHA256": sha(raw), "transformation": transformation, "originalAvailableIn": "sealed-original-archive"}
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode(), transformation


def verify_local_observer(path: Path, archive: zipfile.ZipFile) -> dict:
    expected = json.loads(archive.read("source-manifest.json"))["fullOriginalObserverManifestSHA256"]
    receipt = {"schema": "opsloop-m3-local-observer-verification/v1", "scope": "Read-only check of existing local original collection; no archive reconstruction and no change to sealed ZIP subset coverage.", "manifestSHA256": expected, "status": "UNPROVEN", "checked": 0}
    manifest_path = path / "manifest.json"
    if not manifest_path.is_file():
        receipt["reason"] = "local original manifest unavailable"
        return receipt
    require(sha(manifest_path.read_bytes()) == expected, "local observer original manifest mismatch")
    manifest = json.loads(manifest_path.read_bytes())
    missing = []
    for item in manifest["files"]:
        safe_path(item["ref"])
        member = path / item["ref"]
        require(member.resolve().is_relative_to(path.resolve()), "local observer path escapes collection")
        if not member.is_file():
            missing.append(item["ref"])
            continue
        require(sha(member.read_bytes()) == item["sha256"], "local observer member mismatch: " + item["ref"])
        receipt["checked"] += 1
    receipt.update(status="PARTIAL" if missing else "PASS", missing=len(missing), collectionRunID=manifest["runID"], collector=manifest["collector"], liveProof=manifest["liveProof"])
    return receipt


def event(z, archive, ident, title, at, status, summary, facts, paths):
    require(at is None or isinstance(at, str), "event time must be a source timestamp or null")
    return {"id": ident, "title": title, "at": at, "status": status, "summary": summary, "facts": facts, "sources": [source(z, archive, p) for p in paths]}


def runtime_run(z: zipfile.ZipFile) -> dict:
    v = json.loads(z.read("verification.json"))
    d = v["candidateDecision"]
    p = v["PolicyFinal"]
    s = v["ServiceStability"]
    observer = json.loads(z.read("runtime-evidence/observer/manifest.json"))
    pins = observer["pinnedUIDs"]
    require(pins["/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo-a/recoveryincidents/" + d["incidentID"]] == v["incidentUID"], "runtime incident binding mismatch")
    require(pins["/apis/apps/v1/namespaces/opsloop-demo-a/deployments/demo-api"] == d["workloadUID"], "runtime deployment binding mismatch")
    require(pins["/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo-a/recoverypolicies/demo-api"] == p["uid"], "runtime policy binding mismatch")
    require(v["controllerActionID"] == d["actionID"] == s["actionID"], "runtime action binding mismatch")
    require(v["newPod"]["uid"] == s["podUID"] == v["schedulerEvents"][0]["podUID"], "runtime Pod binding mismatch")
    fault = json.loads(z.read("runtime-evidence/hybrid-fault-3-result.json"))
    require(fault["finishReceipt"]["runID"] == v["runID"], "runtime run binding mismatch")
    plan = json.loads(z.read("runtime-evidence/inputs/approvals/hybrid-trial-v3/plan.json"))
    binding_equal(plan, v, ("runID", "epoch"))
    require(plan["observerRunID"] == observer["runID"], "observer run binding mismatch")
    require(plan["deploymentUID"] == d["workloadUID"] and plan["policyUID"] == p["uid"], "trial scope binding mismatch")
    require(v["result"] == "PASS" and v["actualFunctionalE2EVerified"] is True, "runtime functional result missing")
    require(p["enabled"] is False and p["closedThroughGeneration"] == p["generation"], "runtime stop not confirmed")
    def ev(i, title, at, status, summary, facts):
        return event(z, "demo-a", i, title, at, status, summary, facts, ["verification.json"])
    candidate_rows = []
    for c in d["candidates"]:
        m = c["measurements"]
        candidate_rows.append({"node": c["nodeName"], "nodeUID": c["nodeUID"], "platform": c["platform"], "eligible": c["eligible"], "selected": c["nodeUID"] == d["selectedNodeUID"], "cpuUtilizationPermille": m["cpuUtilizationPermille"], "cpuObservedAt": m["cpuSampleTime"], "memoryObservedAt": m["memSampleTime"], "memAvailableBytes": m["memAvailableBytes"], "memTotalBytes": m["memTotalBytes"], "allocatableCPUMilli": m["allocatableCPUMilli"], "requestedCPUMilli": m["requestedCPUMilli"], "requestedMemoryBytes": m["requestedMemoryBytes"], "newPodCPUMilli": m["newPodCPUMilli"], "newPodMemoryBytes": m["newPodMemoryBytes"], "scoreMilli": c["score"]["scoreMilli"]})
    return {"id": "demo-a", "title": "Demo A · Runtime Self-Healing", "track": "runtime", "status": v["result"], "scope": v["resultScope"],
            "binding": {"incidentUID": v["incidentUID"], "runID": v["runID"], "epoch": v["epoch"], "namespace": "opsloop-demo-a", "policyUID": p["uid"], "deploymentUID": d["workloadUID"], "podUIDs": [v["source"]["uid"], v["newPod"]["uid"]]},
            "outcome": {"runtime": "RECOVERED", "engineering": None, "serviceRecovered": True, "patchedImageDeployed": False, "remoteCI": None, "prURL": None, "prNumber": None, "commitSHA": None}, "candidates": candidate_rows,
            "events": [
                ev("fault", "물리 Worker의 readiness 장애", None, "OBSERVED", "AMD64 Worker의 기존 Pod readiness 장애가 실제로 관측되었습니다. 정확한 장애 발생 UTC 시각은 이 요약에서 단정하지 않습니다.", {"podUID": v["source"]["uid"], "node": v["classification"]["nodeName"], "outageCorroborated": s["outageCorroborated"]}),
                ev("detection", "Controller 장애 감지", v["classification"]["observedAt"], "OBSERVED", "POD_NOT_READY 분류 및 실제 Incident가 생성되었습니다.", {"incidentUID": v["incidentUID"], "reason": v["classification"]["reason"], "actionID": v["controllerActionID"]}),
                ev("selection", "자원 기반 후보 선택", d["decidedAt"], "SELECTED", "CPU·메모리·요청량·아키텍처를 비교하여 Azure ARM64 Worker를 선택했습니다.", {"selectedNode": d["selectedNodeName"], "reason": d["decisionReason"], "candidateCount": len(candidate_rows)}),
                ev("patch", "Deployment 변경", None, "OBSERVED", "Controller action에 연결된 template 변경을 확인했습니다. template 외 spec은 보존되었습니다.", {"deploymentUID": d["workloadUID"], "actionID": d["actionID"], "beforeTemplateHash": v["mutationProof"]["beforeTemplateHash"], "afterTemplateHash": v["mutationProof"]["afterTemplateHash"], "outsideTemplateUnchanged": v["mutationProof"]["outsideTemplateUnchanged"]}),
                ev("placement", "Scheduler ARM64 배치", v["schedulerEvents"][0]["observed"]["at"], "OBSERVED", "default-scheduler가 새로운 Pod를 Azure Worker에 실제 배치했습니다. 시각은 Observer 관측 시각입니다.", {"oldPodUID": v["source"]["uid"], "newPodUID": v["newPod"]["uid"], "node": v["newPod"]["node"], "architecture": s["architecture"]}),
                ev("endpoint", "Endpoint 전환", None, "OBSERVED", "새 Pod UID로 Endpoint가 전환된 증거입니다. 마지막 관측 시각을 최초 전환 시각으로 해석하지 않습니다.", {"readyPodUID": v["lastServiceObservation"]["endpointReadyPodUIDs"][0], "lastObservedAt": v["lastServiceObservation"]["at"]["at"]}),
                ev("stability", "Service 안정화", None, "RECOVERED", "직접 Service HTTP 13회 연속 성공, 관측 안정화 구간 32.42초. 장애 감지부터의 총 복구 시간과는 구별합니다.", {"stableSamples": s["stableSamples"], "stableElapsedMS": s["stableElapsedMS"], "architecture": s["architecture"], "podUID": s["podUID"]}),
                ev("stop", "Guard 안전 종료", None, "STOP_CONFIRMED", "Policy disabled 및 strong fence를 확인하고 Observer를 정리했습니다. 현재 상태를 의미하지 않습니다.", {"policyEnabled": p["enabled"], "generation": p["generation"], "closedThroughGeneration": p["closedThroughGeneration"], "observerCleanup": v["ObserverCleanup"]}),
            ], "limitations": ["과거 기능 E2E PASS이며 현재 Live 상태가 아닙니다.", "runtime-v1 전체 actor/clock 증명 및 실제 전원 차단은 UNPROVEN입니다.", "Observer 원본은 complete-with-gaps이며 ZIP에는 선택한 원본 파일만 포함되어 있습니다.", "일부 이벤트의 정확한 UTC 시각은 제공되지 않아 null로 유지합니다."]}


def engineering_run(z: zipfile.ZipFile) -> dict:
    bp, rp, pp, sp = "incident-evidence/bundle.json", "patch-and-tests/actual-run/result.json", "pr-result.json", "incident-evidence/runtime-and-stop.json"
    b, r, p, stop = [json.loads(z.read(x)) for x in (bp, rp, pp, sp)]
    v = json.loads(z.read("verification.json"))
    verify_b_binding(b, r, p, stop, sha(z.read(bp)))
    pr_observation = json.loads(z.read("patch-and-tests/receipts/github-pr-observation.json"))
    require(pr_observation["response"]["head"]["sha"] == p["head_commit"], "PR head binding mismatch")
    require(pr_observation["response"]["number"] == p["pr_number"], "PR number binding mismatch")
    i, w, broker = b["incident"], b["workload"], r["broker"]
    baseline = r["baseline_verification"]["report"]
    checks = r["candidate_verification"]["report"]["checks"]
    require(baseline["baseline_defect_reproduced"] is True and all(c["passed"] for c in checks), "independent verification missing")
    def ev(ident, title, at, status, summary, facts, paths):
        return event(z, "demo-b", ident, title, at, status, summary, facts, paths)
    return {"id": "demo-b", "title": "Demo B · Engineering Self-Healing", "track": "engineering", "status": v["status"], "scope": v["scope"],
            "binding": {"incidentUID": i["uid"], "runID": i["run_id"], "epoch": i["epoch"], "namespace": w["namespace"], "policyUID": w["policy_uid"], "deploymentUID": w["deployment_uid"], "podUIDs": [x["pod_uid"] for x in b["evidence"]["reproduction"]]},
            "outcome": {"runtime": b["runtime"]["outcome"], "engineering": p["state"], "serviceRecovered": False, "patchedImageDeployed": p["deployed"], "remoteCI": p["remote_ci"], "prURL": p["pr_url"], "prNumber": p["pr_number"], "commitSHA": p["head_commit"]}, "candidates": [],
            "events": [
                ev("defect", "실제 애플리케이션 결함", b["evidence"]["reproduction"][0]["observed_at"], "HTTP_500", "빈 주문 요청에서 QUOTE_DIVISION_BY_ZERO가 발생했습니다.", {"errorCode": b["evidence"]["reproduction"][0]["error_code"], "httpStatus": 500, "podUID": b["evidence"]["reproduction"][0]["pod_uid"]}, [bp]),
                ev("runtime", "Runtime 재배치 후 동일 오류", b["evidence"]["reproduction"][1]["observed_at"], "EXHAUSTED", "실제 Runtime 재시도 후 새 Pod에서도 같은 HTTP 500이 재현되어 budget이 소진되었습니다.", {"attempts": len(b["runtime"]["attempts"]), "actionID": b["runtime"]["attempts"][0]["action_id"], "podUID": b["evidence"]["reproduction"][1]["pod_uid"], "errorCode": b["evidence"]["reproduction"][1]["error_code"]}, [bp]),
                ev("stop", "Guard strong stop", None, "STOP_CONFIRMED", "Policy disabled와 fence를 확인했습니다. 종료 helper exit 1은 서비스 복구 성공을 뜻하지 않습니다.", {"generation": stop["stopReceipt"]["state"]["stopGeneration"], "policyEnabled": stop["stopReceipt"]["state"]["proof"]["enabled"], "finishExitCode": stop["finishAttempt"]["exit_code"]}, [sp]),
                ev("evidence", "불변 Incident Evidence Bundle", None, "SEALED", "같은 Incident UID·run·epoch 및 workload UID에 결합된 봉인 증거입니다.", {"bundleSHA256": sha(z.read(bp)), "incidentUID": i["uid"], "runID": i["run_id"], "epoch": i["epoch"]}, [bp, "incident-evidence/manifest.json"]),
                ev("luna", "Luna 격리 실행 및 코드 수정", None, "MODEL_EXECUTED", "실제 gpt-6-luna Medium / ChatGPT 인증 호출 1회. Runtime run과 Engineering job 식별자는 별도로 보존합니다.", {"model": broker["model"], "reasoningEffort": broker["reasoning_effort"], "auth": broker["mode"], "requests": broker["requests"], "engineeringJobID": r["run_id"], "isolation": r["model_process"]["isolation"], "providerCredentialsInSandbox": broker["provider_credentials_in_sandbox"], "changedFiles": ", ".join(r["candidate"]["changed_paths"]), "patchSHA256": r["patch_sha256"]}, [rp, "patch-and-tests/actual-run/candidate.patch"]),
                ev("verification", "수정 전 FAIL → 독립 검증 PASS", None, "VERIFIED", "기존 결함을 독립 재현한 후 후보의 15개 검사를 통과했습니다. ARM64는 교차 빌드이며 native 실행이 아닙니다.", {"baselineDefectReproduced": baseline["baseline_defect_reproduced"], "checksPassed": sum(c["passed"] for c in checks), "checksTotal": len(checks), "candidateSourceSHA256": r["candidate"]["source_sha256"], "nativeARM64": "NOT_RUN"}, [rp]),
                ev("pr", "GitHub Draft PR #6", pr_observation["response"]["created_at"], "PR_CREATED", "검증한 두 파일을 Draft PR로 제출했습니다. 승인·merge·수정 이미지 배포·현재 서비스 복구와 구별합니다.", {"prNumber": p["pr_number"], "prURL": p["pr_url"], "commitSHA": p["head_commit"], "draft": pr_observation["response"]["draft"], "merged": p["merged"], "deployed": p["deployed"], "remoteCI": p["remote_ci"], "prObservedAt": pr_observation["observed_at"]}, [pp, "patch-and-tests/receipts/github-pr-observation.json"]),
            ], "limitations": ["Engineering E2E PASS 범위는 실제 수정·독립 검증·Draft PR 제출까지입니다.", "기록 당시 결함 Pod는 Running/NotReady였으며 현재 상태는 Live 조회로 확인해야 합니다.", "원격 CI NOT_RUN, 수정 이미지 배포 NOT_RUN, native ARM64 실행 NOT_RUN입니다.", "일부 이벤트의 UTC 시각은 원본 receipt에 없어 null로 유지합니다."]}


def reject_secrets(data: bytes) -> None:
    # Defense in depth for the fixed, reviewed export allowlist; this is not a
    # claim to detect every possible secret in arbitrary input.
    patterns = [rb"-----BEGIN (?:[A-Z ]+)?PRIVATE KEY-----", rb"gh[pousr]_[A-Za-z0-9]{20,}", rb"github_pat_[A-Za-z0-9_]{20,}", rb'"(?:client-key-data|client-certificate-data|token|access_token|refresh_token)"\s*:', rb"Bearer [A-Za-z0-9._~-]{16,}"]
    require(not any(re.search(p, data) for p in patterns), "credential material in export")


def export(downloads: Path, output: Path, report: Path, public: Path, observer_original: Path | None = None) -> dict:
    opened, archives = {}, []
    try:
        for ident, (name, digest) in ARCHIVES.items():
            opened[ident], receipt = open_verified(downloads / name, digest, ident)
            archives.append(receipt)
        runs = [runtime_run(opened["demo-a"]), engineering_run(opened["demo-b"])]
        local_receipt = verify_local_observer(observer_original or ROOT / ".opsloop/demo-a-20261025/evidence/hybrid-runtime-observer-3", opened["demo-a"])
        require(runs[0]["binding"]["incidentUID"] != runs[1]["binding"]["incidentUID"], "cross-run collision")
        data = {"schema": "opsloop-m3-evidence/v1", "mode": "historical", "archives": archives, "runs": runs}
        encoded = (json.dumps(data, ensure_ascii=False, indent=2) + "\n").encode()
        reject_secrets(encoded)
        refs = {(s["archive"], s["path"]): s for r in runs for e in r["events"] for s in e["sources"]}
        # Only referenced, reviewed receipt files are exported, never entire archives.
        originals = {key: opened[key[0]].read(key[1]) for key in refs}
        payloads = {key: public_copy(*key, raw)[0] for key, raw in originals.items()}
        for payload in payloads.values():
            reject_secrets(payload)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(encoded)
        for (archive, path), payload in payloads.items():
            target = public / archive / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(payload)
        report.mkdir(parents=True, exist_ok=True)
        index = {"schema": "opsloop-m3-evidence-index/v1", "archiveVerification": "PASS", "scope": "Pinned archive SHA256, CRC, complete package manifests, available nested manifest members, cross-receipt identity bindings. Missing original observer/source members are explicitly counted, not reconstructed.", "archives": archives, "projectionSHA256": sha(encoded), "references": list(refs.values()), "kubernetesMutationRequests": 0, "networkRequests": 0}
        (report / "evidence-index.json").write_text(json.dumps(index, ensure_ascii=False, indent=2) + "\n")
        replay = report / "replay"
        replay.mkdir(exist_ok=True)
        for (archive, path), raw in originals.items():
            if refs[(archive, path)]["transformation"] != "none":
                original_path = replay / "originals" / archive / path
                original_path.parent.mkdir(parents=True, exist_ok=True)
                original_path.write_bytes(raw)
        index["localOriginalObserverVerification"] = local_receipt
        (report / "evidence-index.json").write_text(json.dumps(index, ensure_ascii=False, indent=2) + "\n")
        (replay / "local-original-observer-verification.json").write_text(json.dumps(local_receipt, ensure_ascii=False, indent=2) + "\n")
        (replay / "evidence.json").write_bytes(encoded)
        (replay / "verification.json").write_text(json.dumps({"status": "PASS", "scope": "Offline source verification and deterministic projection; browser playback is checked separately.", "projectionSHA256": sha(encoded), "events": {r["id"]: len(r["events"]) for r in runs}, "kubernetesMutationRequests": 0, "networkRequests": 0}, indent=2) + "\n")
        return index
    finally:
        for archive in opened.values():
            archive.close()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--archives", type=Path, default=Path.home() / "Downloads")
    p.add_argument("--output", type=Path, default=ROOT / "frontend/dashboard/src/data/m3/evidence.json")
    p.add_argument("--report", type=Path, default=ROOT / "reports/m3")
    p.add_argument("--public", type=Path, default=ROOT / "frontend/dashboard/public/m3-evidence")
    p.add_argument("--observer-original", type=Path, help="Optional existing original observer collection; read-only, never reconstructed")
    args = p.parse_args()
    result = export(args.archives, args.output, args.report, args.public, args.observer_original)
    print(json.dumps({"status": "PASS", "projectionSHA256": result["projectionSHA256"], "archives": len(result["archives"]), "networkRequests": 0}))


if __name__ == "__main__":
    main()
