// Command opsloop-controller runs the RecoveryPolicy manager.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/controller"
)

const (
	healthProbeAddress = ":8081"
	leaderElectionID   = "opsloop-controller.opsloop.io"
)

func main() {
	var promURL string
	var leaderElect bool
	flag.StringVar(&promURL, "prometheus-url", "", "Prometheus base URL (required)")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election")
	flag.Parse()
	ctrl.SetLogger(zap.New())
	if err := run(promURL, leaderElect); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(promURL string, leaderElect bool) error {
	if promURL == "" {
		return fmt.Errorf("--prometheus-url is required")
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	pc, err := promapi.NewClient(promapi.Config{Address: promURL})
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// OpsLoop exposes no metrics yet (D22); "0" disables the server.
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        healthProbeAddress,
		LeaderElection:                leaderElect,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	r := &controller.PolicyReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Now:       time.Now,
		Prom:      promv1.NewAPI(pc),
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
