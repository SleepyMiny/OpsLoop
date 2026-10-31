package controller

import (
	"encoding/json"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvidenceRolesPythonIssuerToController(t *testing.T) {
	p := &v1alpha1.RecoveryPolicy{Spec: v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "engineering-demo"}}}
	p.Spec.Budget.MaxActionsPerIncident = 1
	code := `import json,sys,os
from pathlib import Path
from tests.test_exhibition_evidence_roles import V2Fixture
os.chmod(sys.argv[1],0o700)
f=V2Fixture(Path(sys.argv[1]),sys.argv[2]);p=f.payload();e=f.issue_v2(p)
print(json.dumps({'permit':p,'envelope':e,'public':f.public,'deployment':f.objects['b-deployment']}))`
	cmd := exec.Command("python3", "-c", code, t.TempDir(), v1alpha1.PolicyHash(p.Spec))
	cmd.Dir = filepath.Join("..", "..")
	var raw []byte
	var err error
	if fixturePath := os.Getenv("OPSLOOP_LOCAL_REPLAY_FIXTURE"); fixturePath != "" {
		raw, err = os.ReadFile(fixturePath)
	} else {
		raw, err = cmd.CombinedOutput()
	}
	if err != nil {
		t.Fatalf("Python issuer failed: %v %s", err, raw)
	}
	var got struct {
		PolicySpec *v1alpha1.RecoveryPolicySpec `json:"policySpec"`
		Permit     ExhibitionPermit             `json:"permit"`
		Envelope   SignedExhibitionPermit       `json:"envelope"`
		Public     string                       `json:"public"`
		Deployment appsv1.Deployment            `json:"deployment"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PolicySpec != nil {
		p.Spec = *got.PolicySpec
	}
	s := got.Permit.Grant.Scope
	a, err := NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: got.Public, Targets: []ExhibitionTarget{{s.ClusterUID, s.NamespaceUID, s.Namespace, s.Policy, s.PolicyUID, s.Deployment, s.DeploymentUID}}})
	if err != nil {
		t.Fatal(err)
	}
	// Only this test installs the test instance; production uses its own boot identity.
	a.instance = got.Permit.Grant.Instance
	a.sample = func() (authoritySample, error) {
		return authoritySample{got.Permit.Grant.NotBefore, time.Second * 100}, nil
	}
	p.ObjectMeta = metav1.ObjectMeta{Name: s.Policy, Namespace: s.Namespace, UID: types.UID(s.PolicyUID), Generation: s.PolicyGeneration - 2}
	p.Status.IncidentSequence = got.Permit.BaseIncidentSequence
	p.Spec.Enabled = false
	p.Status.Conditions = []metav1.Condition{{Type: CondQuiesced, Status: metav1.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: p.Generation}}
	a.ObserveStopped(p)
	p.Generation = s.PolicyGeneration
	p.Spec.Enabled = true
	envelope, _ := json.Marshal(got.Envelope)
	grant, _ := json.Marshal(ExecutionGrant{ExecutionGrantCore: got.Permit.Grant, HeartbeatAt: got.Permit.Grant.NotBefore, HeartbeatSequence: 1})
	p.Annotations = map[string]string{ExhibitionPermitAnnotation: string(envelope), ExecutionGrantAnnotation: string(grant)}
	if err = a.Authorize(p, nil); err != nil {
		t.Fatal(err)
	}
	if !a.AllowsCircuit(p, got.Permit.CircuitReplay.Signature, &got.Deployment) {
		t.Fatal("real issued exact replay rejected")
	}
	if a.AllowsCircuit(p, strings.Repeat("c", 64), &got.Deployment) {
		t.Fatal("unknown signature accepted")
	}
	got.Deployment.Spec.Template.Spec.Containers[0].Image = "changed"
	if a.ValidateReplayWorkload(p, &got.Deployment) == nil {
		t.Fatal("changed workload accepted")
	}
}

func TestEvidenceRolesRejectSignedRoleChanges(t *testing.T) {
	for _, kind := range []string{"source-policy", "predecessor-deployment", "predecessor-completion", "retirement", "approval", "v1-upgrade", "missing-predecessor", "new-run-as-predecessor"} {
		t.Run(kind, func(t *testing.T) {
			f := newExhibitionFixture(t)
			p := &f.permit
			r := p.CircuitReplay
			p.Schema = "opsloop-exhibition-permit/v2"
			p.CircuitEvidence = &CircuitEvidence{WorkloadImage: r.WorkloadImage, Signature: r.Signature, PriorRunID: r.PriorRunID, PriorEpoch: r.PriorEpoch, PriorIncidentUID: r.PriorIncidentUID, IncidentSHA256: r.IncidentSHA256, BundleSHA256: r.BundleSHA256, CompletionSHA256: r.CompletionSHA256, PolicyUID: p.Grant.Scope.PolicyUID, DeploymentUID: p.Grant.Scope.DeploymentUID}
			p.PreviousCompletionSHA256 = strings.Repeat("f", 64)
			p.PredecessorCompletion = &PredecessorCompletion{RunID: "failed-previous-run", Epoch: "failed-previous-epoch", PolicyUID: p.Grant.Scope.PolicyUID, DeploymentUID: p.Grant.Scope.DeploymentUID, CompletionSHA256: p.PreviousCompletionSHA256, RetirementSHA256: strings.Repeat("e", 64)}
			p.NewExecutionAuthorization = &NewExecutionAuthorization{AuthorizationID: p.AuthorizationID, ApprovalSHA256: r.ApprovalSHA256, CircuitEvidenceSHA256: evidenceProjectionHash(p.CircuitEvidence), PredecessorCompletionSHA256: evidenceProjectionHash(p.PredecessorCompletion)}
			if err := validateEvidenceRoles(*p); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source-policy":
				p.CircuitEvidence.PolicyUID = "other"
			case "predecessor-deployment":
				p.PredecessorCompletion.DeploymentUID = "other"
			case "predecessor-completion":
				p.PredecessorCompletion.CompletionSHA256 = r.CompletionSHA256
			case "retirement":
				p.PredecessorCompletion.RetirementSHA256 = ""
			case "approval":
				p.NewExecutionAuthorization.ApprovalSHA256 = strings.Repeat("0", 64)
			case "v1-upgrade":
				p.Schema = "opsloop-exhibition-permit/v1"
			case "missing-predecessor":
				p.PredecessorCompletion = nil
			case "new-run-as-predecessor":
				p.PredecessorCompletion.RunID = p.Grant.Scope.RunID
			}
			f.publish(t)
			if err := f.a.Authorize(f.p, nil); err == nil {
				t.Fatal("changed signed role accepted")
			}
		})
	}
}
