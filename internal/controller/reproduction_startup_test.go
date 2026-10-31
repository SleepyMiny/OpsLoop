package controller

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func startupHeartbeat(t *testing.T, f *reproductionFixture, elapsed time.Duration) {
	t.Helper()
	f.now = f.now.Add(elapsed)
	var g ExecutionGrant
	if err := json.Unmarshal([]byte(f.p.Annotations[ExecutionGrantAnnotation]), &g); err != nil {
		t.Fatal(err)
	}
	g.HeartbeatSequence++
	g.HeartbeatAt = f.now
	b, _ := json.Marshal(g)
	f.p.Annotations[ExecutionGrantAnnotation] = string(b)
}

func TestReproductionStartupMissingIdentityThenBound(t *testing.T) {
	for _, kind := range []string{"no-status", "no-container-id", "no-image-id", "unscheduled", "waiting"} {
		t.Run(kind, func(t *testing.T) {
			f := newPythonReproduction(t)
			original := f.pod.DeepCopy()
			switch kind {
			case "no-status":
				f.pod.Status = corev1.PodStatus{}
			case "no-container-id":
				f.pod.Status.ContainerStatuses[0].ContainerID = ""
			case "no-image-id":
				f.pod.Status.ContainerStatuses[0].ImageID = ""
			case "unscheduled":
				f.pod.Spec.NodeName = ""
			case "waiting":
				f.pod.Status.Phase = corev1.PodPending
				f.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			}
			if err := f.observe(); !errors.Is(err, errReproductionPending) {
				t.Fatalf("not pending: %v", err)
			}
			deadline := f.a.current.sourceWaitDeadline
			if f.a.current.occurrence != nil || f.a.current.authority.closed != "" {
				t.Fatal("pending armed/retired")
			}
			startupHeartbeat(t, f, 3*time.Second)
			if err := f.observe(); !errors.Is(err, errReproductionPending) {
				t.Fatal(err)
			}
			if f.a.current.sourceWaitDeadline != deadline {
				t.Fatal("deadline reset")
			}
			f.pod = original
			if err := f.observe(); err != nil {
				t.Fatal(err)
			}
			if f.a.current.occurrence == nil {
				t.Fatal("identity not bound")
			}
		})
	}
}

func TestReproductionStartupRejectsViolations(t *testing.T) {
	for _, kind := range []string{"owner", "template", "image", "container", "status-name", "status-image", "terminated", "node-unready", "uid", "deployment", "revoke", "stop", "generation", "instance", "old-run", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			f := newPythonReproduction(t)
			original := f.pod.DeepCopy()
			f.pod.Status = corev1.PodStatus{}
			if err := f.observe(); !errors.Is(err, errReproductionPending) {
				t.Fatal(err)
			}
			f.pod = original
			switch kind {
			case "owner":
				f.pod.OwnerReferences[0].UID = "other"
			case "template":
				f.rs.Spec.Template.Spec.Containers[0].Env[0].Value = "other"
			case "image":
				f.pod.Spec.Containers[0].Image = "other"
			case "container":
				f.pod.Spec.Containers[0].Name = "other"
			case "status-name":
				f.pod.Status.ContainerStatuses[0].Name = "other"
			case "status-image":
				f.pod.Status.ContainerStatuses[0].ImageID = "other"
			case "terminated":
				f.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			case "node-unready":
				f.node.Status.Conditions[0].Status = corev1.ConditionFalse
			case "uid":
				f.p.UID = "other"
			case "deployment":
				f.d.UID = "other"
			case "revoke":
				delete(f.p.Annotations, ExhibitionPermitAnnotation)
			case "stop":
				f.p.Spec.Enabled = false
				f.a.ObserveStopped(f.p)
			case "generation":
				f.p.Generation++
			case "instance":
				f.a.instance = "restarted-instance"
			case "expiry":
				startupHeartbeat(t, f, 601*time.Second)
			case "old-run":
				var g ExecutionGrant
				json.Unmarshal([]byte(f.p.Annotations[ExecutionGrantAnnotation]), &g)
				g.Scope.RunID = "old-run"
				b, _ := json.Marshal(g)
				f.p.Annotations[ExecutionGrantAnnotation] = string(b)
			}
			if err := f.observe(); err == nil || errors.Is(err, errReproductionPending) {
				t.Fatalf("violation not rejected: %v", err)
			}
		})
	}
}

func TestReproductionStartupDeadlineDoesNotReset(t *testing.T) {
	f := newPythonReproduction(t)
	original := f.pod.DeepCopy()
	f.pod.Status = corev1.PodStatus{}
	if err := f.observe(); !errors.Is(err, errReproductionPending) {
		t.Fatal(err)
	}
	deadline := f.a.current.sourceWaitDeadline
	for range 7 {
		startupHeartbeat(t, f, 8*time.Second)
		if err := f.observe(); !errors.Is(err, errReproductionPending) {
			t.Fatal(err)
		}
	}
	if f.a.current.sourceWaitDeadline != deadline {
		t.Fatal("deadline extended")
	}
	startupHeartbeat(t, f, 4*time.Second)
	f.pod = original
	if err := f.observe(); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("late identity bound: %v", err)
	}
	if f.a.current.occurrence != nil {
		t.Fatal("late binding")
	}
	if f.observe() == nil {
		t.Fatal("expired wait revived")
	}
}

func TestReproductionStartupNotReadyIdentityCannotInventHealthyProof(t *testing.T) {
	f := newPythonReproduction(t)
	f.pod.Status.ContainerStatuses[0].Ready = false
	for i := range f.pod.Status.Conditions {
		f.pod.Status.Conditions[i].Status = corev1.ConditionFalse
	}
	if err := f.observe(); err != nil {
		t.Fatal(err)
	}
	if f.a.current.occurrence == nil || !f.a.current.occurrence.HealthyObservedAt.IsZero() {
		t.Fatal("identity/health conflated")
	}
	// Even a caller-provided trigger cannot replace genuine observed healthy history.
	trigger := f.a.current.occurrence
	if f.a.bindReproductionTrigger(f.p, f.triggerWithoutHistory(), strings.Split(f.permit.CircuitEvidence.WorkloadImage, "@")[1]) == nil {
		t.Fatalf("missing healthy proof accepted %+v", trigger)
	}
}

func (f *reproductionFixture) triggerWithoutHistory() v1alpha1.Trigger {
	result := classification.Classify(classification.Input{Now: f.now.Add(16 * time.Second), Detection: v1alpha1.DetectionSpec{PodUnreadyFor: &metav1.Duration{Duration: 15 * time.Second}}, WorkloadUID: f.d.UID, ImageDigest: strings.Split(f.permit.CircuitEvidence.WorkloadImage, "@")[1], Pods: []corev1.Pod{*f.pod}, Nodes: map[string]*corev1.Node{f.node.Name: f.node}, HealthyPods: map[types.UID]bool{f.pod.UID: true}})
	return result.Trigger
}
