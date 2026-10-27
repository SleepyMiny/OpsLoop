package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// There is deliberately no kubeconfig, exec credential, proxy, discovery or
// in-cluster fallback. Credentials are loaded only from the explicit file.
type ObserverConfig struct {
	Readiness       *ReadinessObserverConfig `json:"readiness,omitempty"`
	RunID           string                   `json:"runID"`
	Source          string                   `json:"source"`
	EvidenceKind    string                   `json:"evidenceKind"`
	APIOrigin       string                   `json:"apiOrigin"`
	APICAFile       string                   `json:"apiCAFile"`
	BearerFile      string                   `json:"bearerFile"`
	ServiceCAFile   string                   `json:"serviceCAFile"`
	Namespace       string                   `json:"namespace"`
	Workload        string                   `json:"workload"`
	Policy          string                   `json:"policy"`
	Incident        string                   `json:"incident"`
	Service         string                   `json:"service"`
	ServiceURL      string                   `json:"serviceURL"`
	PathID          string                   `json:"pathID"`
	ExpectNode      string                   `json:"expectNode"`
	ExpectVersion   string                   `json:"expectVersion"`
	ImageRef        string                   `json:"imageRef"`
	PlatformDigests []string                 `json:"platformDigests"`
	IntervalMS      int64                    `json:"intervalMs"`
	TimeoutMS       int64                    `json:"timeoutMs"`
	DeadlineMS      int64                    `json:"deadlineMs"`
	OutputRoot      string                   `json:"outputRoot"`
	ReceiptsFile    string                   `json:"receiptsFile"`
}

var observerBearer = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

var observerName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func validObserverName(s string) bool {
	return len(s) > 0 && len(s) <= 253 && observerName.MatchString(s)
}
func observerURL(s string, api bool) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.RawPath != "" || u.Opaque != "" || strings.Contains(u.Path, "..") || strings.ContainsAny(s, "\r\n\\") {
		return nil, errors.New("observer: invalid approved URL")
	}
	if (api && u.Scheme != "https") || (!api && u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("observer: API requires HTTPS; Service requires HTTP(S)")
	}
	if api && u.Path != "" {
		return nil, errors.New("observer: API origin must have no path")
	}
	if !api && u.Path == "" {
		return nil, errors.New("observer: explicit Service path required")
	}
	return u, nil
}
func (c ObserverConfig) validate() error {
	names := []string{c.RunID, c.Namespace, c.Workload, c.Policy, c.Incident, c.Service, c.PathID}
	if c.Readiness == nil || c.Readiness.Mode == "baseline" {
		names = append(names, c.ExpectNode)
	}
	for _, s := range names {
		if !validObserverName(s) {
			return errors.New("observer: invalid target name or run/path ID")
		}
	}
	if c.Source == "" || len(c.Source) > 256 || c.OutputRoot == "" || c.ExpectVersion == "" || c.ImageRef == "" || (c.EvidenceKind != "synthetic" && c.EvidenceKind != "observed") {
		return errors.New("observer: explicit provenance, output, image and version required")
	}
	if c.IntervalMS < 1 || c.TimeoutMS < 1 || c.TimeoutMS > 60000 || c.DeadlineMS < 1 || c.DeadlineMS > 3600000 || c.DeadlineMS/c.IntervalMS > 1000 {
		return errors.New("observer: invalid timing limits (maximum 1000 polls, one hour)")
	}
	api, e := observerURL(c.APIOrigin, true)
	if e != nil {
		return e
	}
	svc, e := observerURL(c.ServiceURL, false)
	if e != nil {
		return e
	}
	if api.Host == svc.Host {
		return errors.New("observer: Service must have a separate authority from API")
	}
	if c.Readiness != nil {
		return c.Readiness.validate(c)
	}
	return nil
}

type observerGuard struct {
	origin  string
	allowed map[string]bool
	bearer  string
	base    http.RoundTripper
}

func (g *observerGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(r.URL.String()) > 4096 || r.Method != http.MethodGet || r.URL.Scheme+"://"+r.URL.Host != g.origin || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawPath != "" || !g.allowed[r.URL.RequestURI()] || r.Body != nil || r.ContentLength > 0 || (r.Host != "" && r.Host != r.URL.Host) || len(r.Header) > 0 {
		return nil, errors.New("observer: request outside read-only boundary")
	}
	q := r.Clone(r.Context())
	q.Header = make(http.Header)
	q.Header.Set("Accept", "application/json")
	if g.bearer != "" {
		q.Header.Set("Authorization", "Bearer "+g.bearer)
	}
	return g.base.RoundTrip(q)
}
func observerClient(origin, ca, bearer string, timeout time.Duration) (*http.Client, *observerGuard, error) {
	var roots *x509.CertPool
	if ca != "" {
		b, e := observerLocalFile(ca, 1<<20)
		if e != nil {
			return nil, nil, errors.New("observer: CA file unavailable")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return nil, nil, errors.New("observer: invalid CA PEM")
		}
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: timeout, TLSHandshakeTimeout: timeout, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	g := &observerGuard{origin: origin, allowed: map[string]bool{}, bearer: bearer, base: tr}
	return &http.Client{Transport: g, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, g, nil
}
func observerLocalFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("observer: explicit input file unavailable")
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("observer: invalid input file size/type")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errors.New("observer: input read failed")
	}
	return b, nil
}

// Errors are classifications only: never include a URL, credential, response
// body, TLS diagnostic or API error message from an untrusted peer.
func observerGET(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, int, string) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if e != nil {
		return nil, 0, "request-invalid"
	}
	resp, e := c.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, 0, "cancelled"
		}
		var ne net.Error
		if errors.As(e, &ne) && ne.Timeout() {
			return nil, 0, "timeout-measurement-path"
		}
		return nil, 0, "transport-measurement-path"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, "redirect-rejected"
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil {
		return nil, resp.StatusCode, "body-read-failed"
	}
	if int64(len(b)) > limit {
		return nil, resp.StatusCode, "body-limit"
	}
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, fmt.Sprintf("http-%d", resp.StatusCode)
	}
	return b, resp.StatusCode, ""
}
