package candidates

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

const maxAge = 90 * time.Second

func unix(t time.Time) string { return fmt.Sprintf("%.3f", float64(t.UnixMilli())/1000) }

func series(instance, value string) string {
	return fmt.Sprintf(`{"metric":{"instance":%q,"job":"node-exporter"},"value":[%s,%q]}`, instance, unix(now), value)
}

// promData holds per-query series JSON, keyed like the Metrics fields.
type promData map[string][]string

func goodData(instances ...string) promData {
	d := promData{}
	for _, inst := range instances {
		d["CPUUtil"] = append(d["CPUUtil"], series(inst, "0.2"))
		d["CPUTime"] = append(d["CPUTime"], series(inst, unix(now.Add(-10*time.Second))))
		d["CPUCount"] = append(d["CPUCount"], series(inst, "6"))
		d["MemAvail"] = append(d["MemAvail"], series(inst, "6442450944"))
		d["MemAvailTime"] = append(d["MemAvailTime"], series(inst, unix(now.Add(-10*time.Second))))
		d["MemTotal"] = append(d["MemTotal"], series(inst, "10737418240"))
		d["Up"] = append(d["Up"], series(inst, "1"))
	}
	return d
}

func queryKey(q string) string {
	switch {
	case strings.Contains(q, "rate("):
		return "CPUUtil"
	case strings.Contains(q, "timestamp(node_cpu"):
		return "CPUTime"
	case strings.Contains(q, "count_over_time"):
		return "CPUCount"
	case strings.Contains(q, "timestamp(node_memory"):
		return "MemAvailTime"
	case strings.Contains(q, "MemAvailable"):
		return "MemAvail"
	case strings.Contains(q, "MemTotal"):
		return "MemTotal"
	case strings.HasPrefix(q, "up{"):
		return "Up"
	}
	return ""
}

func fakeProm(t *testing.T, h http.HandlerFunc) promv1.API {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := promapi.NewClient(promapi.Config{Address: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return promv1.NewAPI(c)
}

func serve(t *testing.T, d promData, seen *[]string) promv1.API {
	return fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if seen != nil {
			*seen = append(*seen, q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(d[queryKey(q)], ","))
	})
}

func ipNode(ips ...string) *corev1.Node {
	n := &corev1.Node{}
	for _, ip := range ips {
		n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeHostName, Address: "host"})
	return n
}

func assess(t *testing.T, d promData, n *corev1.Node) (bool, bool, v1alpha1.Measurements) {
	t.Helper()
	m, err := QueryMetrics(context.Background(), serve(t, d, nil), now, 2*maxAge)
	if err != nil {
		t.Fatal(err)
	}
	var meas v1alpha1.Measurements
	missing, stale, details := assessMetrics(m, n, now, maxAge, &meas)
	t.Logf("details: %v", details)
	return missing, stale, meas
}

func TestQueryMetricsParses(t *testing.T) {
	var seen []string
	m, err := QueryMetrics(context.Background(), serve(t, goodData("10.0.0.1:9100"), &seen), now, 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 7 || !strings.Contains(seen[0], "[180s]") {
		t.Fatalf("queries: %v", seen)
	}
	if !m.ObservedAt.Equal(now) || len(m.CPUUtil["10.0.0.1"]) != 1 || m.CPUUtil["10.0.0.1"][0].Value != 0.2 {
		t.Fatalf("metrics: %+v", m)
	}
	var meas v1alpha1.Measurements
	missing, stale, _ := assessMetrics(m, ipNode("10.0.0.1"), now, maxAge, &meas)
	if missing || stale || *meas.CPUUtilizationPermille != 200 || *meas.CPUSampleCount != 6 ||
		*meas.MemAvailableBytes != 6442450944 || *meas.MemTotalBytes != 10737418240 ||
		!meas.CPUSampleTime.Time.Equal(now.Add(-10*time.Second)) {
		t.Fatalf("meas: %+v", meas)
	}
}

func TestQueryMetricsErrors(t *testing.T) {
	t.Run("http 500", func(t *testing.T) {
		api := fakeProm(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
		if _, err := QueryMetrics(context.Background(), api, now, 2*maxAge); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		api := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm() // drain the body so the server notices the client hanging up
			select {
			case <-time.After(6 * time.Second):
			case <-r.Context().Done():
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := QueryMetrics(ctx, api, now, 2*maxAge); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("not a vector", func(t *testing.T) {
		api := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"scalar","result":[%s,"1"]}}`, unix(now))
		})
		if _, err := QueryMetrics(context.Background(), api, now, 2*maxAge); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestAssessMetrics(t *testing.T) {
	const inst = "10.0.0.1:9100"
	cases := []struct {
		name           string
		mutate         func(promData)
		node           *corev1.Node
		missing, stale bool
	}{
		{"healthy", func(promData) {}, nil, false, false},
		{"no series", func(d promData) { d["MemTotal"] = nil }, nil, true, false},
		{"NaN value", func(d promData) { d["CPUUtil"] = []string{series(inst, "NaN")} }, nil, true, false},
		{"cpu sample older than max age", func(d promData) {
			d["CPUTime"] = []string{series(inst, unix(now.Add(-maxAge-time.Second)))}
		}, nil, false, true},
		{"memory sample exactly max age", func(d promData) {
			d["MemAvailTime"] = []string{series(inst, unix(now.Add(-maxAge)))}
			d["CPUTime"] = []string{series(inst, unix(now.Add(-maxAge)))}
		}, nil, false, false},
		{"up 0", func(d promData) { d["Up"] = []string{series(inst, "0")} }, nil, false, true},
		{"duplicate series", func(d promData) {
			d["MemAvail"] = append(d["MemAvail"], strings.Replace(series(inst, "1"), `"job"`, `"pod":"x","job"`, 1))
		}, nil, true, false},
		{"cpu count 2", func(d promData) { d["CPUCount"] = []string{series(inst, "2")} }, nil, true, false},
		{"memtotal zero", func(d promData) { d["MemTotal"] = []string{series(inst, "0")} }, nil, true, false},
		{"memavail above memtotal", func(d promData) { d["MemAvail"] = []string{series(inst, "10737418241")} }, nil, true, false},
		{"memavail negative", func(d promData) { d["MemAvail"] = []string{series(inst, "-1")} }, nil, true, false},
		{"memtotal beyond range", func(d promData) { d["MemTotal"] = []string{series(inst, "1e30")} }, nil, true, false},
		{"cpu count beyond int32", func(d promData) { d["CPUCount"] = []string{series(inst, "1e12")} }, nil, true, false},
		{"sample time beyond int64", func(d promData) { d["CPUTime"] = []string{series(inst, "1e300")} }, nil, true, false},
		{"sample time beyond year 9999", func(d promData) { d["CPUTime"] = []string{series(inst, "1e12")} }, nil, true, false},
		{"sample time beyond year 9999 after rounding", func(d promData) { d["CPUTime"] = []string{series(inst, "253402300799.9999")} }, nil, true, false},
		{"sample time negative", func(d promData) { d["MemAvailTime"] = []string{series(inst, "-1")} }, nil, true, false},
		{"cpu sample 6s in future", func(d promData) {
			d["CPUTime"] = []string{series(inst, unix(now.Add(6*time.Second)))}
		}, nil, false, true},
		{"memory sample 5s in future", func(d promData) {
			d["MemAvailTime"] = []string{series(inst, unix(now.Add(5*time.Second)))}
		}, nil, false, false},
		{"other instance only", func(promData) {}, ipNode("10.0.0.2"), true, false},
		{"two internal IPs", func(promData) {}, ipNode("10.0.0.1", "10.0.0.2"), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodData(inst)
			c.mutate(d)
			n := c.node
			if n == nil {
				n = ipNode("10.0.0.1")
			}
			missing, stale, meas := assess(t, d, n)
			if missing != c.missing || stale != c.stale {
				t.Fatalf("missing=%v stale=%v, want %v %v", missing, stale, c.missing, c.stale)
			}
			if c.name == "up 0" && meas.CPUUtilizationPermille == nil {
				t.Fatal("stale values must still be recorded")
			}
			if c.name == "NaN value" && meas.CPUUtilizationPermille != nil {
				t.Fatal("NaN must not be recorded")
			}
			if (c.name == "memavail negative" && meas.MemAvailableBytes != nil) ||
				(c.name == "memtotal beyond range" && meas.MemTotalBytes != nil) ||
				(c.name == "cpu count beyond int32" && meas.CPUSampleCount != nil) ||
				(strings.HasPrefix(c.name, "sample time beyond") && meas.CPUSampleTime != nil) {
				t.Fatal("out-of-range values must not be recorded")
			}
		})
	}
}

func TestCPUUtilClamped(t *testing.T) {
	d := goodData("10.0.0.1:9100")
	d["CPUUtil"] = []string{series("10.0.0.1:9100", "1.02")}
	_, _, meas := assess(t, d, ipNode("10.0.0.1"))
	if *meas.CPUUtilizationPermille != 1000 {
		t.Fatalf("got %d", *meas.CPUUtilizationPermille)
	}
}
