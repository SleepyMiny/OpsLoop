package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessFaultIsExplicitBoundedAndSingleUse(t *testing.T) {
	now := time.Now()
	f := &readinessFault{uid: "pod-uid", ns: "opsloop-demo-a", token: strings.Repeat("a", 64), maxTTL: 120 * time.Second, now: func() time.Time { return now }}
	var ready atomic.Bool
	ready.Store(true)
	h := newHandlerWithFault(meta{PodUID: f.uid, Namespace: f.ns}, &ready, f)
	admin := newFaultAdminHandler(f)
	if serve(t, h, http.MethodPost, "/_opsloop/fault").Code != 404 {
		t.Fatal("fault endpoint exposed on the Service lane")
	}
	body := `{"podUID":"pod-uid","namespace":"opsloop-demo-a","durationSeconds":60}`
	post := func(token, payload string) int {
		r := httptest.NewRequest(http.MethodPost, "/_opsloop/fault", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-OpsLoop-Fault-Token", token)
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), f.token) {
			t.Fatal("secret reflected")
		}
		return w.Code
	}
	if post("wrong", body) != http.StatusForbidden || f.active() {
		t.Fatal("unauthorized injection changed readiness")
	}
	if post(f.token, strings.Replace(body, "pod-uid", "wrong", 1)) != http.StatusConflict || f.active() {
		t.Fatal("wrong UID changed readiness")
	}
	if post(f.token, strings.Replace(body, "60", "999999999", 1)) != http.StatusBadRequest || f.active() {
		t.Fatal("unbounded TTL accepted")
	}
	if post(f.token, body) != http.StatusAccepted || !f.active() {
		t.Fatal("authorized bounded fault was not applied")
	}
	until := f.until
	if post(f.token, body) != http.StatusConflict || !f.until.Equal(until) {
		t.Fatal("duplicate request extended/replayed the fault")
	}
	if serve(t, h, http.MethodGet, "/").Code != 503 || serve(t, h, http.MethodGet, "/ready").Code != 503 || serve(t, h, http.MethodGet, "/health").Code != 200 {
		t.Fatal("fault must affect Service/readiness while health stays live")
	}
	ready.Store(false) // Real shutdown begins while a fault remains active.
	now = now.Add(61 * time.Second)
	if f.active() || serve(t, h, http.MethodGet, "/ready").Code != 503 {
		t.Fatal("TTL expiry must not make a shutting-down Pod Ready")
	}
	if post(f.token, body) != http.StatusConflict {
		t.Fatal("expired fault was rearmed")
	}
}

func TestReadinessFaultEndpointAbsentByDefault(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	if serve(t, newHandler(meta{}, &ready), http.MethodPost, "/_opsloop/fault").Code != 404 {
		t.Fatal("default app exposes fault endpoint")
	}
	if got := serve(t, newHandler(meta{}, &ready), http.MethodGet, "/"); got.Code != 200 {
		t.Fatal("legacy normal path changed")
	}
}

func TestReadinessFaultConfigurationIsOptInAndPodBound(t *testing.T) {
	t.Setenv("OPSLOOP_ENABLE_READINESS_FAULT", "")
	f, err := faultFromEnvironment(meta{})
	if f != nil || err != nil {
		t.Fatal("default must be disabled")
	}
	t.Setenv("OPSLOOP_ENABLE_READINESS_FAULT", "true")
	t.Setenv("OPSLOOP_FAULT_TOKEN", strings.Repeat("b", 64))
	t.Setenv("OPSLOOP_FAULT_MAX_SECONDS", "120")
	t.Setenv("OPSLOOP_FAULT_NAMESPACE", "opsloop-demo-a")
	if _, err := faultFromEnvironment(meta{PodUID: "p", Namespace: "wrong"}); err == nil {
		t.Fatal("foreign namespace enabled")
	}
	if _, err := faultFromEnvironment(meta{Namespace: "opsloop-demo-a"}); err == nil {
		t.Fatal("missing Pod UID enabled")
	}
	if f, err := faultFromEnvironment(meta{PodUID: "p", Namespace: "opsloop-demo-a"}); err != nil || f == nil {
		t.Fatal("valid dedicated configuration rejected")
	}
}
