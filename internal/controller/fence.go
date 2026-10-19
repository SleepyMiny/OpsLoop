package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	FenceAnnotation = "opsloop.io/execution-fence"
	CondQuiesced    = "Quiesced"
)

// executionFence is durable ownership and a monotonically increasing generation
// floor. It lives on Deployment metadata so closing it does not start a rollout.
// All participating writers preserve it and compare-and-swap the same Deployment RV.
type executionFence struct {
	Version                 int         `json:"version"`
	PolicyUID               string      `json:"policyUID"`
	PolicyName              string      `json:"policyName"`
	DeploymentUID           string      `json:"deploymentUID"`
	ClosedThroughGeneration int64       `json:"closedThroughGeneration"`
	SafetyHold              *safetyHold `json:"safetyHold,omitempty"`
}

// safetyHold is durable authority, unlike the Incident's retry/diagnostic status.
// No normal policy edit, Node recovery or restart removes it.
type safetyHold struct {
	HoldID           string `json:"holdID"`
	IncidentUID      string `json:"incidentUID"`
	ActionID         string `json:"actionID"`
	PolicyGeneration int64  `json:"policyGeneration"`
	Reason           string `json:"reason"`
}

func readFence(d *appsv1.Deployment) (*executionFence, error) {
	if d == nil {
		return nil, nil
	}
	raw, present := d.Annotations[FenceAnnotation]
	if !present {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, fmt.Errorf("execution fence exceeds bounded size")
	}
	if err := uniqueJSONFields(json.NewDecoder(strings.NewReader(raw))); err != nil {
		return nil, fmt.Errorf("invalid execution fence: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"version", "policyUID", "policyName", "deploymentUID", "closedThroughGeneration"} {
		if value, ok := fields[key]; !ok || string(value) == "null" {
			return nil, fmt.Errorf("execution fence missing %s", key)
		}
	}
	for key := range fields {
		if key != "version" && key != "policyUID" && key != "policyName" && key != "deploymentUID" && key != "closedThroughGeneration" && key != "safetyHold" {
			return nil, fmt.Errorf("unknown execution fence field %s", key)
		}
	}
	var f executionFence
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("invalid execution fence: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid execution fence trailing data")
	}
	if (f.Version != 1 && f.Version != 2) || f.PolicyUID == "" || f.PolicyName == "" || f.DeploymentUID == "" || f.ClosedThroughGeneration < 0 {
		return nil, fmt.Errorf("invalid execution fence identity, version or generation")
	}
	if hold, ok := fields["safetyHold"]; ok {
		if f.Version != 2 || string(hold) == "null" {
			return nil, fmt.Errorf("invalid safetyHold presence")
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(hold, &keys); err != nil {
			return nil, err
		}
		for _, key := range []string{"holdID", "incidentUID", "actionID", "policyGeneration", "reason"} {
			if _, ok := keys[key]; !ok {
				return nil, fmt.Errorf("safety hold missing %s", key)
			}
		}
		for key := range keys {
			if key != "holdID" && key != "incidentUID" && key != "actionID" && key != "policyGeneration" && key != "reason" {
				return nil, fmt.Errorf("unknown safety hold field %s", key)
			}
		}
	}
	if len(f.PolicyUID) > 128 || len(f.PolicyName) > 253 || len(f.DeploymentUID) > 128 {
		return nil, fmt.Errorf("execution fence identity too long")
	}
	if f.SafetyHold != nil {
		h := f.SafetyHold
		if f.Version != 2 || h.HoldID == "" || len(h.HoldID) > 256 || h.IncidentUID == "" || len(h.IncidentUID) > 128 || h.ActionID == "" || len(h.ActionID) > 128 || h.PolicyGeneration <= 0 || h.PolicyGeneration > f.ClosedThroughGeneration || h.Reason == "" || len(h.Reason) > 256 {
			return nil, fmt.Errorf("invalid safety hold identity, generation or reason (generation=%d floor=%d holdID=%d incidentUID=%d actionID=%d reason=%d)", h.PolicyGeneration, f.ClosedThroughGeneration, len(h.HoldID), len(h.IncidentUID), len(h.ActionID), len(h.Reason))
		}
	}
	return &f, nil
}

func ownedFence(f *executionFence, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment) bool {
	return f != nil && f.PolicyUID == string(p.UID) && f.PolicyName == p.Name && f.DeploymentUID == string(d.UID)
}

func putFence(d *appsv1.Deployment, f *executionFence) string {
	b, _ := json.Marshal(f)
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[FenceAnnotation] = string(b)
	return string(b)
}

// claimExecutionFence never refreshes a conflicting RV. A later reconcile must
// read the Deployment again, then reauthorize against a freshly read Policy.
func (x deploymentExecution) claimExecutionFence(ctx context.Context, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment) (bool, error) {
	f, err := readFence(d)
	if err != nil {
		return false, err
	}
	if f != nil {
		if !ownedFence(f, p, d) {
			return false, fmt.Errorf("execution fence ownership collision")
		}
		if f.SafetyHold != nil {
			return false, fmt.Errorf("deployment has a persistent safety hold")
		}
		if p.Generation <= f.ClosedThroughGeneration {
			return false, fmt.Errorf("policy generation %d is fenced through %d", p.Generation, f.ClosedThroughGeneration)
		}
		if f.Version == 1 {
			f.Version = 2
			putFence(d, f)
			if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}
	if p.UID == "" || d.UID == "" || d.DeletionTimestamp != nil {
		return false, fmt.Errorf("execution fence requires live object identities")
	}
	putFence(d, &executionFence{Version: 2, PolicyUID: string(p.UID), PolicyName: p.Name, DeploymentUID: string(d.UID)})
	if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
		return false, err
	}
	return true, nil
}

func stopped(p *v1alpha1.RecoveryPolicy) bool {
	return !p.Spec.Enabled || p.Spec.Mode != v1alpha1.PolicyModeEnforce
}

func setQuiesced(p *v1alpha1.RecoveryPolicy, status metav1.ConditionStatus, reason, msg string, now metav1.Time) bool {
	return meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: CondQuiesced, Status: status, Reason: reason, Message: msg, ObservedGeneration: p.Generation, LastTransitionTime: now})
}

// closeStop serializes with every action on the Deployment. A raw Policy write
// is only a request; only a successful close CAS can exclude a delayed old write.
func (x deploymentExecution) closeStop(ctx context.Context, p *v1alpha1.RecoveryPolicy) (*appsv1.Deployment, *executionFence, error) {
	d, err := x.r.liveDeployment(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, fmt.Errorf("stop target Deployment is absent; no fence acknowledgement")
	}
	f, err := readFence(d)
	if err != nil {
		return nil, nil, err
	}
	if f != nil && !ownedFence(f, p, d) {
		return nil, nil, fmt.Errorf("execution fence ownership collision")
	}
	if f == nil && p.Spec.Mode != v1alpha1.PolicyModeEnforce {
		// From-start ObserveOnly must work with read-only Deployment RBAC. A
		// reservation of an earlier Enforce snapshot requires the strong close.
		enforce := false
		if a := p.Status.ActiveIncident; a != nil {
			inc := &v1alpha1.RecoveryIncident{}
			err := x.r.APIReader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: a.Name}, inc)
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, nil, err
			}
			enforce = err == nil && inc.Spec.PolicyRef.UID == string(p.UID) && inc.Spec.Policy.Mode == v1alpha1.PolicyModeEnforce
		}
		if !enforce {
			return d, nil, nil
		}
	}
	current := &v1alpha1.RecoveryPolicy{}
	if err := x.r.APIReader.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
		return nil, nil, err
	}
	if current.UID != p.UID || current.Generation != p.Generation || !stopped(current) || current.DeletionTimestamp != nil {
		return nil, nil, fmt.Errorf("stop policy identity or generation changed")
	}
	if f == nil {
		f = &executionFence{Version: 2, PolicyUID: string(p.UID), PolicyName: p.Name, DeploymentUID: string(d.UID)}
	}
	if f.Version == 1 || f.ClosedThroughGeneration < p.Generation {
		f.Version = 2
		f.ClosedThroughGeneration = max(f.ClosedThroughGeneration, p.Generation)
		putFence(d, f)
		if err := x.r.Update(ctx, d, client.FieldOwner(fieldOwner)); err != nil {
			return nil, nil, err
		}
	}
	return d, f, nil
}

func (r *PolicyReconciler) claimExecutionFence(ctx context.Context, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment) (bool, error) {
	return (deploymentExecution{r}).claimExecutionFence(ctx, p, d)
}
func (r *PolicyReconciler) closeStop(ctx context.Context, p *v1alpha1.RecoveryPolicy) (*appsv1.Deployment, *executionFence, error) {
	return (deploymentExecution{r}).closeStop(ctx, p)
}

// reconcileStop publishes True only after the fence and active reservation are
// settled. Status conflicts and lost close responses are retried by observation,
// never by replaying a workload mutation on a refreshed RV.
func (r *PolicyReconciler) reconcileStop(ctx context.Context, p *v1alpha1.RecoveryPolicy) (ctrl.Result, error) {
	now := r.Now()
	hadActive := p.Status.ActiveIncident != nil
	_, f, err := r.closeStop(ctx, p)
	if err != nil {
		if setQuiesced(p, metav1.ConditionUnknown, "FenceUnconfirmed", err.Error(), metav1.NewTime(now)) {
			err = errors.Join(err, r.writePolicy(ctx, p))
		}
		return requeueOnConflict(err)
	}
	if hadActive {
		result, err := r.reconcileActive(ctx, p, now)
		if err != nil || result.RequeueAfter == conflictRequeue {
			return result, err
		}
	}
	current := &v1alpha1.RecoveryPolicy{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
		return ctrl.Result{}, err
	}
	if current.UID != p.UID || current.Generation != p.Generation || !stopped(current) {
		return ctrl.Result{RequeueAfter: conflictRequeue}, nil
	}
	// Active cleanup may have added a safety hold. Refresh the full live
	// proof after cleanup rather than acknowledging the earlier close snapshot.
	_, f, err = r.closeStop(ctx, current)
	if err != nil {
		if setQuiesced(current, metav1.ConditionUnknown, "FenceUnconfirmed", err.Error(), metav1.NewTime(now)) {
			err = errors.Join(err, r.writePolicy(ctx, current))
		}
		return requeueOnConflict(err)
	}
	status, reason, msg := metav1.ConditionUnknown, "FenceNotEstablished", "ObserveOnly without an execution fence; no strong stop acknowledgement"
	if f != nil && current.Status.ActiveIncident == nil {
		status, reason = metav1.ConditionTrue, "FenceClosed"
		b, _ := json.Marshal(f)
		msg = string(b)
	} else if f != nil {
		status, reason, msg = metav1.ConditionFalse, "ReservationPending", "execution fence closed; active incident cleanup is pending"
	}
	changed := setQuiesced(current, status, reason, msg, metav1.NewTime(now))
	if !current.Spec.Enabled {
		changed = setCondition(current, CondDisabled, "", now) || changed
	}
	if changed {
		if err := r.writePolicy(ctx, current); err != nil {
			return requeueOnConflict(err)
		}
	}
	if !hadActive && current.Status.ActiveIncident == nil && current.Spec.Enabled {
		// Continue the existing ObserveOnly monitoring path without Deployment writes.
		return r.reconcileMonitoring(ctx, current, now)
	}
	return ctrl.Result{RequeueAfter: monitorPeriod}, nil
}

// encoding/json otherwise accepts duplicate keys and case-insensitive aliases.
// Authority metadata rejects both instead of choosing one interpretation.
func uniqueJSONFields(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON field")
			}
			seen[name] = true
			if err := uniqueJSONFields(dec); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for dec.More() {
			if err := uniqueJSONFields(dec); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}
