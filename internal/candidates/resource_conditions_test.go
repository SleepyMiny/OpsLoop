package candidates

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func resourceInput() Input {
	in := baseInput(candNode("first", "10.0.0.1"), candNode("second", "10.0.0.2"))
	cpu := int32(950)
	mem := int64(512 * mi)
	in.RunID, in.Epoch, in.PolicyUID = "run-1", "epoch-1", "policy-1"
	in.ResourceConditions = &ResourceConditions{Schema: "opsloop-resource-conditions/v1", RunID: in.RunID, Epoch: in.Epoch, PolicyUID: in.PolicyUID, DeploymentUID: string(in.Deployment.UID), NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(30 * time.Second), Nodes: []NodeResourceCondition{{NodeName: "first", NodeUID: "uid-first", CPUUtilizationFloorPermille: &cpu, MemoryAvailableCeilingBytes: &mem}}}
	return in
}
func TestResourceConditionsMeasuredAppliedAndNoLeak(t *testing.T) {
	in := resourceInput()
	before, _ := json.Marshal(in)
	d := Evaluate(in)
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("evaluation mutated observations/configuration")
	}
	if d.SelectedNodeName != "second" {
		t.Fatalf("selection %s", d.SelectedNodeName)
	}
	first := d.Candidates[0]
	if first.Eligible || !slices.Contains(first.RejectReasons, RejectCPUPressure) || !slices.Contains(first.RejectReasons, RejectMemoryPressure) {
		t.Fatalf("injection not applied: %+v", first)
	}
	if *first.Measurements.CPUUtilizationPermille != 200 || *first.Measurements.MemAvailableBytes != 6*gi {
		t.Fatal("real observation overwritten")
	}
	c := first.ResourceCondition
	if c == nil || *c.AppliedMeasurements.CPUUtilizationPermille != 950 || *c.AppliedMeasurements.MemAvailableBytes != 512*mi || c.RunID != in.RunID || c.Epoch != in.Epoch || c.PolicyUID != in.PolicyUID || len(c.ConfigSHA256) != 64 {
		t.Fatalf("missing bound applied evidence: %+v", c)
	}
	if !d.SnapshotExpiresAt.Time.Equal(in.ResourceConditions.ExpiresAt) {
		t.Fatal("decision exceeded injection lifetime")
	}
	*c.AppliedMeasurements.CPUUtilizationPermille = 1000
	if *first.Measurements.CPUUtilizationPermille != 200 {
		t.Fatal("applied evidence aliases original")
	}
	in.ResourceConditions = nil
	normal := Evaluate(in)
	if normal.SelectedNodeName != "first" || normal.Candidates[0].ResourceCondition != nil {
		t.Fatal("condition leaked into ordinary run")
	}
}
func TestResourceConditionsCannotMaskUnsafeMeasurements(t *testing.T) {
	in := resourceInput()
	cpu := int32(100)
	mem := int64(20 * gi)
	in.ResourceConditions.Nodes[0].CPUUtilizationFloorPermille = &cpu
	in.ResourceConditions.Nodes[0].MemoryAvailableCeilingBytes = &mem
	in.Metrics.CPUUtil["10.0.0.1"] = one(.99)
	in.Metrics.MemAvail["10.0.0.1"] = one(float64(mi))
	in.Nodes[0].Status.Conditions = append(in.Nodes[0].Status.Conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
	c := Evaluate(in).Candidates[0]
	if c.Eligible || !slices.Contains(c.RejectReasons, RejectCPUPressure) || !slices.Contains(c.RejectReasons, RejectDiskPressure) || !slices.Contains(c.RejectReasons, RejectInsufficientAvailableMemory) {
		t.Fatal("unsafe real conditions bypassed")
	}
	if *c.ResourceCondition.AppliedMeasurements.CPUUtilizationPermille != 990 || *c.ResourceCondition.AppliedMeasurements.MemAvailableBytes != mi {
		t.Fatal("conditions improved measurements")
	}
}
func TestResourceConditionsRejectEntireAmbiguousExperiment(t *testing.T) {
	cases := map[string]func(*Input){
		"run": func(in *Input) { in.RunID = "new-run" }, "epoch": func(in *Input) { in.Epoch = "new-epoch" }, "policy": func(in *Input) { in.PolicyUID = "new-policy" }, "deployment": func(in *Input) { in.Deployment.UID = "new-workload" },
		"node-uid": func(in *Input) { in.Nodes[0].UID = "reused-name" }, "node-absent": func(in *Input) { in.Nodes = in.Nodes[1:] }, "expired": func(in *Input) { in.Now = in.ResourceConditions.ExpiresAt }, "future": func(in *Input) { in.ResourceConditions.NotBefore = in.Now.Add(time.Second) },
		"missing": func(in *Input) { delete(in.Metrics.MemAvail, "10.0.0.1") }, "stale": func(in *Input) { in.Metrics.CPUTime["10.0.0.1"] = one(ts(now.Add(-time.Hour))) }, "duplicates": func(in *Input) {
			in.ResourceConditions.Nodes = append(in.ResourceConditions.Nodes, in.ResourceConditions.Nodes[0])
		},
		"negative": func(in *Input) { *in.ResourceConditions.Nodes[0].MemoryAvailableCeilingBytes = -1 }, "too-long": func(in *Input) {
			in.ResourceConditions.ExpiresAt = in.ResourceConditions.NotBefore.Add(601 * time.Second)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			in := resourceInput()
			change(&in)
			d := Evaluate(in)
			if ValidateResourceInput(in) == nil || d.SelectedNodeName != "" || d.ActionID != "" {
				t.Fatal("invalid experiment accepted")
			}
			for _, c := range d.Candidates {
				if c.Eligible || c.Score != nil || c.ResourceCondition != nil {
					t.Fatal("ambiguous condition became eligible")
				}
			}
		})
	}
}
func TestResourceConditionsParseStrictAndDetached(t *testing.T) {
	in := resourceInput()
	raw, _ := json.Marshal(in.ResourceConditions)
	got, err := ParseResourceConditions(raw)
	if err != nil || !reflect.DeepEqual(got, in.ResourceConditions) {
		t.Fatal(got, err)
	}
	for _, raw := range [][]byte{append(append([]byte(nil), raw...), []byte(" {}")...), []byte(`{"schema":"opsloop-resource-conditions/v1","extra":1}`), make([]byte, 16385), bytes.Replace(raw, []byte(`"runID":"run-1"`), []byte(`"runID":"run-1","RunID":"run-2"`), 1)} {
		if _, err := ParseResourceConditions(raw); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
