package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

// deploymentExecution owns every Deployment mutation and its final live checks.
// It never writes Incident/Policy status, and never returns a detached permit.
type deploymentExecution struct{ r *PolicyReconciler }
type executionEffect string

const (
	effectUnapplied executionEffect = "Unapplied"
	effectApplied   executionEffect = "Applied"
	effectUncertain executionEffect = "Uncertain"
)

type executionResult struct {
	Live               *appsv1.Deployment
	Policy             *v1alpha1.RecoveryPolicy
	Effect             executionEffect
	Guard              nodeSafety
	HaltReason         string
	Unauthorized       string
	Deferred           bool
	PreResourceVersion string
}

func (x deploymentExecution) AdvanceAttempt(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, dec v1alpha1.RecoveryDecision, inputs candidates.Input, recordDispatch func() error) (executionResult, error) {
	out := executionResult{Effect: effectUnapplied}
	current, reason, err := x.executionPolicy(ctx, p, inc)
	out.Policy, out.Unauthorized = current, reason
	if err != nil || reason != "" {
		return out, err
	}
	d, err := x.r.getDeployment(ctx, types.NamespacedName{Namespace: inc.Namespace, Name: inc.Spec.Workload.Name})
	if err != nil {
		return out, err
	}
	if d == nil || string(d.UID) != inc.Spec.Workload.UID {
		return out, fmt.Errorf("execution target identity changed")
	}
	out.Live = d
	a := lastReserved(inc)
	if a == nil {
		return out, fmt.Errorf("execution requires a reserved attempt")
	}
	action, _ := recovery.Resume(d, types.UID(inc.Spec.Workload.UID), inc.Spec.Baseline.SpecHash, *a)
	if action != recovery.Apply {
		return out, fmt.Errorf("execution template changed; resume required")
	}
	claimed, err := x.claimExecutionFence(ctx, current, d)
	if err != nil {
		return out, err
	}
	if claimed {
		current, reason, err = x.executionPolicy(ctx, current, inc)
		out.Policy, out.Unauthorized = current, reason
		if err != nil || reason != "" {
			return out, err
		}
	}
	// The Reconciler owns the write-ahead status CAS. No workload can be
	// dispatched before its acknowledgement. Final live checks remain here.
	if err := recordDispatch(); err != nil {
		return out, err
	}
	if inputs.ResourceConditions != nil {
		reserved := lastReserved(inc)
		if reserved == nil || inc.Status.Runtime.Decision == nil || !resourceEvidenceBound(reserved.Decision, inputs.ResourceConditions) || !resourceEvidenceBound(*inc.Status.Runtime.Decision, inputs.ResourceConditions) {
			out.HaltReason = "RESOURCE_CONDITIONS_UNCONFIRMED: persisted evidence missing or changed"
			return out, nil
		}
	}
	current, reason, err = x.executionPolicy(ctx, current, inc)
	out.Policy, out.Unauthorized = current, reason
	if err != nil || reason != "" {
		return out, err
	}
	eligible, cleared, why, err := x.executionNodes(ctx, inc, a, inputs)
	if err != nil {
		out.Guard = nodeSafety{nodeUnknown, err.Error()}
		out.HaltReason = "NODE_OBSERVATION_UNKNOWN: " + err.Error()
		return out, nil
	}
	if cleared {
		out.Guard = nodeSafety{nodeTriggerCleared, ReasonTriggerCleared}
		out.HaltReason = ReasonTriggerCleared
		return out, nil
	}
	if !eligible {
		out.Guard = nodeSafety{nodeUnsafe, why}
		out.HaltReason = "RESERVED_NODE_INVALID: " + why
		return out, nil
	}
	// Node/API I/O may consume the remaining authority window. This fresh live
	// Policy read checks the process grant again immediately before dispatch.
	current, reason, err = x.executionPolicy(ctx, current, inc)
	out.Policy, out.Unauthorized = current, reason
	if err != nil || reason != "" {
		return out, err
	}
	now := x.r.Now()
	if !now.Before(inc.Spec.Deadline.Time) {
		out.HaltReason = ReasonDeadlineExceeded
		return out, nil
	}
	if inputs.ResourceConditions != nil {
		inputs.Now = now
		if err := x.r.bindResourceInput(current, inc, &inputs); err != nil {
			out.HaltReason = "RESOURCE_CONDITIONS_UNCONFIRMED: " + err.Error()
			return out, nil
		}
	}
	if !attemptMetricsFresh(dec, a, inc.Spec.Policy.Filter.MaxSampleAge.Duration, now) {
		out.Deferred = true
		return out, nil
	}
	// v3 binds a live source incarnation, not only the failure class. Recheck it
	// after candidate/Policy I/O, then re-read the Policy and authority after that source I/O.
	// Kubernetes cannot atomically CAS this Pod and Deployment together.
	if authority, ok := x.r.Authority.(*ExhibitionAuthority); ok && authority.reproductionEnabled(current) {
		valid, sourceErr := x.r.readinessTrigger(ctx, inc, false)
		if sourceErr != nil || !valid {
			out.HaltReason = "REPRODUCTION_SOURCE_UNCONFIRMED"
			return out, nil
		}
		current, reason, err = x.executionPolicy(ctx, current, inc)
		out.Policy, out.Unauthorized = current, reason
		if err != nil || reason != "" {
			return out, err
		}
		if !x.r.Now().Before(inc.Spec.Deadline.Time) {
			out.HaltReason = ReasonDeadlineExceeded
			return out, nil
		}
	}
	out.PreResourceVersion = d.ResourceVersion
	recovery.Mutate(&d.Spec.Template, a.Hostname, a.ActionID)
	if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
		if apierrors.IsConflict(err) {
			return out, err
		}
		// A non-409 response cannot prove that the workload was not stored, even
		// when it looks like a timeout. Fence before settling from a fresh GET.
		out.Effect = effectUncertain
		out.HaltReason = "WORKLOAD_WRITE_UNCERTAIN"
		return out, nil
	}
	out.Effect = effectApplied
	out.Guard = x.nodeGuard(ctx, inc, d, a)
	if out.Guard.State != nodePass {
		out.HaltReason = out.Guard.Reason
	}
	return out, nil
}

func (x deploymentExecution) ObserveApplied(ctx context.Context, inc *v1alpha1.RecoveryIncident, d *appsv1.Deployment, a *v1alpha1.Attempt) executionResult {
	out := executionResult{Live: d, Effect: effectApplied}
	unknown := func(reason string) executionResult {
		out.Guard = nodeSafety{nodeUnknown, reason}
		out.HaltReason = reason
		return out
	}
	// Probe/Endpoint I/O can outlive the caller's Deployment snapshot. Refresh
	// authority here, before the final Node sample and any success decision.
	current, err := x.r.getDeployment(ctx, types.NamespacedName{Namespace: inc.Namespace, Name: inc.Spec.Workload.Name})
	if err != nil {
		return unknown("WORKLOAD_OBSERVATION_UNKNOWN: " + err.Error())
	}
	if current == nil || string(current.UID) != inc.Spec.Workload.UID {
		return unknown("WORKLOAD_IDENTITY_CHANGED")
	}
	out.Live = current
	if v1alpha1.SpecHash(current.Spec) != inc.Spec.Baseline.SpecHash {
		return unknown("WORKLOAD_SPEC_CHANGED")
	}
	wantHash := expectedTemplateHash(inc)
	if a.Outcome == recovery.OutcomeReserved {
		wantHash = a.TargetTemplateHash
	}
	if v1alpha1.TemplateHash(current.Spec.Template) != wantHash {
		return unknown("WORKLOAD_TEMPLATE_CHANGED")
	}
	f, err := readFence(current)
	if err != nil {
		return unknown("EXECUTION_FENCE_UNKNOWN: " + err.Error())
	}
	if f == nil || f.PolicyUID != inc.Spec.PolicyRef.UID || f.PolicyName != inc.Spec.PolicyRef.Name || f.DeploymentUID != inc.Spec.Workload.UID {
		return unknown("EXECUTION_FENCE_OWNER_UNCONFIRMED")
	}
	if f.SafetyHold != nil {
		return unknown(f.SafetyHold.Reason)
	}
	if f.ClosedThroughGeneration >= inc.Spec.PolicyGeneration {
		return unknown("EXECUTION_GENERATION_CLOSED")
	}
	out.Guard = x.nodeGuard(ctx, inc, current, a)
	if out.Guard.State != nodePass {
		out.HaltReason = out.Guard.Reason
	}
	return out
}

// Hold confirms the barrier by a fresh read after CAS. Lost responses/conflicts
// remain pending. Identity mismatch never authorizes a replacement owner/target.
func (x deploymentExecution) Hold(ctx context.Context, inc *v1alpha1.RecoveryIncident, reason string) (*appsv1.Deployment, error) {
	at := inc.Status.Runtime.Attempts
	if len(at) == 0 || inc.UID == "" {
		return nil, fmt.Errorf("safety hold requires an identified attempt")
	}
	a := at[len(at)-1]
	key := types.NamespacedName{Namespace: inc.Namespace, Name: inc.Spec.Workload.Name}
	d, err := x.r.getDeployment(ctx, key)
	if err != nil {
		return nil, err
	}
	if d == nil || string(d.UID) != inc.Spec.Workload.UID {
		return nil, fmt.Errorf("safety hold target absent or replaced; settlement unconfirmed")
	}
	f, err := readFence(d)
	if err != nil {
		return nil, err
	}
	if f == nil {
		f = &executionFence{Version: 2, PolicyUID: inc.Spec.PolicyRef.UID, PolicyName: inc.Spec.PolicyRef.Name, DeploymentUID: inc.Spec.Workload.UID}
	}
	if f.PolicyUID != inc.Spec.PolicyRef.UID || f.PolicyName != inc.Spec.PolicyRef.Name || f.DeploymentUID != inc.Spec.Workload.UID {
		return nil, fmt.Errorf("safety hold owner mismatch")
	}
	sum := sha256.Sum256([]byte(f.PolicyUID + "/" + f.DeploymentUID + "/" + string(inc.UID) + "/" + a.ActionID))
	holdID := fmt.Sprintf("%x", sum[:])
	if f.SafetyHold != nil {
		if f.SafetyHold.HoldID != holdID || f.SafetyHold.IncidentUID != string(inc.UID) || f.SafetyHold.ActionID != a.ActionID || f.SafetyHold.PolicyGeneration != inc.Spec.PolicyGeneration {
			return nil, fmt.Errorf("safety hold belongs to a different attempt; manual inspection required")
		}
		return d, nil
	}
	f.Version = 2
	f.ClosedThroughGeneration = max(f.ClosedThroughGeneration, inc.Spec.PolicyGeneration)
	f.SafetyHold = &safetyHold{HoldID: holdID, IncidentUID: string(inc.UID), ActionID: a.ActionID, PolicyGeneration: inc.Spec.PolicyGeneration, Reason: v1alpha1.Truncate(reason, 256)}
	putFence(d, f)
	if _, err := readFence(d); err != nil {
		return nil, err
	}
	if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
		return nil, err
	}
	live, err := x.r.getDeployment(ctx, key)
	if err != nil {
		return nil, err
	}
	if live == nil || live.UID != d.UID {
		return nil, fmt.Errorf("safety hold target changed after CAS")
	}
	confirmed, err := readFence(live)
	if err != nil {
		return nil, err
	}
	if confirmed == nil || confirmed.PolicyUID != f.PolicyUID || confirmed.PolicyName != f.PolicyName || confirmed.DeploymentUID != f.DeploymentUID || confirmed.SafetyHold == nil || *confirmed.SafetyHold != *f.SafetyHold || confirmed.ClosedThroughGeneration < f.ClosedThroughGeneration {
		return nil, fmt.Errorf("safety hold CAS unconfirmed")
	}
	return live, nil
}

// executionNodes rechecks the trigger and reserved Node through the APIReader
// after metrics collection, reservation and fence I/O. It reuses this reconcile's
// raw metrics with the normal candidate filters, including latest Pod requests.
// Node GETs and the Deployment CAS are separate operations: changes after these
// reads can still race with the action; this is not a cross-object atomic fence.
func (x deploymentExecution) executionNodes(ctx context.Context, inc *v1alpha1.RecoveryIncident, a *v1alpha1.Attempt, inputs candidates.Input) (bool, bool, string, error) {
	trigger := &corev1.Node{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKey{Name: inc.Spec.Trigger.NodeName}, trigger); err != nil {
		if apierrors.IsNotFound(err) {
			return false, true, "", nil
		}
		return false, false, "", err
	}
	if string(trigger.UID) != inc.Spec.Trigger.NodeUID || !classification.TriggerPersists(inc.Spec.Trigger, map[string]*corev1.Node{trigger.Name: trigger}) {
		return false, true, "", nil
	}
	if persists, err := x.r.readinessTrigger(ctx, inc, false); err != nil || !persists {
		return false, !persists && err == nil, "", err
	}
	var pods corev1.PodList
	if err := x.r.APIReader.List(ctx, &pods, client.MatchingFields{podNodeNameField: a.NodeName}); err != nil {
		return false, false, "", err
	}
	node := &corev1.Node{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKey{Name: a.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, "node deleted", nil
		}
		return false, false, "", err
	}
	if string(node.UID) != a.NodeUID || node.Labels[hostnameLabel] != a.Hostname || node.DeletionTimestamp != nil {
		return false, false, "node identity or hostname changed", nil
	}
	sel, err := metav1.LabelSelectorAsSelector(&inc.Spec.Policy.CandidateSelector)
	if err != nil {
		return false, false, "", err
	}
	if !sel.Matches(labels.Set(node.Labels)) {
		return false, false, "candidate selector no longer matches", nil
	}
	// Keep the collection evaluation time here; the exclusive metric expiry
	// and deadline are checked after these reads without cancelling the action.
	// Preserve the complete bound candidate set for condition validation while
	// replacing the reserved target with its fresh API identity and requests.
	inputs.Nodes = append([]corev1.Node(nil), inputs.Nodes...)
	replaced := false
	for i := range inputs.Nodes {
		if inputs.Nodes[i].Name == node.Name {
			inputs.Nodes[i] = *node
			replaced = true
		}
	}
	if !replaced {
		return false, false, "reserved node absent from evaluation", nil
	}
	freshPods := map[string][]corev1.Pod{}
	for name, observed := range inputs.PodsByNode {
		freshPods[name] = observed
	}
	freshPods[node.Name] = pods.Items
	inputs.PodsByNode = freshPods
	for _, current := range candidates.Evaluate(inputs).Candidates {
		if current.NodeUID == a.NodeUID && current.NodeName == a.NodeName {
			return current.Eligible, false, strings.Join(current.RejectReasons, ","), nil
		}
	}
	return false, false, "reserved node absent from decision", nil
}

// executionPolicy authorizes only the same, live Policy generation and active
// Incident. The Deployment fence RV, rather than this cross-object GET alone,
// excludes delayed old actions after an acknowledged stop.
func (x deploymentExecution) executionPolicy(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) (*v1alpha1.RecoveryPolicy, string, error) {
	current := &v1alpha1.RecoveryPolicy{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ReasonPolicyDeleted, nil
		}
		return nil, "", err
	}
	switch {
	case current.UID != p.UID || string(current.UID) != inc.Spec.PolicyRef.UID || current.DeletionTimestamp != nil:
		return current, ReasonPolicyDeleted, nil
	case !current.Spec.Enabled:
		return current, ReasonPolicyDisabled, nil
	case current.Spec.Mode != v1alpha1.PolicyModeEnforce || inc.Spec.Policy.Mode != v1alpha1.PolicyModeEnforce:
		return current, ReasonModeChanged, nil
	case current.Generation != inc.Spec.PolicyGeneration || v1alpha1.PolicyHash(current.Spec) != inc.Spec.PolicyHash ||
		v1alpha1.PolicyHash(inc.Spec.Policy) != inc.Spec.PolicyHash || !executionReservationMatches(current, inc):
		return current, ReasonPolicyChanged, nil
	}
	if err := x.r.authorizeExecution(current, inc); err != nil {
		return current, ReasonExecutionAuthority, nil
	}
	if authority, ok := x.r.Authority.(*ExhibitionAuthority); ok {
		live, err := x.r.liveDeployment(ctx, current)
		if err != nil {
			return current, "", err
		}
		if err = authority.ValidateReplayWorkload(current, live); err != nil {
			return current, ReasonExecutionAuthority, nil
		}
	}
	return current, "", nil
}

// CloseAttempt fences a cancelled old generation without permanently holding a
// normal policy edit/stop. Its immutable Incident owner cannot be transferred.
// It confirms the live same-Deployment barrier before returning settlement data.
func (x deploymentExecution) CloseAttempt(ctx context.Context, inc *v1alpha1.RecoveryIncident) (*appsv1.Deployment, error) {
	key := types.NamespacedName{Namespace: inc.Namespace, Name: inc.Spec.Workload.Name}
	d, err := x.r.getDeployment(ctx, key)
	if err != nil {
		return nil, err
	}
	if d == nil || string(d.UID) != inc.Spec.Workload.UID {
		return nil, fmt.Errorf("cancelled attempt target absent or replaced; barrier unconfirmed")
	}
	f, err := readFence(d)
	if err != nil {
		return nil, err
	}
	if f == nil {
		f = &executionFence{Version: 2, PolicyUID: inc.Spec.PolicyRef.UID, PolicyName: inc.Spec.PolicyRef.Name, DeploymentUID: inc.Spec.Workload.UID}
	}
	if f.PolicyUID != inc.Spec.PolicyRef.UID || f.PolicyName != inc.Spec.PolicyRef.Name || f.DeploymentUID != inc.Spec.Workload.UID {
		return nil, fmt.Errorf("cancelled attempt fence owner mismatch")
	}
	if f.Version != 2 || f.ClosedThroughGeneration < inc.Spec.PolicyGeneration {
		f.Version = 2
		f.ClosedThroughGeneration = max(f.ClosedThroughGeneration, inc.Spec.PolicyGeneration)
		putFence(d, f)
		if _, err := readFence(d); err != nil {
			return nil, err
		}
		if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
			return nil, err
		}
	}
	live, err := x.r.getDeployment(ctx, key)
	if err != nil {
		return nil, err
	}
	if live == nil || live.UID != d.UID {
		return nil, fmt.Errorf("cancelled attempt target changed after CAS")
	}
	confirmed, err := readFence(live)
	if err != nil {
		return nil, err
	}
	if confirmed == nil || confirmed.PolicyUID != f.PolicyUID || confirmed.PolicyName != f.PolicyName || confirmed.DeploymentUID != f.DeploymentUID || confirmed.ClosedThroughGeneration < inc.Spec.PolicyGeneration {
		return nil, fmt.Errorf("cancelled attempt barrier unconfirmed")
	}
	return live, nil
}
