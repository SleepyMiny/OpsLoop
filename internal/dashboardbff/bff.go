// Package dashboardbff joins Kubernetes observations and Prometheus metrics.
// It has no recovery actions, credential endpoint, proxy, or Kubernetes write path.
package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Object = map[string]any
type Reader interface {
	Read(context.Context, string, string) (Input, error)
}
type MetricsReader func(context.Context, time.Time, time.Duration) (candidates.Metrics, error)
type Input struct {
	Policy             v1alpha1.RecoveryPolicy
	Incidents          []v1alpha1.RecoveryIncident
	Deployment         appsv1.Deployment
	Nodes              []corev1.Node
	Pods               []corev1.Pod
	PodInventoryScoped bool
}
type Service struct {
	Reader                Reader
	Metrics               MetricsReader
	Namespace, PolicyName string
	Now                   func() time.Time
	Timeout               time.Duration
	Until                 time.Time
}

// GetOnly is defense in depth even when a local owner's kubeconfig is used.
// Production still requires the separate read-only ServiceAccount/RBAC.
type GetOnly struct{ Base http.RoundTripper }

func (t GetOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, errors.New("dashboard Kubernetes transport permits GET only")
	}
	return t.Base.RoundTrip(r)
}

type KubernetesReader struct {
	client        kubernetes.Interface
	dynamic       dynamic.Interface
	PodNamespaces []string
}

func NewKubernetesReader(config *rest.Config) (*KubernetesReader, error) {
	cfg := rest.CopyConfig(config)
	previous := cfg.WrapTransport
	cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		if previous != nil {
			base = previous(base)
		}
		return GetOnly{Base: base}
	}
	cfg.Timeout = 3 * time.Second
	k, e := kubernetes.NewForConfig(cfg)
	if e != nil {
		return nil, e
	}
	d, e := dynamic.NewForConfig(cfg)
	if e != nil {
		return nil, e
	}
	return &KubernetesReader{client: k, dynamic: d}, nil
}
func (r *KubernetesReader) Read(ctx context.Context, ns, name string) (Input, error) {
	var in Input
	p, e := r.dynamic.Resource(schema.GroupVersionResource{Group: "opsloop.io", Version: "v1alpha1", Resource: "recoverypolicies"}).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return in, e
	}
	if e = runtime.DefaultUnstructuredConverter.FromUnstructured(p.Object, &in.Policy); e != nil {
		return in, e
	}
	dep, e := r.client.AppsV1().Deployments(ns).Get(ctx, in.Policy.Spec.TargetRef.Name, metav1.GetOptions{})
	if e != nil {
		return in, e
	}
	in.Deployment = *dep
	nodes, e := r.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if e != nil {
		return in, e
	}
	in.Nodes = nodes.Items
	namespaces := r.PodNamespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	} else {
		in.PodInventoryScoped = true
	}
	for _, namespace := range namespaces {
		pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: 500})
		if err != nil {
			return in, err
		}
		if pods.Continue != "" {
			return in, errors.New("scoped pod inventory incomplete")
		}
		in.Pods = append(in.Pods, pods.Items...)
	}
	incidents, e := r.dynamic.Resource(schema.GroupVersionResource{Group: "opsloop.io", Version: "v1alpha1", Resource: "recoveryincidents"}).Namespace(ns).List(ctx, metav1.ListOptions{})
	if e != nil {
		return in, e
	}
	for _, o := range incidents.Items {
		var i v1alpha1.RecoveryIncident
		if e = runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &i); e != nil {
			return in, e
		}
		in.Incidents = append(in.Incidents, i)
	}
	return in, nil
}

func metadata(m metav1.ObjectMeta) Object {
	return Object{"name": m.Name, "namespace": m.Namespace, "uid": m.UID, "generation": m.Generation, "creationTimestamp": m.CreationTimestamp}
}

// Typed spec/status only: no annotations, managedFields, Pod env, volumes or Secret data.
func policy(p v1alpha1.RecoveryPolicy) Object {
	p = *p.DeepCopy()
	for n := range p.Status.Conditions {
		p.Status.Conditions[n].Message = ""
	}
	return Object{"apiVersion": "opsloop.io/v1alpha1", "kind": "RecoveryPolicy", "metadata": metadata(p.ObjectMeta), "spec": p.Spec, "status": p.Status}
}
func incident(i v1alpha1.RecoveryIncident) Object {
	// Free-form diagnostic strings can contain application or endpoint details.
	// They are unnecessary for the presentation contract; never forward bodies.
	i = *i.DeepCopy()
	i.Spec.Trigger.Evidence = nil
	i.Spec.Baseline.Image = ""
	i.Status.Runtime.Message = ""
	for n := range i.Status.Runtime.Attempts {
		i.Status.Runtime.Attempts[n].LastProbeError = ""
	}
	if d := i.Status.Runtime.Decision; d != nil {
		for n := range d.Candidates {
			d.Candidates[n].Details = nil
		}
	}
	for n := range i.Status.Runtime.Attempts {
		for j := range i.Status.Runtime.Attempts[n].Decision.Candidates {
			i.Status.Runtime.Attempts[n].Decision.Candidates[j].Details = nil
		}
	}
	return Object{"apiVersion": "opsloop.io/v1alpha1", "kind": "RecoveryIncident", "metadata": metadata(i.ObjectMeta), "spec": i.Spec, "status": i.Status}
}
func quantity(cpu, mem int64) (Object, error) {
	if cpu < 0 || mem < 0 || cpu > 1<<53-1 || mem > 1<<53-1 {
		return nil, errors.New("resource quantity outside JavaScript safe integer range")
	}
	return Object{"cpuMillicores": cpu, "memoryBytes": mem}, nil
}
func emptyUsage() Object {
	return Object{"cpu": Object{"value": nil, "observedAt": int64(0)}, "memory": Object{"value": nil, "observedAt": int64(0)}, "memAvailableBytes": Object{"value": nil, "observedAt": int64(0)}, "memTotalBytes": nil, "cpuSampleCount": nil, "exporterUp": nil}
}
func one(series map[string][]candidates.Sample, ip string) (float64, bool) {
	v := series[ip]
	if len(v) != 1 || math.IsNaN(v[0].Value) || math.IsInf(v[0].Value, 0) {
		return 0, false
	}
	return v[0].Value, true
}
func metrics(m candidates.Metrics, n corev1.Node) Object {
	u := emptyUsage()
	ips := []string{}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ips = append(ips, a.Address)
		}
	}
	if len(ips) != 1 {
		return u
	}
	ip := ips[0]
	cpu, a := one(m.CPUUtil, ip)
	ct, b := one(m.CPUTime, ip)
	count, c := one(m.CPUCount, ip)
	avail, d := one(m.MemAvail, ip)
	mt, e := one(m.MemAvailTime, ip)
	total, f := one(m.MemTotal, ip)
	up, g := one(m.Up, ip)
	if !(a && b && c && d && e && f && g) || cpu < 0 || cpu > 1 || count < 0 || count != math.Trunc(count) || avail < 0 || total <= 0 || avail > total || total > 1<<50 || (up != 0 && up != 1) || ct <= 0 || mt <= 0 || ct >= 253402300800 || mt >= 253402300800 {
		return u
	}
	u["cpu"] = Object{"value": cpu, "observedAt": int64(math.Round(ct * 1000))}
	u["memory"] = Object{"value": 1 - avail/total, "observedAt": int64(math.Round(mt * 1000))}
	u["memAvailableBytes"] = Object{"value": avail, "observedAt": int64(math.Round(mt * 1000))}
	u["memTotalBytes"] = total
	u["cpuSampleCount"] = count
	u["exporterUp"] = up
	return u
}
func podSummary(p corev1.Pod) Object {
	role := "system"
	switch {
	case p.Namespace == "monitoring":
		role = "observability"
	case p.Namespace == "opsloop-system":
		role = "opsloop"
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	restarts := int32(0)
	for _, c := range p.Status.ContainerStatuses {
		restarts += c.RestartCount
	}
	phase := p.Status.Phase
	if phase == "" {
		phase = corev1.PodUnknown
	}
	return Object{"uid": p.UID, "name": p.Name, "namespace": p.Namespace, "role": role, "phase": phase, "ready": ready, "restarts": restarts}
}

// Assemble never creates a historical Incident or substitutes demo data.
func Assemble(in Input, m candidates.Metrics, at time.Time, metricError bool) (Object, error) {
	nodes := []Object{}
	selector, selectorErr := metav1.LabelSelectorAsSelector(in.Deployment.Spec.Selector)
	sort.Slice(in.Nodes, func(i, j int) bool { return in.Nodes[i].Name < in.Nodes[j].Name })
	for _, n := range in.Nodes {
		arch := n.Status.NodeInfo.Architecture
		if arch != "amd64" && arch != "arm64" {
			return nil, errors.New("unsupported node architecture")
		}
		site := ""
		switch n.Name {
		case "opsloop-control":
			site = "laptop-vm"
		case "opsloop-azure-worker":
			site = "azure-vm"
		case "sarmubuntuserver":
			site = "physical-server"
		default:
			return nil, fmt.Errorf("node site mapping required")
		}
		role := "worker"
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			role = "control-plane"
		}
		readiness := "Unknown"
		pressure := []string{}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				if c.Status == corev1.ConditionTrue {
					readiness = "Ready"
				} else if c.Status == corev1.ConditionFalse {
					readiness = "NotReady"
				}
			}
			if c.Status == corev1.ConditionTrue && (c.Type == corev1.NodeMemoryPressure || c.Type == corev1.NodeDiskPressure || c.Type == corev1.NodePIDPressure) {
				pressure = append(pressure, string(c.Type))
			}
		}
		var cpu, mem int64
		pods := []Object{}
		for _, p := range in.Pods {
			if p.Spec.NodeName != n.Name {
				continue
			}
			summary := podSummary(p)
			// This is a workload presentation grouping, not owner/rollout proof.
			// Never group a same-name or same-label Pod from another Namespace.
			if selectorErr == nil && !selector.Empty() && p.Namespace == in.Deployment.Namespace && selector.Matches(labels.Set(p.Labels)) {
				summary["role"] = "demo-api"
			}
			pods = append(pods, summary)
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				a, b := candidates.PodRequests(p.Spec)
				cpu += a
				mem += b
			}
		}
		alloc, e := quantity(n.Status.Allocatable.Cpu().MilliValue(), n.Status.Allocatable.Memory().Value())
		if e != nil {
			return nil, e
		}
		requested, e := quantity(cpu, mem)
		if e != nil {
			return nil, e
		}
		taints := []Object{}
		for _, t := range n.Spec.Taints {
			taints = append(taints, Object{"key": t.Key, "value": t.Value, "effect": t.Effect})
		}
		usage := emptyUsage()
		if !metricError {
			usage = metrics(m, n)
		}
		nodes = append(nodes, Object{"uid": n.UID, "name": n.Name, "hostnameLabel": n.Labels["kubernetes.io/hostname"], "displayName": n.Name, "site": site, "arch": arch, "role": role, "readiness": readiness, "unschedulable": n.Spec.Unschedulable, "taints": taints, "pressure": pressure, "recoveryCandidate": n.Labels["opsloop.io/recovery-candidate"] == "true", "allocatable": alloc, "requested": requested, "usage": usage, "pods": pods})
	}
	cpu, mem := candidates.PodRequests(in.Deployment.Spec.Template.Spec)
	req, e := quantity(cpu, mem)
	if e != nil {
		return nil, e
	}
	var hostname any
	if h := in.Deployment.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; h != "" {
		hostname = h
	}
	image := ""
	if len(in.Deployment.Spec.Template.Spec.Containers) > 0 {
		image = in.Deployment.Spec.Template.Spec.Containers[0].Image
	}
	digest := ""
	if _, tail, ok := strings.Cut(image, "@"); ok {
		digest = tail
	}
	tolerations := []corev1.Toleration{}
	tolerations = append(tolerations, in.Deployment.Spec.Template.Spec.Tolerations...)
	issues := candidates.CheckWorkload(&in.Deployment)
	if issues == nil {
		issues = []string{}
	}
	replicas := int32(1)
	if in.Deployment.Spec.Replicas != nil {
		replicas = *in.Deployment.Spec.Replicas
	}
	workload := Object{"uid": in.Deployment.UID, "namespace": in.Deployment.Namespace, "name": in.Deployment.Name, "replicas": replicas, "nodeSelectorHostname": hostname, "imageIndexDigest": digest, "platforms": []any{}, "requests": req, "tolerations": tolerations, "unsupportedConstraints": issues}
	recent := []Object{}
	var active any
	sort.Slice(in.Incidents, func(i, j int) bool {
		return in.Incidents[i].CreationTimestamp.After(in.Incidents[j].CreationTimestamp.Time)
	})
	for _, i := range in.Incidents {
		if i.Spec.PolicyRef.UID != string(in.Policy.UID) || i.Spec.Workload.UID != string(in.Deployment.UID) {
			continue
		}
		record := incident(i)
		if binding := observedBinding(in.Policy, in.Deployment, i); binding != nil {
			record["runtimeBinding"] = binding
		}
		if a := in.Policy.Status.ActiveIncident; a != nil && a.IncidentUID == string(i.UID) {
			active = record
		}
		if len(recent) < 20 {
			recent = append(recent, record)
		}
	}
	validMetrics := 0
	for _, n := range nodes {
		if n["usage"].(Object)["cpu"].(Object)["value"] != nil {
			validMetrics++
		}
	}
	warnings := []string{"Image platform index not queried: preview fails closed until a verified manifest source is configured.", "HTTP recovery probes/driver timings are not collected by this BFF.", "Pod requests use the Controller contract; native sidecar/Pod overhead limitations apply."}
	if in.PodInventoryScoped {
		warnings = append(warnings, "Pod inventory limited to approved namespaces; displayed requests are partial, not cluster-wide available capacity.")
	}
	if validMetrics < len(nodes) {
		warnings = append(warnings, fmt.Sprintf("Complete finite metric tuples available for %d/%d nodes; missing/ambiguous metrics remain null.", validMetrics, len(nodes)))
	}
	if metricError {
		warnings = append(warnings, "Prometheus unavailable: metrics are null, never zero or demo values.")
	}
	version := ""
	if len(in.Nodes) > 0 {
		version = in.Nodes[0].Status.NodeInfo.KubeletVersion
	}
	return Object{"source": "live", "observedAt": at.UnixMilli(), "cluster": Object{"name": "opsloop", "k3sVersion": version, "nodes": nodes}, "workload": workload, "http": Object{"endpoint": "", "probes": []any{}}, "policy": policy(in.Policy), "policyHash": v1alpha1.PolicyHash(in.Policy.Spec), "activeIncident": active, "recentIncidents": recent, "diagnostics": Object{"warnings": warnings, "metricsAvailable": !metricError && validMetrics > 0, "readOnly": true}}, nil
}
func (s Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != "/api/v1/snapshot" {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(Object{"error": "not found"})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(405)
		_ = json.NewEncoder(w).Encode(Object{"error": "read-only endpoint"})
		return
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	if !s.Until.IsZero() {
		remaining := time.Until(s.Until)
		if remaining <= 0 {
			w.WriteHeader(http.StatusGone)
			return
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	at := now()
	in, e := s.Reader.Read(ctx, s.Namespace, s.PolicyName)
	if e != nil {
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(Object{"error": "Kubernetes snapshot unavailable"})
		return
	}
	var m candidates.Metrics
	metricError := s.Metrics == nil
	if s.Metrics != nil {
		m, e = s.Metrics(ctx, at, 2*in.Policy.Spec.Filter.MaxSampleAge.Duration)
		metricError = e != nil
	}
	if ctx.Err() != nil {
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(Object{"error": "Snapshot collection timed out"})
		return
	}
	snapshot, e := Assemble(in, m, at, metricError)
	if e != nil {
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(Object{"error": "Snapshot contract unavailable"})
		return
	}
	// Marshal first: malformed/nonfinite values never produce a partial HTTP200 body.
	body, e := json.Marshal(snapshot)
	if e != nil {
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(Object{"error": "Snapshot encoding failed"})
		return
	}
	_, _ = w.Write(body)
}
