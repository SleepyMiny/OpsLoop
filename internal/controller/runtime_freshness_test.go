package controller

import (
	"context"
	"fmt"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"math"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"strings"
	"testing"
	"time"
)

// Each query advances the controller clock while the exporter samples remain fixed.
// This reproduces the audited seven-query / six-seconds-past-expiry write without sleeping.
type delayedMetrics struct {
	promv1.API
	e     *enforceFixture
	delay time.Duration
	calls int
}

func (s *delayedMetrics) Query(ctx context.Context, q string, at time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	s.calls++
	s.e.now = s.e.now.Add(s.delay)
	return s.API.Query(ctx, q, at, opts...)
}
func fixedMetrics(e *enforceFixture, sample time.Time, delay time.Duration) *delayedMetrics {
	clone := *e.fixture
	clone.now = sample
	stub := &promStub{f: &clone, ips: e.prom.ips}
	s := &delayedMetrics{API: stub.api(), e: e, delay: delay}
	e.r.Prom = s
	return s
}

func TestRuntimeFreshnessSlowQueries(t *testing.T) {
	e := newEnforce(t, candidateIP)
	base := e.now
	s := fixedMetrics(e, base.Add(-29*time.Second), time.Second)
	e.reconcile()
	d := e.liveDeployment()
	t.Logf("queries=%d clock_elapsed=%s snapshot_expired_for=%s deployment_generation=%d", s.calls, e.now.Sub(base), e.now.Sub(base.Add(time.Second)), d.Generation)
	if s.calls != 7 {
		t.Fatalf("queries=%d, want 7", s.calls)
	}
	if e.workloadChanged(d) {
		t.Fatal("expired metrics changed Deployment")
	}
}

func TestRuntimeFreshnessReservationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		want  int64
	}{
		{"before_expiry", 9 * time.Second, 2}, {"exact_expiry", 10 * time.Second, 1}, {"after_expiry", 11 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			fixedMetrics(e, e.now.Add(-20*time.Second), 0)
			delayed := false
			e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				err := c.SubResource(sub).Update(ctx, obj, opts...)
				if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && lastReserved(inc) != nil && err == nil && !delayed {
					delayed = true
					e.now = e.now.Add(tc.delay)
				}
				return err
			}}, func() { e.reconcile() })
			if d := e.liveDeployment(); e.workloadChanged(d) != (tc.want == 2) {
				t.Fatalf("generation=%d, want %d", d.Generation, tc.want)
			}
			if tc.want == 1 {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
				// Restart with fresh samples must continue the same action, once.
				restarted := *e.r
				e.r = &restarted
				e.r.Prom = e.prom.api()
				e.reconcile()
				e.reconcile()
				if d := e.liveDeployment(); !e.workloadChanged(d) {
					t.Fatalf("fresh resume generation=%d", d.Generation)
				}
				if at := e.onlyIncident().Status.Runtime.Attempts; len(at) != 1 || at[0].Outcome != recovery.OutcomePatched {
					t.Fatalf("attempts after resume: %+v", at)
				}
			}
		})
	}
}

func TestRuntimeFreshnessRestartAfterConflict(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			base := e.now
			e.crashBeforeUpdate()
			e.now = e.now.Add(40 * time.Second)
			restarted := *e.r
			e.r = &restarted
			sample := base
			if fresh {
				sample = e.now
			}
			fixedMetrics(e, sample, 0)
			e.reconcile()
			want := int64(1)
			if fresh {
				want = 2
			}
			if d := e.liveDeployment(); e.workloadChanged(d) != (want == 2) {
				t.Fatalf("generation=%d want=%d", d.Generation, want)
			}
		})
	}
}

func TestRuntimeFreshnessInvalidMetrics(t *testing.T) {
	for _, kind := range []string{"missing", "nan", "duplicate", "future", "down", "error"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			s := fixedMetrics(e, e.now, 0)
			e.r.Prom = &alteredMetrics{API: s, kind: kind}
			e.reconcile()
			if d := e.liveDeployment(); e.workloadChanged(d) {
				t.Fatalf("%s metrics changed Deployment", kind)
			}
		})
	}
}

type alteredMetrics struct {
	promv1.API
	kind string
}

func (s *alteredMetrics) Query(ctx context.Context, q string, at time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	v, w, err := s.API.Query(ctx, q, at, opts...)
	if err != nil {
		return v, w, err
	}
	if s.kind == "error" {
		return nil, nil, fmt.Errorf("injected unavailable")
	}
	vec := v.(model.Vector)
	if strings.HasPrefix(q, "1 - avg") {
		switch s.kind {
		case "missing":
			return model.Vector{}, w, nil
		case "nan":
			vec[0].Value = model.SampleValue(math.NaN())
		case "duplicate":
			vec = append(vec, vec[0])
		}
	}
	if s.kind == "future" && strings.Contains(q, "timestamp(") {
		vec[0].Value = model.SampleValue(float64(at.Add(time.Minute).Unix()))
	}
	if s.kind == "down" && strings.HasPrefix(q, "up{") {
		vec[0].Value = 0
	}
	return vec, w, nil
}

func TestRuntimeFreshnessAlreadyAppliedRestart(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashAfterUpdate()
	e.now = e.now.Add(40 * time.Second)
	e.prom.fail = true
	restarted := *e.r
	e.r = &restarted
	e.reconcile()
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
	if d := e.liveDeployment(); !e.workloadChanged(d) {
		t.Fatalf("already applied generation=%d", d.Generation)
	}
}

// The freshly preferred node's expiry must never authorize the older samples of
// the reserved node. At the evaluator's inclusive age boundary it is eligible,
// but the write boundary must reject it at exact expiry.
func TestRuntimeFreshnessReservedNodeOwnExpiry(t *testing.T) {
	e := newEnforce(t, candidateIP, "10.0.0.22")
	e.crashBeforeUpdate()
	base := e.now
	slow := fixedMetrics(e, base, time.Second)
	e.r.Prom = &reservedBoundaryMetrics{API: slow, older: base.Add(-23 * time.Second)}
	e.reconcile()
	if d := e.liveDeployment(); e.workloadChanged(d) {
		t.Fatalf("winner's fresh metrics authorized expired reserved node: generation=%d", d.Generation)
	}
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
	e.r.Prom = e.prom.api()
	e.reconcile()
	if d := e.liveDeployment(); !e.workloadChanged(d) || d.Spec.Template.Spec.NodeSelector[hostnameLabel] != e.cands[0].Labels[hostnameLabel] {
		t.Fatalf("reservation not resumed once on original node: %+v", d.Spec.Template.Spec.NodeSelector)
	}
}

type reservedBoundaryMetrics struct {
	promv1.API
	older time.Time
}

func (s *reservedBoundaryMetrics) Query(ctx context.Context, q string, at time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	v, w, err := s.API.Query(ctx, q, at, opts...)
	if err != nil {
		return v, w, err
	}
	for _, sample := range v.(model.Vector) {
		if strings.HasPrefix(string(sample.Metric["instance"]), candidateIP+":") && strings.Contains(q, "timestamp(") {
			sample.Value = model.SampleValue(s.older.Unix())
		}
		if !strings.HasPrefix(string(sample.Metric["instance"]), candidateIP+":") && strings.HasPrefix(q, "1 - avg") {
			sample.Value = 0.1
		}
	}
	return v, w, nil
}

func TestRuntimeFreshnessDeadlineDuringReservation(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		err := c.SubResource(sub).Update(ctx, obj, opts...)
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && lastReserved(inc) != nil && err == nil {
			e.now = inc.Spec.Deadline.Time
		}
		return err
	}}, func() { e.reconcile() })
	requireSafetyHeld(t, e, false)
	if d := e.liveDeployment(); e.workloadChanged(d) {
		t.Fatalf("deadline reached before write, generation=%d", d.Generation)
	}
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
}
