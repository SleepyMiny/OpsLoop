package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	"github.com/SleepyMiny/OpsLoop/internal/verification"
)

const (
	PhaseExecuting        = "EXECUTING"
	PhaseVerifying        = "VERIFYING"
	ReasonBudgetExhausted = "RUNTIME_BUDGET_EXHAUSTED"

	fieldOwner   = "opsloop-controller"
	verifyPeriod = 2 * time.Second
)

// lastReserved returns the last attempt when it is still Reserved, i.e. its Deployment change is not settled.
func lastReserved(inc *v1alpha1.RecoveryIncident) *v1alpha1.Attempt {
	at := inc.Status.Runtime.Attempts
	if len(at) == 0 || at[len(at)-1].Outcome != recovery.OutcomeReserved {
		return nil
	}
	return &at[len(at)-1]
}

// settleReserved fixes a Reserved attempt's outcome from the live template when the Incident goes terminal.
func settleReserved(inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) {
	if a := lastReserved(inc); a != nil {
		closeAttempt(a, recovery.ReservedOutcome(live, types.UID(inc.Spec.Workload.UID), *a), now)
	}
}

func closeAttempt(a *v1alpha1.Attempt, outcome string, now time.Time) {
	t := metav1.NewTime(now)
	a.Outcome, a.CompletedAt = outcome, &t
}

// budgetLeft counts every reserved attempt, Cancelled included, once (arch §7:195).
func budgetLeft(inc *v1alpha1.RecoveryIncident) bool {
	return int32(len(inc.Status.Runtime.Attempts)) < inc.Spec.Policy.Budget.MaxActionsPerIncident
}

// nextActionID is the id a selection in this evaluation would reserve; empty when no attempt can follow.
func nextActionID(inc *v1alpha1.RecoveryIncident) string {
	if inc.Spec.Policy.Mode != v1alpha1.PolicyModeEnforce || !budgetLeft(inc) {
		return ""
	}
	return fmt.Sprintf("%s-a%d", inc.Name, len(inc.Status.Runtime.Attempts)+1)
}

// startAttempt persists the action intent before any Deployment change, then executes it in the same reconcile (7.3 ①②).
// live is this reconcile's APIReader Deployment whose template the global check matched against the expected hash.
func (r *PolicyReconciler) startAttempt(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, dec v1alpha1.RecoveryDecision, now time.Time) (ctrl.Result, error) {
	if !budgetLeft(inc) {
		return r.finish(ctx, p, inc, PhaseFailed, ReasonBudgetExhausted, "", now)
	}
	target := live.Spec.Template.DeepCopy()
	recovery.Mutate(target, dec.SelectedHostname, dec.ActionID)
	rt := &inc.Status.Runtime
	rt.Attempts = append(rt.Attempts, v1alpha1.Attempt{
		ActionID:           dec.ActionID,
		Outcome:            recovery.OutcomeReserved,
		NodeName:           dec.SelectedNodeName,
		NodeUID:            dec.SelectedNodeUID,
		Hostname:           dec.SelectedHostname,
		PreTemplateHash:    expectedTemplateHash(inc),
		TargetTemplateHash: v1alpha1.TemplateHash(*target),
		Decision:           dec,
		ReservedAt:         metav1.NewTime(now),
	})
	setPhase(inc, PhaseExecuting, dec.DecisionReason, "", now)
	if err := r.Status().Update(ctx, inc); err != nil {
		// The Deployment is untouched; a stale duplicate reservation ends here.
		return requeueOnConflict(err)
	}
	return r.execute(ctx, p, inc, live, now)
}

// resume continues the Reserved attempt found at reconcile start, e.g. after a controller restart (7.3 ②③).
func (r *PolicyReconciler) resume(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	a := lastReserved(inc)
	action, reason := recovery.Resume(live, types.UID(inc.Spec.Workload.UID), inc.Spec.Baseline.SpecHash, *a)
	switch action {
	case recovery.Superseded:
		closeAttempt(a, recovery.OutcomeSuperseded, now)
		return r.finish(ctx, p, inc, PhaseSuperseded, reason, "", now)
	case recovery.AlreadyApplied:
		var rss appsv1.ReplicaSetList
		if err := r.List(ctx, &rss, client.InNamespace(live.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		// The attempt RS creation time keeps a restart from postponing the placement timeout (arch §7:196).
		patched := a.ReservedAt
		if rs, _ := verification.AttemptPod(rss.Items, nil, live.UID, a.ActionID); rs != nil {
			patched = rs.CreationTimestamp
		}
		a.Outcome, a.PatchedAt = recovery.OutcomePatched, &patched
		inc.Status.Runtime.ExpectedTemplateHash = a.TargetTemplateHash
		setPhase(inc, PhaseVerifying, "", "resumed", now)
		if err := r.Status().Update(ctx, inc); err != nil {
			return requeueOnConflict(err)
		}
		return ctrl.Result{RequeueAfter: verifyPeriod}, nil
	}

	// A decision reserved before a long controller outage may have expired, so the reserved Node is checked again.
	dec, cleared, err := r.decide(ctx, p, inc, live, now, "")
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleared {
		closeAttempt(a, recovery.OutcomeCancelled, now)
		return r.finish(ctx, p, inc, PhaseAborted, ReasonTriggerCleared, "", now)
	}
	i := slices.IndexFunc(dec.Candidates, func(c v1alpha1.CandidateEvaluation) bool { return c.NodeUID == a.NodeUID })
	if i >= 0 && dec.Candidates[i].Eligible {
		return r.execute(ctx, p, inc, live, now)
	}
	why := "not a candidate"
	if i >= 0 {
		why = strings.Join(dec.Candidates[i].RejectReasons, ",")
	}
	closeAttempt(a, recovery.OutcomeCancelled, now)
	inc.Status.Runtime.Decision = &dec
	setPhase(inc, PhaseWaiting, dec.DecisionReason, "reserved node "+a.NodeName+" no longer eligible: "+why, now)
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: waitingPeriod}, nil
}

// execute is the only Deployment write. The live resourceVersion rides along as the optimistic concurrency precondition.
func (r *PolicyReconciler) execute(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	if inc.Spec.Policy.Mode != v1alpha1.PolicyModeEnforce || p.Spec.Mode != v1alpha1.PolicyModeEnforce || !p.Spec.Enabled {
		return ctrl.Result{}, fmt.Errorf("refusing to change Deployment %s: policy is not enabled in Enforce mode", live.Name)
	}
	a := lastReserved(inc)
	preRV := live.ResourceVersion
	recovery.Mutate(&live.Spec.Template, a.Hostname, a.ActionID)
	if err := r.Update(ctx, live, client.FieldOwner(fieldOwner)); err != nil {
		// Never overwrite on conflict; whether the change landed is settled by the next reconcile's Resume.
		return requeueOnConflict(err)
	}
	h := v1alpha1.TemplateHash(live.Spec.Template)
	msg := ""
	if h != a.TargetTemplateHash {
		msg = "applied template hash " + h + " differs from target (apiserver defaulting)"
	}
	t := metav1.NewTime(now)
	a.Outcome, a.PreResourceVersion, a.PatchedAt = recovery.OutcomePatched, preRV, &t
	inc.Status.Runtime.ExpectedTemplateHash = h
	setPhase(inc, PhaseVerifying, "", msg, now)
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: verifyPeriod}, nil
}

// verify observes the attempt Pod once (7.4); a failure re-evaluates in the same reconcile while budget remains.
func (r *PolicyReconciler) verify(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	rt := &inc.Status.Runtime
	i := len(rt.Attempts) - 1
	if i < 0 || rt.Attempts[i].Outcome != recovery.OutcomePatched {
		return ctrl.Result{}, fmt.Errorf("incident %s is VERIFYING without a Patched attempt", inc.Name)
	}
	spec := inc.Spec.Policy
	var rss appsv1.ReplicaSetList
	if err := r.List(ctx, &rss, client.InNamespace(live.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(live.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	rs, pod := verification.AttemptPod(rss.Items, pods.Items, live.UID, rt.Attempts[i].ActionID)
	in := verification.Input{Now: now, Attempt: rt.Attempts[i], Verification: spec.Verification, Budget: spec.Budget, RS: rs, Pod: pod}
	if pod != nil && pod.Status.PodIP != "" && podIsReady(pod) {
		in.ProbeRan = true
		in.ProbeErr = r.Probe(ctx, pod.Status.PodIP, spec.Verification.Port, string(pod.UID))
	}
	var eps discoveryv1.EndpointSliceList
	if err := r.List(ctx, &eps, client.InNamespace(live.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: spec.Verification.ServiceName}); err != nil {
		return ctrl.Result{}, err
	}
	in.Slices = eps.Items

	next, outcome := verification.Step(in)
	rt.Attempts[i] = next
	switch outcome {
	case verification.Succeeded:
		rt.OldPodsOutstanding = oldPodsOutstanding(rss.Items, pods.Items, live.UID, rs)
		return r.finish(ctx, p, inc, PhaseRecovered, "", "", now)
	case verification.Failed:
		if !budgetLeft(inc) {
			return r.finish(ctx, p, inc, PhaseFailed, ReasonBudgetExhausted, next.ActionID+" failed: "+next.FailureReason, now)
		}
		// The failed Node is rejected through the attempts merge (COOLDOWN); the Policy ledger is written on release.
		return r.evaluate(ctx, p, inc, live, now)
	}
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: verifyPeriod}, nil
}

func podIsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// oldPodsOutstanding lists Pods of the Deployment's other ReplicaSets still present at success; it is recorded once.
func oldPodsOutstanding(rss []appsv1.ReplicaSet, pods []corev1.Pod, deploymentUID types.UID, attemptRS *appsv1.ReplicaSet) []v1alpha1.PodRef {
	old := map[types.UID]bool{}
	for i := range rss {
		ref := metav1.GetControllerOf(&rss[i])
		if ref != nil && ref.UID == deploymentUID && rss[i].UID != attemptRS.UID {
			old[rss[i].UID] = true
		}
	}
	var out []v1alpha1.PodRef
	for i := range pods {
		ref := metav1.GetControllerOf(&pods[i])
		if ref == nil || !old[ref.UID] {
			continue
		}
		// shortcut: only the first 5 old Pods are recorded (CRD MaxItems), enough for replicas=1 workloads
		if len(out) == v1alpha1.MaxOldPodsOutstanding {
			break
		}
		out = append(out, v1alpha1.PodRef{Name: pods[i].Name, UID: string(pods[i].UID), NodeName: pods[i].Spec.NodeName, Terminating: pods[i].DeletionTimestamp != nil})
	}
	return out
}

// getDeployment reads a Deployment through the APIReader; nil means it does not exist.
func (r *PolicyReconciler) getDeployment(ctx context.Context, key types.NamespacedName) (*appsv1.Deployment, error) {
	d := &appsv1.Deployment{}
	if err := r.APIReader.Get(ctx, key, d); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}
