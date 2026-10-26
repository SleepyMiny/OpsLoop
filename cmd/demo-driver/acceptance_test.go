package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptr[T any](v T) *T { return &v }
func fixtureStamp(in ReportInput, sec float64) Stamp {
	ms := math.Round(sec * 1000)
	elapsed := ms + 100000
	return Stamp{Recorder: "observer", Segment: "one", Sequence: int64(ms) + 100001, ElapsedMS: &elapsed, At: in.InjectAt.Add(time.Duration(ms) * time.Millisecond)}
}
func acceptanceFixture(t *testing.T) (ReportInput, RuntimeEvidence) {
	t.Helper()
	in := baseInput(t)
	in.Incident.UID = "incident-uid"
	in.Incident.Spec.Policy.Enabled = true
	in.Incident.Spec.Policy.Mode = v1alpha1.PolicyModeEnforce
	in.Incident.Spec.Policy.Budget.MaxActionsPerIncident = 1
	in.Incident.Spec.PolicyHash = v1alpha1.PolicyHash(in.Incident.Spec.Policy)
	in.Incident.Spec.Baseline.Image = in.Env.ImageRef
	in.Incident.Status.Runtime.Decision.PolicyHash = in.Incident.Spec.PolicyHash
	a := attempt(&in)
	a.Decision.PolicyHash = in.Incident.Spec.PolicyHash
	a.PreResourceVersion = "rv-before"
	a.PatchedAt = &metav1.Time{Time: in.InjectAt.Add(27 * time.Second)}
	a.EndpointReady = true
	a.ReplicaSetUID = "rs-uid"
	fixturePodIdentity(t, &in, nil)
	svc := ServiceIdentity{Namespace: in.Incident.Namespace, Name: "demo-api", UID: "service-uid", PathID: "approved", URL: "http://demo.invalid/"}
	in.Env.RuntimeService = &svc
	e := RuntimeEvidence{Version: evidenceVersion, EvidenceKind: "synthetic", RunID: in.Env.RunID, Service: svc,
		Identity:       RunIdentity{PolicyUID: in.Incident.Spec.PolicyRef.UID, PolicyGeneration: 1, PolicyHash: in.Incident.Spec.PolicyHash, IncidentUID: "incident-uid", IncidentSequence: 1, ActionID: a.ActionID, DeploymentUID: "w", NodeUID: "n1", PodUID: "new"},
		FaultRequested: fixtureStamp(in, 0), Decision: fixtureStamp(in, 26), Patched: fixtureStamp(in, 27), ReadySource: fixtureStamp(in, 40), ReadyObserved: fixtureStamp(in, 45),
		Apply: ApplyProof{DeploymentUID: "w", PreRV: a.PreResourceVersion, PostRV: "opaque-post", PreTemplateHash: "t0", TargetTemplateHash: "t1", AttemptAnnotation: a.ActionID, Hostname: a.Hostname, Source: "fresh-get", Ref: "apply-snapshot.json", Observed: fixtureStamp(in, 28)}, Interventions: []Intervention{}}
	in.Probe = nil
	sample := func(sec float64, uid string, status int) {
		start, end := fixtureStamp(in, sec), fixtureStamp(in, sec+0.1)
		p := ProbeRecord{T: start.At, LatencyMs: 100, OK: status == 200, Status: status, PodUID: uid, Node: in.ExpectNode, Arch: "amd64", Version: in.ExpectVersion, PathID: svc.PathID, URL: svc.URL}
		result := "response"
		if status != 200 {
			p.Error = "HTTP failure"
			result = "service-failure"
		}
		in.Probe = append(in.Probe, p)
		e.Probe = append(e.Probe, ProbeTiming{Start: start, End: end, Result: result})
	}
	for i := 0; i <= 30; i++ {
		sample(-40+float64(i), "old", 200)
	}
	sample(1, "", 503)
	sample(2, "", 503)
	for i := 0; i <= 30; i++ {
		sample(50+float64(i), "new", 200)
	}
	for i, actor := range []string{"watchdog", "admin", "cleanup", "desired-state"} {
		e.Coverage = append(e.Coverage, ActorCoverage{Actor: actor, Complete: ptr(true), Start: fixtureStamp(in, -5-float64(i)), End: fixtureStamp(in, 90+float64(i)), Ref: actor + ".jsonl"})
	}
	f := StopFence{Version: 2, PolicyUID: e.Identity.PolicyUID, PolicyName: in.Incident.Spec.PolicyRef.Name, DeploymentUID: "w", ClosedThroughGeneration: 2}
	e.Stop = &StopProof{PolicyUID: e.Identity.PolicyUID, Generation: 2, Enabled: ptr(false), Mode: modeEnforce, Deleting: ptr(false), ActiveIncidentAbsent: ptr(true), QuiescedStatus: "True", QuiescedReason: "FenceClosed", ObservedGeneration: 2, LiveFence: f, AcknowledgedFence: f, Requested: fixtureStamp(in, 100), Acknowledged: fixtureStamp(in, 101), Ref: "stop.json"}
	e.Cleanup = &CleanupProof{Status: "succeeded", Completed: fixtureStamp(in, 110), WorkloadChecked: ptr(true), OldPodsChecked: ptr(true), ServiceChecked: ptr(true), Ref: "cleanup.json"}
	bindFixture(t, &in, &e)
	return in, e
}
func bindFixture(t *testing.T, in *ReportInput, e *RuntimeEvidence) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	var buf bytes.Buffer
	for _, p := range in.Probe {
		if err := json.NewEncoder(&buf).Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	files["probe"] = buf.Bytes()
	for role, v := range map[string]any{"incident": in.Incident, "pods": in.Pods, "env": in.Env} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		files[role] = b
	}
	in.SourceHashes = map[string]string{}
	e.Sources = nil
	for _, role := range []string{"probe", "incident", "pods", "env"} {
		h := digestBytes(files[role])
		in.SourceHashes[role] = h
		e.Sources = append(e.Sources, RawSource{Role: role, Ref: role + ".json", SHA256: h})
	}
	return files
}
func hasReason(r AcceptanceResult, reason string) bool {
	return slices.Contains(r.ServiceRecovery.Reasons, reason) || slices.Contains(r.Attribution.Reasons, reason) || slices.Contains(r.Cleanup.Reasons, reason)
}
func TestAcceptanceNormalAndJSONRoundTrip(t *testing.T) {
	in, e := acceptanceFixture(t)
	r := Acceptance(in, e)
	if !r.OverallPass || r.LiveProof || r.ControllerOutcome != "RECOVERED" {
		t.Fatalf("%+v", r)
	}
	// time.Time monotonic state is never needed, even for ReportInput round trips.
	b, _ := json.Marshal(e)
	var decoded RuntimeEvidence
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(in)
	var din ReportInput
	if err := json.Unmarshal(b, &din); err != nil {
		t.Fatal(err)
	}
	if got := Acceptance(din, decoded); !reflect.DeepEqual(r, got) {
		t.Fatalf("round trip changed result: %+v", got)
	}
	eq(t, "stable duration", r.ServiceRecoveryDuration.Seconds, 79.1)
}
func TestAcceptanceNegativeControls(t *testing.T) {
	cases := []struct {
		name, reason string
		mutate       func(*ReportInput, *RuntimeEvidence)
	}{
		{"no outage", "NO_OUTAGE_OBSERVED", func(in *ReportInput, e *RuntimeEvidence) {
			for i := 31; i <= 32; i++ {
				in.Probe[i].OK = true
				in.Probe[i].Status = 200
				in.Probe[i].Error = ""
				in.Probe[i].PodUID = "old"
				e.Probe[i].Result = "response"
			}
		}},
		{"recorder error", "NO_OUTAGE_OBSERVED", func(in *ReportInput, e *RuntimeEvidence) {
			e.Probe[31].Result = "recorder-error"
			e.Probe[32].Result = "recorder-error"
		}},
		{"before apply", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) { e.Apply.Observed = fixtureStamp(*in, 51.5) }},
		{"five in four seconds", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) { in.Probe = in.Probe[:38]; e.Probe = e.Probe[:38] }},
		{"29 seconds", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			in.Probe = in.Probe[:len(in.Probe)-1]
			e.Probe = e.Probe[:len(e.Probe)-1]
		}},
		{"long gap", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			in.Probe = append(in.Probe[:47], in.Probe[51:]...)
			e.Probe = append(e.Probe[:47], e.Probe[51:]...)
		}},
		{"old only", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			for i := 33; i < len(in.Probe); i++ {
				in.Probe[i].PodUID = "old"
			}
		}},
		{"middle different UID", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) { in.Probe[48].PodUID = "different" }},
		{"middle 503", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			in.Probe[48].OK = false
			in.Probe[48].Status = 503
			e.Probe[48].Result = "service-failure"
		}},
		{"middle redirect", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) { in.Probe[48].OK = false; in.Probe[48].Status = 302 }},
		{"service down controller recovered", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			for i := 33; i < len(in.Probe); i++ {
				in.Probe[i].OK = false
				in.Probe[i].Status = 503
				e.Probe[i].Result = "service-failure"
			}
		}},
		{"wrong version", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			for i := 33; i < len(in.Probe); i++ {
				in.Probe[i].Version = "wrong"
			}
		}},
		{"wrong arch", "SERVICE_NOT_STABLE", func(in *ReportInput, e *RuntimeEvidence) {
			for i := 33; i < len(in.Probe); i++ {
				in.Probe[i].Arch = "arm64"
			}
		}},
		{"coverage missing", "ACTOR_COVERAGE_MISSING", func(in *ReportInput, e *RuntimeEvidence) { e.Coverage = e.Coverage[1:] }},
		{"coverage false", "ACTOR_COVERAGE_INCOMPLETE", func(in *ReportInput, e *RuntimeEvidence) { e.Coverage[0].Complete = ptr(false) }},
		{"watchdog", "EXTERNAL_INTERVENTION_ORDER", func(in *ReportInput, e *RuntimeEvidence) {
			e.Interventions = []Intervention{{Actor: "watchdog", Kind: "restore", TargetUID: "fault-node", Requested: fixtureStamp(*in, 49), Ref: "watchdog.log"}}
		}},
		{"admin desired state", "EXTERNAL_INTERVENTION_ORDER", func(in *ReportInput, e *RuntimeEvidence) {
			e.Interventions = []Intervention{{Actor: "admin", Kind: "desired-state", TargetUID: "w", Requested: fixtureStamp(*in, 49), Ref: "admin.log"}}
		}},
		{"only stop requested", "STOP_PROOF_MISSING", func(in *ReportInput, e *RuntimeEvidence) { e.Stop = nil }},
		{"stop generation", "STOP_IDENTITY_GENERATION_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.ObservedGeneration = 1 }},
		{"stop UID", "STOP_IDENTITY_GENERATION_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.PolicyUID = "replacement" }},
		{"active incident", "STOP_NOT_QUIESCED", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.ActiveIncidentAbsent = ptr(false) }},
		{"stop requested unacked", "STOP_NOT_QUIESCED", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.QuiescedStatus = "" }},
		{"fence generation", "STOP_FENCE_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.LiveFence.ClosedThroughGeneration = 1 }},
		{"fence owner", "STOP_FENCE_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Stop.LiveFence.PolicyUID = "other" }},
		{"cleanup missing", "CLEANUP_MISSING", func(in *ReportInput, e *RuntimeEvidence) { e.Cleanup = nil }},
		{"cleanup failed", "CLEANUP_FAILED", func(in *ReportInput, e *RuntimeEvidence) { e.Cleanup.Status = "failed" }},
		{"incident UID missing", "INCIDENT_UID_MISSING", func(in *ReportInput, e *RuntimeEvidence) { in.Incident.UID = "" }},
		{"action mismatch", "ACTION_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Identity.ActionID = "other" }},
		{"policy hash", "POLICY_HASH_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Identity.PolicyHash = "other" }},
		{"deployment UID", "DEPLOYMENT_UID_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Identity.DeploymentUID = "other" }},
		{"raw source missing", "RAW_SOURCE_MISSING", func(in *ReportInput, e *RuntimeEvidence) { e.Sources = nil }},
		{"source hash mismatch", "RAW_SOURCE_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { e.Sources[0].SHA256 = strings.Repeat("0", 64) }},
		{"path mismatch", "PROBE_PATH_MISMATCH", func(in *ReportInput, e *RuntimeEvidence) { in.Probe[33].PathID = "direct-pod" }},
		{"reverse samples", "PROBE_INPUT_ORDER", func(in *ReportInput, e *RuntimeEvidence) {
			in.Probe[34], in.Probe[35] = in.Probe[35], in.Probe[34]
			e.Probe[34], e.Probe[35] = e.Probe[35], e.Probe[34]
		}},
		{"duplicate samples", "DUPLICATE_SEQUENCE", func(in *ReportInput, e *RuntimeEvidence) { in.Probe[34] = in.Probe[33]; e.Probe[34] = e.Probe[33] }},
		{"elapsed missing", "STAMP_MISSING", func(in *ReportInput, e *RuntimeEvidence) { e.Apply.Observed.ElapsedMS = nil }},
		{"cross segment", "PATCHED_APPLY_CONFIRMATION_CLOCK_UNPROVEN", func(in *ReportInput, e *RuntimeEvidence) { e.Apply.Observed.Segment = "restarted" }},
		{"old fixture contradiction", "SERVICE_BEFORE_PATCHED", func(in *ReportInput, e *RuntimeEvidence) {
			in.Probe[33].T = in.InjectAt.Add(10 * time.Second)
			e.Probe[33].Start = fixtureStamp(*in, 10)
			e.Probe[33].End = fixtureStamp(*in, 10.1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, e := acceptanceFixture(t)
			tc.mutate(&in, &e)
			r := Acceptance(in, e)
			if r.OverallPass || !hasReason(r, tc.reason) {
				t.Fatalf("want %s: %+v", tc.reason, r)
			}
			if r.ControllerOutcome != "RECOVERED" {
				t.Fatal("controller fact lost")
			}
		})
	}
}
func TestAcceptanceOptionalReadyClockAndLateCleanup(t *testing.T) {
	in, e := acceptanceFixture(t)
	// Ready lastTransitionTime on kubelet clock is optional duration evidence;
	// Ready observed may arrive after the Service has already become healthy.
	e.ReadySource.Recorder = "kubelet"
	e.ReadyObserved = fixtureStamp(in, 85)
	e.Interventions = []Intervention{{Actor: "cleanup", Kind: "restore", TargetUID: "fault-node", Requested: fixtureStamp(in, 102), Confirmed: ptr(fixtureStamp(in, 103)), Ref: "cleanup-restore.log"}}
	r := Acceptance(in, e)
	if !r.OverallPass || r.PlacementDuration.Seconds != nil || r.PlacementDuration.Reason == "" {
		t.Fatalf("%+v", r)
	}
	// An opaque post RV that sorts lexically before pre RV is accepted.
	e.Apply.PostRV = "a"
	if !Acceptance(in, e).OverallPass {
		t.Fatal("RV was ordered")
	}
}
func TestAcceptanceUnknownCoreClockAndCalibration(t *testing.T) {
	in, e := acceptanceFixture(t)
	e.Decision.Recorder = "controller"
	e.Patched.Recorder = "controller"
	if r := Acceptance(in, e); r.OverallPass || r.Attribution.Status != "UNPROVEN" {
		t.Fatalf("%+v", r)
	}
	e.Clocks = []ClockBound{{Recorder: "controller", Segment: "one", UncertaintyMS: ptr(1.0), Ref: "clock.json"}, {Recorder: "observer", Segment: "one", UncertaintyMS: ptr(1.0), Ref: "clock.json"}}
	if r := Acceptance(in, e); !r.OverallPass {
		t.Fatalf("%+v", r)
	}
	e.Clocks[0].UncertaintyMS = ptr(60000.0)
	if r := Acceptance(in, e); r.OverallPass || r.Attribution.Status != "UNPROVEN" {
		t.Fatalf("%+v", r)
	}
}
func writeAcceptanceFiles(t *testing.T, in *ReportInput, e *RuntimeEvidence) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	files := bindFixture(t, in, e)
	for role, b := range files {
		if err := os.WriteFile(filepath.Join(dir, role+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.MarshalIndent(e, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "evidence.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, []string{"report", "-acceptance", "runtime-v1", "-evidence", filepath.Join(dir, "evidence.json"), "-probe", filepath.Join(dir, "probe.json"), "-incident", filepath.Join(dir, "incident.json"), "-pods", filepath.Join(dir, "pods.json"), "-env", filepath.Join(dir, "env.json"), "-mode", modeEnforce, "-scenario", in.Scenario, "-expect-node", in.ExpectNode, "-expect-version", in.ExpectVersion, "-platform-digests", strings.Join(in.PlatformDigests, ","), "-inject-at", in.InjectAt.Format(time.RFC3339Nano), "-out", filepath.Join(dir, "summary.json")}
}
func TestAcceptanceCLIAndSummary(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"synthetic", "observed"} {
		in, e := acceptanceFixture(t)
		e.EvidenceKind = kind
		dir, args := writeAcceptanceFiles(t, &in, &e)
		if err := run(args, io.Discard); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
		if bytes.Contains(b, []byte(`"pass":`)) {
			t.Fatal("legacy pass leaked")
		}
		dest := filepath.Join(root, kind)
		os.Mkdir(dest, 0700)
		os.WriteFile(filepath.Join(dest, "summary.json"), b, 0600)
	}
	in, e := acceptanceFixture(t)
	e.Cleanup.Status = "failed"
	dir, args := writeAcceptanceFiles(t, &in, &e)
	if err := run(args, io.Discard); err == nil {
		t.Fatal("FAIL needs nonzero")
	}
	var r AcceptanceResult
	readJSON(filepath.Join(dir, "summary.json"), &r)
	if r.OverallPass || r.ServiceRecovery.Status != "PASS" || r.Cleanup.Status != "FAIL" {
		t.Fatalf("%+v", r)
	}
	missing := append([]string{}, args...)
	missing[4] = ""
	if err := run(missing, io.Discard); err == nil {
		t.Fatal("missing evidence")
	}
	legacy := filepath.Join(root, "legacy")
	os.Mkdir(legacy, 0700)
	b, _ := json.Marshal(Report(baseInput(t)))
	os.WriteFile(filepath.Join(legacy, "summary.json"), b, 0600)
	var out bytes.Buffer
	if err := summarize(root, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Legacy v1", "Runtime v2", "| synthetic | azure-loaded | 1/1 |", "| observed | azure-loaded | 1/1 |", "independently proven live recoveries: 0"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("missing %s: %s", s, out.String())
		}
	}
}
func TestStrictEvidenceInputs(t *testing.T) {
	for name, data := range map[string]string{"duplicate": `{"version":"a","version":"b"}`, "case alias": `{"version":"a","Version":"b"}`, "trailing": `{} {}`, "unknown": `{"version":"demo-a-evidence/v1","evidenceKind":"synthetic","oops":1}`, "version": `{"version":"v9","evidenceKind":"synthetic"}`, "null": `null`, "array": `[]`, "bad JSON": `{"version":`} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "evidence.json")
			os.WriteFile(p, []byte(data), 0600)
			if _, err := readEvidence(p); err == nil {
				t.Fatal("malformed accepted")
			}
		})
	}
	if _, err := ReadProbe(strings.NewReader(strings.Repeat(" ", maxProbeLineBytes+1))); err == nil {
		t.Fatal("oversized line")
	}
	if _, err := ReadProbe(strings.NewReader(`{"ok":true,"ok":false}`)); err == nil {
		t.Fatal("duplicate probe field")
	}
	p := filepath.Join(t.TempDir(), "large.json")
	os.WriteFile(p, bytes.Repeat([]byte(" "), maxInputBytes+1), 0600)
	if _, err := readEvidence(p); err == nil {
		t.Fatal("oversized evidence")
	}
}

func retimeSample(in *ReportInput, e *RuntimeEvidence, i int, sec float64) {
	in.Probe[i].T = fixtureStamp(*in, sec).At
	e.Probe[i].Start = fixtureStamp(*in, sec)
	e.Probe[i].End = fixtureStamp(*in, sec+float64(in.Probe[i].LatencyMs)/1000)
}
func TestAcceptanceTimingBoundaries(t *testing.T) {
	t.Run("exact max gap", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		p, tm := in.Probe, e.Probe
		in.Probe = nil
		e.Probe = nil
		for i := range p {
			if i < 33 || (i-33)%3 == 0 {
				in.Probe = append(in.Probe, p[i])
				e.Probe = append(e.Probe, tm[i])
			}
		}
		if r := Acceptance(in, e); !r.OverallPass {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("request straddles apply", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		retimeSample(&in, &e, 33, 27.95)
		in.Probe[33].LatencyMs = 100
		retimeSample(&in, &e, 33, 27.95)
		r := Acceptance(in, e)
		if r.OverallPass || !hasReason(r, "SERVICE_BEFORE_READY_SOURCE") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("29.999 seconds", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		retimeSample(&in, &e, len(in.Probe)-1, 79.999)
		r := Acceptance(in, e)
		if r.OverallPass || r.ServiceRecovery.Status != "UNPROVEN" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("unsegmented UTC step", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		e.Apply.Observed.At = e.Apply.Observed.At.Add(200 * time.Millisecond)
		r := Acceptance(in, e)
		if r.OverallPass || !hasReason(r, "CLOCK_DISCONTINUITY_REQUIRES_SEGMENT") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("uncertain external intervention", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		s := fixtureStamp(in, 49)
		s.Recorder = "admin-recorder"
		e.Interventions = []Intervention{{Actor: "admin", Kind: "restore", TargetUID: "fault-node", Requested: s, Ref: "admin.log"}}
		r := Acceptance(in, e)
		if r.OverallPass || r.Attribution.Status != "UNPROVEN" || !hasReason(r, "EXTERNAL_INTERVENTION_CLOCK_UNPROVEN") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("missing Pod identity", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		e.Identity.PodUID = ""
		r := Acceptance(in, e)
		if r.OverallPass || r.Attribution.Status != "UNPROVEN" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("missing Node identity", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		e.Identity.NodeUID = ""
		r := Acceptance(in, e)
		if r.OverallPass || r.Attribution.Status != "UNPROVEN" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("explicit intervention log", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		e.Interventions = nil
		r := Acceptance(in, e)
		if r.OverallPass || !hasReason(r, "INTERVENTION_LOG_MISSING") {
			t.Fatalf("%+v", r)
		}
	})
}
func TestAcceptanceSummaryRejectsUnsupportedAndHybrid(t *testing.T) {
	for _, data := range []string{`{"schemaVersion":"v9","pass":true}`, `{"schemaVersion":"demo-a-summary/v2","acceptance":"runtime-v1","evidenceKind":"synthetic","pass":true}`, `{"schemaVersion":"demo-a-summary/v2","acceptance":"runtime-v1","evidenceKind":"synthetic","overallPass":true}`, `{"schemaVersion":"demo-a-summary/v1","schemaVersion":"demo-a-summary/v2"}`} {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "summary.json"), []byte(data), 0600)
		if err := summarize(d, io.Discard); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestAcceptanceClockKeyCannotAlias(t *testing.T) {
	in, e := acceptanceFixture(t)
	a, b := fixtureStamp(in, 1), fixtureStamp(in, 2)
	a.Recorder = "observer/one"
	a.Segment = "two"
	b.Recorder = "observer"
	b.Segment = "one/two"
	if d := delta(e, a, b); d.Seconds != nil {
		t.Fatalf("distinct clock keys subtracted: %+v", d)
	}
}
func TestAcceptanceDelayedApplyObservationIsNotReadyContradiction(t *testing.T) {
	in, e := acceptanceFixture(t)
	e.ReadySource = fixtureStamp(in, 27.5)
	attempt(&in).PodReadyAt = &metav1.Time{Time: e.ReadySource.At}
	r := Acceptance(in, e)
	if !r.OverallPass || r.PlacementDuration.Seconds != nil || r.PlacementDuration.Reason != "READY_SOURCE_PRECEDES_APPLY_OBSERVATION" {
		t.Fatalf("%+v", r)
	}
}
func TestAcceptanceDuplicateObjectIdentities(t *testing.T) {
	in, e := acceptanceFixture(t)
	in.Pods.Items = append(in.Pods.Items, in.Pods.Items[1])
	r := Acceptance(in, e)
	if r.OverallPass || !hasReason(r, "DUPLICATE_ATTEMPT_POD") {
		t.Fatalf("%+v", r)
	}
	in, e = acceptanceFixture(t)
	a := attempt(&in)
	a.Decision.Candidates = append(a.Decision.Candidates, a.Decision.Candidates[1])
	r = Acceptance(in, e)
	if r.OverallPass || !hasReason(r, "DUPLICATE_CANDIDATE_IDENTITY") {
		t.Fatalf("%+v", r)
	}
}
