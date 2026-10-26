package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type readinessHistory struct {
	policy, workload types.UID
	generation       int64
	template         string
	observed         time.Time
	healthy          map[types.UID]string
}

// History is scoped to the exact Policy generation, workload and current
// template. Restart/missing history cannot manufacture a prior healthy sample.
func (r *PolicyReconciler) observeReadiness(p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment, pods []corev1.Pod, nodes map[string]*corev1.Node, now time.Time) map[types.UID]bool {
	key := client.ObjectKeyFromObject(p)
	if !classification.PodReadinessEnabled(p.Spec.Detection) {
		delete(r.readiness, key)
		return nil
	}
	if r.readiness == nil {
		r.readiness = map[types.NamespacedName]readinessHistory{}
	}
	h := r.readiness[key]
	template := v1alpha1.TemplateHash(d.Spec.Template)
	if h.policy != p.UID || h.workload != d.UID || h.generation != p.Generation || h.template != template || now.Before(h.observed) || now.Sub(h.observed) > 4*monitorPeriod {
		h = readinessHistory{policy: p.UID, workload: d.UID, generation: p.Generation, template: template, healthy: map[types.UID]string{}}
	}
	current := map[types.UID]bool{}
	identities := map[types.UID]string{}
	armed := false
	for i := range pods {
		pod := &pods[i]
		identity := classification.ReadinessContainerHash(pod)
		_, knownUnready := classification.PodUnreadySince(pod)
		if nodeReady(nodes[pod.Spec.NodeName]) && identity != "" {
			current[pod.UID] = classification.PodHealthy(pod) || (knownUnready && h.healthy[pod.UID] == identity)
		}
		if current[pod.UID] {
			identities[pod.UID] = identity
		}
		armed = armed || current[pod.UID]
	}
	h.healthy = identities
	h.observed = now
	r.readiness[key] = h
	status, reason := metav1.ConditionFalse, "AwaitingHealthyPod"
	if armed {
		status, reason = metav1.ConditionTrue, "HealthyPodObserved"
	}
	metav1Condition := metav1.Condition{Type: "ReadinessObserved", Status: status, Reason: reason,
		ObservedGeneration: p.Generation, LastTransitionTime: metav1.NewTime(now),
		Message: "Prior healthy sample for this Policy generation/template only; not execution authorization"}
	setReadinessCondition(p, metav1Condition)
	return current
}

func setReadinessCondition(p *v1alpha1.RecoveryPolicy, value metav1.Condition) {
	for i, old := range p.Status.Conditions {
		if old.Type == value.Type {
			if old.Status == value.Status {
				value.LastTransitionTime = old.LastTransitionTime
			}
			p.Status.Conditions[i] = value
			return
		}
	}
	p.Status.Conditions = append(p.Status.Conditions, value)
}

// Before dispatch the original Pod must still be the unhealthy source. After
// our workload CAS, rollout deletion/termination is expected and must not be
// mistaken for an infrastructure trigger clearing before execution.
func (r *PolicyReconciler) readinessTrigger(ctx context.Context, inc *v1alpha1.RecoveryIncident, applied bool) (bool, error) {
	t := inc.Spec.Trigger
	if !classification.IsPodReadinessTrigger(t) {
		return true, nil
	}
	if !classification.PodReadinessEnabled(inc.Spec.Policy.Detection) || t.PodName == "" || t.PodUID == "" || t.NodeName == "" || t.NodeUID == "" {
		return false, fmt.Errorf("invalid readiness trigger binding")
	}
	node := &corev1.Node{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: t.NodeName}, node); err != nil {
		return false, err
	}
	if string(node.UID) != t.NodeUID || !nodeReady(node) {
		return false, fmt.Errorf("readiness source Node changed or is not Ready")
	}
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: inc.Namespace, Name: t.PodName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return applied, nil
		}
		return false, err
	}
	if string(pod.UID) != t.PodUID || pod.Spec.NodeName != t.NodeName {
		return false, fmt.Errorf("readiness source Pod identity changed")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "ReplicaSet" || owner.UID == "" {
		return false, fmt.Errorf("readiness source has no ReplicaSet owner")
	}
	rs := &appsv1.ReplicaSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: inc.Namespace, Name: owner.Name}, rs); err != nil {
		return false, err
	}
	deployment := metav1.GetControllerOf(rs)
	template := rs.Spec.Template.DeepCopy()
	delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if rs.UID != owner.UID || deployment == nil || deployment.Kind != "Deployment" || string(deployment.UID) != inc.Spec.Workload.UID || deployment.Name != inc.Spec.Workload.Name || v1alpha1.TemplateHash(*template) != inc.Spec.Baseline.TemplateHash {
		return false, fmt.Errorf("readiness source owner/template binding changed")
	}
	if applied {
		return true, nil
	}
	expectedContainer := classification.ReadinessTriggerContainerHash(t)
	if expectedContainer == "" || classification.ReadinessContainerHash(pod) != expectedContainer {
		return false, fmt.Errorf("readiness source container incarnation changed or is unbound")
	}
	since, unhealthy := classification.PodUnreadySince(pod)
	return unhealthy && r.Now().Sub(since) >= inc.Spec.Policy.Detection.PodUnreadyFor.Duration, nil
}

func nodeReady(node *corev1.Node) bool {
	if node == nil || node.DeletionTimestamp != nil {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
