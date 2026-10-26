package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeDialsPerRequest(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"node":"n","pod":"p","podUID":"u","arch":"amd64","version":"v"}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	client := newProbeClient(time.Second)
	for range 3 {
		if rec := probeOnce(context.Background(), client, srv.URL, time.Now()); !rec.OK {
			t.Fatalf("probe failed: %+v", rec)
		}
	}
	if n := conns.Load(); n != 3 {
		t.Fatalf("connections = %d, want 3 (one per request so the Service re-selects an endpoint)", n)
	}
}

func TestProbeRejectsRedirect(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.Write([]byte(`{"podUID":"new"}`)) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	rec := probeOnce(context.Background(), newProbeClient(time.Second), source.URL, time.Now())
	if rec.OK || rec.Status != 302 || calls.Load() != 0 {
		t.Fatalf("redirect followed or accepted: %+v calls=%d", rec, calls.Load())
	}
}
