package main

import (
	"net"
	"net/http"
	"strings"
)

// Binding to loopback alone does not stop a browser's DNS-rebinding request.
// Check Host before collecting any Kubernetes observations. Non-loopback
// deployments still require the separately authorized authenticated proxy.
func protectLoopbackHost(listen string, next http.Handler) http.Handler {
	host, port, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	if err != nil || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return next
	}
	allowed := map[string]bool{
		net.JoinHostPort(host, port):        true,
		net.JoinHostPort("localhost", port): true,
		net.JoinHostPort("127.0.0.1", port): true,
		net.JoinHostPort("::1", port):       true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost := strings.ToLower(r.Host)
		origin := r.Header.Get("Origin")
		if !allowed[requestHost] || (origin != "" && origin != "http://"+requestHost) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.Error(w, "local observation origin required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
