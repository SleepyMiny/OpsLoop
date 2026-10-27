package dashboardbff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/controller"
	appsv1 "k8s.io/api/apps/v1"
)

// observedBinding only identifies a recorded run. It never grants execution or
// claims that an expired grant, a heartbeat, or a Guard is currently healthy.
// Read the already-observed Policy; no additional API or RBAC is needed.
func observedBinding(p v1alpha1.RecoveryPolicy, d appsv1.Deployment, i v1alpha1.RecoveryIncident) Object {
	raw := p.Annotations[controller.ExecutionGrantAnnotation]
	if len(raw) == 0 || len(raw) > 8192 {
		return nil
	}
	var g controller.ExecutionGrant
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&g) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil
	}
	s := g.Scope
	if s.Validate() != nil || s.RunID == "" || g.Version != "opsloop-execution-grant/v1" || g.Instance == "" || g.NotBefore.IsZero() || !g.NotAfter.After(g.NotBefore) {
		return nil
	}
	if s.Namespace != p.Namespace || i.Namespace != p.Namespace || d.Namespace != p.Namespace || s.Policy != p.Name || s.PolicyUID != string(p.UID) || s.Deployment != d.Name || s.DeploymentUID != string(d.UID) || s.Incident != i.Name || i.UID == "" || i.Spec.PolicyRef.UID != s.PolicyUID || i.Spec.PolicyRef.Name != s.Policy || i.Spec.Workload.UID != s.DeploymentUID || i.Spec.Workload.Name != s.Deployment || i.Spec.PolicyGeneration != s.PolicyGeneration || i.Spec.PolicyHash != s.PolicyHash {
		return nil
	}
	core, err := json.Marshal(g.ExecutionGrantCore)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(core)
	hash := hex.EncodeToString(sum[:])
	if i.Annotations[controller.ExecutionGrantBindingAnnotation] != hash {
		return nil
	}
	return Object{"runID": s.RunID, "epoch": s.Epoch, "policyUID": s.PolicyUID, "workloadUID": s.DeploymentUID, "clusterUID": s.ClusterUID, "namespaceUID": s.NamespaceUID, "grantSha256": hash}
}
