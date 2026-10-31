package controller

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type finiteIssuedRound struct {
	Permit     ExhibitionPermit       `json:"permit"`
	Envelope   SignedExhibitionPermit `json:"envelope"`
	Deployment appsv1.Deployment      `json:"deployment"`
}

// The Python helper uses temporary signing keys and one temporary durable issuer
// ledger. Completion, fence and workload observations are synthetic. This test
// exercises production signature/authority code, not Kubernetes or recovery.
func TestFiniteRoundsPythonIssuerController(t *testing.T) {
	finiteIssuedAuthorityContract(t, "tests.test_exhibition_finite_rounds", "export_controller_fixture")
}

// The actual operator CLI, child Coordinator, Lifecycle and issuer are used;
// Kubernetes and Runtime/Engineering effects remain synthetic transports.
func TestFiniteCLIChildLifecycleController(t *testing.T) {
	finiteIssuedAuthorityContract(t, "tests.test_exhibition_finite_integration", "export_cli_controller_fixture")
}

func finiteIssuedAuthorityContract(t *testing.T, module, exporter string) {
	t.Helper()
	spec := v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "engineering-demo"}}
	spec.Budget.MaxActionsPerIncident = 1
	code := fmt.Sprintf(`import json,sys,os
from pathlib import Path
from %s import %s as export_controller_fixture
os.chmod(sys.argv[1],0o700)
print(json.dumps(export_controller_fixture(Path(sys.argv[1]),sys.argv[2])))`, module, exporter)
	cmd := exec.Command("python3", "-B", "-c", code, t.TempDir(), v1alpha1.PolicyHash(spec))
	cmd.Dir = filepath.Join("..", "..")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual Python input/issuer: %v %s", err, raw)
	}
	var got struct {
		Public string              `json:"public"`
		Rounds []finiteIssuedRound `json:"rounds"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rounds) != 2 {
		t.Fatalf("expected two issued rounds, got %d", len(got.Rounds))
	}
	first, second := got.Rounds[0].Permit, got.Rounds[1].Permit
	if first.Grant.Instance != second.Grant.Instance || first.Grant.Scope.PolicyUID != second.Grant.Scope.PolicyUID || second.Ordinal <= first.Ordinal || second.BaseIncidentSequence != first.BaseIncidentSequence+1 || second.Grant.Scope.PolicyGeneration <= first.Grant.Scope.PolicyGeneration {
		t.Fatal("same-instance cumulative bindings absent")
	}
	if first.AuthorizationID == second.AuthorizationID || first.Grant.Scope.RunID == second.Grant.Scope.RunID || first.Grant.Scope.Epoch == second.Grant.Scope.Epoch || first.PreviousCompletionSHA256 == second.PreviousCompletionSHA256 {
		t.Fatal("rounds or predecessor reused")
	}
	s := first.Grant.Scope
	makeAuthority := func() (*ExhibitionAuthority, *time.Time) {
		a, e := NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: got.Public, Targets: []ExhibitionTarget{{s.ClusterUID, s.NamespaceUID, s.Namespace, s.Policy, s.PolicyUID, s.Deployment, s.DeploymentUID}}})
		if e != nil {
			t.Fatal(e)
		}
		a.instance = first.Grant.Instance
		now := first.Grant.NotBefore
		a.sample = func() (authoritySample, error) { return authoritySample{now, 100 * time.Second}, nil }
		return a, &now
	}
	policy := func(r finiteIssuedRound) *v1alpha1.RecoveryPolicy {
		scope := r.Permit.Grant.Scope
		p := &v1alpha1.RecoveryPolicy{ObjectMeta: metav1.ObjectMeta{Name: scope.Policy, Namespace: scope.Namespace, UID: types.UID(scope.PolicyUID), Generation: scope.PolicyGeneration}, Spec: spec}
		p.Status.IncidentSequence = r.Permit.BaseIncidentSequence
		envelope, _ := json.Marshal(r.Envelope)
		grant, _ := json.Marshal(ExecutionGrant{ExecutionGrantCore: r.Permit.Grant, HeartbeatAt: r.Permit.Grant.NotBefore, HeartbeatSequence: 1})
		p.Annotations = map[string]string{ExhibitionPermitAnnotation: string(envelope), ExecutionGrantAnnotation: string(grant)}
		return p
	}
	stopped := func(p *v1alpha1.RecoveryPolicy) *v1alpha1.RecoveryPolicy {
		p = p.DeepCopy()
		p.Spec.Enabled = false
		p.Generation -= 2
		p.Status.Conditions = []metav1.Condition{{Type: CondQuiesced, Status: metav1.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: p.Generation}}
		return p
	}
	t.Run("two-rounds-one-authority-and-prior-cannot-return", func(t *testing.T) {
		a, now := makeAuthority()
		for i, r := range got.Rounds {
			p := policy(r)
			*now = r.Permit.Grant.NotBefore
			if i == 1 && a.Authorize(p, nil) == nil {
				t.Fatal("second run accepted before previous fence")
			}
			a.ObserveStopped(stopped(p))
			if e := a.Authorize(p, nil); e != nil {
				t.Fatalf("round %d: %v", i+1, e)
			}
			if e := a.ValidateReplayWorkload(p, &r.Deployment); e != nil {
				t.Fatalf("round %d workload: %v", i+1, e)
			}
			if a.AllowsCircuit(p, r.Permit.CircuitEvidence.Signature, &r.Deployment) {
				t.Fatal("historical signature authorized action before current occurrence binding")
			}
			if a.highest != r.Permit.Ordinal {
				t.Fatal("ordinal not consumed")
			}
		}
		if a.Authorize(policy(got.Rounds[0]), nil) == nil {
			t.Fatal("previous round revived")
		}
	})
	for _, kind := range []string{"missing-fence", "changed-instance", "changed-policy-uid", "changed-generation", "expired", "signature", "wrong-image"} {
		t.Run(kind, func(t *testing.T) {
			a, now := makeAuthority()
			p := policy(got.Rounds[0])
			baseline := stopped(p)
			if kind != "missing-fence" {
				a.ObserveStopped(baseline)
			}
			switch kind {
			case "changed-instance":
				a.instance = strings.Repeat("e", 64)
			case "changed-policy-uid":
				p.UID = "other-policy"
			case "changed-generation":
				p.Generation++
			case "expired":
				*now = first.Grant.NotAfter
			case "signature":
				env := got.Rounds[0].Envelope
				env.Signature = strings.Repeat("A", 88)
				b, _ := json.Marshal(env)
				p.Annotations[ExhibitionPermitAnnotation] = string(b)
			}
			if kind == "wrong-image" {
				if e := a.Authorize(p, nil); e != nil {
					t.Fatal(e)
				}
				dep := got.Rounds[0].Deployment.DeepCopy()
				dep.Spec.Template.Spec.Containers[0].Image = "other/image@sha256:" + strings.Repeat("e", 64)
				if a.ValidateReplayWorkload(p, dep) == nil {
					t.Fatal("changed workload image accepted")
				}
			} else if a.Authorize(p, nil) == nil {
				t.Fatalf("%s accepted", kind)
			}
		})
	}
}
