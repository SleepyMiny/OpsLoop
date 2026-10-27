package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	v1alpha1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These raw synthetic fields are explicit input, never inferred by the checker.
func fixturePodIdentity(t *testing.T, in *ReportInput, change func(map[string]any)) {
	t.Helper()
	b, _ := json.Marshal(in.Pods)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	p := raw["items"].([]any)[1].(map[string]any)
	m := p["metadata"].(map[string]any)
	m["namespace"] = in.Incident.Namespace
	m["name"] = attempt(in).PodName
	m["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "uid": "rs-uid", "controller": true}}
	p["spec"].(map[string]any)["nodeName"] = in.ExpectNode
	if change != nil {
		change(p)
	}
	b, _ = json.Marshal(raw)
	var pods PodList
	if err := strictDecode(b, &pods, false); err != nil {
		t.Fatal(err)
	}
	in.Pods = pods
}

func TestP2BTypedKeys(t *testing.T) {
	cases := []struct {
		name, single, pair string
		target             func() any
	}{
		{"cleanup", `{"cleanup":{"status":"failed"}}`, `{"cleanup":{"status":"failed","ſtatus":"succeeded"}}`, func() any { return &RuntimeEvidence{} }},
		{"stop", `{"stop":{"quiescedStatus":"False"}}`, `{"stop":{"quiescedStatus":"False","quieſcedStatus":"True"}}`, func() any { return &RuntimeEvidence{} }},
		{"fence", `{"stop":{"liveFence":{"closedThroughGeneration":2}}}`, `{"stop":{"liveFence":{"closedThroughGeneration":2,"cloſedThroughGeneration":3}}}`, func() any { return &RuntimeEvidence{} }},
		{"probe", `{"status":200}`, `{"status":200,"ſtatus":503}`, func() any { return &ProbeRecord{} }},
		{"env", `{"runtimeService":{"namespace":"n"}}`, `{"runtimeService":{"namespace":"n","nameſpace":"x"}}`, func() any { return &Env{} }},
		{"incident", `{"spec":{"policyHash":"h"}}`, `{"spec":{"policyHash":"h","policyHaſh":"x"}}`, func() any { return &v1alpha1.RecoveryIncident{} }},
		{"embedded TypeMeta", `{"kind":"RecoveryIncident"}`, `{"kind":"RecoveryIncident","Kind":"other"}`, func() any { return &v1alpha1.RecoveryIncident{} }},
		{"ObjectMeta", `{"metadata":{"namespace":"n"}}`, `{"metadata":{"namespace":"n","nameſpace":"x"}}`, func() any { return &v1alpha1.RecoveryIncident{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := strictDecode([]byte(c.single), c.target(), false); err != nil {
				t.Fatal(err)
			}
			for _, data := range []string{c.pair, reverseFixtureObjects(t, c.pair), strings.ReplaceAll(c.pair, "ſ", `\u017f`), strings.ReplaceAll(reverseFixtureObjects(t, c.pair), "ſ", `\u017f`), strings.ReplaceAll(c.pair, "K", `\u212a`)} {
				if err := strictDecode([]byte(data), c.target(), false); err == nil || !strings.Contains(err.Error(), "duplicate JSON field") {
					t.Errorf("want duplicate: %s: %v", data, err)
				}
			}
		})
	}
	for _, key := range []string{"status", "STATUS", "ſtatus", `\u017ftatus`, `\u0073tatus`} {
		for _, reverse := range []bool{false, true} {
			a, b := `"status":"failed"`, `"`+key+`":"succeeded"`
			if reverse {
				a, b = b, a
			}
			var e RuntimeEvidence
			if err := strictDecode([]byte(`{"cleanup":{`+a+`,`+b+`}}`), &e, true); err == nil || !strings.Contains(err.Error(), "duplicate JSON field") {
				t.Errorf("alias %q reverse=%v: %v", key, reverse, err)
			}
		}
	}
	for _, key := range []string{"STATUS", "ſtatus", `\u017ftatus`} {
		var e RuntimeEvidence
		if err := strictDecode([]byte(`{"cleanup":{"`+key+`":"failed"}}`), &e, true); err != nil || e.Cleanup.Status != "failed" {
			t.Fatalf("single alias: %v %+v", err, e)
		}
	}
}

func TestP2BFreeMapKeys(t *testing.T) {
	for _, target := range []any{&PodList{}, &v1alpha1.RecoveryIncident{}} {
		good := `{"metadata":{"labels":{"App":"a","app":"b"},"annotations":{"App":"a","app":"b"}}}`
		if _, ok := target.(*PodList); ok {
			good = `{"items":[` + good + `],"additional":{"App":1,"app":2}}`
		}
		if err := strictDecode([]byte(good), target, false); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{strings.Replace(good, `"app":"b"`, `"App":"b"`, 1), strings.Replace(good, `"app":"b"`, `"\u0041pp":"b"`, 1)} {
			if err := strictDecode([]byte(bad), target, false); err == nil || !strings.Contains(err.Error(), "duplicate JSON field") {
				t.Errorf("map duplicate: %v", err)
			}
		}
	}
}

func TestP2BPodIdentity(t *testing.T) {
	tests := []struct {
		name, status, reason string
		change               func(map[string]any)
	}{
		{"normal", "PASS", "", nil},
		{"name", "FAIL", "POD_NAME_MISMATCH", func(p map[string]any) { p["metadata"].(map[string]any)["name"] = "other" }},
		{"namespace", "FAIL", "POD_NAMESPACE_MISMATCH", func(p map[string]any) { p["metadata"].(map[string]any)["namespace"] = "other" }},
		{"node", "FAIL", "POD_NODE_MISMATCH", func(p map[string]any) { p["spec"].(map[string]any)["nodeName"] = "other" }},
		{"owner", "FAIL", "POD_CONTROLLER_OWNER_MISMATCH", func(p map[string]any) {
			p["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "other"
		}},
		{"missing name", "UNPROVEN", "POD_NAME_MISSING", func(p map[string]any) { delete(p["metadata"].(map[string]any), "name") }},
		{"missing namespace", "UNPROVEN", "POD_NAMESPACE_MISSING", func(p map[string]any) { delete(p["metadata"].(map[string]any), "namespace") }},
		{"missing node", "UNPROVEN", "POD_NODE_MISSING", func(p map[string]any) { delete(p["spec"].(map[string]any), "nodeName") }},
		{"missing owner", "UNPROVEN", "POD_CONTROLLER_OWNER_MISSING", func(p map[string]any) { delete(p["metadata"].(map[string]any), "ownerReferences") }},
		{"non controller", "UNPROVEN", "POD_CONTROLLER_OWNER_MISSING", func(p map[string]any) {
			p["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["controller"] = false
		}},
		{"wrong kind", "FAIL", "POD_CONTROLLER_OWNER_MISMATCH", func(p map[string]any) {
			p["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["kind"] = "Deployment"
		}},
		{"ambiguous controller", "FAIL", "POD_CONTROLLER_OWNER_AMBIGUOUS", func(p map[string]any) {
			m := p["metadata"].(map[string]any)
			a := m["ownerReferences"].([]any)
			m["ownerReferences"] = append(a, a[0])
		}},
		{"missing owner UID", "UNPROVEN", "POD_CONTROLLER_OWNER_MISSING", func(p map[string]any) {
			delete(p["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any), "uid")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, e := acceptanceFixture(t)
			fixturePodIdentity(t, &in, tc.change)
			bindFixture(t, &in, &e)
			r := Acceptance(in, e)
			if r.Attribution.Status != tc.status || (tc.reason != "" && !hasReason(r, tc.reason)) || r.ServiceRecovery.Status != "PASS" || r.Cleanup.Status != "PASS" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func insertEarly(t *testing.T, in *ReportInput, e *RuntimeEvidence, sec float64) {
	t.Helper()
	e.ReadySource = fixtureStamp(*in, 27.5)
	attempt(in).PodReadyAt = &metav1.Time{Time: e.ReadySource.At}
	p, tm := in.Probe[33], e.Probe[33]
	p.T = fixtureStamp(*in, sec).At
	tm.Start = fixtureStamp(*in, sec)
	tm.End = fixtureStamp(*in, sec+.1)
	in.Probe = append(in.Probe[:33], append([]ProbeRecord{p}, in.Probe[33:]...)...)
	e.Probe = append(e.Probe[:33], append([]ProbeTiming{tm}, e.Probe[33:]...)...)
}
func TestP2BEarlyRequests(t *testing.T) {
	for _, sec := range []float64{27.6, 27.95, 26.5} {
		t.Run(fmt.Sprintf("%.2f", sec), func(t *testing.T) {
			in, e := acceptanceFixture(t)
			fixturePodIdentity(t, &in, nil)
			insertEarly(t, &in, &e, sec)
			bindFixture(t, &in, &e)
			r := Acceptance(in, e)
			if sec > 27 {
				if !r.OverallPass {
					t.Fatalf("%+v", r)
				}
			} else if r.Attribution.Status != "FAIL" || !hasReason(r, "SERVICE_BEFORE_PATCHED") {
				t.Fatalf("%+v", r)
			}
		})
	}
	t.Run("excluded request cannot extend window", func(t *testing.T) {
		in, e := acceptanceFixture(t)
		fixturePodIdentity(t, &in, nil)
		insertEarly(t, &in, &e, 27.95)
		for i := 34; i < len(in.Probe); i++ {
			retimeSample(&in, &e, i, 28.95+float64(i-34))
		}
		in.Probe = in.Probe[:len(in.Probe)-1]
		e.Probe = e.Probe[:len(e.Probe)-1]
		bindFixture(t, &in, &e)
		r := Acceptance(in, e)
		if r.ServiceRecovery.Status != "UNPROVEN" || r.Attribution.Status == "FAIL" || !hasReason(r, "SERVICE_NOT_STABLE") {
			t.Fatalf("%+v", r)
		}
	})
}
func TestP2BStopClassification(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		change               func(*StopProof)
	}{
		{"normal", "PASS", "", func(s *StopProof) {}},
		{"observe only", "PASS", "", func(s *StopProof) { s.Enabled = ptr(true); s.Mode = modeObserveOnly }},
		{"active", "FAIL", "STOP_ACTIVE_INCIDENT", func(s *StopProof) { s.ActiveIncidentAbsent = ptr(false) }},
		{"quiesced false", "FAIL", "STOP_QUIESCED_FALSE", func(s *StopProof) { s.QuiescedStatus = "False" }},
		{"nil active", "UNPROVEN", "STOP_PROOF_INCOMPLETE", func(s *StopProof) { s.ActiveIncidentAbsent = nil }},
		{"unknown", "UNPROVEN", "STOP_NOT_QUIESCED", func(s *StopProof) { s.QuiescedStatus = "Unknown" }},
		{"missing", "UNPROVEN", "STOP_NOT_QUIESCED", func(s *StopProof) { s.QuiescedStatus = "" }},
		{"ack missing", "UNPROVEN", "STOP_ACK_CLOCK_UNPROVEN", func(s *StopProof) { s.Acknowledged = Stamp{} }},
		{"old state", "UNPROVEN", "STOP_IDENTITY_GENERATION_MISSING", func(s *StopProof) {
			s.ObservedGeneration = 0
			s.ActiveIncidentAbsent = ptr(false)
			s.QuiescedStatus = "False"
		}},
		{"fail dominates", "FAIL", "STOP_ACTIVE_INCIDENT", func(s *StopProof) { s.ActiveIncidentAbsent = ptr(false); s.Acknowledged = Stamp{}; s.Ref = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, e := acceptanceFixture(t)
			fixturePodIdentity(t, &in, nil)
			bindFixture(t, &in, &e)
			tc.change(e.Stop)
			r := Acceptance(in, e)
			if r.Cleanup.Status != tc.status || (tc.reason != "" && !hasReason(r, tc.reason)) || r.ControllerOutcome != "RECOVERED" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestP2BInputLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"probe", make([]ProbeTiming, maxProbeRecords+1)},
		{"clocks", make([]ClockBound, 65)},
		{"coverage", make([]ActorCoverage, 17)},
		{"sources", make([]RawSource, 17)},
		{"interventions", make([]Intervention, 1001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{"version": evidenceVersion, "evidenceKind": "synthetic", tc.name: tc.value}
			b, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readEvidence(p); err == nil || !strings.Contains(err.Error(), "collection exceeds record limits") {
				t.Fatalf("%v", err)
			}
		})
	}
	var v Env
	for _, tc := range []struct{ data, reason string }{
		{strings.Repeat(`{"extra":`, 65) + `0` + strings.Repeat(`}`, 65), "nesting exceeds 64"},
		{`{} {}`, "trailing JSON data"},
	} {
		if err := strictDecode([]byte(tc.data), &v, false); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("want %s: %v", tc.reason, err)
		}
	}
	if _, err := ReadProbe(strings.NewReader(strings.Repeat("{}\n", maxProbeRecords+1))); err == nil || !strings.Contains(err.Error(), "exceeds 10000 records") {
		t.Fatalf("%v", err)
	}
}

func TestP2BPodExistingGuards(t *testing.T) {
	for _, tc := range []struct {
		name, reason, status string
		change               func(*ReportInput)
	}{
		{"duplicate UID", "DUPLICATE_ATTEMPT_POD", "FAIL", func(in *ReportInput) { in.Pods.Items = append(in.Pods.Items, in.Pods.Items[1]) }},
		{"absent Pod", "ATTEMPT_POD_MISSING", "FAIL", func(in *ReportInput) { in.Pods.Items = in.Pods.Items[:1] }},
		{"image mismatch", "IMAGE_MISMATCH", "FAIL", func(in *ReportInput) { in.Pods.Items[1].Status.ContainerStatuses[0].ImageID = "sha256:foreign" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, e := acceptanceFixture(t)
			tc.change(&in)
			bindFixture(t, &in, &e)
			r := Acceptance(in, e)
			if r.Attribution.Status != tc.status || !hasReason(r, tc.reason) {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestP2BStaleStopCounterexample(t *testing.T) {
	in, e := acceptanceFixture(t)
	e.Stop.ObservedGeneration = 1
	e.Stop.ActiveIncidentAbsent = ptr(false)
	e.Stop.QuiescedStatus = "False"
	r := Acceptance(in, e)
	if r.Cleanup.Status != "FAIL" || !hasReason(r, "STOP_IDENTITY_GENERATION_MISMATCH") || hasReason(r, "STOP_ACTIVE_INCIDENT") || hasReason(r, "STOP_QUIESCED_FALSE") {
		t.Fatalf("%+v", r)
	}
}

func TestP2BMissingAttemptIdentity(t *testing.T) {
	for _, key := range []string{"PodName", "PodNodeName", "NodeName", "ReplicaSetUID"} {
		t.Run(key, func(t *testing.T) {
			in, e := acceptanceFixture(t)
			reflect.ValueOf(attempt(&in)).Elem().FieldByName(key).SetString("")
			bindFixture(t, &in, &e)
			r := Acceptance(in, e)
			if r.Attribution.Status != "UNPROVEN" || r.OverallPass {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// Only fixture construction uses maps; product validation always sees raw tokens.
func reverseFixtureObjects(t *testing.T, data string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatal(err)
	}
	var encode func(any) string
	encode = func(v any) string {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(keys)))
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				b, _ := json.Marshal(k)
				pairs = append(pairs, string(b)+":"+encode(x[k]))
			}
			return "{" + strings.Join(pairs, ",") + "}"
		case []any:
			parts := make([]string, len(x))
			for i := range x {
				parts[i] = encode(x[i])
			}
			return "[" + strings.Join(parts, ",") + "]"
		default:
			b, _ := json.Marshal(v)
			return string(b)
		}
	}
	return encode(v)
}
