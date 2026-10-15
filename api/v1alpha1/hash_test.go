package v1alpha1

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api"}},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "primary"},
			Containers:   []corev1.Container{{Name: "demo-api", Image: "demo-api:v1"}},
		},
	}
}

func testDeploymentSpec() appsv1.DeploymentSpec {
	replicas := int32(1)
	return appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo-api"}},
		Template: testTemplate(),
	}
}

func TestPolicyHash(t *testing.T) {
	a := RecoveryPolicySpec{TargetRef: TargetRef{Name: "demo-api"}, Mode: PolicyModeObserveOnly}
	b := a
	if PolicyHash(a) != PolicyHash(b) {
		t.Fatal("same spec produced different hashes")
	}
	if len(PolicyHash(a)) != 64 {
		t.Fatalf("not a sha256 hex: %q", PolicyHash(a))
	}
	b.Mode = PolicyModeEnforce
	if PolicyHash(a) == PolicyHash(b) {
		t.Fatal("mode change did not change hash")
	}
}

func TestTemplateHash(t *testing.T) {
	a, b := testTemplate(), testTemplate()
	if TemplateHash(a) != TemplateHash(b) {
		t.Fatal("same template produced different hashes")
	}
	b.Spec.NodeSelector["kubernetes.io/hostname"] = "azure"
	if TemplateHash(a) == TemplateHash(b) {
		t.Fatal("nodeSelector change did not change hash")
	}
}

func TestSpecHash(t *testing.T) {
	base := testDeploymentSpec()
	if SpecHash(base) != SpecHash(testDeploymentSpec()) {
		t.Fatal("same spec produced different hashes")
	}

	tmpl := testDeploymentSpec()
	tmpl.Template.Spec.Containers[0].Image = "demo-api:v2"
	if SpecHash(base) != SpecHash(tmpl) {
		t.Fatal("template change altered SpecHash")
	}
	if tmpl.Template.Spec.Containers[0].Image != "demo-api:v2" {
		t.Fatal("SpecHash mutated the caller's template")
	}

	scaled := testDeploymentSpec()
	zero := int32(0)
	scaled.Replicas = &zero
	if SpecHash(base) == SpecHash(scaled) {
		t.Fatal("replicas change did not change SpecHash")
	}

	paused := testDeploymentSpec()
	paused.Paused = true
	if SpecHash(base) == SpecHash(paused) {
		t.Fatal("paused change did not change SpecHash")
	}
}
