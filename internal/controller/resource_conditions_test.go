package controller

import (
	"context"
	"encoding/json"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"strings"
	"testing"
	"time"
)

func authorityConditions(f *authorityFixture) *candidates.ResourceConditions {
	cpu := int32(900)
	return &candidates.ResourceConditions{Schema: "opsloop-resource-conditions/v1", RunID: f.g.Scope.RunID, Epoch: f.g.Scope.Epoch, PolicyUID: f.g.Scope.PolicyUID, DeploymentUID: f.g.Scope.DeploymentUID, NotBefore: f.g.NotBefore, ExpiresAt: f.g.NotAfter, Nodes: []candidates.NodeResourceCondition{{NodeName: "node", NodeUID: "node-uid", CPUUtilizationFloorPermille: &cpu}}}
}
func TestResourceConditionsRequireCurrentExecutionScope(t *testing.T) {
	f := newAuthorityFixture(t)
	c := authorityConditions(f)
	if err := BindResourceConditions(f.g.Scope, c); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"empty", "run", "epoch", "policy", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			s := f.g.Scope
			switch kind {
			case "empty":
				s = ExecutionScope{}
			case "run":
				s.RunID = "next-run"
			case "epoch":
				s.Epoch = "next-epoch"
			case "policy":
				s.PolicyUID = "next-policy"
			case "deployment":
				s.DeploymentUID = "next-deployment"
			}
			if BindResourceConditions(s, c) == nil {
				t.Fatal("foreign scope accepted")
			}
		})
	}
}
func TestResourceConditionsDoNotReplaceGrantAuthority(t *testing.T) {
	for _, kind := range []string{"no-authorizer", "wrong-epoch", "outside-window", "expired-grant"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorityFixture(t)
			r := &PolicyReconciler{Authority: f.a, ResourceConditions: authorityConditions(f)}
			switch kind {
			case "no-authorizer":
				r.Authority = nil
			case "wrong-epoch":
				r.ResourceConditions.Epoch = "next"
			case "outside-window":
				r.ResourceConditions.NotBefore = f.g.NotBefore.Add(-time.Second)
				r.ResourceConditions.ExpiresAt = f.g.NotAfter.Add(-time.Second)
			case "expired-grant":
				f.advance(601 * time.Second)
			}
			in := candidates.Input{Now: f.n.wall, Deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{UID: types.UID(f.g.Scope.DeploymentUID)}}}
			if r.bindResourceInput(f.p, f.incident(), &in) == nil {
				t.Fatal("invalid permission accepted")
			}
		})
	}
}

// This fixture exercises the actual API/CRD and dispatch integration while the
// separate authority tests above exercise the real process/grant authorizer.
func installResourceConditions(t *testing.T, e *enforceFixture) {
	p := e.policy()
	d := e.liveDeployment()
	scope := ExecutionScope{ClusterUID: "cluster", NamespaceUID: "namespace", Namespace: p.Namespace, Policy: p.Name, PolicyUID: string(p.UID), PolicyGeneration: p.Generation, PolicyHash: v1alpha1.PolicyHash(p.Spec), Deployment: d.Name, DeploymentUID: string(d.UID), Incident: "fixture", RunID: "run-conditions", Epoch: "epoch-conditions"}
	grant := ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: scope, Instance: "fixture", NotBefore: e.now, NotAfter: e.now.Add(60 * time.Second)}, HeartbeatSequence: 1, HeartbeatAt: e.now}
	raw, _ := json.Marshal(grant)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[ExecutionGrantAnnotation] = string(raw)
	e.must(e.c.Update(e.ctx, p))
	cpu := int32(950)
	e.r.ResourceConditions = &candidates.ResourceConditions{Schema: "opsloop-resource-conditions/v1", RunID: scope.RunID, Epoch: scope.Epoch, PolicyUID: scope.PolicyUID, DeploymentUID: scope.DeploymentUID, NotBefore: e.now, ExpiresAt: e.now.Add(10 * time.Second), Nodes: []candidates.NodeResourceCondition{{NodeName: e.cands[0].Name, NodeUID: string(e.cands[0].UID), CPUUtilizationFloorPermille: &cpu}}}
}
func TestResourceConditionsControllerSelectionAndPersistedEvidence(t *testing.T) {
	e := newEnforce(t, "10.0.0.31", "10.0.0.32")
	installResourceConditions(t, e)
	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseVerifying, "")
	if lastAttempt(inc).NodeName != e.cands[1].Name {
		t.Fatal("condition not used in real dispatch selection")
	}
	for _, c := range inc.Status.Runtime.Decision.Candidates {
		if c.NodeName == e.cands[0].Name {
			if c.ResourceCondition == nil || *c.Measurements.CPUUtilizationPermille != 200 || *c.ResourceCondition.AppliedMeasurements.CPUUtilizationPermille != 950 {
				t.Fatal("CRD lost original/applied evidence")
			}
			return
		}
	}
	t.Fatal("target candidate absent")
}
func TestResourceConditionsExpireAfterNodeReadBeforeDispatch(t *testing.T) {
	e := newEnforce(t, "10.0.0.41", "10.0.0.42")
	installResourceConditions(t, e)
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	advanced := false
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*corev1.Node); ok && key.Name == e.cands[1].Name {
			e.now = e.r.ResourceConditions.ExpiresAt
			advanced = true
		}
		return err
	}})
	e.reconcile()
	if !advanced {
		t.Fatal("test did not cross expiry at dispatch boundary")
	}
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("workload changed after conditions expired")
	}
	if e.onlyIncident().Status.Runtime.Phase == PhaseVerifying {
		t.Fatal("expired experiment dispatched")
	}
}

func TestResourceConditionsPrunedEvidenceBlocksInitialAndResumedDispatch(t *testing.T) {
	e := newEnforce(t, "10.0.0.51", "10.0.0.52")
	installResourceConditions(t, e)
	pruned := false
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.Phase == PhaseExecuting {
			// Model a prior installed CRD pruning the new field during persistence.
			if inc.Status.Runtime.Decision != nil {
				for i := range inc.Status.Runtime.Decision.Candidates {
					inc.Status.Runtime.Decision.Candidates[i].ResourceCondition = nil
				}
			}
			for i := range inc.Status.Runtime.Attempts {
				for j := range inc.Status.Runtime.Attempts[i].Decision.Candidates {
					inc.Status.Runtime.Attempts[i].Decision.Candidates[j].ResourceCondition = nil
				}
			}
			pruned = true
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}, func() {
		_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: policyName}})
		if err == nil || !strings.Contains(err.Error(), "evidence not persisted") {
			t.Fatalf("missing evidence accepted: %v", err)
		}
	})
	if !pruned || e.workloadChanged(e.liveDeployment()) {
		t.Fatal("initial dispatch ignored missing evidence")
	}
	// A subsequent reconcile must not bypass the persistence check via Resume.
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("resumed dispatch ignored missing evidence")
	}
}

func TestResourceConditionsLeaveOtherPolicyObservationUnchanged(t *testing.T) {
	f := newAuthorityFixture(t)
	r := &PolicyReconciler{ResourceConditions: authorityConditions(f)}
	r.ResourceConditions.ExpiresAt = f.n.wall.Add(-time.Second)
	other := f.p.DeepCopy()
	other.UID = "other-policy-uid"
	other.Spec.Mode = v1alpha1.PolicyModeObserveOnly
	input := candidates.Input{Now: f.n.wall}
	if err := r.bindResourceInput(other, nil, &input); err != nil {
		t.Fatalf("unrelated ordinary observation blocked: %v", err)
	}
	if input.ResourceConditions != nil || input.RunID != "" || input.Epoch != "" || input.PolicyUID != "" {
		t.Fatal("experiment conditions leaked into another Policy")
	}
	// The configured Policy must still reject missing authority, not silently
	// fall back to unmodified observations for an expired experiment.
	if r.bindResourceInput(f.p, f.incident(), &input) == nil {
		t.Fatal("configured experiment bypassed execution authority")
	}
}
