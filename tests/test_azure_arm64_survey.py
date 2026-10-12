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
    def check(self, sku=SKU, usages=USAGE, image=IMAGE, providers=PROVIDERS, zone=None, disk=32):
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
        self.assertEqual(self.check(image=image)["checks"]["disk_size"], "unknown")
        self.assertEqual(self.check(disk=16)["checks"]["disk_size"], "blocked")
        self.assertEqual(self.check(image=None)["checks"]["image"], "unknown")

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


if __name__ == "__main__":
    unittest.main()
