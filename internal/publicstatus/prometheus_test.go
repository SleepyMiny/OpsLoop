package publicstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type prometheusRoundTrip func(*http.Request) (*http.Response, error)

func (f prometheusRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCollectorPrometheusRejectsEveryRedirectBeforeDispatch(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for name, location := range map[string]string{
			"hostname":  "https://other.invalid/api/v1/query?query=PRIVATE_QUERY",
			"port":      "https://approved.invalid:444/api/v1/query",
			"downgrade": "http://approved.invalid/api/v1/query",
			"query":     "https://approved.invalid/api/v1/query?query=PRIVATE_QUERY",
			"path":      "https://approved.invalid/changed/api/v1/query",
			"relative":  "query?query=PRIVATE_QUERY",
			"loop":      "/api/v1/query",
			"missing":   "",
			"malformed": "http://[invalid",
		} {
			t.Run(fmt.Sprintf("%d/%s", status, name), func(t *testing.T) {
				calls := 0
				api, err := NewPrometheusAPI("https://approved.invalid", prometheusRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					h := make(http.Header)
					if location != "" {
						h.Set("Location", location)
					}
					// A success-shaped body must never turn a 3xx into a valid observation.
					return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[]}}`)), Request: r}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = api.Query(context.Background(), "up", observedTime)
				if err == nil || calls != 1 {
					t.Fatalf("error=%v dispatches=%d; want error and zero follow-up dispatch", err, calls)
				}
				if location != "" && name != "malformed" && !errors.Is(err, errPrometheusRedirect) {
					t.Fatalf("missing explicit rejection: %v", err)
				}
			})
		}
	}
}

func TestCollectorPrometheusContextAndTimeout(t *testing.T) {
	for _, useDeadline := range []bool{true, false} {
		t.Run(fmt.Sprint(useDeadline), func(t *testing.T) {
			api, err := NewPrometheusAPI("https://approved.invalid", prometheusRoundTrip(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if useDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			_, _, err = api.Query(ctx, "up", observedTime)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 8*time.Second {
				t.Fatalf("timeout/context lost: %v", err)
			}
		})
	}
}

func TestCollectorRedirectFailureClearsPreviousObservationAndHidesDetails(t *testing.T) {
	c, _ := testCollector(t)
	reject := false
	api, err := NewPrometheusAPI("https://approved.invalid", prometheusRoundTrip(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		status := 200
		q := r.URL.Query().Get("query")
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
		var results []any
		for i := 1; i <= 3; i++ {
			results = append(results, map[string]any{"metric": map[string]string{"instance": fmt.Sprintf("192.0.2.%d:9100", i)}, "value": []any{float64(observedTime.UnixMilli()) / 1000, strconv.FormatFloat(value, 'f', -1, 64)}})
		}
		raw, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": results}})
		body := string(raw)
		if reject {
			status = 302
			h.Set("Location", "https://PRIVATE_HOST.invalid/api/v1/query?query=PRIVATE_QUERY")
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	c.Prometheus = api
	first := c.Collect(context.Background())
	if first.Sources.Prometheus.State != "ok" || first.Nodes[0].CPU.Value == nil {
		t.Fatal("normal query failed")
	}
	reject = true
	second := c.Collect(context.Background())
	if second.Sources.Prometheus.State != "unavailable" || second.Sources.Prometheus.ObservedAt != nil {
		t.Fatal("redirect represented as fresh observation")
	}
	for _, n := range second.Nodes {
		if n.CPU.Value != nil || n.Memory.Value != nil || n.Exporter.Value != nil {
			t.Fatal("previous values retained")
		}
	}
	raw, err := second.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PRIVATE_HOST", "PRIVATE_QUERY", "redirect", "approved.invalid"} {
		if strings.Contains(string(raw), marker) {
			t.Fatal("diagnostic leaked")
		}
	}
}
