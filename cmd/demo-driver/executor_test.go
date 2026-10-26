package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const finishTestToken = "p2d-SYNTHETIC-API-secret"
const finishTestUUID = "11111111-2222-3333-4444-555555555555"

type finishFakeVM struct {
	mu                   sync.Mutex
	uuid, state, variant string
	calls                [][]string
	resumes              int
	afterRead            func()
}

func (v *finishFakeVM) Run(ctx context.Context, args []string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, append([]string{}, args...))
	if len(args) != 4 || args[0] != "--connect" || args[1] != "test:///opsloop-p2d" || args[3] != finishTestUUID {
		return "", errors.New("invalid fixed argv")
	}
	switch args[2] {
	case "domuuid":
		return v.uuid, nil
	case "domstate":
		if v.afterRead != nil {
			f := v.afterRead
			v.afterRead = nil
			f()
		}
		if v.variant == "query-fail" {
			return "", errors.New("query failed")
		}
		return v.state, nil
	case "resume":
		v.resumes++
		switch v.variant {
		case "failed":
			return "", errors.New("failed")
		case "timeout":
			<-ctx.Done()
			return "", ctx.Err()
		case "zero-paused":
			return "", nil
		case "lost":
			v.state = "running"
			return "", errors.New("response lost")
		case "shutoff":
			v.state = "shut off"
			return "", nil
		case "replace":
			v.uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			return "", nil
		}
		v.state = "running"
		return "resumed", nil
	}
	return "", errors.New("blocked")
}

type finishScene struct {
	c                           FinishConfig
	mu                          sync.Mutex
	p                           v1.RecoveryPolicy
	d                           apps.Deployment
	vm                          *finishFakeVM
	patches, gets, serviceCalls int
	variant                     string
	namespaceUID, clusterUID    string
	requests                    []string
}

func newFinishScene(t *testing.T, variant string) *finishScene {
	t.Helper()
	dir := t.TempDir()
	s := &finishScene{variant: variant, namespaceUID: "namespace-uid", clusterUID: "cluster-uid", vm: &finishFakeVM{uuid: finishTestUUID, state: "paused"}}
	s.p = v1.RecoveryPolicy{ObjectMeta: meta.ObjectMeta{Name: "policy", Namespace: "synthetic", UID: "policy-uid", ResourceVersion: "policy-rv-1", Generation: 1}, Spec: v1.RecoveryPolicySpec{TargetRef: v1.TargetRef{Name: "workload"}, Enabled: true, Mode: v1.PolicyModeEnforce}}
	s.d = apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: "workload", Namespace: "synthetic", UID: "deployment-uid", ResourceVersion: "deployment-rv-1", Generation: 1}}
	c := FinishConfig{Version: "opsloop-finish/v1", Environment: "synthetic-local", RunID: "finish-test", ClusterUID: s.clusterUID, Namespace: "synthetic", NamespaceUID: s.namespaceUID, Policy: "policy", PolicyUID: string(s.p.UID), PolicyGeneration: 1, PolicyHash: v1.PolicyHash(s.p.Spec), Workload: "workload", DeploymentUID: string(s.d.UID), DeploymentSpecHash: v1.SpecHash(s.d.Spec), DeploymentTemplateHash: v1.TemplateHash(s.d.Spec.Template), APICAFile: filepath.Join(dir, "ca.pem"), BearerFile: filepath.Join(dir, "synthetic-token"), Backend: "test-executable", Executable: filepath.Join(dir, "fixture"), ExecutableSHA256: strings.Repeat("0", 64), VMURI: "test:///opsloop-p2d", VMUUID: finishTestUUID, Scenario: "single-vm-pause-resume", AdditionalChanges: []string{}, DeadlineMS: 2000, TimeoutMS: 100, PollMS: 20, JournalDir: filepath.Join(dir, "journal"), Service: "service", ServiceUID: "service-uid", ExpectedPod: FinishPod{"new-pod", "new-pod-uid"}, ExpectedNode: "node", ExpectedVersion: "test-v1", OldPods: []FinishPod{{"old-pod", "old-pod-uid"}}}
	c.ReceiptFile = filepath.Join(c.JournalDir, "receipts.json")
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.serviceCalls++
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked to Service")
		}
		if variant == "service-fail" {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"node": c.ExpectedNode, "pod": c.ExpectedPod.Name, "podUID": c.ExpectedPod.UID, "version": c.ExpectedVersion})
	}))
	t.Cleanup(svc.Close)
	c.ServiceURL = svc.URL + "/identity"
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer "+finishTestToken {
			t.Error("wrong synthetic API credential")
		}
		if r.Method == "PATCH" {
			s.patches++
			if r.URL.Path != c.path("recoverypolicies", c.Policy) || r.Header.Get("Content-Type") != "application/json-patch+json" {
				t.Error("write outside boundary")
				w.WriteHeader(403)
				return
			}
			var ops []finishPatchOp
			data, _ := io.ReadAll(r.Body)
			if json.Unmarshal(data, &ops) != nil || len(ops) != 4 || ops[0].Op != "test" || ops[0].Path != "/metadata/uid" || ops[0].Value != c.PolicyUID || ops[1].Path != "/metadata/resourceVersion" || ops[1].Value != s.p.ResourceVersion || ops[2].Path != "/spec" || ops[3].Op != "replace" || ops[3].Path != "/spec/enabled" || ops[3].Value != false {
				t.Errorf("unsafe patch: %s", data)
				w.WriteHeader(409)
				return
			}
			if s.patches == 1 && (variant == "409" || variant == "409-spec") {
				s.p.ResourceVersion = "external-rv"
				if variant == "409-spec" {
					s.p.Spec.Mode = v1.PolicyModeObserveOnly
					s.p.Generation++
				}
				w.WriteHeader(409)
				return
			}
			if variant == "before-timeout" {
				<-r.Context().Done()
				return
			}
			s.p.Spec.Enabled = false
			s.p.Generation++
			s.p.ResourceVersion = "policy-stopped"
			s.controllerAck()
			if variant == "lost" {
				conn, _, e := w.(http.Hijacker).Hijack()
				if e == nil {
					conn.Close()
				}
				return
			}
			if variant == "after-timeout" {
				<-r.Context().Done()
				return
			}
			json.NewEncoder(w).Encode(s.p)
			return
		}
		if r.Method != "GET" {
			t.Error("unexpected method")
			w.WriteHeader(403)
			return
		}
		s.gets++
		var obj any
		switch r.URL.Path {
		case c.path("namespaces", "kube-system"):
			obj = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: types.UID(s.clusterUID)}}
		case c.path("namespaces", c.Namespace):
			obj = core.Namespace{ObjectMeta: meta.ObjectMeta{Name: c.Namespace, UID: types.UID(s.namespaceUID)}}
		case c.path("recoverypolicies", c.Policy):
			obj = s.p
		case c.path("deployments", c.Workload):
			obj = s.d
		case c.path("services", c.Service):
			obj = core.Service{ObjectMeta: meta.ObjectMeta{Name: c.Service, Namespace: c.Namespace, UID: types.UID(c.ServiceUID)}}
		case c.path("pods", c.ExpectedPod.Name):
			obj = core.Pod{ObjectMeta: meta.ObjectMeta{Name: c.ExpectedPod.Name, Namespace: c.Namespace, UID: types.UID(c.ExpectedPod.UID)}, Spec: core.PodSpec{NodeName: c.ExpectedNode}}
		case c.path("pods", "old-pod"):
			w.WriteHeader(404)
			return
		default:
			t.Error("unexpected resource", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(obj)
	}))
	t.Cleanup(api.Close)
	c.APIOrigin = api.URL
	observerTestJSON(t, c.BearerFile, finishTestToken) // raw token below, not JSON
	if e := os.WriteFile(c.BearerFile, []byte(finishTestToken), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(c.APICAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	s.c = c
	return s
}
func (s *finishScene) controllerAck() {
	f := StopFence{Version: 2, PolicyUID: string(s.p.UID), PolicyName: s.p.Name, DeploymentUID: string(s.d.UID), ClosedThroughGeneration: s.p.Generation}
	b, _ := json.Marshal(f)
	s.d.Annotations = map[string]string{"opsloop.io/execution-fence": string(b)}
	s.p.Status.Conditions = []meta.Condition{{Type: "Quiesced", Status: meta.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: s.p.Generation, Message: string(b)}}
	switch s.variant {
	case "no-ack":
		s.p.Status.Conditions = nil
	case "stale":
		s.p.Status.Conditions[0].ObservedGeneration--
	case "active":
		s.p.Status.ActiveIncident = &v1.ActiveIncidentRef{Name: "active"}
	case "fence":
		s.d.Annotations["opsloop.io/execution-fence"] = `{"version":2,"policyUID":"other"}`
	}
}
func (s *finishScene) execute(hook func(*finishJournal)) error {
	return executeFinish(context.Background(), s.c, io.Discard, observerWallClock{}, s.vm, hook)
}
func finishReceipt(t *testing.T, c FinishConfig) ObserverReceipts {
	t.Helper()
	var r ObserverReceipts
	b, e := os.ReadFile(c.ReceiptFile)
	if e != nil {
		t.Fatal(e)
	}
	if strictDecode(b, &r, true) != nil {
		t.Fatal("receipt schema")
	}
	return r
}
func TestFinishHTTPAndVM(t *testing.T) {
	for _, variant := range []string{"normal", "409", "409-spec", "before-timeout", "lost", "after-timeout", "no-ack", "stale", "active", "fence", "service-fail"} {
		t.Run(variant, func(t *testing.T) {
			s := newFinishScene(t, variant)
			err := s.execute(nil)
			r := finishReceipt(t, s.c)
			pass := variant == "normal" || variant == "409" || variant == "lost" || variant == "after-timeout"
			if (err == nil) != pass || (r.Cleanup.Status == "succeeded") != pass {
				t.Fatalf("err=%v receipt=%+v", err, r)
			}
			if !pass && !r.Cleanup.Completed.At.IsZero() {
				t.Fatal("pending cleanup manufactured a completion time")
			}
			want := 1
			if variant == "409" {
				want = 2
			}
			if s.patches != want {
				t.Fatalf("patch count %d", s.patches)
			}
			expectedResume := 0
			if pass || variant == "service-fail" {
				expectedResume = 1
			}
			if s.vm.resumes != expectedResume {
				t.Fatalf("resume count %d", s.vm.resumes)
			}
			if len(r.Coverage) != 0 || len(r.Clocks) != 0 || stampValid(r.FaultRequested) || stampValid(r.Decision) || r.CollectorClock != nil {
				t.Fatal("fabricated source/coverage/clock")
			}
			if pass {
				if r.StopRequested == nil || len(r.Interventions) != 1 || r.Interventions[0].Actor != "cleanup" || r.Interventions[0].Confirmed == nil {
					t.Fatal("missing action receipts")
				}
				before, _ := os.ReadFile(s.c.ReceiptFile)
				if e := s.execute(nil); e != nil {
					t.Fatal(e)
				}
				after, _ := os.ReadFile(s.c.ReceiptFile)
				if !bytes.Equal(before, after) || s.vm.resumes != 1 || s.patches != want {
					t.Fatal("redelivery duplicated/retimed action")
				}
			}
			saveFinishExample(t, variant, s, r, err)
		})
	}
}
func TestFinishIdentityAndPlan(t *testing.T) {
	for _, variant := range []string{"cluster", "namespace", "policy", "deployment", "spec", "vm", "run-reentry", "external-disabled", "plan", "confirm", "additional", "nonlocal"} {
		t.Run(variant, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			switch variant {
			case "cluster":
				s.c.ClusterUID = "wrong"
			case "namespace":
				s.c.NamespaceUID = "wrong"
			case "policy":
				s.c.PolicyUID = "wrong"
			case "deployment":
				s.c.DeploymentUID = "wrong"
			case "spec":
				s.c.PolicyHash = strings.Repeat("0", 64)
			case "vm":
				s.vm.uuid = "wrong"
			case "external-disabled":
				s.p.Spec.Enabled = false
			case "additional":
				s.c.AdditionalChanges = []string{"load"}
			case "nonlocal":
				s.c.APIOrigin = "https://192.0.2.1:443"
			case "run-reentry":
				if e := s.execute(nil); e != nil {
					t.Fatal(e)
				}
				s.c.RunID = "other-run"
			}
			if variant == "plan" || variant == "confirm" {
				p := filepath.Join(t.TempDir(), "config.json")
				observerTestJSON(t, p, s.c)
				args := []string{"finish-run", "-config", p}
				if variant == "confirm" {
					args = append(args, "-execute", "-confirm", "wrong")
				}
				var out bytes.Buffer
				e := run(args, &out)
				if (e == nil) != (variant == "plan") {
					t.Fatal(e)
				}
				if s.gets != 0 || s.patches != 0 || len(s.vm.calls) != 0 {
					t.Fatal("plan touched boundary")
				}
				if _, e = os.Stat(s.c.JournalDir); !os.IsNotExist(e) {
					t.Fatal("plan created journal")
				}
				return
			}
			beforeP, beforeR := s.patches, s.vm.resumes
			if s.execute(nil) == nil {
				t.Fatal("identity accepted")
			}
			if variant != "vm" && s.patches != beforeP {
				t.Fatal("invalid identity wrote")
			}
			if s.vm.resumes != beforeR {
				t.Fatal("invalid identity resumed")
			}
		})
	}
}
func TestFinishVMFailuresAndStopRecheck(t *testing.T) {
	for _, variant := range []string{"failed", "timeout", "zero-paused", "lost", "shutoff", "replace", "query-fail", "reactivated", "owner-changed"} {
		t.Run(variant, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			s.vm.variant = variant
			if variant == "reactivated" || variant == "owner-changed" {
				s.vm.afterRead = func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					if variant == "reactivated" {
						s.p.Spec.Enabled = true
						s.p.Generation++
					} else {
						s.d.Annotations["opsloop.io/execution-fence"] = `{"version":2,"policyUID":"other"}`
					}
				}
			}
			err := s.execute(nil)
			pass := variant == "lost"
			if (err == nil) != pass {
				t.Fatal(err)
			}
			r := finishReceipt(t, s.c)
			if (r.Cleanup.Status == "succeeded") != pass {
				t.Fatal("false cleanup success")
			}

			if variant == "failed" {
				saveFinishExample(t, "restore-failed", s, r, err)
			}
			before := s.vm.resumes
			_ = s.execute(nil)
			if s.vm.resumes != before {
				t.Fatal("uncertain resume replayed")
			}
			if strings.HasSuffix(variant, "changed") || variant == "reactivated" {
				if before != 0 {
					t.Fatal("resume after stop contradiction")
				}
			}
		})
	}
}
func TestFinishJournalBoundaries(t *testing.T) {
	// Fail persistence before each named action/result boundary, then reenter.
	for _, kind := range []string{"stop-intent", "stop-dispatch", "stop-response", "stop-confirmed", "resume-intent", "resume-dispatch", "vm-result", "resume-response", "resume-confirmed", "postchecks-confirmed", "cleanup-confirmed", "receipt-published"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			fired := false
			err := s.execute(func(j *finishJournal) {
				j.before = func(k string) error {
					if k == kind && !fired {
						fired = true
						return errors.New("injected persistence interruption")
					}
					return nil
				}
			})
			if err == nil || !fired {
				t.Fatal("boundary not interrupted", err)
			}
			beforeP, beforeR := s.patches, s.vm.resumes
			err = s.execute(nil)
			if beforeP > 0 && s.patches != beforeP {
				t.Fatal("replayed stop")
			}
			if beforeR > 0 && s.vm.resumes != beforeR {
				t.Fatal("replayed resume")
			}
			// A persisted dispatch with no effect stays unconfirmed, never resent.

		})
	}
	// Real torn bytes must block instead of resetting the ledger.
	s := newFinishScene(t, "normal")
	if e := s.execute(nil); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.c.JournalDir, "event-999999.json"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	p, r := s.patches, s.vm.resumes
	if s.execute(nil) == nil || s.patches != p || s.vm.resumes != r {
		t.Fatal("corrupt journal replay")
	}
}
func saveFinishExample(t *testing.T, variant string, s *finishScene, r ObserverReceipts, err error) {
	t.Helper()
	root := os.Getenv("P2D_EXAMPLES")
	if root == "" {
		return
	}
	switch variant {
	case "normal", "lost", "no-ack", "service-fail", "restore-failed":
	default:
		return
	}
	dir := filepath.Join(root, variant)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	observerTestJSON(t, filepath.Join(dir, "receipt.json"), r)
	observerTestJSON(t, filepath.Join(dir, "boundary-counts.json"), struct {
		Synthetic                                  bool
		Requests                                   []string
		Patches, Resumes, VMCommands, ServiceCalls int
		Succeeded                                  bool
	}{true, s.requests, s.patches, s.vm.resumes, len(s.vm.calls), s.serviceCalls, err == nil})
	entries, _ := os.ReadDir(s.c.JournalDir)
	var selection []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "event-") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(s.c.JournalDir, entry.Name()))
		var e finishEvent
		_ = json.Unmarshal(b, &e)
		switch e.Kind {
		case "run-bound", "stop-intent", "stop-dispatch", "stop-response", "stop-confirmed", "resume-dispatch", "resume-response", "resume-confirmed", "cleanup-confirmed", "incomplete":
			selection = append(selection, entry.Name())
			if er := os.WriteFile(filepath.Join(dir, entry.Name()), b, 0600); er != nil {
				t.Fatal(er)
			}
		}
	}
	observerTestJSON(t, filepath.Join(dir, "selection.json"), selection)
}

func (v *finishFakeVM) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
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
