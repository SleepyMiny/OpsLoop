package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Local API server only. Kubelet, scheduler, Prometheus and probe outputs are fixtures.
// Production classifier/authority/reconciler and workload Update are exercised.
func newReproductionEnforce(t *testing.T) (*enforceFixture, *corev1.Pod, *exhibitionFixture) {
	return newReproductionEnforceStages(t, false)
}
func newReproductionEnforceStages(t *testing.T, startup bool, steps ...func(*enforceFixture, *exhibitionFixture, string, func())) (*enforceFixture, *corev1.Pod, *exhibitionFixture) {
	return newReproductionEnforceDurationStages(t, startup, false, steps...)
}

func newReproductionEnforceDurationStages(t *testing.T, startup, rawDurations bool, steps ...func(*enforceFixture, *exhibitionFixture, string, func())) (*enforceFixture, *corev1.Pod, *exhibitionFixture) {
	e, old := newReadinessEnforce(t)
	e.updateDeployment(func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "DEMO_B_REPLAY", Value: ""})
	})
	baseline := *e.liveDeployment().Spec.Template.DeepCopy()
	p := e.policy()
	p.Spec.Enabled = false
	e.must(e.c.Update(e.ctx, p))
	p.Status.Conditions = []metav1.Condition{{Type: CondQuiesced, Status: metav1.ConditionTrue, Reason: "FenceClosed", LastTransitionTime: metav1.NewTime(e.now), ObservedGeneration: p.Generation}}
	e.must(e.c.Status().Update(e.ctx, p))
	f := newExhibitionFixture(t)
	f.p = p
	var err error
	f.a, err = NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: base64.StdEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)), Targets: []ExhibitionTarget{{"local-cluster", "local-namespace", e.ns, p.Name, string(p.UID), e.deploy.Name, string(e.deploy.UID)}}})
	e.must(err)
	start := e.now
	f.a.sample = func() (authoritySample, error) {
		return authoritySample{e.now, 100*time.Second + e.now.Sub(start)}, nil
	}
	f.a.ObserveStopped(p)
	p.Spec.Enabled = true
	e.must(e.c.Update(e.ctx, p))
	if rawDurations {
		// API server retains submitted duration spelling; typed Controller reads
		// and Incident creation serialize these same values differently.
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("RecoveryPolicy"))
		e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(p), u))
		e.must(unstructured.SetNestedField(u.Object, fmt.Sprintf("%dns", p.Spec.Budget.Cooldown.Duration.Nanoseconds()), "spec", "budget", "cooldown"))
		e.must(e.c.Update(e.ctx, u))
		p = e.policy()
		f.p = p
	}
	scope := ExecutionScope{ClusterUID: "local-cluster", NamespaceUID: "local-namespace", Namespace: e.ns, Policy: p.Name, PolicyUID: string(p.UID), PolicyGeneration: p.Generation, PolicyHash: v1alpha1.PolicyHash(p.Spec), Deployment: e.deploy.Name, DeploymentUID: string(e.deploy.UID), Incident: p.Name + "-" + firstUID(string(p.UID)) + "-00001", RunID: "local-new-p2", Epoch: "local-new-epoch"}
	f.grant = ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: scope, Instance: f.a.Instance(), NotBefore: e.now, NotAfter: e.now.Add(300 * time.Second)}, HeartbeatAt: e.now, HeartbeatSequence: 1}
	f.permit.Grant = f.grant.ExecutionGrantCore
	f.permit.BaseIncidentSequence = p.Status.IncidentSequence
	f.permit.CircuitReplay.WorkloadImage = baseline.Spec.Containers[0].Image
	digest := strings.Split(baseline.Spec.Containers[0].Image, "@")[1]
	historical := classification.Signature(classification.ClassApplication, classification.ReasonPodNotReady, string(e.deploy.UID), digest, string(old.UID), classification.ReadinessContainerHash(old))
	f.permit.CircuitReplay.Signature = historical
	f.permit.AllowedCircuitSignatures = []string{historical}
	promoteReproductionPermit(&f.permit, baseline)
	beforePermit := f.p.DeepCopy()
	f.publish(t)
	e.must(e.c.Patch(e.ctx, f.p, client.MergeFrom(beforePermit)))
	p = e.policy()
	p.Status.Circuits = []v1alpha1.Circuit{{Signature: historical, Incident: "past-p1", Phase: PhaseFailed, OpenedAt: metav1.NewTime(e.now.Add(-time.Hour))}}
	e.must(e.c.Status().Update(e.ctx, p))
	e.r.Authority = f.a
	step := func(stage string) {
		if len(steps) == 0 {
			e.reconcile()
			return
		}
		steps[0](e, f, stage, func() { e.reconcile() })
	}
	if startup {
		// Real host order: Enforce, observe healthy baseline, then the fault CAS.
		// The baseline must remain observable without falsely binding P1.
		var baselineRS appsv1.ReplicaSet
		e.must(e.c.Get(e.ctx, client.ObjectKey{Namespace: old.Namespace, Name: old.OwnerReferences[0].Name}, &baselineRS))
		baselineRS.Spec.Template = baseline
		e.must(e.c.Update(e.ctx, &baselineRS))
		e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(old), old))
		old.Spec.Containers = baseline.Spec.Containers
		// Pod env is immutable in an API server; the pre-existing baseline fixture
		// was created before adding the replay env. Recreate only this local fixture.
		e.must(e.c.Delete(e.ctx, old, client.GracePeriodSeconds(0)))
		old.ResourceVersion = ""
		old.UID = ""
		old.CreationTimestamp = metav1.Time{}
		old.ManagedFields = nil
		saved := old.Status
		old.Status = corev1.PodStatus{}
		e.must(e.c.Create(e.ctx, old))
		old.Status = saved
		if len(steps) != 0 {
			old.Status.ContainerStatuses[0].ImageID = baseline.Spec.Containers[0].Image
		}
		e.must(e.c.Status().Update(e.ctx, old))
		e.reconcile()
		healthy, source := false, false
		for _, c := range e.policy().Status.Conditions {
			if c.Type == "ReadinessObserved" && c.Status == metav1.ConditionTrue {
				healthy = true
			}
			if c.Type == "ReproductionSource" && c.Reason == "AwaitingApprovedSource" {
				source = true
			}
		}
		if !healthy || !source || f.a.current.occurrence != nil {
			t.Fatal("baseline gate blocked or falsely source-bound")
		}
		if len(steps) != 0 {
			step("baseline")
		}
	}
	e.updateDeployment(func(d *appsv1.Deployment) {
		for i := range d.Spec.Template.Spec.Containers[0].Env {
			if d.Spec.Template.Spec.Containers[0].Env[i].Name == "DEMO_B_REPLAY" {
				d.Spec.Template.Spec.Containers[0].Env[i].Value = "empty-order-after-30s"
			}
		}
	})
	e.deploy = e.liveDeployment()
	one := int32(1)
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "new-reproduction-rs", Namespace: e.ns, Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"}, OwnerReferences: controllerRef(e.deploy, "Deployment")}, Spec: appsv1.ReplicaSetSpec{Replicas: &one, Selector: e.deploy.Spec.Selector, Template: e.deploy.Spec.Template}}
	e.must(e.c.Create(e.ctx, rs))
	var deadline time.Time
	assertPending := func() {
		t.Helper()
		step("pending")
		var incidents v1alpha1.RecoveryIncidentList
		e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
		if len(incidents.Items) != 0 || e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
			t.Fatal("startup produced Incident/action")
		}
		if f.a.current.occurrence != nil || f.a.current.authority.closed != "" {
			t.Fatal("startup bound/retired authority")
		}
		if deadline.IsZero() {
			deadline = f.a.current.sourceWaitDeadline
		} else if deadline != f.a.current.sourceWaitDeadline {
			t.Fatal("startup deadline reset")
		}
		found := false
		for _, c := range e.policy().Status.Conditions {
			if c.Type == "ReproductionSource" && c.Reason == "SourcePending" {
				found = true
			}
		}
		if !found {
			t.Fatal("pending state not published")
		}
	}
	if startup {
		assertPending()
	} else {
		e.must(e.c.Delete(e.ctx, old, client.GracePeriodSeconds(0)))
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-reproduction-p2", Namespace: e.ns, Labels: e.deploy.Spec.Template.Labels, OwnerReferences: controllerRef(rs, "ReplicaSet")}, Spec: *e.deploy.Spec.Template.Spec.DeepCopy()}
	pod.Spec.NodeName = old.Spec.NodeName
	e.must(e.c.Create(e.ctx, pod))
	if startup {
		assertPending() // P1 baseline and P2 startup coexist; P1 cannot bind.
		e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(pod), pod))
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: pod.Spec.Containers[0].Name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		e.must(e.c.Status().Update(e.ctx, pod))
		assertPending()
		e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(pod), pod))
	}
	pod.Status = e.readyStatus()
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-p2-incarnation"
	pod.Status.ContainerStatuses[0].ImageID = e.deploy.Spec.Template.Spec.Containers[0].Image
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.now)})
	e.must(e.c.Status().Update(e.ctx, pod))
	if pod.UID == old.UID {
		t.Fatal("positive case reused historical Pod")
	}
	step("bound")
	if startup {
		e.must(e.c.Delete(e.ctx, old, client.GracePeriodSeconds(0)))
	}
	if f.a.current == nil || f.a.current.occurrence == nil || f.a.current.occurrence.SourcePodUID != string(pod.UID) {
		t.Fatalf("new healthy source was not bound: authorize=%v conditions=%+v current=%v", f.a.Authorize(e.policy(), nil), e.policy().Status.Conditions, f.a.current != nil)
	}
	return e, pod, f
}
func refreshReproductionHeartbeat(e *enforceFixture, f *exhibitionFixture) {
	p := e.policy()
	f.grant.HeartbeatSequence++
	f.grant.HeartbeatAt = e.now
	raw, _ := json.Marshal(f.grant)
	before := p.DeepCopy()
	p.Annotations[ExecutionGrantAnnotation] = string(raw)
	e.must(e.c.Patch(e.ctx, p, client.MergeFrom(before)))
}
func TestReproductionRuntimeNewSourceDispatchAndOwnReplacement(t *testing.T) {
	reproductionDispatchAndReplacement(t, false)
}
func TestReproductionStartupToFirstActionAndReplacement(t *testing.T) {
	reproductionDispatchAndReplacement(t, true)
}
func reproductionDispatchAndReplacement(t *testing.T, startup bool) {
	e, source, f := newReproductionEnforceStages(t, startup)
	unreadyReproduction(e, source, f)
	e.reconcile()
	inc := e.onlyIncident()
	e.wantOutcomes(inc, recovery.OutcomePatched)
	if inc.Spec.Trigger.Signature == f.permit.CircuitEvidence.Signature || inc.Spec.Trigger.PodUID != string(source.UID) || inc.Annotations[CurrentOccurrenceAnnotation] == "" {
		t.Fatal("occurrence reused historical identity")
	}
	pod := e.attemptPod(lastAttempt(inc), e.cands[0].Name, e.readyStatus())
	e.endpointSlice(pod)
	e.must(e.c.Delete(e.ctx, source, client.GracePeriodSeconds(0)))
	for range 6 {
		refreshReproductionHeartbeat(e, f)
		e.reconcile()
		inc = e.onlyIncident()
		if inc.Status.Runtime.Phase == PhaseRecovered {
			break
		}
		e.now = e.now.Add(8 * time.Second)
	}
	wantPhase(t, inc, PhaseRecovered, "")
	if f.a.current.occurrence.SourcePodUID != string(source.UID) {
		t.Fatal("replacement rebound initial occurrence")
	}
}
func TestReproductionRuntimeOwnerSwapAtDispatchBlocked(t *testing.T) {
	e, source, f := newReproductionEnforce(t)
	unreadyReproduction(e, source, f)
	injected := false
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		err := c.SubResource(sub).Update(ctx, obj, opts...)
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && err == nil && inc.Status.Runtime.WorkloadDispatchActionID != "" && !injected {
			injected = true
			one := int32(1)
			d := e.liveDeployment()
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "substituted-rs", Namespace: e.ns, OwnerReferences: controllerRef(d, "Deployment")}, Spec: appsv1.ReplicaSetSpec{Replicas: &one, Selector: d.Spec.Selector, Template: d.Spec.Template}}
			e.must(e.c.Create(ctx, rs))
			e.must(e.c.Get(ctx, client.ObjectKeyFromObject(source), source))
			source.OwnerReferences = controllerRef(rs, "ReplicaSet")
			e.must(e.c.Update(ctx, source))
		}
		return err
	}}, func() { e.reconcile() })
	if !injected {
		t.Fatal("dispatch boundary not reached")
	}
	if e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
		t.Fatal("changed owner received workload action")
	}
	for _, a := range e.onlyIncident().Status.Runtime.Attempts {
		if a.Outcome == recovery.OutcomePatched || a.Outcome == recovery.OutcomeSucceeded {
			t.Fatal("rejected source reported applied")
		}
	}
}

func TestReproductionRuntimeLateSourceIOChangesBlocked(t *testing.T) {
	for _, mode := range []string{"owner-swap", "permit-revoked"} {
		t.Run(mode, func(t *testing.T) {
			e, source, f := newReproductionEnforce(t)
			unreadyReproduction(e, source, f)
			armed, injected := false, false
			reads := 0
			wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
			e.must(err)
			e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && armed && key.Name == source.Name {
					reads++
					if reads == 2 && !injected {
						injected = true
						if mode == "permit-revoked" {
							p := e.policy()
							delete(p.Annotations, ExhibitionPermitAnnotation)
							e.must(e.c.Update(ctx, p))
						} else {
							one := int32(1)
							d := e.liveDeployment()
							rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "late-substituted-rs", Namespace: e.ns, OwnerReferences: controllerRef(d, "Deployment")}, Spec: appsv1.ReplicaSetSpec{Replicas: &one, Selector: d.Spec.Selector, Template: d.Spec.Template}}
							e.must(e.c.Create(ctx, rs))
							e.must(e.c.Get(ctx, client.ObjectKeyFromObject(source), source))
							source.OwnerReferences = controllerRef(rs, "ReplicaSet")
							e.must(e.c.Update(ctx, source))
						}
					}
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				err := c.SubResource(sub).Update(ctx, obj, opts...)
				if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && err == nil && inc.Status.Runtime.WorkloadDispatchActionID != "" {
					armed = true
				}
				return err
			}}, func() { e.reconcile() })
			if !injected {
				t.Fatalf("final source I/O boundary not reached: reads=%d", reads)
			}
			if e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
				t.Fatal("late change allowed workload mutation")
			}
		})
	}
}

// Deterministic normal heartbeat delivery while the readiness threshold elapses.
func unreadyReproduction(e *enforceFixture, source *corev1.Pod, f *exhibitionFixture) {
	start := e.now
	unreadySource(e, source)
	end := e.now
	e.now = start
	for e.now.Before(end) {
		next := e.now.Add(8 * time.Second)
		if next.After(end) {
			next = end
		}
		e.now = next
		refreshReproductionHeartbeat(e, f)
		e.must(f.a.Authorize(e.policy(), nil))
	}
}

// A live API read failure is not an empty successful observation, binding, or
// evidence sufficient to create an Incident. This uses the real monitor path.
func TestReproductionStartupReadUncertaintyBlocks(t *testing.T) {
	e, source, f := newReproductionEnforce(t)
	unreadyReproduction(e, source, f)
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.PodList); ok {
			return errors.New("synthetic observation unavailable")
		}
		return c.List(ctx, list, opts...)
	}})
	result, _, err := e.r.monitor(e.ctx, e.policy(), e.liveDeployment(), e.now)
	if err == nil || result.Detected {
		t.Fatal("uncertain observation became successful classification")
	}
	var incidents v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
	if len(incidents.Items) != 0 || e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
		t.Fatal("read uncertainty dispatched action")
	}
}

func TestReproductionRejectedConditionPersistsOnMonitorError(t *testing.T) {
	for _, kind := range []string{"runtime-identity-changed", "read-unavailable"} {
		t.Run(kind, func(t *testing.T) {
			e, source, _ := newReproductionEnforce(t)
			// Seed the last published startup condition to test stale UI state removal.
			// Authority/source identity remain genuine and are never reset by this test.
			p := e.policy()
			setReadinessCondition(p, metav1.Condition{Type: "ReproductionSource", Status: metav1.ConditionUnknown, Reason: "SourcePending", Message: "previous observation", ObservedGeneration: p.Generation, LastTransitionTime: metav1.NewTime(e.now)})
			e.must(e.c.Status().Update(e.ctx, p))
			if kind == "runtime-identity-changed" {
				e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
				source.Status.ContainerStatuses[0].ContainerID = "containerd://unexpected-runtime"
				e.must(e.c.Status().Update(e.ctx, source))
			} else {
				wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
				e.must(err)
				e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						return errors.New("synthetic observation unavailable")
					}
					return c.List(ctx, list, opts...)
				}})
			}
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
			if err == nil {
				t.Fatal("monitor failure lost")
			}
			check := func() {
				t.Helper()
				found := false
				for _, c := range e.policy().Status.Conditions {
					if c.Type == "ReproductionSource" {
						found = c.Reason == "SourceRejected" && c.Status == metav1.ConditionFalse
					}
				}
				if !found {
					t.Fatal("source rejection not persisted")
				}
				var incs v1alpha1.RecoveryIncidentList
				e.must(e.c.List(e.ctx, &incs, client.InNamespace(e.ns)))
				if len(incs.Items) != 0 || e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
					t.Fatal("rejected source dispatched action")
				}
			}
			check()
			if kind == "runtime-identity-changed" {
				e.reconcile()
				check()
			} // closed authority bypasses monitor
		})
	}
}

func TestReproductionRejectedConditionRecoversAfterStatusConflict(t *testing.T) {
	e, source, f := newReproductionEnforce(t)
	p := e.policy()
	setReadinessCondition(p, metav1.Condition{Type: "ReproductionSource", Status: metav1.ConditionUnknown, Reason: "SourcePending", Message: "previous observation", ObservedGeneration: p.Generation, LastTransitionTime: metav1.NewTime(e.now)})
	e.must(e.c.Status().Update(e.ctx, p))
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
	source.Status.ContainerStatuses[0].ContainerID = "containerd://unexpected-runtime"
	e.must(e.c.Status().Update(e.ctx, source))
	collided := false
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if q, ok := obj.(*v1alpha1.RecoveryPolicy); ok && sub == "status" && !collided {
			for _, condition := range q.Status.Conditions {
				if condition.Type == "ReproductionSource" && condition.Reason == "SourceRejected" {
					collided = true
					return apierrors.NewConflict(schema.GroupResource{Group: "opsloop.io", Resource: "recoverypolicies"}, q.Name, errors.New("deterministic status RV conflict"))
				}
			}
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}, func() { e.reconcile() })
	if !collided || f.a.current.authority.closed == "" {
		t.Fatal("conflict/authority closure not reached")
	}
	previous := false
	for _, c := range e.policy().Status.Conditions {
		if c.Type == "ReproductionSource" && c.Reason == "SourcePending" {
			previous = true
		}
	}
	if !previous {
		t.Fatal("test did not preserve stale condition after rejected CAS")
	}
	e.reconcile() // Authorize is closed; monitor cannot repair the condition.
	rejected := false
	for _, c := range e.policy().Status.Conditions {
		if c.Type == "ReproductionSource" && c.Reason == "SourceRejected" && c.Status == metav1.ConditionFalse {
			rejected = true
		}
	}
	if !rejected || f.a.Authorize(e.policy(), nil) == nil {
		t.Fatal("status reconciliation reopened authority or retained pending")
	}
	var incs v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &incs, client.InNamespace(e.ns)))
	if len(incs.Items) != 0 || e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
		t.Fatal("conflict dispatched action")
	}
}
