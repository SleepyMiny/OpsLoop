// Command demo-api is the placement-observable workload that OpsLoop recovers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is injected with -ldflags "-X main.version=<sha>".
var version = "dev"

var requests = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "demo_api_http_requests_total",
	Help: "HTTP requests by handler, status code and method.",
}, []string{"handler", "code", "method"})

func init() {
	promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "demo_api_build_info",
		Help: "Build information; value is always 1.",
	}, []string{"version", "arch"}).WithLabelValues(version, runtime.GOARCH).Set(1)
}

// meta is what GET / reports; it is for placement observation, not provenance.
type meta struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	Node      string `json:"node"`
	Pod       string `json:"pod"`
	PodUID    string `json:"podUID"`
	Namespace string `json:"namespace"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	StartedAt string `json:"startedAt"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newHandler(m meta, ready *atomic.Bool) http.Handler {
	return newHandlerWithFault(m, ready, nil)
}

func newHandlerWithFault(m meta, ready *atomic.Bool, fault *readinessFault) http.Handler {
	instrument := func(name string, h http.HandlerFunc) http.Handler {
		// Curry handler so InstrumentHandlerCounter only sees code and method.
		return promhttp.InstrumentHandlerCounter(requests.MustCurryWith(prometheus.Labels{"handler": name}), h)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", instrument("/", func(w http.ResponseWriter, _ *http.Request) {
		code := http.StatusOK
		if fault.active() {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, m)
	}))
	mux.Handle("GET /health", instrument("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.Handle("GET /ready", instrument("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting-down"})
			return
		}
		if fault.active() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "fault-active"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}))
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

func main() {
	m := meta{
		Service:   "demo-api",
		Version:   version,
		Node:      os.Getenv("NODE_NAME"),
		Pod:       os.Getenv("POD_NAME"),
		PodUID:    os.Getenv("POD_UID"),
		Namespace: os.Getenv("POD_NAMESPACE"),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(os.Args) > 1 {
		if err := runLocalFault(os.Args[1:], m); err != nil {
			fmt.Fprintln(os.Stderr, "demo-api:", err)
			os.Exit(1)
		}
		return
	}
	var ready atomic.Bool
	ready.Store(true)
	fault, err := faultFromEnvironment(m)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           newHandlerWithFault(m, &ready, fault),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris guard; the port is exposed via NodePort.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var admin *http.Server
	if fault != nil {
		admin = &http.Server{Addr: "127.0.0.1:18081", Handler: newFaultAdminHandler(fault), ReadHeaderTimeout: 2 * time.Second}
		go func() {
			if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Print("readiness fault loopback server unavailable")
				stop()
			}
		}()
	}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if admin != nil {
		_ = admin.Shutdown(sctx)
	}
}
