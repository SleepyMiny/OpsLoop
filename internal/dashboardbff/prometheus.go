package dashboardbff

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PrometheusQueryOnly permits only the read-only instant-query endpoint.
// client_golang normally sends form POST queries; convert these to GET before
// any network dispatch so even a Kubernetes Service proxy needs no write verb.
type PrometheusQueryOnly struct{ Base http.RoundTripper }

func (t PrometheusQueryOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/api/v1/query") || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		return nil, errors.New("dashboard Prometheus transport permits queries only")
	}
	request := r.Clone(r.Context())
	if r.Method == http.MethodPost {
		if r.Body == nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			return nil, errors.New("invalid query form")
		}
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 8193))
		if err != nil || len(body) > 8192 {
			return nil, errors.New("query form exceeds bounds")
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, errors.New("invalid query parameters")
		}
		q := request.URL.Query()
		for key, value := range values {
			q[key] = value
		}
		request.URL.RawQuery = q.Encode()
		request.Method = http.MethodGet
		request.Body = nil
		request.GetBody = nil
		request.ContentLength = 0
		request.Header.Del("Content-Type")
	}
	return t.Base.RoundTrip(request)
}
