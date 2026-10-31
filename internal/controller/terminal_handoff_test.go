package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Real Controller/action/terminal/stop API and Python capture/seal. Kubelet,
// application HTTP/logs, Guard IPC and model transport are explicit local doubles.
func TestTerminalControllerThroughSealAndEngineeringIntent(t *testing.T) {
	b := newHeartbeatControllerBridge(t)
	hook := func(e *enforceFixture, f *exhibitionFixture, stage string, next func()) {
		b.pulse(e, f, stage, next, true)
	}
	e, source, f := newReproductionEnforceDurationStages(t, true, true, hook)
	unreadyReproduction(e, source, f)
	b.pulse(e, f, "known-unready-before-action", func() {}, true)
	before := e.liveDeployment().DeepCopy()
	before.Kind = "Deployment"
	before.APIVersion = "apps/v1"
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
	beforePod := source.DeepCopy()
	beforePod.Kind = "Pod"
	beforePod.APIVersion = "v1"
	beforeTime := e.now
	e.reconcile()
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
	b.pulse(e, f, "same-incident-progress", func() { e.reconcile() }, true)
	status := e.readyStatus()
	status.ContainerStatuses[0].ImageID = before.Spec.Template.Spec.Containers[0].Image
	status.ContainerStatuses[0].ContainerID = "containerd://local-replacement"
	pod := e.attemptPod(lastAttempt(e.onlyIncident()), e.cands[0].Name, status)
	pod.Annotations = map[string]string{recovery.AttemptAnnotation: lastAttempt(e.onlyIncident()).ActionID}
	e.must(e.c.Update(e.ctx, pod))
	e.endpointSlice(pod)
	// envtest has no kubelet: explicit synthetic Event, same live Pod UID.
	e.must(e.c.Create(e.ctx, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-probe-failed", Namespace: e.ns}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: e.ns, Name: pod.Name, UID: pod.UID}, Reason: "Unhealthy", Message: "synthetic readiness probe failed"}))
	e.r.Probe = func(context.Context, string, int32, string) error {
		return errors.New("synthetic application HTTP 500")
	}
	e.reconcile()
	// Advance a deterministic clock while delivering fresh signed-scope grants.
	// The last +1 second happens in pulse, exactly across the terminal boundary.
	deadline := e.onlyIncident().Status.Runtime.Attempts[0].PodReadyAt.Time.Add(e.policy().Spec.Budget.VerificationTimeout.Duration)
	for e.now.Before(deadline) {
		n := e.now.Add(8 * time.Second)
		if n.After(deadline) {
			n = deadline
		}
		e.now = n
		refreshReproductionHeartbeat(e, f)
		e.reconcile()
	}
	if e.onlyIncident().Status.Runtime.Phase != PhaseVerifying {
		t.Fatal("fixture ended before controlled terminal boundary")
	}
	b.terminal = map[string]any{"beforeDeployment": before, "beforePod": beforePod, "beforeTime": beforeTime, "startedAt": beforeTime.Add(-time.Second)}
	b.terminalStop = func() {
		p := e.policy()
		old := p.DeepCopy()
		p.Spec.Enabled = false
		e.must(e.c.Patch(e.ctx, p, client.MergeFrom(old)))
		e.reconcile()
		for _, c := range e.policy().Status.Conditions {
			if c.Type == CondQuiesced && c.Status == "True" && c.Reason == "FenceClosed" {
				return
			}
		}
		t.Fatal("local Controller did not confirm stop fence")
	}
	b.terminalCleanup = func() {
		p := e.policy()
		if p.Spec.Enabled || p.Status.ActiveIncident != nil {
			t.Fatal("cleanup before confirmed stop")
		}
		d := e.liveDeployment()
		baseline, _, err := reproductionTemplates(f.permit.ApprovedReproduction)
		e.must(err)
		d.Spec.Template = baseline
		e.must(e.c.Update(e.ctx, d))
		inc := e.onlyIncident()
		wantPhase(t, inc, PhaseFailed, ReasonBudgetExhausted)
		e.must(e.c.Delete(e.ctx, inc, client.Preconditions{UID: &inc.UID}))
	}
	b.pulse(e, f, "terminal-after-applied-heartbeat", func() {
		e.reconcile()
		e.reconcile()
		wantPhase(t, e.onlyIncident(), PhaseFailed, ReasonBudgetExhausted)
		if f.a.Authorize(e.policy(), nil) == nil {
			t.Fatal("consumed Incident permitted another action")
		}
	}, true)
	if e.policy().Spec.Enabled {
		t.Fatal("final cleanup left Policy enabled")
	}
}
