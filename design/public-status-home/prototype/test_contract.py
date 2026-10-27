"""Focused, offline design acceptance tests. No live credentials or network."""
import concurrent.futures
import copy
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

from contract import accept, data_state, project, sign, validate

AT = 1800000000000  # Synthetic, deliberately unrelated to current observations.
KEY = b"non-secret-offline-test-key"
ROOT = Path(__file__).resolve().parents[1]


def fixture():
    nodes = []
    for id_, site, arch in (("control", "laptop-vm", "amd64"),
                             ("physical", "physical-server", "amd64"),
                             ("cloud", "azure-vm", "arm64")):
        sample = lambda v: {"value": v, "observedAt": AT-2000}
        nodes.append({"id": id_, "site": site, "os": "linux", "arch": arch,
                      "readiness": "Ready", "readyObservedAt": AT-1000,
                      "cpu": sample(0.25), "memory": sample(0.5), "exporter": sample(1)})
    return {"schemaVersion": 1, "source": "observed", "collectedAt": AT,
            "sources": {k: {"state": "ok", "observedAt": AT, "errorCode": None}
                        for k in ("kubernetes", "prometheus")},
            "nodes": nodes,
            "workloads": [{"id": id_, "desired": 1, "ready": 1, "observedAt": AT}
                          for id_ in ("controller", "demo-api")],
            "policy": {"mode": "ObserveOnly", "enabled": True, "monitoring": True,
                       "incidentCount": 0, "activeIncidentCount": 0, "observedAt": AT}}


class PrivacyAndFreshness(unittest.TestCase):
    def test_valid_sample_and_real_zero(self):
        s = fixture(); s["nodes"][0]["cpu"]["value"] = 0
        validate(s); self.assertEqual(data_state(s, AT), ("current", "healthy"))

    def test_projection_omits_internal_fields_at_all_levels(self):
        s = fixture()
        s.update({"kubeconfig": "SYNTHETIC_PRIVATE_SENTINEL", "rawLogs": "SYNTHETIC_PRIVATE_SENTINEL"})
        for n in s["nodes"]:
            n.update({"name": "internal-node.private.test", "uid": "SYNTHETIC_UID",
                      "internalIP": "192.0.2.7", "tailscaleIP": "SYNTHETIC_PRIVATE_SENTINEL"})
            n["cpu"]["labels"] = {"instance": "SYNTHETIC_PRIVATE_SENTINEL"}
        s["policy"].update({"spec": {"token": "SYNTHETIC_PRIVATE_SENTINEL"}, "uid": "SYNTHETIC_UID"})
        s["workloads"][0]["env"] = {"PASSWORD": "SYNTHETIC_PRIVATE_SENTINEL"}
        out = project(s)
        text = json.dumps(out)
        for marker in ("SYNTHETIC", "192.0.2.7", "internal-node", "instance", "token", "kubeconfig"):
            self.assertNotIn(marker, text)
        self.assertEqual(out, fixture())

    def test_worker_schema_rejects_unknown_fields(self):
        for field in ("ip", "name", "uid", "token", "details", "log"):
            s = fixture(); s["nodes"][0][field] = "SYNTHETIC"
            with self.subTest(field=field), self.assertRaises(ValueError): validate(s)

    def test_rejects_internal_alias_and_free_form_policy_text(self):
        for change in (lambda s: s["nodes"][0].update(id="internal-node.private.test"),
                       lambda s: s["policy"].update(mode="arbitrary free text")):
            s = fixture(); change(s)
            with self.assertRaises(ValueError): validate(s)

    def test_rejects_duplicates_sites_future_times_and_version(self):
        changes = [lambda s: s["nodes"][0].update(id="physical"),
                   lambda s: s["nodes"][0].update(site="azure-vm"),
                   lambda s: s.update(schemaVersion=2),
                   lambda s: s["nodes"][0]["cpu"].update(observedAt=AT+5001)]
        for change in changes:
            s = fixture(); change(s)
            with self.assertRaises(ValueError): validate(s)

    def test_rejects_nonfinite_out_of_range_and_boolean_numbers(self):
        for bad in (float("nan"), float("inf"), -0.1, 1.1, True, 10**400):
            s = fixture(); s["nodes"][0]["cpu"]["value"] = bad
            with self.subTest(bad=bad), self.assertRaises(ValueError): validate(s)

    def test_null_is_unknown_and_requires_no_measurement_time(self):
        s = fixture(); s["nodes"][0]["cpu"] = {"value": None, "observedAt": None}
        self.assertEqual(data_state(s, AT), ("partial", "unknown"))
        s["nodes"][0]["cpu"]["observedAt"] = AT
        with self.assertRaises(ValueError): validate(s)

    def test_stale_collector_at_boundary(self):
        self.assertNotEqual(data_state(fixture(), AT+75000)[0], "stale")
        self.assertEqual(data_state(fixture(), AT+75001), ("stale", "unknown"))

    def test_original_sample_clock_is_not_refreshed(self):
        s = fixture(); s["nodes"][0]["cpu"]["observedAt"] = AT-90001
        out = project(s)
        self.assertEqual(out["nodes"][0]["cpu"]["observedAt"], AT-90001)
        self.assertEqual(data_state(out, AT), ("partial", "unknown"))

    def test_clock_skew_boundaries(self):
        self.assertEqual(data_state(fixture(), AT-5000), ("current", "healthy"))
        self.assertEqual(data_state(fixture(), AT-5001), ("unavailable", "unknown"))

    def test_failure_heartbeat_cannot_reuse_ready_values(self):
        s = fixture(); s["sources"]["kubernetes"] = {"state": "unavailable", "observedAt": None, "errorCode": "UNREACHABLE"}
        with self.assertRaises(ValueError): validate(s)
        for n in s["nodes"]:
            for k in ("os", "arch", "readiness", "readyObservedAt"): n[k] = None
        for w in s["workloads"]:
            for k in ("desired", "ready", "observedAt"): w[k] = None
        s["policy"] = dict.fromkeys(s["policy"], None)
        self.assertEqual(data_state(s, AT), ("partial", "unknown"))

    def test_observed_unhealthy_is_not_stale_or_simulated(self):
        s = fixture(); s["nodes"][0]["readiness"] = "NotReady"
        self.assertEqual(data_state(s, AT), ("current", "degraded"))
        s["policy"]["mode"] = "Enforce"  # Expose an unexpected real mode honestly.
        self.assertEqual(project(s)["policy"]["mode"], "Enforce")

    def test_terminal_history_is_not_presented_as_active_failure(self):
        s = fixture(); s["policy"]["incidentCount"] = 2
        self.assertEqual(data_state(s, AT), ("current", "healthy"))
        s["policy"]["activeIncidentCount"] = 1
        self.assertEqual(data_state(s, AT), ("current", "degraded"))

    def test_missing_platform_is_partial_not_healthy(self):
        s = fixture(); s["nodes"][0]["arch"] = None
        self.assertEqual(data_state(s, AT), ("partial", "unknown"))

    def test_scaled_to_zero_is_not_a_running_service(self):
        s = fixture(); s["workloads"][0].update(desired=0, ready=0)
        self.assertEqual(data_state(s, AT), ("current", "degraded"))


class AuthenticationAndStorage(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dbfile = str(Path(self.tmp.name)/"offline.sqlite")
        self.db = sqlite3.connect(self.dbfile)
        self.db.executescript((ROOT/"status-storage.sql").read_text())

    def tearDown(self):
        self.db.close(); self.tmp.cleanup()

    def request(self, seq=1, now=AT, raw=None, **overrides):
        raw = raw if raw is not None else json.dumps(fixture(), separators=(",", ":")).encode()
        values = dict(key=KEY, seq=seq, sent=AT, raw=raw, now=now,
                      signature=sign(KEY, "home-v1", "test-key", seq, AT, raw))
        values.update(overrides)
        return values

    def test_valid_request_and_same_request_replay_rejected(self):
        accept(self.db, **self.request())
        with self.assertRaises(ValueError): accept(self.db, **self.request(now=AT+15000))

    def test_tamper_wrong_key_and_wrong_signature_rejected(self):
        for kwargs in ({"raw": b"{}"}, {"key": b"wrong-public-test-key"}, {"signature": "0"*64}):
            r = self.request(); r.update(kwargs)
            with self.assertRaises(ValueError): accept(self.db, **r)
        self.assertEqual(self.db.execute("SELECT seq FROM collector_current").fetchone()[0], 0)

    def test_origin_path_method_and_identity_are_bound(self):
        for kwargs in ({"origin": "https://wrong.invalid"}, {"path": "/api/collect/status?x=1"},
                       {"method": "GET"}, {"collector": "unknown"}, {"key_id": "unknown"}):
            with self.assertRaises(ValueError): accept(self.db, **self.request(**kwargs))

    def test_sent_time_window_and_counter_bounds(self):
        for kwargs in ({"now": AT+120001}, {"now": AT-5001}, {"seq": True}, {"seq": 0}, {"seq": 2**53}):
            with self.assertRaises(ValueError): accept(self.db, **self.request(**kwargs))
        accept(self.db, **self.request(now=AT+120000))

    def test_oversize_and_duplicate_keys_rejected_after_correct_signature(self):
        valid = json.dumps(fixture()).encode()
        # Both would otherwise parse as a complete valid snapshot: avoid a test
        # that merely fails because required fields are absent.
        for raw in (b" "*16385 + valid, b'{"schemaVersion":2,' + valid[1:]):
            with self.assertRaises(ValueError): accept(self.db, **self.request(raw=raw))

    def test_global_minimum_interval_not_just_pop_memory(self):
        accept(self.db, **self.request())
        with self.assertRaises(ValueError): accept(self.db, **self.request(seq=2, now=AT+14999))
        accept(self.db, **self.request(seq=2, now=AT+15000))

    def test_older_sequence_and_counter_reset_rejected(self):
        accept(self.db, **self.request(seq=10))
        for seq in (9, 1, 10):
            with self.assertRaises(ValueError): accept(self.db, **self.request(seq=seq, now=AT+15000))

    def test_collector_measurement_must_not_move_backward(self):
        accept(self.db, **self.request())
        s = fixture(); s["collectedAt"] -= 5000
        raw = json.dumps(s).encode()
        with self.assertRaises(ValueError): accept(self.db, **self.request(seq=2, now=AT+15000, raw=raw))

    def test_sql_failure_does_not_advance_replay_counter(self):
        before = self.db.execute("SELECT * FROM collector_current").fetchall()
        self.db.execute("CREATE TRIGGER fail_write BEFORE UPDATE ON collector_current BEGIN SELECT RAISE(ABORT, 'offline injected SQL error'); END")
        with self.assertRaises(sqlite3.Error): accept(self.db, **self.request())
        self.db.rollback()
        self.assertEqual(self.db.execute("SELECT * FROM collector_current").fetchall(), before)

    def test_restart_preserves_replay_fence(self):
        accept(self.db, **self.request())
        self.db.close(); self.db = sqlite3.connect(self.dbfile)
        with self.assertRaises(ValueError): accept(self.db, **self.request(now=AT+15000))

    def test_missing_state_is_not_automatically_bootstrapped(self):
        self.db.execute("DELETE FROM collector_current"); self.db.commit()
        with self.assertRaises(ValueError): accept(self.db, **self.request())
        self.assertEqual(self.db.execute("SELECT count(*) FROM collector_current").fetchone()[0], 0)

    def test_failure_heartbeat_preserves_authentication_but_not_health(self):
        s = fixture()
        s["sources"]["prometheus"] = {"state": "unavailable", "observedAt": None, "errorCode": "TIMEOUT"}
        for n in s["nodes"]:
            for k in ("cpu", "memory", "exporter"): n[k] = {"value": None, "observedAt": None}
        result = accept(self.db, **self.request(raw=json.dumps(s).encode()))
        self.assertEqual(data_state(result, AT), ("partial", "unknown"))

    def test_schema_and_semantic_failures_do_not_advance_counter(self):
        for mutation in (lambda s: s.update(secret="NOT_A_REAL_SECRET"),
                         lambda s: s["nodes"][0].update(readiness=None)):
            s = fixture(); mutation(s)
            with self.assertRaises(ValueError): accept(self.db, **self.request(raw=json.dumps(s).encode()))
        self.assertEqual(self.db.execute("SELECT seq FROM collector_current").fetchone()[0], 0)

    def test_concurrent_duplicate_admission_has_one_winner(self):
        request = self.request()
        def invoke(_):
            with sqlite3.connect(self.dbfile, timeout=10) as db:
                try: accept(db, **request); return 1
                except ValueError: return 0
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            self.assertEqual(sum(pool.map(invoke, range(8))), 1)
        self.assertEqual(self.db.execute("SELECT seq FROM collector_current").fetchone()[0], 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)
