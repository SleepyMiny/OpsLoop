package controller

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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

func TestCircuitReplayHistoryIsNotAuthority(t *testing.T) {
	f := newExhibitionFixture(t)
	f.permit.CircuitReplay = nil
	// Legacy nonempty allowlist without distinct explicit proof is no longer authority.
	f.publish(t)
	if f.a.Authorize(f.p, nil) == nil {
		t.Fatal("implicit circuit exception accepted")
	}
	f = newExhibitionFixture(t)
	f.permit.CircuitReplay = nil
	f.permit.AllowedCircuitSignatures = []string{}
	f.publish(t)
	f.p.Status.Circuits = []v1alpha1.Circuit{{Signature: strings.Repeat("b", 64), Phase: PhaseFailed}}
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	if f.a.AllowsCircuit(f.p, strings.Repeat("b", 64), f.replayDeployment()) {
		t.Fatal("history produced exception")
	}
}
func TestCircuitReplayRejectsChangedBinding(t *testing.T) {
	for _, kind := range []string{"image", "deployment", "policy", "signature", "run", "instance", "expired", "stale-heartbeat", "stopped", "consumed", "revoked", "grant-revoked", "extra-container"} {
		t.Run(kind, func(t *testing.T) {
			f := newExhibitionFixture(t)
			d := f.replayDeployment()
			sig := strings.Repeat("b", 64)
			if !f.a.AllowsCircuit(f.p, sig, d) {
				t.Fatal("explicit permit rejected")
			}
			switch kind {
			case "image":
				d.Spec.Template.Spec.Containers[0].Image = "example/other@sha256:" + strings.Repeat("d", 64)
			case "deployment":
				d.UID = "another"
			case "policy":
				f.p.UID = "another"
			case "signature":
				sig = strings.Repeat("f", 64)
			case "run":
				f.grant.Scope.RunID = "another"
				f.publish(t)
			case "instance":
				f.grant.Instance = "another"
				f.publish(t)
			case "expired":
				f.n.wall = f.n.wall.Add(61 * time.Second)
				f.n.tick += 61 * time.Second
			case "stale-heartbeat":
				f.n.wall = f.n.wall.Add(11 * time.Second)
				f.n.tick += 11 * time.Second
			case "stopped":
				f.p.Generation++
				f.recordStop()
			case "consumed":
				f.incident(t)
				f.p.Status.ActiveIncident = nil
			case "grant-revoked":
				delete(f.p.Annotations, ExecutionGrantAnnotation)
			case "revoked":
				delete(f.p.Annotations, ExhibitionPermitAnnotation)
			case "extra-container":
				d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, d.Spec.Template.Spec.Containers[0])
			}
			if f.a.AllowsCircuit(f.p, sig, d) {
				t.Fatal("changed or ended exception accepted")
			}
			if kind == "revoked" || kind == "grant-revoked" {
				f.publish(t)
				if f.a.AllowsCircuit(f.p, sig, d) {
					t.Fatal("revoked authority revived")
				}
			}
		})
	}
}
func TestCircuitReplayLiveImageCheckPermanentlyClosesRun(t *testing.T) {
	f := newExhibitionFixture(t)
	d := f.replayDeployment()
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.Containers[0].Image = "mutated"
	if f.a.ValidateReplayWorkload(f.p, d) == nil {
		t.Fatal("mutation accepted")
	}
	if f.a.Authorize(f.p, nil) == nil {
		t.Fatal("changed image run still authorized")
	}
	if f.a.AllowsCircuit(f.p, strings.Repeat("b", 64), f.replayDeployment()) {
		t.Fatal("image restored revived exception")
	}
}

// Actual Python B2 input builder -> host issuer with throwaway key/ledger ->
// actual Go Controller validator. No operational credentials/API/model are used.
func TestCircuitReplayPythonIssuerToController(t *testing.T) {
	p := &v1alpha1.RecoveryPolicy{Spec: v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "engineering-demo"}}}
	p.Spec.Budget.MaxActionsPerIncident = 1
	code := `import json,sys,os
from pathlib import Path
from tests.test_exhibition_replay import ReplayFixture
os.chmod(sys.argv[1],0o700)
f=ReplayFixture(Path(sys.argv[1]));f.args['policy_hash']=sys.argv[2];f.intent['scope']['policyHash']=sys.argv[2];f.approve();p=f.payload();e=f.issue(p)
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

func TestCircuitReplayDoesNotAuthorizeNewTrigger(t *testing.T) {
	f := newExhibitionFixture(t)
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	if f.a.ValidateReplayTrigger(f.p, strings.Repeat("f", 64)) == nil {
		t.Fatal("unapproved new failure allowed")
	}
	inc := f.incident(t)
	inc.Spec.Trigger.Signature = strings.Repeat("f", 64)
	if f.a.Authorize(f.p, inc) == nil {
		t.Fatal("changed incident failure allowed")
	}
}

func TestCircuitReplayRealReconciler(t *testing.T) {
	for _, kind := range []string{"history-only", "explicit", "wrong-trigger", "no-candidate", "image-changed"} {
		t.Run(kind, func(t *testing.T) {
			var e *enforceFixture
			if kind == "no-candidate" {
				e = newEnforce(t)
			} else {
				e = newEnforce(t, candidateIP)
			}
			p := e.policy()
			p.Spec.Enabled = false
			p.Spec.Budget.MaxActionsPerIncident = 1
			e.must(e.c.Update(e.ctx, p))
			p = e.policy()
			result, _, err := e.r.monitor(e.ctx, p, e.liveDeployment(), e.now)
			e.must(err)
			if !result.Detected {
				t.Fatal("fixture did not detect failure")
			}
			p.Status.Circuits = []v1alpha1.Circuit{{Signature: result.Trigger.Signature, Incident: "preserved-history", Phase: PhaseFailed, OpenedAt: metav1.NewTime(e.now)}}
			e.must(e.c.Status().Update(e.ctx, p))
			f := newExhibitionFixture(t)
			target := ExhibitionTarget{"cluster", e.ns, e.ns, p.Name, string(p.UID), p.Spec.TargetRef.Name, string(e.liveDeployment().UID)}
			a, err := NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: base64.StdEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)), Targets: []ExhibitionTarget{target}})
			e.must(err)
			a.sample = func() (authoritySample, error) { return authoritySample{e.now, 100 * time.Second}, nil }
			e.r.Authority = a
			e.reconcile()
			p = e.policy()
			base := p.Status.IncidentSequence
			p.Spec.Enabled = true
			e.must(e.c.Update(e.ctx, p))
			p = e.policy()
			s := ExecutionScope{ClusterUID: target.ClusterUID, NamespaceUID: target.NamespaceUID, Namespace: p.Namespace, Policy: p.Name, PolicyUID: string(p.UID), PolicyGeneration: p.Generation, PolicyHash: v1alpha1.PolicyHash(p.Spec), Deployment: target.Deployment, DeploymentUID: target.DeploymentUID, Incident: fmt.Sprintf("%s-%s-%05d", p.Name, firstUID(string(p.UID)), base+1), RunID: "test-b2", Epoch: "test-epoch"}
			grant := ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: s, Instance: a.Instance(), NotBefore: e.now, NotAfter: e.now.Add(time.Minute)}, HeartbeatSequence: 1, HeartbeatAt: e.now}
			permit := f.permit
			permit.Grant = grant.ExecutionGrantCore
			permit.BaseIncidentSequence = base
			permit.CircuitReplay.WorkloadImage = e.liveDeployment().Spec.Template.Spec.Containers[0].Image
			permit.CircuitReplay.Signature = result.Trigger.Signature
			permit.AllowedCircuitSignatures = []string{result.Trigger.Signature}
			if kind == "history-only" {
				permit.CircuitReplay = nil
				permit.AllowedCircuitSignatures = []string{}
			}
			if kind == "wrong-trigger" {
				permit.CircuitReplay.Signature = strings.Repeat("f", 64)
				permit.AllowedCircuitSignatures = []string{strings.Repeat("f", 64)}
			}
			if kind == "image-changed" {
				permit.CircuitReplay.WorkloadImage = "other@sha256:" + strings.Repeat("d", 64)
			}
			raw, _ := json.Marshal(permit)
			env, _ := json.Marshal(SignedExhibitionPermit{base64.StdEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, raw))})
			wire, _ := json.Marshal(grant)
			p.Annotations = map[string]string{ExhibitionPermitAnnotation: string(env), ExecutionGrantAnnotation: string(wire)}
			e.must(e.c.Update(e.ctx, p))
			e.reconcile()
			p = e.policy()
			if kind == "history-only" || kind == "wrong-trigger" || kind == "image-changed" {
				if p.Status.IncidentSequence != base || p.Status.ActiveIncident != nil {
					t.Fatal("blocked exception created incident")
				}
				if kind == "history-only" {
					found := false
					for _, c := range p.Status.Conditions {
						if c.Reason == CondCircuitOpen {
							found = true
						}
					}
					if !found {
						t.Fatalf("expected CircuitOpen: %+v", p.Status.Conditions)
					}
				}
			} else {
				if p.Status.IncidentSequence != base+1 || p.Status.ActiveIncident == nil {
					t.Fatalf("explicit replay not reserved: %+v", p.Status)
				}
				if kind == "no-candidate" && len(e.onlyIncident().Status.Runtime.Attempts) != 0 {
					t.Fatal("exception bypassed candidate safety")
				}
			}
			if len(p.Status.Circuits) != 1 || p.Status.Circuits[0].Incident != "preserved-history" {
				t.Fatal("history reset")
			}
		})
	}
}
