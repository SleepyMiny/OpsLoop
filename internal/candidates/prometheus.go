package candidates

import (
	"context"
	"fmt"
	"math"
	"net"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const QueryVersion = "node-exporter-v1"

const (
	queryTimeout  = 5 * time.Second
	minCPUSamples = 3
	instanceLabel = "instance"
	idleSelector  = `{job="node-exporter",mode="idle"}`
)

type Sample struct {
	Value  float64
	Time   time.Time
	Labels map[string]string
}

type Metrics struct {
	ObservedAt time.Time
	// instance host (IP) -> samples per query; a slice so duplicate series are detectable.
	CPUUtil, CPUTime, CPUCount, MemAvail, MemAvailTime, MemTotal, Up map[string][]Sample
}

// QueryMetrics runs every node-exporter query at now; window is the CPU rate window W (2 x maxSampleAge).
// Any HTTP error, timeout or non-vector result fails the whole snapshot.
func QueryMetrics(ctx context.Context, api promv1.API, now time.Time, window time.Duration) (Metrics, error) {
	w := fmt.Sprintf("%ds", int64(window/time.Second))
	m := Metrics{ObservedAt: now}
	queries := []struct {
		dst  *map[string][]Sample
		expr string
	}{
		{&m.CPUUtil, `1 - avg by (instance) (rate(node_cpu_seconds_total` + idleSelector + `[` + w + `]))`},
		{&m.CPUTime, `min by (instance) (timestamp(node_cpu_seconds_total` + idleSelector + `))`},
		{&m.CPUCount, `min by (instance) (count_over_time(node_cpu_seconds_total` + idleSelector + `[` + w + `]))`},
		{&m.MemAvail, `node_memory_MemAvailable_bytes{job="node-exporter"}`},
		{&m.MemAvailTime, `timestamp(node_memory_MemAvailable_bytes{job="node-exporter"})`},
		{&m.MemTotal, `node_memory_MemTotal_bytes{job="node-exporter"}`},
		{&m.Up, `up{job="node-exporter"}`},
	}
	for _, q := range queries {
		res, err := query(ctx, api, q.expr, now)
		if err != nil {
			return Metrics{}, err
		}
		*q.dst = res
	}
	return m, nil
}

func query(ctx context.Context, api promv1.API, expr string, now time.Time) (map[string][]Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	v, _, err := api.Query(ctx, expr, now)
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", expr, err)
	}
	vec, ok := v.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: result %T, want vector", expr, v)
	}
	out := map[string][]Sample{}
	for _, s := range vec {
		inst := string(s.Metric[instanceLabel])
		host, _, err := net.SplitHostPort(inst)
		if err != nil {
			host = inst
		}
		labels := make(map[string]string, len(s.Metric))
		for k, val := range s.Metric {
			labels[string(k)] = string(val)
		}
		out[host] = append(out[host], Sample{Value: float64(s.Value), Time: s.Timestamp.Time(), Labels: labels})
	}
	return out, nil
}

// assessMetrics maps the Node to its exporter by InternalIP, records every usable value in meas and
// reports METRICS_MISSING / METRICS_STALE conditions; missing values stay nil, never zero.
func assessMetrics(m Metrics, node *corev1.Node, now time.Time, maxAge time.Duration, meas *v1alpha1.Measurements) (missing, stale bool, details []string) {
	var ips []string
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ips = append(ips, a.Address)
		}
	}
	if len(ips) != 1 {
		return true, false, []string{"node identity ambiguous"}
	}
	ip := ips[0]

	one := func(name string, series map[string][]Sample) (float64, bool) {
		s := series[ip]
		switch {
		case len(s) == 0:
			details = append(details, name+" series missing")
		case len(s) > 1:
			details = append(details, name+" duplicate series")
		case math.IsNaN(s[0].Value) || math.IsInf(s[0].Value, 0):
			details = append(details, name+" value not finite")
		default:
			return s[0].Value, true
		}
		missing = true
		return 0, false
	}
	fresh := func(name string, ts float64) *metav1.Time {
		// timestamp() returns seconds with millisecond precision.
		t := metav1.NewTime(time.UnixMilli(int64(math.Round(ts * 1000))).UTC())
		if age := now.Sub(t.Time); age > maxAge {
			stale = true
			details = append(details, fmt.Sprintf("%s sample age %s", name, age))
		}
		return &t
	}

	if v, ok := one("cpu utilization", m.CPUUtil); ok {
		p := int32(math.Round(math.Min(math.Max(v, 0), 1) * 1000))
		meas.CPUUtilizationPermille = &p
	}
	if v, ok := one("cpu sample count", m.CPUCount); ok {
		c := int32(v)
		meas.CPUSampleCount = &c
		if v < minCPUSamples {
			missing = true
			details = append(details, fmt.Sprintf("cpu sample count %d below %d", c, minCPUSamples))
		}
	}
	if v, ok := one("cpu sample time", m.CPUTime); ok {
		meas.CPUSampleTime = fresh("cpu", v)
	}
	if v, ok := one("memory available", m.MemAvail); ok {
		b := int64(v)
		meas.MemAvailableBytes = &b
	}
	if v, ok := one("memory total", m.MemTotal); ok {
		if v <= 0 {
			missing = true
			details = append(details, "memory total not positive")
		} else {
			b := int64(v)
			meas.MemTotalBytes = &b
		}
	}
	if v, ok := one("memory sample time", m.MemAvailTime); ok {
		meas.MemSampleTime = fresh("memory", v)
	}
	if v, ok := one("up", m.Up); ok && v == 0 {
		stale = true
		details = append(details, "exporter target down")
	}
	return missing, stale, details
}
