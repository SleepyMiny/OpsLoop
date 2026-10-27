// Command opsloop-controller runs the RecoveryPolicy manager.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	"github.com/SleepyMiny/OpsLoop/internal/verification"
)

const (
	healthProbeAddress = ":8081"
	leaderElectionID   = "opsloop-controller.opsloop.io"
)

func main() {
	var promURL string
	var scopeFile string
	var leaderElect bool
	flag.StringVar(&promURL, "prometheus-url", "", "Prometheus base URL (required)")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election")
	flag.StringVar(&scopeFile, "execution-scope-file", "", "trusted exact experiment scope; empty denies all Enforce actions")
	flag.Parse()
	ctrl.SetLogger(zap.New())
	if err := run(promURL, leaderElect, scopeFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(promURL string, leaderElect bool, scopeFile string) error {
	if promURL == "" {
		return fmt.Errorf("--prometheus-url is required")
	}
	var scope controller.ExecutionScope
	if scopeFile != "" {
		f, err := os.Open(scopeFile)
		if err != nil {
			return fmt.Errorf("execution scope: cannot open configured file")
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, 16385))
		if readErr != nil || len(raw) > 16384 {
			f.Close()
			return fmt.Errorf("execution scope: unreadable/oversize configuration")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&scope)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = fmt.Errorf("trailing input")
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("execution scope: invalid configuration")
		}
	}
	authority, err := controller.NewExecutionAuthority(scope)
	if err != nil {
		return err
	}
	// Non-secret identity only. Approval tooling must bind this exact process in
	// the exact current Pod's authenticated log; it is never an execution permit.
	identity, _ := json.Marshal(map[string]string{"schema": "opsloop-controller-execution-identity/v1", "instance": authority.Instance(), "enforceDefault": "deny-without-fresh-bound-grant"})
	fmt.Fprintln(os.Stdout, string(identity))
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
		Authority: authority,
		Probe: func(ctx context.Context, podIP string, port int32, podUID string) error {
			return verification.Probe(ctx, nil, podIP, port, podUID)
		},
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
