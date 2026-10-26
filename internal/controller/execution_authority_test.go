package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type authorityFixture struct {
	a *ExecutionAuthority
	p *v1alpha1.RecoveryPolicy
	g ExecutionGrant
	n authoritySample
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	f := &authorityFixture{n: authoritySample{time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), 100 * time.Second}}
	f.p = &v1alpha1.RecoveryPolicy{ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "demo-a", UID: "policy-uid", Generation: 5}, Spec: v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "demo-api"}}}
	s := ExecutionScope{ClusterUID: "cluster", NamespaceUID: "namespace", Namespace: f.p.Namespace, Policy: f.p.Name, PolicyUID: string(f.p.UID), PolicyGeneration: 5, PolicyHash: v1alpha1.PolicyHash(f.p.Spec), Deployment: "demo-api", DeploymentUID: "deployment", Incident: "first", RunID: "demo-a", Epoch: "epoch-2"}
	var err error
	f.a, err = NewExecutionAuthority(s)
	if err != nil {
		t.Fatal(err)
	}
	f.a.sample = func() (authoritySample, error) { return f.n, nil }
	f.g = ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: s, Instance: f.a.Instance(), NotBefore: f.n.wall, NotAfter: f.n.wall.Add(600 * time.Second)}, HeartbeatSequence: 1, HeartbeatAt: f.n.wall}
	f.publish(t)
	return f
}
func (f *authorityFixture) publish(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.g)
	if err != nil {
		t.Fatal(err)
	}
	f.p.Annotations = map[string]string{ExecutionGrantAnnotation: string(raw)}
}
func (f *authorityFixture) advance(d time.Duration) { f.n.wall = f.n.wall.Add(d); f.n.tick += d }
func (f *authorityFixture) beat(t *testing.T) {
	f.g.HeartbeatSequence++
	f.g.HeartbeatAt = f.n.wall
	f.publish(t)
}
func (f *authorityFixture) incident() *v1alpha1.RecoveryIncident {
	return &v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Name: f.g.Scope.Incident, Namespace: f.p.Namespace, UID: "incident-uid", Annotations: map[string]string{ExecutionGrantBindingAnnotation: executionGrantBinding(f.p)}}, Spec: v1alpha1.RecoveryIncidentSpec{PolicyRef: v1alpha1.PolicyRef{Name: f.p.Name, UID: string(f.p.UID)}, PolicyGeneration: f.p.Generation, PolicyHash: v1alpha1.PolicyHash(f.p.Spec), Workload: v1alpha1.WorkloadRef{Name: "demo-api", UID: f.g.Scope.DeploymentUID}}}
}
func mustAllowAuthority(t *testing.T, f *authorityFixture, inc *v1alpha1.RecoveryIncident) {
	t.Helper()
	if err := f.a.Authorize(f.p, inc); err != nil {
		t.Fatal(err)
	}
}
func mustDenyAuthority(t *testing.T, f *authorityFixture, inc *v1alpha1.RecoveryIncident) {
	t.Helper()
	if err := f.a.Authorize(f.p, inc); err == nil {
		t.Fatal("unbound/expired execution permitted")
	}
}
func TestExecutionAuthorityDefaultDeny(t *testing.T) {
	f := newAuthorityFixture(t)
	r := &PolicyReconciler{}
	if r.authorizeExecution(f.p, nil) == nil {
		t.Fatal("nil authorizer permitted Enforce")
	}
	a, err := NewExecutionAuthority(ExecutionScope{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Authorize(f.p, nil) == nil {
		t.Fatal("empty scope permitted Enforce")
	}
}
func TestExecutionAuthorityHealthyHeartbeatCannotExtendFixedDeadline(t *testing.T) {
	f := newAuthorityFixture(t)
	mustAllowAuthority(t, f, nil)
	inc := f.incident()
	for i := 0; i < 299; i++ {
		f.advance(2 * time.Second)
		f.beat(t)
		mustAllowAuthority(t, f, inc)
	}
	f.advance(2 * time.Second)
	f.beat(t)
	mustDenyAuthority(t, f, inc)
}
func TestExecutionAuthorityGuardExitRetiresGrant(t *testing.T) {
	f := newAuthorityFixture(t)
	mustAllowAuthority(t, f, nil)
	f.advance(10 * time.Second)
	mustDenyAuthority(t, f, nil)
	f.beat(t)
	mustDenyAuthority(t, f, nil)
}
func TestExecutionAuthorityReplayCannotRefreshHeartbeat(t *testing.T) {
	f := newAuthorityFixture(t)
	mustAllowAuthority(t, f, nil)
	f.advance(9 * time.Second)
	mustAllowAuthority(t, f, nil)
	f.advance(time.Second)
	mustDenyAuthority(t, f, nil)
}
func TestExecutionAuthorityHeartbeatCounterAndTimestampReplay(t *testing.T) {
	for _, kind := range []string{"lower-sequence", "same-sequence-new-time", "new-sequence-old-time"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorityFixture(t)
			mustAllowAuthority(t, f, nil)
			f.advance(time.Second)
			f.beat(t)
			mustAllowAuthority(t, f, nil)
			f.advance(time.Second)
			switch kind {
			case "lower-sequence":
				f.g.HeartbeatSequence = 1
			case "same-sequence-new-time":
				f.g.HeartbeatAt = f.n.wall
			case "new-sequence-old-time":
				f.g.HeartbeatSequence++
			}
			f.publish(t)
			mustDenyAuthority(t, f, nil)
		})
	}
}
func TestExecutionAuthorityPowerRestartRejectsEvenUnexpiredGrant(t *testing.T) {
	f := newAuthorityFixture(t)
	mustAllowAuthority(t, f, nil)
	oldInstance := f.a.Instance()
	fresh, err := NewExecutionAuthority(f.g.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if oldInstance == fresh.Instance() {
		t.Fatal("process identity reused")
	}
	fresh.sample = func() (authoritySample, error) { return f.n, nil }
	f.a = fresh
	mustDenyAuthority(t, f, nil)
}
func TestExecutionAuthorityExpiredFirstGrantCannotBeRewritten(t *testing.T) {
	f := newAuthorityFixture(t)
	f.advance(601 * time.Second)
	mustDenyAuthority(t, f, nil)
	f.g.NotBefore = f.n.wall
	f.g.NotAfter = f.n.wall.Add(600 * time.Second)
	f.beat(t)
	mustDenyAuthority(t, f, nil)
}
func TestExecutionAuthorityWrongBindingsAndMalformedGrant(t *testing.T) {
	for _, kind := range []string{"cluster", "namespace-uid", "run", "epoch", "process", "generation", "policy-hash", "deployment", "deployment-uid", "missing", "unknown-json", "future-beat", "zero-sequence", "oversize-window"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorityFixture(t)
			switch kind {
			case "cluster":
				f.g.Scope.ClusterUID = "other"
			case "namespace-uid":
				f.g.Scope.NamespaceUID = "other"
			case "run":
				f.g.Scope.RunID = "other"
			case "epoch":
				f.g.Scope.Epoch = "other"
			case "process":
				f.g.Instance = "other"
			case "generation":
				f.p.Generation++
			case "policy-hash":
				f.g.Scope.PolicyHash = strings.Repeat("0", 64)
			case "deployment":
				f.g.Scope.Deployment = "other"
			case "deployment-uid":
				f.g.Scope.DeploymentUID = "other"
			case "future-beat":
				f.g.HeartbeatAt = f.n.wall.Add(time.Second)
			case "zero-sequence":
				f.g.HeartbeatSequence = 0
			case "oversize-window":
				f.g.NotAfter = f.g.NotAfter.Add(time.Second)
			}
			f.publish(t)
			if kind == "missing" {
				delete(f.p.Annotations, ExecutionGrantAnnotation)
			}
			if kind == "unknown-json" {
				f.p.Annotations[ExecutionGrantAnnotation] = strings.TrimSuffix(f.p.Annotations[ExecutionGrantAnnotation], "}") + `,"unknown":true}`
			}
			mustDenyAuthority(t, f, nil)
		})
	}
}
func TestExecutionAuthorityGrantReplacementAndClockFailuresRetire(t *testing.T) {
	for _, kind := range []string{"extend", "epoch", "wall-backwards", "boot-backwards", "boot-elapsed-frozen-wall", "clock-error", "removed"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorityFixture(t)
			mustAllowAuthority(t, f, nil)
			f.advance(time.Second)
			switch kind {
			case "extend":
				f.g.NotBefore = f.g.NotBefore.Add(time.Second)
				f.g.NotAfter = f.g.NotAfter.Add(time.Second)
			case "epoch":
				f.g.Scope.Epoch = "new"
			case "wall-backwards":
				f.n.wall = f.n.wall.Add(-2 * time.Second)
			case "boot-backwards":
				f.n.tick = 0
			case "boot-elapsed-frozen-wall":
				f.n.tick += 10 * time.Second
			case "clock-error":
				f.a.sample = func() (authoritySample, error) { return f.n, errors.New("unavailable") }
			}
			f.publish(t)
			if kind == "removed" {
				delete(f.p.Annotations, ExecutionGrantAnnotation)
			}
			mustDenyAuthority(t, f, nil)
		})
	}
}
func TestExecutionAuthorityIncidentAndSingleRunBinding(t *testing.T) {
	for _, kind := range []string{"correct", "missing-binding", "old-grant-binding", "new-incident-uid", "wrong-workload", "second-incident", "consumed-monitor"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthorityFixture(t)
			mustAllowAuthority(t, f, nil)
			inc := f.incident()
			mustAllowAuthority(t, f, inc)
			switch kind {
			case "missing-binding":
				inc.Annotations = nil
			case "old-grant-binding":
				inc.Annotations[ExecutionGrantBindingAnnotation] = "old"
			case "new-incident-uid":
				inc.UID = "replacement"
			case "wrong-workload":
				inc.Spec.Workload.UID = "replacement"
			case "second-incident":
				inc.Name = "second"
			case "consumed-monitor":
				f.p.Status.IncidentSequence = 1
				inc = nil
			}
			if kind == "correct" {
				mustAllowAuthority(t, f, inc)
			} else {
				mustDenyAuthority(t, f, inc)
			}
		})
	}
}

func realFixtureAuthority(t *testing.T, e *enforceFixture) (*ExecutionAuthority, *ExecutionGrant) {
	t.Helper()
	p := e.policy()
	d := e.liveDeployment()
	s := ExecutionScope{ClusterUID: "local-api-fixture", NamespaceUID: e.ns, Namespace: e.ns, Policy: p.Name, PolicyUID: string(p.UID), PolicyGeneration: p.Generation, PolicyHash: v1alpha1.PolicyHash(p.Spec), Deployment: d.Name, DeploymentUID: string(d.UID), Incident: p.Name + "-" + string(p.UID)[:8] + "-00001", RunID: "power-safety-local", Epoch: "epoch"}
	a, err := NewExecutionAuthority(s)
	e.must(err)
	start := e.now
	a.sample = func() (authoritySample, error) {
		return authoritySample{e.now, 100*time.Second + e.now.Sub(start)}, nil
	}
	g := &ExecutionGrant{ExecutionGrantCore: ExecutionGrantCore{Version: "opsloop-execution-grant/v1", Scope: s, Instance: a.Instance(), NotBefore: e.now, NotAfter: e.now.Add(600 * time.Second)}, HeartbeatSequence: 1, HeartbeatAt: e.now}
	b, _ := json.Marshal(g)
	p.Annotations = map[string]string{ExecutionGrantAnnotation: string(b)}
	e.must(e.c.Update(e.ctx, p))
	e.r.Authority = a
	return a, g
}
func TestExecutionAuthorityLocalAPINormalActionAndStop(t *testing.T) {
	e := newEnforce(t, candidateIP)
	realFixtureAuthority(t, e)
	e.reconcile()
	if !e.workloadChanged(e.liveDeployment()) {
		t.Fatal("authorized native Controller action absent in local API")
	}
	p := e.policy()
	p.Spec.Enabled = false
	e.must(e.c.Update(e.ctx, p))
	e.reconcile()
	requireQuiesced(t, e, true)
}
func TestExecutionAuthorityLocalAPINoGrantNoIncidentNoAction(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.r.Authority = nil
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("ungranted action")
	}
	var list v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &list, client.InNamespace(e.ns)))
	if len(list.Items) != 0 {
		t.Fatal("ungranted Incident created")
	}
}
func TestExecutionAuthorityLocalAPIRestartCannotResumeReservedAttempt(t *testing.T) {
	e := newEnforce(t, candidateIP)
	a, _ := realFixtureAuthority(t, e)
	e.crashBeforeUpdate()
	fresh, err := NewExecutionAuthority(a.scope)
	e.must(err)
	fresh.sample = a.sample
	e.r.Authority = fresh
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("old Reserved action resumed after simulated power/process restart")
	}
	wantPhase(t, e.onlyIncident(), PhaseAborted, ReasonExecutionAuthority)
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
}
func TestExecutionAuthorityLocalAPIGuardHeartbeatLossPreventsDispatch(t *testing.T) {
	e := newEnforce(t, candidateIP)
	realFixtureAuthority(t, e)
	e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() { e.now = e.now.Add(10 * time.Second) }}
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("expired grant caused workload mutation")
	}
	wantPhase(t, e.onlyIncident(), PhaseAborted, ReasonExecutionAuthority)
}

// Final live Node reads can consume the remaining heartbeat window. Keep the
// existing node evidence valid and expire only the execution authorization.
type authorityNodeDelayReader struct {
	client.Reader
	once func()
}

func (r *authorityNodeDelayReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*corev1.Node); ok && err == nil && r.once != nil {
		f := r.once
		r.once = nil
		f()
	}
	return err
}
func TestExecutionAuthorityLocalAPIFinalNodeDelayCannotDispatch(t *testing.T) {
	e := newEnforce(t, candidateIP)
	realFixtureAuthority(t, e)
	e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() {
		e.r.APIReader = &authorityNodeDelayReader{Reader: e.c, once: func() { e.now = e.now.Add(10 * time.Second) }}
	}}
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("slow Node read crossed heartbeat expiry before dispatch")
	}
	wantPhase(t, e.onlyIncident(), PhaseAborted, ReasonExecutionAuthority)
}

func TestExecutionAuthorityLocalAPIExpiredAuthorityStillAllowsStrongStop(t *testing.T) {
	e := newEnforce(t, candidateIP)
	a, _ := realFixtureAuthority(t, e)
	e.reconcile()
	e.now = e.now.Add(601 * time.Second)
	if a.Authorize(e.policy(), e.onlyIncident()) == nil {
		t.Fatal("expired authority accepted")
	}
	p := e.policy()
	p.Spec.Enabled = false
	e.must(e.c.Update(e.ctx, p))
	e.reconcile()
	requireQuiesced(t, e, true)
}
func TestExecutionAuthorityLocalAPIObserveOnlyWithoutGrant(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.r.Authority = nil
	p := e.policy()
	p.Spec.Mode = v1alpha1.PolicyModeObserveOnly
	e.must(e.c.Update(e.ctx, p))
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("ObserveOnly changed workload")
	}
	if e.onlyIncident().Status.Runtime.Phase != PhaseObserved {
		t.Fatal("ObserveOnly behavior blocked by execution authorization")
	}
}
func TestExecutionAuthorityBootClockReadable(t *testing.T) {
	tick, err := executionBootTick()
	if err != nil || tick <= 0 {
		t.Fatalf("boot clock unavailable: %v/%v", tick, err)
	}
}

type authorityFinalPolicyDelayReader struct {
	client.Reader
	candidate      string
	afterCandidate bool
	delay          func()
}

func (r *authorityFinalPolicyDelayReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if err != nil {
		return err
	}
	if _, ok := obj.(*corev1.Node); ok && key.Name == r.candidate {
		r.afterCandidate = true
	}
	if _, ok := obj.(*v1alpha1.RecoveryPolicy); ok && r.afterCandidate && r.delay != nil {
		f := r.delay
		r.delay = nil
		f()
	}
	return nil
}
func TestExecutionAuthorityLocalAPIFinalPolicyDelayPreservesDeadlineAndMetrics(t *testing.T) {
	for _, kind := range []string{"incident-deadline", "metrics-expiry"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			p := e.policy()
			if kind == "incident-deadline" {
				p.Spec.Budget.IncidentDeadline.Duration = time.Second
			} else {
				p.Spec.Filter.MaxSampleAge.Duration = time.Second
			}
			e.must(e.c.Update(e.ctx, p))
			realFixtureAuthority(t, e)
			e.r.APIReader = &authorityFinalPolicyDelayReader{Reader: e.c, candidate: e.cands[0].Name, delay: func() { e.now = e.now.Add(2 * time.Second) }}
			e.reconcile()
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("final Policy GET crossed Incident/metrics expiry despite still-valid execution grant")
			}
			if kind == "incident-deadline" {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
			} else {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
			}
		})
	}
}

func TestExecutionAuthorityMetadataHeartbeatEnqueuesWithoutGenerationChange(t *testing.T) {
	f := newAuthorityFixture(t)
	old := f.p.DeepCopy()
	f.advance(2 * time.Second)
	f.beat(t)
	pred := policyExecutionPredicate()
	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: f.p}) {
		t.Fatal("metadata heartbeat would be ignored until after grant TTL")
	}
	unchanged := old.DeepCopy()
	unchanged.ResourceVersion = "new-status-rv"
	unchanged.Status.IncidentSequence = 1
	if pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: unchanged}) {
		t.Fatal("unrelated status churn enqueued grant")
	}
	removed := old.DeepCopy()
	removed.Annotations = nil
	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: removed}) {
		t.Fatal("grant revocation not enqueued")
	}
}
func TestExecutionAuthorityUnobservedNewSequenceCannotBypassFrozenWallTTL(t *testing.T) {
	f := newAuthorityFixture(t)
	mustAllowAuthority(t, f, nil)
	f.advance(time.Second)
	f.beat(t)
	f.n.tick += 20 * time.Second
	mustDenyAuthority(t, f, nil)
}
