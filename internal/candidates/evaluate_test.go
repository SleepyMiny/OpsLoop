package candidates

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const (
	gi = int64(1) << 30
	mi = int64(1) << 20
)

func spec() v1alpha1.RecoveryPolicySpec {
	return v1alpha1.RecoveryPolicySpec{
		RequiredPlatforms: []string{"linux/amd64", "linux/arm64"},
		Filter: v1alpha1.FilterSpec{
			MaxCPUUtilizationPercent: 85, MaxMemoryUtilizationPercent: 85,
			MinimumAvailableMemory: resource.MustParse("512Mi"), StartupMemoryBudget: resource.MustParse("256Mi"),
			RequestedCapacityReservePercent: 10, MaxSampleAge: metav1.Duration{Duration: maxAge},
		},
		Score: v1alpha1.ScoreSpec{
			CPUWeight: 30, MemoryWeight: 35, RequestsWeight: 35,
			RecentFailurePenalty: 15, MaxRecentFailurePenalty: 45, RecentFailureWindow: metav1.Duration{Duration: 30 * time.Minute},
		},
		Budget: v1alpha1.BudgetSpec{Cooldown: metav1.Duration{Duration: 120 * time.Second}},
	}
}

func resources(cpu, mem string) corev1.ResourceRequirements {
	rl := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl}
}

func deployment() *appsv1.Deployment {
	one := int32(1)
	surge, unavail := intstr.FromInt32(1), intstr.FromInt32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", UID: "wl-uid"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavail},
			},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{hostnameLabel: "old"},
				Containers:   []corev1.Container{{Name: "app", Resources: resources("100m", "128Mi")}},
			}},
		},
	}
}

func candNode(name, ip string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{
			hostnameLabel: name, osLabel: "linux", archLabel: "amd64",
		}},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
	}
}

func one(v float64) []Sample { return []Sample{{Value: v, Time: now}} }

func ts(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

// setMetrics writes a healthy snapshot for ip: cpu util .2, MemAvailable 6Gi of 10Gi, samples 10s old.
func setMetrics(m *Metrics, ip string) {
	for _, p := range []*map[string][]Sample{&m.CPUUtil, &m.CPUTime, &m.CPUCount, &m.MemAvail, &m.MemAvailTime, &m.MemTotal, &m.Up} {
		if *p == nil {
			*p = map[string][]Sample{}
		}
	}
	m.ObservedAt = now
	m.CPUUtil[ip] = one(0.2)
	m.CPUTime[ip] = one(ts(now.Add(-10 * time.Second)))
	m.CPUCount[ip] = one(6)
	m.MemAvail[ip] = one(float64(6 * gi))
	m.MemAvailTime[ip] = one(ts(now.Add(-10 * time.Second)))
	m.MemTotal[ip] = one(float64(10 * gi))
	m.Up[ip] = one(1)
}

func reqPod(cpu, mem string) corev1.Pod {
	return corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: resources(cpu, mem)}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func baseInput(nodes ...corev1.Node) Input {
	in := Input{
		Now: now, Spec: spec(), PolicyHash: "ph", PolicyGeneration: 3,
		IncidentName: "inc", ActionID: "inc-a1", Deployment: deployment(),
		Nodes: nodes, PodsByNode: map[string][]corev1.Pod{},
		ImageDigest: "sha256:abc", BaselineTemplateHash: "th",
	}
	for _, n := range nodes {
		setMetrics(&in.Metrics, n.Status.Addresses[0].Address)
	}
	return in
}

func single(t *testing.T, in Input) v1alpha1.CandidateEvaluation {
	t.Helper()
	d := Evaluate(in)
	if len(d.Candidates) != 1 {
		t.Fatalf("candidates: %+v", d.Candidates)
	}
	return d.Candidates[0]
}

func TestEachRejectReason(t *testing.T) {
	const ip = "10.0.0.1"
	cases := []struct {
		reason string
		mutate func(*Input, *corev1.Node)
	}{
		{RejectNodeNotReady, func(_ *Input, n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{RejectUnschedulable, func(_ *Input, n *corev1.Node) { n.Spec.Unschedulable = true }},
		{RejectUntoleratedTaint, func(_ *Input, n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "x", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{RejectMemoryPressure, func(_ *Input, n *corev1.Node) {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
		}},
		{RejectMemoryPressure, func(in *Input, _ *corev1.Node) {
			in.Metrics.MemTotal[ip] = one(float64(16 * gi))
			in.Metrics.MemAvail[ip] = one(float64(gi)) // 93.75% used
		}},
		{RejectDiskPressure, func(_ *Input, n *corev1.Node) {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
		}},
		{RejectPIDPressure, func(_ *Input, n *corev1.Node) {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue})
		}},
		{RejectCPUPressure, func(in *Input, _ *corev1.Node) { in.Metrics.CPUUtil[ip] = one(0.9) }},
		{RejectArchMismatch, func(_ *Input, n *corev1.Node) { n.Labels[archLabel] = "s390x" }},
		{RejectInsufficientRequestedCPU, func(in *Input, n *corev1.Node) {
			in.PodsByNode[n.Name] = []corev1.Pod{reqPod("1800m", "1Mi")}
		}},
		{RejectInsufficientRequestedMemory, func(in *Input, n *corev1.Node) {
			in.PodsByNode[n.Name] = []corev1.Pod{reqPod("1m", "3800Mi")}
		}},
		{RejectInsufficientAvailableMemory, func(in *Input, _ *corev1.Node) {
			in.Metrics.MemTotal[ip] = one(float64(gi))
			in.Metrics.MemAvail[ip] = one(float64(700 * mi))
		}},
		{RejectMetricsMissing, func(in *Input, _ *corev1.Node) { delete(in.Metrics.MemTotal, ip) }},
		{RejectMetricsStale, func(in *Input, _ *corev1.Node) { in.Metrics.Up[ip] = one(0) }},
		{RejectCooldown, func(in *Input, n *corev1.Node) {
			in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "x-a1", NodeUID: string(n.UID), At: metav1.NewTime(now.Add(-time.Minute))}}
		}},
		{RejectUnsupportedConstraint, func(_ *Input, n *corev1.Node) { delete(n.Labels, hostnameLabel) }},
		{RejectUnsupportedConstraint, func(_ *Input, n *corev1.Node) { n.Labels[hostnameLabel] = "" }},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%02d-%s", i, c.reason), func(t *testing.T) {
			n := candNode("n1", ip)
			in := baseInput(n)
			c.mutate(&in, &n)
			in.Nodes = []corev1.Node{n}
			got := single(t, in)
			if !reflect.DeepEqual(got.RejectReasons, []string{c.reason}) || got.Eligible || got.Score != nil {
				t.Fatalf("got %v eligible=%v details=%v", got.RejectReasons, got.Eligible, got.Details)
			}
		})
	}
}

func TestHealthyCandidateSelected(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	d := Evaluate(in)
	c := d.Candidates[0]
	if !c.Eligible || d.SelectedNodeName != "n1" || d.SelectedHostname != "n1" || d.SelectedNodeUID != "uid-n1" ||
		d.ActionType != ActionRelocation || d.DecisionReason != DecisionSelected || d.ActionID != "inc-a1" {
		t.Fatalf("decision: %+v", d)
	}
	if d.IncidentID != "inc" || d.PolicyHash != "ph" || d.PolicyGeneration != 3 || d.WorkloadUID != "wl-uid" ||
		d.BaselineTemplateHash != "th" || d.ImageIndexDigest != "sha256:abc" || d.QueryVersion != QueryVersion ||
		!d.DecidedAt.Time.Equal(now) || !d.SnapshotObservedAt.Time.Equal(now) || c.Platform != "linux/amd64" {
		t.Fatalf("decision fields: %+v", d)
	}
}

func TestMultipleViolationsAllRecorded(t *testing.T) {
	n := candNode("n1", "10.0.0.1")
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	n.Spec.Unschedulable = true
	n.Labels[archLabel] = "s390x"
	got := single(t, baseInput(n))
	want := []string{RejectNodeNotReady, RejectUnschedulable, RejectArchMismatch}
	if !reflect.DeepEqual(got.RejectReasons, want) {
		t.Fatalf("got %v", got.RejectReasons)
	}
}

func TestCPUBoundary(t *testing.T) {
	for _, c := range []struct {
		util   float64
		reject bool
	}{{0.850, false}, {0.851, true}} {
		in := baseInput(candNode("n1", "10.0.0.1"))
		in.Metrics.CPUUtil["10.0.0.1"] = one(c.util)
		if got := single(t, in); got.Eligible == c.reject {
			t.Errorf("util %v: %v", c.util, got.RejectReasons)
		}
	}
}

func TestRequestsBoundaryAndReserve(t *testing.T) {
	// allocatable 2000m, reserve 10% = 200m, new Pod 100m: existing 1700m fits exactly.
	for _, c := range []struct {
		existing string
		reject   bool
	}{{"1700m", false}, {"1701m", true}} {
		n := candNode("n1", "10.0.0.1")
		in := baseInput(n)
		in.PodsByNode["n1"] = []corev1.Pod{reqPod(c.existing, "1Mi")}
		got := single(t, in)
		if got.Measurements.ReserveCPUMilli != 200 || got.Measurements.NewPodCPUMilli != 100 {
			t.Fatalf("measurements: %+v", got.Measurements)
		}
		if got.Eligible == c.reject {
			t.Errorf("existing %s: %v", c.existing, got.RejectReasons)
		}
	}
}

func TestRequestedIncludesTerminatingExcludesFinished(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	term := reqPod("300m", "1Mi")
	del := metav1.NewTime(now)
	term.DeletionTimestamp = &del
	done := reqPod("500m", "1Mi")
	done.Status.Phase = corev1.PodSucceeded
	failed := reqPod("500m", "1Mi")
	failed.Status.Phase = corev1.PodFailed
	in.PodsByNode["n1"] = []corev1.Pod{term, done, failed}
	if got := single(t, in); got.Measurements.RequestedCPUMilli != 300 {
		t.Fatalf("requested: %d", got.Measurements.RequestedCPUMilli)
	}
}

func TestAvailableMemoryBoundary(t *testing.T) {
	for _, c := range []struct {
		avail  int64
		reject bool
	}{{768 * mi, false}, {768*mi - 1, true}} {
		in := baseInput(candNode("n1", "10.0.0.1"))
		in.Metrics.MemTotal["10.0.0.1"] = one(float64(2 * gi))
		in.Metrics.MemAvail["10.0.0.1"] = one(float64(c.avail))
		got := single(t, in)
		if got.Eligible == c.reject {
			t.Errorf("avail %d: %v", c.avail, got.RejectReasons)
		}
	}
}

func TestScoreHandComputed(t *testing.T) {
	// cpu util .2 -> 800, mem 6Gi/10Gi -> 600, cpu requests (2000-200-700-100)/2000 -> 500 (memory side 868).
	in := baseInput(candNode("n1", "10.0.0.1"))
	in.PodsByNode["n1"] = []corev1.Pod{reqPod("700m", "0")}
	s := single(t, in).Score
	want := v1alpha1.ScoreBreakdown{CPUHeadroomPermille: 800, MemoryHeadroomPermille: 600, RequestedHeadroomPermille: 500, ScoreMilli: 62500}
	if *s != want {
		t.Fatalf("score %+v", *s)
	}
	in.RecentFailures = []v1alpha1.NodeFailure{
		{ActionID: "a", NodeUID: "uid-n1", At: metav1.NewTime(now.Add(-10 * time.Minute))},
		{ActionID: "b", NodeUID: "uid-n1", At: metav1.NewTime(now.Add(-20 * time.Minute))},
	}
	if s := single(t, in).Score; s.ScoreMilli != 32500 || s.PenaltyMilli != 30000 || s.RecentFailures != 2 {
		t.Fatalf("score with failures %+v", *s)
	}
}

func TestTieBrokenByName(t *testing.T) {
	d := Evaluate(baseInput(candNode("b-node", "10.0.0.2"), candNode("a-node", "10.0.0.1")))
	if d.SelectedNodeName != "a-node" || d.Candidates[0].NodeName != "a-node" {
		t.Fatalf("selected %s, order %s", d.SelectedNodeName, d.Candidates[0].NodeName)
	}
}

func TestPenalty(t *testing.T) {
	fail := func(id string, ago time.Duration) v1alpha1.NodeFailure {
		return v1alpha1.NodeFailure{ActionID: id, NodeUID: "uid-n1", At: metav1.NewTime(now.Add(-ago))}
	}
	cases := []struct {
		name     string
		failures []v1alpha1.NodeFailure
		want     int64
	}{
		{"one", []v1alpha1.NodeFailure{fail("a", 10*time.Minute)}, 15000},
		{"four capped", []v1alpha1.NodeFailure{fail("a", 5*time.Minute), fail("b", 6*time.Minute), fail("c", 7*time.Minute), fail("d", 8*time.Minute)}, 45000},
		{"outside window", []v1alpha1.NodeFailure{fail("a", 31*time.Minute)}, 0},
	}
	for _, c := range cases {
		in := baseInput(candNode("n1", "10.0.0.1"))
		in.RecentFailures = c.failures
		if got := single(t, in).Score.PenaltyMilli; got != c.want {
			t.Errorf("%s: penalty %d want %d", c.name, got, c.want)
		}
	}
}

func TestCooldownBoundary(t *testing.T) {
	for _, c := range []struct {
		ago    time.Duration
		reject bool
	}{{119 * time.Second, true}, {121 * time.Second, false}} {
		in := baseInput(candNode("n1", "10.0.0.1"))
		in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "a", NodeUID: "uid-n1", At: metav1.NewTime(now.Add(-c.ago))}}
		if got := single(t, in); got.Eligible == c.reject {
			t.Errorf("%s: %v", c.ago, got.RejectReasons)
		}
	}
}

func TestCurrentIncidentFailedAttemptCooldown(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	done := metav1.NewTime(now.Add(-30 * time.Second))
	in.Attempts = []v1alpha1.Attempt{{ActionID: "inc-a1", Outcome: "Failed", NodeUID: "uid-n1", CompletedAt: &done}}
	if got := single(t, in); !reflect.DeepEqual(got.RejectReasons, []string{RejectCooldown}) {
		t.Fatalf("got %v", got.RejectReasons)
	}
}

func TestSameActionPenalizedOnce(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	at := metav1.NewTime(now.Add(-10 * time.Minute))
	in.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "inc-a1", NodeUID: "uid-n1", At: at}}
	in.Attempts = []v1alpha1.Attempt{{ActionID: "inc-a1", Outcome: "Failed", NodeUID: "uid-n1", CompletedAt: &at}}
	if s := single(t, in).Score; s.RecentFailures != 1 || s.PenaltyMilli != 15000 {
		t.Fatalf("score %+v", *s)
	}
}

func TestCancelledAttemptNoCooldown(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	done := metav1.NewTime(now.Add(-time.Second))
	in.Attempts = []v1alpha1.Attempt{{ActionID: "inc-a1", Outcome: "Cancelled", NodeUID: "uid-n1", CompletedAt: &done}}
	if got := single(t, in); !got.Eligible {
		t.Fatalf("got %v", got.RejectReasons)
	}
}

func TestCandidatesCapped(t *testing.T) {
	var nodes []corev1.Node
	for i := 16; i >= 0; i-- {
		nodes = append(nodes, candNode(fmt.Sprintf("n%02d", i), fmt.Sprintf("10.0.0.%d", i+1)))
	}
	d := Evaluate(baseInput(nodes...))
	if len(d.Candidates) != v1alpha1.MaxCandidates || d.Candidates[0].NodeName != "n00" || d.Candidates[15].NodeName != "n15" {
		t.Fatalf("got %d candidates", len(d.Candidates))
	}
}

func TestMetricsErrorRejectsAll(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"), candNode("n2", "10.0.0.2"))
	in.Metrics = Metrics{}
	in.MetricsErr = errors.New("connection refused")
	d := Evaluate(in)
	for _, c := range d.Candidates {
		if !reflect.DeepEqual(c.RejectReasons, []string{RejectMetricsMissing}) || c.Measurements.CPUUtilizationPermille != nil {
			t.Fatalf("%s: %v", c.NodeName, c.RejectReasons)
		}
	}
	if d.DecisionReason != DecisionMetricsUnavailable || d.ActionType != ActionNone || d.ActionID != "" ||
		!d.SnapshotObservedAt.Time.Equal(now) || !d.SnapshotExpiresAt.Time.Equal(now.Add(maxAge)) {
		t.Fatalf("decision %+v", d)
	}
}

func TestAuthoritySeparation(t *testing.T) {
	n := candNode("n1", "10.0.0.1")
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	if got := single(t, baseInput(n)); !reflect.DeepEqual(got.RejectReasons, []string{RejectNodeNotReady}) {
		t.Fatalf("API NotReady: %v", got.RejectReasons)
	}
	in := baseInput(candNode("n1", "10.0.0.1"))
	in.Metrics.CPUTime["10.0.0.1"] = one(ts(now.Add(-5 * time.Minute)))
	in.Metrics.CPUUtil["10.0.0.1"] = one(0.99) // stale values are not compared
	got := single(t, in)
	if !reflect.DeepEqual(got.RejectReasons, []string{RejectMetricsStale}) || got.Measurements.CPUUtilizationPermille == nil {
		t.Fatalf("stale: %v", got.RejectReasons)
	}
}

func TestNoCandidateDecision(t *testing.T) {
	d := Evaluate(baseInput())
	if d.Candidates == nil || len(d.Candidates) != 0 || d.DecisionReason != DecisionNoCandidate || d.ActionType != ActionNone {
		t.Fatalf("decision %+v", d)
	}
}

func TestSnapshotExpiresAtUsesOldestSelectedSample(t *testing.T) {
	in := baseInput(candNode("n1", "10.0.0.1"))
	in.Metrics.CPUTime["10.0.0.1"] = one(ts(now.Add(-40 * time.Second)))
	in.Metrics.MemAvailTime["10.0.0.1"] = one(ts(now.Add(-20 * time.Second)))
	d := Evaluate(in)
	if want := now.Add(-40 * time.Second).Add(maxAge); !d.SnapshotExpiresAt.Time.Equal(want) {
		t.Fatalf("expires %v want %v", d.SnapshotExpiresAt, want)
	}
}

func TestUntoleratedTaint(t *testing.T) {
	cases := []struct {
		name   string
		taint  corev1.Taint
		tol    []corev1.Toleration
		reject bool
	}{
		{"exists with empty key tolerates all", corev1.Taint{Key: "a", Value: "b", Effect: corev1.TaintEffectNoExecute},
			[]corev1.Toleration{{Operator: corev1.TolerationOpExists}}, false},
		{"empty effect tolerates every effect", corev1.Taint{Key: "a", Value: "b", Effect: corev1.TaintEffectNoSchedule},
			[]corev1.Toleration{{Key: "a", Operator: corev1.TolerationOpEqual, Value: "b"}}, false},
		{"equal value mismatch", corev1.Taint{Key: "a", Value: "b", Effect: corev1.TaintEffectNoSchedule},
			[]corev1.Toleration{{Key: "a", Operator: corev1.TolerationOpEqual, Value: "c"}}, true},
		{"PreferNoSchedule ignored", corev1.Taint{Key: "a", Effect: corev1.TaintEffectPreferNoSchedule}, nil, false},
	}
	for _, c := range cases {
		n := candNode("n1", "10.0.0.1")
		n.Spec.Taints = []corev1.Taint{c.taint}
		in := baseInput(n)
		in.Deployment.Spec.Template.Spec.Tolerations = c.tol
		if got := single(t, in); got.Eligible == c.reject {
			t.Errorf("%s: %v", c.name, got.RejectReasons)
		}
	}
}

func TestCheckWorkload(t *testing.T) {
	if issues := CheckWorkload(deployment()); len(issues) != 0 {
		t.Fatalf("supported deployment: %v", issues)
	}
	cases := map[string]func(*appsv1.Deployment){
		"replicas": func(d *appsv1.Deployment) { two := int32(2); d.Spec.Replicas = &two },
		"strategy": func(d *appsv1.Deployment) { s := intstr.FromString("25%"); d.Spec.Strategy.RollingUpdate.MaxSurge = &s },
		"recreate": func(d *appsv1.Deployment) {
			d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		},
		"requests missing": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Resources.Requests = nil },
		"limits missing": func(d *appsv1.Deployment) {
			delete(d.Spec.Template.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory)
		},
		"pvc": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}}}
		},
		"nodeName": func(d *appsv1.Deployment) { d.Spec.Template.Spec.NodeName = "n1" },
		"affinity": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Affinity = &corev1.Affinity{} },
		"topologySpread": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{MaxSkew: 1}}
		},
		"nodeSelector key": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"] = "arm64"
		},
		"hostPort": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		},
	}
	for name, mutate := range cases {
		d := deployment()
		mutate(d)
		if issues := CheckWorkload(d); len(issues) != 1 {
			t.Errorf("%s: issues %v", name, issues)
		}
	}
}

func TestPodRequestsInitContainer(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Resources: resources("500m", "64Mi")}},
		Containers:     []corev1.Container{{Resources: resources("100m", "128Mi")}, {Resources: resources("200m", "128Mi")}},
	}
	cpu, mem := PodRequests(spec)
	if cpu != 500 || mem != 256*mi {
		t.Fatalf("got %d %d", cpu, mem)
	}
}
