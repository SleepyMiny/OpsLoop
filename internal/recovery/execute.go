// Package recovery holds the pure decisions behind one runtime recovery action:
// the template mutation, restart-safe resume, and final outcome of a reserved attempt.
package recovery

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const (
	AttemptAnnotation = "opsloop.io/attempt-id"
	HostnameKey       = corev1.LabelHostname
)

// Attempt outcomes, matching the CRD enum on Attempt.Outcome.
const (
	OutcomeReserved   = "Reserved"
	OutcomePatched    = "Patched"
	OutcomeSucceeded  = "Succeeded"
	OutcomeFailed     = "Failed"
	OutcomeSuperseded = "Superseded"
	OutcomeCancelled  = "Cancelled"
)

type ResumeAction int

const (
	Apply ResumeAction = iota
	AlreadyApplied
	Superseded
)

// Resume decides how to continue a Reserved attempt against the live Deployment.
// The second value is the SUPERSEDED reason and is empty unless the action is Superseded.
func Resume(live *appsv1.Deployment, workloadUID types.UID, baselineSpecHash string, a v1alpha1.Attempt) (ResumeAction, string) {
	if live.UID != workloadUID {
		return Superseded, "WORKLOAD_REPLACED"
	}
	if v1alpha1.SpecHash(live.Spec) != baselineSpecHash {
		return Superseded, "SPEC_CHANGED_EXTERNALLY"
	}
	// The whole template is compared, so an external image change on top of our annotation is not mistaken for ours.
	switch v1alpha1.TemplateHash(live.Spec.Template) {
	case a.TargetTemplateHash:
		return AlreadyApplied, ""
	case a.PreTemplateHash:
		return Apply, ""
	default:
		return Superseded, "TEMPLATE_CHANGED_EXTERNALLY"
	}
}

// Mutate pins the template to hostname and tags it with actionID. It is deterministic,
// so the target template hash can be computed at reservation time.
func Mutate(t *corev1.PodTemplateSpec, hostname, actionID string) {
	if t.Spec.NodeSelector == nil {
		t.Spec.NodeSelector = map[string]string{}
	}
	t.Spec.NodeSelector[HostnameKey] = hostname
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations[AttemptAnnotation] = actionID
}

// ReservedOutcome settles a Reserved attempt when the incident goes terminal. live is nil when the Deployment is gone.
// Only the template decides whether our action was applied; spec drift is recorded by the incident reason instead.
func ReservedOutcome(live *appsv1.Deployment, workloadUID types.UID, a v1alpha1.Attempt) string {
	if live == nil || live.UID != workloadUID {
		return OutcomeSuperseded
	}
	switch v1alpha1.TemplateHash(live.Spec.Template) {
	case a.TargetTemplateHash:
		return OutcomePatched
	case a.PreTemplateHash:
		return OutcomeCancelled
	default:
		return OutcomeSuperseded
	}
}
