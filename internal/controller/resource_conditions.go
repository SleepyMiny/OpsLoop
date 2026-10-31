package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
)

// BindResourceConditions requires the same trusted scope as workload execution.
// Loading a condition is never a substitute for a fresh process-bound grant.
func BindResourceConditions(scope ExecutionScope, c *candidates.ResourceConditions) error {
	if c == nil {
		return nil
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if scope == (ExecutionScope{}) || scope.RunID != c.RunID || scope.Epoch != c.Epoch || scope.PolicyUID != c.PolicyUID || scope.DeploymentUID != c.DeploymentUID {
		return errors.New("resource conditions: trusted execution scope mismatch")
	}
	return nil
}

func (r *PolicyReconciler) bindResourceInput(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident, in *candidates.Input) error {
	config := r.ResourceConditions
	if authority, ok := r.Authority.(*ExhibitionAuthority); ok && p.Spec.Mode == v1alpha1.PolicyModeEnforce {
		var err error
		config, err = authority.ResourceConditions(p, inc)
		if err != nil {
			return err
		}
	}
	if config == nil {
		return nil
	}
	// An experiment must not alter or block another Policy's ordinary evaluation.
	// Execution authority is still checked independently before any dispatch.
	if string(p.UID) != config.PolicyUID {
		return nil
	}
	if err := r.authorizeExecution(p, inc); err != nil {
		return err
	}
	grant, err := decodeExecutionGrant(p)
	if err != nil {
		return err
	}
	if err = BindResourceConditions(grant.Scope, config); err != nil {
		return err
	}
	c := config
	if c.NotBefore.Before(grant.NotBefore) || c.ExpiresAt.After(grant.NotAfter) {
		return errors.New("resource conditions: outside approved grant window")
	}
	in.RunID, in.Epoch, in.PolicyUID = grant.Scope.RunID, grant.Scope.Epoch, string(p.UID)
	in.ResourceConditions = c
	return candidates.ValidateResourceInput(*in)
}

func resourceDecisionRecorded(actual, expected v1alpha1.RecoveryDecision) bool {
	// Compare the JSON representation to tolerate normal metav1 time encoding
	// while detecting pruning/rewriting of the complete candidate evidence.
	a, errA := json.Marshal(actual.Candidates)
	b, errB := json.Marshal(expected.Candidates)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func resourceEvidenceBound(dec v1alpha1.RecoveryDecision, config *candidates.ResourceConditions) bool {
	if len(dec.Candidates) == 0 {
		return false
	}
	for _, c := range dec.Candidates {
		evidence := c.ResourceCondition
		if evidence == nil || evidence.RunID != config.RunID || evidence.Epoch != config.Epoch || evidence.PolicyUID != config.PolicyUID || evidence.ConfigSHA256 != config.SHA256() || evidence.ExpiresAt.Unix() != config.ExpiresAt.Unix() {
			return false
		}
	}
	return true
}
