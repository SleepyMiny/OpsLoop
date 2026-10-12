"""Offline fixtures for Azure restrictions/quota and the read-only CLI contract."""
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("azure_survey", Path(__file__).resolve().parents[1] / "scripts/azure-arm64-survey.py")
survey = importlib.util.module_from_spec(spec)
spec.loader.exec_module(survey)

SKU = {"name": "Standard_B2pls_v2", "family": "standardBpsv2Family",
       "capabilities": [{"name": k, "value": v} for k, v in {
           "CpuArchitectureType": "Arm64", "vCPUs": "2", "MemoryGB": "4",
           "HyperVGenerations": "V2", "AcceleratedNetworkingEnabled": "True"}.items()],
       "locationInfo": [{"location": "JapanEast", "zones": ["1"]}], "restrictions": []}
USAGE = [{"name": "cores", "current": "0", "limit": "6"},
         {"name": "standardBpsv2Family", "current": "0", "limit": "10"},
         {"name": "virtualMachines", "current": "0", "limit": "25000"}]
IMAGE = {"version": "24.04.202609040", "architecture": "Arm64", "generation": "V2", "plan": None, "disk_gib": 30}
PROVIDERS = [{"namespace": p, "state": "Registered"} for p in survey.PROVIDERS]


class SurveyFixtureTests(unittest.TestCase):
    def check(self, sku=SKU, usages=USAGE, image=IMAGE, providers=PROVIDERS, zone=None, disk=None):
        return survey.candidate_check(sku, "japaneast", usages, image, providers, zone, disk)

    def test_zero_family_quota_blocks_despite_regional_headroom(self):
        usages = copy.deepcopy(USAGE)
        usages[1]["limit"] = "0"
        result = self.check(usages=usages)
        self.assertEqual(result["checks"]["regional_quota"]["status"], "pass")
        self.assertEqual(result["checks"]["family_quota"]["status"], "blocked")
        self.assertEqual(result["read_checks_status"], "blocked")

    def test_missing_or_malformed_quota_is_unknown_and_family_case_insensitive(self):
        self.assertEqual(survey.quota_check([], "cores", 2)["status"], "unknown")
        self.assertEqual(survey.quota_check([{ "name": "cores", "current": "?", "limit": "6"}], "cores", 2)["status"], "unknown")
        self.assertEqual(survey.quota_check(USAGE, "StandardBpsv2Family", 2)["headroom"], 10)

    def test_zone_restriction_does_not_become_region_restriction(self):
        sku = copy.deepcopy(SKU)
        sku["restrictions"] = [{"type": "Zone", "values": ["JapanEast"], "reasonCode": "NotAvailableForSubscription",
                                "restrictionInfo": {"locations": ["JapanEast"], "zones": ["1"]}}]
        self.assertEqual(self.check(sku=sku)["checks"]["restrictions"]["status"], "pass")
        self.assertEqual(self.check(sku=sku, zone="1")["checks"]["restrictions"]["status"], "blocked")
        self.assertEqual(self.check(zone="2")["checks"]["restrictions"]["status"], "blocked")

    def test_location_restriction_blocks_without_zone(self):
        sku = copy.deepcopy(SKU)
        sku["restrictions"] = [{"type": "Location", "values": ["JapanEast"], "reasonCode": "NotAvailableForSubscription"}]
        self.assertEqual(self.check(sku=sku)["checks"]["restrictions"]["status"], "blocked")
        sku["restrictions"][0]["values"] = ["KoreaCentral"]
        self.assertEqual(self.check(sku=sku)["checks"]["restrictions"]["status"], "pass")

    def test_unknown_restriction_fails_closed(self):
        sku = copy.deepcopy(SKU)
        sku["restrictions"] = [{"type": "NewRestriction"}]
        self.assertEqual(self.check(sku=sku)["read_checks_status"], "unknown")

    def test_disk_minimum_unknown_and_too_small_are_distinct(self):
        image = dict(IMAGE, disk_gib=None)
        self.assertEqual(self.check(image=image, disk=32)["checks"]["disk_size"], "unknown")
        self.assertEqual(self.check(disk=16)["checks"]["disk_size"], "blocked")
        self.assertEqual(self.check(image=None)["checks"]["image"], "unknown")

    def test_inheritance_does_not_assume_32_gib_or_require_size_metadata(self):
        for image in (dict(IMAGE, disk_gib=64), dict(IMAGE, disk_gib=None),
                      {k: v for k, v in IMAGE.items() if k != "disk_gib"}):
            with self.subTest(image=image):
                result = self.check(image=image)
                self.assertEqual(result["checks"]["disk_size"], "pass")
                self.assertEqual(result["read_checks_status"], "pass")

    def test_explicit_override_compares_with_image_minimum(self):
        image = dict(IMAGE, disk_gib=64)
        for disk, status in ((32, "blocked"), (64, "pass"), (128, "pass")):
            with self.subTest(disk=disk):
                self.assertEqual(self.check(image=image, disk=disk)["checks"]["disk_size"], status)

    def test_inheritance_does_not_hide_failed_image_read(self):
        result = self.check(image=None)
        self.assertEqual(result["checks"]["image"], "unknown")
        self.assertEqual(result["read_checks_status"], "unknown")

    def test_cli_disk_omission_and_existing_explicit_option(self):
        def read(args):
            if args[:2] == ["account", "show"]:
                return {"id": "fixture-private-id", "name": "Fixture", "state": "Enabled"}
            if args[:2] == ["provider", "list"]: return PROVIDERS
            if args[:2] == ["vm", "list-usage"]: return USAGE
            if args[:2] == ["vm", "list-skus"]: return [SKU]
            return dict(IMAGE, disk_gib=None)
        for extra, disk, mode, status in (([], None, "inherit", "pass"),
                                          (["--disk-gib", "32"], 32, "override", "unknown")):
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as directory:
                capture = io.StringIO()
                argv = ["survey", "--region", "eastasia", "--cache-dir", directory, *extra]
                with patch.object(survey.sys, "argv", argv), patch.object(survey, "az_json", side_effect=read), contextlib.redirect_stdout(capture):
                    self.assertEqual(survey.main(), 2)
                result = json.loads(capture.getvalue())
                self.assertEqual(result["disk_gib"], disk)
                self.assertEqual(result["disk_mode"], mode)
                self.assertEqual(result["regions"][0]["candidates"][0]["checks"]["disk_size"], status)
                self.assertFalse(result["deployment_gate"])

    def test_cli_invalid_disk_override_is_rejected_before_azure_read(self):
        for disk in ("0", "-1", "32.5", "null"):
            with self.subTest(disk=disk), patch.object(survey.sys, "argv", ["survey", "--disk-gib", disk]), patch.object(survey, "az_json") as read, contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error:
                    survey.main()
                self.assertEqual(error.exception.code, 2)
                read.assert_not_called()

    def test_image_architecture_generation_and_plan_are_gates(self):
        for update in ({"architecture": "x64"}, {"generation": "V1"}, {"plan": {"name": "paid"}}):
            self.assertEqual(self.check(image=dict(IMAGE, **update))["checks"]["image"], "blocked")

    def test_memory_and_accelerated_networking_are_gates(self):
        for name, value, gate in (("MemoryGB", "1", "minimum_resources"),
                                  ("AcceleratedNetworkingEnabled", "False", "accelerated_networking")):
            sku = copy.deepcopy(SKU)
            for capability in sku["capabilities"]:
                if capability["name"] == name:
                    capability["value"] = value
            self.assertEqual(self.check(sku=sku)["checks"][gate], "blocked")

    def test_unregistered_provider_blocks_catalog_positive_candidate(self):
        providers = copy.deepcopy(PROVIDERS)
        providers[1]["state"] = "NotRegistered"
        self.assertEqual(self.check(providers=providers)["read_checks_status"], "blocked")

    def test_read_allowlist_rejects_registration_and_create_before_process(self):
        with patch.object(survey.subprocess, "run") as process:
            for args in (["provider", "register"], ["vm", "create"], ["account", "set"], ["rest", "--method", "POST"]):
                with self.assertRaises(ValueError):
                    survey.az_json(args)
            process.assert_not_called()

    def test_cli_failure_does_not_publish_identifiers_or_cache_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(survey.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "secret subscription-id")):
                result = survey.cached_read(Path(directory), ["provider", "list"], 900, False)
            self.assertIsNone(result["data"])
            self.assertNotIn("secret", json.dumps(result))
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_fresh_cache_reuses_timestamp_and_refresh_bypasses_it(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(survey, "az_json", return_value=USAGE) as read:
                first = survey.cached_read(Path(directory), ["vm", "list-usage"], 900, False)
                second = survey.cached_read(Path(directory), ["vm", "list-usage"], 900, False)
                self.assertEqual(read.call_count, 1)
                self.assertEqual(first["observed_at"], second["observed_at"])
                self.assertTrue(second["cached"])
                survey.cached_read(Path(directory), ["vm", "list-usage"], 900, True)
                self.assertEqual(read.call_count, 2)

    def test_cli_fixture_reports_missing_skus_redacts_id_and_keeps_unknown_gate(self):
        calls = []
        def fake_read(args):
            calls.append(args)
            if args[:2] == ["account", "show"]:
                return {"id": "fixture-subscription-identifier", "name": "Fixture Students", "state": "Enabled", "isDefault": True}
            if args[:2] == ["provider", "list"]:
                return PROVIDERS
            if args[:2] == ["vm", "list-usage"]:
                return USAGE
            if args[:2] == ["vm", "list-skus"]:
                return [SKU]
            return IMAGE
        with tempfile.TemporaryDirectory() as directory:
            argv = ["survey", "--region", "japaneast", "--cache-dir", directory]
            capture = io.StringIO()
            with patch.object(survey.sys, "argv", argv), patch.object(survey, "az_json", side_effect=fake_read), contextlib.redirect_stdout(capture):
                self.assertEqual(survey.main(), 2)
            result = json.loads(capture.getvalue())
            self.assertFalse(result["deployment_gate"])
            self.assertEqual(result["regions"][0]["candidates"][0]["read_checks_status"], "pass")
            self.assertIn("Standard_D2pls_v6", result["regions"][0]["missing_skus"])
            self.assertNotIn("fixture-subscription-identifier", capture.getvalue())
            self.assertEqual(len(calls), 5)
            capture = io.StringIO()
            with patch.object(survey.sys, "argv", argv), patch.object(survey, "az_json", side_effect=fake_read), contextlib.redirect_stdout(capture):
                self.assertEqual(survey.main(), 2)
            self.assertEqual(len(calls), 6)  # Only current account identity is re-read.

    def test_broad_discovery_does_not_drop_new_arm_series_or_quota_families(self):
        usage, skus = survey.region_queries("eastasia", all_arm64=True)
        self.assertEqual(usage[usage.index("--query") + 1], "[].{name:name.value,label:name.localizedValue,current:currentValue,limit:limit}")
        self.assertIn("CpuArchitectureType", skus[skus.index("--query") + 1])
        self.assertNotIn("Standard_B2pls_v2", skus[skus.index("--query") + 1])

    def test_broad_discovery_finds_unlisted_series_and_excludes_small_or_gen1_vm(self):
        novel = copy.deepcopy(SKU)
        novel.update(name="Standard_D2pds_v6", family="newArmFamily")
        small = copy.deepcopy(novel)
        small["name"] = "Standard_TooSmall"
        small["capabilities"].append({"name": "MemoryGB", "value": "1"})
        gen1 = copy.deepcopy(novel)
        gen1["name"] = "Standard_Gen1"
        gen1["capabilities"].append({"name": "HyperVGenerations", "value": "V1"})
        def read(args):
            if args[:2] == ["account", "show"]:
                return {"id": "fixture-private-id", "name": "Fixture", "state": "Enabled"}
            if args[:2] == ["provider", "list"]: return PROVIDERS
            if args[:2] == ["vm", "list-usage"]: return USAGE + [{"name": "newArmFamily", "current": 0, "limit": 2}]
            if args[:2] == ["vm", "list-skus"]: return [novel, small, gen1]
            return IMAGE
        with tempfile.TemporaryDirectory() as directory:
            capture = io.StringIO()
            argv = ["survey", "--all-arm64", "--region", "eastasia", "--cache-dir", directory]
            with patch.object(survey.sys, "argv", argv), patch.object(survey, "az_json", side_effect=read), contextlib.redirect_stdout(capture):
                self.assertEqual(survey.main(), 2)
            result = json.loads(capture.getvalue())
            self.assertEqual([c["sku"] for c in result["regions"][0]["candidates"]], ["Standard_D2pds_v6"])
            self.assertEqual(result["regions"][0]["candidates"][0]["checks"]["family_quota"]["status"], "pass")
            self.assertNotIn("fixture-private-id", capture.getvalue())


if __name__ == "__main__":
    unittest.main()
