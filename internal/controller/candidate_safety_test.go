package controller

import (
	"context"
	"errors"
	"fmt"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAstraNodeChangeDuringMetricsMustPreventDeploymentWrite(t *testing.T) {
	for _, kind := range []string{"cordon", "ready_false", "trigger_cleared"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.r.Prom = &astraQueryHook{API: e.r.Prom, once: func() {
				switch kind {
				case "cordon":
					n := e.cands[0].DeepCopy()
					n.Spec.Unschedulable = true
					e.must(e.c.Update(e.ctx, n))
				case "ready_false":
					e.setReady(e.cands[0], corev1.ConditionFalse)
				case "trigger_cleared":
					var ns corev1.NodeList
					e.must(e.c.List(e.ctx, &ns))
					for i := range ns.Items {
						if ns.Items[i].Name == e.ns+"-primary" {
							e.setReady(&ns.Items[i], corev1.ConditionTrue)
						}
					}
				}
			}}
			e.reconcile()
			d, inc := e.liveDeployment(), e.onlyIncident()
			t.Logf("change=%s deploymentGeneration=%d phase=%s selectedNode=%s", kind, d.Generation, inc.Status.Runtime.Phase, inc.Status.Runtime.Decision.SelectedNodeName)
			if e.workloadChanged(d) {
				t.Fatalf("Deployment changed after %s during metrics query", kind)
			}
		})
	}
}

func TestAstraOldTriggerMustNotAdoptNewDeploymentBaseline(t *testing.T) {
	e := newEnforce(t, candidateIP)
	injected := false
	e.withFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		err := c.List(ctx, list, opts...)
		// Monitor has already read the old Deployment/RS/Pods. Emulate an external
		// deployment change immediately after that last monitor input was read.
		if _, ok := list.(*corev1.PodList); ok && !injected {
			injected = true
			e.updateDeployment(func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].Image = "demo-api:v2"
				d.Spec.Template.Spec.NodeSelector[hostnameLabel] = e.cands[0].Labels[hostnameLabel]
			})
		}
		return err
	}}, func() { e.reconcile() })
	e.reconcile() // Re-observation must not reuse the old ReplicaSet before revision metadata catches up.
	d := e.liveDeployment()
	if d.Spec.Template.Annotations["opsloop.io/attempt-id"] != "" || d.Spec.Template.Spec.Containers[0].Image != "demo-api:v2" || d.Spec.Template.Spec.NodeSelector[hostnameLabel] != e.cands[0].Labels[hostnameLabel] {
		t.Fatalf("old failure changed externally replaced template: %+v", d.Spec.Template)
	}
	var incidents v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
	if len(incidents.Items) != 0 || e.policy().Status.ActiveIncident != nil {
		t.Fatalf("old failure adopted new Deployment baseline: incidents=%d active=%+v", len(incidents.Items), e.policy().Status.ActiveIncident)
	}
}

// Each mutation happens after the original decision's Nodes were read. Checking
// again after reservation also covers retries of a durable Reserved attempt.
func TestExecutionNodeRevalidation(t *testing.T) {
	for _, boundary := range []string{"metrics", "reservation", "restart"} {
		for _, change := range []string{"uid", "hostname", "selector", "taint", "memory", "disk", "pid", "ready", "cordon", "arch", "capacity", "trigger_uid", "trigger_missing", "trigger_ready"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				e := newEnforce(t, candidateIP)
				mutated := false
				mutate := func() {
					if !mutated {
						mutated = true
						mutateExecutionNode(e, change)
					}
				}
				switch boundary {
				case "metrics":
					e.r.Prom = &astraQueryHook{API: e.r.Prom, once: mutate}
					e.reconcile()
				case "reservation":
					e.withFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						err := c.SubResource(sub).Update(ctx, obj, opts...)
						if inc, ok := obj.(*v1alpha1.RecoveryIncident); ok && lastReserved(inc) != nil && err == nil {
							mutate()
						}
						return err
					}}, func() { e.reconcile() })
				case "restart":
					e.crashBeforeUpdate()
					restarted := *e.r
					e.r = &restarted
					e.r.Prom = &astraQueryHook{API: e.r.Prom, once: mutate}
					e.reconcile()
				}
				if e.workloadChanged(e.liveDeployment()) {
					t.Fatal("changed workload despite invalidated execution Node")
				}
				inc := e.onlyIncident()
				if lastReserved(inc) != nil {
					t.Fatal("invalidated Node left the attempt Reserved")
				}
			})
		}
	}
}

func mutateExecutionNode(e *enforceFixture, change string) {
	e.t.Helper()
	n := &corev1.Node{}
	name := e.cands[0].Name
	if strings.HasPrefix(change, "trigger_") {
		name = e.ns + "-primary"
	}
	e.must(e.c.Get(e.ctx, client.ObjectKey{Name: name}, n))
	switch change {
	case "uid", "trigger_uid", "trigger_missing":
		e.must(e.c.Delete(e.ctx, n))
		if change == "trigger_missing" {
			return
		}
		status := n.Status
		n.ObjectMeta = metav1.ObjectMeta{Name: n.Name, Labels: n.Labels}
		e.must(e.c.Create(e.ctx, n))
		n.Status = status
		e.must(e.c.Status().Update(e.ctx, n))
	case "hostname":
		n.Labels[hostnameLabel] = "new-hostname"
		e.must(e.c.Update(e.ctx, n))
	case "selector":
		delete(n.Labels, testLabel)
		e.must(e.c.Update(e.ctx, n))
	case "arch":
		n.Labels["kubernetes.io/arch"] = "unsupported"
		e.must(e.c.Update(e.ctx, n))
	case "taint":
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "blocked", Effect: corev1.TaintEffectNoSchedule})
		e.must(e.c.Update(e.ctx, n))
	case "cordon":
		n.Spec.Unschedulable = true
		e.must(e.c.Update(e.ctx, n))
	case "ready", "trigger_ready":
		status := corev1.ConditionFalse
		if change == "trigger_ready" {
			status = corev1.ConditionTrue
		}
		e.setReady(n, status)
	case "capacity":
		n.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse("1m")
		e.must(e.c.Status().Update(e.ctx, n))
	default:
		conditions := map[string]corev1.NodeConditionType{"memory": corev1.NodeMemoryPressure, "disk": corev1.NodeDiskPressure, "pid": corev1.NodePIDPressure}
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: conditions[change], Status: corev1.ConditionTrue})
		e.must(e.c.Status().Update(e.ctx, n))
	}
}

func TestExecutionNodeReadErrors(t *testing.T) {
	for _, kind := range []string{"trigger", "candidate", "pods"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
			e.must(err)
			e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Node); ok && ((kind == "trigger" && key.Name == e.ns+"-primary") || (kind == "candidate" && key.Name == e.cands[0].Name)) {
						return errors.New("injected Node read failure")
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok && kind == "pods" {
						return errors.New("injected Pod read failure")
					}
					return c.List(ctx, list, opts...)
				},
			})
			// A reserved read failure may race a delayed worker. The new contract
			// confirms a hold and live settlement, rather than retrying its action.
			e.reconcile()
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("read failure authorized workload change")
			}
			requireSafetyHeld(t, e, false)
			e.r.APIReader = e.c
			e.reconcile()
			if e.workloadChanged(e.liveDeployment()) || len(e.onlyIncident().Status.Runtime.Attempts) != 1 {
				t.Fatal("hold recovery created another attempt")
			}

		})
	}
}

func TestObservationDeploymentChanges(t *testing.T) {
	for _, change := range []string{"template", "spec", "uid", "status", "metadata"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			injected := false
			e.withFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				err := c.List(ctx, list, opts...)
				if _, ok := list.(*corev1.PodList); ok && !injected {
					injected = true
					d := e.liveDeployment()
					switch change {
					case "template":
						d.Spec.Template.Annotations = map[string]string{"external-change": "v2"}
						e.must(e.c.Update(e.ctx, d))
					case "spec":
						d.Spec.MinReadySeconds++
						e.must(e.c.Update(e.ctx, d))
					case "uid":
						e.must(e.c.Delete(e.ctx, d))
						d.ObjectMeta = metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, Annotations: d.Annotations}
						e.must(e.c.Create(e.ctx, d))
					case "status":
						d.Status.ObservedGeneration = d.Generation
						e.must(e.c.Status().Update(e.ctx, d))
					case "metadata":
						d.Labels = map[string]string{"external": "updated"}
						e.must(e.c.Update(e.ctx, d))
					}
				}
				return err
			}}, func() { e.reconcile() })
			var incidents v1alpha1.RecoveryIncidentList
			e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
			if change == "status" || change == "metadata" {
				if len(incidents.Items) != 1 || lastAttempt(&incidents.Items[0]).Outcome != recovery.OutcomePatched {
					t.Fatalf("non-workload change rejected: %+v", incidents.Items)
				}
			} else if len(incidents.Items) != 0 || e.policy().Status.ActiveIncident != nil {
				t.Fatal("stale observation reserved an Incident")
			}
		})
	}
}

func TestMonitorReplicaSetTemplateConsistency(t *testing.T) {
	for _, change := range []string{"generated_hash", "template_annotation", "revision"} {
		t.Run(change, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			rs := &appsv1.ReplicaSet{}
			e.must(e.c.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "demo-api-1"}, rs))
			switch change {
			case "generated_hash":
				rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "controller-generated"
			case "template_annotation":
				rs.Spec.Template.Annotations = map[string]string{"external": "different"}
			case "revision":
				rs.Annotations["deployment.kubernetes.io/revision"] = "different"
			}
			e.must(e.c.Update(e.ctx, rs))
			e.reconcile()
			var incidents v1alpha1.RecoveryIncidentList
			e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
			if change == "generated_hash" {
				if len(incidents.Items) != 1 || lastAttempt(&incidents.Items[0]).Outcome != recovery.OutcomePatched {
					t.Fatal("generated pod-template-hash blocked matching template")
				}
			} else if len(incidents.Items) != 0 {
				t.Fatal("different ReplicaSet template/revision supplied a trigger")
			}
		})
	}
}

// Characterizes the remaining cross-object race without sleeps: a Node update
// after its final GET snapshot can precede the Deployment CAS. No atomic Node
// eligibility guarantee is claimed by these live checks.
func TestExecutionNodeChangeAfterFinalGETBoundary(t *testing.T) {
	e := newEnforce(t, candidateIP)
	wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
	e.must(err)
	injected := false
	e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*corev1.Node); ok && key.Name == e.cands[0].Name && err == nil && !injected {
			injected = true
			mutateExecutionNode(e, "cordon")
		}
		return err
	}})
	e.reconcile()
	if !injected || !e.workloadChanged(e.liveDeployment()) {
		t.Fatal("expected the documented post-GET Node race to remain observable")
	}
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
}

func TestObservationReservationUIDSurvivesRestart(t *testing.T) {
	e := newEnforce(t, candidateIP)
	originalUID := e.deploy.UID
	e.withFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*v1alpha1.RecoveryIncident); ok {
			return errors.New("crash after Policy reservation before Incident creation")
		}
		return c.Create(ctx, obj, opts...)
	}}, func() {
		if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: e.ns, Name: policyName}}); err == nil {
			t.Fatal("reservation crash not injected")
		}
	})
	d := e.liveDeployment()
	e.must(e.c.Delete(e.ctx, d))
	d.ObjectMeta = metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace, Annotations: d.Annotations}
	e.must(e.c.Create(e.ctx, d))
	restarted := *e.r
	e.r = &restarted
	e.reconcile()
	if e.workloadChanged(e.liveDeployment()) {
		t.Fatal("old reservation executed against replacement Deployment")
	}
	inc := e.onlyIncident()
	if inc.Spec.Workload.UID != string(originalUID) {
		t.Fatalf("reservation adopted new UID %q, want original %q", inc.Spec.Workload.UID, originalUID)
	}
	wantPhase(t, inc, PhaseSuperseded, ReasonWorkloadReplaced)
}

func TestObservationLegacyPendingReservation(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped=%v", stopped), func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			e.reserveOnly()
			p := e.policy()
			p.Status.ActiveIncident.WorkloadUID = ""
			e.must(e.c.Status().Update(e.ctx, p))
			if stopped {
				p.Spec.Enabled = false
				e.must(e.c.Update(e.ctx, p))
			}
			restarted := *e.r
			e.r = &restarted
			e.reconcile()
			var incidents v1alpha1.RecoveryIncidentList
			e.must(e.c.List(e.ctx, &incidents, client.InNamespace(e.ns)))
			if len(incidents.Items) != 0 || e.policy().Status.ActiveIncident != nil || e.workloadChanged(e.liveDeployment()) {
				t.Fatal("legacy pending reservation was executed or retained")
			}
			if stopped {
				cond := meta.FindStatusCondition(e.policy().Status.Conditions, CondQuiesced)
				if cond == nil || cond.Status != metav1.ConditionTrue {
					t.Fatalf("legacy cleanup prevented fenced stop: %+v", cond)
				}
			} else {
				cond := meta.FindStatusCondition(e.policy().Status.Conditions, CondMonitoring)
				if cond == nil || cond.Reason != ReasonReservationUIDMissing {
					t.Fatalf("legacy discard reason missing: %+v", cond)
				}
				e.reconcile()
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
			}
		})
	}
}

func TestObservationLegacyConfirmedReservation(t *testing.T) {
	e := newEnforce(t, candidateIP)
	e.crashBeforeUpdate()
	p := e.policy()
	p.Status.ActiveIncident.WorkloadUID = ""
	e.must(e.c.Status().Update(e.ctx, p))
	restarted := *e.r
	e.r = &restarted
	e.reconcile()
	e.wantOutcomes(e.onlyIncident(), recovery.OutcomePatched)
	if e.onlyIncident().Spec.Workload.UID != string(e.deploy.UID) {
		t.Fatal("confirmed immutable workload UID changed")
	}
}

// Legacy unconfirmed reservations cannot distinguish an original Incident UID
// from a same-name replacement, so transition cleanup leaves its status intact.
func TestObservationLegacyPendingExistingIncident(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			p := e.reserveOnly()
			inc := incidentFor(p, e.liveDeployment())
			if foreign {
				inc.Spec.PolicyRef.UID = "different-policy-uid"
			}
			e.must(e.c.Create(e.ctx, inc))
			p.Status.ActiveIncident.WorkloadUID = ""
			e.must(e.c.Status().Update(e.ctx, p))
			originalRV := inc.ResourceVersion
			e.reconcile()
			got := e.onlyIncident()
			if got.UID != inc.UID || got.ResourceVersion != originalRV || got.Status.Runtime.Phase != "" {
				t.Fatal("legacy transition changed unconfirmed existing Incident")
			}
			if e.policy().Status.ActiveIncident != nil || e.workloadChanged(e.liveDeployment()) {
				t.Fatal("legacy transition executed or retained pending intent")
			}
			condition := meta.FindStatusCondition(e.policy().Status.Conditions, CondMonitoring)
			if condition == nil || !strings.Contains(condition.Message, "cleanup blocked pending UID verification") {
				t.Fatalf("unconfirmed orphan cleanup boundary missing: %+v", condition)
			}
		})
	}
}

func TestObservationReservationUIDMismatchNotConfirmed(t *testing.T) {
	e := newEnforce(t, candidateIP)
	p := e.reserveOnly()
	inc := incidentFor(p, e.liveDeployment())
	inc.Spec.Workload.UID = "replacement-uid"
	e.must(e.c.Create(e.ctx, inc))
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: e.ns, Name: policyName}}); err == nil {
		t.Fatal("Incident with different workload UID was confirmed")
	}
	if e.policy().Status.ActiveIncident.IncidentUID != "" || e.onlyIncident().ResourceVersion != inc.ResourceVersion || e.workloadChanged(e.liveDeployment()) {
		t.Fatal("mismatched Incident changed or executed")
	}
}

func TestExecutionNodeReadTimeBoundary(t *testing.T) {
	for _, kind := range []string{"metric_expiry", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnforce(t, candidateIP)
			wc, err := client.NewWithWatch(startEnv(t).Config, client.Options{Scheme: e.c.Scheme()})
			e.must(err)
			e.r.APIReader = interceptor.NewClient(wc, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if _, ok := obj.(*corev1.Node); ok && key.Name == e.cands[0].Name && err == nil {
					if kind == "metric_expiry" {
						e.now = e.now.Add(e.policy().Spec.Filter.MaxSampleAge.Duration)
					} else {
						e.now = e.onlyIncident().Spec.Deadline.Time
					}
				}
				return err
			}})
			e.reconcile()
			if e.workloadChanged(e.liveDeployment()) {
				t.Fatal("final Node I/O crossed expiry/deadline and executed")
			}
			if kind == "metric_expiry" {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeReserved)
			} else {
				e.wantOutcomes(e.onlyIncident(), recovery.OutcomeCancelled)
				requireSafetyHeld(t, e, false)
			}
		})
	}
}
