// Package v1alpha1 contains the OpsLoop API types for the opsloop.io v1alpha1 group.
// +kubebuilder:object:generate=true
// +groupName=opsloop.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 object crd paths=./... output:crd:artifacts:config=../../config/crd/bases

var (
	GroupVersion  = schema.GroupVersion{Group: "opsloop.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
