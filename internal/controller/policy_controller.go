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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
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

	ReasonPolicyDeleted         = "POLICY_DELETED"
	ReasonDuplicatePolicy       = "DUPLICATE_POLICY"
	ReasonPolicyDisabled        = "POLICY_DISABLED"
	ReasonModeChanged           = "MODE_CHANGED"
	ReasonPolicyChanged         = "POLICY_CHANGED"
	ReasonWorkloadDeleted       = "WORKLOAD_DELETED"
	ReasonWorkloadReplaced      = "WORKLOAD_REPLACED"
	ReasonSpecChanged           = "SPEC_CHANGED_EXTERNALLY"
	ReasonTemplateChanged       = "TEMPLATE_CHANGED_EXTERNALLY"
	ReasonDeadlineExceeded      = "DEADLINE_EXCEEDED"
	ReasonUnsupportedWorkload   = "UNSUPPORTED_WORKLOAD"
	ReasonClassNotAutomated     = "CLASS_NOT_AUTOMATED"
	ReasonInsufficientEvidence  = "INSUFFICIENT_EVIDENCE"
	ReasonTriggerCleared        = "TRIGGER_CLEARED"
	ReasonReservationUIDMissing = "RESERVATION_WORKLOAD_UID_MISSING"
	attemptFailed               = "Failed"
)

// PolicyReconciler is the only reconciler; MaxConcurrentReconciles=1 plus leader election serializes every action.
type PolicyReconciler struct {
	client.Client
	// APIReader reads Policy, Incident, Deployment and final execution inputs live.
	APIReader client.Reader
	Now       func() time.Time
	Prom      promv1.API
	Authority ExecutionAuthorizer
	// Probe checks the attempt Pod directly during VERIFYING.
	Probe func(ctx context.Context, podIP string, port int32, podUID string) error

	// Accessed only from the single reconcile worker.
	restarts  map[types.NamespacedName]map[string]*restartState
	readiness map[types.NamespacedName]readinessHistory
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
		For(&v1alpha1.RecoveryPolicy{}, builder.WithPredicates(policyExecutionPredicate())).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.policiesFor)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// A heartbeat is metadata-only: generation-only filtering would sleep fifteen
// seconds despite a ten-second grant TTL. Enqueue only our grant changes, not
// unrelated metadata/status churn. A delayed worker still fails closed.
func policyExecutionPredicate() predicate.Predicate {
	return predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld != nil && e.ObjectNew != nil && e.ObjectOld.GetAnnotations()[ExecutionGrantAnnotation] != e.ObjectNew.GetAnnotations()[ExecutionGrantAnnotation]
	}})
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
			delete(r.readiness, req.NamespacedName)
			return ctrl.Result{}, r.abortOrphans(ctx, req.NamespacedName, "", now)
		}
		return ctrl.Result{}, err
	}
	// A Policy recreated before its predecessor's delete event was handled still owns that predecessor's Incidents.
	// Only a Policy this controller never wrote can have them: every status write sets observedGeneration.
	if p.Status.ObservedGeneration == 0 {
		if err := r.abortOrphans(ctx, req.NamespacedName, p.UID, now); err != nil {
			return ctrl.Result{}, err
		}
	}
	if stopped(p) {
		return r.reconcileStop(ctx, p)
	}
	if setQuiesced(p, metav1.ConditionFalse, "ExecutionEnabled", "enabled Enforce generation", metav1.NewTime(now)) {
		if err := r.writePolicy(ctx, p); err != nil {
			return requeueOnConflict(err)
		}
	}
	if p.Status.ActiveIncident != nil {
		return r.reconcileActive(ctx, p, now)
	}
	return r.reconcileMonitoring(ctx, p, now)
}

func (r *PolicyReconciler) reconcileMonitoring(ctx context.Context, p *v1alpha1.RecoveryPolicy, now time.Time) (ctrl.Result, error) {
	if p.Spec.Mode == v1alpha1.PolicyModeEnforce {
		if err := r.authorizeExecution(p, nil); err != nil {
			return r.block(ctx, p, ReasonExecutionAuthority, err.Error(), now)
		}
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
	if p.Spec.Mode == v1alpha1.PolicyModeEnforce {
		// Live authority, never cached status, blocks every generation/new Incident.
		live, err := r.liveDeployment(ctx, p)
		if err != nil {
			return ctrl.Result{}, err
		}
		if live == nil {
			return r.block(ctx, p, CondTargetNotFound, "", now)
		}
		f, err := readFence(live)
		if err != nil {
			return ctrl.Result{}, err
		}
		if f != nil && f.SafetyHold != nil {
			return r.block(ctx, p, "SafetyHalted", "Deployment has a persistent safety hold; manual inspection required", now)
		}
	}
	// arch §7:215 forbids automatic circuit removal, so a full list blocks every new Incident until reset.
	if len(p.Status.Circuits) >= v1alpha1.MaxCircuits {
		return r.block(ctx, p, CondCircuitCapacity, "circuits full; reset status.circuits", now)
	}

	conditionsBefore := append([]metav1.Condition(nil), p.Status.Conditions...)
	res, digest, err := r.monitor(ctx, p, d, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !res.Detected {
		after := monitorPeriod
		if res.RecheckAfter > 0 && res.RecheckAfter < after {
			after = res.RecheckAfter
		}
		monitoringChanged := setCondition(p, CondActive, "", now)
		if monitoringChanged || !equality.Semantic.DeepEqual(conditionsBefore, p.Status.Conditions) {
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
	return r.reserve(ctx, p, d, res.Trigger, digest, now)
}

// monitor collects the 6.1 inputs from the cache and classifies them.
func (r *PolicyReconciler) monitor(ctx context.Context, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment, now time.Time) (classification.Result, string, error) {
	nodes, err := r.nodes(ctx)
	if err != nil {
		delete(r.readiness, client.ObjectKeyFromObject(p))
		return classification.Result{}, "", err
	}
	var rss appsv1.ReplicaSetList
	if err := r.List(ctx, &rss, client.InNamespace(p.Namespace)); err != nil {
		delete(r.readiness, client.ObjectKeyFromObject(p))
		return classification.Result{}, "", err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace)); err != nil {
		delete(r.readiness, client.ObjectKeyFromObject(p))
		return classification.Result{}, "", err
	}
	// Deployment revision metadata can lag a new template. An old ReplicaSet
	// must not make its Pods' failures evidence about that new template.
	current := slices.DeleteFunc(rss.Items, func(rs appsv1.ReplicaSet) bool {
		template := rs.Spec.Template.DeepCopy()
		delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
		return !equality.Semantic.DeepEqual(*template, d.Spec.Template)
	})
	monitored := classification.MonitoredPods(d, current, pods.Items)
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
		HealthyPods: r.observeReadiness(p, d, monitored, nodes, now),
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
func (r *PolicyReconciler) reserve(ctx context.Context, p *v1alpha1.RecoveryPolicy, observed *appsv1.Deployment, t v1alpha1.Trigger, digest string, now time.Time) (ctrl.Result, error) {
	live, err := r.liveDeployment(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if live == nil {
		return r.block(ctx, p, CondTargetNotFound, "", now)
	}
	// The trigger and digest describe the Deployment passed to monitor. Never
	// attach that observation to another UID or a changed desired workload.
	// ResourceVersion/status/metadata changes alone do not invalidate it.
	if live.UID != observed.UID || v1alpha1.TemplateHash(live.Spec.Template) != v1alpha1.TemplateHash(observed.Spec.Template) || v1alpha1.SpecHash(live.Spec) != v1alpha1.SpecHash(observed.Spec) {
		return ctrl.Result{RequeueAfter: conflictRequeue}, nil
	}
	if p.Spec.Mode == v1alpha1.PolicyModeEnforce {
		f, err := readFence(live)
		if err != nil {
			return ctrl.Result{}, err
		}
		if f != nil && f.SafetyHold != nil {
			return r.block(ctx, p, "SafetyHalted", "Deployment has a persistent safety hold", now)
		}
	}
	reservation(p, live, t, digest, now)
	// A new observation reservation invalidates completion atomically with its
	// activeIncident pointer, including ObserveOnly generations after a stop.
	if stopped(p) {
		setQuiesced(p, metav1.ConditionFalse, "ReservationPending", "active incident cleanup is pending", metav1.NewTime(now))
	}
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
		Name:        fmt.Sprintf("%s-%s-%05d", p.Name, string(p.UID)[:8], seq),
		Sequence:    seq,
		WorkloadUID: string(live.UID),
		ReservedAt:  metav1.NewTime(now),
		Trigger:     t,
		Baseline:    base,
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
			Workload:         v1alpha1.WorkloadRef{Name: p.Spec.TargetRef.Name, UID: a.WorkloadUID},
			Trigger:          a.Trigger,
			Baseline:         a.Baseline,
			DetectedAt:       a.ReservedAt,
			// The API server stores whole seconds; a fractional deadline would never compare equal after Create.
			Deadline: metav1.NewTime(a.ReservedAt.Add(p.Spec.Budget.IncidentDeadline.Duration).Truncate(time.Second)),
		},
	}
	if binding := executionGrantBinding(p); binding != "" {
		inc.Annotations = map[string]string{ExecutionGrantBindingAnnotation: binding}
	}
	return inc
}

// confirmAndProgress checks that the Incident is this reservation's, records its UID and progresses it.
func (r *PolicyReconciler) confirmAndProgress(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	// An Incident created by someone else must not supply the snapshot Enforce acts on; a differing Workload UID only supersedes.
	want := incidentFor(p, live).Spec
	if a.WorkloadUID == "" && a.IncidentUID != "" {
		// Already-confirmed legacy Incidents retain their immutable workload UID.
		want.Workload.UID = inc.Spec.Workload.UID
	}
	// A spec edit between Create and the UID write leaves an older snapshot that cannot be verified, so only the reservation is matched and the Incident is aborted below.
	stale := inc.Spec.PolicyGeneration != p.Generation
	if stale {
		want.PolicyGeneration, want.PolicyHash, want.Policy, want.Deadline, want.Workload.Name = inc.Spec.PolicyGeneration, inc.Spec.PolicyHash, inc.Spec.Policy, inc.Spec.Deadline, inc.Spec.Workload.Name
	}
	if !equality.Semantic.DeepEqual(inc.Spec, want) {
		err := fmt.Errorf("existing incident %s does not match this policy reservation", inc.Name)
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
	if stale {
		// ABORTED opens no circuit, so a forged snapshot cannot steer one; the next detection reserves with the current spec.
		reason := ReasonPolicyChanged
		if !p.Spec.Enabled {
			reason = ReasonPolicyDisabled
		}
		return r.finish(ctx, p, inc, PhaseAborted, reason, "", now)
	}
	return r.progress(ctx, p, inc, live, now)
}

// reconcileActive reads the active Incident live once and resumes it after any restart (6.5 ③).
func (r *PolicyReconciler) reconcileActive(ctx context.Context, p *v1alpha1.RecoveryPolicy, now time.Time) (ctrl.Result, error) {
	a := p.Status.ActiveIncident
	if a.WorkloadUID == "" && a.IncidentUID == "" {
		// Old pending records cannot prove which Deployment supplied the trigger.
		// Discard the intent rather than seed an identity from a new live object.
		// A stopped Policy can then finish cleanup; enabled monitoring must make
		// a new observation on a later reconcile before reserving anything.
		msg := "discarded pending reservation without observed Deployment UID"
		existing := &v1alpha1.RecoveryIncident{}
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: a.Name}, existing)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			msg += "; existing Incident unchanged, cleanup blocked pending UID verification"
		}
		p.Status.ActiveIncident = nil
		setCondition(p, ReasonReservationUIDMissing, msg, now)
		if err := r.writePolicy(ctx, p); err != nil {
			return requeueOnConflict(err)
		}
		return ctrl.Result{RequeueAfter: monitorPeriod}, nil
	}
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
	if marker := inc.Status.Runtime.WorkloadDispatchActionID; marker != "" {
		at := inc.Status.Runtime.Attempts
		if len(at) == 0 || at[len(at)-1].ActionID != marker {
			return ctrl.Result{}, fmt.Errorf("dispatch marker/action mismatch; manual inspection required")
		}
		return r.haltAndSettle(ctx, p, inc, "WORKLOAD_DISPATCH_UNCONFIRMED")
	}
	if inc.Status.Runtime.SafetyHaltReason != "" {
		return r.haltAndSettle(ctx, p, inc, inc.Status.Runtime.SafetyHaltReason)
	}
	if live != nil && string(live.UID) == inc.Spec.Workload.UID {
		f, err := readFence(live)
		if err != nil {
			return ctrl.Result{}, err
		}
		if f != nil && f.SafetyHold != nil {
			return r.haltAndSettle(ctx, p, inc, f.SafetyHold.Reason)
		}
	}
	phase, reason, err := r.globalCheck(ctx, p, inc, live, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if phase != "" {
		if len(inc.Status.Runtime.Attempts) > 0 && reason == ReasonDeadlineExceeded {
			return r.haltAndSettle(ctx, p, inc, reason)
		}
		settleReserved(inc, live, now)
		return r.finish(ctx, p, inc, phase, reason, "", now)
	}
	if lastReserved(inc) != nil {
		return r.resume(ctx, p, inc, live, now)
	}
	switch inc.Status.Runtime.Phase {
	case "":
		if issues := candidates.CheckWorkload(live); len(issues) > 0 {
			return r.finish(ctx, p, inc, PhaseUnsupportedWorkload, ReasonUnsupportedWorkload, strings.Join(issues, "; "), now)
		}
		switch inc.Spec.Trigger.Class {
		case classification.ClassInfrastructure:
		case classification.ClassApplication:
			if !classification.IsPodReadinessTrigger(inc.Spec.Trigger) || !classification.PodReadinessEnabled(inc.Spec.Policy.Detection) {
				return r.finish(ctx, p, inc, PhaseManualRequired, ReasonClassNotAutomated, "", now)
			}
		case classification.ClassUnknown:
			return r.finish(ctx, p, inc, PhaseManualRequired, ReasonInsufficientEvidence, "", now)
		default:
			return r.finish(ctx, p, inc, PhaseManualRequired, ReasonClassNotAutomated, "", now)
		}
		return r.evaluate(ctx, p, inc, live, now)
	case PhaseWaiting:
		return r.evaluate(ctx, p, inc, live, now)
	case PhaseVerifying:
		return r.verify(ctx, p, inc, live, now)
	}
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
	// A Reserved attempt may already be applied, which is not an external change; Resume decides instead.
	case lastReserved(inc) == nil && v1alpha1.TemplateHash(live.Spec.Template) != expectedTemplateHash(inc):
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

// evaluate records one fresh decision (DETECTED and WAITING) and starts an attempt on an Enforce selection.
func (r *PolicyReconciler) evaluate(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time) (ctrl.Result, error) {
	dec, inputs, cleared, err := r.decide(ctx, p, inc, live, now, nextActionID(inc))
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleared {
		return r.finish(ctx, p, inc, PhaseAborted, ReasonTriggerCleared, "", now)
	}
	inc.Status.Runtime.Decision = &dec
	if dec.SelectedNodeName != "" {
		if inc.Spec.Policy.Mode == v1alpha1.PolicyModeObserveOnly {
			return r.finish(ctx, p, inc, PhaseObserved, dec.DecisionReason, "", now)
		}
		return r.startAttempt(ctx, p, inc, live, dec, inputs, now)
	}
	setPhase(inc, PhaseWaiting, dec.DecisionReason, "", now)
	if err := r.Status().Update(ctx, inc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: waitingPeriod}, nil
}

// decide re-collects Nodes, Pods and metrics and evaluates every candidate; cleared means the trigger is gone.
func (r *PolicyReconciler) decide(ctx context.Context, p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, live *appsv1.Deployment, now time.Time, actionID string) (v1alpha1.RecoveryDecision, candidates.Input, bool, error) {
	nodes, err := r.nodes(ctx)
	if err != nil {
		return v1alpha1.RecoveryDecision{}, candidates.Input{}, false, err
	}
	if !classification.TriggerPersists(inc.Spec.Trigger, nodes) {
		return v1alpha1.RecoveryDecision{}, candidates.Input{}, true, nil
	}
	if persists, err := r.readinessTrigger(ctx, inc, false); err != nil || !persists {
		return v1alpha1.RecoveryDecision{}, candidates.Input{}, !persists && err == nil, err
	}
	spec := inc.Spec.Policy
	sel, err := metav1.LabelSelectorAsSelector(&spec.CandidateSelector)
	if err != nil {
		return v1alpha1.RecoveryDecision{}, candidates.Input{}, false, err
	}
	var cands []corev1.Node
	podsByNode := map[string][]corev1.Pod{}
	for _, n := range nodes {
		if !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: n.Name}); err != nil {
			return v1alpha1.RecoveryDecision{}, candidates.Input{}, false, err
		}
		cands = append(cands, *n)
		podsByNode[n.Name] = pods.Items
	}
	metrics, merr := candidates.QueryMetrics(ctx, r.Prom, now, 2*spec.Filter.MaxSampleAge.Duration)
	inputs := candidates.Input{
		// Collection can take several Prometheus timeouts. Evaluate against the
		// clock after collection, not the time captured at reconcile entry.
		Now:                  r.Now(),
		Spec:                 spec,
		RecentFailures:       p.Status.RecentFailures,
		Attempts:             inc.Status.Runtime.Attempts,
		PolicyHash:           inc.Spec.PolicyHash,
		PolicyGeneration:     inc.Spec.PolicyGeneration,
		IncidentName:         inc.Name,
		ActionID:             actionID,
		Deployment:           live,
		Nodes:                cands,
		PodsByNode:           podsByNode,
		Metrics:              metrics,
		MetricsErr:           merr,
		ImageDigest:          inc.Spec.Baseline.ImageDigest,
		BaselineTemplateHash: inc.Spec.Baseline.TemplateHash,
	}
	return candidates.Evaluate(inputs), inputs, false, nil
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
	if inc.Status.Runtime.SafetyHaltReason != "" {
		p.Status.ActiveIncident = nil
		setCondition(p, "SafetyHalted", inc.Status.Runtime.SafetyHaltReason, now)
		if err := r.writePolicy(ctx, p); err != nil {
			return requeueOnConflict(err)
		}
		return ctrl.Result{RequeueAfter: monitorPeriod}, nil
	}
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

// abortOrphans closes every non-terminal Incident of a deleted Policy, i.e. not owned by keep, without touching the Deployment.
// shortcut: a Policy deleted while the controller is down leaves no reconcile key after restart, so non-terminal Incidents remain (no Deployment risk); a startup sweep of all Incidents arrives with the Incident reconciler in P5
func (r *PolicyReconciler) abortOrphans(ctx context.Context, key types.NamespacedName, keep types.UID, now time.Time) error {
	var list v1alpha1.RecoveryIncidentList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(key.Namespace), client.MatchingLabels{PolicyLabel: key.Name}); err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		inc := &list.Items[i]
		if isTerminal(inc.Status.Runtime.Phase) || (keep != "" && inc.Spec.PolicyRef.UID == string(keep)) {
			continue
		}
		if inc.Status.Runtime.WorkloadDispatchActionID != "" || inc.Status.Runtime.SafetyHaltReason != "" || lastReserved(inc) != nil {
			_, err := r.haltAndSettle(ctx, nil, inc, ReasonPolicyDeleted)
			errs = append(errs, err)
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
	return r.getDeployment(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.TargetRef.Name})
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
		// A rollout left in VERIFYING is not reverted, but its attempt closes with the Incident.
		if n := len(rt.Attempts); n > 0 && rt.Attempts[n-1].Outcome == recovery.OutcomePatched && rt.Attempts[n-1].CompletedAt == nil {
			rt.Attempts[n-1].CompletedAt = &t
		}
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
