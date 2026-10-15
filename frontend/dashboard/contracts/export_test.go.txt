package candidates

import (
	"encoding/json"
	"fmt"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"testing"
	"time"
)

func TestDashboardContractExport(t *testing.T) {
	type entry struct {
		Name     string                    `json:"name"`
		Input    Input                     `json:"input"`
		Decision v1alpha1.RecoveryDecision `json:"decision"`
	}
	out := []entry{}
	for _, name := range []string{"ordinary", "rounding", "threshold", "stale", "missing", "tie", "reserve-edge", "cooldown", "future", "penalty", "cpu-half", "cpu-clamp", "threshold-single", "reserve-single", "memory-half", "memory-boundary", "expiry-boundary", "missing-memory", "missing-hostname", "negative-score", "sample-count", "exporter-down", "memory-too-large", "memory-negative-fraction", "timestamp-zero", "future-boundary"} {
		in := baseInput(candNode("a", "10.0.0.1"), candNode("b", "10.0.0.2"))
		in.Spec.TargetRef.Name = "demo-api"
		in.Spec.Enabled = true
		in.Spec.Mode = v1alpha1.PolicyModeEnforce
		in.Spec.Detection = v1alpha1.DetectionSpec{NodeUnreadyFor: metav1.Duration{Duration: 20 * time.Second}, RestartWindow: metav1.Duration{Duration: 120 * time.Second}, RestartThreshold: 3}
		in.Spec.Verification = v1alpha1.VerificationSpec{ServiceName: "demo-api", Port: 8080, ConsecutiveSuccesses: 5, StabilityWindow: metav1.Duration{Duration: 30 * time.Second}}
		in.Spec.Budget.MaxActionsPerIncident = 2
		in.Spec.Budget.PlacementTimeout = metav1.Duration{Duration: 90 * time.Second}
		in.Spec.Budget.VerificationTimeout = metav1.Duration{Duration: 60 * time.Second}
		in.Spec.Budget.IncidentDeadline = metav1.Duration{Duration: 10 * time.Minute}
		in.Spec.CandidateSelector = metav1.LabelSelector{MatchLabels: map[string]string{"opsloop.io/recovery-candidate": "true"}}
		for i := range in.Nodes {
			in.Nodes[i].Labels["opsloop.io/recovery-candidate"] = "true"
		}
		switch name {
		case "memory-too-large":
			in.Metrics.MemTotal["10.0.0.1"] = one(1 << 51)
			in.Metrics.MemAvail["10.0.0.1"] = one(1 << 50)
		case "memory-negative-fraction":
			in.Spec.Filter.MaxMemoryUtilizationPercent = 100
			in.Spec.Filter.MinimumAvailableMemory = resource.MustParse("0")
			in.Spec.Filter.StartupMemoryBudget = resource.MustParse("0")
			in.Metrics.MemAvail["10.0.0.1"] = one(-.1)
		case "timestamp-zero":
			in.Metrics.CPUTime["10.0.0.1"] = one(0)
		case "future-boundary":
			in.Metrics.CPUTime["10.0.0.1"] = one(ts(in.Now.Add(5 * time.Second)))

		case "sample-count":
			in.Metrics.CPUCount["10.0.0.1"] = one(2)
		case "exporter-down":
			in.Metrics.Up["10.0.0.1"] = one(0)
		case "negative-score":
			in.Spec.Score.RecentFailurePenalty = 100
			in.Spec.Score.MaxRecentFailurePenalty = 100
			in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "old", NodeUID: "uid-a", NodeName: "a", At: metav1.NewTime(in.Now.Add(-180 * time.Second))}}
		case "missing-hostname":
			delete(in.Nodes[0].Labels, hostnameLabel)
		case "missing-memory":
			delete(in.Metrics.MemAvail, "10.0.0.1")
		case "expiry-boundary":
			in.Metrics.CPUTime["10.0.0.1"] = one(ts(in.Now.Add(-in.Spec.Filter.MaxSampleAge.Duration)))
		case "memory-half":
			in.Metrics.MemAvail["10.0.0.1"] = one(3845 * 1024 * 1024)
			in.Metrics.MemTotal["10.0.0.1"] = one(10000 * 1024 * 1024)
		case "memory-boundary":
			in.Metrics.MemAvail["10.0.0.1"] = one(15 * 1024 * 1024 * 1024)
			in.Metrics.MemTotal["10.0.0.1"] = one(100 * 1024 * 1024 * 1024)
		case "cpu-half":
			in.Metrics.CPUUtil["10.0.0.1"] = one(.2005)
		case "cpu-clamp":
			in.Metrics.CPUUtil["10.0.0.1"] = one(-.01)
		case "threshold-single":
			in.Nodes = in.Nodes[:1]
			in.Metrics.CPUUtil["10.0.0.1"] = one(.85049)
		case "reserve-single":
			in.Nodes = in.Nodes[:1]
			in.Nodes[0].Status.Allocatable["cpu"] = resource.MustParse("111m")
		case "rounding":
			in.Metrics.CPUUtil["10.0.0.1"] = one(.20049)
			in.Metrics.MemAvail["10.0.0.1"] = one(float64(6*gi + 123))
		case "threshold":
			in.Metrics.CPUUtil["10.0.0.1"] = one(.85049)
		case "stale":
			in.Metrics.CPUTime["10.0.0.1"] = one(ts(in.Now.Add(-in.Spec.Filter.MaxSampleAge.Duration - time.Second)))
		case "missing":
			delete(in.Metrics.CPUUtil, "10.0.0.1")
		case "reserve-edge":
			in.Nodes[0].Status.Allocatable["cpu"] = resource.MustParse("111m")
			in.Deployment.Spec.Template.Spec.Containers[0].Resources.Requests["cpu"] = resource.MustParse("100m")
		case "cooldown":
			in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "old", NodeUID: "uid-a", NodeName: "a", At: metav1.NewTime(in.Now.Add(-time.Second))}}
		case "penalty":
			in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "old", NodeUID: "uid-a", NodeName: "a", At: metav1.NewTime(in.Now.Add(-180 * time.Second))}}
		case "future":
			in.Metrics.CPUTime["10.0.0.1"] = one(ts(in.Now.Add(6 * time.Second)))
		}
		out = append(out, entry{name, in, Evaluate(in)})
	}
	b, e := json.MarshalIndent(out, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(os.Getenv("DASHBOARD_CONTRACT_OUT"), b, 0600); e != nil {
		t.Fatal(e)
	}
}

// String outputs preserve exact int64 values past JavaScript's safe integer range.
func TestDashboardQuantityExport(t *testing.T) {
	type entry struct {
		Input   string `json:"input"`
		Bytes   string `json:"bytes,omitempty"`
		Invalid bool   `json:"invalid,omitempty"`
	}
	out := []entry{}
	for _, value := range []string{"1e6", "1E+6", "1m", "100m", "1Pi", "512Mi", "1.5Gi", "0.1", "1.1", ".5", "1.", "+1", "0", "-0", "1n", "1u", "1e-100", "0e100000", "1.25Ki", "9007199254740991", "9007199254740990.1", "9007199254740991.1", "8Pi", "1Ei", "1MiB", "1K", " 1", "1 ", "", ".", "1e", "1e1.5"} {
		q, err := resource.ParseQuantity(value)
		item := entry{Input: value, Invalid: err != nil}
		if err == nil {
			item.Bytes = fmt.Sprint(q.Value())
		}
		out = append(out, item)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(os.Getenv("DASHBOARD_QUANTITY_OUT"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
