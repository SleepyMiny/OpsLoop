#!/usr/bin/env python3
"""Read-only local two-node baseline; stdout contains allowlisted evidence only.

Exit 0: all selected checks pass. Exit 1: drift/unhealthy/incomplete evidence.
Exit 2: collection/configuration error. No kubeconfig or secret contents are read.
Requires Python 3 and PyYAML for the local, non-secret values file.
"""
import argparse
import datetime
import json
from pathlib import Path
import subprocess
import sys
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
EXPECTED = {"opsloop-control": "100.73.4.76", "sarmubuntuserver": "100.82.241.77"}
VERSION = "v1.35.9+k3s1"
RELEASE = "opsloop-mon"
CHART = "kube-prometheus-stack-91.8.1"
NS = "monitoring"
CONTROL = "opsloop-control"
# No arbitrary key recursive traversal: unknown Helm keys (including credentials)
# never enter evidence or error messages. Values themselves remain in memory.
VALUE_PATHS = (
    "kubeEtcd.enabled", "kubeControllerManager.enabled", "kubeScheduler.enabled", "kubeProxy.enabled",
    "defaultRules.rules.etcd", "defaultRules.rules.kubeControllerManager",
    "defaultRules.rules.kubeSchedulerAlerting", "defaultRules.rules.kubeSchedulerRecording", "defaultRules.rules.kubeProxy",
    "prometheus.service.type", "prometheus.prometheusSpec.nodeSelector", "prometheus.prometheusSpec.replicas",
    "prometheus.prometheusSpec.retention", "prometheus.prometheusSpec.retentionSize",
    "prometheus.prometheusSpec.scrapeInterval", "prometheus.prometheusSpec.evaluationInterval",
    "prometheus.prometheusSpec.resources", "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.storageClassName",
    "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.accessModes",
    "prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.resources.requests.storage",
    "grafana.nodeSelector", "grafana.service.type", "grafana.persistence.enabled", "grafana.persistence.type",
    "grafana.persistence.storageClassName", "grafana.persistence.accessModes", "grafana.persistence.size", "grafana.resources",
    "alertmanager.alertmanagerSpec.replicas", "alertmanager.alertmanagerSpec.nodeSelector", "alertmanager.alertmanagerSpec.resources",
    "prometheusOperator.nodeSelector", "prometheusOperator.resources", "kube-state-metrics.nodeSelector",
    "kube-state-metrics.resources", "prometheus-node-exporter.resources",
)


def get_path(value, path):
    for key in path.split("."):
        if not isinstance(value, dict) or key not in value:
            return None
        value = value[key]
    return value


def safe_values(values):
    """Filter leaves too: an unexpected nested secret must not escape."""
    result = {}
    for path in VALUE_PATHS:
        value = get_path(values, path)
        if path.endswith("nodeSelector"):
            value = {"kubernetes.io/hostname": (value or {}).get("kubernetes.io/hostname")}
        elif path.endswith("resources"):
            value = {kind: {key: (value or {}).get(kind, {}).get(key)
                            for key in ("cpu", "memory")} for kind in ("requests", "limits")}
        result[path] = value
    return result


def command_json(command):
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=45, check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeError(f"{Path(command[0]).name}: {type(exc).__name__}; no raw output emitted") from None
    if result.returncode:
        # Never print stderr: Helm or auth helpers can return credentials there.
        raise RuntimeError(f"{Path(command[0]).name}: read-only command failed (exit {result.returncode}); no raw output emitted")
    try:
        return json.loads(result.stdout)
    except (ValueError, TypeError):
        raise RuntimeError(f"{Path(command[0]).name}: invalid JSON; no raw output emitted") from None


def sanitize_nodes(raw):
    nodes = []
    for node in raw.get("items", []):
        metadata, status = node.get("metadata", {}), node.get("status", {})
        info = status.get("nodeInfo", {})
        conditions = {c["type"]: c["status"] for c in status.get("conditions", [])}
        nodes.append({"name": metadata.get("name"), "hostname": metadata.get("labels", {}).get("kubernetes.io/hostname"),
                      "ips": [a["address"] for a in status.get("addresses", []) if a.get("type") == "InternalIP"],
                      "arch": info.get("architecture"), "version": info.get("kubeletVersion"), "os": info.get("osImage"),
                      "runtime": info.get("containerRuntimeVersion"), "conditions": conditions,
                      "unschedulable": node.get("spec", {}).get("unschedulable", False),
                      "cpu": status.get("allocatable", {}).get("cpu"), "memory": status.get("allocatable", {}).get("memory")})
    return nodes


def sanitize_pods(raw):
    pods = []
    for pod in raw.get("items", []):
        metadata, spec, status = pod.get("metadata", {}), pod.get("spec", {}), pod.get("status", {})
        labels = metadata.get("labels", {})
        pods.append({"name": metadata.get("name"), "namespace": metadata.get("namespace"),
                     "component": labels.get("app.kubernetes.io/name", labels.get("app", "")),
                     "node": spec.get("nodeName"), "phase": status.get("phase"),
                     "ready": any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", [])),
                     "restarts": sum(c.get("restartCount", 0) for c in status.get("containerStatuses", [])),
                     "images": [c.get("image") for c in spec.get("containers", [])]})
    return pods


def sanitize_targets(raw):
    if raw.get("status") != "success":
        raise RuntimeError("Prometheus targets API did not return success")
    return [{"job": t.get("labels", {}).get("job"), "instance": t.get("labels", {}).get("instance"),
             "node": t.get("labels", {}).get("node"), "endpoint": t.get("labels", {}).get("endpoint"), "health": t.get("health"),
             "metricsPath": urlsplit(t.get("scrapeUrl", "")).path,
             "lastScrape": t.get("lastScrape"), "scrapeInterval": t.get("scrapeInterval"),
             "hasError": bool(t.get("lastError"))} for t in raw.get("data", {}).get("activeTargets", [])]


def collect(args):
    k = [args.kubectl, "--kubeconfig", str(Path(args.kubeconfig).expanduser()), "--request-timeout=30s"]
    def get(*arguments):
        try:
            return command_json(k + ["get", *arguments, "-o", "json"])
        except RuntimeError:
            raise RuntimeError(f"Kubernetes {arguments[0]} GET failed; no raw output emitted") from None
    evidence = {"observedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(), "scope": "local-two-node-baseline"}
    evidence["nodes"] = sanitize_nodes(get("nodes"))
    evidence["systemPods"] = sanitize_pods(get("pods", "-n", "kube-system"))
    evidence["monitoringPods"] = sanitize_pods(get("pods", "-n", NS))
    evidence["services"] = [{"name": s["metadata"]["name"], "type": s.get("spec", {}).get("type")}
                            for s in get("services", "-n", NS).get("items", [])]
    evidence["storageClasses"] = [{"name": s["metadata"]["name"], "provisioner": s.get("provisioner"),
                                   "reclaimPolicy": s.get("reclaimPolicy"), "volumeBindingMode": s.get("volumeBindingMode")}
                                  for s in get("storageclass").get("items", [])]
    evidence["pvcs"] = [{"name": p["metadata"]["name"], "phase": p.get("status", {}).get("phase"),
                        "storageClass": p.get("spec", {}).get("storageClassName"),
                        "size": p.get("status", {}).get("capacity", {}).get("storage"),
                        "volume": p.get("spec", {}).get("volumeName")}
                       for p in get("pvc", "-n", NS).get("items", [])]
    bound = {p["volume"] for p in evidence["pvcs"]}
    evidence["pvs"] = [{"name": p["metadata"]["name"], "storageClass": p.get("spec", {}).get("storageClassName"),
                       "reclaimPolicy": p.get("spec", {}).get("persistentVolumeReclaimPolicy"),
                       "nodes": [v for term in p.get("spec", {}).get("nodeAffinity", {}).get("required", {}).get("nodeSelectorTerms", [])
                                 for expr in term.get("matchExpressions", []) if expr.get("key") == "kubernetes.io/hostname"
                                 for v in expr.get("values", [])]}
                      for p in get("pv").get("items", []) if p["metadata"]["name"] in bound]
    h = [args.helm, "--kubeconfig", str(Path(args.kubeconfig).expanduser())]
    releases = command_json(h + ["list", "-n", NS, "--filter", "^" + RELEASE + "$", "-o", "json"])
    evidence["release"] = [{key: r.get(key) for key in ("name", "namespace", "revision", "status", "chart", "app_version")} for r in releases]
    # Raw deployed values exist only in captured subprocess memory, never logs/files.
    remote_values = safe_values(command_json(h + ["get", "values", RELEASE, "-n", NS, "-o", "json"]))
    import yaml
    try:
        with Path(args.values).open() as stream:
            local_values = safe_values(yaml.safe_load(stream) or {})
    except yaml.YAMLError:
        raise RuntimeError("Local values YAML is invalid; no raw parser output emitted") from None
    evidence["valuesComparison"] = {"checkedPaths": len(VALUE_PATHS), "mismatchedPaths": [p for p in VALUE_PATHS if local_values[p] != remote_values[p]]}
    evidence["intervals"] = {"scrape": remote_values["prometheus.prometheusSpec.scrapeInterval"], "evaluation": remote_values["prometheus.prometheusSpec.evaluationInterval"]}
    prometheus_services = [s["name"] for s in evidence["services"] if s["name"].startswith(RELEASE + "-") and s["name"].endswith("-prometheus")]
    if len(prometheus_services) != 1:
        raise RuntimeError("Could not identify exactly one release Prometheus Service")
    path = f"/api/v1/namespaces/{NS}/services/http:{prometheus_services[0]}:9090/proxy/api/v1/targets"
    evidence["targets"] = sanitize_targets(command_json(k + ["get", "--raw", path]))
    return evidence


def evaluate(e):
    failures = []
    def check(ok, message):
        if not ok:
            failures.append(message)
    nodes = e.get("nodes", [])
    check(len(nodes) == 2 and {n.get("name") for n in nodes} == set(EXPECTED), "expected exactly the two named local nodes; Azure third node is outside this baseline")
    for n in nodes:
        name = n.get("name")
        check(n.get("ips") == [EXPECTED.get(name)], f"node {name}: InternalIP mismatch")
        check(n.get("hostname") == name, f"node {name}: hostname label mismatch")
        check(n.get("arch") == "amd64" and n.get("version") == VERSION, f"node {name}: architecture/version mismatch")
        check(n.get("conditions", {}).get("Ready") == "True", f"node {name}: not Ready")
        check(not n.get("unschedulable"), f"node {name}: unschedulable")
        for pressure in ("MemoryPressure", "DiskPressure", "PIDPressure"):
            check(n.get("conditions", {}).get(pressure) == "False", f"node {name}: {pressure} missing or active")
    for key in ("systemPods", "monitoringPods"):
        pods = e.get(key, [])
        check(bool(pods), f"{key}: no evidence")
        for p in pods:
            check(p.get("phase") == "Succeeded" or p.get("phase") == "Running" and p.get("ready"), f"pod {p.get('name')}: not healthy")
    system = e.get("systemPods", [])
    for component in ("coredns", "metrics-server", "local-path-provisioner"):
        check(any(component in p.get("name", "") for p in system), f"system component {component}: absent")
    check(not any("traefik" in p.get("name", "") or "svclb-" in p.get("name", "") for p in system), "Traefik/ServiceLB pods present")
    monitoring = [p for p in e.get("monitoringPods", []) if p.get("phase") != "Succeeded"]
    exporters = [p for p in monitoring if "node-exporter" in p.get("component", "") or "node-exporter" in p.get("name", "")]
    check(len(exporters) == 2 and {p.get("node") for p in exporters} == set(EXPECTED), "node exporters must cover exactly both nodes")
    for p in monitoring:
        if p not in exporters:
            check(p.get("node") == CONTROL, f"monitoring pod {p.get('name')}: control-plane placement mismatch")
    for component in ("grafana", "kube-state-metrics", "operator", "prometheus", "alertmanager"):
        check(any(component in p.get("name", "") for p in monitoring if p not in exporters), f"monitoring component {component}: absent")
    releases = e.get("release", [])
    check(len(releases) == 1 and releases[0].get("name") == RELEASE and releases[0].get("namespace") == NS and releases[0].get("status") == "deployed" and releases[0].get("chart") == CHART, "monitoring Helm release/chart mismatch")
    comparison = e.get("valuesComparison", {})
    check(comparison.get("checkedPaths") == len(VALUE_PATHS) and comparison.get("mismatchedPaths") == [], "allowlisted Helm values differ or comparison incomplete")
    for kind, size in (("prometheus", "10Gi"), ("grafana", "2Gi")):
        claims = [p for p in e.get("pvcs", []) if kind in p.get("name", "")]
        check(len(claims) == 1 and claims[0].get("phase") == "Bound" and claims[0].get("size") == size and claims[0].get("storageClass") == "local-path", f"{kind} PVC binding/class/size mismatch")
        for claim in claims:
            check(any(p.get("name") == claim.get("volume") and p.get("nodes") == [CONTROL] and p.get("storageClass") == "local-path" for p in e.get("pvs", [])), f"{kind} PV control-plane locality missing")
    services = e.get("services", [])
    check(any(s.get("name") == "local-path" and s.get("provisioner") == "rancher.io/local-path" and s.get("reclaimPolicy") == "Delete" and s.get("volumeBindingMode") == "WaitForFirstConsumer" for s in e.get("storageClasses", [])), "local-path StorageClass provisioner/reclaim/binding mismatch")
    check(bool(services) and all(s.get("type") == "ClusterIP" for s in services), "monitoring services must remain ClusterIP")
    targets = e.get("targets", [])
    check(bool(targets), "Prometheus active targets absent")
    for target in targets:
        check(target.get("health") == "up" and not target.get("hasError"), f"Prometheus target {target.get('job')} {target.get('instance')}: unhealthy")
    exporter_targets = [t for t in targets if "node-exporter" in (t.get("job") or "")]
    check(len(exporter_targets) == 2 and {t.get("instance") for t in exporter_targets} == {ip + ":9100" for ip in EXPECTED.values()}, "Prometheus exporter target IP mapping mismatch")
    check(any("kube-state-metrics" in (t.get("job") or "") for t in targets), "kube-state-metrics scrape target absent")
    for job in ("apiserver", "coredns", "opsloop-mon-grafana", "opsloop-mon-kube-prometheu-alertmanager", "opsloop-mon-kube-prometheu-operator", "opsloop-mon-kube-prometheu-prometheus"):
        check(any(t.get("job") == job and t.get("metricsPath") == "/metrics" for t in targets), f"{job} /metrics scrape target absent")
    check(any(t.get("job") == "apiserver" and t.get("instance") == EXPECTED[CONTROL] + ":6443" for t in targets), "apiserver target IP mismatch")
    for name, ip in EXPECTED.items():
        for metrics_path in ("/metrics", "/metrics/cadvisor", "/metrics/probes"):
            check(any(t.get("job") == "kubelet" and t.get("node") == name and t.get("instance") == ip + ":10250" and t.get("metricsPath") == metrics_path for t in targets), f"node {name}: kubelet {metrics_path} target absent or mismapped")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubectl", default=str(Path.home() / ".local/bin/kubectl-opsloop"))
    parser.add_argument("--kubeconfig", default=str(Path.home() / ".kube/opsloop.yaml"))
    parser.add_argument("--helm", default="helm")
    parser.add_argument("--values", default=str(ROOT / "infra/observability/kube-prometheus-stack.values.yaml"))
    args = parser.parse_args()
    try:
        evidence = collect(args)
        failures = evaluate(evidence)
    except (RuntimeError, OSError, ImportError, ValueError, TypeError, KeyError, AttributeError) as exc:
        # Do not include arbitrary file/parser exception text, which may quote values.
        message = str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__
        print(json.dumps({"status": "ERROR", "error": message}))
        return 2
    print(json.dumps({"status": "FAIL" if failures else "PASS", "failures": failures, "evidence": evidence}, indent=2))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
