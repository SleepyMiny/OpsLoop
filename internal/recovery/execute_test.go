package recovery

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const uid = types.UID("dep-uid")

func baseDeployment() *appsv1.Deployment {
	d := &appsv1.Deployment{}
	d.UID = uid
	d.Spec.Replicas = int32p(1)
	d.Spec.Template.Labels = map[string]string{"app": "demo-api"}
	d.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "api", Image: "demo:v1"}}
	return d
}

// reserved returns a deployment and an attempt reserved against it, as the controller would compute them.
func reserved() (*appsv1.Deployment, v1alpha1.Attempt) {
	d := baseDeployment()
	target := d.Spec.Template.DeepCopy()
	Mutate(target, "node-b", "inc-a1")
	return d, v1alpha1.Attempt{
		ActionID:           "inc-a1",
		Outcome:            OutcomeReserved,
		Hostname:           "node-b",
		PreTemplateHash:    v1alpha1.TemplateHash(d.Spec.Template),
		TargetTemplateHash: v1alpha1.TemplateHash(*target),
	}
}

func TestResume(t *testing.T) {
	cases := []struct {
		name       string
		edit       func(d *appsv1.Deployment)
		wantAction ResumeAction
		wantReason string
	}{
		{"pre template", func(d *appsv1.Deployment) {}, Apply, ""},
		{"uid changed", func(d *appsv1.Deployment) { d.UID = "other" }, Superseded, "WORKLOAD_REPLACED"},
		{"replicas changed", func(d *appsv1.Deployment) { d.Spec.Replicas = int32p(2) }, Superseded, "SPEC_CHANGED_EXTERNALLY"},
		{"paused", func(d *appsv1.Deployment) { d.Spec.Paused = true }, Superseded, "SPEC_CHANGED_EXTERNALLY"},
		{"target applied", func(d *appsv1.Deployment) { Mutate(&d.Spec.Template, "node-b", "inc-a1") }, AlreadyApplied, ""},
		{"target plus image change", func(d *appsv1.Deployment) {
			Mutate(&d.Spec.Template, "node-b", "inc-a1")
			d.Spec.Template.Spec.Containers[0].Image = "demo:v2"
		}, Superseded, "TEMPLATE_CHANGED_EXTERNALLY"},
		{"other template", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "demo:v2" }, Superseded, "TEMPLATE_CHANGED_EXTERNALLY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, a := reserved()
			specHash := v1alpha1.SpecHash(d.Spec)
			tc.edit(d)
			action, reason := Resume(d, uid, specHash, a)
			if action != tc.wantAction || reason != tc.wantReason {
				t.Fatalf("Resume = (%v, %q), want (%v, %q)", action, reason, tc.wantAction, tc.wantReason)
			}
		})
	}
}

func TestReservedOutcome(t *testing.T) {
	d, a := reserved()
	if got := ReservedOutcome(nil, uid, a); got != OutcomeSuperseded {
		t.Errorf("nil deployment = %q, want Superseded", got)
	}
	other := baseDeployment()
	other.UID = "other"
	if got := ReservedOutcome(other, uid, a); got != OutcomeSuperseded {
		t.Errorf("uid mismatch = %q, want Superseded", got)
	}
	if got := ReservedOutcome(d, uid, a); got != OutcomeCancelled {
		t.Errorf("pre template = %q, want Cancelled", got)
	}
	applied := baseDeployment()
	Mutate(&applied.Spec.Template, "node-b", "inc-a1")
	if got := ReservedOutcome(applied, uid, a); got != OutcomePatched {
		t.Errorf("target template = %q, want Patched", got)
	}
	changed := baseDeployment()
	changed.Spec.Template.Spec.Containers[0].Image = "demo:v2"
	if got := ReservedOutcome(changed, uid, a); got != OutcomeSuperseded {
		t.Errorf("neither template = %q, want Superseded", got)
	}
}

func TestMutate(t *testing.T) {
	var empty corev1.PodTemplateSpec
	Mutate(&empty, "node-b", "inc-a1")
	if empty.Spec.NodeSelector[HostnameKey] != "node-b" || empty.Annotations[AttemptAnnotation] != "inc-a1" {
		t.Fatalf("nil maps not created: %+v", empty)
	}

	d := baseDeployment()
	before := v1alpha1.TemplateHash(d.Spec.Template)
	t1 := d.Spec.Template.DeepCopy()
	t2 := d.Spec.Template.DeepCopy()
	Mutate(t1, "node-b", "inc-a1")
	Mutate(t2, "node-b", "inc-a1")
	if t1.Spec.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Errorf("existing nodeSelector key lost: %v", t1.Spec.NodeSelector)
	}
	if v1alpha1.TemplateHash(*t1) != v1alpha1.TemplateHash(*t2) {
		t.Errorf("Mutate not deterministic")
	}
	if v1alpha1.TemplateHash(*t1) == before {
		t.Errorf("TemplateHash unchanged after Mutate")
	}
}

func int32p(v int32) *int32 { return &v }
