package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Uses a real local envtest API server and the existing reconciler. ReplicaSet,
// Pod status, Prometheus and probe responses are fixture inputs, not live E2E.
func newReadinessEnforce(t *testing.T) (*enforceFixture, *corev1.Pod) {
	t.Helper()
	e := newEnforce(t, candidateIP)
	primary := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: e.ns + "-primary"}}
	e.setReady(primary, corev1.ConditionTrue)
	e.updateDeployment(func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
		}}
	})
	e.deploy = e.liveDeployment()
	rs := &appsv1.ReplicaSet{}
	e.must(e.c.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "demo-api-1"}, rs))
	rs.Spec.Template = *e.deploy.Spec.Template.DeepCopy()
	e.must(e.c.Update(e.ctx, rs))
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-1-a", Namespace: e.ns}}
	e.must(e.c.Delete(e.ctx, old, client.GracePeriodSeconds(0)))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "healthy-source", Namespace: e.ns,
		Labels: e.deploy.Spec.Template.Labels, OwnerReferences: controllerRef(rs, "ReplicaSet")},
		Spec: *e.deploy.Spec.Template.Spec.DeepCopy()}
	pod.Spec.NodeName = primary.Name
	e.must(e.c.Create(e.ctx, pod))
	pod.Status = e.readyStatus()
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://source-incarnation"
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.now)})
	e.must(e.c.Status().Update(e.ctx, pod))
	p := e.policy()
	p.Spec.Detection.PodUnreadyFor = &metav1.Duration{Duration: 30 * time.Second}
	p.Spec.Budget.MaxActionsPerIncident = 1
	e.must(e.c.Update(e.ctx, p))
	e.reconcile()
	incidents := &v1alpha1.RecoveryIncidentList{}
	e.must(e.c.List(e.ctx, incidents, client.InNamespace(e.ns)))
	if len(incidents.Items) != 0 {
		t.Fatal("healthy Pod created an incident")
	}
	return e, pod
}

func unreadySource(e *enforceFixture, pod *corev1.Pod) {
	e.t.Helper()
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(pod), pod))
	pod.Status.ContainerStatuses[0].Ready = false
	for i := range pod.Status.Conditions {
		pod.Status.Conditions[i].Status = corev1.ConditionFalse
		pod.Status.Conditions[i].LastTransitionTime = metav1.NewTime(e.now)
	}
	e.must(e.c.Status().Update(e.ctx, pod))
	e.now = e.now.Add(31 * time.Second)
}

func TestReadinessRuntimeExistingControllerRecoversAfterOwnRollout(t *testing.T) {
	e, source := newReadinessEnforce(t)
	unreadySource(e, source)
	e.reconcile()
	inc := e.onlyIncident()
	if inc.Spec.Trigger.Reason != classification.ReasonPodNotReady || inc.Spec.Trigger.PodUID != string(source.UID) {
		t.Fatalf("trigger=%+v", inc.Spec.Trigger)
	}
	e.wantOutcomes(inc, recovery.OutcomePatched)
	if e.liveDeployment().Spec.Template.Spec.NodeSelector[hostnameLabel] != e.cands[0].Labels[hostnameLabel] {
		t.Fatal("existing Controller did not relocate template")
	}
	pod := e.attemptPod(lastAttempt(inc), e.cands[0].Name, e.readyStatus())
	e.endpointSlice(pod)
	// A real Deployment rollout deletes the source; this cannot clear a trigger
	// that has already dispatched its own workload mutation.
	e.must(e.c.Delete(e.ctx, source, client.GracePeriodSeconds(0)))
	for range 6 {
		e.reconcile()
		inc = e.onlyIncident()
		if inc.Status.Runtime.Phase == PhaseRecovered {
			break
		}
		wantPhase(t, inc, PhaseVerifying, "")
		e.now = e.now.Add(8 * time.Second)
	}
	wantPhase(t, inc, PhaseRecovered, "")
	e.wantOutcomes(inc, recovery.OutcomeSucceeded)
	primary := &corev1.Node{}
	e.must(e.c.Get(e.ctx, types.NamespacedName{Name: source.Spec.NodeName}, primary))
	if !nodeReady(primary) || len(inc.Status.Runtime.OldPodsOutstanding) != 0 {
		t.Fatal("node changed or source remains outstanding")
	}
}

func TestReadinessRuntimeClearedImmediatelyBeforeDispatchDoesNotWriteWorkload(t *testing.T) {
	e, source := newReadinessEnforce(t)
	unreadySource(e, source)
	injected := false
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		err := c.SubResource(sub).Update(ctx, obj, opts...)
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && err == nil && inc.Status.Runtime.WorkloadDispatchActionID != "" && !injected {
			injected = true
			e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
			source.Status.ContainerStatuses[0].Ready = true
			for i := range source.Status.Conditions {
				source.Status.Conditions[i].Status = corev1.ConditionTrue
			}
			e.must(e.c.Status().Update(e.ctx, source))
		}
		return err
	}}, func() { e.reconcile() })
	if !injected {
		t.Fatal("dispatch boundary was not reached")
	}
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("cleared source changed workload template")
	}
	for _, a := range e.onlyIncident().Status.Runtime.Attempts {
		if a.Outcome == recovery.OutcomePatched || a.Outcome == recovery.OutcomeSucceeded {
			t.Fatal("unapplied attempt reported applied")
		}
	}
}

func TestReadinessRuntimeRepeatedExitRemainsManualWithoutWorkloadWrite(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		t.Run(map[bool]string{false: "samePod", true: "otherPod"}[sibling], func(t *testing.T) {
			e, source := newReadinessEnforce(t)
			exiting := source
			if sibling {
				exiting = source.DeepCopy()
				exiting.Name, exiting.UID, exiting.ResourceVersion, exiting.ManagedFields = "z-exiting", "", "", nil
				status := *source.Status.DeepCopy()
				e.must(e.c.Create(e.ctx, exiting))
				exiting.Status = status
				e.must(e.c.Status().Update(e.ctx, exiting))
			}
			// Seed previously observed exits in the existing history input. No
			// Scheduler, kubelet, or real fault is claimed by this API test.
			key := types.NamespacedName{Namespace: e.ns, Name: policyName}
			var obs []classification.RestartObservation
			for range 3 {
				obs = append(obs, classification.RestartObservation{PodUID: exiting.UID, Container: "demo-api", At: e.now, ExitCode: 1, Reason: "Error"})
			}
			e.r.restarts[key][string(exiting.UID)+"/demo-api"] = &restartState{obs: obs}
			unreadySource(e, source)
			e.reconcile()
			inc := e.onlyIncident()
			wantPhase(t, inc, PhaseManualRequired, ReasonClassNotAutomated)
			if inc.Spec.Trigger.Reason != classification.ReasonRepeatedExit || len(inc.Status.Runtime.Attempts) != 0 || e.workloadChanged(e.liveDeployment()) {
				t.Fatalf("repeated exits automated: %+v", inc)
			}
		})
	}
}

func TestReadinessRuntimeObservationErrorClearsHistory(t *testing.T) {
	e, source := newReadinessEnforce(t)
	unreadySource(e, source)
	e.withFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("observation unavailable")
	}}, func() {
		if _, _, err := e.r.monitor(e.ctx, e.policy(), e.liveDeployment(), e.now); err == nil {
			t.Fatal("observation failure hidden")
		}
	})
	result, _, err := e.r.monitor(e.ctx, e.policy(), e.liveDeployment(), e.now)
	e.must(err)
	if result.Detected || e.workloadChanged(e.liveDeployment()) {
		t.Fatal("read error reused healthy history")
	}
}

func TestReadinessRuntimeConditionPersistsWithoutMonitoringTransition(t *testing.T) {
	e, source := newReadinessEnforce(t)
	// Keep Monitoring Active, clear the readiness condition through a known gap,
	// then reobserve the healthy Pod without changing Policy generation.
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
	source.Status.Conditions[0].Status = corev1.ConditionUnknown
	e.must(e.c.Status().Update(e.ctx, source))
	e.reconcile()
	check := func(want metav1.ConditionStatus) {
		for _, c := range e.policy().Status.Conditions {
			if c.Type == "ReadinessObserved" {
				if c.Status != want {
					t.Fatalf("readiness condition=%s want=%s", c.Status, want)
				}
				return
			}
		}
		t.Fatal("readiness condition missing")
	}
	check(metav1.ConditionFalse)
	source.Status.Conditions[0].Status = corev1.ConditionTrue
	e.must(e.c.Status().Update(e.ctx, source))
	e.reconcile()
	check(metav1.ConditionTrue)
}
