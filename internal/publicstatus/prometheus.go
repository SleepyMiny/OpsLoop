package publicstatus

import (
	"errors"
	"net/http"
	"time"

	"github.com/SleepyMiny/OpsLoop/internal/dashboardbff"
	prom "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

var errPrometheusRedirect = errors.New("Collector Prometheus redirects are forbidden")

// NewPrometheusAPI is the Collector's client construction path. Configure the
// final API address: even same-origin redirects can change the fixed queries.
// A nil base uses the standard verified TLS transport without modifying it.
func NewPrometheusAPI(address string, base http.RoundTripper) (promv1.API, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	client, err := prom.NewClient(prom.Config{Address: address, Client: &http.Client{
		Transport:     dashboardbff.PrometheusQueryOnly{Base: base},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errPrometheusRedirect },
	}})
	if err != nil {
		return nil, err
	}
	return promv1.NewAPI(client), nil
}
