package controller

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type exhibitionFixture struct {
	a      *ExhibitionAuthority
	p      *v1alpha1.RecoveryPolicy
	permit ExhibitionPermit
	grant  ExecutionGrant
	key    ed25519.PrivateKey
	n      authoritySample
}

func newExhibitionFixture(t *testing.T) *exhibitionFixture {
	t.Helper()
	f := &exhibitionFixture{n: authoritySample{time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), 100 * time.Second}}
	seed := sha256.Sum256([]byte("PUBLIC TEST ONLY exhibition fixture key; never deployed"))
	f.key = ed25519.NewKeyFromSeed(seed[:])
	f.p = &v1alpha1.RecoveryPolicy{ObjectMeta: metav1.ObjectMeta{Name: "fixed-policy", Namespace: "demo", UID: "fixed-policy-uid", Generation: 4}, Spec: v1alpha1.RecoveryPolicySpec{Enabled: false, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "app"}, Budget: v1alpha1.BudgetSpec{}}}
	f.p.Spec.Budget.MaxActionsPerIncident = 1
	f.p.Status.IncidentSequence = 7
	config := ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: base64.StdEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)), Targets: []ExhibitionTarget{{"cluster", "namespace", "demo", "fixed-policy", "fixed-policy-uid", "app", "deployment-uid"}}}
	var err error
	f.a, err = NewExhibitionAuthority(config)
	if err != nil {
		t.Fatal(err)
	}
	f.a.sample = func() (authoritySample, error) { return f.n, nil }
	f.recordStop()
	f.newPermit(t, 1)
	return f
}
func (f *exhibitionFixture) recordStop() {
	f.p.Spec.Enabled = false
	f.p.Status.ActiveIncident = nil
	f.p.Status.Conditions = []metav1.Condition{{Type: CondQuiesced, Status: metav1.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: f.p.Generation}}
	f.a.ObserveStopped(f.p)
}
func (f *exhibitionFixture) newPermit(t *testing.T, ordinal uint64) {
	f.p.Generation++
	f.p.Spec.Enabled = true
	scope := ExecutionScope{ClusterUID: "cluster", NamespaceUID: "namespace", Namespace: f.p.Namespace, Policy: f.p.Name, PolicyUID: string(f.p.UID), PolicyGeneration: f.p.Generation, PolicyHash: v1alpha1.PolicyHash(f.p.Spec), Deployment: "app", DeploymentUID: "deployment-uid", Incident: fmt.Sprintf("%s-%s-%05d", f.p.Name, firstUID(string(f.p.UID)), f.p.Status.IncidentSequence+1), RunID: fmt.Sprintf("run-%d", ordinal), Epoch: fmt.Sprintf("epoch-%d", ordinal)}
	f.grant = ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: scope, Instance: f.a.Instance(), NotBefore: f.n.wall, NotAfter: f.n.wall.Add(time.Minute)}, HeartbeatSequence: 1, HeartbeatAt: f.n.wall}
	f.permit = ExhibitionPermit{Schema: "opsloop-exhibition-permit/v1", Grant: f.grant.ExecutionGrantCore, AuthorizationID: fmt.Sprintf("authority-%d", ordinal), Ordinal: ordinal, BaseIncidentSequence: f.p.Status.IncidentSequence, MaxIncidents: 1, MaxActions: 1, PreviousCompletionSHA256: strings.Repeat("a", 64), AllowedCircuitSignatures: []string{strings.Repeat("b", 64)}}
	f.permit.CircuitReplay = &CircuitReplay{Purpose: "ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION", WorkloadImage: "example/app@sha256:" + strings.Repeat("d", 64), Signature: strings.Repeat("b", 64), PriorRunID: "previous-run", PriorEpoch: "previous-epoch", PriorIncidentUID: "prior-incident", IncidentSHA256: strings.Repeat("c", 64), BundleSHA256: strings.Repeat("c", 64), CompletionSHA256: f.permit.PreviousCompletionSHA256, ApprovalSHA256: strings.Repeat("e", 64)}
	f.publish(t)
}
func (f *exhibitionFixture) publish(t *testing.T) {
	t.Helper()
	payload, err := json.Marshal(f.permit)
	if err != nil {
		t.Fatal(err)
	}
	f.publishRaw(t, payload)
}
func (f *exhibitionFixture) publishRaw(t *testing.T, payload []byte) {
	t.Helper()
	env, _ := json.Marshal(SignedExhibitionPermit{base64.StdEncoding.EncodeToString(payload), base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, payload))})
	grant, _ := json.Marshal(f.grant)
	f.p.Annotations = map[string]string{ExhibitionPermitAnnotation: string(env), ExecutionGrantAnnotation: string(grant)}
}
func (f *exhibitionFixture) incident(t *testing.T) *v1alpha1.RecoveryIncident {
	f.p.Status.IncidentSequence++
	s := f.grant.Scope
	inc := &v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Name: s.Incident, Namespace: s.Namespace, UID: "incident-uid", Annotations: map[string]string{ExecutionGrantBindingAnnotation: executionGrantBinding(f.p)}}, Spec: v1alpha1.RecoveryIncidentSpec{Sequence: f.p.Status.IncidentSequence, PolicyRef: v1alpha1.PolicyRef{Name: s.Policy, UID: s.PolicyUID}, PolicyGeneration: s.PolicyGeneration, PolicyHash: s.PolicyHash, Workload: v1alpha1.WorkloadRef{Name: s.Deployment, UID: s.DeploymentUID}}}
	f.p.Status.ActiveIncident = &v1alpha1.ActiveIncidentRef{Name: inc.Name, Sequence: inc.Spec.Sequence, IncidentUID: string(inc.UID)}
	if f.permit.CircuitReplay != nil {
		inc.Spec.Trigger.Signature = f.permit.CircuitReplay.Signature
	}
	if err := f.a.AnnotateIncident(f.p, inc); err != nil {
		t.Fatal(err)
	}
	return inc
}
func TestExhibitionFixedPolicyRepeatsWithoutResetOrControllerRestart(t *testing.T) {
	f := newExhibitionFixture(t)
	instance := f.a.Instance()
	uid := f.p.UID
	f.p.Status.Circuits = []v1alpha1.Circuit{{Signature: strings.Repeat("b", 64), Incident: "historical", Phase: PhaseFailed}}
	f.p.Status.RecentFailures = []v1alpha1.NodeFailure{{ActionID: "historical-action", NodeUID: "historical-node"}}
	for ordinal := uint64(1); ordinal <= 25; ordinal++ {
		if ordinal > 1 {
			f.newPermit(t, ordinal)
		}
		if err := f.a.Authorize(f.p, nil); err != nil {
			t.Fatal(err)
		}
		if !f.a.AllowsCircuit(f.p, strings.Repeat("b", 64), f.replayDeployment()) || f.a.AllowsCircuit(f.p, strings.Repeat("c", 64), f.replayDeployment()) {
			t.Fatal("circuit scope widened")
		}
		inc := f.incident(t)
		if err := f.a.Authorize(f.p, inc); err != nil {
			t.Fatal(err)
		}
		addCircuit(f.p, v1alpha1.Trigger{Signature: strings.Repeat("b", 64)}, inc.Name, PhaseFailed, f.n.wall)
		f.p.Status.ActiveIncident = nil
		if f.a.Authorize(f.p, nil) == nil {
			t.Fatal("same permit minted second incident")
		}
		f.p.Generation++
		f.recordStop()
	}
	if f.p.UID != uid || f.a.Instance() != instance || f.p.Status.IncidentSequence != 32 || len(f.p.Status.Circuits) != 1 || f.p.Status.Circuits[0].Incident != "historical" || len(f.p.Status.RecentFailures) != 1 {
		t.Fatal("lifetime history or fixed identities changed")
	}
}
func TestExhibitionFreshPermitCannotReplaceUnstoppedRun(t *testing.T) {
	f := newExhibitionFixture(t)
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	before := f.p.DeepCopy()
	permit, grant := f.permit, f.grant
	f.newPermit(t, 2)
	if f.a.Authorize(f.p, nil) == nil {
		t.Fatal("new permit replaced active run")
	}
	f.p = before
	f.permit = permit
	f.grant = grant
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal("invalid replacement damaged current authority", err)
	}
}
func TestExhibitionRejectsPastPermitAndOldInstance(t *testing.T) {
	f := newExhibitionFixture(t)
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	old := f.p.DeepCopy()
	f.p.Generation++
	f.recordStop()
	f.newPermit(t, 2)
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal(err)
	}
	if f.a.Authorize(old, nil) == nil {
		t.Fatal("old ordinal replay accepted")
	}
	fresh, err := NewExhibitionAuthority(f.a.config)
	if err != nil {
		t.Fatal(err)
	}
	fresh.sample = f.a.sample
	fresh.ObserveStopped(old)
	if fresh.Authorize(f.p, nil) == nil {
		t.Fatal("previous process permit accepted")
	}
	if err := f.a.Authorize(f.p, nil); err != nil {
		t.Fatal("old replay replaced current authority", err)
	}
}
func TestExhibitionStoppedStatusAndCompletionAreRequired(t *testing.T) {
	for _, kind := range []string{"no-stop", "stale-generation", "still-active", "enabled", "no-completion"} {
		t.Run(kind, func(t *testing.T) {
			f := newExhibitionFixture(t)
			if kind == "no-completion" {
				f.permit.PreviousCompletionSHA256 = ""
				f.publish(t)
			} else {
				f.a.stopped = map[string]stoppedExhibitionTarget{}
				p := f.p.DeepCopy()
				p.Generation--
				p.Spec.Enabled = false
				p.Status.Conditions[0].ObservedGeneration = p.Generation
				switch kind {
				case "no-stop":
					p.Status.Conditions = nil
				case "stale-generation":
					p.Status.Conditions[0].ObservedGeneration--
				case "still-active":
					p.Status.ActiveIncident = &v1alpha1.ActiveIncidentRef{Name: "old"}
				case "enabled":
					p.Spec.Enabled = true
				}
				f.a.ObserveStopped(p)
			}
			if f.a.Authorize(f.p, nil) == nil {
				t.Fatal("unconfirmed predecessor accepted")
			}
		})
	}
}
func TestExhibitionRejectsInvalidPermitScopeAndBudget(t *testing.T) {
	for _, kind := range []string{"signature", "policy", "instance", "incident", "base", "max-incidents", "max-actions", "model", "model-calls", "model-expiry", "missing", "unknown", "case-alias", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := newExhibitionFixture(t)
			switch kind {
			case "signature":
				f.key = ed25519.NewKeyFromSeed(make([]byte, 32))
			case "policy":
				f.permit.Grant.Scope.PolicyUID = "other"
			case "instance":
				f.permit.Grant.Instance = "old"
			case "incident":
				f.permit.Grant.Scope.Incident = "other"
			case "base":
				f.permit.BaseIncidentSequence--
			case "max-incidents":
				f.permit.MaxIncidents = 2
			case "max-actions":
				f.permit.MaxActions = 2
			case "model", "model-calls", "model-expiry":
				f.permit.ModelBudget = &ExhibitionModelBudget{"gpt-6-luna", "medium", "chatgpt", 1, 900, f.n.wall.Add(900 * time.Second)}
				if kind == "model" {
					f.permit.ModelBudget.Model = "other"
				}
				if kind == "model-calls" {
					f.permit.ModelBudget.MaxModelCalls = 2
				}
				if kind == "model-expiry" {
					f.permit.ModelBudget.ExpiresAt = f.n.wall.Add(901 * time.Second)
				}
			}
			f.publish(t)
			switch kind {
			case "missing":
				delete(f.p.Annotations, ExhibitionPermitAnnotation)
			case "unknown", "case-alias", "duplicate":
				raw, _ := json.Marshal(f.permit)
				suffix := `,"unknown":1}`
				if kind == "case-alias" {
					raw = []byte(strings.Replace(string(raw), `"ordinal"`, `"Ordinal"`, 1))
					f.publishRaw(t, raw)
				} else {
					if kind == "duplicate" {
						suffix = `,"ordinal":1}`
					}
					f.publishRaw(t, []byte(strings.TrimSuffix(string(raw), "}")+suffix))
				}
			}
			if f.a.Authorize(f.p, nil) == nil {
				t.Fatal("invalid signed scope/budget accepted")
			}
		})
	}
}
func TestExhibitionLateHeartbeatCannotReviveConsumedPermit(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			f := newExhibitionFixture(t)
			if !first {
				if err := f.a.Authorize(f.p, nil); err != nil {
					t.Fatal(err)
				}
			}
			f.n.wall = f.n.wall.Add(11 * time.Second)
			f.n.tick += 11 * time.Second
			if f.a.Authorize(f.p, nil) == nil {
				t.Fatal("late beat accepted")
			}
			f.grant.HeartbeatSequence++
			f.grant.HeartbeatAt = f.n.wall
			f.publish(t)
			if f.a.Authorize(f.p, nil) == nil {
				t.Fatal("late heartbeat revived consumed permit")
			}
		})
	}
}
func TestExhibitionResourceInputIsSignedAndRunScoped(t *testing.T) {
	f := newExhibitionFixture(t)
	cpu := int32(900)
	f.permit.ResourceConditions = &candidates.ResourceConditions{Schema: "opsloop-resource-conditions/v1", RunID: f.grant.Scope.RunID, Epoch: f.grant.Scope.Epoch, PolicyUID: string(f.p.UID), DeploymentUID: f.grant.Scope.DeploymentUID, NotBefore: f.n.wall, ExpiresAt: f.n.wall.Add(time.Minute), Nodes: []candidates.NodeResourceCondition{{NodeName: "node", NodeUID: "node-uid", CPUUtilizationFloorPermille: &cpu}}}
	f.publish(t)
	c, err := f.a.ResourceConditions(f.p, nil)
	if err != nil {
		t.Fatal(err)
	}
	*c.Nodes[0].CPUUtilizationFloorPermille = 1
	c, err = f.a.ResourceConditions(f.p, nil)
	if err != nil || *c.Nodes[0].CPUUtilizationFloorPermille != 900 {
		t.Fatal("caller changed signed conditions")
	}
	f.permit.ResourceConditions.RunID = "old-run"
	f.publish(t)
	if _, err = f.a.ResourceConditions(f.p, nil); err == nil {
		t.Fatal("other run inputs accepted")
	}
}

func TestExhibitionPolicyCountOneAndCumulativeIncidents(t *testing.T) {
	// Local API fixture only: no real cluster, workloads or fault injection.
	e := newEnforce(t, candidateIP)
	p := e.policy()
	p.Spec.Enabled = false
	p.Spec.Budget.MaxActionsPerIncident = 1
	e.must(e.c.Update(e.ctx, p))
	p = e.policy()
	p.Status.IncidentSequence = 9
	p.Status.Circuits = []v1alpha1.Circuit{{Signature: strings.Repeat("b", 64), Class: "RUNTIME", Incident: "historical-incident", Phase: PhaseFailed, OpenedAt: metav1.NewTime(e.now)}}
	e.must(e.c.Status().Update(e.ctx, p))
	seed := sha256.Sum256([]byte("PUBLIC TEST ONLY exhibition fixture key; never deployed"))
	key := ed25519.NewKeyFromSeed(seed[:])
	target := ExhibitionTarget{"fixture-cluster", e.ns, e.ns, p.Name, string(p.UID), p.Spec.TargetRef.Name, string(e.liveDeployment().UID)}
	authority, err := NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Targets: []ExhibitionTarget{target}})
	e.must(err)
	authority.sample = func() (authoritySample, error) { return authoritySample{e.now, time.Second * 100}, nil }
	e.r.Authority = authority
	e.reconcile() // Real local API fence close produces the trusted stop observation.
	policyUID := e.policy().UID
	for ordinal := uint64(1); ordinal <= 2; ordinal++ {
		p = e.policy()
		base := p.Status.IncidentSequence
		p.Spec.Enabled = true
		e.must(e.c.Update(e.ctx, p))
		p = e.policy()
		scope := ExecutionScope{ClusterUID: target.ClusterUID, NamespaceUID: target.NamespaceUID, Namespace: target.Namespace, Policy: target.Policy, PolicyUID: target.PolicyUID, PolicyGeneration: p.Generation, PolicyHash: v1alpha1.PolicyHash(p.Spec), Deployment: target.Deployment, DeploymentUID: target.DeploymentUID, Incident: fmt.Sprintf("%s-%s-%05d", p.Name, firstUID(string(p.UID)), base+1), RunID: fmt.Sprintf("api-run-%d", ordinal), Epoch: fmt.Sprintf("api-epoch-%d", ordinal)}
		grant := ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: scope, Instance: authority.Instance(), NotBefore: e.now, NotAfter: e.now.Add(time.Minute)}, HeartbeatSequence: 1, HeartbeatAt: e.now}
		permit := ExhibitionPermit{Schema: "opsloop-exhibition-permit/v1", Grant: grant.ExecutionGrantCore, AuthorizationID: fmt.Sprintf("api-approval-%d", ordinal), Ordinal: ordinal, BaseIncidentSequence: base, MaxIncidents: 1, MaxActions: 1, PreviousCompletionSHA256: strings.Repeat("a", 64), AllowedCircuitSignatures: []string{}}
		raw, _ := json.Marshal(permit)
		envelope, _ := json.Marshal(SignedExhibitionPermit{base64.StdEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))})
		wire, _ := json.Marshal(grant)
		p.Annotations = map[string]string{ExecutionGrantAnnotation: string(wire), ExhibitionPermitAnnotation: string(envelope)}
		e.must(e.c.Update(e.ctx, p))
		e.reconcile()
		p = e.policy()
		if p.Status.IncidentSequence != base+1 || p.Status.ActiveIncident == nil {
			t.Fatalf("run %d did not reserve next cumulative incident: %+v", ordinal, p.Status)
		}
		inc := &v1alpha1.RecoveryIncident{}
		e.must(e.c.Get(e.ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Status.ActiveIncident.Name}, inc))
		if inc.Annotations[ExhibitionRunAnnotation] != scope.RunID || inc.Spec.Sequence != base+1 {
			t.Fatal("incident binding lost")
		}
		p.Spec.Enabled = false
		e.must(e.c.Update(e.ctx, p))
		e.reconcile()
		p = e.policy()
		if p.Status.ActiveIncident != nil {
			t.Fatal("stop did not settle reservation")
		}
		// A new exhibition requires the original baseline after the previous
		// runtime action. Restore only the fixture Deployment spec, preserving
		// the durable execution fence and all Policy/Incident history.
		live := e.liveDeployment()
		live.Spec = *e.deploy.Spec.DeepCopy()
		e.must(e.c.Update(e.ctx, live))
	}
	var policies v1alpha1.RecoveryPolicyList
	e.must(e.c.List(e.ctx, &policies, client.InNamespace(e.ns)))
	if len(policies.Items) != 1 || policies.Items[0].UID != policyUID || policies.Items[0].Status.IncidentSequence != 11 {
		t.Fatal("repetition changed Policy count/identity or reset sequence")
	}
	if len(policies.Items[0].Status.Circuits) < 1 || policies.Items[0].Status.Circuits[0].Incident != "historical-incident" {
		t.Fatal("historical circuit erased")
	}
}

func TestExhibitionStaticConfigurationIsExactAndIndependent(t *testing.T) {
	f := newExhibitionFixture(t)
	config := f.a.config
	raw, _ := json.Marshal(config)
	if _, err := ParseExhibitionAuthorityConfig(raw); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing-key", "duplicate-target", "private-key", "case-alias", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			c := config
			c.Targets = append([]ExhibitionTarget(nil), config.Targets...)
			switch kind {
			case "missing-key":
				c.PublicKey = ""
			case "duplicate-target":
				c.Targets = append(c.Targets, c.Targets[0])
			case "private-key":
				c.PublicKey = base64.StdEncoding.EncodeToString(f.key)
			}
			b, _ := json.Marshal(c)
			if kind == "case-alias" {
				b = []byte(strings.Replace(string(b), `"publicKey"`, `"PublicKey"`, 1))
			}
			if kind == "unknown" {
				b = []byte(strings.TrimSuffix(string(b), "}") + `,"enableAll":true}`)
			}
			if _, err := ParseExhibitionAuthorityConfig(b); err == nil {
				t.Fatal("invalid startup boundary accepted")
			}
		})
	}
	a, err := NewExhibitionAuthority(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Targets[0].PolicyUID = "edited-by-caller"
	if a.config.Targets[0].PolicyUID == "edited-by-caller" {
		t.Fatal("caller mutated startup boundary")
	}
}

func (f *exhibitionFixture) replayDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: f.grant.Scope.Deployment, Namespace: f.p.Namespace, UID: types.UID(f.grant.Scope.DeploymentUID)}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example/app@sha256:" + strings.Repeat("d", 64)}}}}}}
}
