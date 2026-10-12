"""Offline drift and disclosure tests; no live credentials or network required."""
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

SPEC = importlib.util.spec_from_file_location("verify_platform", Path(__file__).resolve().parents[1] / "scripts/verify-platform.py")
platform = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(platform)


def healthy_fixture():
    e = {"nodes": [], "systemPods": [], "monitoringPods": [], "targets": [], "services": [{"type": "ClusterIP"}],
         "storageClasses": [{"name": "local-path", "provisioner": "rancher.io/local-path", "reclaimPolicy": "Delete", "volumeBindingMode": "WaitForFirstConsumer"}],
         "pvcs": [], "pvs": [], "release": [{"name": platform.RELEASE, "namespace": platform.NS, "chart": platform.CHART, "status": "deployed"}],
         "valuesComparison": {"checkedPaths": len(platform.VALUE_PATHS), "mismatchedPaths": []}}
    def pod(name, node=platform.CONTROL):
        return {"name": name, "node": node, "phase": "Running", "ready": True}
    def target(job, instance="10.42.0.1:8080", node=None, path="/metrics"):
        return {"job": job, "instance": instance, "node": node, "metricsPath": path, "health": "up", "hasError": False}
    for name, ip in platform.EXPECTED.items():
        e["nodes"].append({"name": name, "hostname": name, "ips": [ip], "arch": "amd64", "version": platform.VERSION,
                           "conditions": {"Ready": "True", "MemoryPressure": "False", "DiskPressure": "False", "PIDPressure": "False"}})
        e["monitoringPods"].append(pod("node-exporter-" + name, name))
        e["targets"].append(target("node-exporter", ip + ":9100"))
        for path in ("/metrics", "/metrics/cadvisor", "/metrics/probes"):
            e["targets"].append(target("kubelet", ip + ":10250", name, path))
    for name in ("coredns", "metrics-server", "local-path-provisioner"):
        e["systemPods"].append(pod(name))
    for name in ("grafana", "kube-state-metrics", "operator", "prometheus", "alertmanager"):
        e["monitoringPods"].append(pod(name))
    for job in ("apiserver", "coredns", "kube-state-metrics", "opsloop-mon-grafana", "opsloop-mon-kube-prometheu-alertmanager", "opsloop-mon-kube-prometheu-operator", "opsloop-mon-kube-prometheu-prometheus"):
        e["targets"].append(target(job, platform.EXPECTED[platform.CONTROL] + ":6443" if job == "apiserver" else "10.42.0.1:8080"))
    for name, size in (("prometheus", "10Gi"), ("grafana", "2Gi")):
        e["pvcs"].append({"name": name, "size": size, "phase": "Bound", "storageClass": "local-path", "volume": name + "-pv"})
        e["pvs"].append({"name": name + "-pv", "nodes": [platform.CONTROL], "storageClass": "local-path"})
    return e


class PlatformTests(unittest.TestCase):
    def test_healthy_local_fixture(self):
        self.assertEqual(platform.evaluate(healthy_fixture()), [])

    def test_missing_node_ip_version_arch_ready_pressure_and_third_node(self):
        base = healthy_fixture()
        changes = [{"ips": ["192.168.122.102"]}, {"version": "v1.36.0+k3s1"}, {"arch": "arm64"}, {"hostname": "archgram"},
                   {"unschedulable": True}, {"conditions": {"Ready": "False"}},
                   {"conditions": {"Ready": "True", "MemoryPressure": "True", "DiskPressure": "False", "PIDPressure": "False"}}]
        for change in changes:
            with self.subTest(change=change):
                e = copy.deepcopy(base)
                e["nodes"][0].update(change)
                self.assertTrue(platform.evaluate(e))
        base["nodes"].append(copy.deepcopy(base["nodes"][0]))
        self.assertTrue(platform.evaluate(base))
        base["nodes"] = []
        self.assertTrue(platform.evaluate(base))

    def test_missing_target_even_when_all_remaining_targets_up(self):
        for job in ("apiserver", "coredns", "kubelet", "kube-state-metrics", "node-exporter", "opsloop-mon-grafana", "opsloop-mon-kube-prometheu-prometheus", "opsloop-mon-kube-prometheu-alertmanager"):
            with self.subTest(job=job):
                e = healthy_fixture()
                e["targets"] = [t for t in e["targets"] if t["job"] != job]
                self.assertTrue(platform.evaluate(e))

    def test_target_error_missing_cadvisor_and_incorrect_identity(self):
        for change in ({"health": "down"}, {"hasError": True}, {"instance": "100.80.16.28:9100"}):
            e = healthy_fixture()
            e["targets"][0].update(change)
            self.assertTrue(platform.evaluate(e))
        e = healthy_fixture()
        e["targets"] = [t for t in e["targets"] if t["metricsPath"] != "/metrics/cadvisor"]
        self.assertTrue(platform.evaluate(e))
        e = healthy_fixture()
        for t in e["targets"]:
            if t["job"] == "kubelet":
                t["node"] = "wrong-node"
        self.assertTrue(platform.evaluate(e))

    def test_storage_locality_binding_and_service_exposure(self):
        for collection, change in (("pvcs", {"phase": "Pending"}), ("pvcs", {"size": "1Gi"}), ("pvs", {"nodes": ["sarmubuntuserver"]}),
                                   ("storageClasses", {"provisioner": "wrong"}), ("services", {"type": "NodePort"})):
            with self.subTest(collection=collection, change=change):
                e = healthy_fixture()
                e[collection][0].update(change)
                self.assertTrue(platform.evaluate(e))

    def test_helm_drift_and_pod_placement_health(self):
        e = healthy_fixture()
        e["valuesComparison"]["mismatchedPaths"] = ["prometheus.prometheusSpec.scrapeInterval"]
        self.assertTrue(platform.evaluate(e))
        e = healthy_fixture()
        e["release"][0]["chart"] = "kube-prometheus-stack-0.0.0"
        self.assertTrue(platform.evaluate(e))
        for change in ({"node": "sarmubuntuserver"}, {"ready": False}, {"phase": "Pending"}):
            e = healthy_fixture()
            e["monitoringPods"][-1].update(change)
            self.assertTrue(platform.evaluate(e))

    def test_allowlist_excludes_nested_credentials(self):
        marker = "SENTINEL_NOT_A_CREDENTIAL"
        raw = {"grafana": {"adminPassword": marker, "resources": {"requests": {"cpu": "50m", "password": marker}},
                           "nodeSelector": {"kubernetes.io/hostname": "opsloop-control", "password": marker}}, "token": marker}
        output = json.dumps(platform.safe_values(raw))
        self.assertNotIn(marker, output)
        self.assertIn("50m", output)

    def test_node_pod_target_sanitizers_exclude_sensitive_fields(self):
        marker = "SENTINEL_NOT_A_CREDENTIAL"
        node = {"items": [{"metadata": {"annotations": {"token": marker}}, "status": {}}]}
        pod = {"items": [{"metadata": {}, "spec": {"containers": [{"image": "safe", "env": [{"value": marker}]}]}}]}
        targets = {"status": "success", "data": {"activeTargets": [{"scrapeUrl": "http://user:" + marker + "@100.73.4.76:9100/metrics", "lastError": marker, "labels": {"password": marker}}]}}
        output = json.dumps([platform.sanitize_nodes(node), platform.sanitize_pods(pod), platform.sanitize_targets(targets)])
        self.assertNotIn(marker, output)
        self.assertIn("/metrics", output)

    def test_command_failure_does_not_echo_stdout_stderr(self):
        marker = "SENTINEL_NOT_A_CREDENTIAL"
        result = subprocess.CompletedProcess(["helm"], 1, marker, marker)
        with patch.object(platform.subprocess, "run", return_value=result):
            with self.assertRaises(RuntimeError) as error:
                platform.command_json(["helm", "get", "values"])
        self.assertNotIn(marker, str(error.exception))

    def test_invalid_values_yaml_main_outputs_only_fixed_error(self):
        marker = "SENTINEL_NOT_A_CREDENTIAL"
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "values.yaml"
            path.write_text('grafana:\n  adminPassword: "' + marker + '\n')
            # Collection succeeds up to YAML parsing; network commands are mocked.
            def response(command):
                if "list" in command:
                    return []
                if "values" in command:
                    return {}
                return {"items": []}
            output = io.StringIO()
            with patch.object(platform.sys, "argv", ["verify-platform.py", "--values", str(path)]), patch.object(platform, "command_json", side_effect=response), contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                code = platform.main()
            self.assertEqual(code, 2)
            self.assertEqual(json.loads(output.getvalue())["status"], "ERROR")
            self.assertNotIn(marker, output.getvalue())
            self.assertNotIn("Traceback", output.getvalue())


if __name__ == "__main__":
    unittest.main()
