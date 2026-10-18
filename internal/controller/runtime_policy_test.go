package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Ported from the audit's failing CTL-01 reproducer: the API update completes
// inside the first metrics call, before the same reconcile reaches execute.
type astraQueryHook struct {
	promv1.API
	once func()
}

func (s *astraQueryHook) Query(ctx context.Context, q string, at time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	if s.once != nil {
		f := s.once
		s.once = nil
		f()
	}
	return s.API.Query(ctx, q, at, opts...)
}

func TestAstraStopDuringMetricsMustPreventDeploymentWrite(t *testing.T) {
	for _, mode := range []string{"disable", "observe"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			reason := ReasonPolicyDisabled
			e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() {
				p := e.policy()
				if mode == "disable" {
					p.Spec.Enabled = false
				} else {
					p.Spec.Mode = v1alpha1.PolicyModeObserveOnly
					reason = ReasonModeChanged
				}
				e.must(e.c.Update(e.ctx, p))
			}}
			e.reconcile()
			p, d, inc := e.policy(), e.liveDeployment(), e.onlyIncident()
			t.Logf("live enabled=%v mode=%s deploymentGeneration=%d phase=%s attempts=%d", p.Spec.Enabled, p.Spec.Mode, d.Generation, inc.Status.Runtime.Phase, len(inc.Status.Runtime.Attempts))
			if e.workloadChanged(d) {
				t.Fatalf("Deployment changed after completed policy stop: generation=%d", d.Generation)
			}
			wantPhase(t, inc, PhaseAborted, reason)
			e.wantOutcomes(inc, recovery.OutcomeCancelled)
			if p.Status.ActiveIncident != nil {
				t.Fatal("stopped reservation not released")
			}
		})
	}
}

func TestExecutionPolicyChangesFailClosed(t *testing.T) {
	for _, kind := range []string{"spec", "spec_round_trip", "deleting", "deleted", "recreated", "active_name", "active_uid", "active_sequence", "active_removed"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			reason := ReasonPolicyChanged
			preserve := false
			var saved *v1alpha1.RecoveryPolicy
			e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() {
				p := e.policy()
				switch kind {
				case "spec":
					p.Spec.Filter.MaxCPUUtilizationPercent--
					e.must(e.c.Update(e.ctx, p))
				case "spec_round_trip":
					p.Spec.Mode = v1alpha1.PolicyModeObserveOnly
					e.must(e.c.Update(e.ctx, p))
					p.Spec.Mode = v1alpha1.PolicyModeEnforce
					e.must(e.c.Update(e.ctx, p))
				case "deleting":
					reason = ReasonPolicyDeleted
					p.Finalizers = []string{"opsloop.io/test-hold"}
					e.must(e.c.Update(e.ctx, p))
					e.must(e.c.Delete(e.ctx, p))
				case "deleted", "recreated":
					reason = ReasonPolicyDeleted
					e.must(e.c.Delete(e.ctx, p))
					if kind == "recreated" {
						p = e.newPolicy(policyName)
						p.Spec.Mode = v1alpha1.PolicyModeEnforce
						e.must(e.c.Create(e.ctx, p))
						preserve = true
					}
				default:
					preserve = true
					switch kind {
					case "active_name":
						p.Status.ActiveIncident.Name = "other-incident"
					case "active_uid":
						p.Status.ActiveIncident.IncidentUID = "other-uid"
					case "active_sequence":
						p.Status.ActiveIncident.Sequence++
					case "active_removed":
						p.Status.ActiveIncident = nil
					}
					e.must(e.c.Status().Update(e.ctx, p))
				}
				if preserve {
					saved = e.policy()
				}
			}}
			e.reconcile()
			inc := e.onlyIncident()
			wantPhase(t, inc, PhaseAborted, reason)
			e.wantOutcomes(inc, recovery.OutcomeCancelled)
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("invalid authorization changed Deployment")
			}
			if preserve && e.policy().ResourceVersion != saved.ResourceVersion {
				t.Fatal("unrelated Policy reservation/status overwritten")
			}
			if kind == "spec" || kind == "spec_round_trip" || kind == "deleting" {
				if e.policy().Status.ActiveIncident != nil {
					t.Fatal("matching reservation not released")
				}
			}
		})
	}
}

type executionPolicyReader struct {
	client.Reader
	fail bool
}

func (r *executionPolicyReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1alpha1.RecoveryPolicy); ok && r.fail {
		r.fail = false
		return errors.New("injected uncached policy read failure")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestExecutionPolicyReadFailureRetriesSameReservation(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "resume"
		if stop {
			name = "stop_before_retry"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			reader := &executionPolicyReader{Reader: e.c}
			e.r.APIReader = reader
			e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() { reader.fail = true }}
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
			if err == nil {
				t.Fatal("authorization read failure not surfaced")
			}
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("read failure allowed write")
			}
			inc := e.onlyIncident()
			action := lastAttempt(inc).ActionID
			e.wantOutcomes(inc, recovery.OutcomeReserved)
			if stop {
				p := e.policy()
				p.Spec.Enabled = false
				e.must(e.c.Update(e.ctx, p))
			}
			restarted := *e.r
			e.r = &restarted
			e.reconcile()
			inc = e.onlyIncident()
			if len(inc.Status.Runtime.Attempts) != 1 || lastAttempt(inc).ActionID != action {
				t.Fatal("retry reserved a second action")
			}
			if stop {
				wantPhase(t, inc, PhaseAborted, ReasonPolicyDisabled)
				e.wantOutcomes(inc, recovery.OutcomeCancelled)
			} else {
				wantPhase(t, inc, PhaseVerifying, "")
				e.wantOutcomes(inc, recovery.OutcomePatched)
			}
			want := int64(2)
			if stop {
				want = 1
			}
			if e.workloadChanged(e.liveDeployment()) != (want == 2) {
				t.Fatal("unexpected retry Deployment generation")
			}
		})
	}
}

func TestExecutionPolicyStopWhileResumingReserved(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	action := lastAttempt(e.onlyIncident()).ActionID
	restarted := *e.r
	e.r = &restarted
	e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() { p := e.policy(); p.Spec.Enabled = false; e.must(e.c.Update(e.ctx, p)) }}
	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseAborted, ReasonPolicyDisabled)
	e.wantOutcomes(inc, recovery.OutcomeCancelled)
	if lastAttempt(inc).ActionID != action || e.workloadChanged(e.liveDeployment()) {
		t.Fatal("resume ignored stop or duplicated action")
	}
}

func TestExecutionPolicyStopAfterReservation(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		err := c.SubResource(sub).Update(ctx, obj, opts...)
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && lastReserved(inc) != nil && err == nil {
			p := e.policy()
			p.Spec.Enabled = false
			e.must(e.c.Update(e.ctx, p))
		}
		return err
	}}, func() { e.reconcile() })
	wantPhase(t, e.onlyIncident(), PhaseAborted, ReasonPolicyDisabled)
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("reservation boundary stop allowed write")
	}
}

func TestExecutionPolicyAbortConflictRetriesWithoutWrite(t *testing.T) {
	for _, where := range []string{"incident", "release"} {
		t.Run(where, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() { p := e.policy(); p.Spec.Enabled = false; e.must(e.c.Update(e.ctx, p)) }}
			injected := false
			e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				match := false
				if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok {
					match = where == "incident" && inc.Status.Runtime.Phase == PhaseAborted
				}
				if p, ok := obj.(*v1alpha1.RecoveryPolicy); ok {
					match = where == "release" && p.Status.ActiveIncident == nil
				}
				if match && !injected {
					injected = true
					return apierrors.NewConflict(schema.GroupResource{Group: "opsloop.io", Resource: "status"}, obj.GetName(), errors.New("injected"))
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}}, func() {
				if res := e.reconcile(); res.RequeueAfter != conflictRequeue {
					t.Fatalf("conflict requeue=%v", res.RequeueAfter)
				}
			})
			e.reconcile()
			wantPhase(t, e.onlyIncident(), PhaseAborted, ReasonPolicyDisabled)
			e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
			if !injected || e.workloadChanged(e.liveDeployment()) || e.policy().Status.ActiveIncident != nil {
				t.Fatal("abort retry failed to close safely")
			}
		})
	}
}
