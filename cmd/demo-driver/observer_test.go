package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type observerTestClock struct {
	mu    sync.Mutex
	t     time.Time
	start time.Time
	gap   bool
	step  bool
}

func (c *observerTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(10 * time.Millisecond)
	return c.t
}
func (c *observerTestClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	if c.gap && c.t.Sub(c.start) > 58*time.Second {
		c.t = c.t.Add(12 * time.Second)
		c.gap = false
	}
	if c.step && c.t.Sub(c.start) > 58*time.Second {
		c.t = c.t.Add(-10 * time.Second)
		c.step = false
	}
	return nil
}
func (c *observerTestClock) seconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t.Sub(c.start).Seconds()
}
func (c *observerTestClock) set(s float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.start.Add(time.Duration(s * 1e9))
}
func receiptStamp(at time.Time, sec float64, lane string) Stamp {
	ms := sec * 1000
	return Stamp{Recorder: lane, Segment: "synthetic-clock", Sequence: int64(ms) + 10001, ElapsedMS: &ms, At: at.Add(time.Duration(sec * 1e9))}
}
func observerTestJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
}

type observerScene struct {
	config     ObserverConfig
	clock      *observerTestClock
	receipts   ObserverReceipts
	configPath string
	requests   []string
	mu         sync.Mutex
}

func newObserverScene(t *testing.T, variant string) *observerScene {
	t.Helper()
	dir := t.TempDir()
	origin := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	clock := &observerTestClock{t: origin, start: origin, gap: variant == "sample-gap"}
	s := &observerScene{clock: clock}
	cfg := ObserverConfig{RunID: "synthetic-" + variant, Source: "httptest API and Service; explicit synthetic actor test double", EvidenceKind: "synthetic", Namespace: "opsloop-demo", Workload: "demo-api", Policy: "demo-api", Incident: "demo-api-12345678-00001", Service: "demo-api", PathID: "service-approved", ExpectNode: "opsloop-control", ExpectVersion: "abc123", ImageRef: "acr.example/opsloop/demo-api@sha256:idx", PlatformDigests: []string{"sha256:amd"}, IntervalMS: 1000, TimeoutMS: 150, DeadlineMS: 85000, OutputRoot: dir, BearerFile: filepath.Join(dir, "synthetic-bearer"), APICAFile: filepath.Join(dir, "synthetic-ca.pem"), ReceiptsFile: filepath.Join(dir, "receipts-input.json")}
	const token = "synthetic-api-credential-DO-NOT-EXPORT"
	if e := os.WriteFile(cfg.BearerFile, []byte(token), 0600); e != nil {
		t.Fatal(e)
	}
	// Only API objects are supplied by the server. No completed evidence or
	// summary fixture is read by this integration scene.
	in := baseInput(t)
	inc := in.Incident.DeepCopy()
	inc.UID = "incident-uid"
	inc.Spec.Policy.Enabled = true
	inc.Spec.Policy.Mode = v1.PolicyModeEnforce
	inc.Spec.Policy.Budget.MaxActionsPerIncident = 1
	inc.Spec.PolicyHash = v1.PolicyHash(inc.Spec.Policy)
	inc.Spec.Baseline.Image = cfg.ImageRef
	pre := apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: cfg.Workload, Namespace: cfg.Namespace, UID: "w", ResourceVersion: "rv-before", Generation: 1}, Spec: apps.DeploymentSpec{Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{Name: "app", Image: cfg.ImageRef, Env: []core.EnvVar{{Name: "PRIVATE", Value: "do-not-dump-pod-env"}}}}}}}}
	inc.Spec.Baseline.SpecHash = v1.SpecHash(pre.Spec)
	post := pre.DeepCopy()
	post.ResourceVersion = "rv-after"
	post.Spec.Template.Annotations = map[string]string{"opsloop.io/attempt-id": inc.Status.Runtime.Attempts[0].ActionID}
	post.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": cfg.ExpectNode}
	a := &inc.Status.Runtime.Attempts[0]
	a.PreResourceVersion = pre.ResourceVersion
	a.PreTemplateHash = v1.TemplateHash(pre.Spec.Template)
	a.TargetTemplateHash = v1.TemplateHash(post.Spec.Template)
	a.Outcome = "Succeeded"
	a.EndpointReady = true
	a.ReplicaSetName = "demo-rs"
	a.ReplicaSetUID = "rs-uid"
	a.PatchedAt = &meta.Time{Time: origin.Add(40 * time.Second)}
	a.PodReadyAt = &meta.Time{Time: origin.Add(41 * time.Second)}
	a.Decision.PolicyHash = inc.Spec.PolicyHash
	a.Decision.BaselineTemplateHash = a.PreTemplateHash
	a.Decision.DecidedAt = meta.NewTime(origin.Add(39 * time.Second))
	inc.Spec.Baseline.TemplateHash = a.PreTemplateHash
	inc.Status.Runtime.Decision = &a.Decision
	policy := v1.RecoveryPolicy{ObjectMeta: meta.ObjectMeta{Name: cfg.Policy, Namespace: cfg.Namespace, UID: types.UID(inc.Spec.PolicyRef.UID), Generation: 1}, Spec: inc.Spec.Policy}
	f := StopFence{Version: 2, PolicyUID: string(policy.UID), PolicyName: cfg.Policy, DeploymentUID: "w", ClosedThroughGeneration: 2}
	fb, _ := json.Marshal(f)
	owner := func(kind, name, uid string) []meta.OwnerReference {
		return []meta.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr(true)}}
	}
	pod := core.Pod{ObjectMeta: meta.ObjectMeta{Name: a.PodName, Namespace: cfg.Namespace, UID: types.UID(a.PodUID), OwnerReferences: owner("ReplicaSet", a.ReplicaSetName, a.ReplicaSetUID)}, Spec: core.PodSpec{NodeName: cfg.ExpectNode, Containers: pre.Spec.Template.Spec.Containers}, Status: core.PodStatus{Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue, LastTransitionTime: *a.PodReadyAt}}, ContainerStatuses: []core.ContainerStatus{{Name: "app", ImageID: "acr.example/opsloop/demo-api@sha256:amd"}}}}
	rs := apps.ReplicaSet{ObjectMeta: meta.ObjectMeta{Name: a.ReplicaSetName, Namespace: cfg.Namespace, UID: types.UID(a.ReplicaSetUID), OwnerReferences: owner("Deployment", cfg.Workload, "w")}}
	node := core.Node{ObjectMeta: meta.ObjectMeta{Name: cfg.ExpectNode, UID: types.UID(a.NodeUID), Labels: map[string]string{"kubernetes.io/hostname": cfg.ExpectNode}}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}}
	service := core.Service{ObjectMeta: meta.ObjectMeta{Name: cfg.Service, Namespace: cfg.Namespace, UID: "service-uid"}}
	eps := discovery.EndpointSliceList{Items: []discovery.EndpointSlice{{ObjectMeta: meta.ObjectMeta{Name: "demo-slice", Namespace: cfg.Namespace, UID: "slice-uid", Labels: map[string]string{discovery.LabelServiceName: cfg.Service}, OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: cfg.Service, UID: service.UID}}}, Endpoints: []discovery.Endpoint{{TargetRef: &core.ObjectReference{Kind: "Pod", Namespace: cfg.Namespace, Name: a.PodName, UID: pod.UID}, Conditions: discovery.EndpointConditions{Ready: ptr(true)}}}}}}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing synthetic API auth")
		}
		if r.Method != "GET" {
			t.Error("write method")
		}
		sec := clock.seconds()
		if variant == "write-collision" && sec < 1 {
			p := filepath.Join(cfg.OutputRoot, cfg.RunID, "raw-0001.json")
			f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e == nil {
				f.Write([]byte("preserved partial file"))
				f.Close()
			}
		}
		path := r.URL.Path
		if strings.Contains(path, "/deployments/") && sec > 55 && sec < 58 {
			switch variant {
			case "api-403":
				w.WriteHeader(403)
				fmt.Fprint(w, token)
				return
			case "api-404":
				w.WriteHeader(404)
				return
			case "api-timeout":
				<-r.Context().Done()
				return
			case "api-redirect":
				w.Header().Set("Location", "/api/v1/secrets")
				w.WriteHeader(302)
				return
			}
		}
		var v any
		switch {
		case strings.Contains(path, "/recoverypolicies/"):
			p := policy.DeepCopy()
			if sec >= 81 && variant != "stop-no-ack" {
				p.Generation = 2
				p.Spec.Enabled = false
				p.Status.Conditions = []meta.Condition{{Type: "Quiesced", Status: meta.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: 2, Message: string(fb)}}
			}
			v = p
		case strings.Contains(path, "/deployments/"):
			p := pre.DeepCopy()
			if sec >= 40 {
				p = post.DeepCopy()
			}
			if sec >= 81 {
				p.Annotations = map[string]string{"opsloop.io/execution-fence": string(fb)}
			}
			if variant == "uid-replaced" && sec > 55 {
				p.UID = "replacement"
			}
			v = p
		case strings.Contains(path, "/services/"):
			v = service
		case strings.Contains(path, "/recoveryincidents/"):
			i := inc.DeepCopy()
			if sec < 42 {
				i.Status.Runtime.Attempts = nil
				i.Status.Runtime.Phase = "WAITING"
			}
			v = i
		case strings.Contains(path, "/pods/"):
			p := pod.DeepCopy()
			if variant == "missing-owner" {
				p.OwnerReferences = nil
			}
			if variant == "credential-reflection" {
				p.Name = token
			}
			v = p
		case strings.Contains(path, "/replicasets/"):
			p := rs.DeepCopy()
			if variant == "rs-owner" {
				p.OwnerReferences = nil
			}
			v = p
		case strings.Contains(path, "/nodes/"):
			v = node
		case strings.HasSuffix(path, "/endpointslices"):
			if r.URL.Query().Get("labelSelector") != "kubernetes.io/service-name="+cfg.Service || r.URL.Query().Get("limit") != "100" {
				t.Error("unbounded LIST")
			}
			v = eps
		default:
			t.Errorf("unexpected API path %s", path)
			w.WriteHeader(404)
			return
		}
		if e := json.NewEncoder(w).Encode(v); e != nil {
			t.Error(e)
		}
	}))
	t.Cleanup(api.Close)
	cfg.APIOrigin = api.URL
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	if e := os.WriteFile(cfg.APICAFile, ca, 0600); e != nil {
		t.Fatal(e)
	}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("API credentials leaked to Service")
		}
		if r.URL.Path != "/identity" || r.Method != "GET" {
			t.Error("unexpected Service path/method")
		}
		sec := clock.seconds()
		uid := "old"
		if sec >= 42 && variant != "old-pod" {
			uid = string(pod.UID)
		}
		if sec >= 33 && sec < 39 && variant != "no-outage" {
			w.WriteHeader(503)
			return
		}
		if sec > 59 && sec < 62 {
			switch variant {
			case "middle-uid":
				uid = "other"
			case "middle-503":
				w.WriteHeader(503)
				return
			case "middle-redirect":
				w.Header().Set("Location", api.URL+"/api/v1/secrets")
				w.WriteHeader(302)
				return
			case "invalid-body":
				fmt.Fprint(w, "{bad")
				return
			}
		}
		version := cfg.ExpectVersion
		if variant == "service-credential-reflection" {
			version = token
		}
		json.NewEncoder(w).Encode(map[string]string{"node": cfg.ExpectNode, "pod": pod.Name, "podUID": uid, "arch": "amd64", "version": version})
	}))
	t.Cleanup(svc.Close)
	cfg.ServiceURL = svc.URL + "/identity"
	stamp := func(sec float64) Stamp { return receiptStamp(origin, sec, "actor-test-double") }
	rec := ObserverReceipts{Version: "opsloop-observer-receipts/v1", RunID: cfg.RunID, EvidenceKind: "synthetic", Provider: "explicit-synthetic-actor-and-clock-test-double", FaultRequested: stamp(32.5), Decision: receiptStamp(origin, 39, "controller-test-double"), Patched: receiptStamp(origin, 40, "controller-test-double"), ReadySource: receiptStamp(origin, 41, "kubelet-test-double"), Interventions: []Intervention{}, StopRequested: ptr(stamp(80)), Cleanup: &CleanupProof{Status: "succeeded", Completed: stamp(82), WorkloadChecked: ptr(true), OldPodsChecked: ptr(true), ServiceChecked: ptr(true), Ref: "test-double/cleanup"}, CollectorClock: &ClockBound{UncertaintyMS: ptr(1.0), Ref: "test-double/shared-virtual-UTC"}}
	for _, lane := range []string{"actor-test-double", "controller-test-double", "kubelet-test-double"} {
		rec.Clocks = append(rec.Clocks, ClockBound{Recorder: lane, Segment: "synthetic-clock", UncertaintyMS: ptr(1.0), Ref: "test-double/shared-virtual-UTC"})
	}
	for i, actor := range []string{"watchdog", "admin", "cleanup", "desired-state"} {
		rec.Coverage = append(rec.Coverage, ActorCoverage{Actor: actor, Complete: ptr(true), Start: stamp(31 + float64(i)/10), End: stamp(79 + float64(i)/10), Ref: "test-double/" + actor})
	}
	switch variant {
	case "coverage-missing":
		rec.Coverage = nil
	case "external-restore":
		rec.Interventions = []Intervention{{Actor: "admin", Kind: "restore", TargetUID: "fault-node", Requested: stamp(60), Confirmed: ptr(stamp(61)), Ref: "test-double/restore"}}
	case "cleanup-missing":
		rec.Cleanup = nil
	case "cleanup-failed":
		rec.Cleanup.Status = "failed"
	case "no-receipts":
		cfg.ReceiptsFile = ""
	case "segment-changed":
		rec.Patched.Segment = "restarted-unbounded"
	case "source-clock-unbounded":
		rec.Clocks = nil
		rec.CollectorClock = nil
	}
	s.config = cfg
	s.receipts = rec
	if cfg.ReceiptsFile != "" {
		observerTestJSON(t, cfg.ReceiptsFile, rec)
	}
	s.configPath = filepath.Join(dir, "config.json")
	observerTestJSON(t, s.configPath, cfg)
	return s
}

func TestObserverIntegration(t *testing.T) {
	for _, variant := range []string{"normal", "no-outage", "old-pod", "middle-uid", "middle-503", "middle-redirect", "api-timeout", "api-403", "api-404", "api-redirect", "uid-replaced", "missing-owner", "rs-owner", "sample-gap", "segment-changed", "source-clock-unbounded", "external-restore", "coverage-missing", "stop-no-ack", "cleanup-missing", "cleanup-failed", "no-receipts", "invalid-body", "service-credential-reflection", "credential-reflection", "write-collision"} {
		t.Run(variant, func(t *testing.T) {
			s := newObserverScene(t, variant)
			var output bytes.Buffer
			err := runWithObserverClock([]string{"observe", "-config", s.configPath}, &output, s.clock)
			d := filepath.Join(s.config.OutputRoot, s.config.RunID)
			incomplete := variant == "uid-replaced" || variant == "service-credential-reflection" || variant == "credential-reflection" || variant == "write-collision"
			if incomplete {
				if bytes.Contains(output.Bytes(), []byte("synthetic-api-credential-DO-NOT-EXPORT")) {
					t.Fatal("credential in output")
				}
				entries, e := os.ReadDir(d)
				if e != nil {
					t.Fatal(e)
				}
				for _, entry := range entries {
					b, e := os.ReadFile(filepath.Join(d, entry.Name()))
					if e != nil {
						t.Fatal(e)
					}
					if bytes.Contains(b, []byte("synthetic-api-credential-DO-NOT-EXPORT")) {
						t.Fatal("credential in incomplete evidence")
					}
				}
				if err == nil {
					t.Fatal("incomplete run accepted")
				}
				if _, e := os.Stat(filepath.Join(d, "COMPLETE")); e == nil {
					t.Fatal("success marker after interruption")
				}
				if _, e := os.Stat(filepath.Join(d, "summary.json")); e == nil {
					t.Fatal("summary after fatal identity/write failure")
				}
				return
			}
			var result AcceptanceResult
			if e := readJSON(filepath.Join(d, "summary.json"), &result); e != nil {
				t.Fatal(e)
			}
			if variant == "no-outage" && !hasReason(result, "NO_OUTAGE_OBSERVED") {
				t.Fatal("missing outage reason", result)
			}
			if variant == "stop-no-ack" && result.Cleanup.Status != "UNPROVEN" {
				t.Fatal("missing stop ack must be UNPROVEN", result)
			}
			if strings.HasPrefix(variant, "api-") && result.ServiceRecovery.Status != "PASS" {
				t.Fatal("API gap became Service failure", result)
			}
			if result.OverallPass != (variant == "normal") || result.LiveProof || result.EvidenceKind != "synthetic" {
				t.Fatalf("err=%v output=%s result=%+v", err, output.String(), result)
			}
			if (err == nil) != (variant == "normal") {
				t.Fatalf("wrong CLI exit: %v", err)
			}
			var m observerManifest
			if e := readJSON(filepath.Join(d, "manifest.json"), &m); e != nil {
				t.Fatal(e)
			}
			if m.State != "complete" {
				t.Fatal(m)
			}
			for _, f := range m.Files {
				b, e := os.ReadFile(filepath.Join(d, f.Ref))
				if e != nil || digestBytes(b) != f.SHA256 {
					t.Fatalf("manifest hash %s", f.Ref)
				}
				if bytes.Contains(b, []byte("synthetic-api-credential-DO-NOT-EXPORT")) || bytes.Contains(b, []byte("do-not-dump-pod-env")) {
					t.Fatal("sensitive field persisted")
				}
				info, _ := os.Stat(filepath.Join(d, f.Ref))
				if info.Mode().Perm() != 0600 {
					t.Fatalf("mode %s", f.Ref)
				}
			}
			if variant == "normal" || variant == "no-outage" || variant == "stop-no-ack" {
				if cli := os.Getenv("P2C_CLI"); cli != "" {
					verifyBuiltObserverReport(t, cli, s, d, variant == "normal")
				}
				exportObserverExample(t, s, d)
			}
		})
	}
}
func exportObserverExample(t *testing.T, s *observerScene, d string) {
	root := os.Getenv("P2C_ARTIFACTS")
	if root == "" {
		return
	}
	dest, e := os.MkdirTemp(root, "collected-"+s.config.RunID+"-")
	if os.IsNotExist(e) {
		if e = os.MkdirAll(root, 0700); e != nil {
			t.Fatal(e)
		}
		dest, e = os.MkdirTemp(root, "collected-"+s.config.RunID+"-")
	}
	if e != nil {
		t.Fatal(e)
	}
	// Export essential generated files and only raw observations referenced by
	// apply/stop and first/final Service samples, not every repeated poll.
	keep := map[string]bool{"evidence.json": true, "probe.jsonl": true, "incident.json": true, "pods.json": true, "env.json": true, "summary.json": true, "manifest.json": true, "receipts.json": true, "COMPLETE": true, "started.json": true}
	var ev RuntimeEvidence
	readJSON(filepath.Join(d, "evidence.json"), &ev)
	keep[ev.Apply.Ref] = true
	if ev.Stop != nil {
		keep[ev.Stop.Ref] = true
	}
	for name := range keep {
		if name == "" {
			continue
		}
		b, e := os.ReadFile(filepath.Join(d, name))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dest, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	cfg := s.config
	cfg.BearerFile = "SYNTHETIC_BEARER_FILE"
	cfg.APICAFile = "SYNTHETIC_CA_FILE"
	cfg.OutputRoot = "NEW_EMPTY_OUTPUT_PARENT"
	cfg.ReceiptsFile = "receipts.json"
	observerTestJSON(t, filepath.Join(dest, "input-config.json"), cfg)
}

// A child process runs the exact CLI dispatcher/report path with clock injection
// confined to tests. Production observe has no virtual-clock flag.
func TestObserverCLIProcess(t *testing.T) {
	if os.Getenv("P2C_OBSERVER_CHILD") == "1" {
		s := newObserverScene(t, "normal")
		if e := runWithObserverClock([]string{"observe", "-config", s.configPath}, os.Stdout, s.clock); e != nil {
			t.Fatal(e)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestObserverCLIProcess$")
	cmd.Env = append(os.Environ(), "P2C_OBSERVER_CHILD=1")
	b, e := cmd.CombinedOutput()
	if e != nil || !bytes.Contains(b, []byte("collector=complete acceptance=PASS liveProof=false")) {
		t.Fatalf("child CLI: %v %s", e, b)
	}
}

func TestObserverBuiltCLI(t *testing.T) {
	cli := os.Getenv("P2C_CLI")
	if cli == "" {
		t.Skip("set P2C_CLI to the built production demo-driver for executable smoke test")
	}
	s := newObserverScene(t, "normal")
	cfg := s.config
	cfg.RunID = "built-cli-local"
	cfg.ReceiptsFile = ""
	cfg.IntervalMS = 10000
	cfg.TimeoutMS = 1000
	cfg.DeadlineMS = 10000
	observerTestJSON(t, s.configPath, cfg)
	cmd := exec.Command(cli, "observe", "-config", s.configPath)
	b, e := cmd.CombinedOutput()
	if e == nil || !bytes.Contains(b, []byte("acceptance=FAIL/UNPROVEN")) {
		t.Fatalf("production CLI: %v %s", e, b)
	}
	var result AcceptanceResult
	if e = readJSON(filepath.Join(cfg.OutputRoot, cfg.RunID, "summary.json"), &result); e != nil {
		t.Fatal(e)
	}
	if result.OverallPass || !hasReason(result, "NO_OUTAGE_OBSERVED") {
		t.Fatal(result)
	}
}

func verifyBuiltObserverReport(t *testing.T, cli string, s *observerScene, d string, wantPass bool) {
	t.Helper()
	path := func(name string) string { return filepath.Join(d, name) }
	// A new output location avoids reading/reusing the collector's summary.
	out := filepath.Join(t.TempDir(), "rebuilt-summary.json")
	args := []string{"report", "-acceptance", "runtime-v1", "-evidence", path("evidence.json"), "-probe", path("probe.jsonl"), "-incident", path("incident.json"), "-pods", path("pods.json"), "-env", path("env.json"), "-inject-at", s.receipts.FaultRequested.At.Format(time.RFC3339Nano), "-mode", modeEnforce, "-scenario", "runtime-observer", "-expect-node", s.config.ExpectNode, "-expect-version", s.config.ExpectVersion, "-platform-digests", strings.Join(s.config.PlatformDigests, ","), "-out", out}
	b, e := exec.Command(cli, args...).CombinedOutput()
	if (e == nil) != wantPass {
		t.Fatalf("built report %v %s", e, b)
	}
	actual, e := os.ReadFile(out)
	if e != nil {
		t.Fatal(e)
	}
	expected, e := os.ReadFile(path("summary.json"))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("built report and in-process report diverged")
	}
}
