package main

import (
	"context"
	"errors"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type readinessPodBinding struct {
	Name          string `json:"name"`
	UID           string `json:"uid"`
	Node          string `json:"node"`
	ReplicaSetUID string `json:"replicaSetUID"`
	ActionID      string `json:"actionID"`
	TemplateHash  string `json:"templateHash"`
}

// A native Endpoint can precede the reconciler's recording of attempt.PodUID.
// Resolve its actual owner chain rather than manufacturing Controller status.
func (o *observer) resolveReadinessActionPod(ctx context.Context, ref *core.ObjectReference, a v1.Attempt) (core.Pod, error) {
	var pod core.Pod
	d := o.readiness.deployment
	if a.ActionID == "" || !validSHA256(a.TargetTemplateHash) || d == nil || d.Spec.Template.Annotations["opsloop.io/attempt-id"] != a.ActionID || v1.TemplateHash(d.Spec.Template) != a.TargetTemplateHash || d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != a.Hostname || !o.c.Readiness.allowedNode(a.NodeName, a.NodeUID, true) {
		return pod, errors.New("observer: Endpoint has no applied bound Controller action")
	}
	at, status, problem, err := o.get(ctx, "pods", ref.Name, &pod)
	if err != nil {
		return pod, err
	}
	if problem != "" || string(pod.UID) != string(ref.UID) || pod.Namespace != o.c.Namespace {
		return pod, errors.New("observer: action Endpoint Pod unavailable")
	}
	owner := meta.GetControllerOf(&pod)
	if owner == nil || !observerOwner(pod.ObjectMeta, "ReplicaSet", string(owner.UID)) || (pod.Spec.NodeName != "" && pod.Spec.NodeName != a.NodeName) || pod.Spec.NodeSelector["kubernetes.io/hostname"] != a.Hostname || pod.Spec.SchedulerName != "default-scheduler" {
		return pod, errors.New("observer: action Endpoint Pod placement/owner mismatch")
	}
	var rs apps.ReplicaSet
	rat, _, rp, err := o.get(ctx, "replicasets", owner.Name, &rs)
	if err != nil {
		return pod, err
	}
	if rp != "" || rs.UID != owner.UID || !observerOwner(rs.ObjectMeta, "Deployment", o.c.Readiness.DeploymentUID) {
		return pod, errors.New("observer: action Endpoint ReplicaSet ownership unavailable")
	}
	deploymentOwner := meta.GetControllerOf(&rs)
	if deploymentOwner == nil || deploymentOwner.Name != o.c.Workload {
		return pod, errors.New("observer: action Endpoint Deployment name mismatch")
	}
	template := rs.Spec.Template.DeepCopy()
	delete(template.Labels, apps.DefaultDeploymentUniqueLabelKey)
	if v1.TemplateHash(*template) != a.TargetTemplateHash || template.Annotations["opsloop.io/attempt-id"] != a.ActionID {
		return pod, errors.New("observer: action Endpoint ReplicaSet template mismatch")
	}
	if err = o.object("replicasets", rs.Name, rs.ObjectMeta, rat); err != nil {
		return pod, err
	}
	if o.readiness.actionPods == nil {
		o.readiness.actionPods = map[string]readinessPodBinding{}
	}
	binding := readinessPodBinding{pod.Name, string(pod.UID), pod.Spec.NodeName, string(rs.UID), a.ActionID, a.TargetTemplateHash}
	o.readiness.actionPods[string(pod.UID)] = binding
	_, err = o.r.raw("readiness-action-pod-observed-before-status", o.apiPath("pods", pod.Name), at, status, "", binding)
	return pod, err
}
