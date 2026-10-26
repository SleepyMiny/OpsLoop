package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type readinessRT func(*http.Request) (*http.Response, error)

func (r readinessRT) RoundTrip(q *http.Request) (*http.Response, error) { return r(q) }
func readinessResponse(q *http.Request, status int, obj any) *http.Response {
	b, _ := json.Marshal(obj)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b)), Request: q}
}

type readinessFixture struct {
	t                  *testing.T
	o                  *observer
	clock              *observerTestClock
	output             bytes.Buffer
	policy             v1.RecoveryPolicy
	deployment         apps.Deployment
	source             core.Pod
	rs                 apps.ReplicaSet
	nodes              map[string]core.Node
	eps                discovery.EndpointSliceList
	serviceStatus      int
	endpointStatus     int
	previewError       bool
	podListChange      func(*core.PodList)
	serviceUID         string
	servicePodUID      string
	promCalls          int
	endpointCalls      int
	incident           *v1.RecoveryIncident
	target             *core.Pod
	targetRS           *apps.ReplicaSet
	sourceMissing      bool
	primaryFailure     string
	primaryFailureKind string
	paths              []string
}

func newReadinessFixture(t *testing.T) *readinessFixture {
	t.Helper()
	f := &readinessFixture{t: t, nodes: map[string]core.Node{}, serviceStatus: 200, endpointStatus: 200, serviceUID: "service", servicePodUID: "source-pod"}
	f.clock = &observerTestClock{t: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	ns := "dedicated"
	image := "registry.test/demo-api@sha256:" + strings.Repeat("e", 64)
	c := ObserverConfig{RunID: "synthetic-readiness", Source: "in-memory API/Service/Prometheus doubles; not live infrastructure", EvidenceKind: "synthetic", APIOrigin: "https://api.readiness.test", Namespace: ns, Workload: "demo-api", Policy: "demo-api", Incident: "demo-api-policy-00001", Service: "demo-api", ServiceURL: "http://demo-api.dedicated.svc.cluster.local:8080/", PathID: "clusterip", ExpectNode: "source-node", ExpectVersion: "test-version", ImageRef: image, IntervalMS: 1000, TimeoutMS: 1000, DeadlineMS: 10000, OutputRoot: t.TempDir()}
	c.Readiness = &ReadinessObserverConfig{Mode: "baseline", ClusterUID: "cluster", NamespaceUID: "namespace", PolicyUID: "policy", PolicyGeneration: 1, DeploymentUID: "deployment", ServiceUID: "service", SourcePod: FinishPod{Name: "source", UID: "source-pod"}, SourceNode: ReadinessNode{Name: "source-node", UID: "uid-source-node", PodCIDR: "10.42.2.0/24", Architecture: "amd64"}, Candidates: []ReadinessNode{{Name: "cp", UID: "uid-cp", PodCIDR: "10.42.0.0/24", Architecture: "amd64"}, {Name: "azure", UID: "uid-azure", PodCIDR: "10.42.1.0/24", Architecture: "arm64"}}, Platforms: []ReadinessPlatform{{Architecture: "amd64", ManifestDigest: "sha256:" + strings.Repeat("c", 64), ConfigDigest: "sha256:" + strings.Repeat("a", 64)}, {Architecture: "arm64", ManifestDigest: "sha256:" + strings.Repeat("d", 64), ConfigDigest: "sha256:" + strings.Repeat("b", 64)}}, PrometheusURL: "http://opsloop-mon-kube-prometheu-prometheus.monitoring.svc:9090", Export: true}
	one := int32(1)
	surge, unavailable := intstr.FromInt32(1), intstr.FromInt32(0)
	resources := core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("50m"), core.ResourceMemory: resource.MustParse("64Mi")}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("200m"), core.ResourceMemory: resource.MustParse("128Mi")}}
	podSpec := core.PodSpec{
		NodeSelector:  map[string]string{core.LabelHostname: c.ExpectNode},
		SchedulerName: "default-scheduler",
		Containers: []core.Container{{
			Name: "app", Image: image, Resources: resources,
			Env:            []core.EnvVar{{Name: "PRIVATE_LITERAL", Value: "never-export-this-environment"}},
			ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)}}},
		}},
	}
	f.deployment = apps.Deployment{
		ObjectMeta: meta.ObjectMeta{Name: c.Workload, Namespace: ns, UID: "deployment", ResourceVersion: "10", Generation: 1},
		Spec: apps.DeploymentSpec{
			Replicas: &one, Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": "demo"}},
			Strategy: apps.DeploymentStrategy{Type: apps.RollingUpdateDeploymentStrategyType, RollingUpdate: &apps.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}},
			Template: core.PodTemplateSpec{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{"app": "demo"}}, Spec: podSpec},
		},
	}

	yes := true
	f.rs = apps.ReplicaSet{ObjectMeta: meta.ObjectMeta{Name: "rs", Namespace: ns, UID: "rs", OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: c.Workload, UID: "deployment", Controller: &yes}}}, Spec: apps.ReplicaSetSpec{Template: *f.deployment.Spec.Template.DeepCopy()}}
	f.rs.Spec.Template.Labels[apps.DefaultDeploymentUniqueLabelKey] = "abc"
	f.source = core.Pod{ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: ns, UID: "source-pod", OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs", Controller: &yes}}}, Spec: *f.deployment.Spec.Template.Spec.DeepCopy(), Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.42.2.8", Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}, {Type: core.ContainersReady, Status: core.ConditionTrue}}, ContainerStatuses: []core.ContainerStatus{{Name: "app", Image: "sha256:" + strings.Repeat("a", 64), ImageID: image, ContainerID: "containerd://source", Ready: true, State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}}}
	f.source.Spec.NodeName = c.ExpectNode
	period := meta.Duration{Duration: 30 * time.Second}
	f.policy = v1.RecoveryPolicy{ObjectMeta: meta.ObjectMeta{Name: c.Policy, Namespace: ns, UID: "policy", Generation: 1, ResourceVersion: "20"}, Spec: v1.RecoveryPolicySpec{Enabled: true, Mode: v1.PolicyModeObserveOnly, TargetRef: v1.TargetRef{Name: c.Workload}, CandidateSelector: meta.LabelSelector{MatchLabels: map[string]string{"opsloop.io/recovery-candidate": "true"}}, RequiredPlatforms: []string{"linux/amd64", "linux/arm64"}, Detection: v1.DetectionSpec{PodUnreadyFor: &period, NodeUnreadyFor: meta.Duration{Duration: 20 * time.Second}, RestartThreshold: 3, RestartWindow: meta.Duration{Duration: 120 * time.Second}}, Filter: v1.FilterSpec{MaxCPUUtilizationPercent: 85, MaxMemoryUtilizationPercent: 85, MinimumAvailableMemory: resource.MustParse("512Mi"), StartupMemoryBudget: resource.MustParse("256Mi"), RequestedCapacityReservePercent: 10, MaxSampleAge: meta.Duration{Duration: 90 * time.Second}}, Score: v1.ScoreSpec{CPUWeight: 30, MemoryWeight: 35, RequestsWeight: 35, RecentFailurePenalty: 15, MaxRecentFailurePenalty: 45, RecentFailureWindow: meta.Duration{Duration: 30 * time.Minute}}}, Status: v1.RecoveryPolicyStatus{Conditions: []meta.Condition{{Type: "ReadinessObserved", Status: meta.ConditionTrue, ObservedGeneration: 1, Reason: "HealthyPodObserved", LastTransitionTime: meta.NewTime(f.clock.Now())}}}}
	c.Readiness.PolicyHash = v1.PolicyHash(f.policy.Spec)
	all := append([]ReadinessNode{c.Readiness.SourceNode}, c.Readiness.Candidates...)
	for i, binding := range all {
		cpu, mem := "4", "8Gi"
		if binding.Architecture == "arm64" {
			cpu, mem = "2", "4Gi"
		}
		labels := map[string]string{core.LabelHostname: binding.Name, core.LabelOSStable: "linux", core.LabelArchStable: binding.Architecture}
		if i > 0 {
			labels["opsloop.io/recovery-candidate"] = "true"
		}
		conditions := []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}, {Type: core.NodeMemoryPressure, Status: core.ConditionFalse}, {Type: core.NodeDiskPressure, Status: core.ConditionFalse}, {Type: core.NodePIDPressure, Status: core.ConditionFalse}}
		f.nodes[binding.Name] = core.Node{ObjectMeta: meta.ObjectMeta{Name: binding.Name, UID: types.UID(binding.UID), Labels: labels}, Spec: core.NodeSpec{PodCIDR: binding.PodCIDR}, Status: core.NodeStatus{Conditions: conditions, Addresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: fmt.Sprintf("100.64.0.%d", i+1)}}, Allocatable: core.ResourceList{core.ResourceCPU: resource.MustParse(cpu), core.ResourceMemory: resource.MustParse(mem)}}}
	}
	ready := true
	port := int32(8080)
	protocol := core.ProtocolTCP
	f.eps = discovery.EndpointSliceList{Items: []discovery.EndpointSlice{{ObjectMeta: meta.ObjectMeta{Name: "eps", Namespace: ns, UID: "eps", Labels: map[string]string{discovery.LabelServiceName: c.Service}, OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: c.Service, UID: "service"}}}, Ports: []discovery.EndpointPort{{Port: &port, Protocol: &protocol}}, Endpoints: []discovery.Endpoint{{Addresses: []string{f.source.Status.PodIP}, TargetRef: &core.ObjectReference{Kind: "Pod", Namespace: ns, Name: f.source.Name, UID: f.source.UID}, Conditions: discovery.EndpointConditions{Ready: &ready}}}}}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	recorder, err := newObserverRecorder(c, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	recorder.forbidden = []byte("private-api-token")
	apiGuard := &observerGuard{origin: c.APIOrigin, allowed: map[string]bool{}, bearer: "private-api-token", base: readinessRT(f.api)}
	serviceGuard := &observerGuard{origin: "http://demo-api.dedicated.svc.cluster.local:8080", allowed: map[string]bool{"/": true}, base: readinessRT(f.service)}
	f.o = &observer{c: c, r: recorder, api: &http.Client{Transport: apiGuard}, apiGuard: apiGuard, service: &http.Client{Transport: serviceGuard}, evidence: RuntimeEvidence{Version: evidenceVersion, EvidenceKind: "synthetic", RunID: c.RunID}, readiness: &readinessObserverState{promBase: readinessRT(f.prom), endpointBase: readinessRT(f.endpoint), stream: &f.output}}
	return f
}
func (f *readinessFixture) api(q *http.Request) (*http.Response, error) {
	f.t.Helper()
	if q.Method != "GET" || q.Header.Get("Authorization") != "Bearer private-api-token" {
		f.t.Fatal("API read-only/bearer boundary violated")
	}
	path := q.URL.Path
	f.paths = append(f.paths, path)
	if f.primaryFailure != "" && strings.Contains(path, "/"+f.primaryFailure+"/") {
		if f.primaryFailureKind == "timeout" {
			return nil, os.ErrDeadlineExceeded
		}
		return readinessResponse(q, 403, map[string]string{"error": "forbidden"}), nil
	}
	ns := f.o.c.Namespace
	status := 200
	var body any
	switch path {
	case "/api/v1/namespaces/kube-system":
		body = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: "cluster"}}
	case "/api/v1/namespaces/" + ns:
		body = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: ns, UID: "namespace"}}
	case "/apis/opsloop.io/v1alpha1/namespaces/" + ns + "/recoverypolicies/demo-api":
		body = f.policy
	case "/apis/apps/v1/namespaces/" + ns + "/deployments/demo-api":
		body = f.deployment
	case "/api/v1/namespaces/" + ns + "/services/demo-api":
		body = core.Service{ObjectMeta: meta.ObjectMeta{Name: "demo-api", Namespace: ns, UID: types.UID(f.serviceUID)}}
	case "/apis/opsloop.io/v1alpha1/namespaces/" + ns + "/recoveryincidents/" + f.o.c.Incident:
		if f.incident != nil {
			body = f.incident
		} else {
			status = 404
			body = map[string]string{"error": "not found"}
		}
	case "/api/v1/namespaces/" + ns + "/pods/source":
		if f.sourceMissing {
			status = 404
			body = map[string]string{"error": "not found"}
		} else {
			body = f.source
		}
	case "/api/v1/namespaces/" + ns + "/pods/target":
		body = f.target
	case "/apis/apps/v1/namespaces/" + ns + "/replicasets/target-rs":
		body = f.targetRS
	case "/api/v1/namespaces/" + ns + "/events":
		if f.target == nil {
			f.t.Fatal("unexpected Scheduler read")
		}
		body = core.EventList{Items: []core.Event{{ObjectMeta: meta.ObjectMeta{Name: "scheduled", Namespace: ns, UID: "scheduled"}, InvolvedObject: core.ObjectReference{Kind: "Pod", Namespace: ns, Name: "target", UID: f.target.UID}, Reason: "Scheduled", ReportingController: "default-scheduler", Message: "Successfully assigned " + ns + "/target to azure"}}}
	case "/apis/apps/v1/namespaces/" + ns + "/replicasets/rs":
		body = f.rs
	case "/apis/discovery.k8s.io/v1/namespaces/" + ns + "/endpointslices":
		body = f.eps
	case "/api/v1/pods":
		name := strings.TrimPrefix(q.URL.Query().Get("fieldSelector"), "spec.nodeName=")
		list := core.PodList{Items: []core.Pod{{ObjectMeta: meta.ObjectMeta{Name: "other", Namespace: "monitoring", UID: "other"}, Spec: core.PodSpec{NodeName: name, Containers: []core.Container{{Name: "monitor", Env: []core.EnvVar{{Name: "PRIVATE", Value: "never-export-this-environment"}}, Resources: core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("10m"), core.ResourceMemory: resource.MustParse("24Mi")}}}}}, Status: core.PodStatus{Phase: core.PodRunning}}}}
		if f.podListChange != nil {
			f.podListChange(&list)
		}
		body = list
	default:
		if strings.HasPrefix(path, "/api/v1/nodes/") {
			node, ok := f.nodes[strings.TrimPrefix(path, "/api/v1/nodes/")]
			if !ok {
				f.t.Fatal("unapproved Node GET", path)
			}
			body = node
		} else {
			f.t.Fatal("unapproved GET", path)
		}
	}
	return readinessResponse(q, status, body), nil
}
func (f *readinessFixture) service(q *http.Request) (*http.Response, error) {
	if q.Header.Get("Authorization") != "" || q.Header.Get("Cookie") != "" || q.Method != "GET" {
		f.t.Fatal("credential/write leaked to Service")
	}
	pod, node, arch := "source", "source-node", "amd64"
	if f.servicePodUID == "target-pod" {
		pod, node, arch = "target", "azure", "arm64"
	}
	return readinessResponse(q, f.serviceStatus, map[string]string{"service": "demo-api", "namespace": f.o.c.Namespace, "podUID": f.servicePodUID, "pod": pod, "node": node, "arch": arch, "version": "test-version"}), nil
}
func (f *readinessFixture) endpoint(q *http.Request) (*http.Response, error) {
	f.endpointCalls++
	if (q.URL.String() != "http://10.42.2.8:8080/" && q.URL.String() != "http://10.42.1.8:8080/") || q.Header.Get("Authorization") != "" || q.Method != "GET" {
		f.t.Fatal("Endpoint escaped approved Pod or received API bearer")
	}
	uid, pod, node, arch := "source-pod", "source", "source-node", "amd64"
	if q.URL.Hostname() == "10.42.1.8" {
		uid, pod, node, arch = "target-pod", "target", "azure", "arm64"
	}
	return readinessResponse(q, f.endpointStatus, map[string]string{"service": "demo-api", "namespace": f.o.c.Namespace, "podUID": uid, "pod": pod, "node": node, "arch": arch, "version": "test-version"}), nil
}
func (f *readinessFixture) prom(q *http.Request) (*http.Response, error) {
	f.promCalls++
	if q.Header.Get("Authorization") != "" {
		f.t.Fatal("API bearer leaked to Prometheus")
	}
	if f.previewError {
		return readinessResponse(q, 503, map[string]string{"status": "error"}), nil
	}
	_ = q.ParseForm()
	expr := q.Form.Get("query")
	ts := q.Form.Get("time")
	var value string
	switch {
	case strings.HasPrefix(expr, "1 - avg"):
		value = "0.3"
	case strings.HasPrefix(expr, "min by (instance) (timestamp("), strings.HasPrefix(expr, "timestamp("):
		value = ts
	case strings.HasPrefix(expr, "min by (instance) (count_over_time("):
		value = "6"
	case strings.HasPrefix(expr, "node_memory_MemAvailable_bytes"):
		value = "6442450944"
	case strings.HasPrefix(expr, "node_memory_MemTotal_bytes"):
		value = "8589934592"
	case strings.HasPrefix(expr, "up{"):
		value = "1"
	default:
		f.t.Fatal("unexpected Prometheus expression", expr)
	}
	series := []string{}
	for index, ip := range []string{"100.64.0.2", "100.64.0.3"} {
		v := value
		if index == 1 {
			if strings.HasPrefix(expr, "1 - avg") {
				v = "0.02"
			}
			if strings.HasPrefix(expr, "node_memory_MemAvailable_bytes") {
				v = "3221225472"
			}
			if strings.HasPrefix(expr, "node_memory_MemTotal_bytes") {
				v = "4294967296"
			}
		}
		series = append(series, fmt.Sprintf(`{"metric":{"instance":"%s:9100"},"value":[%s,"%s"]}`, ip, ts, v))
	}
	b := fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(series, ","))
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(b)), Request: q}, nil
}
func (f *readinessFixture) poll() error {
	if err := f.o.poll(context.Background()); err != nil {
		return err
	}
	return f.o.pollReadiness(context.Background())
}
func TestReadinessObserverBaselineUsesDirectPathsProductionQueriesAndNoRecoveryPASS(t *testing.T) {
	f := newReadinessFixture(t)
	for range 3 {
		if err := f.poll(); err != nil {
			t.Fatal(err)
		}
		_ = f.clock.Sleep(context.Background(), time.Second)
	}
	if f.promCalls != 7 || f.endpointCalls != 3 {
		t.Fatalf("Prom/Endpoint calls %d/%d", f.promCalls, f.endpointCalls)
	}
	if f.o.readiness.preview.SelectedNodeName != "azure" || len(f.o.readiness.preview.Candidates) != 2 || f.o.readiness.preview.QueryVersion != candidates.QueryVersion || f.o.readiness.preview.DecisionReason != candidates.DecisionSelected {
		t.Fatalf("preview=%+v", f.o.readiness.preview)
	}
	if err := f.o.finalizeReadiness(&f.output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.o.r.dir, "readiness-observations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	if result["status"] != "BASELINE_OBSERVED" || result["overallPass"] != false || result["liveProof"] != false {
		t.Fatal("baseline mislabeled as a recovery PASS")
	}
	archive := readinessTestExport(t, f.output.Bytes())
	if bytes.Contains(archive, []byte("never-export-this-environment")) || bytes.Contains(archive, []byte("private-api-token")) {
		t.Fatal("credentials/arbitrary Pod env leaked to export")
	}
}
func readinessTestExport(t *testing.T, logs []byte) []byte {
	t.Helper()
	var compressed []byte
	index := 0
	sha := ""
	expectedBytes := 0
	for _, line := range bytes.Split(logs, []byte("\n")) {
		var frame struct {
			Schema string `json:"schema"`
			Index  int    `json:"index"`
			SHA    string `json:"archiveSHA256"`
			Bytes  int    `json:"archiveBytes"`
			Data   string `json:"base64"`
		}
		if json.Unmarshal(line, &frame) != nil || frame.Schema != "opsloop-observer-export/v1" {
			continue
		}
		if frame.Index != index {
			t.Fatal("export chunk ordering")
		}
		index++
		sha = frame.SHA
		expectedBytes = frame.Bytes
		part, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil {
			t.Fatal(err)
		}
		compressed = append(compressed, part...)
	}
	if index == 0 || len(compressed) != expectedBytes || digestBytes(compressed) != sha {
		t.Fatal("export incomplete/hash mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var all bytes.Buffer
	members := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != h.Name {
			t.Fatal("unsafe export")
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members[h.Name] = body
		all.Write(body)
	}
	var manifest observerManifest
	if err = json.Unmarshal(members["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(members["manifest.json"], members["COMPLETE"]) {
		t.Fatal("completion marker mismatch")
	}
	for _, file := range manifest.Files {
		if len(members[file.Ref]) != file.Bytes || digestBytes(members[file.Ref]) != file.SHA256 {
			t.Fatal("export member verification failed", file.Ref)
		}
	}
	return all.Bytes()
}
func TestReadinessObserverRejectsWrongBindingsAndUnknownPaths(t *testing.T) {
	for _, variant := range []string{"policyUID", "sourceUID", "sourceOwner", "endpointUID", "endpointIP", "endpointPort", "nodeCIDR", "nodeArch", "ServiceUID", "serviceIdentity", "serviceFailsEndpointSucceeds", "prometheusFails", "nativeSidecar", "overhead"} {
		t.Run(variant, func(t *testing.T) {
			f := newReadinessFixture(t)
			switch variant {
			case "policyUID":
				f.policy.UID = "foreign"
			case "sourceUID":
				f.source.UID = "foreign"
			case "sourceOwner":
				f.source.OwnerReferences = nil
			case "endpointUID":
				f.eps.Items[0].Endpoints[0].TargetRef.UID = "foreign"
			case "endpointIP":
				f.eps.Items[0].Endpoints[0].Addresses = []string{"192.168.1.10"}
			case "endpointPort":
				port := int32(18081)
				f.eps.Items[0].Ports[0].Port = &port
			case "nodeCIDR":
				n := f.nodes["source-node"]
				n.Spec.PodCIDR = "192.168.1.0/24"
				f.nodes["source-node"] = n
			case "nodeArch":
				n := f.nodes["source-node"]
				n.Labels[core.LabelArchStable] = "arm64"
				f.nodes["source-node"] = n
			case "ServiceUID":
				f.serviceUID = "foreign"
			case "serviceIdentity":
				f.servicePodUID = "foreign"
			case "serviceFailsEndpointSucceeds":
				f.serviceStatus = 503
			case "prometheusFails":
				f.previewError = true
			case "nativeSidecar":
				f.podListChange = func(list *core.PodList) {
					always := core.ContainerRestartPolicyAlways
					list.Items[0].Spec.InitContainers = []core.Container{{Name: "sidecar", RestartPolicy: &always}}
				}
			case "overhead":
				f.podListChange = func(list *core.PodList) {
					list.Items[0].Spec.Overhead = core.ResourceList{core.ResourceCPU: resource.MustParse("10m")}
				}
			}
			if err := f.poll(); err == nil {
				t.Fatal("unsafe/unproven baseline accepted")
			}
			if _, err := os.Stat(filepath.Join(f.o.r.dir, "COMPLETE")); err == nil {
				t.Fatal("failed collection published COMPLETE")
			}
		})
	}
}
func TestReadinessObserverContainerRestartInvalidatesSource(t *testing.T) {
	f := newReadinessFixture(t)
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	f.source.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	f.source.Status.ContainerStatuses[0].RestartCount++
	if err := f.poll(); err == nil {
		t.Fatal("source incarnation change accepted")
	}
}
func TestReadinessPrometheusGuardOnlyPermitsExactReadQueriesAndTime(t *testing.T) {
	now := time.Now()
	called := false
	g := &readinessPromGuard{origin: "http://prom.test:9090", queries: readinessPromQueries(180 * time.Second), at: now, base: readinessRT(func(q *http.Request) (*http.Response, error) {
		called = true
		return readinessResponse(q, 200, nil), nil
	})}
	query := `up{job="node-exporter"}`
	when := fmt.Sprintf("%.9f", float64(now.UnixNano())/1e9)
	for _, variant := range []string{"valid", "wrongOrigin", "writeVerb", "wrongPath", "queryInjection", "timeChanged", "authorization", "extraParameter"} {
		t.Run(variant, func(t *testing.T) {
			called = false
			u := "http://prom.test:9090/api/v1/query?" + url.Values{"query": {query}, "time": {when}}.Encode()
			q, _ := http.NewRequest("GET", u, nil)
			switch variant {
			case "wrongOrigin":
				q.URL.Host = "other.test"
			case "writeVerb":
				q.Method = "DELETE"
			case "wrongPath":
				q.URL.Path = "/api/v1/admin/tsdb/delete_series"
			case "queryInjection":
				values := q.URL.Query()
				values.Set("query", "up")
				q.URL.RawQuery = values.Encode()
			case "timeChanged":
				values := q.URL.Query()
				values.Set("time", "1")
				q.URL.RawQuery = values.Encode()
			case "authorization":
				q.Header.Set("Authorization", "Bearer private-api-token")
			case "extraParameter":
				values := q.URL.Query()
				values.Set("extra", "anything")
				q.URL.RawQuery = values.Encode()
			}
			_, err := g.RoundTrip(q)
			if (variant == "valid") != (err == nil && called) {
				t.Fatalf("variant %s err=%v called=%v", variant, err, called)
			}
		})
	}
}

func (f *readinessFixture) runtimeMode() {
	f.o.c.Readiness.Mode = "runtime"
	f.o.c.ExpectNode = ""
	f.policy.Spec.Mode = v1.PolicyModeEnforce
	f.policy.Generation = 2
	f.policy.Status.Conditions[0].ObservedGeneration = 2
	f.o.c.Readiness.PolicyGeneration = 2
	f.o.c.Readiness.PolicyHash = v1.PolicyHash(f.policy.Spec)
}
func (f *readinessFixture) appliedTargetBeforeControllerPodStatus() {
	cfg := f.o.c.Readiness
	preHash := v1.TemplateHash(f.deployment.Spec.Template)
	preRV := f.deployment.ResourceVersion
	f.deployment.ResourceVersion = "11"
	f.deployment.Generation++
	f.deployment.Spec.Template.Spec.NodeSelector[core.LabelHostname] = "azure"
	f.deployment.Spec.Template.Annotations = map[string]string{"opsloop.io/attempt-id": "action-one"}
	hash := v1.TemplateHash(f.deployment.Spec.Template)
	yes := true
	rs := apps.ReplicaSet{ObjectMeta: meta.ObjectMeta{Name: "target-rs", Namespace: f.o.c.Namespace, UID: "target-rs", OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: f.o.c.Workload, UID: types.UID(cfg.DeploymentUID), Controller: &yes}}}, Spec: apps.ReplicaSetSpec{Template: *f.deployment.Spec.Template.DeepCopy()}}
	rs.Spec.Template.Labels[apps.DefaultDeploymentUniqueLabelKey] = "target-hash"
	pod := core.Pod{ObjectMeta: meta.ObjectMeta{Name: "target", Namespace: f.o.c.Namespace, UID: "target-pod", OwnerReferences: []meta.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}}, Spec: *f.deployment.Spec.Template.Spec.DeepCopy(), Status: *f.source.Status.DeepCopy()}
	pod.Spec.NodeName = "azure"
	pod.Status.PodIP = "10.42.1.8"
	pod.Status.ContainerStatuses[0].Image = cfg.Platforms[1].ConfigDigest
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://target"
	f.target = &pod
	f.targetRS = &rs
	f.eps.Items[0].Endpoints[0].Addresses = []string{pod.Status.PodIP}
	f.eps.Items[0].Endpoints[0].TargetRef = &core.ObjectReference{Kind: "Pod", Namespace: f.o.c.Namespace, Name: pod.Name, UID: pod.UID}
	f.servicePodUID = string(pod.UID)
	sourceBinding := classification.ReadinessContainerHash(&f.source)
	f.incident = &v1.RecoveryIncident{ObjectMeta: meta.ObjectMeta{Name: f.o.c.Incident, Namespace: f.o.c.Namespace, UID: "incident"}, Spec: v1.RecoveryIncidentSpec{PolicyRef: v1.PolicyRef{Name: f.o.c.Policy, UID: cfg.PolicyUID}, PolicyGeneration: cfg.PolicyGeneration, PolicyHash: cfg.PolicyHash, Policy: f.policy.Spec, Workload: v1.WorkloadRef{Name: f.o.c.Workload, UID: cfg.DeploymentUID}, Trigger: v1.Trigger{Class: classification.ClassApplication, Reason: classification.ReasonPodNotReady, PodName: cfg.SourcePod.Name, PodUID: cfg.SourcePod.UID, NodeName: cfg.SourceNode.Name, NodeUID: cfg.SourceNode.UID, Evidence: []string{"POD_NOT_READY podUID=" + cfg.SourcePod.UID + " previouslyReadyObserved=true containerIdentitySHA256=" + sourceBinding}}}, Status: v1.RecoveryIncidentStatus{Runtime: v1.RuntimeStatus{Phase: "VERIFYING", Attempts: []v1.Attempt{{ActionID: "action-one", Outcome: "Patched", NodeName: "azure", NodeUID: "uid-azure", Hostname: "azure", PreTemplateHash: preHash, TargetTemplateHash: hash, PreResourceVersion: preRV}}}}}
}
func TestReadinessObserverHandlesNativeEndpointBeforeControllerPodStatus(t *testing.T) {
	f := newReadinessFixture(t)
	f.runtimeMode()
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	f.appliedTargetBeforeControllerPodStatus()
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if f.o.incident.Status.Runtime.Attempts[0].PodUID != "" {
		t.Fatal("Observer manufactured Controller Pod status")
	}
	if f.o.readiness.actionPods["target-pod"].ActionID != "action-one" {
		t.Fatal("native owner chain not independently resolved")
	}
	a := &f.incident.Status.Runtime.Attempts[0]
	a.PodUID = "target-pod"
	a.PodName = "target"
	a.ReplicaSetUID = "target-rs"
	a.ReplicaSetName = "target-rs"
	f.sourceMissing = true
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if !f.o.readiness.schedulerWitness {
		t.Fatal("actual fixture Scheduler event was not captured")
	}
	if len(f.o.readiness.schedulerEvents) != 1 || f.o.readiness.schedulerEvents[0].PodUID != a.PodUID {
		t.Fatal("Scheduler event not linked to actual attempt Pod")
	}
	if f.o.readiness.samples[2].SourcePresent {
		t.Fatal("own rollout source deletion not observed")
	}
	var stream struct {
		Schema string            `json:"schema"`
		Frame  readinessProgress `json:"frame"`
	}
	found := false
	for _, line := range bytes.Split(f.output.Bytes(), []byte("\n")) {
		if json.Unmarshal(line, &stream) == nil && stream.Schema == "opsloop-readiness-progress-envelope/v1" && stream.Frame.ControllerPodUID == "target-pod" {
			found = true
			if !stream.Frame.TemplateApplied || !stream.Frame.SchedulerWitness || stream.Frame.Sample.Service.Arch != "arm64" {
				t.Fatal("incomplete native target witness")
			}
		}
	}
	if !found {
		t.Fatal("bounded host-arrival progress channel missing")
	}
	if err := f.o.finalizeReadiness(&f.output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.o.r.dir, "readiness-observations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	_ = json.Unmarshal(raw, &summary)
	if summary["overallPass"] != false || summary["status"] != "RUNTIME_CAPTURED_REQUIRES_FAULT_GUARD_FINISH_VERIFICATION" {
		t.Fatal("unbound collection mislabeled as recovery PASS")
	}
}
func TestReadinessObserverRejectsSourceDeletionBeforeOwnActionAndPolicyDrift(t *testing.T) {
	for _, variant := range []string{"sourceDeleted", "foreignGeneration", "otherPolicyEdit"} {
		t.Run(variant, func(t *testing.T) {
			f := newReadinessFixture(t)
			f.runtimeMode()
			if err := f.poll(); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "sourceDeleted":
				f.sourceMissing = true
			case "foreignGeneration":
				f.policy.Generation++
			case "otherPolicyEdit":
				f.policy.Spec.Filter.MaxCPUUtilizationPercent = 99
			}
			if err := f.poll(); err == nil {
				t.Fatal("out-of-scope runtime authority or source disappearance accepted")
			}
		})
	}
}

func TestReadinessObserverExportRejectsIncompleteTamperedAndEscapingMembers(t *testing.T) {
	for _, variant := range []string{"incomplete", "tampered", "escape"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			run := "export-test"
			body := []byte("safe evidence")
			name := "evidence.json"
			if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
				t.Fatal(err)
			}
			if variant == "escape" {
				outside := filepath.Join(t.TempDir(), "credential")
				if err := os.WriteFile(outside, []byte("private-outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
					t.Fatal(err)
				}
				name = "escape"
			}
			manifest := observerManifest{RunID: run, State: "complete", Files: []observerFile{{Ref: name, SHA256: digestBytes(body), Bytes: len(body)}}}
			raw, _ := json.Marshal(manifest)
			_ = os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600)
			if variant != "incomplete" {
				_ = os.WriteFile(filepath.Join(dir, "COMPLETE"), raw, 0600)
			}
			if variant == "tampered" {
				_ = os.WriteFile(filepath.Join(dir, name), []byte("different content"), 0600)
			}
			var out bytes.Buffer
			if err := exportObserverRun(&out, dir, run); err == nil || out.Len() != 0 {
				t.Fatal("invalid evidence exported")
			}
		})
	}
}
func TestReadinessObserverExportDoesNotReadUndeclaredFiles(t *testing.T) {
	f := newReadinessFixture(t)
	for range 3 {
		if err := f.poll(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.o.r.dir, "undeclared-credential"), []byte("private-unlisted-value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.o.finalizeReadiness(&f.output); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(readinessTestExport(t, f.output.Bytes()), []byte("private-unlisted-value")) {
		t.Fatal("export scanned files outside manifest")
	}
}

func TestReadinessObserverNeverReusesPrimaryObjectsAfterFailedFreshRead(t *testing.T) {
	for _, resource := range []string{"recoverypolicies", "deployments", "services"} {
		for _, kind := range []string{"forbidden", "timeout"} {
			t.Run(resource+"/"+kind, func(t *testing.T) {
				f := newReadinessFixture(t)
				if err := f.poll(); err != nil {
					t.Fatal(err)
				}
				calls := f.endpointCalls
				f.primaryFailure = resource
				f.primaryFailureKind = kind
				if err := f.poll(); err == nil {
					t.Fatal("cached primary object reused after failed current read")
				}
				if f.endpointCalls != calls {
					t.Fatal("dynamic endpoint probe occurred after failed primary read")
				}
				if err := f.o.pollReadiness(context.Background()); err == nil {
					t.Fatal("direct readiness entry accepted stale primary reads")
				}
			})
		}
	}
}
func TestReadinessObserverRejectsForeignIncidentBeforeDynamicFollow(t *testing.T) {
	for _, variant := range []string{"policyUID", "policyHash", "snapshotPolicy", "workloadUID", "generation", "sourcePodUID", "containerBinding"} {
		t.Run(variant, func(t *testing.T) {
			f := newReadinessFixture(t)
			f.runtimeMode()
			if err := f.poll(); err != nil {
				t.Fatal(err)
			}
			f.appliedTargetBeforeControllerPodStatus()
			a := &f.incident.Status.Runtime.Attempts[0]
			a.PodName = "foreign-dynamic"
			a.PodUID = "target-pod"
			a.ReplicaSetName = "foreign-dynamic-rs"
			a.ReplicaSetUID = "target-rs"
			switch variant {
			case "policyUID":
				f.incident.Spec.PolicyRef.UID = "foreign"
			case "policyHash":
				f.incident.Spec.PolicyHash = strings.Repeat("0", 64)
			case "snapshotPolicy":
				f.incident.Spec.Policy.Filter.MaxCPUUtilizationPercent = 99
			case "workloadUID":
				f.incident.Spec.Workload.UID = "foreign"
			case "generation":
				f.incident.Spec.PolicyGeneration++
			case "sourcePodUID":
				f.incident.Spec.Trigger.PodUID = "foreign"
			case "containerBinding":
				f.incident.Spec.Trigger.Evidence = nil
			}
			f.paths = nil
			if err := f.poll(); err == nil {
				t.Fatal("foreign Incident accepted")
			}
			for _, path := range f.paths {
				if strings.Contains(path, "foreign-dynamic") {
					t.Fatal("Incident-selected object followed before binding validation", path)
				}
			}
		})
	}
}
