package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackHostBoundary(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090"} {
		for _, tc := range []struct {
			host, origin string
			allowed      bool
		}{
			{"127.0.0.1:8090", "", true},
			{"localhost:8090", "http://localhost:8090", true},
			{"[::1]:8090", "", true},
			{"attacker.example:8090", "", false},
			{"127.0.0.1:8091", "", false},
			{"127.0.0.1:8090", "https://attacker.example", false},
			{"127.0.0.1:8090", "null", false},
		} {
			t.Run(listen+"/"+tc.host+"/"+tc.origin, func(t *testing.T) {
				calls := 0
				handler := protectLoopbackHost(listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
				r := httptest.NewRequest("GET", "http://"+tc.host+"/api/v1/snapshot", nil)
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if tc.allowed && (w.Code != 200 || calls != 1) {
					t.Fatalf("valid local request rejected: %d", w.Code)
				}
				if !tc.allowed && (w.Code != 403 || calls != 0) {
					t.Fatalf("foreign request reached collector: %d/%d", w.Code, calls)
				}
			})
		}
	}
}
