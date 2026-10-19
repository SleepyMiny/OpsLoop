package controller

import (
	"context"
	"strings"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nodeSafetyState string

const (
	nodePass           nodeSafetyState = "Pass"
	nodeUnsafe         nodeSafetyState = "Unsafe"
	nodeTriggerCleared nodeSafetyState = "TriggerCleared"
	nodeUnknown        nodeSafetyState = "Unknown"
)

type nodeSafety struct {
	State  nodeSafetyState
	Reason string
}

// Structural guard deliberately excludes capacity, metrics and cooldown: the
// action's own new Pod already consumes capacity after placement. Node RV is not
// compared: unrelated status updates are not an unsafe structural change.
func (x deploymentExecution) nodeGuard(ctx context.Context, inc *v1alpha1.RecoveryIncident, d *appsv1.Deployment, a *v1alpha1.Attempt) nodeSafety {
	unknown := func(err error) nodeSafety { return nodeSafety{nodeUnknown, "NODE_OBSERVATION_UNKNOWN: " + err.Error()} }
	trigger := &corev1.Node{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKey{Name: inc.Spec.Trigger.NodeName}, trigger); err != nil {
		if apierrors.IsNotFound(err) {
			return nodeSafety{nodeTriggerCleared, "TRIGGER_MISSING"}
		}
		return unknown(err)
	}
	if string(trigger.UID) != inc.Spec.Trigger.NodeUID || !classification.TriggerPersists(inc.Spec.Trigger, map[string]*corev1.Node{trigger.Name: trigger}) {
		return nodeSafety{nodeTriggerCleared, ReasonTriggerCleared}
	}
	node := &corev1.Node{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKey{Name: a.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nodeSafety{nodeUnsafe, "CANDIDATE_MISSING"}
		}
		return unknown(err)
	}
	if string(node.UID) != a.NodeUID || node.Labels[hostnameLabel] != a.Hostname || node.DeletionTimestamp != nil {
		return nodeSafety{nodeUnsafe, "CANDIDATE_IDENTITY_CHANGED"}
	}
	sel, err := metav1.LabelSelectorAsSelector(&inc.Spec.Policy.CandidateSelector)
	if err != nil {
		return unknown(err)
	}
	if !sel.Matches(labels.Set(node.Labels)) {
		return nodeSafety{nodeUnsafe, "CANDIDATE_SELECTOR_CHANGED"}
	}
	reasons := candidates.StructuralRejectReasons(node, inc.Spec.Policy, d.Spec.Template.Spec.Tolerations)
	if len(reasons) > 0 {
		return nodeSafety{nodeUnsafe, strings.Join(reasons, ",")}
	}
	return nodeSafety{State: nodePass}
}
