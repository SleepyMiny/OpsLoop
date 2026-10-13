package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const injectAtFixture = "2026-10-13T00:00:00Z"

// Offsets from injectAt: old Pod before injection, 3s outage, short new-Pod runs broken by an
// old-Pod response and a failure, then the first 5-success run from the new Pod at +10s.
const probeFixture = `
{"t":"2026-10-12T23:59:58Z","ok":true,"status":200,"node":"sarmubuntuserver","pod":"p-old","podUID":"old","arch":"amd64","version":"abc123"}
{"t":"2026-10-12T23:59:59Z","ok":true,"status":200,"node":"sarmubuntuserver","pod":"p-old","podUID":"old","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:01Z","ok":false,"status":0,"error":"timeout"}
{"t":"2026-10-13T00:00:02Z","ok":false,"status":0,"error":"timeout"}
{"t":"2026-10-13T00:00:03Z","ok":false,"status":0,"error":"timeout"}
{"t":"2026-10-13T00:00:04Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:05Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:06Z","ok":true,"status":200,"node":"sarmubuntuserver","pod":"p-old","podUID":"old","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:07Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:08Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:09Z","ok":false,"status":503,"error":"HTTP 503"}
{"t":"2026-10-13T00:00:10Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:11Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:12Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:13Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
{"t":"2026-10-13T00:00:14Z","ok":true,"status":200,"node":"opsloop-control","pod":"p-new","podUID":"new","arch":"amd64","version":"abc123"}
`

const decisionFixture = `{"incidentID":"demo-api-12345678-00001","actionID":"demo-api-12345678-00001-a1","policyGeneration":1,
 "policyHash":"ph","workloadUID":"w","baselineTemplateHash":"t0","imageIndexDigest":"sha256:idx","queryVersion":"q1",
 "snapshotObservedAt":"2026-10-13T00:00:26Z","snapshotExpiresAt":"2026-10-13T00:01:56Z",
 "candidates":[
  {"nodeName":"sarmubuntuserver","nodeUID":"n0","hostname":"sarmubuntuserver","platform":"linux/amd64","eligible":false,"rejectReasons":["NODE_NOT_READY"],"measurements":{"allocatableCPUMilli":4000,"allocatableMemoryBytes":1,"requestedCPUMilli":0,"requestedMemoryBytes":0,"newPodCPUMilli":50,"newPodMemoryBytes":1,"reserveCPUMilli":400,"reserveMemoryBytes":1}},
  {"nodeName":"opsloop-control","nodeUID":"n1","hostname":"opsloop-control","platform":"linux/amd64","eligible":true,"measurements":{"cpuUtilizationPermille":300,"allocatableCPUMilli":4000,"allocatableMemoryBytes":1,"requestedCPUMilli":0,"requestedMemoryBytes":0,"newPodCPUMilli":50,"newPodMemoryBytes":1,"reserveCPUMilli":400,"reserveMemoryBytes":1}},
  {"nodeName":"opsloop-azure-worker","nodeUID":"n2","hostname":"opsloop-azure-worker","platform":"linux/arm64","eligible":false,"rejectReasons":["CPU_PRESSURE"],"measurements":{"cpuUtilizationPermille":930,"allocatableCPUMilli":2000,"allocatableMemoryBytes":1,"requestedCPUMilli":0,"requestedMemoryBytes":0,"newPodCPUMilli":50,"newPodMemoryBytes":1,"reserveCPUMilli":200,"reserveMemoryBytes":1}}],
 "selectedNodeUID":"n1","selectedNodeName":"opsloop-control","selectedHostname":"opsloop-control",
 "actionType":"Relocation","decisionReason":"SELECTED_HIGHEST_SCORE","decidedAt":"2026-10-13T00:00:26Z"}`

const incidentFixture = `{"apiVersion":"opsloop.io/v1alpha1","kind":"RecoveryIncident",
 "metadata":{"name":"demo-api-12345678-00001","namespace":"opsloop-demo"},
 "spec":{"policyRef":{"name":"demo-api","uid":"12345678"},"sequence":1,"policyGeneration":1,"policyHash":"ph",
  "policy":{"targetRef":{"name":"demo-api"},"candidateSelector":{},"verification":{"serviceName":"demo-api"}},
  "workload":{"name":"demo-api","uid":"w"},
  "trigger":{"class":"INFRASTRUCTURE_FAILURE","reason":"NODE_NOT_READY","signature":"s","ruleVersion":"r1","observedAt":"2026-10-13T00:00:25Z"},
  "baseline":{"templateHash":"t0","specHash":"s0","image":"i","imageDigest":"sha256:idx","replicas":1},
  "detectedAt":"2026-10-13T00:00:25Z","deadline":"2026-10-13T00:10:25Z"},
 "status":{"runtime":{"phase":"RECOVERED","reason":"VERIFIED","decision":` + decisionFixture + `,
  "attempts":[{"actionID":"demo-api-12345678-00001-a1","outcome":"Succeeded","nodeName":"opsloop-control","nodeUID":"n1",
   "hostname":"opsloop-control","preTemplateHash":"t0","targetTemplateHash":"t1","decision":` + decisionFixture + `,
   "reservedAt":"2026-10-13T00:00:26Z","podReadyAt":"2026-10-13T00:00:40Z","podName":"p-new","podUID":"new","podNodeName":"opsloop-control"}],
  "oldPodsOutstanding":[{"name":"p-old","uid":"old","nodeName":"sarmubuntuserver","terminating":true}]}}}`

const podsFixture = `{"items":[
 {"metadata":{"name":"p-old","uid":"old"},"spec":{"containers":[{"image":"acr.example/opsloop/demo-api@sha256:idx"}]},"status":{"containerStatuses":[{"imageID":"acr.example/opsloop/demo-api@sha256:amd"}]}},
 {"metadata":{"name":"p-new","uid":"new"},"spec":{"containers":[{"image":"acr.example/opsloop/demo-api@sha256:idx"}]},"status":{"containerStatuses":[{"imageID":"acr.example/opsloop/demo-api@sha256:amd"}]}}]}`

const envFixture = `{"runId":"20261013T000000Z","injectCmdSeconds":0.4,"imageCache":"warm","netPath":"pong via direct",
 "imageRef":"acr.example/opsloop/demo-api@sha256:idx","nodeTimeSync":{"host":"yes","opsloop-control":"yes"},
 "deploymentBefore":{"generation":3,"specSha256":"aa"},"deploymentAfter":{"generation":3,"specSha256":"aa"}}`

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}

func baseInput(t *testing.T) ReportInput {
	t.Helper()
	in := ReportInput{Mode: modeEnforce, Scenario: "azure-loaded", ExpectNode: "opsloop-control", ExpectVersion: "abc123",
		ExpectReject: map[string]string{"opsloop-azure-worker": "CPU_PRESSURE"}, PlatformDigests: []string{"sha256:amd", "sha256:arm"}}
	var err error
	if in.InjectAt, err = time.Parse(time.RFC3339Nano, injectAtFixture); err != nil {
		t.Fatal(err)
	}
	if in.Probe, err = ReadProbe(strings.NewReader(probeFixture)); err != nil {
		t.Fatal(err)
	}
	mustUnmarshal(t, incidentFixture, &in.Incident)
	mustUnmarshal(t, podsFixture, &in.Pods)
	mustUnmarshal(t, envFixture, &in.Env)
	return in
}

func eq(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func attempt(in *ReportInput) *v1alpha1.Attempt { return &in.Incident.Status.Runtime.Attempts[0] }

func TestReportEnforcePass(t *testing.T) {
	s := Report(baseInput(t))
	if !s.Pass {
		t.Fatalf("pass = false, notes %v", s.Notes)
	}
	eq(t, "tDetect", s.TDetectSeconds, 25)
	eq(t, "tDecide", s.TDecideSeconds, 1)
	eq(t, "tPlace", s.TPlaceSeconds, 14)
	// Excludes the old-Pod response at +6s and the 2-success runs at +4s and +7s.
	eq(t, "tRecover", s.TRecoverSeconds, 10)
	eq(t, "hostMaxGap", s.HostMaxGapSeconds, 3)
	if s.ServedArch != "amd64" || s.ServedVersion != "abc123" || s.ServedNode != "opsloop-control" || s.SelectedPlatform != "linux/amd64" {
		t.Errorf("served = %s/%s/%s platform %s", s.ServedArch, s.ServedVersion, s.ServedNode, s.SelectedPlatform)
	}
	if !s.ImageDigestMatch || s.PodImageID != "acr.example/opsloop/demo-api@sha256:amd" || s.OldPodsOutstanding != 1 || s.Attempts != 1 {
		t.Errorf("image %v %s old %d attempts %d", s.ImageDigestMatch, s.PodImageID, s.OldPodsOutstanding, s.Attempts)
	}
	if p := s.CandidateCPUPermille["opsloop-azure-worker"]; p == nil || *p != 930 || s.CandidateCPUPermille["sarmubuntuserver"] != nil {
		t.Errorf("candidateCPUPermille = %v", s.CandidateCPUPermille)
	}
	if !slices.Contains(s.Rejects["opsloop-azure-worker"], "CPU_PRESSURE") || s.NodeTimeSync["opsloop-control"] != "yes" {
		t.Errorf("rejects %v nodeTimeSync %v", s.Rejects, s.NodeTimeSync)
	}
	if slices.Contains(s.Notes, "tPlace cross-clock (controller VM vs target kubelet)") {
		t.Errorf("single-clock target got cross-clock note: %v", s.Notes)
	}
}

func TestReportFalsePaths(t *testing.T) {
	cases := map[string]func(in *ReportInput){
		"expect-reject not present": func(in *ReportInput) {
			in.ExpectReject = map[string]string{"opsloop-azure-worker": "INSUFFICIENT_REQUESTED_CPU"}
		},
		"expect node not eligible": func(in *ReportInput) {
			attempt(in).Decision.Candidates[1].Eligible = false
		},
		"imageID digest mismatch": func(in *ReportInput) {
			in.Pods.Items[1].Status.ContainerStatuses[0].ImageID = "acr.example/opsloop/demo-api@sha256:other"
		},
		"pod image not pinned to index": func(in *ReportInput) {
			in.Pods.Items[1].Spec.Containers[0].Image = "acr.example/opsloop/demo-api:latest"
		},
		"served version differs": func(in *ReportInput) { in.ExpectVersion = "def456" },
		"served node differs":    func(in *ReportInput) { attempt(in).PodNodeName = "opsloop-azure-worker" },
		"wrong selected node":    func(in *ReportInput) { in.ExpectNode = "opsloop-azure-worker" },
		"not recovered":          func(in *ReportInput) { in.Incident.Status.Runtime.Phase = "FAILED" },
		"no recover run":         func(in *ReportInput) { in.Probe = in.Probe[:13] },
		"retry": func(in *ReportInput) {
			rt := &in.Incident.Status.Runtime
			failed := rt.Attempts[0]
			failed.Outcome = "Failed"
			rt.Attempts = append([]v1alpha1.Attempt{failed}, rt.Attempts...)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := baseInput(t)
			mutate(&in)
			s := Report(in)
			if s.Pass {
				t.Fatalf("pass = true, want false")
			}
			if name == "retry" && !slices.Contains(s.Notes, "retry") {
				t.Errorf("notes %v lack retry", s.Notes)
			}
			if name == "imageID digest mismatch" && s.ImageDigestMatch {
				t.Errorf("imageDigestMatch = true")
			}
		})
	}
}

func TestReportImageIDForms(t *testing.T) {
	for _, id := range []string{"sha256:arm", "sha256:idx", "acr.example/opsloop/demo-api@sha256:idx"} {
		in := baseInput(t)
		in.Pods.Items[1].Status.ContainerStatuses[0].ImageID = id
		if s := Report(in); !s.ImageDigestMatch || !s.Pass {
			t.Errorf("imageID %s: match %v pass %v notes %v", id, s.ImageDigestMatch, s.Pass, s.Notes)
		}
	}
}

func TestReportCrossClockNote(t *testing.T) {
	in := baseInput(t)
	a := attempt(&in)
	a.NodeName, a.PodNodeName = "opsloop-azure-worker", "opsloop-azure-worker"
	s := Report(in)
	if !slices.Contains(s.Notes, "tPlace cross-clock (controller VM vs target kubelet)") {
		t.Errorf("notes %v lack cross-clock", s.Notes)
	}
}

func observeOnlyInput(t *testing.T) ReportInput {
	in := baseInput(t)
	in.Mode, in.Scenario, in.ExpectReject = modeObserveOnly, "primary-only", nil
	rt := &in.Incident.Status.Runtime
	rt.Phase, rt.Reason, rt.Attempts = "OBSERVED", "SELECTED_HIGHEST_SCORE", nil
	return in
}

func TestReportObserveOnly(t *testing.T) {
	if s := Report(observeOnlyInput(t)); !s.Pass || s.TDecideSeconds != nil {
		t.Fatalf("pass %v tDecide %v notes %v", s.Pass, s.TDecideSeconds, s.Notes)
	}
	for name, mutate := range map[string]func(in *ReportInput){
		"generation changed":  func(in *ReportInput) { in.Env.DeploymentAfter.Generation = 4 },
		"spec sha changed":    func(in *ReportInput) { in.Env.DeploymentAfter.SpecSHA256 = "bb" },
		"after state missing": func(in *ReportInput) { in.Env.DeploymentAfter = nil },
		"not observed":        func(in *ReportInput) { in.Incident.Status.Runtime.Phase = "WAITING" },
	} {
		in := observeOnlyInput(t)
		mutate(&in)
		if Report(in).Pass {
			t.Errorf("%s: pass = true", name)
		}
	}
}

func allRejectedInput(t *testing.T) ReportInput {
	in := baseInput(t)
	in.Scenario, in.ExpectNode = scenarioAllRejected, ""
	in.ExpectReject = map[string]string{"opsloop-control": "INSUFFICIENT_REQUESTED_CPU"}
	rt := &in.Incident.Status.Runtime
	rt.Phase, rt.Reason, rt.Attempts = "FAILED", "DEADLINE_EXCEEDED", nil
	d := rt.Decision
	d.DecisionReason, d.SelectedNodeName, d.SelectedNodeUID, d.ActionType = "NO_CANDIDATE", "", "", "None"
	d.Candidates[1].Eligible, d.Candidates[1].RejectReasons = false, []string{"INSUFFICIENT_REQUESTED_CPU"}
	return in
}

func TestReportAllRejected(t *testing.T) {
	if s := Report(allRejectedInput(t)); !s.Pass {
		t.Fatalf("pass = false, notes %v", s.Notes)
	}
	in := allRejectedInput(t)
	in.Incident.Status.Runtime.Attempts = []v1alpha1.Attempt{{Outcome: "Failed"}}
	if Report(in).Pass {
		t.Error("attempts 1: pass = true")
	}
	in = allRejectedInput(t)
	in.Incident.Status.Runtime.Decision.DecisionReason = "METRICS_UNAVAILABLE"
	if Report(in).Pass {
		t.Error("METRICS_UNAVAILABLE: pass = true")
	}
	in = allRejectedInput(t)
	in.ExpectReject["opsloop-azure-worker"] = "INSUFFICIENT_REQUESTED_CPU"
	if Report(in).Pass {
		t.Error("unmet expect-reject: pass = true")
	}
}

func TestReportNulls(t *testing.T) {
	in := baseInput(t)
	attempt(&in).PodReadyAt = nil
	in.Incident.Spec.DetectedAt = v1alpha1.RecoveryIncident{}.Spec.DetectedAt
	in.Probe = nil
	s := Report(in)
	if s.TPlaceSeconds != nil || s.TDetectSeconds != nil || s.TDecideSeconds != nil || s.TRecoverSeconds != nil || s.HostMaxGapSeconds != nil {
		t.Fatalf("want nulls, got place %v detect %v decide %v recover %v gap %v",
			s.TPlaceSeconds, s.TDetectSeconds, s.TDecideSeconds, s.TRecoverSeconds, s.HostMaxGapSeconds)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"tPlaceSeconds":null`, `"tRecoverSeconds":null`, `"hostMaxGapSeconds":null`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("summary JSON lacks %s", k)
		}
	}
}

func TestHostMaxGapUnterminated(t *testing.T) {
	in := baseInput(t)
	// Failures from +9s to the end (+13s injected below) have no later success.
	recs := append([]ProbeRecord{}, in.Probe[:11]...)
	for _, sec := range []int{11, 13} {
		recs = append(recs, ProbeRecord{T: in.InjectAt.Add(time.Duration(sec) * time.Second)})
	}
	eq(t, "hostMaxGap", hostMaxGap(recs), 4)
}

func writeFixtures(t *testing.T) (dir string, args []string) {
	dir = t.TempDir()
	files := map[string]string{"probe.jsonl": probeFixture, "incident.json": incidentFixture, "pods.json": podsFixture, "env.json": envFixture}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args = []string{"report", "-probe", filepath.Join(dir, "probe.jsonl"), "-incident", filepath.Join(dir, "incident.json"),
		"-pods", filepath.Join(dir, "pods.json"), "-env", filepath.Join(dir, "env.json"), "-inject-at", injectAtFixture,
		"-platform-digests", "sha256:amd,sha256:arm", "-expect-version", "abc123", "-out", filepath.Join(dir, "summary.json")}
	return dir, args
}

func TestRunReportExpectNodeRequired(t *testing.T) {
	_, args := writeFixtures(t)
	if err := run(append(args, "-mode", "Enforce", "-scenario", "azure-loaded"), io.Discard); err == nil {
		t.Fatal("Enforce without -expect-node: want error")
	}
	if err := run(append(args, "-mode", "Enforce", "-scenario", "azure-loaded", "-expect-reject", "bad"), io.Discard); err == nil {
		t.Fatal("malformed -expect-reject: want error")
	}
}

func TestRunReportWritesSummary(t *testing.T) {
	dir, args := writeFixtures(t)
	args = append(args, "-mode", "Enforce", "-scenario", "azure-loaded", "-expect-node", "opsloop-control",
		"-expect-reject", "opsloop-azure-worker=CPU_PRESSURE")
	var out strings.Builder
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "PASS ") {
		t.Errorf("stdout %q", out.String())
	}
	var s Summary
	if err := readJSON(filepath.Join(dir, "summary.json"), &s); err != nil {
		t.Fatal(err)
	}
	if !s.Pass || s.SchemaVersion != summarySchemaVersion || s.RunID != "20261013T000000Z" || s.ExpectedRejects["opsloop-azure-worker"] != "CPU_PRESSURE" {
		t.Errorf("summary %+v", s)
	}
}
