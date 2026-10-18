package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// T02: the first raced write remains a fact; the post-write observation must
// establish a durable barrier before terminal cleanup.
func TestSafetyPostWriteRaceHoldsApplied(t *testing.T) {
	for _, change := range []string{"cordon", "ready", "trigger_ready"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			injected := false
			e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !injected {
					injected = true
					mutateExecutionNode(e, change)
				}
				return c.Update(ctx, obj, opts...)
			}}, func() { e.reconcile() })
			d := e.liveDeployment()
			if !injected || !e.workloadChanged(d) {
				t.Fatal("raced write not applied")
			}
			e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
			if e.onlyIncident().Status.Runtime.Phase != PhaseManualRequired || e.policy().Status.ActiveIncident != nil {
				t.Fatal("unsafe applied action was not settled manually")
			}
			if d.Annotations[FenceAnnotation] == "" {
				t.Fatal("hold absent")
			}
			p := e.policy()
			p.Spec.Budget.MaxActionsPerIncident++
			e.must(e.c.Update(e.ctx, p))
			rv := d.ResourceVersion
			e.reconcile()
			if e.liveDeployment().ResourceVersion != rv || e.policy().Status.ActiveIncident != nil {
				t.Fatal("generation edit bypassed safety hold")
			}
		})
	}
}

func TestSafetyCodecV2FailClosed(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"policyUID":"p","policyName":"p","deploymentUID":"d","closedThroughGeneration":1,"safetyHold":{"holdID":"h","incidentUID":"i","actionID":"a","policyGeneration":1,"reason":"unsafe"}}`,
	} {
		d := &appsv1.Deployment{}
		d.Annotations = map[string]string{FenceAnnotation: raw}
		if _, err := readFence(d); err != nil {
			t.Fatalf("v2 hold rejected: %v", err)
		}
	}
}

func requireSafetyHeld(t *testing.T, e *enforceFixture, applied bool) *executionFence {
	t.Helper()
	inc := e.onlyIncident()
	d := e.liveDeployment()
	f, err := readFence(d)
	e.must(err)
	if f == nil || f.Version != 2 || f.SafetyHold == nil || f.SafetyHold.IncidentUID != string(inc.UID) || f.SafetyHold.ActionID != lastAttempt(inc).ActionID || !ownedFence(f, e.policy(), d) || f.ClosedThroughGeneration < inc.Spec.PolicyGeneration {
		t.Fatalf("invalid hold: %+v", f)
	}
	wantPhase(t, inc, PhaseManualRequired, "SAFETY_HALTED")
	outcome := recovery.OutcomeCancelled
	if applied {
		outcome = recovery.OutcomePatched
	}
	e.wantOutcomes(inc, outcome)
	if e.policy().Status.ActiveIncident != nil {
		t.Fatal("confirmed settlement retained reservation")
	}
	return f
}

// T03/T08: independent reconcilers, a shared real API and Deployment CAS. No
// in-memory lock makes these orderings safe.
func TestSafetyHoldWorkloadCASOrderings(t *testing.T) {
	t.Run("hold_before_delayed_write", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		injected := false
		e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !injected {
				injected = true
				mutateExecutionNode(e, "cordon")
				other := *e.r
				other.Client = e.c
				_, err := other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				e.must(err)
				requireSafetyHeld(t, e, false)
			}
			return c.Update(ctx, obj, opts...)
		}}, func() {
			if e.reconcile().RequeueAfter != conflictRequeue {
				t.Fatal("delayed writer did not conflict")
			}
		})
		requireSafetyHeld(t, e, false)
	})
	t.Run("write_before_hold_cas", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.crashBeforeUpdate()
		delayed := e.liveDeployment()
		a := lastAttempt(e.onlyIncident())
		recovery.Mutate(&delayed.Spec.Template, a.Hostname, a.ActionID)
		mutateExecutionNode(e, "cordon")
		injected := false
		e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if d, ok := obj.(*appsv1.Deployment); ok && !injected {
				f, err := readFence(d)
				e.must(err)
				if f != nil && f.SafetyHold != nil {
					injected = true
					e.must(e.c.Update(ctx, delayed))
				}
			}
			return c.Update(ctx, obj, opts...)
		}}, func() {
			if e.reconcile().RequeueAfter != conflictRequeue {
				t.Fatal("hold must conflict with earlier write")
			}
		})
		if e.policy().Status.ActiveIncident == nil || isTerminal(e.onlyIncident().Status.Runtime.Phase) {
			t.Fatal("unconfirmed barrier released")
		}
		e.reconcile()
		requireSafetyHeld(t, e, true)
		if err := e.c.Update(e.ctx, delayed); !apierrors.IsConflict(err) {
			t.Fatalf("stale workload accepted after hold: %v", err)
		}
	})
}

// T06/T07: both applied and unapplied lost workload responses settle only after
// the hold, and a failed hold remains pending across a controller restart.
func TestSafetyWorkloadResponseUncertain(t *testing.T) {
	for _, applied := range []bool{false, true} {
		for _, holdUnavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("applied=%v/holdUnavailable=%v", applied, holdUnavailable), func(t *testing.T) {
				e := newEnforce(t, candidateIP)
				injected := false
				e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok {
						f, err := readFence(d)
						e.must(err)
						if f != nil && f.SafetyHold != nil && holdUnavailable {
							return errors.New("hold API unavailable")
						}
						if d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !injected {
							injected = true
							if applied {
								e.must(c.Update(ctx, obj, opts...))
							}
							return context.DeadlineExceeded
						}
					}
					return c.Update(ctx, obj, opts...)
				}}, func() {
					_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
					if (err != nil) != holdUnavailable {
						t.Fatalf("unexpected pending result: %v", err)
					}
				})
				if holdUnavailable {
					inc := e.onlyIncident()
					if inc.Status.Runtime.SafetyHaltReason == "" || isTerminal(inc.Status.Runtime.Phase) || e.policy().Status.ActiveIncident == nil {
						t.Fatal("unconfirmed hold did not retain pending reservation")
					}
					clone := *e.r
					e.r = &clone
					e.reconcile()
				}
				f := requireSafetyHeld(t, e, applied)
				if f.SafetyHold.Reason != "WORKLOAD_WRITE_UNCERTAIN" {
					t.Fatal("uncertainty cause lost")
				}
			})
		}
	}
}

func TestSafetyPostWriteUnknownAndHoldRetry(t *testing.T) {
	e := newEnforce(t, candidateIP)
	written := false
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Node); ok && written {
			return context.DeadlineExceeded
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if d, ok := obj.(*appsv1.Deployment); ok {
			f, err := readFence(d)
			e.must(err)
			if f != nil && f.SafetyHold != nil {
				return errors.New("hold unavailable")
			}
			err = c.Update(ctx, obj, opts...)
			if err == nil && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
				written = true
			}
			return err
		}
		return c.Update(ctx, obj, opts...)
	}}, func() {
		_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
		if err == nil {
			t.Fatal("unavailable hold hidden")
		}
	})
	if !written || isTerminal(e.onlyIncident().Status.Runtime.Phase) || e.policy().Status.ActiveIncident == nil {
		t.Fatal("unknown read completed before hold")
	}
	e.r.APIReader = e.c
	e.reconcile()
	requireSafetyHeld(t, e, true)
}

// T09: lost CAS response, rejected CAS and status conflict all retain active
// ownership until a subsequent live confirmation and settlement.
func TestSafetyHoldRetryFaults(t *testing.T) {
	for _, fault := range []string{"lost_hold_response", "hold_conflict", "pending_status_conflict", "terminal_status_conflict", "settlement_read_error"} {
		t.Run(fault, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			mutateExecutionNode(e, "cordon")
			injected, stored := false, false
			wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
			e.must(err)
			e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok && stored && fault == "settlement_read_error" && !injected {
					injected = true
					return context.DeadlineExceeded
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			e.withFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok {
						f, err := readFence(d)
						e.must(err)
						if f != nil && f.SafetyHold != nil {
							if fault == "hold_conflict" && !injected {
								injected = true
								return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, d.Name, errors.New("injected"))
							}
							err := c.Update(ctx, obj, opts...)
							stored = err == nil
							if fault == "lost_hold_response" && !injected && err == nil {
								injected = true
								return context.DeadlineExceeded
							}
							return err
						}
					}
					return c.Update(ctx, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && !injected && ((fault == "pending_status_conflict" && inc.Status.Runtime.SafetyHaltReason != "") || (fault == "terminal_status_conflict" && isTerminal(inc.Status.Runtime.Phase))) {
						injected = true
						return apierrors.NewConflict(schema.GroupResource{Group: "opsloop.io", Resource: "recoveryincidents"}, inc.Name, errors.New("injected"))
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, func() {
				_, _ = e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
			})
			if !injected || e.policy().Status.ActiveIncident == nil || isTerminal(e.onlyIncident().Status.Runtime.Phase) {
				t.Fatal("fault did not preserve pending incident")
			}
			e.r.APIReader = e.c
			clone := *e.r
			e.r = &clone
			e.reconcile()
			requireSafetyHeld(t, e, false)
		})
	}
}

// T04/T05/T07: structural post-apply guard on restart, using the exact persisted
// candidate/trigger identities. Resource-only changes are intentionally absent.
func TestSafetyAlreadyAppliedStructuralGuard(t *testing.T) {
	for _, change := range []string{"uid", "missing", "hostname", "selector", "arch", "os", "taint", "memory", "disk", "pid", "ready", "ready_unknown", "cordon", "trigger_uid", "trigger_missing", "trigger_ready"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashAfterUpdate()
			legacy := e.onlyIncident()
			legacy.Status.Runtime.WorkloadDispatchActionID = ""
			e.must(e.c.Status().Update(e.ctx, legacy)) // pre-marker persisted intent compatibility
			switch change {
			case "missing":
				e.must(e.c.Delete(e.ctx, e.cands[0]))
			case "os":
				n := &corev1.Node{}
				e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(e.cands[0]), n))
				n.Labels[corev1.LabelOSStable] = "unsupported"
				e.must(e.c.Update(e.ctx, n))
			case "ready_unknown":
				e.setReady(e.cands[0], corev1.ConditionUnknown)
			default:
				mutateExecutionNode(e, change)
			}
			clone := *e.r
			e.r = &clone
			e.reconcile()
			requireSafetyHeld(t, e, true)
		})
	}
}

// T12/T13: no capacity double count, and the final guard follows HTTP and
// Endpoint I/O, immediately before verification may publish success.
func TestSafetyVerifyingGuardAfterProbe(t *testing.T) {
	for _, boundary := range []string{"probe", "endpoint"} {
		t.Run(boundary, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.reconcile()
			pod := e.attemptPod(lastAttempt(e.onlyIncident()), e.cands[0].Name, e.readyStatus())
			e.endpointSlice(pod)
			for range 4 {
				e.reconcile()
				e.now = e.now.Add(8 * time.Second)
			}
			if boundary == "probe" {
				e.r.Probe = func(context.Context, string, int32, string) error { mutateExecutionNode(e, "cordon"); return nil }
				e.reconcile()
			} else {
				e.withFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					err := c.List(ctx, list, opts...)
					if _, ok := list.(*discoveryv1.EndpointSliceList); ok && err == nil {
						mutateExecutionNode(e, "trigger_ready")
					}
					return err
				}}, func() { e.reconcile() })
			}
			requireSafetyHeld(t, e, true)
		})
	}
}

func TestSafetyStructuralGuardIgnoresResourceUsageAndUnrelatedRV(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.reconcile()
	inc := e.onlyIncident()
	a := lastAttempt(inc)
	e.attemptPod(a, e.cands[0].Name, e.readyStatus())
	n := &corev1.Node{}
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(e.cands[0]), n))
	n.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse("50m")
	n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionFalse})
	e.must(e.c.Status().Update(e.ctx, n))
	observed := (deploymentExecution{e.r}).ObserveApplied(e.ctx, inc, e.liveDeployment(), &a)
	if observed.Guard.State != nodePass || observed.HaltReason != "" {
		t.Fatalf("self usage or unrelated RV triggered hold: %+v", observed)
	}
}

// T10/T15: a generation increase and Node recovery never remove the hold.
// A disabled/ObserveOnly stop may acknowledge quiescence while preserving it.
func TestSafetyHoldPersistsAcrossPolicyChangesAndStop(t *testing.T) {
	for _, mode := range []string{"disable", "observe"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			mutateExecutionNode(e, "cordon")
			e.reconcile()
			held := requireSafetyHeld(t, e, false)
			n := &corev1.Node{}
			e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(e.cands[0]), n))
			n.Spec.Unschedulable = false
			e.must(e.c.Update(e.ctx, n))
			p := e.policy()
			p.Spec.Budget.MaxActionsPerIncident++
			e.must(e.c.Update(e.ctx, p))
			clone := *e.r
			e.r = &clone
			e.reconcile()
			if e.policy().Status.ActiveIncident != nil {
				t.Fatal("generation created new action under hold")
			}
			q := meta.FindStatusCondition(e.policy().Status.Conditions, CondQuiesced)
			if q == nil || q.Status == metav1.ConditionTrue {
				t.Fatal("enabled hold reported Quiesced")
			}
			stopPolicy(e, mode)
			e.reconcile()
			got := requireQuiesced(t, e, true)
			if got.SafetyHold == nil || *got.SafetyHold != *held.SafetyHold {
				t.Fatal("stop erased/changed hold")
			}
			// Even a manually injected new reservation cannot use the held generation.
			if _, err := e.r.claimExecutionFence(e.ctx, e.policy(), e.liveDeployment()); err == nil {
				t.Fatal("held generation got permission")
			}
		})
	}
}

// T11: pending hold precedes deadlines and policy stop; target/owner identity
// loss leaves it pending instead of claiming a terminal acknowledgement.
func TestSafetyPendingCannotBypassBarrier(t *testing.T) {
	for _, transition := range []string{"deadline", "disable", "observe", "delete", "recreate_policy", "replace_deployment"} {
		t.Run(transition, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			mutateExecutionNode(e, "cordon")
			e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return errors.New("hold unavailable")
				}
				return c.Update(ctx, obj, opts...)
			}}, func() {
				_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				if err == nil {
					t.Fatal("hold failure hidden")
				}
			})
			original := e.onlyIncident()
			if original.Status.Runtime.SafetyHaltReason == "" {
				t.Fatal("pending marker absent")
			}
			switch transition {
			case "deadline":
				e.now = original.Spec.Deadline.Time
			case "disable", "observe":
				stopPolicy(e, transition)
			case "delete", "recreate_policy":
				e.must(e.c.Delete(e.ctx, e.policy()))
				if transition == "recreate_policy" {
					p := e.newPolicy(policyName)
					p.Spec.Mode = v1alpha1.PolicyModeEnforce
					e.must(e.c.Create(e.ctx, p))
				}
			case "replace_deployment":
				d := e.liveDeployment()
				e.must(e.c.Delete(e.ctx, d))
				e.must(e.c.Create(e.ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}, Spec: d.Spec}))
			}
			key := client.ObjectKey{Namespace: e.ns, Name: policyName}
			clone := *e.r
			e.r = &clone
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
			inc := e.onlyIncident()
			if transition == "replace_deployment" {
				if err == nil || isTerminal(inc.Status.Runtime.Phase) || e.policy().Status.ActiveIncident == nil {
					t.Fatal("replacement bypassed unconfirmed hold")
				}
			} else {
				// Recreated policy cannot own the old fence; orphan settlement itself is
				// allowed only using immutable original Incident/Deployment identities.
				if transition != "recreate_policy" {
					e.must(err)
				}
				wantPhase(t, inc, PhaseManualRequired, "SAFETY_HALTED")
				e.wantOutcomes(inc, recovery.OutcomeCancelled)
				f, readErr := readFence(e.liveDeployment())
				e.must(readErr)
				if f == nil || f.SafetyHold == nil || f.PolicyUID != original.Spec.PolicyRef.UID {
					t.Fatal("pending stop lost original authority")
				}
			}
		})
	}
}

func TestSafetyV1MigrationCASAndStrictCodec(t *testing.T) {
	t.Run("migration_preserves_owner_floor", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		d := e.liveDeployment()
		p := e.policy()
		f := &executionFence{Version: 1, PolicyUID: string(p.UID), PolicyName: p.Name, DeploymentUID: string(d.UID), ClosedThroughGeneration: p.Generation - 1}
		putFence(d, f)
		e.must(e.c.Update(e.ctx, d))
		before := e.liveDeployment()
		stale := before.DeepCopy()
		changed, err := e.r.claimExecutionFence(e.ctx, p, before)
		e.must(err)
		got, err := readFence(e.liveDeployment())
		e.must(err)
		if !changed || got.Version != 2 || !ownedFence(got, p, before) || got.ClosedThroughGeneration != f.ClosedThroughGeneration || !equality.Semantic.DeepEqual(stale.Spec, before.Spec) {
			t.Fatal("migration changed owner, floor or workload")
		}
		if _, err := e.r.claimExecutionFence(e.ctx, p, stale); !apierrors.IsConflict(err) {
			t.Fatalf("stale migration accepted: %v", err)
		}
		// An unmodified v1 reader rejects the new version even without safetyHold.
		var old struct {
			Version                 int    `json:"version"`
			PolicyUID               string `json:"policyUID"`
			PolicyName              string `json:"policyName"`
			DeploymentUID           string `json:"deploymentUID"`
			ClosedThroughGeneration int64  `json:"closedThroughGeneration"`
		}
		dec := json.NewDecoder(strings.NewReader(e.liveDeployment().Annotations[FenceAnnotation]))
		dec.DisallowUnknownFields()
		decodeErr := dec.Decode(&old)
		if decodeErr == nil && old.Version == 1 {
			t.Fatal("v1 reader accepted v2")
		}
	})
	t.Run("unknown_malformed_duplicate_missing", func(t *testing.T) {
		base := `{"version":2,"policyUID":"p","policyName":"p","deploymentUID":"d","closedThroughGeneration":1}`
		bad := []string{`null`, `{}`, strings.Replace(base, `"version":2`, `"version":3`, 1), strings.Replace(base, `"policyUID":"p"`, `"policyUID":"p","policyUID":"other"`, 1), strings.Replace(base, `"policyUID"`, `"PolicyUID"`, 1), strings.Replace(base, `"closedThroughGeneration":1`, `"closedThroughGeneration":null`, 1), base + `{}`, strings.TrimSuffix(base, "}") + `,"extra":true}`, strings.TrimSuffix(base, "}") + `,"safetyHold":null}`, strings.TrimSuffix(base, "}") + `,"safetyHold":{"holdID":"h","incidentUID":"i","actionID":"a","policyGeneration":1,"reason":"unsafe","extra":true}}`, strings.TrimSuffix(base, "}") + `,"safetyHold":{"holdID":"h","incidentUID":"i","actionID":"a","policyGeneration":2,"reason":"unsafe"}}`}
		for _, raw := range bad {
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{FenceAnnotation: raw}}}
			if f, err := readFence(d); err == nil {
				t.Fatalf("accepted malformed authority %s: %+v", raw, f)
			}
		}
	})
}

// T07: the timed-out first HTTP request can still finish later. A barrier CAS
// conflict must not settle from its earlier pre-write GET as Cancelled.
func TestSafetyTimedOutWriterFinishesDuringHoldCAS(t *testing.T) {
	e := newEnforce(t, candidateIP)
	var delayed *appsv1.Deployment
	injected := false
	e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if d, ok := obj.(*appsv1.Deployment); ok {
			f, err := readFence(d)
			e.must(err)
			if f != nil && f.SafetyHold != nil && !injected {
				injected = true
				e.must(e.c.Update(ctx, delayed))
			}
			if d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && delayed == nil {
				delayed = d.DeepCopy()
				return context.DeadlineExceeded
			}
		}
		return c.Update(ctx, obj, opts...)
	}}, func() {
		if e.reconcile().RequeueAfter != conflictRequeue {
			t.Fatal("late store must invalidate pending hold CAS")
		}
	})
	if !injected || e.policy().Status.ActiveIncident == nil || isTerminal(e.onlyIncident().Status.Runtime.Phase) {
		t.Fatal("late request was prematurely settled")
	}
	clone := *e.r
	e.r = &clone
	e.reconcile()
	requireSafetyHeld(t, e, true)
}

// Independent review P1: ordinary generation edits must not release a Reserved
// cancellation while another worker still holds a valid old Deployment RV.
func TestSafetyUnauthorizedFencesDelayedWriter(t *testing.T) {
	for _, kind := range []string{"generation", "disable", "observe", "replace_policy"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			injected := false
			e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !injected {
					injected = true
					p := e.policy()
					switch kind {
					case "generation":
						p.Spec.Filter.MaxSampleAge.Duration += time.Second
						e.must(e.c.Update(ctx, p))
					case "disable", "observe":
						stopPolicy(e, kind)
					case "replace_policy":
						e.must(e.c.Delete(ctx, p))
						p = e.newPolicy(policyName)
						p.Spec.Mode = v1alpha1.PolicyModeEnforce
						e.must(e.c.Create(ctx, p))
					}
					other := *e.r
					other.Client = e.c
					_, err := other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
					e.must(err)
				}
				return c.Update(ctx, obj, opts...)
			}}, func() {
				_, _ = e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
			})
			if !injected || e.workloadChanged(e.liveDeployment()) {
				t.Fatal("unauthorized delayed workload stored after cancellation")
			}
			inc := e.onlyIncident()
			f, err := readFence(e.liveDeployment())
			e.must(err)
			if f == nil || f.ClosedThroughGeneration < inc.Spec.PolicyGeneration {
				t.Fatal("cancellation omitted durable barrier")
			}
			if kind == "replace_policy" && f.PolicyUID == string(e.policy().UID) {
				t.Fatal("replacement policy inherited original fence")
			}
			e.wantOutcomes(inc, recovery.OutcomeCancelled)
		})
	}
}

func TestSafetyDispatchUncertaintySurvivesDualOutage(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			uncertain, markerFailed, holdFailed := false, false, false
			e.withFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok {
						f, err := readFence(d)
						e.must(err)
						if f != nil && f.SafetyHold != nil {
							holdFailed = true
							return errors.New("hold outage")
						}
						if d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" && !uncertain {
							uncertain = true
							if applied {
								e.must(c.Update(ctx, obj, opts...))
							}
							return context.DeadlineExceeded
						}
					}
					return c.Update(ctx, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.SafetyHaltReason != "" {
						markerFailed = true
						return errors.New("pending status outage")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, func() {
				_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				if err == nil {
					t.Fatal("dual outage hidden")
				}
			})
			if !uncertain || !markerFailed || !holdFailed {
				t.Fatal("fault injection incomplete")
			}
			if isTerminal(e.onlyIncident().Status.Runtime.Phase) || e.policy().Status.ActiveIncident == nil {
				t.Fatal("uncertainty completed during outage")
			}
			clone := *e.r
			e.r = &clone
			e.reconcile()
			requireSafetyHeld(t, e, applied)
		})
	}
}

func TestSafetyDispatchMarkerRequiresAcknowledgement(t *testing.T) {
	for _, fault := range []string{"status_error", "pruned"} {
		t.Run(fault, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			injected := false
			workloadWrites := 0
			e.withFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
						workloadWrites++
					}
					return c.Update(ctx, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.WorkloadDispatchActionID != "" {
						injected = true
						if fault == "status_error" {
							return errors.New("dispatch marker API unavailable")
						}
						inc.Status.Runtime.WorkloadDispatchActionID = ""
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, func() {
				_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
				if err == nil {
					t.Fatal("unconfirmed dispatch marker accepted")
				}
			})
			if !injected || workloadWrites != 0 || e.workloadChanged(e.liveDeployment()) || e.policy().Status.ActiveIncident == nil {
				t.Fatal("unacknowledged dispatch marker authorized workload")
			}
		})
	}
}

func TestSafetyOldWorkerCannotClearNewActionDispatch(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	old := e.onlyIncident()
	e.must(e.r.recordDispatch(e.ctx, old))
	current := e.onlyIncident()
	last := len(current.Status.Runtime.Attempts) - 1
	current.Status.Runtime.Attempts[last].ActionID = "new-action"
	current.Status.Runtime.WorkloadDispatchActionID = "new-action"
	e.must(e.c.Status().Update(e.ctx, current))
	if err := e.r.clearDispatch(e.ctx, old); !apierrors.IsConflict(err) {
		t.Fatalf("old worker clear must conflict: %v", err)
	}
	if e.onlyIncident().Status.Runtime.WorkloadDispatchActionID != "new-action" {
		t.Fatal("old worker cleared a different action")
	}
}

func TestSafetyLegacyCancellationCASOrderings(t *testing.T) {
	for _, order := range []string{"close_first", "write_first"} {
		t.Run(order, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			inc := e.onlyIncident()
			delayed := e.liveDeployment()
			a := lastAttempt(inc)
			recovery.Mutate(&delayed.Spec.Template, a.Hostname, a.ActionID)
			p := e.policy()
			p.Spec.Filter.MaxSampleAge.Duration += time.Second
			e.must(e.c.Update(e.ctx, p))
			injected := false
			e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok && order == "write_first" && !injected {
					f, err := readFence(d)
					e.must(err)
					if f != nil && f.SafetyHold == nil && f.ClosedThroughGeneration >= inc.Spec.PolicyGeneration {
						injected = true
						e.must(e.c.Update(ctx, delayed))
					}
				}
				return c.Update(ctx, obj, opts...)
			}}, func() {
				res := e.reconcile()
				if order == "write_first" && res.RequeueAfter != conflictRequeue {
					t.Fatal("write-first must conflict with close")
				}
			})
			if order == "write_first" {
				if e.policy().Status.ActiveIncident == nil {
					t.Fatal("unconfirmed cancellation released")
				}
				e.reconcile()
			} else if err := e.c.Update(e.ctx, delayed); !apierrors.IsConflict(err) {
				t.Fatalf("close allowed delayed workload: %v", err)
			}
			settled := e.onlyIncident()
			wantPhase(t, settled, PhaseAborted, ReasonPolicyChanged)
			outcome := recovery.OutcomeCancelled
			if order == "write_first" {
				outcome = recovery.OutcomePatched
			}
			e.wantOutcomes(settled, outcome)
			f, err := readFence(e.liveDeployment())
			e.must(err)
			if f == nil || f.ClosedThroughGeneration < inc.Spec.PolicyGeneration || e.policy().Status.ActiveIncident != nil {
				t.Fatal("cancellation barrier/settlement incomplete")
			}
		})
	}
}

func TestSafetyHoldDuringProbePreventsRecovered(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.reconcile()
	pod := e.attemptPod(lastAttempt(e.onlyIncident()), e.cands[0].Name, e.readyStatus())
	e.endpointSlice(pod)
	for range 4 {
		e.reconcile()
		e.now = e.now.Add(8 * time.Second)
	}
	e.r.Probe = func(ctx context.Context, _ string, _ int32, _ string) error {
		mutateExecutionNode(e, "cordon")
		wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
		e.must(err)
		other := *e.r
		other.Probe = func(context.Context, string, int32, string) error { return nil }
		other.Client = interceptor.NewClient(wc, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.SafetyHaltReason != "" {
				return errors.New("pending status unavailable")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}})
		_, err = other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
		if err == nil {
			t.Fatal("pending status outage missing")
		}
		n := &corev1.Node{}
		e.must(e.c.Get(ctx, client.ObjectKeyFromObject(e.cands[0]), n))
		n.Spec.Unschedulable = false
		e.must(e.c.Update(ctx, n))
		return nil
	}
	e.reconcile()
	requireSafetyHeld(t, e, true)
}

func TestSafetyDispatchMarkerLatencyRechecksExpiry(t *testing.T) {
	for _, boundary := range []string{"metrics", "deadline"} {
		t.Run(boundary, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			fixedMetrics(e, e.now.Add(-20*time.Second), 0)
			injected := false
			e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				err := c.SubResource(sub).Update(ctx, obj, opts...)
				if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.WorkloadDispatchActionID != "" && !injected && err == nil {
					injected = true
					if boundary == "metrics" {
						e.now = e.now.Add(10 * time.Second)
					} else {
						e.now = inc.Spec.Deadline.Time
					}
				}
				return err
			}}, func() { e.reconcile() })
			if !injected || e.workloadChanged(e.liveDeployment()) {
				t.Fatal("dispatch CAS latency crossed boundary and still wrote")
			}
			if boundary == "metrics" {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
				if e.onlyIncident().Status.Runtime.WorkloadDispatchActionID != "" {
					t.Fatal("definite defer retained unconfirmed dispatch")
				}
			} else {
				requireSafetyHeld(t, e, false)
			}
		})
	}
}

func TestSafetyVerifyingRefreshesDeploymentAuthority(t *testing.T) {
	for _, change := range []string{"uid", "spec", "template", "owner", "read_error"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.reconcile()
			pod := e.attemptPod(lastAttempt(e.onlyIncident()), e.cands[0].Name, e.readyStatus())
			e.endpointSlice(pod)
			for range 4 {
				e.reconcile()
				e.now = e.now.Add(8 * time.Second)
			}
			injected := false
			e.r.Probe = func(ctx context.Context, _ string, _ int32, _ string) error {
				injected = true
				d := e.liveDeployment()
				switch change {
				case "uid":
					e.must(e.c.Delete(ctx, d))
					e.must(e.c.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, Annotations: d.Annotations}, Spec: d.Spec}))
				case "spec":
					d.Spec.MinReadySeconds++
					e.must(e.c.Update(ctx, d))
				case "template":
					d.Spec.Template.Spec.Containers[0].Image = "external:changed"
					e.must(e.c.Update(ctx, d))
				case "owner":
					f, err := readFence(d)
					e.must(err)
					f.PolicyUID = "other-owner"
					putFence(d, f)
					e.must(e.c.Update(ctx, d))
				case "read_error":
					wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
					e.must(err)
					e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							return context.DeadlineExceeded
						}
						return c.Get(ctx, key, obj, opts...)
					}})
				}
				return nil
			}
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
			if !injected || e.onlyIncident().Status.Runtime.Phase == PhaseRecovered {
				t.Fatal("stale pre-probe authority published recovery")
			}
			if change == "spec" || change == "template" {
				e.must(err)
				requireSafetyHeld(t, e, true)
			} else if err == nil || e.policy().Status.ActiveIncident == nil || isTerminal(e.onlyIncident().Status.Runtime.Phase) {
				t.Fatal("uncertain/foreign authority completed or transferred")
			}
		})
	}
}

func TestSafetyMonitoringTargetDeletionBetweenReaders(t *testing.T) {
	e := newEnforce(t, candidateIP)
	injected := false
	podLists := 0
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*appsv1.Deployment); ok && !injected {
			injected = true
			e.must(e.c.Delete(ctx, e.liveDeployment()))
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	e.withFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.PodList); ok {
			podLists++
		}
		return c.List(ctx, list, opts...)
	}}, func() { e.reconcile() })
	if !injected || podLists != 0 || e.policy().Status.ActiveIncident != nil {
		t.Fatal("missing live target continued stale monitoring or reserved")
	}
	c := meta.FindStatusCondition(e.policy().Status.Conditions, CondMonitoring)
	if c == nil || c.Reason != CondTargetNotFound {
		t.Fatalf("missing target condition: %+v", c)
	}
}
