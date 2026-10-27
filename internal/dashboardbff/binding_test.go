package dashboardbff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boundFixture(t *testing.T) (Input, controller.ExecutionGrant) {
	t.Helper()
	in := fixture()
	s := controller.ExecutionScope{ClusterUID: "cluster-uid", NamespaceUID: "namespace-uid", Namespace: in.Policy.Namespace, Policy: in.Policy.Name, PolicyUID: string(in.Policy.UID), PolicyGeneration: in.Policy.Generation, PolicyHash: v1alpha1.PolicyHash(in.Policy.Spec), Deployment: in.Deployment.Name, DeploymentUID: string(in.Deployment.UID), Incident: "incident", RunID: "runtime-run", Epoch: "runtime-epoch"}
	g := controller.ExecutionGrant{ExecutionGrantCore: controller.ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: s, Instance: "PRIVATE_INSTANCE_SENTINEL", NotBefore: fixtureTime, NotAfter: fixtureTime.Add(time.Minute)}, HeartbeatSequence: 1, HeartbeatAt: fixtureTime}
	raw, _ := json.Marshal(g)
	core, _ := json.Marshal(g.ExecutionGrantCore)
	sum := sha256.Sum256(core)
	in.Policy.Annotations[controller.ExecutionGrantAnnotation] = string(raw)
	in.Incidents = []v1alpha1.RecoveryIncident{{ObjectMeta: metav1.ObjectMeta{Name: s.Incident, Namespace: s.Namespace, UID: "incident-uid", Annotations: map[string]string{controller.ExecutionGrantBindingAnnotation: hex.EncodeToString(sum[:])}}, Spec: v1alpha1.RecoveryIncidentSpec{PolicyRef: v1alpha1.PolicyRef{Name: s.Policy, UID: s.PolicyUID}, Workload: v1alpha1.WorkloadRef{Name: s.Deployment, UID: s.DeploymentUID}, PolicyGeneration: s.PolicyGeneration, PolicyHash: s.PolicyHash}}}
	return in, g
}

func TestObservedRunBindingSurvivesStopWithoutClaimingAuthority(t *testing.T) {
	in, _ := boundFixture(t)
	// Disabled Policy and expired grant are expected after safe stop. Only the
	// historical identity is exposed, without claiming current execution authority.
	in.Policy.Spec.Enabled = false
	in.Policy.Generation++
	out, err := Assemble(in, metricFixture(), fixtureTime.Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	i := out["recentIncidents"].([]Object)[0]
	b := i["runtimeBinding"].(Object)
	if b["runID"] != "runtime-run" || b["epoch"] != "runtime-epoch" || b["policyUID"] != string(in.Policy.UID) {
		t.Fatal(b)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "PRIVATE_INSTANCE_SENTINEL") || strings.Contains(string(raw), "heartbeat") || strings.Contains(string(raw), "execution-grant") {
		t.Fatal("authority annotation leaked")
	}
}

func TestObservedRunBindingRejectsReplacementAndMalformedGrants(t *testing.T) {
	for _, name := range []string{"run", "epoch", "policy", "workload", "namespace", "incident", "hash", "generation", "missing", "unknown", "trailing"} {
		t.Run(name, func(t *testing.T) {
			in, g := boundFixture(t)
			switch name {
			case "run":
				g.Scope.RunID = "another-run"
			case "epoch":
				g.Scope.Epoch = "another-epoch"
			case "policy":
				g.Scope.PolicyUID = "another-policy"
			case "workload":
				g.Scope.DeploymentUID = "another-workload"
			case "namespace":
				in.Incidents[0].Namespace = "another-namespace"
			case "incident":
				g.Scope.Incident = "another-incident"
			case "hash":
				in.Incidents[0].Spec.PolicyHash = strings.Repeat("a", 64)
			case "generation":
				in.Incidents[0].Spec.PolicyGeneration++
			}
			raw, _ := json.Marshal(g)
			in.Policy.Annotations[controller.ExecutionGrantAnnotation] = string(raw)
			switch name {
			case "missing":
				delete(in.Policy.Annotations, controller.ExecutionGrantAnnotation)
			case "unknown":
				in.Policy.Annotations[controller.ExecutionGrantAnnotation] = strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`
			case "trailing":
				in.Policy.Annotations[controller.ExecutionGrantAnnotation] += `{}`
			}
			if observedBinding(in.Policy, in.Deployment, in.Incidents[0]) != nil {
				t.Fatal("mismatched run projected")
			}
		})
	}
}

func TestWorkloadPresentationDoesNotSelectOtherNamespaces(t *testing.T) {
	in := fixture()
	in.Deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "engineering-demo"}}
	in.Pods[0].Labels = map[string]string{"app": "engineering-demo"}
	other := *in.Pods[0].DeepCopy()
	other.Namespace = "another-namespace"
	other.UID = "another-pod"
	other.Labels["app.kubernetes.io/name"] = "demo-api"
	in.Pods = append(in.Pods, other)
	out, err := Assemble(in, metricFixture(), fixtureTime, false)
	if err != nil {
		t.Fatal(err)
	}
	pods := out["cluster"].(Object)["nodes"].([]Object)[0]["pods"].([]Object)
	if pods[0]["role"] != "demo-api" || pods[1]["role"] != "system" {
		t.Fatal(pods)
	}
	if pods[0]["uid"] == pods[1]["uid"] {
		t.Fatal("Pod identity collapsed")
	}
}
