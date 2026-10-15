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
