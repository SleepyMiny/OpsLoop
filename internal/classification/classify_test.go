package classification

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func detection() v1alpha1.DetectionSpec {
	return v1alpha1.DetectionSpec{
		NodeUnreadyFor:   metav1.Duration{Duration: 20 * time.Second},
		RestartThreshold: 3,
		RestartWindow:    metav1.Duration{Duration: 120 * time.Second},
	}
}

func node(name string, ready corev1.ConditionStatus, since time.Duration) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: ready, LastTransitionTime: metav1.NewTime(now.Add(-since)),
		}}},
	}
}

func pod(name, nodeName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}}},
	}
}

func input(pods []corev1.Pod, nodes ...*corev1.Node) Input {
	m := map[string]*corev1.Node{}
	for _, n := range nodes {
		m[n.Name] = n
	}
	return Input{Now: now, Detection: detection(), WorkloadUID: "wl-uid", ImageDigest: "sha256:aaa", Pods: pods, Nodes: m}
}

func restarts(p corev1.Pod, ago []time.Duration, exits []int32) []RestartObservation {
	var out []RestartObservation
	for i, a := range ago {
		out = append(out, RestartObservation{PodUID: p.UID, Container: "app", At: now.Add(-a), ExitCode: exits[i], Reason: "Error"})
	}
	return out
}

func TestNodeNotReadyThreshold(t *testing.T) {
	r := Classify(input([]corev1.Pod{pod("p", "n1")}, node("n1", corev1.ConditionFalse, 19*time.Second)))
	if r.Detected || r.RecheckAfter != time.Second {
		t.Fatalf("19s: got %+v", r)
	}
	r = Classify(input([]corev1.Pod{pod("p", "n1")}, node("n1", corev1.ConditionFalse, 20*time.Second)))
	if !r.Detected || r.Trigger.Class != ClassInfrastructure || r.Trigger.Reason != ReasonNodeNotReady {
		t.Fatalf("20s: got %+v", r)
	}
	if r.Trigger.NodeUID != "uid-n1" || r.Trigger.RuleVersion != RuleVersion || r.Trigger.ObservedAt.IsZero() {
		t.Fatalf("trigger fields: %+v", r.Trigger)
	}
}

func TestNodeUnknownIsInfrastructure(t *testing.T) {
	r := Classify(input([]corev1.Pod{pod("p", "n1")}, node("n1", corev1.ConditionUnknown, time.Minute)))
	if r.Trigger.Class != ClassInfrastructure {
		t.Fatalf("got %+v", r)
	}
}

func TestMissingNodeIsUnknown(t *testing.T) {
	r := Classify(input([]corev1.Pod{pod("p", "gone")}))
	if r.Trigger.Class != ClassUnknown || r.Trigger.Reason != ReasonInsufficientEvidence {
		t.Fatalf("got %+v", r)
	}
}

func TestNoReadyConditionIsUnknown(t *testing.T) {
	n := node("n1", corev1.ConditionTrue, 0)
	n.Status.Conditions = nil
	r := Classify(input([]corev1.Pod{pod("p", "n1")}, n))
	if r.Trigger.Class != ClassUnknown {
		t.Fatalf("got %+v", r)
	}
}

func TestInfrastructureBeatsApplication(t *testing.T) {
	a := pod("a", "bad")
	b := pod("b", "good")
	in := input([]corev1.Pod{b, a}, node("bad", corev1.ConditionFalse, time.Minute), node("good", corev1.ConditionTrue, time.Hour))
	in.Restarts = restarts(b, []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second}, []int32{1, 1, 1})
	r := Classify(in)
	if r.Trigger.Class != ClassInfrastructure || len(r.Trigger.Evidence) != 2 {
		t.Fatalf("got %+v", r.Trigger)
	}
}

func TestOOMKilledIsResource(t *testing.T) {
	p := pod("p", "n1")
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-time.Minute)),
	}
	r := Classify(input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour)))
	if r.Trigger.Class != ClassResource || r.Trigger.Reason != ReasonOOMKilled {
		t.Fatalf("got %+v", r)
	}
}

func TestImagePullIsConfiguration(t *testing.T) {
	p := pod("p", "n1")
	p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}
	r := Classify(input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour)))
	if r.Trigger.Class != ClassConfiguration || r.Trigger.Reason != ReasonImagePull {
		t.Fatalf("got %+v", r)
	}
}

func TestRepeatedExit(t *testing.T) {
	p := pod("p", "n1")
	n := node("n1", corev1.ConditionTrue, time.Hour)
	cases := []struct {
		name   string
		ago    []time.Duration
		exits  []int32
		detect bool
	}{
		{"three same exits", []time.Duration{10 * time.Second, 50 * time.Second, 110 * time.Second}, []int32{1, 1, 1}, true},
		{"two exits", []time.Duration{10 * time.Second, 50 * time.Second}, []int32{1, 1}, false},
		{"one outside window", []time.Duration{10 * time.Second, 50 * time.Second, 121 * time.Second}, []int32{1, 1, 1}, false},
		{"mixed exit codes", []time.Duration{10 * time.Second, 50 * time.Second, 90 * time.Second}, []int32{1, 2, 1}, false},
		{"exit zero", []time.Duration{10 * time.Second, 50 * time.Second, 90 * time.Second}, []int32{0, 0, 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := input([]corev1.Pod{p}, n)
			in.Restarts = restarts(p, c.ago, c.exits)
			r := Classify(in)
			if r.Detected != c.detect {
				t.Fatalf("detected=%v want %v: %+v", r.Detected, c.detect, r)
			}
			if c.detect && (r.Trigger.Class != ClassApplication || r.Trigger.Reason != ReasonRepeatedExit) {
				t.Fatalf("got %+v", r.Trigger)
			}
		})
	}
}

func TestExitCodeAloneNotDetected(t *testing.T) {
	p := pod("p", "n1")
	p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now)}
	in := input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour))
	in.Restarts = restarts(p, []time.Duration{time.Second}, []int32{1})
	if r := Classify(in); r.Detected {
		t.Fatalf("got %+v", r)
	}
}

func TestSignatureDeterministic(t *testing.T) {
	in := input([]corev1.Pod{pod("p", "n1")}, node("n1", corev1.ConditionFalse, time.Minute))
	a, b := Classify(in), Classify(in)
	if a.Trigger.Signature == "" || a.Trigger.Signature != b.Trigger.Signature {
		t.Fatalf("signatures differ: %q %q", a.Trigger.Signature, b.Trigger.Signature)
	}
	in.ImageDigest = "sha256:bbb"
	if c := Classify(in); c.Trigger.Signature == a.Trigger.Signature {
		t.Fatal("imageDigest change kept signature")
	}
}

func TestUnknownSignatureIncludesDigest(t *testing.T) {
	in := input([]corev1.Pod{pod("p", "gone")})
	a := Classify(in)
	in.ImageDigest = "sha256:bbb"
	if b := Classify(in); a.Trigger.Signature == b.Trigger.Signature {
		t.Fatal("UNKNOWN signature ignores imageDigest")
	}
}

func TestUnscheduledPendingPodNotDetected(t *testing.T) {
	p := pod("p", "")
	p.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if r := Classify(input([]corev1.Pod{p}, node("bad", corev1.ConditionFalse, time.Minute))); r.Detected {
		t.Fatalf("got %+v", r)
	}
}

func TestEvidenceCapped(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 11; i++ {
		pods = append(pods, pod(fmt.Sprintf("p%02d", i), fmt.Sprintf("gone-%02d", i)))
	}
	r := Classify(input(pods))
	ev := r.Trigger.Evidence
	if len(ev) != v1alpha1.MaxEvidenceItems || ev[len(ev)-1] != v1alpha1.TruncatedListMarker {
		t.Fatalf("evidence: %v", ev)
	}
}

func TestMonitoredPods(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", UID: "d-uid", Annotations: map[string]string{revisionAnnotation: "2"}}}
	ctrl := true
	rs := func(uid, rev string) appsv1.ReplicaSet {
		return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(uid), Annotations: map[string]string{revisionAnnotation: rev},
			OwnerReferences: []metav1.OwnerReference{{UID: d.UID, Controller: &ctrl}},
		}}
	}
	owned := func(name, rsUID string) corev1.Pod {
		p := pod(name, "n1")
		p.OwnerReferences = []metav1.OwnerReference{{UID: types.UID(rsUID), Controller: &ctrl}}
		p.Status.Phase = corev1.PodRunning
		return p
	}
	keep := owned("keep", "rs-new")
	deleting := owned("deleting", "rs-new")
	ts := metav1.NewTime(now)
	deleting.DeletionTimestamp = &ts
	done := owned("done", "rs-new")
	done.Status.Phase = corev1.PodSucceeded
	old := owned("old", "rs-old")

	got := MonitoredPods(d, []appsv1.ReplicaSet{rs("rs-new", "2"), rs("rs-old", "1")}, []corev1.Pod{keep, deleting, done, old})
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("got %v", got)
	}
}

func TestTriggerPersists(t *testing.T) {
	tr := v1alpha1.Trigger{Class: ClassInfrastructure, NodeName: "n1", NodeUID: "uid-n1"}
	other := node("n1", corev1.ConditionTrue, 0)
	other.UID = "uid-new"
	cases := []struct {
		name  string
		nodes map[string]*corev1.Node
		want  bool
	}{
		{"not ready", map[string]*corev1.Node{"n1": node("n1", corev1.ConditionFalse, time.Minute)}, true},
		{"deleted", map[string]*corev1.Node{}, true},
		{"recreated", map[string]*corev1.Node{"n1": other}, true},
		{"ready", map[string]*corev1.Node{"n1": node("n1", corev1.ConditionTrue, 0)}, false},
	}
	for _, c := range cases {
		if got := TriggerPersists(tr, c.nodes); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
