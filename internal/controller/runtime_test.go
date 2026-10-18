package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
)

const attemptPodIP = "10.244.0.10"

// promStub is a fake Prometheus reporting one healthy node-exporter series per ip, or HTTP 500 when fail is set.
type promStub struct {
	f    *fixture
	ips  []string
	fail bool
}

func (s *promStub) api() promv1.API {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = r.ParseForm()
		q := r.Form.Get("query")
		ts := fmt.Sprintf("%.3f", float64(s.f.now.UnixMilli())/1000)
		var val string
		switch {
		case strings.HasPrefix(q, "1 - avg"):
			val = "0.2"
		case strings.HasPrefix(q, "min by (instance) (timestamp("), strings.HasPrefix(q, "timestamp("):
			val = ts
		case strings.HasPrefix(q, "min by (instance) (count_over_time("):
			val = "6"
		case strings.HasPrefix(q, "node_memory_MemAvailable_bytes"):
			val = "2147483648"
		case strings.HasPrefix(q, "node_memory_MemTotal_bytes"):
			val = "4294967296"
		case strings.HasPrefix(q, "up{"):
			val = "1"
		}
		var series []string
		for _, ip := range s.ips {
			if val != "" {
				series = append(series, fmt.Sprintf(`{"metric":{"instance":"%s:9100"},"value":[%s,"%s"]}`, ip, ts, val))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(series, ","))
	}))
	s.f.t.Cleanup(srv.Close)
	c, err := promapi.NewClient(promapi.Config{Address: srv.URL})
	if err != nil {
		s.f.t.Fatal(err)
	}
	return promv1.NewAPI(c)
}

type probeCall struct {
	ip     string
	port   int32
	podUID string
}

type enforceFixture struct {
	*fixture
	prom   *promStub
	cands  []*corev1.Node
	probes []probeCall
}

// newEnforce switches the base fixture to Enforce and adds one Ready candidate per ip (cand-a, cand-b, ...).
// Candidate hostname labels differ from Node names so the template must carry the label value.
func newEnforce(t *testing.T, ips ...string) *enforceFixture {
	t.Helper()
	e := &enforceFixture{fixture: newFixture(t, "")}
	e.prom = &promStub{f: e.fixture, ips: ips}
	e.r.Prom = e.prom.api()
	e.r.Probe = func(_ context.Context, ip string, port int32, podUID string) error {
		e.probes = append(e.probes, probeCall{ip, port, podUID})
		return nil
	}
	for i, ip := range ips {
		n := e.node("cand-"+string(rune('a'+i)), corev1.ConditionTrue, ip)
		n.Labels[hostnameLabel] = "h-" + n.Name
		e.must(e.c.Update(e.ctx, n))
		e.cands = append(e.cands, n)
	}
	p := e.policy()
	p.Spec.Mode = v1alpha1.PolicyModeEnforce
	e.must(e.c.Update(e.ctx, p))
	return e
}

// withFuncs routes the reconciler's writes through funcs once; the APIReader stays the plain client.
func (e *enforceFixture) withFuncs(funcs interceptor.Funcs, run func()) {
	e.t.Helper()
	wc, err := client.NewWithWatch(startEnv(e.t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	e.r.Client = interceptor.NewClient(wc, funcs)
	defer func() { e.r.Client = e.c }()
	run()
}

// crashBeforeUpdate leaves a Reserved attempt with an unchanged Deployment (Update rejected with 409).
func (e *enforceFixture) crashBeforeUpdate() {
	e.t.Helper()
	e.withFuncs(interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("injected"))
		}
		return c.Update(ctx, obj, opts...)
	}}, func() {
		if res := e.reconcile(); res.RequeueAfter != conflictRequeue {
			e.t.Fatalf("conflict requeue %v, want %v", res.RequeueAfter, conflictRequeue)
		}
	})
	inc := e.onlyIncident()
	wantPhase(e.t, inc, PhaseExecuting, candidates.DecisionSelected)
	e.wantOutcomes(inc, recovery.OutcomeReserved)
	if d := e.liveDeployment(); e.workloadChanged(d) {
		e.t.Fatalf("Deployment changed before crash: generation %d", d.Generation)
	}
}

// crashAfterUpdate applies the Deployment change and then fails the Patched status write.
func (e *enforceFixture) crashAfterUpdate() {
	e.t.Helper()
	e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && inc.Status.Runtime.Phase == PhaseVerifying {
			return errors.New("injected crash")
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}, func() {
		if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: policyName}}); err == nil {
			e.t.Fatal("injected crash not surfaced")
		}
	})
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
	if d := e.liveDeployment(); !e.workloadChanged(d) {
		e.t.Fatalf("Deployment generation %d after crash, want 2", d.Generation)
	}
}

func (e *enforceFixture) wantOutcomes(inc *v1alpha1.RecoveryIncident, outcomes ...string) {
	e.t.Helper()
	var got []string
	for _, a := range inc.Status.Runtime.Attempts {
		got = append(got, a.Outcome)
	}
	if !slices.Equal(got, outcomes) {
		e.t.Fatalf("attempt outcomes %v, want %v", got, outcomes)
	}
}

// workloadChanged compares the persisted pod template with the fixture baseline.
// Deployment metadata annotations also increase generation, so generation alone
// cannot distinguish a fence claim/close from a rollout-producing template write.
func (e *enforceFixture) workloadChanged(d *appsv1.Deployment) bool {
	return v1alpha1.TemplateHash(d.Spec.Template) != v1alpha1.TemplateHash(e.deploy.Spec.Template)
}

func lastAttempt(inc *v1alpha1.RecoveryIncident) v1alpha1.Attempt {
	at := inc.Status.Runtime.Attempts
	return at[len(at)-1]
}

func (e *enforceFixture) updateDeployment(mutate func(*appsv1.Deployment)) {
	e.t.Helper()
	d := e.liveDeployment()
	mutate(d)
	e.must(e.c.Update(e.ctx, d))
}

func (e *enforceFixture) setReady(n *corev1.Node, s corev1.ConditionStatus) {
	e.t.Helper()
	live := &corev1.Node{}
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(n), live))
	live.Status.Conditions[0].Status = s
	live.Status.Conditions[0].LastTransitionTime = metav1.NewTime(e.now)
	e.must(e.c.Status().Update(e.ctx, live))
}

func controllerRef(obj metav1.Object, kind string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: obj.GetName(), UID: obj.GetUID(), Controller: &yes}}
}

// attemptPod creates the ReplicaSet a deployment controller would create for the attempt and its Pod on node.
func (e *enforceFixture) attemptPod(a v1alpha1.Attempt, node string, status corev1.PodStatus) *corev1.Pod {
	e.t.Helper()
	d := e.liveDeployment()
	one := int32(1)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-" + a.ActionID, Namespace: e.ns, OwnerReferences: controllerRef(d, "Deployment")},
		Spec:       appsv1.ReplicaSetSpec{Replicas: &one, Selector: d.Spec.Selector, Template: d.Spec.Template},
	}
	e.must(e.c.Create(e.ctx, rs))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + a.ActionID, Namespace: e.ns, Labels: d.Spec.Template.Labels, OwnerReferences: controllerRef(rs, "ReplicaSet")},
		Spec:       *d.Spec.Template.Spec.DeepCopy(),
	}
	pod.Spec.NodeName = node
	e.must(e.c.Create(e.ctx, pod))
	pod.Status = status
	e.must(e.c.Status().Update(e.ctx, pod))
	return pod
}

func (e *enforceFixture) readyStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: attemptPodIP,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(e.now),
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "demo-api", Ready: true, Image: e.deploy.Spec.Template.Spec.Containers[0].Image, ImageID: "docker.io/demo-api@sha256:01",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(e.now)}},
		}},
	}
}

func pullStatus(msg string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "demo-api", Image: "demo-api",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: msg}},
		}},
	}
}

func (e *enforceFixture) endpointSlice(pod *corev1.Pod) {
	e.t.Helper()
	ready := true
	e.must(e.c.Create(e.ctx, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "demo-api-eps", Namespace: e.ns, Labels: map[string]string{discoveryv1.LabelServiceName: "demo-api"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{attemptPodIP},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: e.ns, Name: pod.Name, UID: pod.UID},
		}},
	}))
}

func candidateReasons(d v1alpha1.RecoveryDecision, node string) []string {
	for _, c := range d.Candidates {
		if c.NodeName == node {
			return c.RejectReasons
		}
	}
	return nil
}

func TestE8EnforceExecutesConditionalUpdate(t *testing.T) {
	e := newEnforce(t, candidateIP)
	before := e.liveDeployment()
	_, err := e.r.claimExecutionFence(e.ctx, e.policy(), before)
	e.must(err)
	before = e.liveDeployment()

	e.reconcile()

	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseVerifying, "")
	e.wantOutcomes(inc, recovery.OutcomePatched)
	a := lastAttempt(inc)
	cand := e.cands[0]
	if a.ActionID != inc.Name+"-a1" || a.Decision.ActionID != a.ActionID || inc.Status.Runtime.Decision.ActionID != a.ActionID {
		t.Fatalf("actionID %q decision %q", a.ActionID, a.Decision.ActionID)
	}
	if a.NodeName != cand.Name || a.Hostname != "h-"+cand.Name || a.PreResourceVersion != before.ResourceVersion || a.PatchedAt == nil {
		t.Fatalf("attempt = %+v", a)
	}
	d := e.liveDeployment()
	if got := d.Spec.Template.Spec.NodeSelector[hostnameLabel]; got != "h-"+cand.Name {
		t.Fatalf("nodeSelector hostname %q, want the label value h-%s", got, cand.Name)
	}
	if d.Spec.Template.Annotations[recovery.AttemptAnnotation] != a.ActionID || d.Generation != before.Generation+1 {
		t.Fatalf("annotation %q generation %d", d.Spec.Template.Annotations[recovery.AttemptAnnotation], d.Generation)
	}
	if h := v1alpha1.TemplateHash(d.Spec.Template); h != a.TargetTemplateHash || h != inc.Status.Runtime.ExpectedTemplateHash || inc.Status.Runtime.Message != "" {
		t.Fatalf("live hash %s target %s expected %s msg %q", h, a.TargetTemplateHash, inc.Status.Runtime.ExpectedTemplateHash, inc.Status.Runtime.Message)
	}
	if a.PreTemplateHash != inc.Spec.Baseline.TemplateHash {
		t.Fatalf("preTemplateHash %s, want baseline", a.PreTemplateHash)
	}
	if !slices.ContainsFunc(d.ManagedFields, func(m metav1.ManagedFieldsEntry) bool { return m.Manager == fieldOwner }) {
		t.Fatalf("managedFields lack %s: %+v", fieldOwner, d.ManagedFields)
	}
}

func TestE9ReservedBeforeUpdateAppliesOnce(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()

	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseVerifying, "")
	e.wantOutcomes(inc, recovery.OutcomePatched)
	if d := e.liveDeployment(); !e.workloadChanged(d) || d.Spec.Template.Annotations[recovery.AttemptAnnotation] != lastAttempt(inc).ActionID {
		t.Fatalf("want exactly one change, generation %d", d.Generation)
	}
}

func TestE10ReservedAfterUpdateResumesWithoutUpdate(t *testing.T) {
	t.Run("applied_ack_not_persisted", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.crashAfterUpdate()
		before := e.liveDeployment().Spec.DeepCopy()
		if e.onlyIncident().Status.Runtime.WorkloadDispatchActionID == "" {
			t.Fatal("write-ahead evidence absent")
		}
		e.reconcile()
		requireSafetyHeld(t, e, true)
		if !reflect.DeepEqual(before, &e.liveDeployment().Spec) {
			t.Fatal("uncertain resume changed workload again")
		}
	})
	t.Run("applied_then_image_changed", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.crashAfterUpdate()
		e.updateDeployment(func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "demo-api:external" })
		e.reconcile()
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseManualRequired, "SAFETY_HALTED")
		e.wantOutcomes(inc, recovery.OutcomeSuperseded)
		if e.liveDeployment().Spec.Template.Spec.Containers[0].Image != "demo-api:external" {
			t.Fatal("external image overwritten")
		}
		f, err := readFence(e.liveDeployment())
		e.must(err)
		if f.SafetyHold == nil {
			t.Fatal("uncertain dispatch not fenced")
		}
	})
	t.Run("legacy_already_applied", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.crashAfterUpdate()
		legacy := e.onlyIncident()
		legacy.Status.Runtime.WorkloadDispatchActionID = ""
		e.must(e.c.Status().Update(e.ctx, legacy))
		rv := e.liveDeployment().ResourceVersion
		e.reconcile()
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseVerifying, "")
		e.wantOutcomes(inc, recovery.OutcomePatched)
		if inc.Status.Runtime.Message != "resumed" || e.liveDeployment().ResourceVersion != rv {
			t.Fatal("legacy applied intent was replayed")
		}
	})
}

// A Reserved cancellation fences delayed workers before reporting completion.
func TestE21ReservedReevaluatesBeforeApplying(t *testing.T) {
	for _, change := range []string{"ready", "trigger_ready"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.crashBeforeUpdate()
			before := e.liveDeployment()
			mutateExecutionNode(e, change)
			e.reconcile()
			requireSafetyHeld(t, e, false)
			if e.workloadChanged(e.liveDeployment()) || e.liveDeployment().ResourceVersion == before.ResourceVersion {
				t.Fatal("reserved cancellation did not store metadata-only barrier")
			}
		})
	}
}

func TestE11ExternalChangesSupersede(t *testing.T) {
	t.Run("image before execution", func(t *testing.T) {
		e := newEnforce(t)
		e.reconcile()
		wantPhase(t, e.onlyIncident(), PhaseWaiting, candidates.DecisionNoCandidate)
		e.updateDeployment(func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "demo-api:external" })

		e.reconcile()
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseSuperseded, ReasonTemplateChanged)
		if len(inc.Status.Runtime.Attempts) != 0 || e.liveDeployment().Spec.Template.Spec.Containers[0].Image != "demo-api:external" {
			t.Fatal("external image not kept or attempt created")
		}
	})
	zero := int32(0)
	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.Deployment)
		reason string
	}{
		{"image while verifying", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "demo-api:external" }, ReasonTemplateChanged},
		{"scale to zero", func(d *appsv1.Deployment) { d.Spec.Replicas = &zero }, ReasonSpecChanged},
		{"paused", func(d *appsv1.Deployment) { d.Spec.Paused = true }, ReasonSpecChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.reconcile()
			e.updateDeployment(tc.mutate)
			rv := e.liveDeployment().ResourceVersion

			e.reconcile()
			wantPhase(t, e.onlyIncident(), PhaseSuperseded, tc.reason)
			if e.liveDeployment().ResourceVersion != rv {
				t.Fatal("external change overwritten")
			}
		})
	}
}

func TestE12WorkloadReplacedSupersedes(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.reconcile()
	e.must(e.c.Delete(e.ctx, e.liveDeployment()))
	d := e.deploy.DeepCopy()
	d.ResourceVersion, d.UID, d.ManagedFields = "", "", nil
	e.must(e.c.Create(e.ctx, d))

	e.reconcile()
	wantPhase(t, e.onlyIncident(), PhaseSuperseded, ReasonWorkloadReplaced)
}

func TestE13VerifiedAttemptRecovers(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.reconcile()
	a := lastAttempt(e.onlyIncident())
	pod := e.attemptPod(a, e.cands[0].Name, e.readyStatus())
	e.endpointSlice(pod)

	var inc *v1alpha1.RecoveryIncident
	for range 6 {
		e.reconcile()
		if inc = e.onlyIncident(); inc.Status.Runtime.Phase == PhaseRecovered {
			break
		}
		wantPhase(t, inc, PhaseVerifying, "")
		e.now = e.now.Add(8 * time.Second)
	}
	wantPhase(t, inc, PhaseRecovered, "")
	e.wantOutcomes(inc, recovery.OutcomeSucceeded)
	got := lastAttempt(inc)
	if got.PodUID != string(pod.UID) || got.CompletedAt == nil || got.ConsecutiveSuccesses < 5 || !got.EndpointReady {
		t.Fatalf("attempt = %+v", got)
	}
	if len(e.probes) < 5 || e.probes[0] != (probeCall{attemptPodIP, 8080, string(pod.UID)}) {
		t.Fatalf("probes = %+v", e.probes)
	}
	old := inc.Status.Runtime.OldPodsOutstanding
	if len(old) != 1 || old[0].Name != "demo-api-1-a" || old[0].NodeName != e.ns+"-primary" {
		t.Fatalf("oldPodsOutstanding = %+v, want the original Pod", old)
	}
	p := e.policy()
	if p.Status.ActiveIncident != nil || len(p.Status.Circuits) != 0 || len(p.Status.RecentFailures) != 0 {
		t.Fatalf("policy status after RECOVERED = %+v", p.Status)
	}
}

func TestE14FailedAttemptsCooldownAndExhaustBudget(t *testing.T) {
	e := newEnforce(t, candidateIP, "10.0.0.6")
	a, b := e.cands[0], e.cands[1]
	e.reconcile()
	first := lastAttempt(e.onlyIncident())
	if first.NodeName != a.Name {
		t.Fatalf("first attempt on %s, want %s", first.NodeName, a.Name)
	}
	e.attemptPod(first, a.Name, corev1.PodStatus{Phase: corev1.PodPending})

	e.now = e.now.Add(91 * time.Second)
	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseVerifying, "")
	e.wantOutcomes(inc, recovery.OutcomeFailed, recovery.OutcomePatched)
	at := inc.Status.Runtime.Attempts
	if at[0].FailureReason != "PLACEMENT_TIMEOUT" || at[0].CompletedAt == nil {
		t.Fatalf("first attempt = %+v", at[0])
	}
	if at[1].NodeName != b.Name || !slices.Contains(candidateReasons(at[1].Decision, a.Name), candidates.RejectCooldown) {
		t.Fatalf("second attempt on %s, reasons for %s: %v", at[1].NodeName, a.Name, candidateReasons(at[1].Decision, a.Name))
	}
	if at[1].PreTemplateHash != at[0].TargetTemplateHash {
		t.Fatal("second attempt must start from the first attempt's applied template")
	}
	if got := e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation]; got != at[1].ActionID {
		t.Fatalf("Deployment annotation %s, want %s", got, at[1].ActionID)
	}
	if n := len(e.policy().Status.RecentFailures); n != 0 {
		t.Fatalf("recentFailures written during the Incident: %d", n)
	}

	active := e.policy().Status.ActiveIncident
	e.now = e.now.Add(91 * time.Second)
	e.reconcile()
	inc = e.onlyIncident()
	wantPhase(t, inc, PhaseFailed, ReasonBudgetExhausted)
	e.wantOutcomes(inc, recovery.OutcomeFailed, recovery.OutcomeFailed)
	ids := func() []string {
		var out []string
		for _, f := range e.policy().Status.RecentFailures {
			out = append(out, f.ActionID)
		}
		slices.Sort(out)
		return out
	}
	want := []string{at[0].ActionID, at[1].ActionID}
	if got := ids(); !slices.Equal(got, want) {
		t.Fatalf("recentFailures %v, want %v", got, want)
	}

	// Crash between the terminal write and release: releasing again must not duplicate failures.
	p := e.policy()
	p.Status.ActiveIncident = active
	e.must(e.r.writePolicy(e.ctx, p))
	e.reconcile()
	if got := ids(); !slices.Equal(got, want) {
		t.Fatalf("recentFailures after repeated release %v, want %v", got, want)
	}
}

func TestE15PolicyDisabledAborts(t *testing.T) {
	disable := func(e *enforceFixture) {
		p := e.policy()
		p.Spec.Enabled = false
		e.must(e.c.Update(e.ctx, p))
	}
	t.Run("verifying", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.reconcile()
		before := e.liveDeployment().Spec.DeepCopy()
		disable(e)

		e.reconcile()
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseAborted, ReasonPolicyDisabled)
		e.wantOutcomes(inc, recovery.OutcomePatched)
		if lastAttempt(inc).CompletedAt == nil || !reflect.DeepEqual(before, &e.liveDeployment().Spec) {
			t.Fatal("patched attempt left open or Deployment changed after disable")
		}
	})
	t.Run("reserved", func(t *testing.T) {
		e := newEnforce(t, candidateIP)
		e.crashBeforeUpdate()
		before := e.liveDeployment().Spec.DeepCopy()
		disable(e)

		e.reconcile()
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseAborted, ReasonPolicyDisabled)
		e.wantOutcomes(inc, recovery.OutcomeCancelled)
		if lastAttempt(inc).CompletedAt == nil || !reflect.DeepEqual(before, &e.liveDeployment().Spec) {
			t.Fatal("reserved attempt not settled or Deployment changed")
		}
	})
}

func TestE16ImagePullFailuresCooldownNode(t *testing.T) {
	for _, tc := range []struct{ msg, reason string }{
		{"failed to pull image: no match for platform in manifest", "IMAGE_PLATFORM_MISSING"},
		{"Back-off pulling image: unauthorized", "IMAGE_PULL_FAILED"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			e := newEnforce(t, candidateIP, "10.0.0.6")
			e.reconcile()
			e.attemptPod(lastAttempt(e.onlyIncident()), e.cands[0].Name, pullStatus(tc.msg))
			e.now = e.now.Add(4 * time.Second)

			e.reconcile()
			inc := e.onlyIncident()
			e.wantOutcomes(inc, recovery.OutcomeFailed, recovery.OutcomePatched)
			at := inc.Status.Runtime.Attempts
			if at[0].FailureReason != tc.reason {
				t.Fatalf("failureReason %s, want %s", at[0].FailureReason, tc.reason)
			}
			if at[1].NodeName != e.cands[1].Name || !slices.Contains(candidateReasons(at[1].Decision, e.cands[0].Name), candidates.RejectCooldown) {
				t.Fatalf("second attempt on %s", at[1].NodeName)
			}
		})
	}
}

func TestE6PrometheusDownWaitsThenDeadline(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.prom.fail = true
	before := e.liveDeployment()

	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseWaiting, candidates.DecisionMetricsUnavailable)
	if !slices.Contains(candidateReasons(*inc.Status.Runtime.Decision, e.cands[0].Name), candidates.RejectMetricsMissing) {
		t.Fatal("candidate not rejected METRICS_MISSING")
	}

	e.now = inc.Spec.Deadline.Time
	e.reconcile()
	inc = e.onlyIncident()
	wantPhase(t, inc, PhaseFailed, ReasonDeadlineExceeded)
	if d := e.liveDeployment(); d.ResourceVersion != before.ResourceVersion || len(inc.Status.Runtime.Attempts) != 0 {
		t.Fatal("Deployment changed or attempt recorded while Prometheus was down")
	}
}

func TestE18AllRejectedSurvivesEvictionUntilDeadline(t *testing.T) {
	e := newEnforce(t, candidateIP)
	cand := e.cands[0]
	cand.Spec.Unschedulable = true
	e.must(e.c.Update(e.ctx, cand))
	before := e.liveDeployment()

	e.reconcile()
	inc := e.onlyIncident()
	wantPhase(t, inc, PhaseWaiting, candidates.DecisionNoCandidate)
	if inc.Status.Runtime.Decision.DecisionReason != candidates.DecisionNoCandidate || len(inc.Status.Runtime.Attempts) != 0 {
		t.Fatal("decision or attempts wrong")
	}

	// Taint eviction after ~300s: the old Pod turns Terminating and a Pending replacement targets the failed Node.
	e.now = e.now.Add(300 * time.Second)
	old := &corev1.Pod{}
	e.must(e.c.Get(e.ctx, types.NamespacedName{Namespace: e.ns, Name: "demo-api-1-a"}, old))
	old.Finalizers = append(old.Finalizers, testLabel)
	e.must(e.c.Update(e.ctx, old))
	e.must(e.c.Delete(e.ctx, old))
	repl := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api-1-b", Namespace: e.ns, Labels: old.Labels, OwnerReferences: old.OwnerReferences},
		Spec:       *e.deploy.Spec.Template.Spec.DeepCopy(),
	}
	e.must(e.c.Create(e.ctx, repl))

	e.reconcile()
	wantPhase(t, e.onlyIncident(), PhaseWaiting, candidates.DecisionNoCandidate)

	e.now = inc.Spec.Deadline.Time
	e.reconcile()
	inc = e.onlyIncident()
	wantPhase(t, inc, PhaseFailed, ReasonDeadlineExceeded)
	if len(inc.Status.Runtime.Attempts) != 0 || e.liveDeployment().ResourceVersion != before.ResourceVersion {
		t.Fatal("attempt recorded or Deployment changed")
	}
	e.reconcile()
	e.onlyIncident()
}
