package classification

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const ReasonPodNotReady = "POD_NOT_READY"
const ReadinessRuleVersion = "classify-readiness-v1"

func PodReadinessEnabled(d v1alpha1.DetectionSpec) bool {
	return d.PodUnreadyFor != nil && d.PodUnreadyFor.Duration >= 15*time.Second && d.PodUnreadyFor.Duration <= 5*time.Minute
}

func IsPodReadinessTrigger(t v1alpha1.Trigger) bool {
	return t.Class == ClassApplication && t.Reason == ReasonPodNotReady
}

// Container incarnation changes invalidate the prior healthy observation.
func ReadinessContainerHash(p *corev1.Pod) string {
	parts := []string{"readiness-container-v1"}
	for _, c := range p.Status.ContainerStatuses {
		if c.ContainerID == "" {
			return ""
		}
		parts = append(parts, fmt.Sprintf("%s/%s/%d", c.Name, c.ContainerID, c.RestartCount))
	}
	if len(parts) == 1 {
		return ""
	}
	sort.Strings(parts[1:])
	return Signature(parts...)
}

func ReadinessTriggerContainerHash(t v1alpha1.Trigger) string {
	prefix := fmt.Sprintf("%s podUID=%s ", ReasonPodNotReady, t.PodUID)
	for _, e := range t.Evidence {
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		for _, field := range strings.Fields(e) {
			if strings.HasPrefix(field, "containerIdentitySHA256=") {
				value := strings.TrimPrefix(field, "containerIdentitySHA256=")
				if len(value) == 64 {
					return value
				}
			}
		}
	}
	return ""
}

// PodHealthy requires a running, scheduled Pod with positive Pod and container
// readiness. Readiness gates and missing/partial status cannot arm this rule.
func PodHealthy(p *corev1.Pod) bool {
	if !runningPod(p) {
		return false
	}
	ready, containers := podCondition(p, corev1.PodReady), podCondition(p, corev1.ContainersReady)
	if ready == nil || containers == nil || ready.Status != corev1.ConditionTrue || containers.Status != corev1.ConditionTrue {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// PodUnreadySince recognizes actual failed container readiness, not Pending,
// image pulls, termination, missing status, custom readiness gates or Node loss.
func PodUnreadySince(p *corev1.Pod) (time.Time, bool) {
	if !runningPod(p) {
		return time.Time{}, false
	}
	ready, containers := podCondition(p, corev1.PodReady), podCondition(p, corev1.ContainersReady)
	if ready == nil || containers == nil || ready.Status != corev1.ConditionFalse || containers.Status != corev1.ConditionFalse || ready.LastTransitionTime.IsZero() || containers.LastTransitionTime.IsZero() {
		return time.Time{}, false
	}
	failedProbe := false
	for _, spec := range p.Spec.Containers {
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == spec.Name && !c.Ready && spec.ReadinessProbe != nil {
				failedProbe = true
			}
		}
	}
	// Both conditions must have remained false for the configured duration.
	since := ready.LastTransitionTime.Time
	if containers.LastTransitionTime.After(since) {
		since = containers.LastTransitionTime.Time
	}
	return since, failedProbe
}

func runningPod(p *corev1.Pod) bool {
	if p == nil || p.UID == "" || p.Spec.NodeName == "" || p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || len(p.Spec.ReadinessGates) != 0 || len(p.Spec.Containers) == 0 || len(p.Status.ContainerStatuses) != len(p.Spec.Containers) {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.State.Running == nil || c.State.Terminated != nil || c.State.Waiting != nil {
			return false
		}
	}
	return true
}

func podCondition(p *corev1.Pod, kind corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == kind {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}
