package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	prom "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixtureTime = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

type readerFunc func(context.Context, string, string) (Input, error)

func (f readerFunc) Read(c context.Context, n, p string) (Input, error) { return f(c, n, p) }
func fixture() Input {
	var p v1alpha1.RecoveryPolicy
	_ = json.Unmarshal([]byte(`{"metadata":{"name":"demo-api","namespace":"opsloop-demo","uid":"fixture-policy","generation":2,"creationTimestamp":"2026-10-20T00:00:00Z"},"spec":{"targetRef":{"name":"demo-api"},"enabled":true,"mode":"ObserveOnly","candidateSelector":{"matchLabels":{"opsloop.io/recovery-candidate":"true"}},"requiredPlatforms":["linux/amd64","linux/arm64"],"detection":{"nodeUnreadyFor":"20s","restartThreshold":3,"restartWindow":"120s"},"filter":{"maxCPUUtilizationPercent":85,"maxMemoryUtilizationPercent":85,"minimumAvailableMemory":"512Mi","startupMemoryBudget":"256Mi","requestedCapacityReservePercent":10,"maxSampleAge":"90s"},"score":{"cpuWeight":30,"memoryWeight":35,"requestsWeight":35,"recentFailurePenalty":15,"maxRecentFailurePenalty":45,"recentFailureWindow":"30m"},"budget":{"maxActionsPerIncident":2,"cooldown":"120s","placementTimeout":"90s","verificationTimeout":"60s","incidentDeadline":"10m"},"verification":{"serviceName":"demo-api","port":8080,"consecutiveSuccesses":5,"stabilityWindow":"30s"}},"status":{"observedGeneration":2}}`), &p)
	p.Annotations = map[string]string{"sensitive": "PRIVATE_SENTINEL"}
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "opsloop-control", UID: "fixture-control", Labels: map[string]string{"kubernetes.io/hostname": "opsloop-control", "node-role.kubernetes.io/control-plane": "true", "opsloop.io/recovery-candidate": "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64", KubeletVersion: "v1.35.9+k3s1"}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}, Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	replicas := int32(1)
	surge := intstr.FromInt32(1)
	unavailable := intstr.FromInt32(0)
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	d := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "opsloop-demo", UID: "fixture-workload"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "opsloop-control"}, Containers: []corev1.Container{{Name: "demo-api", Image: "example.invalid/demo@sha256:" + strings.Repeat("a", 64), Resources: corev1.ResourceRequirements{Requests: requests, Limits: requests}, Env: []corev1.EnvVar{{Name: "TOKEN", Value: "PRIVATE_SENTINEL"}}}}}}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-pod", Namespace: "opsloop-demo", UID: "fixture-pod"}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pod.Spec.NodeName = n.Name
	return Input{Policy: p, Deployment: d, Nodes: []corev1.Node{n}, Pods: []corev1.Pod{pod}}
}
func metricFixture() candidates.Metrics {
	sample := func(v float64) map[string][]candidates.Sample {
		return map[string][]candidates.Sample{"192.0.2.1": {{Value: v, Time: fixtureTime}}}
	}
	return candidates.Metrics{ObservedAt: fixtureTime, CPUUtil: sample(.25), CPUTime: sample(float64(fixtureTime.Add(-20 * time.Second).Unix())), CPUCount: sample(6), MemAvail: sample(4 << 30), MemAvailTime: sample(float64(fixtureTime.Add(-30 * time.Second).Unix())), MemTotal: sample(8 << 30), Up: sample(1)}
}
func TestSnapshotWireContract(t *testing.T) {
	in := fixture()
	out, e := Assemble(in, metricFixture(), fixtureTime, false)
	if e != nil {
		t.Fatal(e)
	}
	bytes, e := json.MarshalIndent(out, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(bytes), "PRIVATE_SENTINEL") || strings.Contains(string(bytes), "192.0.2.1") {
		t.Fatal("private Pod fields/annotations/InternalIP leaked")
	}
	if out["activeIncident"] != nil || len(out["recentIncidents"].([]Object)) != 0 {
		t.Fatal("manufactured Incident")
	}
	n := out["cluster"].(Object)["nodes"].([]Object)[0]
	usage := n["usage"].(Object)
	if usage["cpu"].(Object)["observedAt"] != fixtureTime.Add(-20*time.Second).UnixMilli() {
		t.Fatal("CPU source time replaced")
	}
	if n["requested"].(Object)["cpuMillicores"] != int64(50) {
		t.Fatal("request units")
	}
	if path := os.Getenv("OPSLOOP_BFF_FIXTURE_OUT"); path != "" {
		if e = os.WriteFile(path, append(bytes, '\n'), 0600); e != nil {
			t.Fatal(e)
		}
	} else {
		committed, err := os.ReadFile(filepath.Join("..", "..", "frontend", "dashboard", "contracts", "bff-snapshot.json"))
		if err != nil || string(bytes) != strings.TrimSpace(string(committed)) {
			t.Fatal("BFF wire fixture differs; regenerate explicitly and review")
		}
	}
}
func TestMissingStaleDuplicateAndNaN(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "nan", "whole-error"} {
		t.Run(name, func(t *testing.T) {
			m := metricFixture()
			bad := false
			switch name {
			case "missing":
				m.MemAvail = nil
			case "duplicate":
				m.CPUCount["192.0.2.1"] = append(m.CPUCount["192.0.2.1"], candidates.Sample{Value: 6})
			case "nan":
				m.CPUUtil["192.0.2.1"][0].Value = math.NaN()
			case "whole-error":
				bad = true
			}
			out, e := Assemble(fixture(), m, fixtureTime, bad)
			if e != nil {
				t.Fatal(e)
			}
			u := out["cluster"].(Object)["nodes"].([]Object)[0]["usage"].(Object)
			if u["cpu"].(Object)["value"] != nil {
				t.Fatal("invalid metric treated as usable")
			}
		})
	}
	m := metricFixture()
	m.CPUTime["192.0.2.1"][0].Value = float64(fixtureTime.Add(-2 * time.Minute).Unix())
	out, _ := Assemble(fixture(), m, fixtureTime, false)
	if out["cluster"].(Object)["nodes"].([]Object)[0]["usage"].(Object)["cpu"].(Object)["observedAt"] != fixtureTime.Add(-2*time.Minute).UnixMilli() {
		t.Fatal("stale sample silently refreshed")
	}
}
func TestIncidentJoinUsesPolicyAndWorkloadUID(t *testing.T) {
	in := fixture()
	i := v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Name: "known", UID: "incident", CreationTimestamp: metav1.NewTime(fixtureTime)}, Spec: v1alpha1.RecoveryIncidentSpec{PolicyRef: v1alpha1.PolicyRef{UID: string(in.Policy.UID)}, Workload: v1alpha1.WorkloadRef{UID: string(in.Deployment.UID)}}}
	in.Incidents = []v1alpha1.RecoveryIncident{i}
	in.Policy.Status.ActiveIncident = &v1alpha1.ActiveIncidentRef{Name: i.Name, IncidentUID: string(i.UID)}
	out, _ := Assemble(in, metricFixture(), fixtureTime, false)
	if out["activeIncident"] == nil {
		t.Fatal("active UID not joined")
	}
	in.Incidents[0].Spec.Workload.UID = "replaced"
	out, _ = Assemble(in, metricFixture(), fixtureTime, false)
	if out["activeIncident"] != nil {
		t.Fatal("foreign workload joined")
	}
}
func TestEndpointErrorsAndReadOnly(t *testing.T) {
	calls := 0
	s := Service{Reader: readerFunc(func(context.Context, string, string) (Input, error) {
		calls++
		return Input{}, errors.New("PRIVATE_SENTINEL")
	})}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/snapshot", nil))
		if w.Code != 405 || calls != 0 {
			t.Fatal("write reached collector")
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/snapshot", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "PRIVATE_SENTINEL") {
		t.Fatal("upstream failure leaked or demo fallback")
	}
}
func TestDeadline(t *testing.T) {
	s := Service{Timeout: time.Millisecond, Reader: readerFunc(func(ctx context.Context, _, _ string) (Input, error) { <-ctx.Done(); return Input{}, ctx.Err() })}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/snapshot", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestUnsafeQuantityRejected(t *testing.T) {
	if _, e := quantity(0, 1<<53); e == nil {
		t.Fatal("unsafe JS integer accepted")
	}
}
func TestGetOnlyTransportAndActualClientPaths(t *testing.T) {
	in := fixture()
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method != "GET" {
			t.Error("write transport")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo/recoverypolicies/demo-api":
			_ = json.NewEncoder(w).Encode(policy(in.Policy))
		case "/apis/apps/v1/namespaces/opsloop-demo/deployments/demo-api":
			_ = json.NewEncoder(w).Encode(in.Deployment)
		case "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(corev1.NodeList{Items: in.Nodes})
		case "/api/v1/pods", "/api/v1/namespaces/opsloop-demo-a/pods", "/api/v1/namespaces/opsloop-demo-b/pods", "/api/v1/namespaces/opsloop-system/pods":
			_ = json.NewEncoder(w).Encode(corev1.PodList{Items: in.Pods})
		case "/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo/recoveryincidents":
			_ = json.NewEncoder(w).Encode(Object{"apiVersion": "opsloop.io/v1alpha1", "kind": "RecoveryIncidentList", "items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	reader, e := NewKubernetesReader(&rest.Config{Host: server.URL})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reader.Read(context.Background(), "opsloop-demo", "demo-api"); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 5 {
		t.Fatal(calls)
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/v1/secrets", nil)
	if _, e = (GetOnly{http.DefaultTransport}).RoundTrip(req); e == nil {
		t.Fatal("write escaped transport")
	}
	if len(calls) != 5 {
		t.Fatal("forbidden request reached server")
	}
	calls = nil
	reader.PodNamespaces = []string{"opsloop-demo-a", "opsloop-demo-b", "opsloop-system"}
	scoped, err := reader.Read(context.Background(), "opsloop-demo", "demo-api")
	if err != nil || !scoped.PodInventoryScoped {
		t.Fatalf("scoped read: %v", err)
	}
	for _, call := range calls {
		if call == "GET /api/v1/pods" {
			t.Fatal("cluster-wide Pod read escaped scope")
		}
	}
	if len(calls) != 7 {
		t.Fatal(calls)
	}

}

func TestFreeFormDiagnosticsAreNotForwarded(t *testing.T) {
	in := fixture()
	i := v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Name: "fixture-incident", UID: "fixture-incident"}, Spec: v1alpha1.RecoveryIncidentSpec{PolicyRef: v1alpha1.PolicyRef{UID: string(in.Policy.UID)}, Workload: v1alpha1.WorkloadRef{UID: string(in.Deployment.UID)}}}
	i.Spec.Trigger.Evidence = []string{"PRIVATE_SENTINEL"}
	i.Spec.Baseline.Image = "PRIVATE_SENTINEL"
	i.Status.Runtime.Message = "PRIVATE_SENTINEL"
	i.Status.Runtime.Attempts = []v1alpha1.Attempt{{LastProbeError: "PRIVATE_SENTINEL"}}
	i.Status.Runtime.Decision = &v1alpha1.RecoveryDecision{Candidates: []v1alpha1.CandidateEvaluation{{Details: []string{"PRIVATE_SENTINEL"}}}}
	in.Incidents = []v1alpha1.RecoveryIncident{i}
	in.Policy.Status.Conditions = []metav1.Condition{{Message: "PRIVATE_SENTINEL"}}
	out, err := Assemble(in, metricFixture(), fixtureTime, false)
	data, enc := json.Marshal(out)
	if err != nil || enc != nil || strings.Contains(string(data), "PRIVATE_SENTINEL") {
		t.Fatal("free-form diagnostics leaked")
	}
	if in.Incidents[0].Status.Runtime.Message != "PRIVATE_SENTINEL" {
		t.Fatal("projection mutated input")
	}
}

func TestPreparedRBACIsReadOnlyAndSecretFree(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "dashboard-bff", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(body)), 4096)
	roles, bindings, accounts := 0, 0, 0
	for {
		var obj map[string]any
		err := decoder.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch obj["kind"] {
		case "Role", "ClusterRole":
			roles++
			for _, value := range obj["rules"].([]any) {
				rule := value.(map[string]any)
				for _, verb := range rule["verbs"].([]any) {
					if verb != "get" && verb != "list" {
						t.Fatalf("write verb %v", verb)
					}
				}
				for _, resource := range rule["resources"].([]any) {
					if resource == "secrets" || resource == "*" {
						t.Fatalf("secret/wildcard %v", resource)
					}
				}
			}
		case "RoleBinding", "ClusterRoleBinding":
			bindings++
		case "ServiceAccount":
			accounts++
		default:
			t.Fatalf("unexpected object %v", obj["kind"])
		}
	}
	if roles != 2 || bindings != 2 || accounts != 1 {
		t.Fatal("unexpected RBAC resource scope")
	}
}

func TestPrometheusUsesSevenGETQueriesAndRejectsAdminPaths(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/prefix/api/v1/query" || r.URL.Query().Get("query") == "" {
			t.Errorf("not a read-only query: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()
	transport := PrometheusQueryOnly{Base: http.DefaultTransport}
	client, err := prom.NewClient(prom.Config{Address: server.URL + "/prefix", RoundTripper: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidates.QueryMetrics(context.Background(), promv1.NewAPI(client), fixtureTime, 180*time.Second); err != nil {
		t.Fatal(err)
	}
	if calls != 7 {
		t.Fatalf("want 7 queries, got %d", calls)
	}
	for _, path := range []string{"/api/v1/admin/tsdb/delete_series", "/api/v1/write", "/api/v1/series"} {
		req, _ := http.NewRequest("POST", server.URL+path, strings.NewReader("query=up"))
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("non-query endpoint allowed")
		}
	}
	if calls != 7 {
		t.Fatal("forbidden request reached upstream")
	}
}

func TestExpiredObservationNeverReads(t *testing.T) {
	calls := 0
	s := Service{Until: time.Now().Add(-time.Second), Reader: readerFunc(func(context.Context, string, string) (Input, error) { calls++; return Input{}, nil })}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/snapshot", nil))
	if w.Code != 410 || calls != 0 {
		t.Fatalf("status=%d reads=%d", w.Code, calls)
	}
}
