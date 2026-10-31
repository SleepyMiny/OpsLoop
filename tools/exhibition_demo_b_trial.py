"""Reused Demo B guarded trial core. Imported only by the same-run exhibition handoff.
Historical execution logic is retained; setup is never dispatched by the UI.
ROOT is a fresh approved run directory, never the historical evidence directory.
"""
import argparse
import copy
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import time
from tools.exhibition_terminal import TerminalObserved

ROOT = Path(__file__).resolve().parents[1]
NS, APP = "opsloop-demo-b", "engineering-demo"
POLICY_NAME = "engineering-demo"
CORE = "/api/v1/namespaces/" + NS
POLICY = "/apis/opsloop.io/v1alpha1/namespaces/" + NS + "/recoverypolicies/" + APP
DEPLOY = "/apis/apps/v1/namespaces/" + NS + "/deployments/" + APP
CONTROLLER = "/apis/apps/v1/namespaces/opsloop-system/deployments/opsloop-controller"
SYSTEM = "/api/v1/namespaces/opsloop-system"
GRANT = "opsloop.io/execution-grant"
EXHIBITION_PERMIT = "opsloop.io/exhibition-permit"
FILES = ("incident.json", "policy.json", "deployment-before.json", "deployment-after.json", "pod-before.json",
         "pod-after.json", "replicasets.json", "events.json", "observations.json", "logs.json")


class Stop(RuntimeError):
    def __init__(self, message, diagnostic=None):
        super().__init__(message)
        self.diagnostic = diagnostic


def require(ok, reason):
    if not ok: raise Stop(reason)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def sha(data): return hashlib.sha256(data).hexdigest()
def now(): return dt.datetime.now(dt.timezone.utc)
def stamp(): return now().isoformat().replace("+00:00", "Z")
def instant(value): return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def private_new(path, value):
    data = json.dumps(value, indent=2, ensure_ascii=False).encode() + b"\n"
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as f:
        f.write(data); f.flush(); os.fsync(f.fileno())
    return sha(data)


def atomic(path, value):
    temporary = path.with_suffix(path.suffix + ".new")
    private_new(temporary, value); os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)


def env(): return {"PATH": "/usr/bin:/bin", "HOME": str(ROOT / "work/empty-home"), "LANG": "C", "LC_ALL": "C"}


def policy_generations(p):
    """Cumulative Policy generation is not a per-exhibition budget."""
    if "exhibitionPermit" not in p:
        require(p["executionScope"]["policyGeneration"] == 3, "legacy generation changed")
        return 1, 2, 3, 4
    initial = p.get("initialPolicyGeneration")
    require(type(initial) is int and initial >= 1 and p["executionScope"]["policyGeneration"] == initial + 2,
            "fixed Policy generation baseline changed")
    return initial, initial + 1, initial + 2, initial + 3


def exhibition_permit(p, setup_record=None, *, current=True):
    if "exhibitionPermit" not in p: return None, None
    from tools.exhibition_permit import bound_permit
    binding = {key: p["executionScope"][key] for key in ("runID", "epoch", "policyUID", "deploymentUID")}
    envelope, payload = bound_permit({"exhibitionPermit": p["exhibitionPermit"],
                                    "authorizationId": p["authorizationId"], "binding": binding}, current=current)
    require(payload["grant"]["scope"] == p["executionScope"] and binding["runID"] == p["runID"] and binding["epoch"] == p["epoch"],
            "signed Runtime scope changed")
    seq = p.get("initialIncidentSequence")
    require(type(seq) is int and seq >= 0 and seq == payload["baseIncidentSequence"], "lifetime incident sequence changed")
    scope = p["executionScope"]
    require(scope["incident"] == "%s-%s-%05d" % (scope["policy"], scope["policyUID"][:8], seq + 1), "next incident identity changed")
    require(p["enforcePolicySpec"].get("budget", {}).get("maxActionsPerIncident") == 1, "Runtime action budget changed")
    if setup_record is not None:
        require(payload["grant"]["instance"] == setup_record["authorityController"]["instance"], "signed Controller instance changed")
    return envelope, payload


def initial_policy(pol, p):
    initial, _, _, _ = policy_generations(p)
    exact_policy(pol, p, initial, p["initialPolicySpec"])
    require(pol["spec"].get("enabled") is False and not pol.get("status", {}).get("activeIncident"), "initial Policy is not stopped")
    if "exhibitionPermit" in p:
        require(pol.get("status", {}).get("incidentSequence", 0) == p["initialIncidentSequence"], "lifetime incident sequence changed")
        require(any(c.get("type") == "Quiesced" and c.get("status") == "True" and c.get("reason") == "FenceClosed" and c.get("observedGeneration") == initial
                    for c in pol.get("status", {}).get("conditions", [])), "initial Policy fence unconfirmed")
        require(not any(k in pol["metadata"].get("annotations", {}) for k in (GRANT, EXHIBITION_PERMIT)), "previous run authority not cleaned")


def granted_at(p, setup_record, observed_at, remaining, signed_payload=None):
    if signed_payload is None:
        core = {"version": "opsloop-execution-grant/v1", "scope": p["executionScope"],
                "instance": setup_record["authorityController"]["instance"], "notBefore": observed_at.isoformat(),
                "notAfter": (observed_at + dt.timedelta(seconds=remaining)).isoformat()}
    else:
        core = copy.deepcopy(signed_payload["grant"])
        require(core["scope"] == p["executionScope"] and core["instance"] == setup_record["authorityController"]["instance"], "signed grant binding changed")
        require(instant(core["notBefore"]) <= observed_at < instant(core["notAfter"]), "signed grant clock outside fixed window")
    return {**core, "heartbeatSequence": 1, "heartbeatAt": observed_at.isoformat()}


def remember_incident(incident, p, grant_binding, record, save):
    """First API-observed same-run identity is durable before phase/capture logic.

    A restarted trial must not adopt an existing custody file. Cleanup reads the
    file independently, but this mutating trial is never resumed/replayed.
    """
    scope = p["executionScope"]
    metadata = incident.get("metadata", {})
    spec = incident.get("spec", {})
    annotations = metadata.get("annotations", {})
    require(isinstance(grant_binding, str) and re.fullmatch("[a-f0-9]{64}", grant_binding), "Incident grant binding unavailable")
    require(metadata.get("name") == scope["incident"] and metadata.get("namespace") == scope["namespace"]
            and isinstance(metadata.get("uid"), str) and metadata["uid"] and not metadata.get("deletionTimestamp"), "Incident identity unconfirmed")
    require(spec.get("policyRef", {}).get("uid") == scope["policyUID"]
            and spec.get("policyRef", {}).get("name") == scope["policy"]
            and spec.get("policyGeneration") == scope["policyGeneration"] and spec.get("policyHash") == scope["policyHash"]
            and spec.get("workload", {}).get("uid") == scope["deploymentUID"] and spec.get("workload", {}).get("name") == scope["deployment"], "Incident scope binding changed")
    require(annotations.get("opsloop.io/execution-grant-binding") == grant_binding, "Incident grant binding changed")
    if "exhibitionPermit" in p:
        require(annotations.get("opsloop.io/exhibition-run") == p["runID"]
                and annotations.get("opsloop.io/exhibition-epoch") == p["epoch"], "Incident run binding changed")
    identity = {"schema": "opsloop-exhibition-incident-identity/v1",
                "binding": {key: scope[key] for key in ("runID", "epoch", "policyUID", "deploymentUID")},
                "namespace": scope["namespace"], "name": scope["incident"], "uid": metadata["uid"], "grantBinding": grant_binding}
    path = ROOT / "incident-identity.json"
    if "incidentIdentitySHA256" in record:
        require(path.is_file() and not path.is_symlink(), "Incident custody file unavailable")
        raw = path.read_bytes()
        require(sha(raw) == record["incidentIdentitySHA256"], "Incident custody changed")
        previous = json.loads(raw)
        require({k: previous.get(k) for k in identity} == identity, "Incident UID replaced after first observation")
        require(record.get("incidentUID") == metadata["uid"], "Incident custody record changed")
        return identity
    require(not path.exists() and not path.is_symlink() and not record.get("incidentUID"), "existing Incident custody cannot restart trial")
    checksum = private_new(path, {**identity, "observedAt": stamp()})
    fd = os.open(ROOT, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    record.update(incidentUID=metadata["uid"], incidentIdentitySHA256=checksum)
    save()
    return identity


def load(plan_file, expected):
    require(__debug__, "optimized Python is not an approved execution mode")
    raw = plan_file.read_bytes(); require(sha(raw) == expected, "exact approved plan hash required")
    p = json.loads(raw)
    model = p.get("conditionalModelAuthorization", {})
    require((model.get("model"), model.get("reasoningEffort"), model.get("authMode")) ==
            ("gpt-6-luna", "medium", "chatgpt") and model.get("requests") == 1 and
            model.get("automaticFallback") is False and model.get("automaticRetry") is False,
            "superseded or unapproved model execution plan; only Luna Medium ChatGPT once is allowed")
    require(p["schema"] == "opsloop-demo-b-runtime-plan/v1" and p["namespace"] == NS and p["executionScope"]["deployment"] == APP, "plan scope invalid")
    policy_generations(p); exhibition_permit(p)
    require(p["maximumTrialMS"] == 600000 and p["renewEveryMS"] == 2000 and p["heartbeatMS"] == 10000, "finite execution bounds changed")
    require(now() < instant(p["expiresAt"]), "approved plan expired")
    require(sha(Path(__file__).read_bytes()) == p["runtimeScriptSHA256"], "runtime script changed after approval")
    require(sha(Path(p["snapshotFile"]).read_bytes()) == p["snapshotSHA256"], "source snapshot changed")
    for key, helper in p["helpers"].items():
        path = Path(helper["path"])
        require(not path.is_symlink() and path.is_file() and sha(path.read_bytes()) == helper["sha256"], "pinned helper changed: " + key)
    for name, key in (("initial-deployment-result-v2.json", "initialDeploymentSHA256"), ("image-provenance.json", "provenanceSHA256")):
        require(sha((ROOT / "verification" / name).read_bytes()) == p[key], "prior evidence changed: " + name)
    h = p["helpers"]["kubernetes"]
    spec = importlib.util.spec_from_file_location("demo_b_pinned_kube", h["path"])
    d = importlib.util.module_from_spec(spec); spec.loader.exec_module(d)
    context, origin = d.identity()
    require(origin == p["apiOrigin"] == "https://100.73.4.76:6443" and context == "default", "explicit API origin/context changed")
    return p, d, context


class Client:
    def __init__(self, d, context, record, save):
        self.d, self.context, self.record, self.save = d, context, record, save
        self.heartbeat_config = None

    def request(self, args, payload=None, absent=False, raw=False, permit=None):
        require(len(self.record["requests"]) < 3000, "request budget exhausted")
        argv = [self.d.CLIENT, "--kubeconfig", self.d.CONFIG, "--context", self.context, "--request-timeout=2s", *args]
        entry = {"argv": argv, "at": stamp(), "state": "PRE_DISPATCH_CHECK", "inputSHA256": sha(b'' if payload is None else canonical(payload))}
        self.record["requests"].append(entry); self.save()
        try:
            if permit: permit()
        except Exception as exc:
            entry.update(state='BLOCKED_BEFORE_DISPATCH', error_type=type(exc).__name__); self.save()
            raise
        entry.update(state='DISPATCHING', dispatchedAt=stamp()); self.save()
        try:
            r = subprocess.run(argv, input=None if payload is None else canonical(payload), capture_output=True, timeout=4, env=env())
        except (OSError, subprocess.TimeoutExpired) as exc:
            diagnostic=failed_command('kubernetes',argv,payload,error=exc)
            entry.update(state='DISPATCH_UNCONFIRMED', diagnostic=diagnostic, responseDisposition='TRANSPORT_UNCONFIRMED'); self.save()
            raise Stop("API dispatch unconfirmed; no write replay", diagnostic) from None
        entry.update(exit_code=r.returncode, response_sha256=sha(r.stdout))
        require(len(r.stdout) <= 4 << 20, "API response size limit")
        if r.returncode:
            diagnostic=failed_command('kubernetes',argv,payload,result=r)
            classification = next((x for x in ("notfound", "not found", "forbidden", "conflict", "timeout") if x in r.stderr.decode(errors="replace").lower()), "rejected-or-unknown")
            entry.update(state="REJECTED_OR_UNKNOWN", error_class=classification, diagnostic=diagnostic, responseDisposition=diagnostic["responseDisposition"], kubernetesStatus=diagnostic["kubernetesStatus"]); self.save()
            if absent and classification in ("notfound", "not found"): return None
            raise Stop("API " + classification + "; no automatic write replay", diagnostic)
        entry["state"] = "RESPONSE_RECEIVED"; self.save()
        return r.stdout if raw else json.loads(r.stdout)

    def heartbeat_patch(self,p,payload,directory,before_send):
        from tools.exhibition_runtime import static_kubeconfig
        from tools.exhibition_heartbeat import send
        config=Path(self.d.CONFIG);raw=config.read_bytes()
        require(static_kubeconfig(raw)['current-context']==self.context,'heartbeat auth context changed')
        ref={'path':str(config),'sha256':sha(raw)}
        if self.heartbeat_config is None:self.heartbeat_config=ref
        require(ref==self.heartbeat_config,'heartbeat auth identity changed')
        settings={'kubeconfig':ref,'apiOrigin':p['apiOrigin'],'namespace':NS,'policy':POLICY_NAME}
        entry={'at':stamp(),'target':POLICY,'method':'PATCH','inputSHA256':sha(canonical(payload)),'state':'PRE_DISPATCH_CHECK','heartbeatOnly':True}
        self.record['requests'].append(entry);self.save()
        def checked_send():
            before_send();entry.update(state='DISPATCHING',dispatchedAt=stamp());self.save()
        try:response=send(settings,POLICY,payload,directory,checked_send)
        except Exception:
            entry['state']='APPLY_UNKNOWN' if (directory/'dispatch.json').exists() else 'BLOCKED_BEFORE_DISPATCH';self.save();raise
        entry.update(state=response['disposition'],httpStatus=response['httpStatus'],diagnostic=str(directory/'response.json'));self.save()
        private_new(directory/'response.json',response)
        return response

    def get(self, path, absent=False): return self.request(["get", "--raw", path], absent=absent)
    def log(self, ns, pod, container):
        data = self.request(["get", "--raw", "/api/v1/namespaces/" + ns + "/pods/" + pod + "/log?container=" + container + "&limitBytes=16384"], raw=True)
        require(len(data) <= 16384, "bounded log response exceeded")
        return data.decode(errors="replace")

    def listing(self, path):
        result = self.get(path + "?limit=100")
        require(not result.get("metadata", {}).get("continue") and len(result.get("items", [])) <= 100, "list is incomplete")
        return result

    def patch(self, kind, name, namespace, patch, permit=None):
        require((kind, name, namespace) in (("deployment", "opsloop-controller", "opsloop-system"), ("deployment", APP, NS), ("recoverypolicy", POLICY_NAME, NS)), "mutation target denied")
        return self.request(["patch", kind, name, "-n", namespace, "--type=json", "--patch-file=/dev/stdin", "-o", "json"], patch, permit=permit)


def cas(obj):
    return [{"op": "test", "path": "/metadata/uid", "value": obj["metadata"]["uid"]},
            {"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]},
            {"op": "test", "path": "/spec", "value": obj["spec"]}]


def exact_policy(pol, p, generation, spec):
    m = pol["metadata"]
    require(m["uid"] == p["executionScope"]["policyUID"] and m["generation"] == generation and pol["spec"] == spec and not m.get("deletionTimestamp"), "Policy identity/generation/spec changed")


def identity(c, p):
    require(c.get("/api/v1/namespaces/kube-system")["metadata"]["uid"] == p["clusterUID"], "cluster UID changed")
    ns = c.get(CORE); require(ns["metadata"]["uid"] == p["executionScope"]["namespaceUID"] and not ns["metadata"].get("deletionTimestamp"), "Namespace identity changed")


def preserve_demo_a(c, p):
    a = p["demoA"]
    pol = c.get("/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo-a/recoverypolicies/" + a["policyName"])
    dep = c.get("/apis/apps/v1/namespaces/opsloop-demo-a/deployments/demo-api")
    require(pol["metadata"]["uid"] == a["policyUID"] and pol["metadata"]["generation"] == a["policyGeneration"] and pol["spec"] == a["policySpec"] and not pol.get("status", {}).get("activeIncident"), "Demo A Policy changed")
    require(dep["metadata"]["uid"] == a["deploymentUID"] and dep["spec"] == a["deploymentSpec"] and dep["status"].get("readyReplicas") == 1, "Demo A Deployment spec/health changed")
    return {"policyUID": a["policyUID"], "generation": a["policyGeneration"], "enabled": False, "deploymentUID": a["deploymentUID"], "specSHA256": sha(canonical(dep["spec"]))}


def controller_pod(c, p):
    rss = c.listing("/apis/apps/v1/namespaces/opsloop-system/replicasets")["items"]
    owned = {r["metadata"]["uid"] for r in rss if any(x.get("controller") and x.get("kind") == "Deployment" and x.get("uid") == p["controllerUID"] for x in r["metadata"].get("ownerReferences", []))
             and r["spec"]["template"].get("metadata", {}).get("annotations", {}).get("opsloop.io/demo-a-authority-scope") == p["scopeSHA256"]}
    pods = c.listing(SYSTEM + "/pods")["items"]
    require(not any(x["metadata"]["uid"] in p["priorControllerPodUIDs"] and x.get("status", {}).get("phase") not in ("Succeeded", "Failed") for x in pods), "old Controller Pod still exists")
    live = [x for x in pods if not x["metadata"].get("deletionTimestamp") and any(r.get("controller") and r.get("kind") == "ReplicaSet" and r.get("uid") in owned for r in x["metadata"].get("ownerReferences", []))]
    require(len(live) <= 1, "ambiguous new Controller Pod")
    if not live: return None
    return c.get(SYSTEM + "/pods/" + live[0]["metadata"]["name"])


def ready_pod(pod):
    statuses = pod.get("status", {}).get("containerStatuses", [])
    return len(statuses) == 1 and statuses[0].get("ready") is True and statuses[0].get("restartCount") == 0


def approved_controller_image(pod, p):
    containers = pod.get("spec", {}).get("containers", [])
    statuses = pod.get("status", {}).get("containerStatuses", [])
    require(len(containers) == len(statuses) == 1 and containers[0].get("image") == p["controllerImageRef"] and statuses[0].get("imageID") == p["controllerImageID"] and pod["spec"].get("nodeName") == p["controllerNode"], "actual Controller image/architecture binding differs from approved existing image")


def setup(c, p, record, save):
    require("exhibitionPermit" not in p, "repeat execution cannot redeploy Controller or create setup objects")
    identity(c, p); record["demoABefore"] = preserve_demo_a(c, p); save()
    exact_policy(c.get(POLICY), p, 1, p["initialPolicySpec"])
    dep = c.get(DEPLOY); require(dep["metadata"]["uid"] == p["executionScope"]["deploymentUID"] and dep["spec"] == p["sourceDeploymentSpec"] and dep["status"].get("readyReplicas") == 1, "initial app changed")
    old = c.get(CONTROLLER); require(old["metadata"]["uid"] == p["controllerUID"] and old["spec"] == p["controllerSpec"], "Controller spec changed")
    role = c.get("/apis/rbac.authorization.k8s.io/v1/clusterroles/opsloop-controller")
    binding = c.get("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/opsloop-controller-fenced-v1")
    require(binding["roleRef"] == {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "opsloop-controller"} and binding["subjects"] == [{"kind": "ServiceAccount", "name": "opsloop-controller-fenced-v1", "namespace": "opsloop-system"}], "existing Controller reader binding changed")
    for resource, verbs in (("recoveryincidents", {"create", "get", "list", "watch"}), ("recoveryincidents/status", {"get", "update"}), ("recoverypolicies/status", {"get", "update"})):
        require(any("opsloop.io" in r["apiGroups"] and resource in r["resources"] and verbs <= set(r["verbs"]) for r in role["rules"]), "existing Controller permission absent")
    collections = [("configMap", SYSTEM + "/configmaps"), ("role", "/apis/rbac.authorization.k8s.io/v1/namespaces/" + NS + "/roles"), ("roleBinding", "/apis/rbac.authorization.k8s.io/v1/namespaces/" + NS + "/rolebindings")]
    for key, collection in collections:
        require(c.get(collection + "/" + p[key]["metadata"]["name"], absent=True) is None, "new setup object already exists; no adoption")
    for key, collection in collections:
        created = c.request(["create", "--raw", collection, "-f", "-"], p[key])
        require(created["metadata"]["name"] == p[key]["metadata"]["name"] and created["metadata"]["namespace"] == p[key]["metadata"]["namespace"] and created["metadata"].get("uid"), "create response identity mismatch")
        if key == "configMap": require(created.get("immutable") is True and created["data"] == p[key]["data"], "created immutable scope differs")
        record[key] = {"uid": created["metadata"]["uid"], "name": created["metadata"]["name"]}; save()
    old = c.get(CONTROLLER); require(old["metadata"]["uid"] == p["controllerUID"] and old["spec"] == p["controllerSpec"], "Controller drift before CAS")
    updated = c.patch("deployment", "opsloop-controller", "opsloop-system", cas(old) + [{"op": "replace", "path": "/spec/template", "value": p["controllerTemplate"]}])
    expected = {**p["controllerSpec"], "template": p["controllerTemplate"]}
    require(updated["metadata"]["uid"] == p["controllerUID"] and updated["spec"] == expected and updated["metadata"]["generation"] == old["metadata"]["generation"] + 1, "Controller template CAS unconfirmed")
    record["controllerGeneration"] = updated["metadata"]["generation"]; save()
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        current = c.get(CONTROLLER)
        require(current["spec"] == expected and current["metadata"]["uid"] == p["controllerUID"], "new Controller drift")
        if current["status"].get("readyReplicas") == 1 and current["status"].get("observedGeneration") == record["controllerGeneration"]:
            pod = controller_pod(c, p)
            if pod and ready_pod(pod): break
        time.sleep(2)
    else: raise Stop("new Controller did not become Ready; no Runtime activation")
    require(pod["metadata"]["uid"] not in p["priorControllerPodUIDs"], "pre-existing Controller incarnation cannot be adopted")
    approved_controller_image(pod, p)
    log = c.log("opsloop-system", pod["metadata"]["name"], "manager")
    identities = []
    for line in log.splitlines():
        try: item = json.loads(line)
        except ValueError: continue
        if item.get("schema") == "opsloop-controller-execution-identity/v1": identities.append(item)
    require(len(identities) == 1 and identities[0].get("enforceDefault") == "deny-without-fresh-bound-grant" and re.fullmatch("[a-f0-9]{64}", identities[0].get("instance", "")), "new process identity missing")
    again = c.get(SYSTEM + "/pods/" + pod["metadata"]["name"])
    require(again["metadata"]["uid"] == pod["metadata"]["uid"] and ready_pod(again) and again["status"]["containerStatuses"][0]["containerID"] == pod["status"]["containerStatuses"][0]["containerID"], "Controller incarnation changed during log read")
    approved_controller_image(again, p)
    record["authorityController"] = {"podName": pod["metadata"]["name"], "podUID": pod["metadata"]["uid"], "containerID": pod["status"]["containerStatuses"][0]["containerID"], "imageID": pod["status"]["containerStatuses"][0]["imageID"], "instance": identities[0]["instance"], "logSHA256": sha(log.encode()), "scopeConfigMapUID": record["configMap"]["uid"]}
    private_new(ROOT / "verification/runtime-controller-identity.json", {"pod": pod, "identity": identities[0], "log": log})
    exact_policy(c.get(POLICY), p, 1, p["initialPolicySpec"])
    record["demoAAfter"] = preserve_demo_a(c, p)
    record["state"] = "SCOPE_READY_POLICY_DISABLED"; save()


def failed_command(kind, argv, payload, result=None, error=None):
    """Private bounded diagnostics for pinned non-credential Runtime commands."""
    import uuid
    from tools.exhibition_lifecycle import private_dir,write
    directory=private_dir(str(ROOT/'command-diagnostics'/uuid.uuid4().hex))
    refs={}
    for name in ('stdout','stderr'):
        raw=getattr(result if result is not None else error,name,None) or b''
        if isinstance(raw,str):raw=raw.encode()
        write(directory/name,raw[:1048576],exclusive=True);refs[name]={'sha256':sha(raw[:1048576]),'truncated':len(raw)>1048576}
    status=None
    for name in ('stdout','stderr'):
        raw=getattr(result if result is not None else error,name,None) or b''
        try:
            value=json.loads(raw)
            if value.get('kind')=='Status' and value.get('status')=='Failure':
                status={k:value[k] for k in ('kind','status','code','reason','message','details') if k in value}
        except (ValueError,TypeError,AttributeError):pass
    stderr=getattr(result,'stderr',b'') or b''
    text=stderr.decode(errors='replace') if isinstance(stderr,bytes) else stderr
    explicit=status is not None or any(x in text for x in ('Error from server (','The request is invalid: the server rejected'))
    disposition='TRANSPORT_UNCONFIRMED' if error is not None else 'EXPLICIT_SERVER_REJECTION' if explicit else 'PROCESS_ERROR_UNCLASSIFIED'
    receipt={'at':stamp(),'kind':kind,'argvSHA256':sha(canonical(list(argv))),
        'inputSHA256':sha(b'' if payload is None else canonical(payload)),'exitCode':getattr(result,'returncode',None),
        'errorType':None if error is None else type(error).__name__,'responseDisposition':disposition,
        'kubernetesStatus':status,'httpStatusObserved':None,**refs}
    raw=canonical(receipt);write(directory/'receipt.json',raw,exclusive=True)
    return {'path':str(directory/'receipt.json'),'sha256':sha(raw),'responseDisposition':disposition,'kubernetesStatus':status}



def heartbeat_state(obj):
    """Only Monitoring Active timestamp is non-authority status metadata.
    Every other status/annotation/spec/identity difference remains significant.
    """
    from tools.exhibition_heartbeat_progress import conditions
    conditions(obj)  # Never silently normalize duplicate condition types.
    value=copy.deepcopy(obj)
    for k in ('resourceVersion','managedFields'):value.get('metadata',{}).pop(k,None)
    for cond in value.get('status',{}).get('conditions',[]):
        if (cond.get('type'),cond.get('status'),cond.get('reason'))==('Monitoring','True','Active'):
            cond.pop('lastTransitionTime',None)
    return value


def heartbeat_patch(old,wire,updated,envelope):
    # Setting the read RV asks apiserver storage for optimistic CAS (409), rather
    # than a generic JSON Patch test failure (422). UID/spec/grant/permit/generation
    # tests remain, and the storage RV condition protects the entire read object.
    ops=cas(old)
    ops[1]={'op':'replace','path':'/metadata/resourceVersion','value':old['metadata']['resourceVersion']}
    ops.append({'op':'test','path':'/metadata/generation','value':old['metadata']['generation']})
    ops.append({'op':'test','path':'/metadata/annotations/opsloop.io~1execution-grant','value':wire['annotation']})
    if envelope is not None:ops.append({'op':'test','path':'/metadata/annotations/opsloop.io~1exhibition-permit','value':canonical(envelope).decode()})
    ops.append({'op':'replace','path':'/metadata/annotations/opsloop.io~1execution-grant','value':updated['annotation']})
    return ops


def renew_heartbeat(c,p,generation,envelope,wire,grant,guard_and_clock,*,revalidate):
    """One durable logical heartbeat; <=2 conditional attempts, no crash replay.
    Only an explicit apiserver RV conflict permits the second attempt.
    """
    from tools.exhibition_lifecycle import private_dir,write
    from tools.exhibition_heartbeat import resource_version_conflict
    require(callable(revalidate),'heartbeat Controller revalidation required')
    def policy_valid(obj,annotation):
        from tools.exhibition_heartbeat_progress import conditions
        conditions(obj)
        exact_policy(obj,p,generation,p['enforcePolicySpec'])
        require(obj['metadata']['name']==POLICY_NAME and obj['metadata']['namespace']==NS,'heartbeat target changed')
        require(obj['spec']['enabled'] is True and obj['spec']['mode']=='Enforce','heartbeat stopped')
        require(obj['metadata']['annotations'].get(GRANT)==annotation,'live grant changed')
        require(not any(x.get('type')=='Quiesced' and x.get('status')=='True' for x in obj.get('status',{}).get('conditions',[])),'heartbeat fence closed')
        if envelope is not None:
            require(obj['metadata']['annotations'].get(EXHIBITION_PERMIT)==canonical(envelope).decode(),'live signed permit changed')
            require(p['initialIncidentSequence']<=obj.get('status',{}).get('incidentSequence',0)<=p['initialIncidentSequence']+1,'per-run Incident budget changed')
    def binding_valid():
        parsed=json.loads(wire['annotation'])
        expected=copy.deepcopy(grant);actual=copy.deepcopy(parsed)
        for field in ('notBefore','notAfter','heartbeatAt'):
            expected[field]=instant(expected[field]).isoformat();actual[field]=instant(actual[field]).isoformat()
        require(expected==actual,'heartbeat source sequence/core changed')
        require(grant['scope']==p['executionScope'] and grant['scope']['runID']==p['runID'] and grant['scope']['epoch']==p['epoch'],'heartbeat run scope changed')
        if envelope is not None:
            env,payload=exhibition_permit(p)
            require(env==envelope and payload['grant']=={k:grant[k] for k in ('version','scope','instance','notBefore','notAfter')},'heartbeat signed binding changed')
        revalidate()
    require(not (ROOT/'runtime-terminal.json').exists(), 'Runtime terminal already latched; renewal forbidden')
    old=c.get(POLICY);policy_valid(old,wire['annotation']);binding_valid()
    progress=None
    if envelope is not None:
        _, signed_payload=exhibition_permit(p)
        if signed_payload.get('schema')=='opsloop-exhibition-permit/v3':
            from tools.exhibition_heartbeat_progress import Progress
            progress=Progress(c,p,signed_payload,wire['binding'],ROOT,envelope)
    receipt=clock(p);observed=instant(receipt['apiServerDate'])
    require(instant(grant['notBefore'])<=observed and observed>=instant(grant['heartbeatAt']) and observed<instant(grant['notAfter']),'grant clock reversed/expired')
    applied_response=False
    progress_first=None
    def inspect_progress(obj, clock_check):
        if progress is None:return None
        proof=progress.terminal(obj)
        if proof is not None:
            binding_valid();clock_check()
            # A second exact Policy read closes revocation/stop races during
            # terminal/source proof. This is a read-only closure, not renewal.
            current=c.get(POLICY);policy_valid(current,obj['metadata']['annotations'][GRANT])
            if current!=obj:
                repeated=progress.terminal(current)
                require(repeated is not None and repeated['incident']==proof['incident']
                        and repeated['replacement']==proof['replacement'], 'terminal changed while proving closure')
                progress.compatible(proof['observation'],repeated['observation'])
                proof=repeated
            binding_valid();clock_check()
            if progress_first is not None:
                closing_observation=copy.deepcopy(proof['observation'])
                # policy_valid already proved either the unchanged grant or the
                # exact applied heartbeat. No other authority/history is masked.
                closing_observation['projection']['metadata']['annotations'][GRANT]=wire['annotation']
                progress.compatible(progress_first,closing_observation)
            private_new(ROOT/'runtime-terminal.json',{'schema':'opsloop-runtime-terminal/v1','binding':p['executionScope'],
                'incidentUID':proof['incident']['metadata']['uid'],'incidentSHA256':sha(canonical(proof['incident'])),
                'policySHA256':sha(canonical(proof['policy'])),'observedAt':stamp(),
                'inFlightHeartbeatApplied':applied_response,'additionalHeartbeatAllowed':False})
            raise TerminalObserved(proof)
        return progress.observe(obj)
    progress_first=inspect_progress(old,lambda:guard_and_clock(receipt))
    if observed==instant(grant['heartbeatAt']):return wire,grant,receipt
    updated_grant={**grant,'heartbeatSequence':grant['heartbeatSequence']+1,'heartbeatAt':observed.isoformat()}
    require(updated_grant['heartbeatSequence']<=301,'grant renewal budget exceeded')
    updated=native(p,'grant',payload=updated_grant);require(updated['binding']==wire['binding'],'fixed grant core changed')
    key=sha(canonical({'run':p['runID'],'epoch':p['epoch'],'binding':wire['binding'],'fromSequence':grant['heartbeatSequence']}))
    parent=private_dir(str(ROOT/'heartbeat-diagnostics'));directory=parent/key
    # Exclusive durable identity: a restarted invocation cannot replenish attempts
    # or replace the original clock, even if dispatch completion was unknown.
    directory.mkdir(mode=0o700,exist_ok=False)
    for folder in (ROOT,parent,directory):
        fd=os.open(folder,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    def durable(path,value):
        write(path,canonical(value),exclusive=True)
        fd=os.open(path.parent.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    context={'runID':p['runID'],'epoch':p['epoch'],'target':POLICY,'at':stamp(),'patchType':'application/json-patch+json',
             'policyUID':old['metadata']['uid'],'generation':generation,'specSHA256':sha(canonical(old['spec'])),
             'grantBinding':wire['binding'],'permitSHA256':None if envelope is None else sha(canonical(envelope)),
             'oldHeartbeatSequence':grant['heartbeatSequence'],'newHeartbeatSequence':updated_grant['heartbeatSequence'],
             'clock':receipt,'maximumAttempts':2,'fixedNotAfter':grant['notAfter']}
    durable(directory/'context.json',context);durable(directory/'before.json',old)
    def fresh(reserve_seconds=0):
        # Keep the original boot clock and observation; response receipt is never
        # a fresh heartbeat timestamp. Same existing 5s pre-mutation clock bound.
        tick=time.clock_gettime_ns(time.CLOCK_BOOTTIME)
        require(receipt['beforeTick']<=receipt['afterTick']<=tick and tick-receipt['beforeTick']<5_000_000_000,'heartbeat original clock stale')
        projected=observed+dt.timedelta(seconds=(tick-receipt['beforeTick'])/1e9)
        require(projected+dt.timedelta(seconds=reserve_seconds)<min(instant(grant['notAfter']),instant(p['expiresAt'])),'heartbeat deadline insufficient')
        require(tick-receipt['beforeTick']+int(reserve_seconds*1e9)<5_000_000_000,'heartbeat response budget insufficient')
        guard_and_clock(receipt)
    first=copy.deepcopy(old)
    try:
        for attempt in (1,2):
            binding_valid();policy_valid(old,wire['annotation']);fresh()
            payload=heartbeat_patch(old,wire,updated,envelope)
            step=private_dir(str(directory/('attempt-'+str(attempt))))
            durable(step/'patch.json',payload)
            durable(step/'reserved.json',{'attempt':attempt,'inputSHA256':sha(canonical(payload)),'resourceVersion':old['metadata']['resourceVersion']})
            def before_send():
                binding_valid()
                if progress:
                    live=c.get(POLICY);policy_valid(live,wire['annotation'])
                    progress.compatible(progress_first,inspect_progress(live,lambda:fresh(reserve_seconds=2)))
                fresh(reserve_seconds=2)
            response=c.heartbeat_patch(p,payload,step,before_send)
            if response['disposition']=='APPLIED_RESPONSE':
                applied_response=True
                policy_valid(response['object'],updated['annotation']);fresh()
                # Exact current-run readback, without retrying an uncertain mutation.
                current=c.get(POLICY);policy_valid(current,updated['annotation']);binding_valid();fresh()
                if progress:
                    inspect_progress(current,fresh)
                    comparable=copy.deepcopy(current)
                    # policy_valid proved the exact new annotation; only the
                    # already-authorized heartbeat field changes are normalized.
                    comparable['metadata']['annotations'][GRANT]=wire['annotation']
                    progress.compatible(progress_first,progress.observe(comparable));fresh()
                durable(directory/'after.json',current);durable(directory/'outcome.json',{'state':'CONFIRMED','attempts':attempt,'clock':receipt})
                return updated,updated_grant,receipt
            if attempt==2 or not resource_version_conflict(response,POLICY_NAME):
                # A GET may aid diagnosis, but cannot convert a lost response into
                # success or authorize another request.
                if response['disposition']=='APPLY_UNKNOWN':
                    try:durable(step/'uncertain-readback.json',c.get(POLICY))
                    except Exception:pass
                raise Stop('heartbeat rejected or apply unknown; no retry',{'path':str(step/'response.json'),'responseDisposition':response['disposition']})
            latest=c.get(POLICY)
            durable(step/'conflict-readback.json',latest)
            policy_valid(latest,wire['annotation']);binding_valid();fresh()
            require(latest['metadata']['resourceVersion']!=old['metadata']['resourceVersion'],'heartbeat conflict without changed RV')
            if progress:
                progress.compatible(progress_first,inspect_progress(latest,fresh));fresh()
            else:
                require(heartbeat_state(latest)==heartbeat_state(first),'heartbeat safety state changed during conflict')
            old=latest
    except TerminalObserved:
        durable(directory/'outcome.json',{'state':'TERMINAL_STOP_ONLY','attemptsReserved':attempt,
            'inFlightHeartbeatApplied':applied_response,'additionalHeartbeatAllowed':False})
        raise
    except Exception as exc:
        durable(directory/'outcome.json',{'state':'FAILED_NO_REPLAY','errorType':type(exc).__name__,'diagnostic':getattr(exc,'diagnostic',None)})
        raise


def initial_fault_pod(listed,p):
    # A fixed Policy baseline may retain an older attempt marker. Only this
    # round's exact pre-action marker is accepted; later Runtime pods differ.
    expected=p['faultTemplate'].get('metadata',{}).get('annotations',{}).get('opsloop.io/attempt-id')
    return listed.get('metadata',{}).get('annotations',{}).get('opsloop.io/attempt-id')==expected


def native(p, key, argv=(), payload=None, timeout=4, allow_failure=False):
    path = Path(p["helpers"][key]["path"])
    require(sha(path.read_bytes()) == p["helpers"][key]["sha256"], "native helper changed")
    try: r = subprocess.run([str(path), *argv], input=None if payload is None else canonical(payload), capture_output=True, env=env(), timeout=timeout)
    except (OSError, subprocess.TimeoutExpired) as exc:
        failed_command(key,argv,payload,error=exc)
        raise Stop("native " + key + " operation unconfirmed") from None
    require(len(r.stdout) < 1 << 20, "native response oversized")
    if r.returncode:
        failed_command(key,argv,payload,result=r)
        if allow_failure: return {"exit_code": r.returncode, "stdoutSHA256": sha(r.stdout), "stderrSHA256": sha(r.stderr)}
        raise Stop("native " + key + " operation rejected; no replay")
    return json.loads(r.stdout)


def bind_controller(c, p, setup_record, *, current_authority=True):
    expected = setup_record["authorityController"]
    dep = c.get(CONTROLLER)
    require(dep["metadata"]["uid"] == p["controllerUID"] and dep["metadata"]["generation"] == setup_record["controllerGeneration"] and dep["spec"]["template"] == p["controllerTemplate"] and dep["status"].get("readyReplicas") == 1, "approved Controller Deployment changed")
    pod = c.get(SYSTEM + "/pods/" + expected["podName"])
    approved_controller_image(pod, p)
    require(pod["metadata"]["uid"] == expected["podUID"] and ready_pod(pod) and pod["status"]["containerStatuses"][0]["containerID"] == expected["containerID"] and pod["status"]["containerStatuses"][0]["imageID"] == expected["imageID"], "approved Controller instance changed; no adoption")
    cm = c.get(SYSTEM + "/configmaps/" + p["configMap"]["metadata"]["name"])
    require(cm["metadata"]["uid"] == expected["scopeConfigMapUID"] and cm.get("immutable") is True and cm["data"] == p["configMap"]["data"], "approved Controller scope changed")
    if "exhibitionPermit" in p:
        exhibition_permit(p, setup_record, current=current_authority)
        key = p.get("staticAuthorityKey")
        require(isinstance(key, str) and key in cm["data"], "static authority configuration missing")
        cfg = json.loads(cm["data"][key])
        require(cfg.get("schema") == "opsloop-exhibition-authority/v1" and cfg.get("publicKey") == p["exhibitionPermit"]["publicKey"], "static authority key changed")
        target = {k: p["executionScope"][k] for k in ("clusterUID", "namespaceUID", "namespace", "policy", "policyUID", "deployment", "deploymentUID")}
        require(target in cfg.get("targets", []), "fixed workload outside static Controller scope")


def clock(p):
    value = native(p, "grant", ["--read-server-clock"])
    tick = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
    require(value["origin"] == p["apiOrigin"] and value["clusterUID"] == p["clusterUID"] and value["hostBoot"] == Path("/proc/sys/kernel/random/boot_id").read_text().strip(), "clock origin/boot mismatch")
    require(value["beforeTick"] <= value["afterTick"] <= tick and tick - value["beforeTick"] < 3_000_000_000, "clock receipt stale")
    return value


def fault_patch(dep, p):
    require(dep["metadata"]["uid"] == p["executionScope"]["deploymentUID"] and dep["spec"] == p["sourceDeploymentSpec"], "fault source spec changed")
    return cas(dep) + [{"op": "replace", "path": "/spec/template", "value": p["faultTemplate"]}]


def disable_prearm(c, p):
    current = c.get(POLICY)
    initial, observed, _, _ = policy_generations(p)
    if current["metadata"].get("generation") == initial and current.get("spec") == p["initialPolicySpec"]:
        initial_policy(current, p)
        return
    exact_policy(current, p, observed, p["observePolicySpec"])
    c.patch("recoverypolicy", POLICY_NAME, NS, cas(current) + [{"op": "replace", "path": "/spec/enabled", "value": False}])


def finish_guard(p, guard_args, watcher, record, save):
    # A lost finish response must not prevent read-only reconciliation. Never
    # resend finish here; the existing Guard owns its durable disable-only intent.
    boot=Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    if 'stopWait' not in record:
        seconds=45.0
        if p.get('expiresAt'):seconds=min(seconds,max(0.0,(instant(p['expiresAt'])-now()).total_seconds()))
        record['stopWait']={'hostBoot':boot,'deadlineMonotonic':time.monotonic()+seconds,
                           'deadlineAt':(now()+dt.timedelta(seconds=seconds)).isoformat()};save()
    window=record['stopWait']
    require(window.get('hostBoot')==boot,'strong stop wait boot changed; no deadline reset')
    deadline=min(window['deadlineMonotonic'],time.monotonic()+max(0.0,(instant(window['deadlineAt'])-now()).total_seconds()))
    if not record.get("finishDispatched"):
        record["finishDispatched"] = True; save()
        try:
            record["finishAttempt"] = native(p, "guard", ["finish-workload", *guard_args], timeout=20, allow_failure=True)
        except Exception as exc:
            record["finishAttempt"] = {"state": "RESPONSE_UNCONFIRMED", "errorClass": type(exc).__name__, "resent": False}
        save()
    while time.monotonic() < deadline:
        try:
            status = native(p, "guard", ["guard-workload", "status", *guard_args], timeout=4, allow_failure=True)
        except Exception as exc:
            record["lastGuardStatusError"] = type(exc).__name__; status = {}
        if status.get('state',{}).get('phase'):
            require(status.get('runID')==p['runID'] and status['state'].get('binding')==record['guardBinding'],
                    'Guard stop observation binding changed')
        if status.get("state", {}).get("phase") == "STOP_CONFIRMED":
            require(status.get('runID')==p['runID'] and status['state'].get('binding')==record['guardBinding'],
                    'strong stop binding changed')
            record["stopReceipt"] = status; save(); return
        if status.get("state", {}).get("phase") == "MANUAL_REQUIRED": break
        if watcher is None or watcher.poll() is not None: break
        time.sleep(0.5)
    record["strongStop"] = "UNCONFIRMED"; save()
    raise Stop("strong stop unconfirmed; original Guard retained, manual inspection required")


def capture_terminal(c,p,setup_record,record,save,terminal,raw,before,*,stop,started,read_deadline):
    """Stop the owned Policy once, then collect read-only evidence before outer baseline restore.

    Guard's existing stop authorization is get-and-disable-owned-policy-only;
    it does not restore the Deployment. The outer lifecycle owns that restoration.
    The pre-stop Policy snapshot proves the action's Enforce authority; current
    reads prove the same terminal survives strong stop. Nothing renews its grant.
    """
    require(before.get('pod') is not None and before.get('probe') is not None
            and before.get('deployment') is not None and before.get('log') is not None,
            'initial failure evidence missing before terminal capture')
    inc,pol=terminal['incident'],terminal['policy']
    require(not record.get('captureManifestSHA256'), 'terminal capture already consumed')
    remember_incident(inc,p,inc['metadata']['annotations']['opsloop.io/execution-grant-binding'],record,save)
    _,_,generation,stopped_generation=policy_generations(p)
    exact_policy(pol,p,generation,p['enforcePolicySpec'])
    signed_envelope,payload=exhibition_permit(p,setup_record,current=False)
    require(signed_envelope is not None and payload['schema']=='opsloop-exhibition-permit/v3','terminal capture requires v3 proof')
    require(pol['metadata']['annotations'].get(EXHIBITION_PERMIT)==canonical(signed_envelope).decode(),'terminal permit changed')
    rt=inc['status']['runtime'];attempt=rt['attempts'][0]
    from tools.exhibition_terminal import verify_sources
    from tools.exhibition_heartbeat_progress import conditions, _json
    source=_json(inc['metadata']['annotations']['opsloop.io/current-exhibition-occurrence'])
    expected_template=copy.deepcopy(payload['approvedReproduction']['template'])
    if isinstance(expected_template,str):expected_template=_json(expected_template)
    expected_template.setdefault('metadata',{}).setdefault('annotations',{})['opsloop.io/attempt-id']=attempt['actionID']
    expected_template['spec'].setdefault('nodeSelector',{})['kubernetes.io/hostname']=attempt['hostname']
    expected_deployment={**p['sourceDeploymentSpec'],'template':expected_template}
    require(rt['phase']=='FAILED' and rt['reason']=='RUNTIME_BUDGET_EXHAUSTED'
            and len(rt['attempts'])==1 and attempt['outcome']=='Failed','terminal capture outcome changed')
    def scope_check():
        require(now()<read_deadline and now()<instant(p['expiresAt']),'terminal read-only capture deadline reached')
        bind_controller(c,p,setup_record,current_authority=False)
        live=c.get(POLICY)
        exact_policy(live,p,stopped_generation,{**p['enforcePolicySpec'],'enabled':False})
        require(live['metadata']['annotations'].get(GRANT)==pol['metadata']['annotations'].get(GRANT)
                and live['metadata']['annotations'].get(EXHIBITION_PERMIT)==canonical(signed_envelope).decode(),
                'terminal stop grant/permit changed')
        conditions(live)
        require(not live.get('status',{}).get('activeIncident') and
                any(x.get('type')=='Quiesced' and x.get('status')=='True' and x.get('reason')=='FenceClosed'
                    and x.get('observedGeneration')==stopped_generation for x in live.get('status',{}).get('conditions',[])),
                'terminal stop fence unconfirmed')
        current=c.get('/apis/opsloop.io/v1alpha1/namespaces/'+NS+'/recoveryincidents/'+p['executionScope']['incident'])
        remember_incident(current,p,inc['metadata']['annotations']['opsloop.io/execution-grant-binding'],record,save)
        require(current['spec']==inc['spec'] and current.get('status',{}).get('runtime')==rt,
                'terminal Incident changed during stop/capture')
        dep=c.get(DEPLOY)
        require(dep['metadata'].get('uid')==p['executionScope']['deploymentUID']
                and not dep['metadata'].get('deletionTimestamp') and dep['spec']==expected_deployment,
                'terminal target changed during stop/capture')
        replacement=verify_sources(require,c,p['executionScope'],payload['approvedReproduction'],source,attempt)
        require(replacement==terminal['replacement'],'terminal runtime custody changed during stop/capture')
    stop()
    stop_receipt=record.get('stopReceipt',{})
    require(stop_receipt.get('runID')==p['runID'] and stop_receipt.get('state',{}).get('binding')==record.get('guardBinding')
            and stop_receipt.get('state',{}).get('phase')=='STOP_CONFIRMED','terminal strong stop binding unconfirmed')
    scope_check()
    after_pod=c.get(CORE+'/pods/'+attempt['podName'])
    require(after_pod['metadata']['uid']==attempt['podUID'],'replacement Pod identity changed')
    st=after_pod.get('status',{}).get('containerStatuses',[])
    expected=terminal['replacement']
    require(len(st)==1 and st[0].get('containerID')==expected['containerID'] and st[0].get('imageID')==expected['imageID']
            and st[0].get('restartCount',0)==expected['restartCount'],'terminal replacement runtime changed during capture')
    after_probe=native(p,'quoteProbe',[attempt['podName'],attempt['podUID']],timeout=6);after_probe['phase']='after_runtime'
    after_log={'pod_uid':attempt['podUID'],'text':c.log(NS,attempt['podName'],payload['approvedReproduction']['containerName'])}
    scope_check();after=c.get(DEPLOY);rss=[]
    for pod in (before['pod'],after_pod):
        owners=[x for x in pod['metadata'].get('ownerReferences',[]) if x.get('controller') and x.get('kind')=='ReplicaSet']
        require(len(owners)==1,'application ReplicaSet owner ambiguous')
        rs=c.get('/apis/apps/v1/namespaces/'+NS+'/replicasets/'+owners[0]['name'])
        require(rs['metadata']['uid']==owners[0]['uid'],'ReplicaSet identity changed')
        rss.append(rs)
    scope_check();events=c.listing(CORE+'/events')
    values={'incident.json':inc,'policy.json':pol,'deployment-after.json':after,'pod-after.json':after_pod,
            'replicasets.json':{'apiVersion':'apps/v1','kind':'ReplicaSetList','items':rss},'events.json':events,
            'observations.json':[before['probe'],after_probe],'logs.json':[before['log'],after_log]}
    for name,value in values.items():private_new(raw/name,value)
    authority={'schema_version':'opsloop-engineering-authorization/v1','engineering_enabled':True,'profile_id':'demo-b-discount-v1',
        'issued_at':started.isoformat(),'expires_at':min(now()+dt.timedelta(hours=2),instant(p['expiresAt'])).isoformat(),'max_runtime_attempts':1,
        'allowed_paths':['apps/engineering-demo/discount.go','apps/engineering-demo/discount_test.go'],
        'run_id':p['runID'],'epoch':p['epoch'],'cluster_uid':p['clusterUID'],'api_origin':p['apiOrigin'],'namespace':NS,
        'incident_uid':inc['metadata']['uid'],'policy_uid':p['executionScope']['policyUID'],'policy_name':POLICY_NAME,
        'deployment_uid':p['executionScope']['deploymentUID'],'deployment_name':APP,'approval_reference':record['planSHA256']}
    prov=json.loads((ROOT/'verification/image-provenance.json').read_bytes());authority.update({k:prov[k] for k in ('repository','base_commit','image_digest')})
    record['engineeringAuthorizationSHA256']=private_new(raw/'engineering-authorization.json',authority)
    captured_at=stamp()
    manifest={'schema_version':'opsloop-engineering-capture/v1','evidence_kind':'observed','captured_at':captured_at,
        'run_id':p['runID'],'epoch':p['epoch'],'cluster_uid':p['clusterUID'],'api_origin':p['apiOrigin'],
        'observation_time_semantics':'private evidence-store receipt; source event and HTTP observation timestamps preserved inside files',
        'files':{name:{'sha256':sha((raw/name).read_bytes()),'evidence_kind':'observed','observed_at':dt.datetime.fromtimestamp((raw/name).stat().st_mtime,dt.timezone.utc).isoformat().replace('+00:00','Z')} for name in FILES}}
    scope_check()
    record['captureManifestSHA256']=private_new(raw/'capture-manifest.json',manifest)
    record['state']='ACTUAL_RUNTIME_EXHAUSTED_EVIDENCE_CAPTURED';save()


def trial(c, p, setup_record, record, save):
    initial_generation, observe_generation, enforce_generation, stopped_generation = policy_generations(p)
    signed_envelope, signed_payload = exhibition_permit(p, setup_record)
    require(not (ROOT / "incident-identity.json").exists() and not (ROOT / "incident-identity.json").is_symlink(), "existing Incident custody cannot restart trial")
    record["binding"] = {key: p["executionScope"][key] for key in ("runID", "epoch", "policyUID", "deploymentUID")}
    identity(c, p); record["demoABefore"] = preserve_demo_a(c, p); bind_controller(c, p, setup_record); save()
    pol = c.get(POLICY); initial_policy(pol, p)
    dep = c.get(DEPLOY); require(dep["spec"] == p["sourceDeploymentSpec"] and dep["status"].get("readyReplicas") == 1, "initial app not healthy")
    service = c.get(CORE + "/services/" + APP); require(service["metadata"]["uid"] == p["serviceUID"] and service["spec"] == p["serviceSpec"], "Service changed")
    root = ROOT / "runtime-state"; root.mkdir(mode=0o700)
    raw = ROOT / "incident-raw"; raw.mkdir(mode=0o700)
    guard_args = None; watcher = None; watch_log = None; enforce_intent = False; captured = False
    try:
        pol = c.patch("recoverypolicy", POLICY_NAME, NS, cas(pol) + [{"op": "replace", "path": "/spec", "value": p["observePolicySpec"]}])
        exact_policy(pol, p, observe_generation, p["observePolicySpec"])
        owner = native(p, "guard", ["guard", "owner", "-pid", str(os.getpid())])
        started = now()
        hard = instant(signed_payload["grant"]["notAfter"]) if signed_payload else started + dt.timedelta(seconds=600)
        require(started < hard <= started + dt.timedelta(seconds=600), "fixed Runtime deadline invalid")
        expires = hard + dt.timedelta(seconds=60)
        auth = {"version": "opsloop-workload-authorization/v1", "approvalReference": "DemoB-runtime-plan-sha256:" + record["planSHA256"], "runID": p["runID"], "epoch": p["epoch"], "apiOrigin": p["apiOrigin"], "clusterUID": p["clusterUID"], "namespace": NS, "namespaceUID": p["executionScope"]["namespaceUID"], "policy": POLICY_NAME, "policyUID": p["executionScope"]["policyUID"], "policyGeneration": enforce_generation, "policySpecSHA256": sha(canonical(p["enforcePolicySpec"])), "workload": APP, "deploymentUID": p["executionScope"]["deploymentUID"], "maxTrialMs": 600000, "notAfter": (expires + dt.timedelta(seconds=1)).isoformat(), "allowed": "get-and-disable-owned-policy-only"}
        auth_path = root / "stop-authorization.json"; auth_sha = private_new(auth_path, auth)
        journal = root / "guard"; journal.mkdir(mode=0o700)
        config = {"version": "opsloop-workload-safety/v1", "mode": "hybrid-live", "evidenceKind": "observed", "runID": p["runID"], "epoch": p["epoch"], "apiOrigin": p["apiOrigin"], "kubeconfigFile": c.d.CONFIG, "clusterUID": p["clusterUID"], "namespace": NS, "namespaceUID": p["executionScope"]["namespaceUID"], "policy": POLICY_NAME, "policyUID": p["executionScope"]["policyUID"], "policyGeneration": enforce_generation, "policySpec": p["enforcePolicySpec"], "workload": APP, "deploymentUID": p["executionScope"]["deploymentUID"], "root": str(journal), "owner": owner, "hardDeadline": hard.isoformat(), "authorityExpires": expires.isoformat(), "maxTrialMs": 600000, "heartbeatMs": 10000, "timeoutMs": 2000, "authorizationFile": str(auth_path), "authorizationSHA256": auth_sha}
        config_path = root / "guard-config.json"; private_new(config_path, config)
        binding = native(p, "guard", ["guard-workload", "plan", "-config", str(config_path)])["configBinding"]
        guard_args = ["-config", str(config_path), "-execute", "-confirm", binding]
        record.update(guardBinding=binding, guardOwner=owner, guardConfig=str(config_path), hardDeadline=hard.isoformat(), guardArmIntent=True); save()
        native(p, "guard", ["guard-workload", "arm", *guard_args], timeout=20)
        watch_log = (root / "guard-watch.log").open("xb")
        watcher = subprocess.Popen([p["helpers"]["guard"]["path"], "guard-workload", "watch", *guard_args], stdin=subprocess.DEVNULL, stdout=watch_log, stderr=watch_log, env=env(), start_new_session=True)

        def permit():
            require(watcher.poll() is None, "independent Guard exited")
            value = native(p, "guard", ["guard-workload", "status", *guard_args], timeout=4)
            require(value.get("safetyPermitOnlyNotAuthorization") is True and value["state"]["phase"] == "ARMED", "fresh independent safety permit absent")
            return value

        arm_deadline = time.monotonic() + 8
        while True:
            value = native(p, "guard", ["guard-workload", "status", *guard_args])
            if value.get("safetyPermitOnlyNotAuthorization"): break
            require(watcher.poll() is None and time.monotonic() < arm_deadline, "Guard did not become independently watched")
            time.sleep(0.1)
        observed = clock(p); status = permit()
        def guard_and_clock(receipt):
            permitted = permit()
            tick = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
            require(receipt["hostBoot"] == owner["boot"] and receipt["beforeTick"] <= receipt["afterTick"] <= tick and tick - receipt["beforeTick"] < 5_000_000_000, "fresh grant clock custody absent immediately before mutation")
            return permitted
        remaining = (status["state"]["hardTick"] - observed["afterTick"]) // 1_000_000_000 - 3
        require(360 <= remaining <= 600, "Guard authority insufficient")
        at = instant(observed["apiServerDate"])
        grant = granted_at(p, setup_record, at, remaining, signed_payload)
        wire = native(p, "grant", payload=grant)
        record["executionGrant"] = {**wire, "grant": json.loads(wire["annotation"]), "initialClock": observed}; save()
        pol = c.get(POLICY); exact_policy(pol, p, observe_generation, p["observePolicySpec"])
        annotations = pol["metadata"].get("annotations", {})
        require(GRANT not in annotations and (signed_envelope is None or EXHIBITION_PERMIT not in annotations), "existing grant cannot be adopted")
        issued_annotations = {**annotations, GRANT: wire["annotation"]}
        if signed_envelope is not None: issued_annotations[EXHIBITION_PERMIT] = canonical(signed_envelope).decode()
        patch = cas(pol) + [{"op": "replace", "path": "/spec/mode", "value": "Enforce"}, {"op": "add", "path": "/metadata/annotations", "value": issued_annotations}]
        enforce_intent = True; record["enforceIntent"] = True; save()
        pol = c.patch("recoverypolicy", POLICY_NAME, NS, patch, permit=lambda: guard_and_clock(observed)); exact_policy(pol, p, enforce_generation, p["enforcePolicySpec"])
        require(pol["metadata"]["annotations"][GRANT] == wire["annotation"], "Enforce grant response differs")
        record["state"] = "ENFORCE_GUARD_SUPERVISED"; save()
        next_beat = next_grant = 0; sequence = 0; last_clock = observed; terminal = None

        def pulse():
            nonlocal next_beat, next_grant, sequence, wire, grant, last_clock, terminal
            if terminal is not None:return
            require(now() < hard and time.monotonic() < trial_deadline, "fixed trial deadline reached")
            permit()
            if time.monotonic() >= next_beat:
                sequence += 1; native(p, "guard", ["guard-workload", "heartbeat", *guard_args, "-sequence", str(sequence)])
                next_beat = time.monotonic() + 2
            if time.monotonic() >= next_grant:
                try:
                    wire, grant, receipt = renew_heartbeat(c,p,enforce_generation,signed_envelope,wire,grant,guard_and_clock,revalidate=lambda:bind_controller(c,p,setup_record))
                except TerminalObserved as transition:
                    terminal=transition.proof
                    record['state']='RUNTIME_TERMINAL_STOP_ONLY';record['terminalIncidentUID']=terminal['incident']['metadata']['uid'];save()
                    return
                if wire['annotation'] != record['executionGrant']['annotation']:
                    record['grantHeartbeat']={'sequence':grant['heartbeatSequence'],'clock':receipt,'binding':wire['binding']}; save()
                last_clock = receipt; next_grant = time.monotonic() + 2

        trial_deadline = time.monotonic() + max(1, (hard - now()).total_seconds())
        healthy_deadline = time.monotonic() + 45
        while True:
            pulse(); pol = c.get(POLICY)
            if any(x["type"] == "ReadinessObserved" and x["status"] == "True" and x.get("observedGeneration") == enforce_generation for x in pol.get("status", {}).get("conditions", [])): break
            require(time.monotonic() < healthy_deadline, "Controller did not record healthy readiness before fault")
            time.sleep(0.5)
        bind_controller(c, p, setup_record); pulse()
        dep = c.get(DEPLOY); patch = fault_patch(dep, p)
        record["faultIntent"] = {"at": stamp(), "deploymentUID": dep["metadata"]["uid"], "fromRV": dep["metadata"]["resourceVersion"], "changedField": "spec.template.spec.containers[app].env[DEMO_B_REPLAY]"}; save()
        faulted = c.patch("deployment", APP, NS, patch, permit=lambda: guard_and_clock(last_clock))
        require(faulted["metadata"]["uid"] == p["executionScope"]["deploymentUID"] and faulted["spec"]["template"] == p["faultTemplate"], "fault CAS unconfirmed")
        record["faultResponse"] = {"at": stamp(), "generation": faulted["metadata"]["generation"], "resourceVersion": faulted["metadata"]["resourceVersion"]}; save()
        saw_ready = set(); before = before_pod = before_probe = before_log = None
        incident_path = "/apis/opsloop.io/v1alpha1/namespaces/" + NS + "/recoveryincidents/" + p["executionScope"]["incident"]
        while True:
            pulse()
            if terminal is not None:
                capture_terminal(c,p,setup_record,record,save,terminal,raw,
                    {'deployment':before,'pod':before_pod,'probe':before_probe,'log':before_log},
                    stop=lambda:finish_guard(p,guard_args,watcher,record,save),started=started,
                    read_deadline=min(expires,instant(p['expiresAt'])))
                captured=True
                break
            inc = c.get(incident_path, absent=True)
            if inc is not None: remember_incident(inc, p, wire["binding"], record, save)
            runtime = {} if inc is None else inc.get("status", {}).get("runtime", {})
            if before_pod is None:
                pods = c.listing(CORE + "/pods")["items"]
                for listed in pods:
                    if listed["metadata"].get("deletionTimestamp"): continue
                    containers = listed.get("spec", {}).get("containers", [])
                    if len(containers) != 1 or not any(e.get("name") == "DEMO_B_REPLAY" and e.get("value") == "empty-order-after-30s" for e in containers[0].get("env", [])): continue
                    if not initial_fault_pod(listed,p): continue
                    pod = c.get(CORE + "/pods/" + listed["metadata"]["name"])
                    if ready_pod(pod): saw_ready.add(pod["metadata"]["uid"])
                    if pod["metadata"]["uid"] in saw_ready and not ready_pod(pod):
                        require(not any(a.get("patchedAt") for a in runtime.get("attempts", [])), "initial failure evidence was missed before Runtime action")
                        before = c.get(DEPLOY); require(before["spec"]["template"] == p["faultTemplate"], "runtime already changed initial template")
                        before_pod = pod
                        before_probe = native(p, "quoteProbe", [pod["metadata"]["name"], pod["metadata"]["uid"]], timeout=6); before_probe["phase"] = "before_runtime"
                        before_log = {"pod_uid": pod["metadata"]["uid"], "text": c.log(NS, pod["metadata"]["name"], "app")}
                        private_new(raw / "deployment-before.json", before); private_new(raw / "pod-before.json", before_pod)
                        private_new(raw / "initial-observation.json", before_probe); private_new(raw / "initial-log.json", before_log)
                        record["initialFailureObserved"] = {"podUID": pod["metadata"]["uid"], "at": before_probe["observed_at"]}; save()
                        break
            if signed_payload is not None and signed_payload.get('schema')=='opsloop-exhibition-permit/v3' and runtime.get('phase') in ('RECOVERED','FAILED','ABORTED','SUPERSEDED','MANUAL_REQUIRED','UNSUPPORTED_WORKLOAD'):
                # Go back through the same terminal proof; do not enter the
                # historical legacy capture path or dispatch another heartbeat.
                next_grant=0
                continue
            if runtime.get("phase") in ("RECOVERED", "FAILED", "ABORTED", "SUPERSEDED", "MANUAL_REQUIRED", "UNSUPPORTED_WORKLOAD"):
                require(runtime.get("phase") == "FAILED" and runtime.get("reason") == "RUNTIME_BUDGET_EXHAUSTED", "Runtime terminal outcome is not exhausted application failure")
                attempts = runtime.get("attempts", [])
                require(before_pod is not None and len(attempts) == 1 and attempts[0].get("outcome") == "Failed" and attempts[0].get("patchedAt") and attempts[0].get("completedAt"), "actual patched failed attempt evidence missing")
                attempt = attempts[0]
                require(attempt.get("podUID") and attempt["podUID"] != before_pod["metadata"]["uid"], "Runtime replacement Pod missing")
                pulse()
                after_pod = c.get(CORE + "/pods/" + attempt["podName"])
                require(after_pod["metadata"]["uid"] == attempt["podUID"], "replacement Pod identity changed")
                after_probe = native(p, "quoteProbe", [attempt["podName"], attempt["podUID"]], timeout=6); after_probe["phase"] = "after_runtime"
                after_log = {"pod_uid": attempt["podUID"], "text": c.log(NS, attempt["podName"], "app")}
                pulse(); after = c.get(DEPLOY); pol = c.get(POLICY); exact_policy(pol, p, enforce_generation, p["enforcePolicySpec"])
                rss = []
                for pod in (before_pod, after_pod):
                    owners = [x for x in pod["metadata"].get("ownerReferences", []) if x.get("controller") and x.get("kind") == "ReplicaSet"]
                    require(len(owners) == 1, "application ReplicaSet owner ambiguous")
                    rs = c.get("/apis/apps/v1/namespaces/" + NS + "/replicasets/" + owners[0]["name"])
                    require(rs["metadata"]["uid"] == owners[0]["uid"], "ReplicaSet identity changed")
                    rss.append(rs)
                pulse(); events = c.listing(CORE + "/events")
                values = {"incident.json": inc, "policy.json": pol, "deployment-after.json": after, "pod-after.json": after_pod,
                          "replicasets.json": {"apiVersion": "apps/v1", "kind": "ReplicaSetList", "items": rss}, "events.json": events,
                          "observations.json": [before_probe, after_probe], "logs.json": [before_log, after_log]}
                for name, value in values.items(): private_new(raw / name, value)
                authority = {"schema_version": "opsloop-engineering-authorization/v1", "engineering_enabled": True, "profile_id": "demo-b-discount-v1", "issued_at": started.isoformat(), "expires_at": min(now() + dt.timedelta(hours=2), instant(p["expiresAt"])).isoformat(), "max_runtime_attempts": 1,
                             "allowed_paths": ["apps/engineering-demo/discount.go", "apps/engineering-demo/discount_test.go"], "run_id": p["runID"], "epoch": p["epoch"], "cluster_uid": p["clusterUID"], "api_origin": p["apiOrigin"], "namespace": NS,
                             "incident_uid": inc["metadata"]["uid"], "policy_uid": p["executionScope"]["policyUID"], "policy_name": POLICY_NAME, "deployment_uid": p["executionScope"]["deploymentUID"], "deployment_name": APP,
                             "approval_reference": record["planSHA256"]}
                prov = json.loads((ROOT / "verification/image-provenance.json").read_bytes())
                authority.update({k: prov[k] for k in ("repository", "base_commit", "image_digest")})
                record["engineeringAuthorizationSHA256"] = private_new(raw / "engineering-authorization.json", authority)
                captured_at = stamp()
                manifest = {"schema_version": "opsloop-engineering-capture/v1", "evidence_kind": "observed", "captured_at": captured_at,
                            "run_id": p["runID"], "epoch": p["epoch"], "cluster_uid": p["clusterUID"], "api_origin": p["apiOrigin"],
                            "observation_time_semantics": "private evidence-store receipt; source event and HTTP observation timestamps preserved inside files",
                            "files": {name: {"sha256": sha((raw / name).read_bytes()), "evidence_kind": "observed", "observed_at": dt.datetime.fromtimestamp((raw / name).stat().st_mtime, dt.timezone.utc).isoformat().replace("+00:00", "Z")} for name in FILES}}
                record["captureManifestSHA256"] = private_new(raw / "capture-manifest.json", manifest)
                record["incidentUID"] = inc["metadata"]["uid"]; record["state"] = "ACTUAL_RUNTIME_EXHAUSTED_EVIDENCE_CAPTURED"; save(); captured = True
                break
            time.sleep(0.5)
    finally:
        # Stopping is independent of whether capture or trial logic succeeded.
        try:
            if guard_args:
                if not record.get("stopReceipt"):
                    finish_guard(p, guard_args, watcher, record, save)
                if watcher:
                    try: watcher.wait(timeout=3)
                    except subprocess.TimeoutExpired: pass
            elif not enforce_intent:
                disable_prearm(c, p)
        finally:
            if watch_log: watch_log.close()
            save()
    require(captured, "no complete real Runtime failure evidence")
    stopped = c.get(POLICY); exact_policy(stopped, p, stopped_generation, {**p["enforcePolicySpec"], "enabled": False})
    require(not stopped.get("status", {}).get("activeIncident"), "stopped Policy still has active Incident")
    if signed_payload is not None:
        require(stopped.get("status", {}).get("incidentSequence") == p["initialIncidentSequence"] + 1, "cumulative sequence did not advance exactly once")
        require(any(x.get("type") == "Quiesced" and x.get("status") == "True" and x.get("reason") == "FenceClosed" and x.get("observedGeneration") == stopped_generation for x in stopped.get("status", {}).get("conditions", [])), "stopped Policy fence unconfirmed")
    record["policyAfter"] = {"uid": stopped["metadata"]["uid"], "generation": stopped["metadata"]["generation"], "incidentSequence": stopped.get("status", {}).get("incidentSequence", 0)}
    final_incident = c.get("/apis/opsloop.io/v1alpha1/namespaces/" + NS + "/recoveryincidents/" + p["executionScope"]["incident"])
    remember_incident(final_incident, p, wire["binding"], record, save)
    require(final_incident["metadata"]["uid"] == record["incidentUID"], "captured Incident changed after stop")
    record["objects"] = [{"kind": "RecoveryIncident", "name": final_incident["metadata"]["name"], "uid": final_incident["metadata"]["uid"], "resourceVersion": final_incident["metadata"]["resourceVersion"]}]
    record["demoAAfter"] = preserve_demo_a(c, p)
    record["state"] = "ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED"; save()

