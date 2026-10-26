package main

// This client is deliberately independent of observerGuard. It has no discovery,
// kubeconfig, proxy, exec auth, redirect, retry or in-cluster fallback.
import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type finishClient struct {
	client                     *http.Client
	origin, bearer, policyPath string
	reads                      map[string]bool
}

func localFinishURL(raw string, api bool) (*url.URL, error) {
	u, e := observerURL(raw, api)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("finish: only literal loopback test origins supported")
	}
	return u, nil
}
func newFinishClient(origin, ca, bearer, policyPath string, reads map[string]bool, timeout time.Duration) (*finishClient, error) {
	// Validate the supplied origin independently of config parsing. API writes
	// require HTTPS; Service authority is separate and receives no API bearer.
	u, e := observerURL(origin, true)
	if policyPath == "" {
		u, e = observerURL(origin+"/", false)
	}
	if e != nil || u.Scheme+"://"+u.Host != origin {
		return nil, errors.New("finish: invalid explicit client origin")
	}
	roots := x509.NewCertPool()
	if ca != "" {
		b, e := observerLocalFile(ca, 1<<20)
		if e != nil || !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("finish: invalid explicit test CA")
		}
	} else {
		roots = nil
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: timeout, TLSHandshakeTimeout: timeout, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	return &finishClient{client: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origin: origin, bearer: bearer, policyPath: policyPath, reads: reads}, nil
}

// Only an internally constructed JSON Patch is accepted. The caller cannot
// supply a URL, arbitrary patch, headers, or a different write verb/resource.
func (c *finishClient) request(ctx context.Context, path string, patch []byte) ([]byte, int, string) {
	method := http.MethodGet
	if patch != nil {
		if path != c.policyPath || c.policyPath == "" {
			return nil, 0, "write-boundary"
		}
		method = http.MethodPatch
	} else if !c.reads[path] {
		return nil, 0, "read-boundary"
	}
	r, e := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(patch))
	if e != nil {
		return nil, 0, "request-invalid"
	}
	r.Header.Set("Accept", "application/json")
	if patch != nil {
		r.Header.Set("Content-Type", "application/json-patch+json")
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, e := c.client.Do(r)
	if e != nil {
		return nil, 0, "transport-or-timeout-unconfirmed"
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, resp.StatusCode, "body-unconfirmed"
	}
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, "http-rejected"
	}
	return b, resp.StatusCode, ""
}

type finishPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func stopOnlyPatch(uid, rv string, spec map[string]any) []byte {
	b, _ := json.Marshal([]finishPatchOp{{"test", "/metadata/uid", uid}, {"test", "/metadata/resourceVersion", rv}, {"test", "/spec", spec}, {"replace", "/spec/enabled", false}})
	return b
}
