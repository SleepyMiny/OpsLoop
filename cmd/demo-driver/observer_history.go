package main

import (
	"context"
	"encoding/json"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

// The manifest is a diagnostic ledger, not an acceptance API. Each occurrence
// retains its capture location even when later observations establish a proof.
type observerDiagnostic struct {
	Reason   string `json:"reason"`
	Class    string `json:"class"` // pending, gap, contradiction, fatal, excluded
	Target   string `json:"target"`
	Observed Stamp  `json:"observed"`
	Ref      string `json:"ref"`
	Proof    string `json:"proof"`
}
type observerDeploymentRead struct {
	Projection deploymentObservation `json:"projection"`
	Observed   Stamp                 `json:"observed"`
	Ref        string                `json:"ref"`
}

const observerDeploymentHistoryLimit = 128

func (o *observer) rememberDeployment(d deploymentObservation, at Stamp, ref string) {
	o.deployments = append(o.deployments, observerDeploymentRead{d, at, ref})
	if p := o.initialDeployment; p != nil && (d.SpecHash != p.SpecHash || d.TemplateHash != p.TemplateHash || d.Attempt != p.Attempt || d.Hostname != p.Hostname) {
		o.desiredReads = append(o.desiredReads, observerDeploymentRead{d, at, ref})
	}
	if len(o.deployments) > observerDeploymentHistoryLimit {
		o.deployments = o.deployments[1:]
	}
}
func (o *observer) precondition(a v1.Attempt, post Stamp) *observerDeploymentRead {
	o.preCandidates = nil
	for _, p := range o.deployments {
		d := p.Projection
		if o.before(p.Observed, post) && d.Metadata.ResourceVersion == a.PreResourceVersion && string(d.Metadata.UID) == o.evidence.Identity.DeploymentUID && d.TemplateHash == a.PreTemplateHash && o.initialDeployment != nil && d.SpecHash == o.initialDeployment.SpecHash && d.Attempt == o.initialDeployment.Attempt && d.Hostname == o.initialDeployment.Hostname {
			o.preCandidates = append(o.preCandidates, p)
		}
	}
	if len(o.preCandidates) == 0 {
		return nil
	}
	// Receipts arrive only at collection end. Preserve the bounded candidates
	// now so the actual before-patch clock comparison can select one then.
	return &o.preCandidates[len(o.preCandidates)-1]
}
func (o *observer) checkDesired(a v1.Attempt, d deploymentObservation, at Stamp, ref string) {
	if o.initialDeployment == nil {
		return
	}
	pre := o.initialDeployment
	target := d.TemplateHash == a.TargetTemplateHash && d.Attempt == a.ActionID && d.Hostname == a.Hostname
	baseline := d.TemplateHash == a.PreTemplateHash && d.Attempt == pre.Attempt && d.Hostname == pre.Hostname && !stampValid(o.evidence.Apply.Observed)
	if d.SpecHash != pre.SpecHash || (!target && !baseline) {
		o.r.diagnose("deployment-desired-state-contradiction", "contradiction", o.apiPath("deployments", o.c.Workload), at, ref, "apply")
	}
}

type observerStopRead struct {
	Ref           string     `json:"-"`
	PolicyAt      Stamp      `json:"policyAt"`
	DeploymentAt  Stamp      `json:"deploymentAt"`
	PolicyRef     string     `json:"policyRef"`
	DeploymentRef string     `json:"deploymentRef"`
	Proof         *StopProof `json:"proof"`
}

func (o *observer) observeStop(ctx context.Context, p v1.RecoveryPolicy, d deploymentObservation, pok, dok bool, pat, dat Stamp, pref, dref string) error {
	s := observerStopRead{PolicyAt: pat, DeploymentAt: dat, PolicyRef: pref, DeploymentRef: dref}
	if pok && dok {
		at, err := o.r.stamp(ctx)
		if err != nil {
			return err
		}
		enabled, deleting, absent := p.Spec.Enabled, p.DeletionTimestamp != nil, p.Status.ActiveIncident == nil
		proof := &StopProof{PolicyUID: string(p.UID), Generation: p.Generation, Enabled: &enabled, Mode: string(p.Spec.Mode), Deleting: &deleting, ActiveIncidentAbsent: &absent, Acknowledged: at, Ref: dref}
		if d.Fence != nil {
			proof.LiveFence = *d.Fence
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Quiesced" {
				proof.QuiescedStatus = string(c.Status)
				proof.QuiescedReason = c.Reason
				proof.ObservedGeneration = c.ObservedGeneration
				_ = json.Unmarshal([]byte(c.Message), &proof.AcknowledgedFence)
			}
		}
		s.Proof = proof
	}
	// Poll count is capped at 1000. Store each separate GET and aggregate event;
	// no single combined snapshot is claimed.
	ref, err := o.r.raw("stop-observation", "policy/deployment", dat, 0, "", s)
	if err != nil {
		return err
	}
	if s.Proof != nil {
		s.Proof.Ref = ref
	}
	s.Ref = ref
	o.stops = append(o.stops, s)
	return nil
}
func (o *observer) validStop(s *StopProof) bool {
	if s == nil {
		return false
	}
	id := o.evidence.Identity
	f := s.LiveFence
	return s.PolicyUID == id.PolicyUID && s.Generation > id.PolicyGeneration && s.ObservedGeneration == s.Generation && (falseValue(s.Enabled) || s.Mode == modeObserveOnly) && (s.Mode == modeEnforce || s.Mode == modeObserveOnly) && falseValue(s.Deleting) && trueValue(s.ActiveIncidentAbsent) && s.QuiescedStatus == "True" && s.QuiescedReason == "FenceClosed" && f == s.AcknowledgedFence && f.Version == 2 && f.PolicyUID == id.PolicyUID && f.PolicyName == o.c.Policy && f.DeploymentUID == id.DeploymentUID && f.ClosedThroughGeneration >= s.Generation
}
func (o *observer) before(a, b Stamp) bool {
	ok, known := ordered(o.evidence, a, b)
	return ok && known
}
func (o *observer) validateReceiptHorizon() {
	h := o.r.manifest.CollectionHorizon
	reject := func(at Stamp, ref, reason, proof string) bool {
		if o.before(at, h) {
			return false
		}
		o.r.diagnose(reason, "gap", "receipt", at, ref, proof)
		return true
	}
	if c := o.evidence.Cleanup; c != nil && reject(c.Completed, c.Ref, "cleanup-not-before-collection-horizon", "cleanup") {
		o.evidence.Cleanup = nil
	}
	coverage := make([]ActorCoverage, 0, len(o.evidence.Coverage))
	for _, c := range o.evidence.Coverage {
		if !reject(c.End, c.Ref, "coverage-not-before-collection-horizon", "attribution") {
			coverage = append(coverage, c)
		}
	}
	o.evidence.Coverage = coverage
}
func (o *observer) selectStop() {
	req := o.receipts.StopRequested
	if req == nil {
		return
	}
	var selected *StopProof
	blocked := false
	var counter *StopProof
	for _, s := range o.stops {
		// Both GETs must actually follow this request. Aggregate stamp alone is
		// insufficient. A later receipt request can select a new episode.
		if !o.before(*req, s.PolicyAt) || !o.before(*req, s.DeploymentAt) {
			continue
		}
		if o.evidence.Cleanup != nil {
			c := o.evidence.Cleanup.Completed
			// A read definitely after cleanup is outside this proof's interval.
			if o.before(c, s.PolicyAt) && o.before(c, s.DeploymentAt) {
				continue
			}
			if !o.before(s.PolicyAt, c) || !o.before(s.DeploymentAt, c) || (s.Proof != nil && !o.before(s.Proof.Acknowledged, c)) {
				if selected != nil {
					blocked = true
					o.r.diagnose("stop-cleanup-boundary-unproven", "gap", "policy/deployment", s.DeploymentAt, s.Ref, "cleanup")
				}
				continue
			}
		}
		valid := o.validStop(s.Proof)
		if selected == nil {
			if valid {
				selected = s.Proof
			}
			continue // stale-first may recover only before a valid ack
		}
		if !valid || s.Proof.Generation != selected.Generation || s.Proof.LiveFence != selected.LiveFence {
			blocked = true
			if counter == nil && s.Proof != nil {
				p := s.Proof
				// Retain a real counterexample when the current schema can
				// express FAIL. Other cases remain STOP_PROOF_MISSING.
				if (p.PolicyUID == o.evidence.Identity.PolicyUID && p.Generation > o.evidence.Identity.PolicyGeneration && p.ObservedGeneration == p.Generation && (falseValue(p.ActiveIncidentAbsent) || p.QuiescedStatus == "False")) || (p.ObservedGeneration > 0 && p.ObservedGeneration != p.Generation) || (p.LiveFence.Version > 0 && p.LiveFence.PolicyUID != "" && p.LiveFence.DeploymentUID != "" && p.LiveFence.ClosedThroughGeneration > 0 && p.LiveFence != p.AcknowledgedFence) {
					counter = p
				}
			}
			class := "contradiction"
			if s.Proof == nil {
				class = "gap"
			}
			o.r.diagnose("stop-validity-contradiction-or-gap", class, "policy/deployment", s.DeploymentAt, s.Ref, "cleanup")
		}
	}
	if blocked && counter != nil {
		copy := *counter
		copy.Requested = *req
		o.evidence.Stop = &copy
		return
	}
	if selected != nil && !blocked {
		copy := *selected
		copy.Requested = *req
		o.evidence.Stop = &copy
	}
}
func (o *observer) selectProofs(env Env) {
	o.selectStop()
	// Ask the unchanged checker which Service interval these captured samples
	// select. This provisional evaluation never writes or edits a summary.
	in := ReportInput{Probe: o.probes, Incident: o.incident, Pods: o.pods, Env: env, InjectAt: o.evidence.FaultRequested.At, Mode: modeEnforce, Scenario: "runtime-observer", ExpectNode: o.c.ExpectNode, ExpectVersion: o.c.ExpectVersion, PlatformDigests: o.c.PlatformDigests}
	stable := Acceptance(in, o.evidence).ServiceStableAt
	invalid := false
	if o.selectedPre != nil {
		captured := o.selectedPre
		o.selectedPre = nil
		for i := len(o.preCandidates) - 1; i >= 0; i-- {
			p := &o.preCandidates[i]
			if o.before(p.Observed, o.evidence.Patched) {
				o.selectedPre = p
				break
			}
		}
		if o.selectedPre == nil {
			invalid = true
			o.r.diagnose("precondition-not-before-patch", "contradiction", o.apiPath("deployments", o.c.Workload), captured.Observed, captured.Ref, "apply")
		} else {
			p := o.selectedPre
			o.r.diagnose("precondition-selected", "link", o.apiPath("deployments", o.c.Workload), p.Observed, p.Ref, "apply-precondition")
		}
	}
	if len(o.incident.Status.Runtime.Attempts) == 1 && o.initialDeployment != nil {
		a := o.incident.Status.Runtime.Attempts[0]
		if o.initialDeployment.TemplateHash != a.PreTemplateHash || o.initialDeployment.SpecHash != o.incident.Spec.Baseline.SpecHash {
			invalid = true
			o.r.diagnose("baseline-required-hash-mismatch", "contradiction", o.apiPath("deployments", o.c.Workload), o.firstDeployment.Observed, o.firstDeployment.Ref, "apply")
		}
		for _, read := range o.desiredReads {
			d := read.Projection
			target := d.TemplateHash == a.TargetTemplateHash && d.Attempt == a.ActionID && d.Hostname == a.Hostname
			if d.SpecHash != o.initialDeployment.SpecHash || !target || !o.before(o.evidence.Patched, read.Observed) {
				o.r.diagnose("deployment-history-desired-state-contradiction", "contradiction", o.apiPath("deployments", o.c.Workload), read.Observed, read.Ref, "apply")
			}
		}
	}
	for _, d := range o.r.manifest.Diagnostics {
		if d.Proof != "apply" || d.Class == "pending" || d.Class == "excluded" {
			continue
		}
		if d.Reason == "pre-apply-observation-mismatch" && o.evidence.Apply.Ref != "" {
			continue
		}
		// Gaps before fault can recover through actual baseline/apply/owner reads.
		// Gaps or contradictions through the selected stable end remain material.
		if d.Class == "gap" && o.before(d.Observed, o.evidence.FaultRequested) {
			continue
		}
		if stable != nil && o.before(*stable, d.Observed) {
			continue
		}
		invalid = true
	}
	if invalid {
		o.r.diagnose("apply-invalid-for-selected-interval", "excluded", "apply", o.evidence.Apply.Observed, o.evidence.Apply.Ref, "attribution")
		o.evidence.Apply.Source = ""
		o.evidence.Apply.Ref = ""
	}
}
