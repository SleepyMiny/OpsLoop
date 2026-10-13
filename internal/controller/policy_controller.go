// Package controller runs the single RecoveryPolicy reconciler: Monitor, Incident reservation, progress and release.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
)

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 rbac:roleName=opsloop-controller paths=./... output:rbac:artifacts:config=../../config/rbac

// Deployment write verbs must never appear here; Enforce permissions live only in config/rbac/enforce.
// +kubebuilder:rbac:groups=opsloop.io,resources=recoverypolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=opsloop.io,resources=recoverypolicies/status,verbs=get;update
// +kubebuilder:rbac:groups=opsloop.io,resources=recoveryincidents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=opsloop.io,resources=recoveryincidents/status,verbs=get;update
// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

const (
	PolicyLabel = "opsloop.io/policy"
	// Literal field path so the same key works as an apiserver field selector when no cache is used.
	podNodeNameField = "spec.nodeName"
	hostnameLabel    = "kubernetes.io/hostname"

	monitorPeriod   = 15 * time.Second
	waitingPeriod   = 10 * time.Second
	conflictRequeue = time.Second

	CondMonitoring      = "Monitoring"
	CondActive          = "Active"
	CondDuplicatePolicy = "DuplicatePolicy"
	CondDisabled        = "Disabled"
	CondTargetNotFound  = "TargetNotFound"
	CondCircuitCapacity = "CircuitCapacity"
	CondCircuitOpen     = "CircuitOpen"
	CondIncidentActive  = "IncidentActive"

	PhaseWaiting             = "WAITING"
	PhaseRecovered           = "RECOVERED"
	PhaseFailed              = "FAILED"
	PhaseAborted             = "ABORTED"
	PhaseSuperseded          = "SUPERSEDED"
	PhaseUnsupportedWorkload = "UNSUPPORTED_WORKLOAD"
	PhaseManualRequired      = "MANUAL_REQUIRED"
	PhaseObserved            = "OBSERVED"
	PhaseIncidentLost        = "INCIDENT_LOST"

	ReasonPolicyDeleted        = "POLICY_DELETED"
	ReasonDuplicatePolicy      = "DUPLICATE_POLICY"
	ReasonPolicyDisabled       = "POLICY_DISABLED"
	ReasonModeChanged          = "MODE_CHANGED"
	ReasonWorkloadDeleted      = "WORKLOAD_DELETED"
	ReasonWorkloadReplaced     = "WORKLOAD_REPLACED"
	ReasonSpecChanged          = "SPEC_CHANGED_EXTERNALLY"
	ReasonTemplateChanged      = "TEMPLATE_CHANGED_EXTERNALLY"
	ReasonDeadlineExceeded     = "DEADLINE_EXCEEDED"
	ReasonUnsupportedWorkload  = "UNSUPPORTED_WORKLOAD"
	ReasonClassNotAutomated    = "CLASS_NOT_AUTOMATED"
	ReasonInsufficientEvidence = "INSUFFICIENT_EVIDENCE"
	ReasonTriggerCleared       = "TRIGGER_CLEARED"
	attemptFailed              = "Failed"
)

// PolicyReconciler is the only reconciler; MaxConcurrentReconciles=1 plus leader election serializes every action.
type PolicyReconciler struct {
	client.Client
	// APIReader reads Policy, Incident and Deployment live so a stale cache never drives a decision.
	APIReader client.Reader
	Now       func() time.Time
	Prom      promv1.API
	// Probe is used by the Enforce verification step (P4).
	Probe func(ctx context.Context, podIP string, port int32, podUID string) error

	// Accessed only from the single reconcile worker.
	restarts map[types.NamespacedName]map[string]*restartState
}

type restartState struct {
	count int32
	obs   []classification.RestartObservation
}

// SetupWithManager registers the Pod nodeName index and the watches of 6.0.
func (r *PolicyReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, podNodeNameField, func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RecoveryPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// policiesFor maps a Deployment to Policies targeting it, a Pod to Policies in its namespace and a Node to every Policy.
func (r *PolicyReconciler) policiesFor(ctx context.Context, obj client.Object) []reconcile.Request {
	var opts []client.ListOption
	if _, ok := obj.(*corev1.Node); !ok {
		opts = append(opts, client.InNamespace(obj.GetNamespace()))
	}
	var list v1alpha1.RecoveryPolicyList
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil
	}
	_, isDeployment := obj.(*appsv1.Deployment)
	var out []reconcile.Request
	for _, p := range list.Items {
		if isDeployment && p.Spec.TargetRef.Name != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
	}
	return out
}

func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	now := r.Now()
	p := &v1alpha1.RecoveryPolicy{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, p); err != nil {
		if apierrors.IsNotFound(err) {
			delete(r.restarts, req.NamespacedName)
			return ctrl.Result{}, r.abortOrphans(ctx, req.NamespacedName, now)
		}
		return ctrl.Result{}, err
	}
	if p.Status.ActiveIncident != nil {
		return r.reconcileActive(ctx, p, now)
	}

	dup, err := r.duplicated(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if dup {
		return r.block(ctx, p, CondDuplicatePolicy, "another RecoveryPolicy targets "+p.Spec.TargetRef.Name, now)
	}
	if !p.Spec.Enabled {
		return r.block(ctx, p, CondDisabled, "", now)
	}
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.TargetRef.Name}, d); err != nil {
		if apierrors.IsNotFound(err) {
			return r.block(ctx, p, CondTargetNotFound, "", now)
		}
		return ctrl.Result{}, err
	}
	// arch §7:215 forbids automatic circuit removal, so a full list blocks every new Incident until reset.
	if len(p.Status.Circuits) >= v1alpha1.MaxCircuits {
		return r.block(ctx, p, CondCircuitCapacity, "circuits full; reset status.circuits", now)
	}

	res, digest, err := r.monitor(ctx, p, d, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !res.Detected {
		after := monitorPeriod
		if res.RecheckAfter > 0 && res.RecheckAfter < after {
			after = res.RecheckAfter
		}
		if setCondition(p, CondActive, "", now) {
			if err := r.writePolicy(ctx, p); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{RequeueAfter: after}, nil
	}
	for _, c := range p.Status.Circuits {
		if c.Signature == res.Trigger.Signature {
			return r.block(ctx, p, CondCircuitOpen, "circuit open for signature "+c.Signature, now)
		}
	}
	return r.reserve(ctx, p, res.Trigger, digest, now)
}

// monitor collects the 6.1 inputs from the cache and classifies them.
func (r *PolicyReconciler) monitor(ctx context.Context, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment, now time.Time) (classification.Result, string, error) {
	nodes, err := r.nodes(ctx)
	if err != nil {
		return classification.Result{}, "", err
	}
	var rss appsv1.ReplicaSetList
	if err := r.List(ctx, &rss, client.InNamespace(p.Namespace)); err != nil {
		return classification.Result{}, "", err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace)); err != nil {
		return classification.Result{}, "", err
	}
	monitored := classification.MonitoredPods(d, rss.Items, pods.Items)
	key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	digest := imageDigest(d, monitored)
	return classification.Classify(classification.Input{
		Now:         now,
		Detection:   p.Spec.Detection,
		WorkloadUID: d.UID,
		ImageDigest: digest,
		Pods:        monitored,
		Nodes:       nodes,
		Restarts:    r.observeRestarts(key, monitored, now, p.Spec.Detection.RestartWindow.Duration),
	}), digest, nil
}

// observeRestarts turns restartCount increases into observations and keeps only Pods still monitored.
// shortcut: restart history lives only in memory, so a controller restart under-counts (conservative); persist in Incident/Bundle in P5
func (r *PolicyReconciler) observeRestarts(key types.NamespacedName, pods []corev1.Pod, now time.Time, window time.Duration) []classification.RestartObservation {
	if r.restarts == nil {
		r.restarts = map[types.NamespacedName]map[string]*restartState{}
	}
	old := r.restarts[key]
	cur := map[string]*restartState{}
	var out []classification.RestartObservation
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			k := string(p.UID) + "/" + cs.Name
			st := old[k]
			if st == nil {
				// First sight is the baseline; earlier restarts are not attributable to a time.
				st = &restartState{count: cs.RestartCount}
			}
			// shortcut: k>1 increases in one observation all reuse the last terminated exit and reason; record each in P5 persistent history
			for ; st.count < cs.RestartCount; st.count++ {
				o := classification.RestartObservation{PodUID: p.UID, Container: cs.Name, At: now}
				if t := cs.LastTerminationState.Terminated; t != nil {
					o.ExitCode, o.Reason = t.ExitCode, t.Reason
				}
				st.obs = append(st.obs, o)
			}
			st.obs = slices.DeleteFunc(st.obs, func(o classification.RestartObservation) bool { return now.Sub(o.At) > window })
			cur[k] = st
			out = append(out, st.obs...)
		}
	}
	r.restarts[key] = cur
	return out
}

// reserve persists the Incident intent on the Policy before any Incident exists (6.5 ①).
func (r *PolicyReconciler) reserve(ctx context.Context, p *v1alpha1.RecoveryPolicy, t v1alpha1.Trigger, digest string, now time.Time) (ctrl.Result, error) {
	live, err := r.liveDeployment(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if live == nil {
		return r.block(ctx, p, CondTargetNotFound, "", now)
	}
	reservation(p, live, t, digest, now)
	setCondition(p, CondIncidentActive, p.Status.ActiveIncident.Name, now)
	if err := r.writePolicy(ctx, p); err != nil {
		return requeueOnConflict(err)
	}
	return r.createAndProgress(ctx, p, live, now)
}

// reservation sets the next sequence and activeIncident; the baseline comes from the live Deployment.
func reservation(p *v1alpha1.RecoveryPolicy, live *appsv1.Deployment, t v1alpha1.Trigger, digest string, now time.Time) {
	seq := p.Status.IncidentSequence + 1
	tmpl := live.Spec.Template
	base := v1alpha1.Baseline{
		TemplateHash: v1alpha1.TemplateHash(tmpl),
		SpecHash:     v1alpha1.SpecHash(live.Spec),
		ImageDigest:  digest,
		Replicas:     1,
		Hostname:     tmpl.Spec.NodeSelector[hostnameLabel],
	}
	if len(tmpl.Spec.Containers) > 0 {
		base.Image = tmpl.Spec.Containers[0].Image
	}
	if live.Spec.Replicas != nil {
		base.Replicas = *live.Spec.Replicas
	}
	p.Status.IncidentSequence = seq
	p.Status.ActiveIncident = &v1alpha1.ActiveIncidentRef{
		// The Policy UID prefix keeps a recreated Policy from colliding with Incidents left by its predecessor.
		Name:       fmt.Sprintf("%s-%s-%05d", p.Name, string(p.UID)[:8], seq),
		Sequence:   seq,
		ReservedAt: metav1.NewTime(now),
		Trigger:    t,
		Baseline:   base,
	}
}

// createAndProgress creates the reserved Incident deterministically, records its UID and continues in the same reconcile (6.5 ②).
func (r *PolicyReconciler) createAndProgress(ctx context.Context, p *v1alpha1.RecoveryPolicy, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	inc := incidentFor(p, live)
	if err := r.Create(ctx, inc); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		inc = &v1alpha1.RecoveryIncident{}
		if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: a.Name}, inc); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.confirmAndProgress(ctx, p, inc, live, now)
}

// incidentFor builds the Incident from activeIncident alone, so a retry after a crash creates the same object.
func incidentFor(p *v1alpha1.RecoveryPolicy, live *appsv1.Deployment) *v1alpha1.RecoveryIncident {
	a := p.Status.ActiveIncident
	inc := &v1alpha1.RecoveryIncident{
		ObjectMeta: metav1.ObjectMeta{Name: a.Name, Namespace: p.Namespace, Labels: map[string]string{PolicyLabel: p.Name}},
		Spec: v1alpha1.RecoveryIncidentSpec{
			PolicyRef:        v1alpha1.PolicyRef{Name: p.Name, UID: string(p.UID)},
			Sequence:         a.Sequence,
			PolicyGeneration: p.Generation,
			PolicyHash:       v1alpha1.PolicyHash(p.Spec),
			Policy:           p.Spec,
			Workload:         v1alpha1.WorkloadRef{Name: p.Spec.TargetRef.Name},
			Trigger:          a.Trigger,
			Baseline:         a.Baseline,
			DetectedAt:       a.ReservedAt,
			Deadline:         metav1.NewTime(a.ReservedAt.Add(p.Spec.Budget.IncidentDeadline.Duration)),
		},
	}
	if live != nil {
		inc.Spec.Workload.UID = string(live.UID)
	}
	return inc
}

// confirmAndProgress checks that the Incident is this reservation's, records its UID and progresses it.
func (r *PolicyReconciler) confirmAndProgress(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	if inc.Spec.PolicyRef.UID != string(p.UID) || inc.Spec.Sequence != a.Sequence {
		err := fmt.Errorf("existing incident %s does not belong to this policy reservation", inc.Name)
		if setCondition(p, CondIncidentActive, err.Error(), now) {
			_ = r.writePolicy(ctx, p)
		}
		return ctrl.Result{}, err
	}
	a.IncidentUID = string(inc.UID)
	if err := r.writePolicy(ctx, p); err != nil {
		return requeueOnConflict(err)
	}
	if isTerminal(inc.Status.Runtime.Phase) {
		return r.release(ctx, p, inc, now)
	}
	return r.progress(ctx, p, inc, live, now)
}

// reconcileActive reads the active Incident live once and resumes it after any restart (6.5 ③).
func (r *PolicyReconciler) reconcileActive(ctx context.Context, p *v1alpha1.RecoveryPolicy, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	live, err := r.liveDeployment(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	inc := &v1alpha1.RecoveryIncident{}
	err = r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: a.Name}, inc)
	switch {
	case apierrors.IsNotFound(err) && a.IncidentUID == "":
		// Crashed between reservation and creation.
		return r.createAndProgress(ctx, p, live, now)
	case apierrors.IsNotFound(err):
		return r.releaseLost(ctx, p, now)
	case err != nil:
		return ctrl.Result{}, err
	case a.IncidentUID == "":
		return r.confirmAndProgress(ctx, p, inc, live, now)
	case string(inc.UID) != a.IncidentUID:
		return r.releaseLost(ctx, p, now)
	case isTerminal(inc.Status.Runtime.Phase):
		// Crashed after the terminal write and before release.
		return r.release(ctx, p, inc, now)
	}
	return r.progress(ctx, p, inc, live, now)
}

// progress applies the global checks and then the phase step (6.6).
func (r *PolicyReconciler) progress(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	phase, reason, err := r.globalCheck(ctx, p, inc, live, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if phase != "" {
		return r.finish(ctx, p, inc, phase, reason, "", now)
	}
	switch inc.Status.Runtime.Phase {
	case "":
		if issues := candidates.CheckWorkload(live); len(issues) > 0 {
			return r.finish(ctx, p, inc, PhaseUnsupportedWorkload, ReasonUnsupportedWorkload, strings.Join(issues, "; "), now)
		}
		switch inc.Spec.Trigger.Class {
		case classification.ClassInfrastructure:
		case classification.ClassUnknown:
			return r.finish(ctx, p, inc, PhaseManualRequired, ReasonInsufficientEvidence, "", now)
		default:
			return r.finish(ctx, p, inc, PhaseManualRequired, ReasonClassNotAutomated, "", now)
		}
		return r.evaluate(ctx, p, inc, live, now)
	case PhaseWaiting:
		return r.evaluate(ctx, p, inc, live, now)
	}
	// EXECUTING and VERIFYING are driven by the Enforce runtime added in P4.
	return ctrl.Result{RequeueAfter: monitorPeriod}, nil
}

// globalCheck returns the terminal phase and reason the Incident must take now, or empty when it may continue.
func (r *PolicyReconciler) globalCheck(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (string, string, error) {
	dup, err := r.duplicated(ctx, p)
	switch {
	case err != nil:
		return "", "", err
	case dup:
		return PhaseAborted, ReasonDuplicatePolicy, nil
	case !p.Spec.Enabled:
		return PhaseAborted, ReasonPolicyDisabled, nil
	case p.Spec.Mode != inc.Spec.Policy.Mode:
		return PhaseAborted, ReasonModeChanged, nil
	case live == nil:
		return PhaseSuperseded, ReasonWorkloadDeleted, nil
	case string(live.UID) != inc.Spec.Workload.UID:
		return PhaseSuperseded, ReasonWorkloadReplaced, nil
	case v1alpha1.SpecHash(live.Spec) != inc.Spec.Baseline.SpecHash:
		return PhaseSuperseded, ReasonSpecChanged, nil
	case v1alpha1.TemplateHash(live.Spec.Template) != expectedTemplateHash(inc):
		return PhaseSuperseded, ReasonTemplateChanged, nil
	case !now.Before(inc.Spec.Deadline.Time):
		return PhaseFailed, ReasonDeadlineExceeded, nil
	}
	return "", "", nil
}

// expectedTemplateHash is the template the Deployment should have now; empty status means nothing was applied.
func expectedTemplateHash(inc *v1alpha1.RecoveryIncident) string {
	if h := inc.Status.Runtime.ExpectedTemplateHash; h != "" {
		return h
	}
	return inc.Spec.Baseline.TemplateHash
}

// evaluate re-collects Nodes, Pods and metrics and records one decision (DETECTED and WAITING).
func (r *PolicyReconciler) evaluate(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	nodes, err := r.nodes(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !classification.TriggerPersists(inc.Spec.Trigger, nodes) {
		return r.finish(ctx, p, inc, PhaseAborted, ReasonTriggerCleared, "", now)
	}
	spec := inc.Spec.Policy
	sel, err := metav1.LabelSelectorAsSelector(&spec.CandidateSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	var cands []corev1.Node
	podsByNode := map[string][]corev1.Pod{}
	for _, n := range nodes {
		if !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: n.Name}); err != nil {
			return ctrl.Result{}, err
		}
		cands = append(cands, *n)
		podsByNode[n.Name] = pods.Items
	}
	metrics, merr := candidates.QueryMetrics(ctx, r.Prom, now, 2*spec.Filter.MaxSampleAge.Duration)
	dec := candidates.Evaluate(candidates.Input{
		Now:                  now,
		Spec:                 spec,
		RecentFailures:       p.Status.RecentFailures,
		Attempts:             inc.Status.Runtime.Attempts,
		PolicyHash:           inc.Spec.PolicyHash,
		PolicyGeneration:     inc.Spec.PolicyGeneration,
		IncidentName:         inc.Name,
		Deployment:           live,
		Nodes:                cands,
		PodsByNode:           podsByNode,
		Metrics:              metrics,
		MetricsErr:           merr,
		ImageDigest:          inc.Spec.Baseline.ImageDigest,
		BaselineTemplateHash: inc.Spec.Baseline.TemplateHash,
	})
	inc.Status.Runtime.Decision = &dec
	if dec.SelectedNodeName != "" && spec.Mode == v1alpha1.PolicyModeObserveOnly {
		return r.finish(ctx, p, inc, PhaseObserved, dec.DecisionReason, "", now)
	}
	// P4 replaces the Enforce selection branch with attempt reservation and execution (7.3); until then it only waits.
	setPhase(inc, PhaseWaiting, dec.DecisionReason, "", now)
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: waitingPeriod}, nil
}

// finish writes a terminal phase and releases the Policy in the same reconcile.
func (r *PolicyReconciler) finish(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, phase, reason, msg string, now time.Time) (ctrl.Result, error) {
	setPhase(inc, phase, reason, msg, now)
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return r.release(ctx, p, inc, now)
}

// release clears activeIncident only for the same sequence and UID, opening circuits and recording failures (6.5 ④).
func (r *PolicyReconciler) release(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	if a == nil || a.Sequence != inc.Spec.Sequence || a.IncidentUID != string(inc.UID) {
		return ctrl.Result{RequeueAfter: monitorPeriod}, nil
	}
	phase := inc.Status.Runtime.Phase
	if opensCircuit(phase) {
		addCircuit(p, inc.Spec.Trigger, inc.Name, phase, now)
	}
	failures := p.Status.RecentFailures
	for _, at := range inc.Status.Runtime.Attempts {
		if at.Outcome != attemptFailed {
			continue
		}
		f := v1alpha1.NodeFailure{ActionID: at.ActionID, NodeName: at.NodeName, NodeUID: at.NodeUID, At: metav1.NewTime(now), Reason: at.FailureReason, Incident: inc.Name}
		if at.CompletedAt != nil {
			f.At = *at.CompletedAt
		}
		if i := slices.IndexFunc(failures, func(x v1alpha1.NodeFailure) bool { return x.ActionID == f.ActionID }); i >= 0 {
			failures[i] = f
		} else {
			failures = append(failures, f)
		}
	}
	keep := max(p.Spec.Score.RecentFailureWindow.Duration, p.Spec.Budget.Cooldown.Duration)
	failures = slices.DeleteFunc(failures, func(f v1alpha1.NodeFailure) bool { return now.Sub(f.At.Time) > keep })
	slices.SortStableFunc(failures, func(a, b v1alpha1.NodeFailure) int { return a.At.Time.Compare(b.At.Time) })
	// shortcut: more than 20 failures within 30 minutes drops the oldest penalties, redesign for large candidate pools
	if len(failures) > v1alpha1.MaxRecentFailures {
		failures = failures[len(failures)-v1alpha1.MaxRecentFailures:]
	}
	p.Status.RecentFailures = failures
	return r.writeRelease(ctx, p, now)
}

// releaseLost frees a Policy whose created Incident disappeared; recreating it would reset the attempt budget (arch §9:290).
func (r *PolicyReconciler) releaseLost(ctx context.Context, p *v1alpha1.RecoveryPolicy, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	addCircuit(p, a.Trigger, a.Name, PhaseIncidentLost, now)
	return r.writeRelease(ctx, p, now)
}

func addCircuit(p *v1alpha1.RecoveryPolicy, t v1alpha1.Trigger, incident, phase string, now time.Time) {
	p.Status.Circuits = append(p.Status.Circuits, v1alpha1.Circuit{Signature: t.Signature, Class: t.Class, Incident: incident, Phase: phase, OpenedAt: metav1.NewTime(now)})
}

func (r *PolicyReconciler) writeRelease(ctx context.Context, p *v1alpha1.RecoveryPolicy, now time.Time) (ctrl.Result, error) {
	p.Status.ActiveIncident = nil
	setCondition(p, CondActive, "", now)
	if err := r.writePolicy(ctx, p); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: monitorPeriod}, nil
}

// abortOrphans closes every non-terminal Incident of a deleted Policy without touching the Deployment.
// shortcut: a Policy deleted while the controller is down leaves no reconcile key after restart, so non-terminal Incidents remain (no Deployment risk); a startup sweep of all Incidents arrives with the Incident reconciler in P5
func (r *PolicyReconciler) abortOrphans(ctx context.Context, key types.NamespacedName, now time.Time) error {
	var list v1alpha1.RecoveryIncidentList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(key.Namespace), client.MatchingLabels{PolicyLabel: key.Name}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		inc := &list.Items[i]
		if isTerminal(inc.Status.Runtime.Phase) {
			continue
		}
		setPhase(inc, PhaseAborted, ReasonPolicyDeleted, "", now)
		errs = append(errs, r.Status().Update(ctx, inc))
	}
	return errors.Join(errs...)
}

// block records the Monitoring reason explaining why no Incident is created and waits for the next period.
func (r *PolicyReconciler) block(ctx context.Context, p *v1alpha1.RecoveryPolicy, reason, msg string, now time.Time) (ctrl.Result, error) {
	if setCondition(p, reason, msg, now) {
		if err := r.writePolicy(ctx, p); err != nil {
			return requeueOnConflict(err)
		}
	}
	return ctrl.Result{RequeueAfter: monitorPeriod}, nil
}

// duplicated reports whether another Policy in the namespace targets the same Deployment.
func (r *PolicyReconciler) duplicated(ctx context.Context, p *v1alpha1.RecoveryPolicy) (bool, error) {
	var list v1alpha1.RecoveryPolicyList
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		return false, err
	}
	for _, o := range list.Items {
		if o.UID != p.UID && o.Spec.TargetRef.Name == p.Spec.TargetRef.Name {
			return true, nil
		}
	}
	return false, nil
}

// liveDeployment reads the target through the APIReader; nil means it does not exist.
func (r *PolicyReconciler) liveDeployment(ctx context.Context, p *v1alpha1.RecoveryPolicy) (*appsv1.Deployment, error) {
	d := &appsv1.Deployment{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.TargetRef.Name}, d); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (r *PolicyReconciler) nodes(ctx context.Context) (map[string]*corev1.Node, error) {
	var list corev1.NodeList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*corev1.Node, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}

// writePolicy is a resourceVersion compare-and-swap status update; every write carries observedGeneration.
func (r *PolicyReconciler) writePolicy(ctx context.Context, p *v1alpha1.RecoveryPolicy) error {
	p.Status.ObservedGeneration = p.Generation
	return r.Status().Update(ctx, p)
}

// setCondition sets the single Monitoring condition's reason; it reports whether anything changed.
func setCondition(p *v1alpha1.RecoveryPolicy, reason, msg string, now time.Time) bool {
	status := metav1.ConditionFalse
	if reason == CondActive || reason == CondIncidentActive {
		status = metav1.ConditionTrue
	}
	return meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: CondMonitoring, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(now),
	})
}

func setPhase(inc *v1alpha1.RecoveryIncident, phase, reason, msg string, now time.Time) {
	rt := &inc.Status.Runtime
	if rt.Phase != phase || rt.LastTransitionTime.IsZero() {
		rt.LastTransitionTime = metav1.NewTime(now)
	}
	rt.Phase, rt.Reason, rt.Message = phase, reason, v1alpha1.Truncate(msg, v1alpha1.MaxMessageLength)
	if isTerminal(phase) {
		t := metav1.NewTime(now)
		rt.CompletedAt = &t
	}
}

// opensCircuit lists the terminal phases after which the same signature must not start a new Incident (D13).
func opensCircuit(phase string) bool {
	switch phase {
	case PhaseFailed, PhaseUnsupportedWorkload, PhaseManualRequired, PhaseObserved:
		return true
	}
	return false
}

func isTerminal(phase string) bool {
	return opensCircuit(phase) || phase == PhaseRecovered || phase == PhaseAborted || phase == PhaseSuperseded
}

// imageDigest prefers the digest pinned in the template image, then the running container's imageID.
func imageDigest(d *appsv1.Deployment, pods []corev1.Pod) string {
	cs := d.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	if i := strings.Index(cs[0].Image, "@sha256:"); i >= 0 {
		return cs[0].Image[i+1:]
	}
	for _, p := range pods {
		for _, s := range p.Status.ContainerStatuses {
			if i := strings.Index(s.ImageID, "@"); s.Name == cs[0].Name && i >= 0 {
				return s.ImageID[i+1:]
			}
		}
	}
	return ""
}

func requeueOnConflict(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: conflictRequeue}, nil
	}
	return ctrl.Result{}, err
}
