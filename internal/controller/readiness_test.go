package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func readinessObjects() (*v1alpha1.RecoveryPolicy, *appsv1.Deployment, *corev1.Pod, *corev1.Node, time.Time) {
	now := time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)
	p := &v1alpha1.RecoveryPolicy{ObjectMeta: metav1.ObjectMeta{Name: "health", Namespace: "dedicated", UID: "policy", Generation: 1}}
	p.Spec.Detection.PodUnreadyFor = &metav1.Duration{Duration: 30 * time.Second}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: p.Namespace, UID: "workload"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "pinned@sha256:unchanged", ReadinessProbe: &corev1.Probe{}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: p.Namespace, UID: "pod"}, Spec: *d.Spec.Template.Spec.DeepCopy()}
	pod.Spec.NodeName = "primary"
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://one", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}}}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "primary", UID: "node"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	return p, d, pod, node, now
}

func failReadiness(p *corev1.Pod, now time.Time) {
	p.Status.ContainerStatuses[0].Ready = false
	for i := range p.Status.Conditions {
		p.Status.Conditions[i].Status = corev1.ConditionFalse
		p.Status.Conditions[i].LastTransitionTime = metav1.NewTime(now.Add(-40 * time.Second))
	}
}

func TestReadinessHistoryCannotCrossIdentityOrObservationGaps(t *testing.T) {
	for _, change := range []string{"none", "policyUID", "generation", "workloadUID", "template", "containerRestart", "unknownGap", "controllerRestart", "nodeGap", "observationGap", "clockReversed"} {
		t.Run(change, func(t *testing.T) {
			p, d, pod, node, now := readinessObjects()
			r := &PolicyReconciler{}
			nodes := map[string]*corev1.Node{node.Name: node}
			if !r.observeReadiness(p, d, []corev1.Pod{*pod}, nodes, now)[pod.UID] {
				t.Fatal("healthy observation not recorded")
			}
			failReadiness(pod, now)
			switch change {
			case "policyUID":
				p.UID = "replacement"
			case "generation":
				p.Generation++
			case "workloadUID":
				d.UID = "replacement"
			case "template":
				d.Spec.Template.Spec.Containers[0].Image = "different"
			case "containerRestart":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://two"
				pod.Status.ContainerStatuses[0].RestartCount++
			case "controllerRestart":
				r = &PolicyReconciler{}
			case "observationGap":
				now = now.Add(4*monitorPeriod + time.Second)
			case "clockReversed":
				now = now.Add(-time.Second)
			case "unknownGap":
				pod.Status.Conditions[0].Status = corev1.ConditionUnknown
				r.observeReadiness(p, d, []corev1.Pod{*pod}, nodes, now)
				failReadiness(pod, now)
			case "nodeGap":
				node.Status.Conditions[0].Status = corev1.ConditionUnknown
				r.observeReadiness(p, d, []corev1.Pod{*pod}, nodes, now)
				node.Status.Conditions[0].Status = corev1.ConditionTrue
			}
			got := r.observeReadiness(p, d, []corev1.Pod{*pod}, nodes, now)[pod.UID]
			if got != (change == "none") {
				t.Fatalf("history reused across %s: %t", change, got)
			}
		})
	}
}

// A fake client exercises only input boundaries; no claim of scheduler or API
// CAS coverage is made by this test. The existing envtest suite owns those checks.
func TestReadinessLiveTriggerBeforeAndAfterOwnRollout(t *testing.T) {
	for _, variant := range []string{"unhealthy", "healthy", "missing", "differentUID", "wrongOwner", "newContainer", "nodeNotReady"} {
		t.Run(variant, func(t *testing.T) {
			p, d, pod, node, now := readinessObjects()
			failReadiness(pod, now)
			controller := true
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "source-rs", Namespace: p.Namespace, UID: "rs", OwnerReferences: []metav1.OwnerReference{{Name: d.Name, UID: d.UID, Kind: "Deployment", APIVersion: "apps/v1", Controller: &controller}}}, Spec: appsv1.ReplicaSetSpec{Template: *d.Spec.Template.DeepCopy()}}
			pod.OwnerReferences = []metav1.OwnerReference{{Name: rs.Name, UID: rs.UID, Kind: "ReplicaSet", APIVersion: "apps/v1", Controller: &controller}}
			trigger := v1alpha1.Trigger{Class: classification.ClassApplication, Reason: classification.ReasonPodNotReady,
				PodName: pod.Name, PodUID: string(pod.UID), NodeName: node.Name, NodeUID: string(node.UID),
				Evidence: []string{fmt.Sprintf("POD_NOT_READY podUID=%s previouslyReadyObserved=true containerIdentitySHA256=%s", pod.UID, classification.ReadinessContainerHash(pod))}}
			inc := &v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace}, Spec: v1alpha1.RecoveryIncidentSpec{Policy: p.Spec,
				Workload: v1alpha1.WorkloadRef{Name: d.Name, UID: string(d.UID)}, Trigger: trigger, Baseline: v1alpha1.Baseline{TemplateHash: v1alpha1.TemplateHash(d.Spec.Template)}}}
			switch variant {
			case "healthy":
				for i := range pod.Status.Conditions {
					pod.Status.Conditions[i].Status = corev1.ConditionTrue
				}
				pod.Status.ContainerStatuses[0].Ready = true
			case "differentUID":
				pod.UID = types.UID("replacement")
			case "wrongOwner":
				rs.OwnerReferences[0].UID = "foreign"
			case "newContainer":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://new"
			case "nodeNotReady":
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			objects := []client.Object{node, rs}
			if variant != "missing" {
				objects = append(objects, pod)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &PolicyReconciler{APIReader: reader, Now: func() time.Time { return now }}
			before, err := r.readinessTrigger(context.Background(), inc, false)
			if variant == "unhealthy" {
				if err != nil || !before {
					t.Fatalf("valid live trigger rejected: %v", err)
				}
			} else if err == nil && before {
				t.Fatal("stale/foreign source admitted before mutation")
			}
			after, err := r.readinessTrigger(context.Background(), inc, true)
			wantAfter := variant == "unhealthy" || variant == "healthy" || variant == "missing" || variant == "newContainer"
			if wantAfter != (after && err == nil) {
				t.Fatalf("post-apply %s got=%t err=%v", variant, after, err)
			}
		})
	}
}
