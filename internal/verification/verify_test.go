package verification

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func controller(uid types.UID) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{UID: uid, Controller: &yes}}
}

func rs(name string, owner types.UID, actionID string) appsv1.ReplicaSet {
	r := appsv1.ReplicaSet{}
	r.Name, r.UID, r.OwnerReferences = name, types.UID(name+"-uid"), controller(owner)
	r.Spec.Template.Annotations = map[string]string{recovery.AttemptAnnotation: actionID}
	return r
}

func pod(name string, owner types.UID, created time.Time, ready bool) corev1.Pod {
	p := corev1.Pod{}
	p.Name, p.UID, p.OwnerReferences = name, types.UID(name+"-uid"), controller(owner)
	p.CreationTimestamp = metav1.NewTime(created)
	p.Spec.NodeName = "node-b"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api"}}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(created)}}
	}
	return p
}

func TestAttemptPod(t *testing.T) {
	rss := []appsv1.ReplicaSet{rs("old", "dep", "inc-a0"), rs("foreign", "other", "inc-a1"), rs("attempt", "dep", "inc-a1")}
	target := types.UID("attempt-uid")
	terminating := pod("terminating", target, t0.Add(3*time.Second), true)
	terminating.DeletionTimestamp = &metav1.Time{Time: t0}
	pods := []corev1.Pod{
		pod("old-pod", "old-uid", t0.Add(5*time.Second), true),
		terminating,
		pod("ready-older", target, t0, true),
		pod("pending-newest", target, t0.Add(4*time.Second), false),
		pod("ready-newer", target, t0.Add(time.Second), true),
	}
	gotRS, gotPod := AttemptPod(rss, pods, "dep", "inc-a1")
	if gotRS == nil || gotRS.Name != "attempt" {
		t.Fatalf("RS = %v, want attempt", gotRS)
	}
	if gotPod == nil || gotPod.Name != "ready-newer" {
		t.Fatalf("Pod = %v, want ready-newer", gotPod)
	}
	if r, p := AttemptPod(rss[:2], pods, "dep", "inc-a1"); r != nil || p != nil {
		t.Fatalf("foreign owner/other action must be ignored, got %v %v", r, p)
	}
	if _, p := AttemptPod(rss, pods[:2], "dep", "inc-a1"); p != nil {
		t.Fatalf("terminating Pod must be excluded, got %v", p.Name)
	}
}

// fixture is an attempt patched at t0 whose Pod became Ready at t0+10s.
type fixture struct{ Input }

func newFixture() *fixture {
	r := rs("attempt", "dep", "inc-a1")
	p := pod("p", r.UID, t0.Add(10*time.Second), true)
	slice := discoveryv1.EndpointSlice{Endpoints: []discoveryv1.Endpoint{{
		TargetRef:  &corev1.ObjectReference{UID: p.UID},
		Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)},
	}}}
	return &fixture{Input{
		Attempt: v1alpha1.Attempt{
			ActionID: "inc-a1", Outcome: recovery.OutcomePatched, NodeName: "node-b",
			ReservedAt: metav1.NewTime(t0), PatchedAt: &metav1.Time{Time: t0},
		},
		Verification: v1alpha1.VerificationSpec{ConsecutiveSuccesses: 5, StabilityWindow: metav1.Duration{Duration: 30 * time.Second}},
		Budget: v1alpha1.BudgetSpec{
			PlacementTimeout:    metav1.Duration{Duration: 90 * time.Second},
			VerificationTimeout: metav1.Duration{Duration: 60 * time.Second},
		},
		RS: &r, Pod: &p, Slices: []discoveryv1.EndpointSlice{slice},
	}}
}

// step runs one Step at now and carries the attempt forward like the controller does.
func (f *fixture) step(now time.Time, probeErr error) Outcome {
	f.Now, f.ProbeRan, f.ProbeErr = now, f.Pod != nil && podReady(f.Pod) != nil, probeErr
	var out Outcome
	f.Attempt, out = Step(f.Input)
	return out
}

func ptrBool(b bool) *bool { return &b }

func TestStepStability(t *testing.T) {
	f := newFixture()
	start := t0.Add(10 * time.Second)
	// Five successes spanning only 29s are not yet stable.
	for i, off := range []time.Duration{0, 7, 14, 21, 29} {
		if out := f.step(start.Add(off*time.Second), nil); out != Pending {
			t.Fatalf("probe %d: outcome %v, want Pending", i, out)
		}
	}
	if f.Attempt.ConsecutiveSuccesses != 5 {
		t.Fatalf("ConsecutiveSuccesses = %d, want 5", f.Attempt.ConsecutiveSuccesses)
	}
	if out := f.step(start.Add(30*time.Second), nil); out != Succeeded {
		t.Fatalf("30s and 6 successes: outcome %v, want Succeeded", out)
	}
	a := f.Attempt
	if a.Outcome != recovery.OutcomeSucceeded || a.CompletedAt == nil || !a.PodReadyAt.Time.Equal(start) || a.PodUID != "p-uid" || a.ReplicaSetName != "attempt" {
		t.Fatalf("attempt not finalized: %+v", a)
	}
}

func TestStepExactlyFiveAt30s(t *testing.T) {
	f := newFixture()
	start := t0.Add(10 * time.Second)
	var out Outcome
	for _, off := range []time.Duration{0, 10, 20, 25, 30} {
		out = f.step(start.Add(off*time.Second), nil)
	}
	if out != Succeeded {
		t.Fatalf("5 successes over 30s: outcome %v, want Succeeded", out)
	}
}

func TestStepProbeFailureResets(t *testing.T) {
	f := newFixture()
	start := t0.Add(10 * time.Second)
	f.step(start, nil)
	f.step(start.Add(2*time.Second), nil)
	f.step(start.Add(4*time.Second), errors.New("connection refused"))
	a := f.Attempt
	if a.ConsecutiveSuccesses != 0 || a.FirstHealthyAt != nil || a.LastProbeError != "connection refused" {
		t.Fatalf("failure did not reset: %+v", a)
	}
	f.step(start.Add(6*time.Second), nil)
	if !f.Attempt.FirstHealthyAt.Time.Equal(start.Add(6 * time.Second)) {
		t.Fatalf("FirstHealthyAt = %v, want restart of run", f.Attempt.FirstHealthyAt)
	}
}

func TestStepEndpointNotReady(t *testing.T) {
	f := newFixture()
	f.Slices[0].Endpoints[0].Conditions.Ready = ptrBool(false)
	start := t0.Add(10 * time.Second)
	var out Outcome
	for off := 0; off <= 40; off += 5 {
		out = f.step(start.Add(time.Duration(off)*time.Second), nil)
	}
	if out != Pending || f.Attempt.EndpointReady {
		t.Fatalf("outcome %v endpointReady %v, want Pending/false", out, f.Attempt.EndpointReady)
	}
}

func TestStepFailures(t *testing.T) {
	waiting := func(reason, msg string) func(f *fixture) {
		return func(f *fixture) {
			f.Pod.Status.Conditions = nil
			f.Pod.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: reason, Message: msg}
		}
	}
	crashedNotReady := func(f *fixture) {
		f.step(t0.Add(10*time.Second), nil) // records Ready at restart count 0
		f.Pod.Status.Conditions = nil
		f.Pod.Status.ContainerStatuses[0].RestartCount = 1
	}
	cases := []struct {
		name  string
		setup func(f *fixture)
		at    time.Duration
		want  string
	}{
		{"unexpected node", func(f *fixture) { f.Pod.Spec.NodeName = "node-c" }, 20 * time.Second, "POD_ON_UNEXPECTED_NODE"},
		{"image pull backoff", waiting("ImagePullBackOff", "unauthorized"), 20 * time.Second, "IMAGE_PULL_FAILED"},
		{"platform missing", waiting("ErrImagePull", "no match for platform in manifest"), 20 * time.Second, "IMAGE_PLATFORM_MISSING"},
		{"placement timeout", func(f *fixture) {
			f.Pod.Status.Conditions = nil
			f.Pod.Spec.NodeName = ""
		}, 91 * time.Second, "PLACEMENT_TIMEOUT"},
		{"placement timeout without pod", func(f *fixture) { f.Pod = nil }, 91 * time.Second, "PLACEMENT_TIMEOUT"},
		{"crashed after ready", func(f *fixture) {
			f.step(t0.Add(10*time.Second), nil)
			f.Pod.Status.ContainerStatuses[0].RestartCount = 1
		}, 20 * time.Second, "CRASHED_DURING_STABILITY"},
		{"verification timeout", func(f *fixture) {
			// Keep probing a Ready Pod that never shows up in the EndpointSlice.
			f.Slices = nil
			for off := 10; off < 70; off += 10 {
				f.step(t0.Add(time.Duration(off)*time.Second), nil)
			}
		}, 71 * time.Second, "VERIFICATION_TIMEOUT"},
		{"order: pull backoff beats placement timeout", waiting("ImagePullBackOff", "back-off pulling image"), 91 * time.Second, "IMAGE_PULL_FAILED"},
		{"order: crash beats placement timeout", crashedNotReady, 91 * time.Second, "CRASHED_DURING_STABILITY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.setup(f)
			if out := f.step(t0.Add(tc.at), nil); out != Failed || f.Attempt.FailureReason != tc.want {
				t.Fatalf("outcome %v reason %q, want Failed %q", out, f.Attempt.FailureReason, tc.want)
			}
			if f.Attempt.Outcome != recovery.OutcomeFailed || f.Attempt.CompletedAt == nil {
				t.Fatalf("attempt not finalized: %+v", f.Attempt)
			}
		})
	}
}

func TestStepPendingWithinPlacementTimeout(t *testing.T) {
	f := newFixture()
	f.Pod.Status.Conditions = nil
	f.Pod.Spec.NodeName = ""
	if out := f.step(t0.Add(90*time.Second), nil); out != Pending {
		t.Fatalf("outcome %v at exactly 90s, want Pending", out)
	}
}

func serve(t *testing.T, h http.HandlerFunc) (string, int32) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	a := srv.Listener.Addr().(*net.TCPAddr)
	return a.IP.String(), int32(a.Port)
}

func demoHandler(podUID string, readyStatus int, rootBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/ready":
			w.WriteHeader(readyStatus)
		case "/":
			if rootBody != "" {
				fmt.Fprint(w, rootBody)
				return
			}
			fmt.Fprintf(w, `{"pod":"p","podUID":%q}`, podUID)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	ip, port := serve(t, demoHandler("p-uid", http.StatusOK, ""))
	if err := Probe(ctx, nil, ip, port, "p-uid"); err != nil {
		t.Fatalf("healthy pod: %v", err)
	}
	if err := Probe(ctx, nil, ip, port, "other-uid"); err == nil {
		t.Fatal("podUID mismatch must fail")
	}

	ip, port = serve(t, demoHandler("p-uid", http.StatusServiceUnavailable, ""))
	if err := Probe(ctx, nil, ip, port, "p-uid"); err == nil {
		t.Fatal("/ready 503 must fail")
	}

	big := `{"podUID":"p-uid","pad":"` + strings.Repeat("x", 64<<10) + `"}`
	ip, port = serve(t, demoHandler("p-uid", http.StatusOK, big))
	if err := Probe(ctx, nil, ip, port, "p-uid"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body: err %v, want size error", err)
	}

	ip, port = serve(t, demoHandler("p-uid", http.StatusOK, "not json"))
	if err := Probe(ctx, nil, ip, port, "p-uid"); err == nil {
		t.Fatal("undecodable body must fail")
	}

	ip, port = serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := Probe(ctx, nil, ip, port, "p-uid"); err == nil {
		t.Fatal("redirect must not be followed")
	}
}

func TestStepReadinessFlapAfterReadyIsNotPlacementTimeout(t *testing.T) {
	f := newFixture()
	f.Budget.VerificationTimeout.Duration = 120 * time.Second
	f.step(t0.Add(10*time.Second), nil) // records PodReadyAt
	f.Pod.Status.Conditions = nil       // readiness flaps during stability
	if out := f.step(t0.Add(91*time.Second), nil); out != Pending {
		t.Fatalf("flap past placement timeout: outcome %v reason %q, want Pending", out, f.Attempt.FailureReason)
	}
	if out := f.step(t0.Add(131*time.Second), nil); out != Failed || f.Attempt.FailureReason != "VERIFICATION_TIMEOUT" {
		t.Fatalf("outcome %v reason %q, want Failed VERIFICATION_TIMEOUT", out, f.Attempt.FailureReason)
	}
}
