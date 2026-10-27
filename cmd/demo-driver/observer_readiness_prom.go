package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Prometheus queries are read operations even when the client uses the POST
// form of /api/v1/query. Reject arbitrary expressions, paths and credentials.
type readinessPromGuard struct {
	origin  string
	queries map[string]bool
	at      time.Time
	base    http.RoundTripper
}

func (g *readinessPromGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != g.origin || r.URL.Path != "/api/v1/query" || r.URL.RawPath != "" || r.URL.User != nil || r.URL.Fragment != "" || (r.Method != http.MethodGet && r.Method != http.MethodPost) || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
		return nil, errors.New("observer: Prometheus request outside read-only scope")
	}
	q := r.URL.Query()
	clone := r.Clone(r.Context())
	if r.Method == http.MethodPost {
		if r.URL.RawQuery != "" || r.Body == nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			return nil, errors.New("observer: Prometheus form required")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		r.Body.Close()
		if err != nil || len(body) > 4096 {
			return nil, errors.New("observer: Prometheus form size limit")
		}
		q, err = url.ParseQuery(string(body))
		if err != nil {
			return nil, errors.New("observer: invalid Prometheus form")
		}
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
	}
	for key, values := range q {
		if (key != "query" && key != "time") || len(values) != 1 {
			return nil, errors.New("observer: extra Prometheus query parameter")
		}
	}
	if !g.queries[q.Get("query")] {
		return nil, errors.New("observer: PromQL outside production candidate queries")
	}
	ts, err := strconv.ParseFloat(q.Get("time"), 64)
	if err != nil || !(ts > 0) || math.IsNaN(ts) || math.IsInf(ts, 0) || math.Abs(ts-float64(g.at.UnixNano())/1e9) > 0.001 {
		return nil, errors.New("observer: unbounded Prometheus query time")
	}
	return g.base.RoundTrip(clone)
}
func readinessPromQueries(window time.Duration) map[string]bool {
	selector := `{job="node-exporter",mode="idle"}`
	w := fmt.Sprintf("%ds", int64(window/time.Second))
	return map[string]bool{
		`1 - avg by (instance) (rate(node_cpu_seconds_total` + selector + `[` + w + `]))`:        true,
		`min by (instance) (timestamp(node_cpu_seconds_total` + selector + `))`:                  true,
		`min by (instance) (count_over_time(node_cpu_seconds_total` + selector + `[` + w + `]))`: true,
		`node_memory_MemAvailable_bytes{job="node-exporter"}`:                                    true,
		`timestamp(node_memory_MemAvailable_bytes{job="node-exporter"})`:                         true,
		`node_memory_MemTotal_bytes{job="node-exporter"}`:                                        true,
		`up{job="node-exporter"}`: true,
	}
}
func (o *observer) readinessPodRequests(ctx context.Context, node string) ([]core.Pod, error) {
	path := "/api/v1/pods?" + url.Values{"fieldSelector": {"spec.nodeName=" + node}, "limit": {"500"}}.Encode()
	o.apiGuard.allowed[path] = true
	body, status, problem := observerGET(ctx, o.api, o.c.APIOrigin+path, 16<<20)
	at, err := o.r.stamp(ctx)
	if err != nil {
		return nil, err
	}
	var list core.PodList
	if problem != "" || json.Unmarshal(body, &list) != nil || list.Continue != "" || len(list.Items) > 500 {
		return nil, errors.New("observer: candidate Pod requests inventory incomplete")
	}
	projection := []any{}
	for _, pod := range list.Items {
		if pod.Spec.NodeName != node {
			return nil, errors.New("observer: candidate Pod list scope mismatch")
		}
		// Do not silently apply the current engine's documented shortcuts to Pods
		// whose native sidecar/overhead/Pod budget would undercount Scheduler usage.
		if len(pod.Spec.Overhead) != 0 || pod.Spec.Resources != nil {
			return nil, errors.New("observer: unsupported overhead/Pod resource budget in candidate inventory")
		}
		for _, c := range pod.Spec.InitContainers {
			if c.RestartPolicy != nil {
				return nil, errors.New("observer: native sidecar resource accounting unsupported")
			}
		}
		cpu, memory := candidates.PodRequests(pod.Spec)
		projection = append(projection, struct {
			Metadata    meta.ObjectMeta `json:"metadata"`
			Node        string          `json:"node"`
			Phase       core.PodPhase   `json:"phase"`
			CPUMilli    int64           `json:"cpuMilli"`
			MemoryBytes int64           `json:"memoryBytes"`
		}{projectionMeta(pod.ObjectMeta), node, pod.Status.Phase, cpu, memory})
	}
	if _, err = o.r.raw("readiness-candidate-requests", path, at, status, "", projection); err != nil {
		return nil, err
	}
	return list.Items, nil
}
func (o *observer) readinessPreview(ctx context.Context) error {
	cfg := o.c.Readiness
	policy := o.lastPolicy
	state := o.readiness
	var nodes []core.Node
	pods := map[string][]core.Pod{}
	for _, target := range cfg.Candidates {
		node := state.nodes[target.Name]
		selector, err := meta.LabelSelectorAsSelector(&policy.Spec.CandidateSelector)
		if err != nil {
			return errors.New("observer: invalid candidate selector")
		}
		if !selector.Matches(labels.Set(node.Labels)) {
			return errors.New("observer: pinned candidate no longer satisfies Policy selector")
		}
		nodes = append(nodes, node)
		items, err := o.readinessPodRequests(ctx, target.Name)
		if err != nil {
			return err
		}
		pods[target.Name] = items
	}
	origin := cfg.PrometheusURL
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, ResponseHeaderTimeout: o.c.timeoutDuration()}
	defer transport.CloseIdleConnections()
	queryAt := o.r.clock.Now()
	base := http.RoundTripper(transport)
	if state.promBase != nil {
		base = state.promBase
	}
	guard := &readinessPromGuard{origin: origin, queries: readinessPromQueries(2 * policy.Spec.Filter.MaxSampleAge.Duration), at: queryAt, base: base}
	pc, err := promapi.NewClient(promapi.Config{Address: origin, RoundTripper: guard})
	if err != nil {
		return errors.New("observer: Prometheus client unavailable")
	}
	before, err := o.r.stamp(ctx)
	if err != nil {
		return err
	}
	metrics, merr := candidates.QueryMetrics(ctx, promv1.NewAPI(pc), queryAt, 2*policy.Spec.Filter.MaxSampleAge.Duration)
	after, err := o.r.stamp(ctx)
	if err != nil {
		return err
	}
	if merr != nil {
		_, _ = o.r.raw("readiness-prometheus", origin, after, 0, "production-metrics-query-unavailable", nil)
		return errors.New("observer: actual Prometheus candidate queries failed")
	}
	// Evaluate is the production implementation. This is explicitly a read-only
	// preview, not an Incident or an action authority and never a fake Controller.
	decision := candidates.Evaluate(candidates.Input{Now: o.r.clock.Now(), Spec: policy.Spec, RecentFailures: policy.Status.RecentFailures, PolicyHash: v1.PolicyHash(policy.Spec), PolicyGeneration: policy.Generation, Deployment: state.deployment, Nodes: nodes, PodsByNode: pods, Metrics: metrics, ImageDigest: o.c.ImageRef, BaselineTemplateHash: v1.TemplateHash(state.deployment.Spec.Template)})
	if _, err = o.r.raw("readiness-prometheus-candidate-preview", origin, after, 200, "", struct {
		Before  Stamp               `json:"before"`
		After   Stamp               `json:"after"`
		Metrics candidates.Metrics  `json:"metrics"`
		Preview v1.RecoveryDecision `json:"previewNotControllerDecision"`
	}{before, after, metrics, decision}); err != nil {
		return err
	}
	state.preview = &decision
	return nil
}
