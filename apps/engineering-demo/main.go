// engineering-demo is an isolated demonstration workload; it is never deployed
// over Demo A's demo-api. Its one known defect is repaired only by the AI runner.
//go:debug httpmuxgo121=0
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var version = "uncommitted"

const reproduction = `{"items":[]}`

// Preserve the failure in a structured response instead of relying on the Go
// HTTP server's connection-close behavior. No request content is logged.
func quoteSafely(items []Item) (q Quote, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(runtime.Error); ok && e.Error() == "runtime error: integer divide by zero" {
				err = errors.New("QUOTE_DIVISION_BY_ZERO")
			} else {
				err = errors.New("QUOTE_INTERNAL_ERROR")
			}
		}
	}()
	return CalculateQuote(items)
}

func main() {
	// A fixed fixture is replayed after a bounded bootstrap grace period. This
	// gives the existing Controller real Ready history before software failure.
	// The fixed value is opt-in and cannot execute arbitrary commands or URLs.
	replay := os.Getenv("DEMO_B_REPLAY")
	if replay != "" && replay != "empty-order-after-30s" {
		log.Fatal("unsupported reproduction profile")
	}
	start := time.Now()
	var logged atomic.Bool
	identity := func() map[string]any {
		return map[string]any{"service": "engineering-demo", "version": version,
			"pod": os.Getenv("POD_NAME"), "podUID": os.Getenv("POD_UID"),
			"node": os.Getenv("NODE_NAME"), "namespace": os.Getenv("POD_NAMESPACE"),
			"arch": runtime.GOARCH, "os": runtime.GOOS}
	}
	write := func(w http.ResponseWriter, code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter, r *http.Request) {
		if replay != "" && time.Since(start) >= 30*time.Second {
			if _, err := quoteSafely(nil); err != nil {
				if !logged.Swap(true) {
					log.Print("QUOTE_DIVISION_BY_ZERO fixture=empty-order-v1")
				}
				body := identity()
				body["error"] = err.Error()
				write(w, http.StatusServiceUnavailable, body)
				return
			}
		}
		write(w, http.StatusOK, identity())
	}
	mux.HandleFunc("GET /{$}", ready)
	mux.HandleFunc("GET /ready", ready)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /quote", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []Item `json:"items"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if err := d.Decode(&body); err != nil || len(body.Items) > 100 {
			write(w, http.StatusBadRequest, map[string]string{"error": "INVALID_ORDER"})
			return
		}
		var trailing any
		if d.Decode(&trailing) != io.EOF {
			write(w, http.StatusBadRequest, map[string]string{"error": "INVALID_ORDER"})
			return
		}
		q, err := quoteSafely(body.Items)
		if err != nil {
			code := http.StatusBadRequest
			if err.Error() == "QUOTE_DIVISION_BY_ZERO" || err.Error() == "QUOTE_INTERNAL_ERROR" {
				code = http.StatusInternalServerError
			}
			out := identity()
			out["error"] = err.Error()
			write(w, code, out)
			return
		}
		write(w, http.StatusOK, q)
	})
	srv := &http.Server{Addr: "127.0.0.1:18080", Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	if os.Getenv("DEMO_B_LISTEN") == "pod" {
		srv.Addr = ":8080"
	} else if os.Getenv("DEMO_B_LISTEN") != "" {
		log.Fatal("unsupported listen profile")
	}
	fmt.Println("engineering-demo started")
	log.Fatal(srv.ListenAndServe())
}
