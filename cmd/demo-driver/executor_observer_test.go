package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The collector keeps its normal poll/finalize path. A gated clock permits
// real HTTP stop/ack samples before the independently running executor resumes.
type finishGatedClock struct {
	base   *observerTestClock
	pulses chan chan struct{}
}

func (c *finishGatedClock) Now() time.Time { return c.base.Now() }
func (c *finishGatedClock) Sleep(ctx context.Context, d time.Duration) error {
	ch := make(chan struct{})
	select {
	case c.pulses <- ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ch:
		return c.base.Sleep(ctx, d)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type finishGateVM struct {
	base            *finishFakeVM
	once            sync.Once
	paused, proceed chan struct{}
}

func (v *finishGateVM) Run(ctx context.Context, args []string) (string, error) {
	out, e := v.base.Run(ctx, args)
	if len(args) == 4 && args[2] == "domstate" && out == "paused" {
		v.once.Do(func() {
			close(v.paused)
			select {
			case <-v.proceed:
			case <-ctx.Done():
			}
		})
	}
	return out, e
}
func TestFinishObserverRuntime(t *testing.T) {
	for _, variant := range []string{"normal", "early-restore", "no-clock-witness", "no-stop-ack"} {
		t.Run(variant, func(t *testing.T) {
			finishObserverRuntime(t, variant, nil)
		})
	}
}

// Optional consumer hook prepares configuration before collection and runs only
// after the unchanged collection/executor/receipt/Acceptance flow has finished.
func finishObserverRuntime(t *testing.T, variant string, before func(ObserverConfig) func(*finishGateVM)) {
	base := newObserverScene(t, "normal")
	oc := base.config
	originalAPI := oc.APIOrigin
	upstream, guard, e := observerClient(originalAPI, oc.APICAFile, "synthetic-api-credential-DO-NOT-EXPORT", time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer upstream.CloseIdleConnections()
	// This proxy is the explicit test API/Controller double. Only it creates
	// Quiesced/fence after receiving a real conditional disable HTTP request.
	var mu sync.Mutex
	var policy v1.RecoveryPolicy
	var fence string
	patches := 0
	forward := func(path string, v any) {
		guard.allowed[path] = true
		b, _, p := observerGET(context.Background(), upstream, originalAPI+path, 1<<20)
		if p != "" || strictDecode(b, v, false) != nil {
			t.Error("upstream fixture read failed", p)
		}
	}
	ppath := "/apis/opsloop.io/v1alpha1/namespaces/" + oc.Namespace + "/recoverypolicies/" + oc.Policy
	dpath := "/apis/apps/v1/namespaces/" + oc.Namespace + "/deployments/" + oc.Workload
	forward(ppath, &policy)
	policy.ResourceVersion = "initial-policy-rv"
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer synthetic-api-credential-DO-NOT-EXPORT" {
			t.Error("fixture auth")
		}
		if r.Method == "PATCH" {
			if r.URL.Path != ppath {
				t.Error("out of bounds patch")
				w.WriteHeader(403)
				return
			}
			b, _ := io.ReadAll(r.Body)
			var spec map[string]any
			pb, _ := json.Marshal(policy.Spec)
			json.Unmarshal(pb, &spec)
			if !bytes.Equal(b, stopOnlyPatch(string(policy.UID), policy.ResourceVersion, spec)) {
				t.Error("patch preconditions")
				w.WriteHeader(409)
				return
			}
			patches++
			policy.Spec.Enabled = false
			policy.Generation++
			policy.ResourceVersion = "disabled-policy-rv"
			if variant != "no-stop-ack" {
				f := StopFence{Version: 2, PolicyUID: string(policy.UID), PolicyName: oc.Policy, DeploymentUID: "w", ClosedThroughGeneration: policy.Generation}
				fb, _ := json.Marshal(f)
				fence = string(fb)
				policy.Status.Conditions = []meta.Condition{{Type: "Quiesced", Status: meta.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: policy.Generation, Message: fence}}
			}
			json.NewEncoder(w).Encode(policy)
			return
		}
		if r.Method != "GET" {
			t.Error("unexpected method")
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case ppath:
			json.NewEncoder(w).Encode(policy)
		case dpath:
			var d apps.Deployment
			forward(r.URL.RequestURI(), &d)
			if fence != "" {
				d.Annotations = map[string]string{"opsloop.io/execution-fence": fence}
			} else {
				d.Annotations = nil
			}
			json.NewEncoder(w).Encode(d)
		case "/api/v1/namespaces/kube-system":
			json.NewEncoder(w).Encode(core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: "synthetic-cluster"}})
		case "/api/v1/namespaces/" + oc.Namespace:
			json.NewEncoder(w).Encode(core.Namespace{ObjectMeta: meta.ObjectMeta{Name: oc.Namespace, UID: "synthetic-namespace"}})
		default:
			var v json.RawMessage
			forward(r.URL.RequestURI(), &v)
			w.Write(v)
		}
	}))
	defer proxy.Close()
	ca := filepath.Join(t.TempDir(), "proxy-ca.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	oc.APIOrigin = proxy.URL
	oc.APICAFile = ca
	oc.TimeoutMS = 1000
	// Executor output and external synthetic witness input are separate files.
	// No completed runtime evidence/report is used as an input or edited.
	oc.ReceiptsFile = filepath.Join(t.TempDir(), "witness-composed-receipts.json")
	var after func(*finishGateVM)
	if before != nil {
		after = before(oc)
	}
	gated := &finishGatedClock{base.clock, make(chan chan struct{})}
	result := make(chan error, 1)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { result <- collectObserver(parent, oc, io.Discard, gated) }()
	nextPulse := func() chan struct{} {
		select {
		case ch := <-gated.pulses:
			return ch
		case e := <-result:
			t.Fatalf("collector ended early: %v", e)
		case <-time.After(10 * time.Second):
			t.Fatal("collector stalled")
		}
		return nil
	}
	pulse := nextPulse()
	threshold := 79.0
	if variant == "early-restore" {
		threshold = 60
	}
	for base.clock.seconds() < threshold {
		close(pulse)
		pulse = nextPulse()
	}
	var desired apps.Deployment
	mu.Lock()
	forward(dpath, &desired)
	mu.Unlock()
	cfg := FinishConfig{Version: "opsloop-finish/v1", Environment: "synthetic-local", RunID: oc.RunID, ClusterUID: "synthetic-cluster", Namespace: oc.Namespace, NamespaceUID: "synthetic-namespace", Policy: oc.Policy, PolicyUID: string(policy.UID), PolicyGeneration: 1, PolicyHash: v1.PolicyHash(policy.Spec), Workload: oc.Workload, DeploymentUID: "w", DeploymentSpecHash: v1.SpecHash(desired.Spec), DeploymentTemplateHash: v1.TemplateHash(desired.Spec.Template), APIOrigin: proxy.URL, APICAFile: ca, BearerFile: oc.BearerFile, Backend: "test-executable", Executable: "/tmp/not-launched-injected-fixture", ExecutableSHA256: strings.Repeat("0", 64), VMURI: "test:///opsloop-p2d", VMUUID: finishTestUUID, Scenario: "single-vm-pause-resume", AdditionalChanges: []string{}, DeadlineMS: 5000, TimeoutMS: 1000, PollMS: 50, JournalDir: filepath.Join(t.TempDir(), "execution"), Service: oc.Service, ServiceUID: "service-uid", ServiceURL: oc.ServiceURL, ExpectedPod: FinishPod{baseInput(t).Incident.Status.Runtime.Attempts[0].PodName, baseInput(t).Incident.Status.Runtime.Attempts[0].PodUID}, ExpectedNode: oc.ExpectNode, ExpectedVersion: oc.ExpectVersion, OldPods: []FinishPod{}}
	cfg.ReceiptFile = filepath.Join(cfg.JournalDir, "receipts.json")
	vm := &finishGateVM{base: &finishFakeVM{uuid: finishTestUUID, state: "paused"}, paused: make(chan struct{}), proceed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- executeFinish(context.Background(), cfg, io.Discard, base.clock, vm, nil) }()
	if variant != "no-stop-ack" {
		select {
		case <-vm.paused:
		case err := <-done:
			t.Fatalf("executor before VM gate: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("executor stalled")
		}
		// The Observer independently GETs the new Policy and live Deployment fence.
		close(pulse)
		pulse = nextPulse()
		close(pulse)
		pulse = nextPulse()
		close(vm.proceed)
	}
	err := <-done
	if (err == nil) != (variant != "no-stop-ack") {
		t.Fatal("executor", err)
	}
	execution := finishReceipt(t, cfg)
	composed := execution
	if variant != "no-clock-witness" {
		// Separate, explicit test-only actor/clock witness. It does not alter any
		// execution timestamp, elapsed, segment, request, confirmation or outcome.
		source := base.receipts
		composed.FaultRequested = source.FaultRequested
		composed.Decision = source.Decision
		composed.Patched = source.Patched
		composed.ReadySource = source.ReadySource
		composed.Coverage = source.Coverage
		composed.Clocks = source.Clocks
		composed.CollectorClock = source.CollectorClock
		segments := map[string]bool{}
		for _, at := range []Stamp{func() Stamp {
			if execution.StopRequested != nil {
				return *execution.StopRequested
			}
			return Stamp{}
		}(), execution.Cleanup.Completed} {
			if stampValid(at) && !segments[at.Segment] {
				segments[at.Segment] = true
				composed.Clocks = append(composed.Clocks, ClockBound{Recorder: at.Recorder, Segment: at.Segment, UncertaintyMS: ptr(1.0), Ref: "explicit-test-only-shared-virtual-clock-provider"})
			}
		}
		b, _ := os.ReadFile(cfg.ReceiptFile)
		composed.Provider = "explicit synthetic actor/clock witness + immutable executor receipt sha256=" + digestBytes(b)
	}
	observerTestJSON(t, oc.ReceiptsFile, composed)
	// The receipt is present BEFORE subsequent observations and final horizon.
	for {
		close(pulse)
		select {
		case pulse = <-gated.pulses:
			continue
		case err = <-result:
		}
		break
	}
	var report AcceptanceResult
	if e := readJSON(filepath.Join(oc.OutputRoot, oc.RunID, "summary.json"), &report); e != nil {
		t.Fatal(e)
	}
	if report.OverallPass != (variant == "normal") || report.LiveProof {
		b, _ := json.Marshal(report)
		t.Fatalf("unexpected runtime-v1 result %s (err %v)", b, err)
	}
	if variant == "early-restore" && report.Attribution.Status != "FAIL" {
		t.Fatal("early resume became pure controller success", report.Attribution)
	}
	if variant == "no-stop-ack" && vm.base.resumes != 0 {
		t.Fatal("resume without stop ack")
	}
	if patches != 1 {
		t.Fatal("Policy write count", patches)
	}
	t.Logf("runtime-v1=%t service=%s attribution=%s cleanup=%s HTTPpatches=%d resumes=%d synthetic=true liveProof=false", report.OverallPass, report.ServiceRecovery.Status, report.Attribution.Status, report.Cleanup.Status, patches, vm.base.resumes)
	if root := os.Getenv("P2D_EXAMPLES"); root != "" {
		dir := filepath.Join(root, "observer-"+variant)
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		observerTestJSON(t, filepath.Join(dir, "executor-receipt.json"), execution)
		observerTestJSON(t, filepath.Join(dir, "synthetic-witness-receipt.json"), composed)
		observerTestJSON(t, filepath.Join(dir, "runtime-v1-summary.json"), report)
		observerTestJSON(t, filepath.Join(dir, "selection.json"), []string{"executor-receipt.json: real local executor boundaries", "synthetic-witness-receipt.json: explicit synthetic external actor/clock source", "runtime-v1-summary.json: unchanged Observer/Acceptance output"})
	}
	if after != nil {
		after(vm)
	}
}

func (v *finishGateVM) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e := boundary("vm-start-attempt"); e != nil {
		return "", e
	}
	if e := boundary("vm-started"); e != nil {
		return "", e
	}
	return v.Run(ctx, args)
}
