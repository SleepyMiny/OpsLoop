package classification

import (
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func readinessPod() corev1.Pod {
	p := pod("health-pod", "n1")
	p.Status.Phase = corev1.PodRunning
	p.Spec.Containers = []corev1.Container{{Name: "app", ReadinessProbe: &corev1.Probe{}}}
	p.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}
	p.Status.ContainerStatuses[0].ContainerID = "containerd://first"
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Second))},
		{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Second))},
	}
	return p
}

func TestReadinessRequiresOptInHealthyHistoryAndStableFailure(t *testing.T) {
	for _, tc := range []string{"disabled", "no-history", "below-threshold", "boundary", "unknown", "pending", "terminating", "no-probe", "node-unknown", "invalid-period", "condition-flap"} {
		t.Run(tc, func(t *testing.T) {
			p := readinessPod()
			in := input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour))
			period := metav1.Duration{Duration: 30 * time.Second}
			in.Detection.PodUnreadyFor = &period
			in.HealthyPods = map[types.UID]bool{p.UID: true}
			switch tc {
			case "disabled":
				in.Detection.PodUnreadyFor = nil
			case "no-history":
				in.HealthyPods = nil
			case "below-threshold":
				in.Pods[0].Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-29 * time.Second))
			case "unknown":
				in.Pods[0].Status.Conditions[0].Status = corev1.ConditionUnknown
			case "pending":
				in.Pods[0].Status.Phase = corev1.PodPending
			case "terminating":
				stamp := metav1.NewTime(now)
				in.Pods[0].DeletionTimestamp = &stamp
			case "no-probe":
				in.Pods[0].Spec.Containers[0].ReadinessProbe = nil
			case "node-unknown":
				in.Nodes["n1"].Status.Conditions[0].Status = corev1.ConditionUnknown
				in.Nodes["n1"].Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
			case "invalid-period":
				period.Duration = time.Second
			case "condition-flap":
				in.Pods[0].Status.Conditions[1].LastTransitionTime = metav1.NewTime(now.Add(-time.Second))
			}
			got := Classify(in)
			if tc == "boundary" {
				if !got.Detected || !IsPodReadinessTrigger(got.Trigger) || got.Trigger.PodUID != string(p.UID) || got.Trigger.NodeUID != "uid-n1" {
					t.Fatalf("want bound readiness trigger, got %+v", got)
				}
			} else if IsPodReadinessTrigger(got.Trigger) {
				t.Fatalf("unexpected readiness trigger for %s: %+v", tc, got)
			}
		})
	}
}

func TestReadinessCannotOverrideResourceOrInfrastructureFailure(t *testing.T) {
	p := readinessPod()
	period := metav1.Duration{Duration: 30 * time.Second}
	for _, kind := range []string{"node-down", "oom"} {
		t.Run(kind, func(t *testing.T) {
			in := input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour))
			in.Detection.PodUnreadyFor = &period
			in.HealthyPods = map[types.UID]bool{p.UID: true}
			if kind == "node-down" {
				in.Nodes["n1"].Status.Conditions[0].Status = corev1.ConditionFalse
			} else {
				in.Pods[0].Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(now)}
			}
			if got := Classify(in); !got.Detected || IsPodReadinessTrigger(got.Trigger) {
				t.Fatalf("higher-priority failure lost: %+v", got)
			}
		})
	}
}

func TestReadinessPeriodBounds(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 15 * time.Second, 5 * time.Minute, 5*time.Minute + time.Second} {
		period := metav1.Duration{Duration: d}
		got := PodReadinessEnabled(v1alpha1.DetectionSpec{PodUnreadyFor: &period})
		if got != (d >= 15*time.Second && d <= 5*time.Minute) {
			t.Fatalf("period %s enabled=%t", d, got)
		}
	}
}

func TestReadinessCannotOverrideRepeatedExitOnSameOrDifferentPod(t *testing.T) {
	for _, samePod := range []bool{true, false} {
		p := readinessPod()
		in := input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour))
		in.Detection.PodUnreadyFor = &metav1.Duration{Duration: 30 * time.Second}
		in.HealthyPods = map[types.UID]bool{p.UID: true}
		exiting := p
		if !samePod {
			exiting.Name = "z-exiting"
			exiting.UID = "other"
			in.Pods = append(in.Pods, exiting)
		}
		in.Restarts = restarts(exiting, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}, []int32{1, 1, 1})
		got := Classify(in)
		if got.Trigger.Reason != ReasonRepeatedExit || got.Trigger.Class != ClassApplication {
			t.Fatalf("samePod=%t: %+v", samePod, got)
		}
	}
}

func TestReadinessEvidenceFitsMaximumNamesAndBindsContainer(t *testing.T) {
	p := readinessPod()
	p.Name = strings.Repeat("a", 253)
	p.UID = "01234567-0123-0123-0123-012345678901"
	in := input([]corev1.Pod{p}, node("n1", corev1.ConditionTrue, time.Hour))
	in.Detection.PodUnreadyFor = &metav1.Duration{Duration: 30 * time.Second}
	in.HealthyPods = map[types.UID]bool{p.UID: true}
	got := Classify(in)
	if ReadinessTriggerContainerHash(got.Trigger) != ReadinessContainerHash(&p) {
		t.Fatalf("binding lost to evidence truncation: %+v", got)
	}
}
