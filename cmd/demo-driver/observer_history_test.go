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
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newObserverHistoryScene(t *testing.T, variant string) *observerScene {
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
	if variant == "baseline-spec-mismatch" {
		inc.Spec.Baseline.SpecHash = "wrong"
	}
	post := pre.DeepCopy()
	post.ResourceVersion = "rv-after"
	post.Spec.Template.Annotations = map[string]string{"opsloop.io/attempt-id": inc.Status.Runtime.Attempts[0].ActionID}
	post.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": cfg.ExpectNode}
	a := &inc.Status.Runtime.Attempts[0]
	a.PreResourceVersion = pre.ResourceVersion
	if variant == "rv-observed" || variant == "rv-unobserved" || variant == "rv-wrong-hash" || variant == "rv-late-pre" {
		a.PreResourceVersion = "rv-mid"
	}
	if variant == "rv-evicted" {
		cfg.IntervalMS = 100
		cfg.DeadlineMS = 85000
	}
	if variant == "rv-wrong-required-hash" {
		a.PreTemplateHash = "wrong"
	}
	a.PreTemplateHash = v1.TemplateHash(pre.Spec.Template)
	if variant == "rv-wrong-required-hash" {
		a.PreTemplateHash = "wrong"
	}
	a.TargetTemplateHash = v1.TemplateHash(post.Spec.Template)
	a.Outcome = "Succeeded"
	a.EndpointReady = true
	a.ReplicaSetName = "demo-rs"
	a.ReplicaSetUID = "rs-uid"
	a.PatchedAt = &meta.Time{Time: origin.Add(40 * time.Second)}
	a.PodReadyAt = &meta.Time{Time: origin.Add(41 * time.Second)}
	if variant == "lifecycle" {
		a.PodReadyAt = &meta.Time{Time: origin.Add(45 * time.Second)}
	}
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
		if strings.HasSuffix(path, "/endpointslices") && ((variant == "endpoint-list-404" && sec > 55 && sec < 58) ||
			(variant == "endpoint-list-initial-404" && sec < 43) || (variant == "endpoint-list-late-404" && sec > 75 && sec < 78)) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sec >= 82 && variant == "stop-gap" && (strings.Contains(path, "/recoverypolicies/") || strings.Contains(path, "/deployments/")) {
			w.WriteHeader(403)
			return
		}
		if variant == "api-parser" && sec > 55 && sec < 58 && strings.Contains(path, "/deployments/") {
			w.Write([]byte("{"))
			return
		}
		if variant == "lifecycle" && ((sec < 43 && strings.Contains(path, "/pods/")) || (sec < 44 && strings.Contains(path, "/replicasets/"))) {
			w.WriteHeader(404)
			return
		}
		if strings.Contains(path, "/recoveryincidents/") && ((variant == "incident-created" || variant == "lifecycle") && sec < 34 || variant == "incident-never") {
			w.WriteHeader(404)
			return
		}
		if variant == "api-early-gap" && strings.Contains(path, "/deployments/") && sec > 10 && sec < 13 {
			w.WriteHeader(403)
			return
		}
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

			if variant == "stop-stale-first" && sec >= 81 && sec < 82 {
				p.Status.Conditions[0].ObservedGeneration = 1
			}
			if sec >= 82 {
				switch variant {
				case "stop-reactivated", "stop-reverted", "stop-new-request", "stop-old-request":
					if variant == "stop-reactivated" || sec < 83 {
						p.Generation = 3
						p.Spec.Enabled = true
						p.Status.Conditions = nil
					}
				case "stop-active":
					p.Status.ActiveIncident = &v1.ActiveIncidentRef{Name: "new", Sequence: 2, IncidentUID: "new", WorkloadUID: "w"}
				case "stop-generation":
					p.Generation = 3
				case "stop-quiesced-false":
					p.Status.Conditions[0].Status = meta.ConditionFalse
				}
			}
			v = p
		case strings.Contains(path, "/deployments/"):
			p := pre.DeepCopy()
			if variant == "rv-evicted" && sec >= 1 {
				p.ResourceVersion = "rv-metadata"
			}
			if variant == "pre-spec-reverted" && sec >= 20 && sec < 23 {
				p.Spec.Paused = true
			}
			if variant == "pre-template-reverted" && sec >= 20 && sec < 23 {
				p.Spec.Template.Spec.Containers[0].Image = "external.example/pre"
			}
			if sec >= 20 && (variant == "rv-observed" || variant == "rv-wrong-hash") {
				p.ResourceVersion = "rv-mid"
				p.Labels = map[string]string{"metadata-only": "yes"}
				if variant == "rv-wrong-hash" {
					p.Spec.Paused = true
				}
			}
			if sec >= 40 {
				p = post.DeepCopy()
			}
			if sec >= 81 {
				p.Annotations = map[string]string{"opsloop.io/execution-fence": string(fb)}
			}
			if variant == "rv-late-pre" && sec >= 42 && sec < 44 {
				p = pre.DeepCopy()
				p.ResourceVersion = "rv-mid"
			}
			if variant == "post-get-missing" && sec >= 40 {
				p = pre.DeepCopy()
			}
			if sec >= 55 {
				switch variant {
				case "late-template", "late-reverted":
					if variant != "late-reverted" || sec < 58 {
						p.Spec.Template.Spec.Containers[0].Image = "external.example/changed:other"
						p.ResourceVersion = "rv-external"
					}
				case "late-spec":
					p.Spec.Replicas = ptr(int32(7))
					p.ResourceVersion = "rv-external"
				case "late-strategy":
					p.Spec.Strategy.Type = apps.RecreateDeploymentStrategyType
				case "late-paused":
					p.Spec.Paused = true
				case "late-attempt":
					p.Spec.Template.Annotations["opsloop.io/attempt-id"] = "other"
				case "late-hostname":
					p.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = "other"
				case "normal-rv":
					p.ResourceVersion = "metadata-only"
				}
			}
			if variant == "cleanup-change" && sec >= 83 {
				p.Spec.Replicas = ptr(int32(0))
			}
			if sec >= 82 && (variant == "stop-fence-owner" || variant == "stop-fence-generation") {
				changed := f
				if variant == "stop-fence-owner" {
					changed.PolicyUID = "other"
				} else {
					changed.ClosedThroughGeneration = 1
				}
				b, _ := json.Marshal(changed)
				p.Annotations["opsloop.io/execution-fence"] = string(b)
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
			if variant == "lifecycle" {
				if sec >= 39 {
					i.Status.Runtime.Attempts = append([]v1.Attempt(nil), inc.Status.Runtime.Attempts...)
					a := &i.Status.Runtime.Attempts[0]
					if sec < 40 {
						a.Outcome = "Reserved"
						a.PreResourceVersion = ""
						a.PatchedAt = nil
						i.Status.Runtime.Phase = "EXECUTING"
					}
					if sec >= 40 && sec < 45 {
						a.Outcome = "Patched"
						i.Status.Runtime.Phase = "VERIFYING"
					}
					if sec < 42 {
						a.PodName = ""
						a.PodUID = ""
						a.ReplicaSetName = ""
						a.ReplicaSetUID = ""
					}
					if sec < 45 {
						a.PodReadyAt = nil
						a.EndpointReady = false
					}
				}
			}
			v = i
		case strings.Contains(path, "/pods/"):
			p := pod.DeepCopy()
			if variant == "late-pod-ready" && sec >= 55 {
				p.Status.Conditions[0].Status = core.ConditionFalse
				p.Status.Conditions[0].LastTransitionTime = meta.NewTime(origin.Add(55 * time.Second))
			}
			if variant == "lifecycle" && sec < 45 {
				p.Status.Conditions[0].Status = core.ConditionFalse
			}
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
			p := eps.DeepCopy()
			if (variant == "endpoints-empty-delayed" && sec < 45) || (variant == "endpoint-empty-lost" && sec > 55 && sec < 58) {
				p.Items = nil
			}
			if ((variant == "endpoints-delayed" || variant == "lifecycle") && sec < 45) || (variant == "endpoint-lost" && sec > 55 && sec < 58) {
				p.Items[0].Endpoints[0].Conditions.Ready = ptr(false)
			}
			v = p
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
		if sec >= 33 && (sec < 39 || (variant == "lifecycle" && sec < 45)) && variant != "no-outage" {
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
	rec := ObserverReceipts{Version: "opsloop-observer-receipts/v1", RunID: cfg.RunID, EvidenceKind: "synthetic", Provider: "explicit-synthetic-actor-and-clock-test-double", FaultRequested: stamp(32.5), Decision: receiptStamp(origin, 39, "controller-test-double"), Patched: receiptStamp(origin, 40, "controller-test-double"), ReadySource: receiptStamp(origin, 41, "kubelet-test-double"), Interventions: []Intervention{}, StopRequested: ptr(stamp(80)), Cleanup: &CleanupProof{Status: "succeeded", Completed: stamp(84), WorkloadChecked: ptr(true), OldPodsChecked: ptr(true), ServiceChecked: ptr(true), Ref: "test-double/cleanup"}, CollectorClock: &ClockBound{UncertaintyMS: ptr(1.0), Ref: "test-double/shared-virtual-UTC"}}
	if variant == "lifecycle" {
		rec.ReadySource = receiptStamp(origin, 45, "kubelet-test-double")
	}
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

	if variant == "cleanup-change" {
		rec.Interventions = []Intervention{{Actor: "cleanup", Kind: "desired-state", TargetUID: "w", Requested: stamp(82), Confirmed: ptr(stamp(83)), Ref: "test-double/cleanup"}}
	}
	if variant == "stop-new-request" {
		rec.StopRequested = ptr(stamp(83))
	}
	if variant == "stop-request-after-get" {
		rec.StopRequested = ptr(stamp(84.99))
	}
	if variant == "cleanup-before-ack" {
		rec.Cleanup.Completed = stamp(80.5)
	}
	if variant == "actor-positive-offset" || variant == "actor-negative-offset" || variant == "actor-hidden-future" || variant == "coverage-future" {
		shift := 10 * time.Second
		if variant != "actor-positive-offset" {
			shift = -10 * time.Second
		}
		if variant == "actor-hidden-future" {
			rec.Cleanup.Completed = stamp(92)
		}
		if variant == "coverage-future" {
			rec.Coverage[0].End = stamp(92)
		}
		rec.FaultRequested.At = rec.FaultRequested.At.Add(shift)
		rec.StopRequested.At = rec.StopRequested.At.Add(shift)
		rec.Cleanup.Completed.At = rec.Cleanup.Completed.At.Add(shift)
		for i := range rec.Coverage {
			rec.Coverage[i].Start.At = rec.Coverage[i].Start.At.Add(shift)
			rec.Coverage[i].End.At = rec.Coverage[i].End.At.Add(shift)
		}
		for i := range rec.Clocks {
			if rec.Clocks[i].Recorder == "actor-test-double" {
				rec.Clocks[i].OffsetMS = -float64(shift.Milliseconds())
			}
		}
	}

	if variant == "horizon-overlap" {
		rec.Cleanup.Completed = stamp(85)
		for i := range rec.Clocks {
			if rec.Clocks[i].Recorder == "actor-test-double" {
				rec.Clocks[i].UncertaintyMS = ptr(1000.0)
			}
		}
	}
	if variant == "clock-bound-missing" {
		rec.Clocks = rec.Clocks[1:]
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

func TestObserverHistory(t *testing.T) {
	pass := map[string]bool{}
	for _, v := range []string{"normal", "incident-created", "endpoints-delayed", "lifecycle", "api-early-gap", "rv-observed", "normal-rv", "stop-stale-first", "stop-new-request", "actor-positive-offset", "actor-negative-offset", "cleanup-change", "late-pod-ready"} {
		pass[v] = true
	}
	variants := []string{"normal", "incident-created", "endpoints-delayed", "lifecycle", "incident-never", "api-early-gap", "api-403", "api-timeout", "api-parser", "endpoint-lost", "uid-replaced", "invalid-body", "rv-observed", "rv-unobserved", "rv-wrong-hash", "rv-wrong-required-hash", "rv-late-pre", "rv-evicted", "baseline-spec-mismatch", "pre-spec-reverted", "pre-template-reverted", "post-get-missing", "normal-rv", "late-template", "late-spec", "late-strategy", "late-paused", "late-attempt", "late-hostname", "late-reverted", "cleanup-change", "late-pod-ready", "stop-reactivated", "stop-active", "stop-generation", "stop-quiesced-false", "stop-fence-owner", "stop-fence-generation", "stop-gap", "stop-stale-first", "stop-reverted", "stop-old-request", "stop-new-request", "stop-request-after-get", "cleanup-before-ack", "actor-positive-offset", "actor-negative-offset", "actor-hidden-future", "coverage-future", "clock-bound-missing", "horizon-overlap"}
	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			s := newObserverHistoryScene(t, variant)
			var out bytes.Buffer
			err := runWithObserverClock([]string{"observe", "-config", s.configPath}, &out, s.clock)
			d := filepath.Join(s.config.OutputRoot, s.config.RunID)
			var m observerManifest
			if e := readJSON(filepath.Join(d, "manifest.json"), &m); e != nil {
				t.Fatal(e)
			}
			var result AcceptanceResult
			re := readJSON(filepath.Join(d, "summary.json"), &result)
			if variant == "uid-replaced" {
				if re == nil || err == nil {
					t.Fatal("UID replacement must be incomplete")
				}
				if _, e := os.Stat(filepath.Join(d, "COMPLETE")); e == nil {
					t.Fatal("unexpected COMPLETE")
				}
				return
			}
			for _, diag := range m.Diagnostics {
				if !strings.HasPrefix(diag.Ref, "raw-") {
					continue
				}
				var raw struct{ Observed Stamp }
				if e := readJSON(filepath.Join(d, diag.Ref), &raw); e != nil {
					t.Fatal(e)
				}
				if lane(raw.Observed) != lane(diag.Observed) || raw.Observed.Sequence != diag.Observed.Sequence || !raw.Observed.At.Equal(diag.Observed.At) {
					t.Fatalf("diagnostic points at a different capture: %+v raw=%+v", diag, raw.Observed)
				}
			}
			if re != nil {
				t.Fatal(re)
			}
			if result.OverallPass != pass[variant] || (err == nil) != pass[variant] {
				t.Fatalf("%s err=%v result=%+v diagnostics=%+v", variant, err, result, m.Diagnostics)
			}
			if variant == "stop-gap" {
				found := false
				for _, diag := range m.Diagnostics {
					if diag.Proof == "cleanup" && diag.Reason == "stop-validity-contradiction-or-gap" {
						var raw struct{ Kind string }
						if e := readJSON(filepath.Join(d, diag.Ref), &raw); e != nil {
							t.Fatal(e)
						}
						if diag.Class != "gap" || raw.Kind != "stop-observation" {
							t.Fatal("cleanup gap must reference its aggregate and remain a gap")
						}
						found = true
					}
				}
				if !found {
					t.Fatal("cleanup gap diagnostic missing")
				}
			}
			if strings.HasPrefix(variant, "stop-") && variant != "stop-stale-first" && variant != "stop-new-request" && result.Cleanup.Status == "PASS" {
				t.Fatal("stop contradiction/gap lost")
			}
			if (variant == "api-403" || variant == "api-timeout" || variant == "api-parser") && result.ServiceRecovery.Status != "PASS" {
				t.Fatal("API gap manufactured outage")
			}
			cli := os.Getenv("P2C_CLI")
			if cli == "" {
				t.Fatal("P2C_CLI required")
			}
			verifyBuiltObserverReport(t, cli, s, d, pass[variant])
			var evidence RuntimeEvidence
			if e := readJSON(filepath.Join(d, "evidence.json"), &evidence); e != nil {
				t.Fatal(e)
			}
			if variant == "invalid-body" {
				for _, diag := range m.Diagnostics {
					if strings.HasPrefix(diag.Reason, "service-measurement:") && diag.Class != "gap" {
						t.Fatal("parser classification must remain a recorder gap")
					}
				}
			}
			if variant == "lifecycle" {
				pod, rs := false, false
				for _, diag := range m.Diagnostics {
					pod = pod || diag.Reason == "pods:http-404"
					rs = rs || diag.Reason == "replicasets:http-404"
				}
				if !pod || !rs {
					t.Fatal("full lifecycle must actually observe initial Pod and RS 404")
				}
			}
			if variant == "incident-created" && len(m.Diagnostics) == 0 {
				t.Fatal("pending diagnostics lost")
			}
			if variant == "actor-hidden-future" && (evidence.Cleanup != nil || evidence.Apply.Ref == "") {
				t.Fatal("future cleanup must affect cleanup alone")
			}
			if variant == "coverage-future" && !s.receipts.Coverage[0].End.At.Before(m.CollectionHorizon.At) {
				t.Fatal("coverage fixture must be raw-UTC past and corrected future")
			}
			if variant == "coverage-future" && (len(evidence.Coverage) != 3 || evidence.Apply.Ref == "" || evidence.Cleanup == nil) {
				t.Fatal("coverage exclusion affected unrelated proof")
			}
			original, e := os.ReadFile(s.config.ReceiptsFile)
			if e != nil {
				t.Fatal(e)
			}
			saved, e := os.ReadFile(filepath.Join(d, "receipts.json"))
			if e != nil || !bytes.Equal(original, saved) {
				t.Fatal("receipt bytes changed")
			}
			if root := os.Getenv("P2C_FIX_EXAMPLES"); root != "" {
				exportObserverHistory(t, d, filepath.Join(root, variant), m, evidence)
			}
			t.Logf("overall=%v service=%s attribution=%s cleanup=%s diagnostics=%d", result.OverallPass, result.ServiceRecovery.Status, result.Attribution.Status, result.Cleanup.Status, len(m.Diagnostics))
		})
	}
}
func exportObserverHistory(t *testing.T, d, dest string, m observerManifest, e RuntimeEvidence) {
	t.Helper()
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	for _, name := range []string{"probe.jsonl", "incident.json", "pods.json", "env.json", "evidence.json", "summary.json", "manifest.json", "receipts.json", "COMPLETE"} {
		selected[name] = true
	}
	for _, diag := range m.Diagnostics {
		if strings.HasPrefix(diag.Ref, "raw-") {
			selected[diag.Ref] = true
		}
	}
	for _, ref := range []string{e.Apply.Ref, func() string {
		if e.Stop != nil {
			return e.Stop.Ref
		}
		return ""
	}()} {
		if ref != "" {
			selected[ref] = true
		}
	}
	// Keep the first raw projection of each state hash/condition and each error.
	seen := map[string]bool{}
	raw, _ := filepath.Glob(filepath.Join(d, "raw-*.json"))
	for _, p := range raw {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var x struct {
			Kind, Problem string
			Projection    json.RawMessage
		}
		if err = json.Unmarshal(b, &x); err != nil {
			t.Fatal(err)
		}
		key := x.Kind + ":" + x.Problem + ":" + digestBytes(x.Projection)
		if !seen[key] {
			seen[key] = true
			selected[filepath.Base(p)] = true
		}
	}
	// Keep both original GET files behind every retained aggregate stop read.
	for name := range selected {
		if !strings.HasPrefix(name, "raw-") {
			continue
		}
		var raw struct {
			Kind       string
			Projection observerStopRead
		}
		b, err := os.ReadFile(filepath.Join(d, name))
		if err != nil {
			t.Fatal(err)
		}
		// Other projection shapes need no recursive references.
		var kind struct{ Kind string }
		if err = json.Unmarshal(b, &kind); err != nil {
			t.Fatal(err)
		}
		if kind.Kind != "stop-observation" {
			continue
		}
		if err = json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{raw.Projection.PolicyRef, raw.Projection.DeploymentRef} {
			if ref != "" {
				selected[ref] = true
			}
		}
	}
	for name := range selected {
		b, err := os.ReadFile(filepath.Join(d, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dest, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	observerTestJSON(t, filepath.Join(dest, "selection.json"), map[string]any{"selected": selected, "originalRawCount": len(raw), "note": "Exact generated files. Repeated raw projections omitted; manifest retains their hashes. Production report replay verified before export. No credentials or config included."})
}

// Supplemental boundary checks use the existing ordering contract directly.
// Integration scenes above cover the client/recorder/report path.
func TestObserverReceiptHorizonSameLane(t *testing.T) {
	s := newObserverHistoryScene(t, "normal")
	r, err := newObserverRecorder(s.config, s.clock)
	if err != nil {
		t.Fatal(err)
	}
	past, err := r.stamp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.clock.set(10)
	h, err := r.stamp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Wall time alone cannot override elapsed and sequence in the same lane.
	past.At = h.At.Add(time.Hour)
	o := &observer{r: r, evidence: RuntimeEvidence{Cleanup: &CleanupProof{Completed: past}}}
	r.manifest.CollectionHorizon = h
	s.clock.set(100) // receipt-read/serialization time cannot extend horizon
	o.validateReceiptHorizon()
	if o.evidence.Cleanup == nil {
		t.Fatal("same-lane elapsed proof discarded due to raw UTC")
	}
	future := h
	future.Sequence++
	ms := *h.ElapsedMS + 1
	future.ElapsedMS = &ms
	future.At = past.At.Add(-2 * time.Hour)
	o.evidence.Cleanup = &CleanupProof{Completed: future}
	o.validateReceiptHorizon()
	if o.evidence.Cleanup != nil {
		t.Fatal("same-lane future accepted due to raw UTC/read-time Now")
	}
	overlap := h
	o.evidence.Cleanup = &CleanupProof{Completed: overlap}
	o.validateReceiptHorizon()
	if o.evidence.Cleanup != nil {
		t.Fatal("equal horizon is not strict ordering")
	}
}
