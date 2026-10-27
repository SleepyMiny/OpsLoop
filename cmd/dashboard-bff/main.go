// dashboard-bff is a local/read-only observation endpoint. It never runs recovery.
package main

import (
	"context"
	"flag"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/dashboardbff"
	prom "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Dependencies may register process-global flags during init (including
	// controller-runtime's kubeconfig). Keep this executable's CLI independent.
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:8090", "Loopback default; external exposure needs separate authorization/authenticated proxy")
	kube := flags.String("kubeconfig", "", "Server-only kubeconfig; empty uses in-cluster ServiceAccount")
	endpoint := flags.String("prometheus-url", "", "Server-side Prometheus URL; empty returns missing metrics")
	ns := flags.String("namespace", "opsloop-demo", "Observed policy namespace")
	policy := flags.String("policy", "demo-api", "Observed policy")
	_ = flags.Parse(os.Args[1:])
	var cfg *rest.Config
	var e error
	if *kube != "" {
		cfg, e = clientcmd.BuildConfigFromFlags("", *kube)
	} else {
		cfg, e = rest.InClusterConfig()
	}
	if e != nil {
		log.Fatal("Kubernetes configuration unavailable (credential details omitted)")
	}
	reader, e := dashboardbff.NewKubernetesReader(cfg)
	if e != nil {
		log.Fatal("Kubernetes read-only client unavailable")
	}
	service := dashboardbff.Service{Reader: reader, Namespace: *ns, PolicyName: *policy}
	if *endpoint != "" {
		client, e := prom.NewClient(prom.Config{Address: *endpoint, RoundTripper: dashboardbff.PrometheusQueryOnly{Base: http.DefaultTransport}})
		if e != nil {
			log.Fatal("Prometheus configuration unavailable")
		}
		api := promv1.NewAPI(client)
		service.Metrics = func(ctx context.Context, t time.Time, w time.Duration) (candidates.Metrics, error) {
			return candidates.QueryMetrics(ctx, api, t, w)
		}
	}
	server := &http.Server{Addr: *listen, Handler: protectLoopbackHost(*listen, service), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("read-only dashboard BFF listening on %s", *listen)
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal("dashboard BFF server failed")
	}
}
