package publicstatus

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/dashboardbff"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Collector struct {
	Kube       kubernetes.Interface
	Dynamic    dynamic.Interface
	Prometheus promv1.API
	Names      map[string]string
	Now        func() time.Time
}

var errPartialMetrics = errors.New("Prometheus returned query warnings")

// QueryMetrics is shared with the Controller; the public observer adds its own
// fail-closed warning policy without changing the seven expressions or exposing
// diagnostic text from the private monitoring service.
type completeMetricsAPI struct{ promv1.API }

func (a completeMetricsAPI) Query(ctx context.Context, query string, at time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	value, warnings, err := a.API.Query(ctx, query, at, opts...)
	if err == nil && len(warnings) != 0 {
		return nil, nil, errPartialMetrics
	}
	return value, warnings, err
}

func NewCollector(cfg *rest.Config, names map[string]string, api promv1.API) (*Collector, error) {
	origin, err := url.Parse(cfg.Host)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
		return nil, errors.New("HTTPS Kubernetes origin required")
	}
	if cfg.Insecure || len(cfg.CAData) == 0 && cfg.CAFile == "" {
		return nil, errors.New("pinned Kubernetes CA required")
	}
	if len(names) != 3 {
		return nil, errors.New("three explicit node mappings required")
	}
	seen := map[string]bool{}
	for _, id := range []string{"control", "physical", "cloud"} {
		n := names[id]
		if n == "" || seen[n] {
			return nil, errors.New("invalid node mappings")
		}
		seen[n] = true
	}
	c := rest.CopyConfig(cfg)
	previous := c.WrapTransport
	c.WrapTransport = func(t http.RoundTripper) http.RoundTripper {
		if previous != nil {
			t = previous(t)
		}
		return dashboardbff.GetOnly{Base: boundObservationTransport{base: t, scheme: origin.Scheme, host: origin.Host}}
	}
	c.Timeout = 3 * time.Second
	c.QPS = 5
	c.Burst = 5
	k, e := kubernetes.NewForConfig(c)
	if e != nil {
		return nil, e
	}
	d, e := dynamic.NewForConfig(c)
	if e != nil {
		return nil, e
	}
	return &Collector{Kube: k, Dynamic: d, Names: names, Prometheus: api, Now: time.Now}, nil
}

type boundObservationTransport struct {
	base         http.RoundTripper
	scheme, host string
}

func (t boundObservationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != t.scheme || r.URL.Host != t.host {
		return nil, errors.New("Kubernetes origin changed")
	}
	return t.base.RoundTrip(r)
}
func errorCode(e error) string {
	if errors.Is(e, errPartialMetrics) {
		return "INVALID"
	}
	if apierrors.IsForbidden(e) || apierrors.IsUnauthorized(e) {
		return "UNAUTHORIZED"
	}
	if apierrors.IsNotFound(e) {
		return "MISSING"
	}
	var networkError net.Error
	if apierrors.IsTimeout(e) || errors.Is(e, context.DeadlineExceeded) || errors.As(e, &networkError) && networkError.Timeout() {
		return "TIMEOUT"
	}
	return "UNREACHABLE"
}
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := c.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	out := Empty(start.UnixMilli())
	successes, failures := 0, 0
	lastCode := "MISSING"
	failed := func(code string) { failures++; lastCode = code }
	nodes := map[string]corev1.Node{}
	for i := range out.Nodes {
		n := &out.Nodes[i]
		name := c.Names[n.ID]
		node, e := c.Kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			failed(errorCode(e))
			continue
		}
		successes++
		nodes[n.ID] = *node
		if node.Status.NodeInfo.OperatingSystem == "linux" {
			n.OS = Ptr("linux")
		} else {
			failed("INVALID")
		}
		arch := node.Status.NodeInfo.Architecture
		if arch == "amd64" || arch == "arm64" {
			n.Arch = Ptr(arch)
		} else {
			failed("INVALID")
		}
		lease, e := c.Kube.CoordinationV1().Leases("kube-node-lease").Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			failed(errorCode(e))
			continue
		}
		if lease.Spec.RenewTime == nil || lease.Spec.RenewTime.UnixMilli() <= 0 || lease.Spec.RenewTime.UnixMilli() > now().UnixMilli()+5000 {
			failed("INVALID")
			continue
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				r := "Unknown"
				if condition.Status == corev1.ConditionTrue {
					r = "Ready"
				} else if condition.Status == corev1.ConditionFalse {
					r = "NotReady"
				}
				n.Readiness = Ptr(r)
				n.ReadyObservedAt = Ptr(lease.Spec.RenewTime.UnixMilli())
				break
			}
		}
		if n.Readiness == nil {
			failed("MISSING")
		}
	}
	var demo *appsv1.Deployment
	for i, pair := range [][2]string{{"opsloop-system", "opsloop-controller"}, {"opsloop-demo", "demo-api"}} {
		d, e := c.Kube.AppsV1().Deployments(pair[0]).Get(ctx, pair[1], metav1.GetOptions{})
		if e != nil {
			failed(errorCode(e))
			continue
		}
		successes++
		if i == 1 {
			demo = d
		}
		w := &out.Workloads[i]
		w.Desired = Ptr(int32(1))
		if d.Spec.Replicas != nil {
			w.Desired = Ptr(*d.Spec.Replicas)
		}
		w.ObservedAt = Ptr(now().UnixMilli())
		if *w.Desired < 0 || *w.Desired > 100 || d.Status.ReadyReplicas < 0 || d.Status.ReadyReplicas > 100 {
			*w = Workload{ID: w.ID}
			failed("INVALID")
			continue
		}
		if d.Status.ObservedGeneration == d.Generation {
			w.Ready = Ptr(d.Status.ReadyReplicas)
		} else {
			failed("MISSING")
		}
	}
	gvr := schema.GroupVersionResource{Group: "opsloop.io", Version: "v1alpha1", Resource: "recoverypolicies"}
	raw, e := c.Dynamic.Resource(gvr).Namespace("opsloop-demo").Get(ctx, "demo-api", metav1.GetOptions{})
	if e != nil {
		failed(errorCode(e))
	} else {
		var p v1alpha1.RecoveryPolicy
		if runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, &p) != nil {
			failed("INVALID")
		} else {
			successes++
			out.Policy.ObservedAt = Ptr(now().UnixMilli())
			out.Policy.Enabled = Ptr(p.Spec.Enabled)
			mode := string(p.Spec.Mode)
			if mode == "ObserveOnly" || mode == "Enforce" {
				out.Policy.Mode = &mode
			} else {
				failed("INVALID")
			}
			if p.Status.ObservedGeneration == p.Generation {
				for _, condition := range p.Status.Conditions {
					if condition.Type == "Monitoring" && condition.ObservedGeneration == p.Generation && condition.Status != metav1.ConditionUnknown {
						out.Policy.Monitoring = Ptr(condition.Status == metav1.ConditionTrue)
						break
					}
				}
			}
			if out.Policy.Monitoring == nil {
				failed("MISSING")
			}
			if demo != nil && p.Spec.TargetRef.Name == demo.Name {
				count, active := 0, 0
				valid := true
				token := ""
				seenTokens := map[string]bool{}
				for {
					list, err := c.Dynamic.Resource(schema.GroupVersionResource{Group: "opsloop.io", Version: "v1alpha1", Resource: "recoveryincidents"}).Namespace("opsloop-demo").List(ctx, metav1.ListOptions{Limit: 100, Continue: token})
					if err != nil {
						failed(errorCode(err))
						valid = false
						break
					}
					for _, raw := range list.Items {
						var in v1alpha1.RecoveryIncident
						if runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, &in) != nil {
							failed("INVALID")
							valid = false
							break
						}
						if in.Spec.PolicyRef.UID != string(p.UID) || in.Spec.Workload.UID != string(demo.UID) {
							continue
						}
						count++
						if p.Status.ActiveIncident != nil && p.Status.ActiveIncident.IncidentUID == string(in.UID) {
							active++
						}
					}
					if !valid || count > 10000 {
						valid = false
						failed("INVALID")
						break
					}
					token = list.GetContinue()
					if token == "" {
						break
					}
					if seenTokens[token] {
						valid = false
						failed("INVALID")
						break
					}
					seenTokens[token] = true
				}
				if valid {
					out.Policy.IncidentCount = &count
					if p.Status.ActiveIncident == nil || active == 1 {
						out.Policy.ActiveIncidentCount = &active
					} else {
						failed("MISSING")
					}
				}
			} else {
				failed("MISSING")
			}
		}
	}
	if successes > 0 {
		out.Sources.Kubernetes = Source{State: "ok", ObservedAt: Ptr(start.UnixMilli())}
		if failures > 0 {
			out.Sources.Kubernetes.State = "partial"
			out.Sources.Kubernetes.ErrorCode = Ptr(lastCode)
		}
	} else {
		out.Sources.Kubernetes.ErrorCode = Ptr(lastCode)
	}
	if c.Prometheus != nil {
		m, err := candidates.QueryMetrics(ctx, completeMetricsAPI{c.Prometheus}, now(), 180*time.Second)
		if err != nil {
			out.Sources.Prometheus.ErrorCode = Ptr(errorCode(err))
		} else {
			out.Sources.Prometheus = Source{State: "ok", ObservedAt: Ptr(now().UnixMilli())}
			for i := range out.Nodes {
				n := &out.Nodes[i]
				node, ok := nodes[n.ID]
				if !ok {
					out.Sources.Prometheus.State = "partial"
					out.Sources.Prometheus.ErrorCode = Ptr("MISSING")
					continue
				}
				metricsFor(n, m, node, now().UnixMilli())
				if n.CPU.Value == nil || n.Memory.Value == nil || n.Exporter.Value == nil {
					out.Sources.Prometheus.State = "partial"
					out.Sources.Prometheus.ErrorCode = Ptr("MISSING")
				}
			}
		}
	}
	out.CollectedAt = now().UnixMilli()
	return out
}
func metricsFor(n *Node, m candidates.Metrics, node corev1.Node, now int64) {
	ips := []string{}
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ips = append(ips, a.Address)
		}
	}
	if len(ips) != 1 {
		return
	}
	ip := ips[0]
	one := func(x map[string][]candidates.Sample) (candidates.Sample, bool) {
		s := x[ip]
		if len(s) != 1 || math.IsNaN(s[0].Value) || math.IsInf(s[0].Value, 0) {
			return candidates.Sample{}, false
		}
		return s[0], true
	}
	sample := func(v, t float64) Sample {
		ms := math.Round(t * 1000)
		if v < 0 || v > 1 || ms <= 0 || ms > float64(now+5000) || ms > float64(MaxInt) {
			return Sample{}
		}
		return Sample{Ptr(v), Ptr(int64(ms))}
	}
	cpu, a := one(m.CPUUtil)
	ct, b := one(m.CPUTime)
	count, c := one(m.CPUCount)
	if a && b && c && count.Value >= 3 && count.Value <= math.MaxInt32 && count.Value == math.Trunc(count.Value) {
		n.CPU = sample(cpu.Value, ct.Value)
	}
	avail, a := one(m.MemAvail)
	mt, b := one(m.MemAvailTime)
	total, c := one(m.MemTotal)
	if a && b && c && avail.Value >= 0 && total.Value > 0 && avail.Value <= total.Value && total.Value <= 1<<50 {
		n.Memory = sample(1-avail.Value/total.Value, mt.Value)
	}
	up, ok := one(m.Up)
	if ok && (up.Value == 0 || up.Value == 1) {
		n.Exporter = sample(up.Value, float64(up.Time.UnixMilli())/1000)
	}
}

// ValidateEndpoints prevents credential-bearing URLs and arbitrary paths/query forms.
func ValidateEndpoints(api, prom string) bool {
	a, e := url.Parse(api)
	if e != nil || a.Scheme != "https" || a.Host == "" || a.User != nil || a.RawQuery != "" || a.Fragment != "" || a.Path != "" && a.Path != "/" {
		return false
	}
	p, e := url.Parse(prom)
	return e == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != "" && p.User == nil && p.RawQuery == "" && p.Fragment == "" && (p.Path == "" || p.Path == "/")
}
