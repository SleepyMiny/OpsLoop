package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func serve(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestRootReportsMetadata(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(meta{
		Service: "demo-api", Version: "abc", Node: "n1", Pod: "p1", PodUID: "u1",
		Namespace: "ns", OS: runtime.GOOS, Arch: runtime.GOARCH, StartedAt: "2026-01-01T00:00:00Z",
	}, &ready)

	rec := serve(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"service", "version", "node", "pod", "podUID", "namespace", "os", "arch", "startedAt"} {
		if got[k] == "" {
			t.Errorf("field %q missing or empty in %v", k, got)
		}
	}
	if len(got) != 9 {
		t.Errorf("want 9 fields, got %d: %v", len(got), got)
	}
	if got["arch"] != runtime.GOARCH {
		t.Errorf("arch=%q want %q", got["arch"], runtime.GOARCH)
	}
}

func TestReadyTransitionsToShuttingDown(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	h := newHandler(meta{}, &ready)

	if rec := serve(t, h, http.MethodGet, "/ready"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ready"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ready.Store(false)
	if rec := serve(t, h, http.MethodGet, "/ready"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting-down") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := serve(t, h, http.MethodGet, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("health must stay 200 while shutting down, got %d", rec.Code)
	}
}

func TestMetricsExposed(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(meta{}, &ready)
	serve(t, h, http.MethodGet, "/health")
	body := serve(t, h, http.MethodGet, "/metrics").Body.String()
	for _, want := range []string{"demo_api_build_info", `demo_api_http_requests_total{code="200",handler="/health",method="get"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	var ready atomic.Bool
	h := newHandler(meta{}, &ready)
	if rec := serve(t, h, http.MethodGet, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path code=%d", rec.Code)
	}
	if rec := serve(t, h, http.MethodPost, "/health"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code=%d", rec.Code)
	}
}
