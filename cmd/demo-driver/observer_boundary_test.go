package main

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type observerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f observerRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestObserverTransportBoundary(t *testing.T) {
	calls := 0
	guard := &observerGuard{origin: "https://api.invalid", allowed: map[string]bool{"/api/v1/namespaces/demo/services/app": true}, bearer: "synthetic-token", base: observerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("API authentication missing")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})}
	approved := "https://api.invalid/api/v1/namespaces/demo/services/app"
	for _, tc := range []struct {
		name, method, url, host, auth string
		body                          bool
	}{
		{name: "post", method: "POST", url: approved}, {name: "delete", method: "DELETE", url: approved}, {name: "patch", method: "PATCH", url: approved}, {name: "put", method: "PUT", url: approved},
		{name: "host", method: "GET", url: "https://elsewhere.invalid/api/v1/namespaces/demo/services/app"}, {name: "path", method: "GET", url: "https://api.invalid/api/v1/secrets"}, {name: "exec", method: "GET", url: approved + "/exec"}, {name: "logs", method: "GET", url: approved + "/log"}, {name: "proxy", method: "GET", url: approved + "/proxy"}, {name: "token", method: "GET", url: approved + "/token"}, {name: "query", method: "GET", url: approved + "?watch=true"}, {name: "userinfo", method: "GET", url: "https://user:pass@api.invalid/api/v1/namespaces/demo/services/app"}, {name: "host-header", method: "GET", url: approved, host: "elsewhere.invalid"}, {name: "caller-auth", method: "GET", url: approved, auth: "credential"}, {name: "request-body", method: "GET", url: approved, body: true}, {name: "encoded-path", method: "GET", url: "https://api.invalid/api/v1/namespaces/demo/services/%61pp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body {
				body = strings.NewReader("payload")
			}
			r, _ := http.NewRequest(tc.method, tc.url, body)
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if _, e := guard.RoundTrip(r); e == nil {
				t.Fatal("boundary accepted")
			}
		})
	}
	if calls != 0 {
		t.Fatal("blocked request reached transport")
	}
	r, _ := http.NewRequest("GET", approved, nil)
	resp, e := guard.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestObserverRedirectTLSLimits(t *testing.T) {
	followups := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followups++ }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", target.URL)
			w.WriteHeader(302)
		case "/huge":
			io.WriteString(w, strings.Repeat("x", maxBodyBytes+1))
		case "/slow":
			<-r.Context().Done()
		case "/badstatus":
			w.WriteHeader(403)
			io.WriteString(w, "secret-error-body")
		}
	}))
	defer server.Close()
	c, g, e := observerClient(server.URL, "", "", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	g.allowed["/redirect"] = true
	if _, _, problem := observerGET(context.Background(), c, server.URL+"/redirect", maxBodyBytes); problem != "transport-measurement-path" {
		t.Fatal("untrusted TLS accepted", problem)
	}
	// Trust the test CA explicitly, never use InsecureSkipVerify.
	ca := filepath.Join(t.TempDir(), "ca.pem")
	writeObserverTestCA(t, ca, server)
	c, g, e = observerClient(server.URL, ca, "", 200*time.Millisecond)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ path, problem string }{{"/redirect", "redirect-rejected"}, {"/huge", "body-limit"}, {"/slow", "timeout-measurement-path"}, {"/badstatus", "http-403"}} {
		g.allowed[tc.path] = true
		b, _, problem := observerGET(context.Background(), c, server.URL+tc.path, maxBodyBytes)
		if problem != tc.problem || b != nil {
			t.Fatalf("%s: %s %q", tc.path, problem, b)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, problem := observerGET(ctx, c, server.URL+"/redirect", maxBodyBytes); problem != "cancelled" {
		t.Fatal(problem)
	}
	if followups != 0 {
		t.Fatal("redirect followed")
	}
}
func TestObserverConfigAndAmbientCredentials(t *testing.T) {
	s := newObserverScene(t, "normal")
	for _, tc := range []struct {
		name   string
		mutate func(*ObserverConfig)
	}{
		{"http-api", func(c *ObserverConfig) { c.APIOrigin = "http://api.invalid" }}, {"api-path", func(c *ObserverConfig) { c.APIOrigin += "/api" }}, {"userinfo", func(c *ObserverConfig) { c.ServiceURL = "https://token@example.invalid/path" }}, {"query", func(c *ObserverConfig) { c.ServiceURL += "?token=secret" }}, {"traversal", func(c *ObserverConfig) { c.ServiceURL += "/../secret" }}, {"unsafe-name", func(c *ObserverConfig) { c.Incident = "../secrets" }}, {"same-authority", func(c *ObserverConfig) { c.ServiceURL = c.APIOrigin + "/identity" }}, {"unbounded", func(c *ObserverConfig) { c.TimeoutMS = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := s.config
			tc.mutate(&c)
			if c.validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "must-not-read"))
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:8888")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:8888")
	cfg := s.config
	cfg.BearerFile = ""
	b, e := observerLocalFile(s.configPath, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{`"kubeconfig":"ambient"`, `"exec":{"command":"sh"}`, `"insecureSkipTLSVerify":true`, `"proxyURL":"http://proxy.invalid"`} {
		p := filepath.Join(t.TempDir(), "bad.json")
		text := strings.TrimSuffix(string(b), "}") + "," + field + "}"
		os.WriteFile(p, []byte(text), 0600)
		if e := runObserve([]string{"-config", p}, io.Discard, s.clock); e == nil {
			t.Fatal("unsupported auth option accepted")
		}
	}
	// Proxy is nil and no ambient credential is discovered even when present.
	client, guard, e := observerClient(cfg.APIOrigin, cfg.APICAFile, "", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()
	if guard.bearer != "" || guard.base.(*http.Transport).Proxy != nil {
		t.Fatal("ambient auth/proxy used")
	}
	cfg = s.config
	cfg.ServiceURL += "/synthetic-api-credential-DO-NOT-EXPORT"
	if e := collectObserver(context.Background(), cfg, io.Discard, s.clock); e == nil || !strings.Contains(e.Error(), "credential in configuration") {
		t.Fatal("credential target URL accepted", e)
	}
}

type observerFailSink struct{ *os.File }

func (f observerFailSink) Write(b []byte) (int, error) {
	n, _ := f.File.Write(b[:len(b)/2])
	return n, errors.New("injected disk failure")
}
func TestObserverRecorderFailures(t *testing.T) {
	c := ObserverConfig{RunID: "storage", Source: "test", EvidenceKind: "synthetic", OutputRoot: t.TempDir()}
	r, e := newObserverRecorder(c, observerWallClock{})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.write("existing.json", []byte("original")); e != nil {
		t.Fatal(e)
	}
	if e = r.write("existing.json", []byte("overwrite")); e == nil {
		t.Fatal("overwrote existing evidence")
	}
	r.open = func(p string) (observerSink, error) {
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		return observerFailSink{f}, e
	}
	if e = r.write("partial.json", []byte("partial-evidence")); e == nil {
		t.Fatal("write failure lost")
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "partial.json"))
	if string(b) != "partial-" {
		t.Fatalf("partial bytes not preserved %q", b)
	}
	r.open = nil
	if e = r.finish(false); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(r.dir, "COMPLETE")); !os.IsNotExist(e) {
		t.Fatal("completion marker for failed write")
	}
	if e = r.finish(true); e == nil {
		t.Fatal("duplicate finalize allowed")
	}
	if _, e = newObserverRecorder(c, observerWallClock{}); e == nil {
		t.Fatal("run overwritten")
	}
	c.RunID = "storage-new-segment"
	r2, e := newObserverRecorder(c, observerWallClock{})
	if e != nil {
		t.Fatal(e)
	}
	if r2.manifest.Segment == r.manifest.Segment {
		t.Fatal("segment reused")
	}
	if e = r2.finish(true); e != nil {
		t.Fatal(e)
	}
	if e = r2.finish(true); e == nil {
		t.Fatal("double completion")
	}
}

type observerCancelClock struct {
	*observerTestClock
	cancel context.CancelFunc
}

func (c observerCancelClock) Sleep(ctx context.Context, d time.Duration) error {
	if c.seconds() > 55 {
		c.cancel()
		return ctx.Err()
	}
	return c.observerTestClock.Sleep(ctx, d)
}
func TestObserverInterruptionAndFinalizeCollision(t *testing.T) {
	s := newObserverScene(t, "normal")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if e := collectObserver(ctx, s.config, io.Discard, observerCancelClock{s.clock, cancel}); e == nil {
		t.Fatal("interruption accepted")
	}
	dir := filepath.Join(s.config.OutputRoot, s.config.RunID)
	for _, name := range []string{"COMPLETE", "summary.json"} {
		if _, e := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
			t.Fatal("interrupted run published", name)
		}
	}
	var m observerManifest
	if e := readJSON(filepath.Join(dir, "manifest.json"), &m); e != nil || m.State != "incomplete" {
		t.Fatal(m, e)
	}
	r, e := newObserverRecorder(ObserverConfig{RunID: "collision", OutputRoot: t.TempDir()}, s.clock)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.write("probe.jsonl", []byte("prior-partial")); e != nil {
		t.Fatal(e)
	}
	o := &observer{r: r, c: s.config}
	if e = o.finalize(io.Discard); e == nil {
		t.Fatal("partial file overwritten")
	}
	if _, e = os.Stat(filepath.Join(r.dir, "COMPLETE")); !os.IsNotExist(e) {
		t.Fatal("failed finalize marker")
	}
}

func writeObserverTestCA(t *testing.T, path string, s *httptest.Server) {
	t.Helper()
	if e := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
}

func TestObserverMeasurementFailureIsNotOutage(t *testing.T) {
	for _, status := range []int{200, 503} {
		clock := &observerTestClock{t: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)}
		clock.start = clock.t
		r, e := newObserverRecorder(ObserverConfig{RunID: "measurement", OutputRoot: t.TempDir()}, clock)
		if e != nil {
			t.Fatal(e)
		}
		o := &observer{r: r, c: ObserverConfig{ServiceURL: "http://service.invalid/identity", PathID: "approved"}, service: &http.Client{Transport: observerRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))}, nil
		})}}
		if e = o.probe(context.Background()); e != nil {
			t.Fatal(e)
		}
		if o.evidence.Probe[0].Result != "recorder-error" || o.probes[0].Error != "body-limit" {
			t.Fatal("measurement failure became an outage", o.evidence.Probe, o.probes)
		}
	}
}
