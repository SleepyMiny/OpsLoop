package publicstatus

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

var observedTime = time.UnixMilli(1800000000000)

type observedRequest struct{ Method, Path string }
type apiFixture struct {
	mu        sync.Mutex
	requests  []observedRequest
	denyCloud bool
}

func testCollector(t *testing.T) (*Collector, *apiFixture) { return testCollectorAt(t, observedTime) }
func testCollectorAt(t *testing.T, at time.Time) (*Collector, *apiFixture) {
	t.Helper()
	names := map[string]string{"control": "private-control", "physical": "private-physical", "cloud": "private-cloud"}
	objects := map[string]any{}
	i := 0
	for _, alias := range []string{"control", "physical", "cloud"} {
		name := names[alias]
		i++
		arch := "amd64"
		if alias == "cloud" {
			arch = "arm64"
		}
		objects["/api/v1/nodes/"+name] = &corev1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("SYNTHETIC_PRIVATE_UID"), Annotations: map[string]string{"secret": "SYNTHETIC_PRIVATE_SENTINEL"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: arch}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: fmt.Sprintf("192.0.2.%d", i)}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		objects["/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/"+name] = &coordv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-node-lease"}, Spec: coordv1.LeaseSpec{RenewTime: &metav1.MicroTime{Time: at.Add(-time.Second)}}}
	}
	for _, name := range []string{"opsloop-controller", "demo-api"} {
		ns := "opsloop-demo"
		if name == "opsloop-controller" {
			ns = "opsloop-system"
		}
		objects["/apis/apps/v1/namespaces/"+ns+"/deployments/"+name] = &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-SYNTHETIC_PRIVATE_UID"), Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: Ptr(int32(1)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "private", Image: "private.registry.invalid/image", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "SYNTHETIC_PRIVATE_SENTINEL"}}}}}}}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1, ObservedGeneration: 1}}
	}
	objects["/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo/recoverypolicies/demo-api"] = &v1alpha1.RecoveryPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "opsloop.io/v1alpha1", Kind: "RecoveryPolicy"}, ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "opsloop-demo", UID: "private-policy-uid", Generation: 1}, Spec: v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeObserveOnly, TargetRef: v1alpha1.TargetRef{Name: "demo-api"}}, Status: v1alpha1.RecoveryPolicyStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Monitoring", Status: metav1.ConditionTrue, ObservedGeneration: 1, Message: "SYNTHETIC_PRIVATE_SENTINEL"}}}}
	objects["/apis/opsloop.io/v1alpha1/namespaces/opsloop-demo/recoveryincidents"] = map[string]any{"apiVersion": "opsloop.io/v1alpha1", "kind": "RecoveryIncidentList", "metadata": map[string]any{}, "items": []any{}}
	state := &apiFixture{}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.requests = append(state.requests, observedRequest{r.Method, r.URL.Path})
		denied := state.denyCloud && strings.HasSuffix(r.URL.Path, "/nodes/private-cloud")
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		o, ok := objects[r.URL.Path]
		if !ok || denied || r.Method != "GET" {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonForbidden, Message: "SYNTHETIC_PRIVATE_SENTINEL", Code: 403})
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	}))
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	c, e := NewCollector(&rest.Config{Host: s.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, names, nil)
	if e != nil {
		t.Fatal(e)
	}
	c.Now = func() time.Time { return at }
	return c, state
}
func TestCollectorMinimumGETScopeAndPrivacy(t *testing.T) {
	c, state := testCollector(t)
	s := c.Collect(context.Background())
	b, e := s.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	if s.Sources.Kubernetes.State != "ok" || s.Policy.IncidentCount == nil || *s.Policy.IncidentCount != 0 {
		t.Fatalf("incomplete %+v", s)
	}
	for _, marker := range []string{"private-control", "192.0.2.", "SYNTHETIC_PRIVATE", "private.registry", "policy-uid", "PASSWORD"} {
		if strings.Contains(string(b), marker) {
			t.Fatal("projection leaked", marker)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.requests) != 10 {
		t.Fatalf("expected ten exact minimum reads, got %v", state.requests)
	}
	for _, r := range state.requests {
		if r.Method != "GET" || strings.Contains(r.Path, "/pods") || strings.Contains(r.Path, "/secrets") {
			t.Fatal("excess scope", r)
		}
	}
}
func TestCollectorPartialUnauthorizedNoFakeReady(t *testing.T) {
	c, state := testCollector(t)
	state.mu.Lock()
	state.denyCloud = true
	state.mu.Unlock()
	s := c.Collect(context.Background())
	b, e := s.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	if s.Sources.Kubernetes.State != "partial" || s.Nodes[2].Readiness != nil {
		t.Fatal("unauthorized node fabricated")
	}
	if strings.Contains(string(b), "SYNTHETIC_PRIVATE_SENTINEL") {
		t.Fatal("raw error leaked")
	}
}
func TestPrometheusSevenGETQueriesAndOriginalClocks(t *testing.T) {
	c, _ := testCollector(t)
	queries := []string{}
	mu := sync.Mutex{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/query" {
			t.Error("not read-only instant query")
		}
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		value := 0.25
		switch {
		case strings.Contains(q, "count_over_time"):
			value = 6
		case strings.Contains(q, "timestamp("):
			value = float64(observedTime.Add(-2*time.Second).UnixMilli()) / 1000
		case strings.HasPrefix(q, "node_memory_MemAvailable"):
			value = 512
		case strings.HasPrefix(q, "node_memory_MemTotal"):
			value = 1024
		case strings.HasPrefix(q, "up{"):
			value = 1
		}
		var result []any
		for i := 1; i <= 3; i++ {
			result = append(result, map[string]any{"metric": map[string]string{"instance": fmt.Sprintf("192.0.2.%d:9100", i), "private_label": "SYNTHETIC_PRIVATE_SENTINEL"}, "value": []any{float64(observedTime.Add(-2*time.Second).UnixMilli()) / 1000, strconv.FormatFloat(value, 'f', -1, 64)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	defer server.Close()
	pc, e := NewPrometheusAPI(server.URL, nil)
	if e != nil {
		t.Fatal(e)
	}
	c.Prometheus = pc
	s := c.Collect(context.Background())
	b, e := s.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 7 || s.Sources.Prometheus.State != "ok" {
		t.Fatal("query contract", queries, s.Sources.Prometheus)
	}
	for _, n := range s.Nodes {
		if n.CPU.Value == nil || *n.CPU.Value != 0.25 || n.Memory.Value == nil || *n.Memory.Value != 0.5 || n.CPU.ObservedAt == nil || *n.CPU.ObservedAt != observedTime.Add(-2*time.Second).UnixMilli() {
			t.Fatal("values/clocks changed", n)
		}
	}
	if strings.Contains(string(b), "private_label") || strings.Contains(string(b), "192.0.2.") {
		t.Fatal("metric labels leaked")
	}
}
func TestRealKubernetesTransportBlocksPOSTBeforeNetwork(t *testing.T) {
	c, state := testCollector(t)
	if _, e := c.Kube.CoreV1().Nodes().Get(context.Background(), "private-control", metav1.GetOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "blocked"}}, metav1.CreateOptions{}); e == nil {
		t.Fatal("POST passed")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.requests) != 1 || state.requests[0].Method != "GET" {
		t.Fatal("unexpected writes", state.requests)
	}
}
func TestMetricsMissingDuplicateNaNAndFutureFailClosed(t *testing.T) {
	at := observedTime.UnixMilli()
	ip := "192.0.2.1"
	node := corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
	series := func(v float64) map[string][]candidates.Sample {
		return map[string][]candidates.Sample{ip: {{Value: v, Time: observedTime.Add(-time.Second)}}}
	}
	base := func() candidates.Metrics {
		return candidates.Metrics{CPUUtil: series(0.25), CPUTime: series(float64(at-2000) / 1000), CPUCount: series(3), MemAvail: series(512), MemAvailTime: series(float64(at-2000) / 1000), MemTotal: series(1024), Up: series(1)}
	}
	for name, change := range map[string]func(*candidates.Metrics){"few CPU samples": func(m *candidates.Metrics) { m.CPUCount = series(2) }, "duplicate": func(m *candidates.Metrics) { m.CPUUtil[ip] = append(m.CPUUtil[ip], m.CPUUtil[ip][0]) }, "NaN": func(m *candidates.Metrics) { m.CPUUtil = series(math.NaN()) }, "future": func(m *candidates.Metrics) { m.CPUTime = series(float64(at+5001) / 1000) }} {
		t.Run(name, func(t *testing.T) {
			m := base()
			change(&m)
			n := Node{}
			metricsFor(&n, m, node, at)
			if n.CPU.Value != nil {
				t.Fatal("invalid CPU accepted")
			}
			if n.Memory.Value == nil {
				t.Fatal("independent memory observation discarded")
			}
		})
	}
	m := base()
	m.CPUTime = series(float64(at-91000) / 1000)
	n := Node{}
	metricsFor(&n, m, node, at)
	if n.CPU.ObservedAt == nil || *n.CPU.ObservedAt != at-91000 {
		t.Fatal("stale timestamp refreshed")
	}
}

// This is also invoked by the actual workerd test, with an offline envelope output.
func TestCollectorPipelineEnvelope(t *testing.T) {
	at := time.Now().Truncate(time.Millisecond)
	c, _ := testCollectorAt(t, at)
	c.Now = time.Now
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/query" {
			t.Error("unexpected query transport")
		}
		q := r.URL.Query().Get("query")
		v := 0.25
		switch {
		case strings.Contains(q, "count_over_time"):
			v = 6
		case strings.Contains(q, "timestamp("):
			v = float64(at.Add(-2*time.Second).UnixMilli()) / 1000
		case strings.HasPrefix(q, "node_memory_MemAvailable"):
			v = 512
		case strings.HasPrefix(q, "node_memory_MemTotal"):
			v = 1024
		case strings.HasPrefix(q, "up{"):
			v = 1
		}
		results := []any{}
		for i := 1; i <= 3; i++ {
			results = append(results, map[string]any{"metric": map[string]string{"instance": fmt.Sprintf("192.0.2.%d:9100", i), "private": "SYNTHETIC_PRIVATE_SENTINEL"}, "value": []any{float64(at.Add(-2*time.Second).UnixMilli()) / 1000, strconv.FormatFloat(v, 'f', -1, 64)}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": results}})
	}))
	defer server.Close()
	pc, e := NewPrometheusAPI(server.URL, nil)
	if e != nil {
		t.Fatal(e)
	}
	c.Prometheus = pc
	snapshot := c.Collect(context.Background())
	raw, e := snapshot.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 7 || snapshot.Sources.Kubernetes.State != "ok" || snapshot.Sources.Prometheus.State != "ok" {
		t.Fatal("collector pipeline incomplete")
	}
	if strings.Contains(string(raw), "SYNTHETIC_PRIVATE") || strings.Contains(string(raw), "192.0.2.") {
		t.Fatal("raw observation leaked")
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	keyFile := os.Getenv("OPSLOOP_TEST_KEY_FILE")
	if keyFile == "" {
		keyFile = filepath.Join(dir, "key")
		if e = os.WriteFile(keyFile, []byte(strings.Repeat("SYNTHETIC-", 5)), 0600); e != nil {
			t.Fatal(e)
		}
	}
	seq, e := OpenSequence(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer seq.Close()
	sender := Sender{Origin: "https://opsloop.sleepyminy44.workers.dev", KeyID: "k1", KeyFile: keyFile, Sequence: seq}
	request, e := sender.Request(context.Background(), raw, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if out := os.Getenv("OPSLOOP_TEST_ENVELOPE"); out != "" {
		body := struct {
			URL     string      `json:"url"`
			Method  string      `json:"method"`
			Headers http.Header `json:"headers"`
			Body    string      `json:"body"`
		}{request.URL.String(), request.Method, request.Header, string(raw)}
		encoded, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(encoded); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

func TestCollectorRequiresVerifiedPinnedCA(t *testing.T) {
	for _, cfg := range []*rest.Config{
		{Host: "https://kubernetes.invalid"},
		{Host: "https://kubernetes.invalid", TLSClientConfig: rest.TLSClientConfig{Insecure: true, CAData: []byte("not-a-certificate")}},
	} {
		if _, err := NewCollector(cfg, map[string]string{"control": "one", "physical": "two", "cloud": "three"}, nil); err == nil {
			t.Fatal("unverified Kubernetes TLS accepted")
		}
	}
}

func TestPrometheusWarningsFailClosedWithoutLeakingDiagnostic(t *testing.T) {
	c, _ := testCollector(t)
	queries := []string{}
	mu := sync.Mutex{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/query" {
			t.Error("not read-only instant query")
		}
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		value := 0.25
		switch {
		case strings.Contains(q, "count_over_time"):
			value = 6
		case strings.Contains(q, "timestamp("):
			value = float64(observedTime.Add(-2*time.Second).UnixMilli()) / 1000
		case strings.HasPrefix(q, "node_memory_MemAvailable"):
			value = 512
		case strings.HasPrefix(q, "node_memory_MemTotal"):
			value = 1024
		case strings.HasPrefix(q, "up{"):
			value = 1
		}
		var result []any
		for i := 1; i <= 3; i++ {
			result = append(result, map[string]any{"metric": map[string]string{"instance": fmt.Sprintf("192.0.2.%d:9100", i), "private_label": "SYNTHETIC_PRIVATE_SENTINEL"}, "value": []any{float64(observedTime.Add(-2*time.Second).UnixMilli()) / 1000, strconv.FormatFloat(value, 'f', -1, 64)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "warnings": []string{"SYNTHETIC_PRIVATE_DIAGNOSTIC_PARTIAL_RESULT"}, "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	defer server.Close()
	pc, e := NewPrometheusAPI(server.URL, nil)
	if e != nil {
		t.Fatal(e)
	}
	c.Prometheus = pc
	s := c.Collect(context.Background())
	b, e := s.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	if s.Sources.Prometheus.State != "unavailable" || s.Sources.Prometheus.ErrorCode == nil || *s.Sources.Prometheus.ErrorCode != "INVALID" {
		t.Fatal("warning-bearing query was accepted", s.Sources.Prometheus)
	}
	for _, n := range s.Nodes {
		if n.CPU.Value != nil || n.Memory.Value != nil || n.Exporter.Value != nil {
			t.Fatal("partial result leaked as usable metrics")
		}
	}
	if strings.Contains(string(b), "SYNTHETIC_PRIVATE_DIAGNOSTIC") {
		t.Fatal("warning diagnostic leaked")
	}
}
