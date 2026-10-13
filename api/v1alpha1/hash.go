package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// PolicyHash is the sha256 hex of json.Marshal(spec).
func PolicyHash(spec RecoveryPolicySpec) string { return hashJSON(spec) }

// TemplateHash is the sha256 hex of json.Marshal(template).
func TemplateHash(template corev1.PodTemplateSpec) string { return hashJSON(template) }

// SpecHash is the sha256 hex of json.Marshal(spec) with Template zeroed, covering desired state outside the template.
func SpecHash(spec appsv1.DeploymentSpec) string {
	spec.Template = corev1.PodTemplateSpec{}
	return hashJSON(spec)
}

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// These API types contain no unmarshalable values, so this is a programming error.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
