// Command opsloop-controller runs the RecoveryPolicy manager.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/controller"
	"github.com/SleepyMiny/OpsLoop/internal/verification"
)

const (
	healthProbeAddress = ":8081"
	leaderElectionID   = "opsloop-controller.opsloop.io"
)

func main() {
	var promURL string
	var scopeFile, resourceFile, resourceSHA256, exhibitionFile string
	var leaderElect bool
	flag.StringVar(&promURL, "prometheus-url", "", "Prometheus base URL (required)")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election")
	flag.StringVar(&scopeFile, "execution-scope-file", "", "trusted exact experiment scope; empty denies all Enforce actions")
	flag.StringVar(&resourceFile, "resource-conditions-file", "", "trusted run-scoped conservative candidate inputs; empty uses only measurements")
	flag.StringVar(&resourceSHA256, "resource-conditions-sha256", "", "required exact SHA-256 of resource condition file")
	flag.StringVar(&exhibitionFile, "exhibition-authority-file", "", "static exact Policy/workload allowlist and offline Ed25519 issuer public key; mutually exclusive with execution-scope-file")
	flag.Parse()
	ctrl.SetLogger(zap.New())
	if err := run(promURL, leaderElect, scopeFile, resourceFile, resourceSHA256, exhibitionFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(promURL string, leaderElect bool, scopeFile, resourceFile, resourceSHA256 string, exhibitionFiles ...string) error {
	exhibitionFile := ""
	if len(exhibitionFiles) > 1 {
		return fmt.Errorf("exhibition authority: multiple files")
	}
	if len(exhibitionFiles) == 1 {
		exhibitionFile = exhibitionFiles[0]
	}
	if exhibitionFile != "" && (scopeFile != "" || resourceFile != "" || resourceSHA256 != "") {
		return fmt.Errorf("exhibition authority cannot be combined with legacy scope/resource files")
	}
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
	var conditions *candidates.ResourceConditions
	if (resourceFile == "") != (resourceSHA256 == "") {
		return fmt.Errorf("resource conditions: file and SHA-256 must be supplied together")
	}
	if resourceFile != "" {
		f, err := os.Open(resourceFile)
		if err != nil {
			return fmt.Errorf("resource conditions: cannot open configured file")
		}
		raw, err := io.ReadAll(io.LimitReader(f, 16385))
		f.Close()
		if err != nil || len(raw) > 16384 {
			return fmt.Errorf("resource conditions: unreadable/oversize configuration")
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != resourceSHA256 {
			return fmt.Errorf("resource conditions: approved hash mismatch")
		}
		conditions, err = candidates.ParseResourceConditions(raw)
		if err != nil {
			return err
		}
		if err := controller.BindResourceConditions(scope, conditions); err != nil {
			return err
		}
	}
	legacyAuthority, err := controller.NewExecutionAuthority(scope)
	if err != nil {
		return err
	}
	var authority interface {
		controller.ExecutionAuthorizer
		Instance() string
	} = legacyAuthority
	if exhibitionFile != "" {
		f, err := os.Open(exhibitionFile)
		if err != nil {
			return fmt.Errorf("exhibition authority: cannot open configured file")
		}
		raw, err := io.ReadAll(io.LimitReader(f, 32769))
		f.Close()
		if err != nil || len(raw) > 32768 {
			return fmt.Errorf("exhibition authority: unreadable/oversized configuration")
		}
		config, err := controller.ParseExhibitionAuthorityConfig(raw)
		if err != nil {
			return err
		}
		authority, err = controller.NewExhibitionAuthority(config)
		if err != nil {
			return err
		}
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
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		Now:                time.Now,
		Prom:               promv1.NewAPI(pc),
		Authority:          authority,
		ResourceConditions: conditions,
		Probe: func(ctx context.Context, podIP string, port int32, podUID string) error {
			return verification.Probe(ctx, nil, podIP, port, podUID)
		},
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
