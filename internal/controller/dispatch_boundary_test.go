package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// These are local API-server tests, with synthetic clock/metrics/kubelet inputs.
// Inject only after the dispatch intent has been persisted and at the last
// source, Policy or Deployment read. Count action Update requests as well as
// inspecting the stored template: rejection by a CAS is not a zero-write guard.
func TestDispatchBoundaryFinalReads(t *testing.T) {
	for _, boundary := range []string{"source", "policy", "deployment"} {
		for _, kind := range []string{"heartbeat-expired", "metrics-expired", "valid-short-lookup"} {
			t.Run(boundary+"/"+kind, func(t *testing.T) {
				testDispatchBoundary(t, boundary, kind)
			})
		}
	}
	for _, kind := range []string{
		"grant-not-after", "resource-expires-at", "incident-deadline",
		"monotonic-heartbeat-expired", "clock-error", "clock-reversed",
		"read-error", "read-timeout", "valid-resource-lookup",
	} {
		t.Run("deployment/"+kind, func(t *testing.T) {
			testDispatchBoundary(t, "deployment", kind)
		})
	}
	for _, kind := range []string{"permit-revoked", "generation-changed", "source-identity-changed"} {
		t.Run("source/"+kind, func(t *testing.T) {
			testDispatchBoundary(t, "source", kind)
		})
	}
}

func testDispatchBoundary(t *testing.T, boundary, kind string) {
	t.Helper()
	policy := func(p *v1alpha1.RecoveryPolicy) {
		if kind == "metrics-expired" {
			p.Spec.Filter.MaxSampleAge.Duration = time.Second
		}
		if kind == "incident-deadline" {
			p.Spec.Budget.IncidentDeadline.Duration = 2 * time.Second
		}
	}
	permit := func(e *enforceFixture, f *exhibitionFixture) {
		if kind == "grant-not-after" {
			// unreadyReproduction advances exactly 31 seconds with heartbeats;
			// the last API lookup consumes the remaining two-second grant.
			f.grant.NotAfter = e.now.Add(33 * time.Second)
			f.permit.Grant = f.grant.ExecutionGrantCore
		}
		if kind == "resource-expires-at" || kind == "valid-resource-lookup" {
			cpu := int32(300)
			s := f.grant.Scope
			f.permit.ResourceConditions = &candidates.ResourceConditions{
				Schema: "opsloop-resource-conditions/v1", RunID: s.RunID, Epoch: s.Epoch,
				PolicyUID: s.PolicyUID, DeploymentUID: s.DeploymentUID,
				NotBefore: e.now, ExpiresAt: e.now.Add(33 * time.Second),
				Nodes: []candidates.NodeResourceCondition{{NodeName: e.cands[0].Name, NodeUID: string(e.cands[0].UID), CPUUtilizationFloorPermille: &cpu}},
			}
		}
	}
	e, source, f := newReproductionEnforceConfigured(t, false, false, policy, permit)
	unreadyReproduction(e, source, f)
	before := e.liveDeployment().Spec.Template.DeepCopy()
	armed, injected := false, false
	sourceReads, actionRequests := 0, 0
	var dispatchIncident *v1alpha1.RecoveryIncident
	var injectedErr error

	inject := func(ctx context.Context) {
		injected = true
		switch kind {
		case "heartbeat-expired":
			e.now = f.grant.HeartbeatAt.Add(executionHeartbeatTTL)
		case "metrics-expired":
			e.now = e.now.Add(2 * time.Second)
		case "grant-not-after":
			e.now = f.grant.NotAfter
		case "resource-expires-at":
			e.now = f.permit.ResourceConditions.ExpiresAt
		case "incident-deadline":
			e.now = dispatchIncident.Spec.Deadline.Time
		case "monotonic-heartbeat-expired":
			original := f.a.current.authority.sample
			f.a.current.authority.sample = func() (authoritySample, error) {
				n, err := original()
				n.tick += executionHeartbeatTTL
				return n, err
			}
		case "clock-error":
			f.a.current.authority.sample = func() (authoritySample, error) {
				return authoritySample{}, errors.New("synthetic boot clock unavailable")
			}
		case "clock-reversed":
			e.now = e.now.Add(-time.Second)
		case "read-error":
			injectedErr = errors.New("synthetic final Deployment read unavailable")
		case "read-timeout":
			injectedErr = apierrors.NewTimeoutError("synthetic final Deployment read timeout", 0)
		case "permit-revoked":
			p := e.policy()
			delete(p.Annotations, ExhibitionPermitAnnotation)
			e.must(e.c.Update(ctx, p))
		case "generation-changed":
			p := e.policy()
			p.Spec.Filter.MaxCPUUtilizationPercent--
			e.must(e.c.Update(ctx, p))
		case "source-identity-changed":
			e.must(e.c.Get(ctx, client.ObjectKeyFromObject(source), source))
			source.Status.ContainerStatuses[0].ContainerID = "containerd://changed-at-final-source-read"
			e.must(e.c.Status().Update(ctx, source))
		default:
			e.now = e.now.Add(time.Second)
		}
	}
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		atBoundary := false
		if armed {
			switch obj.(type) {
			case *corev1.Pod:
				if key.Name == source.Name {
					sourceReads++
					atBoundary = boundary == "source" && sourceReads == 2
				}
			case *v1alpha1.RecoveryPolicy:
				atBoundary = boundary == "policy" && sourceReads >= 2
			case *appsv1.Deployment:
				atBoundary = boundary == "deployment" && sourceReads >= 2
			}
		}
		// Object changes must precede GET so the final observation sees them.
		// Delays/errors occur after GET, reproducing time spent awaiting its reply.
		change := kind == "permit-revoked" || kind == "generation-changed" || kind == "source-identity-changed"
		if atBoundary && !injected && change {
			inject(ctx)
		}
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if atBoundary && !injected {
			inject(ctx)
			return injectedErr
		}
		return nil
	}})
	var reconcileErr error
	e.withFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			err := c.SubResource(sub).Update(ctx, obj, opts...)
			if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && err == nil && inc.Status.Runtime.WorkloadDispatchActionID != "" {
				armed = true
				dispatchIncident = inc.DeepCopy()
			}
			return err
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if d, ok := obj.(*appsv1.Deployment); ok && d.Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
				actionRequests++
			}
			return c.Update(ctx, obj, opts...)
		},
	}, func() {
		_, reconcileErr = e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.policy())})
	})
	if !injected || dispatchIncident == nil {
		t.Fatalf("final %s boundary not reached: injected=%v sourceReads=%d", boundary, injected, sourceReads)
	}
	if injectedErr != nil {
		if reconcileErr == nil || !strings.Contains(reconcileErr.Error(), injectedErr.Error()) {
			t.Fatalf("read error lost: got %v, want %v", reconcileErr, injectedErr)
		}
	} else {
		e.must(reconcileErr)
	}
	live := e.liveDeployment()
	inc := e.onlyIncident()
	positive := kind == "valid-short-lookup" || kind == "valid-resource-lookup"
	if positive {
		if actionRequests != 1 || reflect.DeepEqual(*before, live.Spec.Template) {
			t.Fatalf("valid dispatch: action Update requests=%d changed=%v", actionRequests, !reflect.DeepEqual(*before, live.Spec.Template))
		}
		e.wantOutcomes(inc, recovery.OutcomePatched)
		return
	}
	if actionRequests != 0 || !reflect.DeepEqual(*before, live.Spec.Template) {
		t.Fatalf("unsafe dispatch: action Update requests=%d changed=%v", actionRequests, !reflect.DeepEqual(*before, live.Spec.Template))
	}
	switch kind {
	case "heartbeat-expired", "grant-not-after", "monotonic-heartbeat-expired", "clock-error", "clock-reversed", "permit-revoked":
		wantPhase(t, inc, PhaseAborted, ReasonExecutionAuthority)
	case "generation-changed":
		wantPhase(t, inc, PhaseAborted, ReasonPolicyChanged)
	case "metrics-expired":
		if lastReserved(inc) == nil || inc.Status.Runtime.WorkloadDispatchActionID != "" || inc.Status.Runtime.SafetyHaltReason != "" || inc.Status.Runtime.Phase != PhaseExecuting {
			t.Fatalf("stale metrics did not defer reserved attempt: %+v", inc.Status.Runtime)
		}
		if err := f.a.Authorize(e.policy(), inc); err != nil {
			t.Fatalf("data-only expiry retired authority: %v", err)
		}
	case "resource-expires-at":
		if !strings.HasPrefix(inc.Status.Runtime.SafetyHaltReason, "RESOURCE_CONDITIONS_UNCONFIRMED:") {
			t.Fatalf("resource expiry not classified: %+v", inc.Status.Runtime)
		}
	case "incident-deadline":
		if inc.Status.Runtime.SafetyHaltReason != ReasonDeadlineExceeded {
			t.Fatalf("deadline not classified: %+v", inc.Status.Runtime)
		}
	case "source-identity-changed":
		if inc.Status.Runtime.SafetyHaltReason != "REPRODUCTION_SOURCE_UNCONFIRMED" {
			t.Fatalf("source change not classified: %+v", inc.Status.Runtime)
		}
	}
}
