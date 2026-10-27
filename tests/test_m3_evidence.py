"""Offline regression tests: archive integrity, binding isolation and shipped data."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("m3_evidence", ROOT / "tools/m3_evidence.py")
m3 = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(m3)


class ManifestTests(unittest.TestCase):
    def make_archive(self, root, members=None, hashes=None):
        members = members or {"proof.json": b'{"observed":true}'}
        hashes = hashes if hashes is not None else {k: m3.sha(v) for k, v in members.items()}
        path = root / "sample.zip"
        with zipfile.ZipFile(path, "w") as z:
            for k, v in members.items():
                z.writestr(k, v)
            z.writestr("artifact-manifest.json", json.dumps({"files": hashes}))
        return path

    def test_changed_archive_rejected_before_parsing(self):
        with tempfile.TemporaryDirectory() as d:
            path = self.make_archive(Path(d))
            with self.assertRaisesRegex(ValueError, "archive SHA-256"):
                m3.open_verified(path, "0" * 64, "demo-b")

    def test_corrupted_member_rejected_even_with_valid_zip_digest(self):
        with tempfile.TemporaryDirectory() as d:
            path = self.make_archive(Path(d), hashes={"proof.json": "0" * 64})
            with self.assertRaisesRegex(ValueError, "manifest hash mismatch"):
                m3.open_verified(path, m3.sha(path.read_bytes()), "demo-b")

    def test_missing_manifest_coverage_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            path = self.make_archive(Path(d), hashes={})
            with self.assertRaisesRegex(ValueError, "coverage mismatch"):
                m3.open_verified(path, m3.sha(path.read_bytes()), "demo-b")

    def test_archive_traversal_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            path = self.make_archive(Path(d), members={"../secret": b"not-secret"})
            with self.assertRaisesRegex(ValueError, "unsafe archive path"):
                m3.open_verified(path, m3.sha(path.read_bytes()), "demo-b")

    def test_missing_subset_counted_not_fabricated(self):
        with tempfile.TemporaryDirectory() as d:
            path = self.make_archive(Path(d))
            with zipfile.ZipFile(path) as z:
                result = m3.check_files(z, {"not-packaged.json": "0" * 64}, allow_missing=True)
                self.assertEqual(result, {"checked": 0, "missing": 1, "status": "PARTIAL"})
                with self.assertRaisesRegex(ValueError, "member missing"):
                    m3.check_files(z, {"not-packaged.json": "0" * 64})


class BindingTests(unittest.TestCase):
    def setUp(self):
        self.bundle = {"incident": {"uid": "incident-A", "run_id": "runtime-run", "epoch": "epoch-1", "signature": "signature"}, "workload": {"policy_uid": "policy-A", "deployment_uid": "deployment-A"}, "source": {"base_commit": "base"}}
        self.result = {"incident": copy.deepcopy(self.bundle["incident"]), "bundle_sha256": "bundle", "candidate": {"source_sha256": "candidate"}}
        self.pr = {"bundle_sha256": "bundle", "candidate_source_sha256": "candidate", "base_commit": "base", "state": "PR_CREATED", "deployed": False, "merged": False}
        self.stop = {"incidentUID": "incident-A", "stopReceipt": {"runID": "runtime-run", "state": {"proof": {"policyUID": "policy-A", "liveFence": {"deploymentUID": "deployment-A"}}}}}

    def verify(self):
        return m3.verify_b_binding(self.bundle, self.result, self.pr, self.stop, "bundle")

    def test_exact_binding_accepted(self):
        self.verify()

    def test_same_workload_different_incident_run_or_epoch_rejected(self):
        for field in ("uid", "run_id", "epoch"):
            with self.subTest(field=field):
                before = self.result["incident"][field]
                self.result["incident"][field] = "unrelated"
                with self.assertRaisesRegex(ValueError, "binding mismatch"):
                    self.verify()
                self.result["incident"][field] = before

    def test_different_bundle_or_candidate_rejected(self):
        for field in ("bundle_sha256", "candidate_source_sha256"):
            with self.subTest(field=field):
                before = self.pr[field]
                self.pr[field] = "unrelated"
                with self.assertRaisesRegex(ValueError, "binding mismatch"):
                    self.verify()
                self.pr[field] = before

    def test_stop_for_different_policy_rejected(self):
        self.stop["stopReceipt"]["state"]["proof"]["policyUID"] = "other-policy"
        with self.assertRaisesRegex(ValueError, "policy binding mismatch"):
            self.verify()

    def test_pr_cannot_imply_deployment(self):
        self.pr["deployed"] = True
        with self.assertRaisesRegex(ValueError, "not deployment"):
            self.verify()


class ShippedEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.data = json.loads((ROOT / "frontend/dashboard/src/data/m3/evidence.json").read_text())

    def test_every_link_is_local_with_explicit_download_hash(self):
        public = ROOT / "frontend/dashboard/public"
        for run in self.data["runs"]:
            for event in run["events"]:
                self.assertTrue(event["sources"])
                for ref in event["sources"]:
                    self.assertEqual(ref["href"], f'm3-evidence/{ref["archive"]}/{ref["path"]}')
                    m3.safe_path(ref["href"])
                    raw = (public / ref["href"]).read_bytes()
                    self.assertEqual(m3.sha(raw), ref["downloadSha256"])
                    if ref["transformation"] == "none":
                        self.assertEqual(ref["downloadSha256"], ref["sha256"])
                    else:
                        self.assertNotEqual(ref["downloadSha256"], ref["sha256"])
                        self.assertEqual(json.loads(raw)["_exportProvenance"]["originalSHA256"], ref["sha256"])

    def test_history_cannot_be_mistaken_for_deployed_repair(self):
        self.assertEqual(self.data["mode"], "historical")
        a, b = self.data["runs"]
        self.assertNotEqual(a["binding"]["incidentUID"], b["binding"]["incidentUID"])
        self.assertEqual(b["outcome"]["engineering"], "PR_CREATED")
        self.assertFalse(b["outcome"]["serviceRecovered"])
        self.assertFalse(b["outcome"]["patchedImageDeployed"])
        self.assertEqual(b["outcome"]["remoteCI"], "NOT_RUN")
        self.assertTrue(any(e["at"] is None for e in a["events"]))

    def test_shipped_sources_have_no_known_credential_material(self):
        m3.reject_secrets(json.dumps(self.data).encode())
        for path in (ROOT / "frontend/dashboard/public/m3-evidence").rglob("*"):
            if path.is_file():
                m3.reject_secrets(path.read_bytes())

    def test_secret_material_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "credential"):
            m3.reject_secrets(b'{"access_token":"sensitive-value"}')

    def test_public_download_omits_internal_service_and_pod_endpoints(self):
        path = ROOT / "frontend/dashboard/public/m3-evidence/demo-a/verification.json"
        value = json.loads(path.read_bytes())
        for field in ("service", "directEndpoint"):
            self.assertNotIn("url", value["lastServiceObservation"][field])
        self.assertNotIn(".svc.cluster.local", path.read_text())
        self.assertNotIn("10.42.", path.read_text())

    def test_public_pr_download_is_allowlisted(self):
        path = ROOT / "frontend/dashboard/public/m3-evidence/demo-b/patch-and-tests/receipts/github-pr-observation.json"
        value = json.loads(path.read_bytes())
        self.assertEqual(set(value["response"]), {"number", "state", "draft", "merged", "created_at", "html_url", "head", "base"})
        self.assertNotIn("head_tree", value)
        self.assertLess(len(path.read_bytes()), 3000)


class LocalOriginalTests(unittest.TestCase):
    def test_existing_original_collection_verification_and_tamper(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            member = root / "observed.json"
            member.write_bytes(b'{"actual":true}')
            manifest = {"files": [{"ref": member.name, "sha256": m3.sha(member.read_bytes())}], "runID": "observer-run", "collector": "complete-with-gaps", "liveProof": False}
            manifest_path = root / "manifest.json"
            manifest_path.write_text(json.dumps(manifest))
            path = root / "archive.zip"
            with zipfile.ZipFile(path, "w") as z:
                z.writestr("source-manifest.json", json.dumps({"fullOriginalObserverManifestSHA256": m3.sha(manifest_path.read_bytes())}))
            with zipfile.ZipFile(path) as z:
                receipt = m3.verify_local_observer(root, z)
                self.assertEqual(receipt["status"], "PASS")
                self.assertEqual(receipt["checked"], 1)
                self.assertFalse(receipt["liveProof"])
                member.write_bytes(b'{"actual":false}')
                with self.assertRaisesRegex(ValueError, "member mismatch"):
                    m3.verify_local_observer(root, z)


if __name__ == "__main__":
    unittest.main()
