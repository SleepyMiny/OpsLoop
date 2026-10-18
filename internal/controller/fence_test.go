package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func requireQuiesced(t *testing.T, e *enforceFixture, want bool) *executionFence {
	t.Helper()
	p := e.policy()
	c := meta.FindStatusCondition(p.Status.Conditions, CondQuiesced)
	if c == nil || (c.Status == metav1.ConditionTrue) != want {
		t.Fatalf("Quiesced=%+v, want true=%v", c, want)
	}
	if !want {
		return nil
	}
	if c.ObservedGeneration != p.Generation || p.Status.ActiveIncident != nil {
		t.Fatalf("premature or stale stop acknowledgement: %+v", p.Status)
	}
	d := e.liveDeployment()
	f, err := readFence(d)
	e.must(err)
	if !ownedFence(f, p, d) || f.ClosedThroughGeneration < p.Generation || c.Message != d.Annotations[FenceAnnotation] {
		t.Fatalf("invalid stop proof: %+v / %+v", c, f)
	}
	return f
}

func stopPolicy(e *enforceFixture, mode string) *v1alpha1.RecoveryPolicy {
	e.t.Helper()
	p := e.policy()
	if mode == "disable" {
		p.Spec.Enabled = false
	} else {
		p.Spec.Mode = v1alpha1.PolicyModeObserveOnly
	}
	e.must(e.c.Update(e.ctx, p))
	return p
}

func TestFenceAcknowledgedStopRejectsPausedAction(t *testing.T) {
	for _, mode := range []string{"disable", "observe"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			injected := false
			e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !injected {
					injected = true
					stopPolicy(e, mode)
					other := *e.r
					other.Client = e.c
					_, err := other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
					e.must(err)
					requireQuiesced(t, e, true)
				}
				return c.Update(ctx, obj, opts...)
			}}, func() {
				res := e.reconcile()
				if res.RequeueAfter != conflictRequeue {
					t.Fatalf("paused old action result=%+v", res)
				}
			})
			if !injected || e.workloadChanged(e.liveDeployment()) {
				t.Fatal("stop failed to fence paused workload update")
			}
			requireQuiesced(t, e, true)
			e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
		})
	}
}

func TestFenceClaimAndCloseCAS(t *testing.T) {
	for _, race := range []string{"claim_loses_to_stop", "close_loses_to_action"} {
		t.Run(race, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			if race == "claim_loses_to_stop" {
				injected := false
				e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok && d.Annotations[FenceAnnotation] != "" && !injected {
						injected = true
						stopPolicy(e, "disable")
						other := *e.r
						other.Client = e.c
						_, err := other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
						e.must(err)
						requireQuiesced(t, e, true)
					}
					return c.Update(ctx, obj, opts...)
				}}, func() {
					if e.reconcile().RequeueAfter != conflictRequeue {
						t.Fatal("claim did not conflict")
					}
				})
				if !injected || e.workloadChanged(e.liveDeployment()) {
					t.Fatal("claim race changed workload")
				}
			} else {
				e.crashBeforeUpdate()
				stopPolicy(e, "disable")
				injected := false
				e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok && !injected {
						injected = true
						old := e.liveDeployment()
						a := lastAttempt(e.onlyIncident())
						recovery.Mutate(&old.Spec.Template, a.Hostname, a.ActionID)
						e.must(e.c.Update(ctx, old))
						_ = d
					}
					return c.Update(ctx, obj, opts...)
				}}, func() {
					if e.reconcile().RequeueAfter != conflictRequeue {
						t.Fatal("close did not conflict")
					}
				})
				requireQuiesced(t, e, false)
				e.reconcile()
				requireQuiesced(t, e, true)
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
				if !e.workloadChanged(e.liveDeployment()) {
					t.Fatal("pre-close action was not settled")
				}
			}
		})
	}
}

func TestFenceLateRequestsRestartAndReactivation(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	old := e.liveDeployment()
	a := lastAttempt(e.onlyIncident())
	recovery.Mutate(&old.Spec.Template, a.Hostname, a.ActionID)
	stopped := stopPolicy(e, "disable")
	e.reconcile()
	f := requireQuiesced(t, e, true)
	clone := *e.r
	e.r = &clone
	e.reconcile()
	requireQuiesced(t, e, true)
	if err := e.c.Update(e.ctx, old); !apierrors.IsConflict(err) {
		t.Fatalf("late request accepted: %v", err)
	}
	p := e.policy()
	p.Spec.Enabled = true
	e.must(e.c.Update(e.ctx, p))
	e.reconcile()
	if !e.workloadChanged(e.liveDeployment()) {
		t.Fatal("new enabled generation cannot act")
	}
	got, err := readFence(e.liveDeployment())
	e.must(err)
	if got.ClosedThroughGeneration != f.ClosedThroughGeneration {
		t.Fatal("reactivation reset floor")
	}
	if _, _, err := e.r.closeStop(e.ctx, stopped); err == nil {
		t.Fatal("old stop authorized a close against newer policy")
	}
	if err := e.c.Update(e.ctx, old); !apierrors.IsConflict(err) {
		t.Fatalf("old request accepted after reactivation: %v", err)
	}
}

func TestFenceCloseLostResponseAndStatusConflict(t *testing.T) {
	for _, where := range []string{"close_response", "ack_status", "cleanup_status"} {
		t.Run(where, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			stopPolicy(e, "disable")
			injected := false
			e.withFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					err := c.Update(ctx, obj, opts...)
					if _, ok := obj.(*appsv1.Deployment); ok && where == "close_response" && !injected && err == nil {
						injected = true
						return errors.New("lost close response")
					}
					return err
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					match := false
					if p, ok := obj.(*v1alpha1.RecoveryPolicy); ok {
						q := meta.FindStatusCondition(p.Status.Conditions, CondQuiesced)
						match = where == "ack_status" && q != nil && q.Status == metav1.ConditionTrue
					}
					if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok {
						match = where == "cleanup_status" && isTerminal(inc.Status.Runtime.Phase)
					}
					if match && !injected {
						injected = true
						return apierrors.NewConflict(schema.GroupResource{Group: "opsloop.io", Resource: "status"}, obj.GetName(), errors.New("injected"))
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, func() {
				_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				if where == "close_response" && err == nil {
					t.Fatal("lost response hidden")
				}
			})
			if !injected {
				t.Fatal("fault not injected")
			}
			requireQuiesced(t, e, false)
			clone := *e.r
			e.r = &clone
			e.reconcile()
			requireQuiesced(t, e, true)
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("retry changed workload")
			}
		})
	}
}

func TestFenceMalformedIdentityAndPermissionsFailClosed(t *testing.T) {
	for _, kind := range []string{"malformed", "negative", "unknown_version", "policy_uid", "deployment_uid", "other_owner", "forbidden", "read_error"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			d := e.liveDeployment()
			f, err := readFence(d)
			e.must(err)
			switch kind {
			case "malformed":
				d.Annotations[FenceAnnotation] = "{"
			case "negative":
				f.ClosedThroughGeneration = -1
				putFence(d, f)
			case "unknown_version":
				f.Version = 3
				putFence(d, f)
			case "policy_uid":
				f.PolicyUID = "replaced-policy"
				putFence(d, f)
			case "deployment_uid":
				f.DeploymentUID = "replaced-deployment"
				putFence(d, f)
			case "other_owner":
				f.PolicyName = "other-policy"
				putFence(d, f)
			}
			e.must(e.c.Update(e.ctx, d))
			stopPolicy(e, "disable")
			run := func() {
				_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				if err == nil {
					t.Fatal("invalid stop acknowledged")
				}
			}
			if kind == "forbidden" {
				e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("denied"))
				}}, run)
			} else if kind == "read_error" {
				reader := &executionPolicyReader{Reader: e.c, fail: true}
				e.r.APIReader = reader
				run()
			} else {
				run()
			}
			requireQuiesced(t, e, false)
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("failed stop changed workload")
			}
		})
	}
}

func TestFenceObserveOnlyNeverWritesDeployment(t *testing.T) {
	e := newEnforce(t, candidateIP)
	p := e.policy()
	p.Spec.Mode = v1alpha1.PolicyModeObserveOnly
	e.must(e.c.Update(e.ctx, p))
	writes := 0
	e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*appsv1.Deployment); ok {
			writes++
			return errors.New("Deployment write forbidden")
		}
		return c.Update(ctx, obj, opts...)
	}}, func() {
		e.reconcile()
		requireQuiesced(t, e, false)
		e.reconcile()
	})
	if writes != 0 || e.liveDeployment().Annotations[FenceAnnotation] != "" {
		t.Fatal("ObserveOnly attempted a Deployment write")
	}
	requireQuiesced(t, e, false)
	if !strings.Contains(meta.FindStatusCondition(e.policy().Status.Conditions, CondQuiesced).Reason, "FenceNotEstablished") {
		t.Fatal("ObserveOnly fabricated proof")
	}
}

func TestFenceMetadataCASPreservesWorkload(t *testing.T) {
	e := newEnforce(t, candidateIP)
	before := e.liveDeployment()
	claimed := before.DeepCopy()
	_, err := e.r.claimExecutionFence(e.ctx, e.policy(), claimed)
	e.must(err)
	after := e.liveDeployment()
	if !equality.Semantic.DeepEqual(before.Spec, after.Spec) || after.ResourceVersion == before.ResourceVersion || after.Generation <= before.Generation {
		t.Fatalf("claim must change metadata only: oldRV=%s newRV=%s generations=%d/%d", before.ResourceVersion, after.ResourceVersion, before.Generation, after.Generation)
	}
	stopPolicy(e, "disable")
	e.reconcile()
	closed := e.liveDeployment()
	requireQuiesced(t, e, true)
	if !equality.Semantic.DeepEqual(before.Spec, closed.Spec) || closed.ResourceVersion == after.ResourceVersion || closed.Generation <= after.Generation {
		t.Fatal("close did not preserve exact workload spec")
	}
}

func TestFenceFloorRejectsOldPolicyOnFreshDeployment(t *testing.T) {
	e := newEnforce(t, candidateIP)
	oldPolicy := e.policy()
	stopPolicy(e, "disable")
	e.reconcile()
	requireQuiesced(t, e, true)
	fresh := e.liveDeployment()
	rv := fresh.ResourceVersion
	if _, err := e.r.claimExecutionFence(e.ctx, oldPolicy, fresh); err == nil {
		t.Fatal("fresh Deployment with old enabled Policy bypassed floor")
	}
	if e.liveDeployment().ResourceVersion != rv || e.workloadChanged(e.liveDeployment()) {
		t.Fatal("old generation modified fenced deployment")
	}
}

func TestFenceTwoOwnersClaimOneDeploymentCAS(t *testing.T) {
	e := newEnforce(t, candidateIP)
	p1 := e.policy()
	p2 := e.newPolicy("other-policy")
	p2.Spec.Mode = v1alpha1.PolicyModeEnforce
	e.must(e.c.Create(e.ctx, p2))
	d1 := e.liveDeployment()
	d2 := d1.DeepCopy()
	_, err := e.r.claimExecutionFence(e.ctx, p1, d1)
	e.must(err)
	if _, err := e.r.claimExecutionFence(e.ctx, p2, d2); !apierrors.IsConflict(err) {
		t.Fatalf("second absent-fence claimant must conflict: %v", err)
	}
	if _, err := e.r.claimExecutionFence(e.ctx, p2, e.liveDeployment()); err == nil {
		t.Fatal("second owner took an existing claim")
	}
	f, err := readFence(e.liveDeployment())
	e.must(err)
	if !ownedFence(f, p1, e.liveDeployment()) {
		t.Fatal("first owner lost fence")
	}
}

func TestFenceAcknowledgementReenableUsesRealStatusCAS(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	stopPolicy(e, "disable")
	injected := false
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if p, ok := obj.(*v1alpha1.RecoveryPolicy); ok && !injected {
			q := meta.FindStatusCondition(p.Status.Conditions, CondQuiesced)
			if q != nil && q.Status == metav1.ConditionTrue {
				injected = true
				live := e.policy()
				live.Spec.Enabled = true
				e.must(e.c.Update(ctx, live))
			}
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}, func() {
		if e.reconcile().RequeueAfter != conflictRequeue {
			t.Fatal("real Policy generation race did not cause status conflict")
		}
	})
	if !injected {
		t.Fatal("acknowledgement race not exercised")
	}
	requireQuiesced(t, e, false)
	e.reconcile()
	if !e.workloadChanged(e.liveDeployment()) {
		t.Fatal("new enabled generation could not execute")
	}
	q := meta.FindStatusCondition(e.policy().Status.Conditions, CondQuiesced)
	if q.Status == metav1.ConditionTrue {
		t.Fatal("enabled generation retained true acknowledgement")
	}
}

func TestFenceActualUIDReplacementFailsClosed(t *testing.T) {
	for _, kind := range []string{"policy", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			if kind == "policy" {
				p := e.policy()
				e.must(e.c.Delete(e.ctx, p))
				p = e.newPolicy(policyName)
				p.Spec.Mode = v1alpha1.PolicyModeEnforce
				p.Spec.Enabled = false
				e.must(e.c.Create(e.ctx, p))
			} else {
				d := e.liveDeployment()
				e.must(e.c.Delete(e.ctx, d))
				replacement := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, Annotations: d.Annotations}, Spec: d.Spec}
				e.must(e.c.Create(e.ctx, replacement))
				stopPolicy(e, "disable")
			}
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
			if err == nil {
				t.Fatal("identity replacement accepted prior fence")
			}
			requireQuiesced(t, e, false)
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("identity replacement changed workload")
			}
		})
	}
}

// Concurrent claimants share an absent-fence RV; the API accepts one owner.
func TestFenceConcurrentClaimCAS(t *testing.T) {
	t.Run("competing_owner_claims", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		d1 := e.liveDeployment()
		d2 := d1.DeepCopy()
		p1 := e.policy()
		p2 := p1.DeepCopy()
		p2.UID = "independent-policy-uid"
		p2.Name = "other-owner"
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, pair := range []struct {
			p *v1alpha1.RecoveryPolicy
			d *appsv1.Deployment
		}{{p1, d1}, {p2, d2}} {
			go func(p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment) {
				<-start
				_, err := e.r.claimExecutionFence(e.ctx, p, d)
				results <- err
			}(pair.p, pair.d)
		}
		close(start)
		ok, conflicts := 0, 0
		for range 2 {
			err := <-results
			if err == nil {
				ok++
			} else if apierrors.IsConflict(err) {
				conflicts++
			} else {
				t.Fatalf("unexpected claim error: %v", err)
			}
		}
		if ok != 1 || conflicts != 1 {
			t.Fatalf("success=%d conflict=%d", ok, conflicts)
		}
		if e.workloadChanged(e.liveDeployment()) {
			t.Fatal("claim changed workload")
		}
	})
}

func TestFenceObserveReservationInvalidatesAcknowledgement(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	stopPolicy(e, "observe")
	e.reconcile()
	requireQuiesced(t, e, true)
	// A later read-only evaluation has no eligible candidate and stays WAITING.
	for _, n := range e.cands {
		n.Spec.Unschedulable = true
		e.must(e.c.Update(e.ctx, n))
	}
	e.reconcile()
	p := e.policy()
	if p.Status.ActiveIncident == nil {
		t.Fatal("ObserveOnly waiting reservation was not exercised")
	}
	q := meta.FindStatusCondition(p.Status.Conditions, CondQuiesced)
	if q == nil || q.Status != metav1.ConditionFalse || q.Reason != "ReservationPending" {
		t.Fatalf("new active reservation retained completion: %+v", q)
	}
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("ObserveOnly changed workload")
	}
}
