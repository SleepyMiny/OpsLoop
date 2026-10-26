package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const evidenceVersion = "demo-a-evidence/v1"
const acceptanceVersion = "demo-a-summary/v2"
const maxInputBytes = 16 << 20
const maxProbeLineBytes = 64 << 10
const maxProbeRecords = 10000

// Stamp preserves elapsed time across JSON round trips. Elapsed values belong
// only to one recorder/segment. UTC alone never supplies monotonic ordering.
type Stamp struct {
	Recorder  string    `json:"recorder"`
	Segment   string    `json:"segment"`
	Sequence  int64     `json:"sequence"`
	ElapsedMS *float64  `json:"elapsedMs"`
	At        time.Time `json:"at"`
}
type ClockBound struct {
	Recorder      string   `json:"recorder"`
	Segment       string   `json:"segment"`
	OffsetMS      float64  `json:"offsetMs"`
	UncertaintyMS *float64 `json:"uncertaintyMs"`
	Ref           string   `json:"ref"`
}
type ServiceIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	PathID    string `json:"pathID"`
	URL       string `json:"url"`
}
type RawSource struct {
	Role   string `json:"role"`
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}
type RunIdentity struct {
	PolicyUID        string `json:"policyUID"`
	PolicyGeneration int64  `json:"policyGeneration"`
	PolicyHash       string `json:"policyHash"`
	IncidentUID      string `json:"incidentUID"`
	IncidentSequence int64  `json:"incidentSequence"`
	ActionID         string `json:"actionID"`
	DeploymentUID    string `json:"deploymentUID"`
	NodeUID          string `json:"nodeUID"`
	PodUID           string `json:"podUID"`
}
type ApplyProof struct {
	DeploymentUID      string `json:"deploymentUID"`
	PreRV              string `json:"preRV"`
	PostRV             string `json:"postRV"`
	PreTemplateHash    string `json:"preTemplateHash"`
	TargetTemplateHash string `json:"targetTemplateHash"`
	AttemptAnnotation  string `json:"attemptAnnotation"`
	Hostname           string `json:"hostname"`
	Source             string `json:"source"`
	Ref                string `json:"ref"`
	Observed           Stamp  `json:"observed"`
}
type ProbeTiming struct {
	Start Stamp `json:"start"`
	End   Stamp `json:"end"`
	// service-failure means a completed Service request (including transport
	// timeout), never a recorder/serialization failure or a missing log line.
	Result string `json:"result"` // response|service-failure|recorder-error
}
type Intervention struct {
	Actor     string `json:"actor"`
	Kind      string `json:"kind"` // restore|desired-state
	TargetUID string `json:"targetUID"`
	Requested Stamp  `json:"requested"`
	Confirmed *Stamp `json:"confirmed"`
	Ref       string `json:"ref"`
}
type ActorCoverage struct {
	Actor    string `json:"actor"` // watchdog|admin|cleanup|desired-state
	Complete *bool  `json:"complete"`
	Start    Stamp  `json:"start"`
	End      Stamp  `json:"end"`
	Ref      string `json:"ref"`
}
type StopFence struct {
	Version                 int    `json:"version"`
	PolicyUID               string `json:"policyUID"`
	PolicyName              string `json:"policyName"`
	DeploymentUID           string `json:"deploymentUID"`
	ClosedThroughGeneration int64  `json:"closedThroughGeneration"`
}
type StopProof struct {
	PolicyUID            string    `json:"policyUID"`
	Generation           int64     `json:"generation"`
	Enabled              *bool     `json:"enabled"`
	Mode                 string    `json:"mode"`
	Deleting             *bool     `json:"deleting"`
	ActiveIncidentAbsent *bool     `json:"activeIncidentAbsent"`
	QuiescedStatus       string    `json:"quiescedStatus"`
	QuiescedReason       string    `json:"quiescedReason"`
	ObservedGeneration   int64     `json:"observedGeneration"`
	AcknowledgedFence    StopFence `json:"acknowledgedFence"`
	LiveFence            StopFence `json:"liveFence"`
	Requested            Stamp     `json:"requested"`
	Acknowledged         Stamp     `json:"acknowledged"`
	Ref                  string    `json:"ref"`
}
type CleanupProof struct {
	Status          string `json:"status"` // succeeded|failed|pending
	Completed       Stamp  `json:"completed"`
	WorkloadChecked *bool  `json:"workloadChecked"`
	OldPodsChecked  *bool  `json:"oldPodsChecked"`
	ServiceChecked  *bool  `json:"serviceChecked"`
	Ref             string `json:"ref"`
}
type RuntimeEvidence struct {
	Version        string          `json:"version"`
	EvidenceKind   string          `json:"evidenceKind"`
	RunID          string          `json:"runID"`
	Identity       RunIdentity     `json:"identity"`
	Service        ServiceIdentity `json:"service"`
	Sources        []RawSource     `json:"sources"`
	Clocks         []ClockBound    `json:"clocks"`
	FaultRequested Stamp           `json:"faultRequested"`
	Decision       Stamp           `json:"decision"`
	Patched        Stamp           `json:"patched"`
	Apply          ApplyProof      `json:"apply"`
	ReadySource    Stamp           `json:"readySource"`
	ReadyObserved  Stamp           `json:"readyObserved"`
	Probe          []ProbeTiming   `json:"probe"`
	Coverage       []ActorCoverage `json:"coverage"`
	Interventions  []Intervention  `json:"interventions"`
	Stop           *StopProof      `json:"stop"`
	Cleanup        *CleanupProof   `json:"cleanup"`
}
type Verdict struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}
type DurationResult struct {
	Seconds            *float64 `json:"seconds"`
	UncertaintySeconds *float64 `json:"uncertaintySeconds"`
	Reason             string   `json:"reason"`
}
type AcceptanceResult struct {
	SchemaVersion           string         `json:"schemaVersion"`
	Acceptance              string         `json:"acceptance"`
	EvidenceKind            string         `json:"evidenceKind"`
	RunID                   string         `json:"runID"`
	Scenario                string         `json:"scenario"`
	Identity                RunIdentity    `json:"identity"`
	ControllerOutcome       string         `json:"controllerOutcome"`
	ControllerReason        string         `json:"controllerReason"`
	ServiceRecovery         Verdict        `json:"serviceRecovery"`
	Attribution             Verdict        `json:"attribution"`
	Cleanup                 Verdict        `json:"cleanup"`
	OverallPass             bool           `json:"overallPass"`
	LiveProof               bool           `json:"liveProof"`
	FirstFailureAt          *Stamp         `json:"firstFailureAt"`
	ServiceFirstHealthyAt   *Stamp         `json:"serviceFirstHealthyAt"`
	ServiceStableAt         *Stamp         `json:"serviceStableAt"`
	ApplySource             string         `json:"applySource"`
	ServiceRecoveryDuration DurationResult `json:"serviceRecoveryDuration"`
	PlacementDuration       DurationResult `json:"placementDuration"`
	Limits                  []string       `json:"limits"`
}

func passVerdict() Verdict { return Verdict{Status: "PASS", Reasons: []string{}} }
func (v *Verdict) add(status, reason string) {
	if v.Status == "PASS" || status == "FAIL" {
		v.Status = status
	}
	if !slices.Contains(v.Reasons, reason) {
		v.Reasons = append(v.Reasons, reason)
	}
}
func trueValue(v *bool) bool  { return v != nil && *v }
func falseValue(v *bool) bool { return v != nil && !*v }

type clockKey struct{ recorder, segment string }

func lane(s Stamp) clockKey { return clockKey{s.Recorder, s.Segment} }
func stampValid(s Stamp) bool {
	_, offset := s.At.Zone()
	return s.Recorder != "" && s.Segment != "" && s.Sequence > 0 && s.ElapsedMS != nil && *s.ElapsedMS >= 0 && !math.IsInf(*s.ElapsedMS, 0) && !math.IsNaN(*s.ElapsedMS) && !s.At.IsZero() && offset == 0
}

// delta returns a duration only within a segment or with explicit clock bounds.
func delta(e RuntimeEvidence, a, b Stamp) DurationResult {
	unknown := DurationResult{Reason: "CLOCK_OR_SEGMENT_UNBOUNDED"}
	if !stampValid(a) || !stampValid(b) {
		return unknown
	}
	if lane(a) == lane(b) {
		d := (*b.ElapsedMS - *a.ElapsedMS) / 1000
		z := 0.0
		return DurationResult{Seconds: &d, UncertaintySeconds: &z}
	}
	bound := func(s Stamp) *ClockBound {
		for i := range e.Clocks {
			c := &e.Clocks[i]
			if c.Recorder == s.Recorder && c.Segment == s.Segment && c.UncertaintyMS != nil && c.Ref != "" {
				return c
			}
		}
		return nil
	}
	ca, cb := bound(a), bound(b)
	if ca == nil || cb == nil {
		return unknown
	}
	d := b.At.Sub(a.At).Seconds() + (cb.OffsetMS-ca.OffsetMS)/1000
	u := (*ca.UncertaintyMS + *cb.UncertaintyMS) / 1000
	return DurationResult{Seconds: &d, UncertaintySeconds: &u}
}
func ordered(e RuntimeEvidence, a, b Stamp) (bool, bool) {
	d := delta(e, a, b)
	if d.Seconds == nil {
		return false, false
	}
	if lane(a) == lane(b) {
		return a.Sequence < b.Sequence && *d.Seconds > 0, true
	}
	if *d.Seconds-*d.UncertaintySeconds > 0 {
		return true, true
	}
	if *d.Seconds+*d.UncertaintySeconds < 0 {
		return false, true
	}
	return false, false
}
func requireOrder(v *Verdict, e RuntimeEvidence, a, b Stamp, reason string) {
	ok, known := ordered(e, a, b)
	if !known {
		v.add("UNPROVEN", reason+"_CLOCK_UNPROVEN")
	} else if !ok {
		v.add("FAIL", reason+"_ORDER")
	}
}

// Acceptance checks consistency of supplied offline observations. It neither
// collects evidence nor authenticates its truth, even when kind is observed.
func Acceptance(in ReportInput, e RuntimeEvidence) AcceptanceResult {
	r := AcceptanceResult{SchemaVersion: acceptanceVersion, Acceptance: "runtime-v1", EvidenceKind: e.EvidenceKind, RunID: e.RunID, Scenario: in.Scenario, Identity: e.Identity,
		ControllerOutcome: in.Incident.Status.Runtime.Phase, ControllerReason: in.Incident.Status.Runtime.Reason,
		ServiceRecovery: passVerdict(), Attribution: passVerdict(), Cleanup: passVerdict(), ApplySource: e.Apply.Source,
		Limits: []string{"Offline input consistency only; observed is not authenticated live evidence.", "Before/after snapshots do not prove CAS execution; conditional writes rely on existing recovery/controller tests.", "Stop proof does not certify process termination or call any API."}}
	// No result from this offline checker is independently authenticated live proof.
	r.LiveProof = false
	service, attr, clean := &r.ServiceRecovery, &r.Attribution, &r.Cleanup
	if e.Version != evidenceVersion || (e.EvidenceKind != "synthetic" && e.EvidenceKind != "observed") {
		attr.add("UNPROVEN", "UNSUPPORTED_EVIDENCE")
		service.add("UNPROVEN", "UNSUPPORTED_EVIDENCE")
		clean.add("UNPROVEN", "UNSUPPORTED_EVIDENCE")
		return r
	}
	if e.Interventions == nil {
		attr.add("UNPROVEN", "INTERVENTION_LOG_MISSING")
	}
	if len(in.Probe) > maxProbeRecords || len(e.Probe) > maxProbeRecords {
		service.add("UNPROVEN", "PROBE_RECORD_LIMIT")
		return r
	}
	if e.RunID == "" || in.Env.RunID == "" {
		attr.add("UNPROVEN", "RUN_ID_MISSING")
	} else if e.RunID != in.Env.RunID {
		attr.add("FAIL", "RUN_ID_MISMATCH")
	}
	roles := map[string]bool{}
	for _, s := range e.Sources {
		if roles[s.Role] {
			attr.add("FAIL", "DUPLICATE_SOURCE")
		}
		roles[s.Role] = true
		if len(s.SHA256) != 64 || s.Ref == "" || in.SourceHashes[s.Role] == "" {
			attr.add("UNPROVEN", "RAW_SOURCE_MISSING")
		} else if s.SHA256 != in.SourceHashes[s.Role] {
			attr.add("FAIL", "RAW_SOURCE_MISMATCH")
		}
	}
	for _, role := range []string{"probe", "incident", "pods", "env"} {
		if !roles[role] {
			attr.add("UNPROVEN", "RAW_SOURCE_MISSING")
		}
	}
	svc := e.Service
	if svc.Namespace == "" || svc.Name == "" || svc.UID == "" || svc.PathID == "" || svc.URL == "" || in.Env.RuntimeService == nil {
		attr.add("UNPROVEN", "SERVICE_IDENTITY_MISSING")
	} else if svc != *in.Env.RuntimeService || svc.Namespace != in.Incident.Namespace || svc.Name != in.Incident.Spec.Policy.Verification.ServiceName {
		attr.add("FAIL", "SERVICE_IDENTITY_MISMATCH")
	}
	// Check all stamps without normalizing or sorting input into a plausible trace.
	stamps := []Stamp{e.FaultRequested, e.Decision, e.Patched, e.Apply.Observed, e.ReadyObserved}
	stamps = append(stamps, e.ReadySource)
	for _, p := range e.Probe {
		stamps = append(stamps, p.Start, p.End)
	}
	for _, c := range e.Coverage {
		stamps = append(stamps, c.Start, c.End)
	}
	for _, v := range e.Interventions {
		stamps = append(stamps, v.Requested)
		if v.Confirmed != nil {
			stamps = append(stamps, *v.Confirmed)
		}
	}
	if e.Stop != nil {
		stamps = append(stamps, e.Stop.Requested, e.Stop.Acknowledged)
	}
	if e.Cleanup != nil {
		stamps = append(stamps, e.Cleanup.Completed)
	}
	seen := map[clockKey][]Stamp{}
	coreCount := 6 + 2*len(e.Probe) + 2*len(e.Coverage)
	for _, event := range e.Interventions {
		coreCount++
		if event.Confirmed != nil {
			coreCount++
		}
	}
	for i, s := range stamps {
		if i == 5 && !stampValid(s) {
			continue
		}
		v := attr
		if i >= coreCount {
			v = clean
		}
		if !stampValid(s) {
			v.add("UNPROVEN", "STAMP_MISSING")
			continue
		}
		seen[lane(s)] = append(seen[lane(s)], s)
	}
	// Sort a separate event registry solely to detect elapsed/UTC discontinuities.
	// The probe trace itself is never reordered; input order is checked below.
	for _, events := range seen {
		slices.SortFunc(events, func(a, b Stamp) int {
			if a.Sequence < b.Sequence {
				return -1
			}
			if a.Sequence > b.Sequence {
				return 1
			}
			return 0
		})
		for i := 1; i < len(events); i++ {
			a, b := events[i-1], events[i]
			if a.Sequence == b.Sequence {
				attr.add("FAIL", "DUPLICATE_SEQUENCE")
			}
			if *a.ElapsedMS >= *b.ElapsedMS {
				attr.add("FAIL", "ELAPSED_SEQUENCE_CONTRADICTION")
			}
			// Allow millisecond serialization precision, never an unsegmented clock step.
			if math.Abs(b.At.Sub(a.At).Seconds()*1000-(*b.ElapsedMS-*a.ElapsedMS)) > 2 {
				attr.add("UNPROVEN", "CLOCK_DISCONTINUITY_REQUIRES_SEGMENT")
			}
		}
	}
	clockSeen := map[clockKey]bool{}
	for _, c := range e.Clocks {
		k := clockKey{c.Recorder, c.Segment}
		if clockSeen[k] || c.Recorder == "" || c.Segment == "" || c.Ref == "" || c.UncertaintyMS == nil || *c.UncertaintyMS < 0 || math.IsInf(*c.UncertaintyMS, 0) || math.IsNaN(*c.UncertaintyMS) || math.IsInf(c.OffsetMS, 0) || math.IsNaN(c.OffsetMS) {
			attr.add("FAIL", "INVALID_CLOCK_BOUND")
		}
		clockSeen[k] = true
	}
	id := e.Identity
	inc := in.Incident
	match := func(a, b, reason string) {
		if a == "" || b == "" {
			attr.add("UNPROVEN", reason+"_MISSING")
		} else if a != b {
			attr.add("FAIL", reason+"_MISMATCH")
		}
	}
	match(id.PolicyUID, inc.Spec.PolicyRef.UID, "POLICY_UID")
	match(id.PolicyHash, inc.Spec.PolicyHash, "POLICY_HASH")
	match(id.IncidentUID, string(inc.UID), "INCIDENT_UID")
	match(id.DeploymentUID, inc.Spec.Workload.UID, "DEPLOYMENT_UID")
	if id.PolicyGeneration <= 0 || id.IncidentSequence <= 0 {
		attr.add("UNPROVEN", "GENERATION_SEQUENCE_MISSING")
	} else if id.PolicyGeneration != inc.Spec.PolicyGeneration || id.IncidentSequence != inc.Spec.Sequence {
		attr.add("FAIL", "GENERATION_SEQUENCE_MISMATCH")
	}
	if !inc.Spec.Policy.Enabled || string(inc.Spec.Policy.Mode) != modeEnforce || inc.Spec.Policy.Budget.MaxActionsPerIncident != 1 {
		attr.add("UNPROVEN", "ENFORCE_POLICY_BUDGET_MISSING")
	}
	if inc.Spec.PolicyHash != v1alpha1.PolicyHash(inc.Spec.Policy) {
		attr.add("FAIL", "POLICY_SNAPSHOT_HASH_MISMATCH")
	}
	if inc.Spec.Policy.TargetRef.Name != inc.Spec.Workload.Name {
		attr.add("FAIL", "POLICY_TARGET_MISMATCH")
	}
	if in.Mode != modeEnforce || in.Scenario == scenarioAllRejected || r.ControllerOutcome != "RECOVERED" {
		attr.add("FAIL", "NORMAL_ENFORCE_NOT_RECOVERED")
	}
	if len(inc.Status.Runtime.Attempts) != 1 {
		attr.add("FAIL", "ATTEMPT_COUNT_NOT_ONE")
	} else {
		a := inc.Status.Runtime.Attempts[0]
		d := a.Decision
		match(id.ActionID, a.ActionID, "ACTION")
		match(id.ActionID, d.ActionID, "DECISION_ACTION")
		match(inc.Name, d.IncidentID, "DECISION_INCIDENT")
		match(id.PodUID, a.PodUID, "POD_UID")
		match(id.NodeUID, a.NodeUID, "NODE_UID")
		match(id.NodeUID, d.SelectedNodeUID, "DECISION_NODE")
		match(id.PolicyHash, d.PolicyHash, "DECISION_POLICY_HASH")
		match(id.DeploymentUID, d.WorkloadUID, "DECISION_WORKLOAD")
		match(a.PreTemplateHash, d.BaselineTemplateHash, "DECISION_BASELINE")
		if id.PolicyGeneration > 0 && d.PolicyGeneration != id.PolicyGeneration {
			attr.add("FAIL", "DECISION_GENERATION_MISMATCH")
		}
		if a.Outcome != "Succeeded" || d.ActionType != "Relocation" || !a.EndpointReady || a.ReplicaSetUID == "" {
			attr.add("UNPROVEN", "CONTROLLER_VERIFICATION_MISSING")
		}
		c := candidate(&d, in.ExpectNode)
		if c == nil {
			attr.add("UNPROVEN", "CANDIDATE_MISSING")
		} else {
			if !c.Eligible {
				attr.add("FAIL", "NODE_CONNECTION_MISMATCH")
			}
			match(id.NodeUID, c.NodeUID, "NODE_CONNECTION")
			match(c.Hostname, a.Hostname, "NODE_CONNECTION")
			match(d.SelectedHostname, a.Hostname, "NODE_CONNECTION")
			match(d.SelectedNodeName, a.NodeName, "NODE_CONNECTION")
			match(a.PodNodeName, a.NodeName, "NODE_CONNECTION")
			match(a.NodeName, in.ExpectNode, "NODE_CONNECTION")
		}
		podCount := 0
		for _, p := range in.Pods.Items {
			if p.Metadata.UID == a.PodUID {
				podCount++
				match(p.Metadata.Name, a.PodName, "POD_NAME")
				match(p.Metadata.Namespace, inc.Namespace, "POD_NAMESPACE")
				match(p.Metadata.Namespace, svc.Namespace, "POD_SERVICE_NAMESPACE")
				match(p.Spec.NodeName, a.PodNodeName, "POD_NODE")
				match(p.Spec.NodeName, a.NodeName, "POD_ATTEMPT_NODE")
				match(p.Spec.NodeName, d.SelectedNodeName, "POD_SELECTED_NODE")
				match(p.Spec.NodeName, in.ExpectNode, "POD_EXPECTED_NODE")
				controllers := 0
				for _, owner := range p.Metadata.OwnerReferences {
					if !trueValue(owner.Controller) {
						continue
					}
					controllers++
					match(string(owner.UID), a.ReplicaSetUID, "POD_CONTROLLER_OWNER")
					if owner.Kind == "" || owner.APIVersion == "" {
						attr.add("UNPROVEN", "POD_CONTROLLER_OWNER_MISSING")
					}
					if (owner.Kind != "" && owner.Kind != "ReplicaSet") || (owner.APIVersion != "" && owner.APIVersion != "apps/v1") {
						attr.add("FAIL", "POD_CONTROLLER_OWNER_MISMATCH")
					}
				}
				if controllers == 0 {
					attr.add("UNPROVEN", "POD_CONTROLLER_OWNER_MISSING")
				} else if controllers > 1 {
					attr.add("FAIL", "POD_CONTROLLER_OWNER_AMBIGUOUS")
				}
			}
		}
		if podCount == 0 {
			attr.add("UNPROVEN", "ATTEMPT_POD_MISSING")
		} else if podCount != 1 {
			attr.add("FAIL", "DUPLICATE_ATTEMPT_POD")
		}
		nodeNames, nodeUIDs := map[string]bool{}, map[string]bool{}
		for _, c := range d.Candidates {
			if nodeNames[c.NodeName] || (c.NodeUID != "" && nodeUIDs[c.NodeUID]) {
				attr.add("FAIL", "DUPLICATE_CANDIDATE_IDENTITY")
			}
			nodeNames[c.NodeName] = true
			nodeUIDs[c.NodeUID] = true
		}
		ss := Summary{}
		if !imageMatches(&ss, in, a.PodUID, d.ImageIndexDigest) || d.ImageIndexDigest != inc.Spec.Baseline.ImageDigest {
			attr.add("FAIL", "IMAGE_MISMATCH")
		}
		if !strings.HasSuffix(in.Env.ImageRef, "@"+d.ImageIndexDigest) || inc.Spec.Baseline.Image != in.Env.ImageRef {
			attr.add("FAIL", "APPROVED_IMAGE_MISMATCH")
		}
		for _, p := range in.Pods.Items {
			if p.Metadata.UID == id.PodUID {
				for _, c := range p.Spec.Containers {
					if c.Image != in.Env.ImageRef {
						attr.add("FAIL", "POD_IMAGE_REFERENCE_MISMATCH")
					}
				}
			}
		}
		if in.ExpectVersion == "" {
			attr.add("UNPROVEN", "EXPECTED_VERSION_MISSING")
		}
		for n, reason := range in.ExpectReject {
			c := candidate(&d, n)
			if c == nil || !slices.Contains(c.RejectReasons, reason) {
				attr.add("FAIL", "EXPECTED_REJECT_MISSING")
			}
		}
		ap := e.Apply
		match(ap.DeploymentUID, id.DeploymentUID, "APPLY_DEPLOYMENT")
		match(ap.PreRV, a.PreResourceVersion, "APPLY_PRE_RV")
		match(ap.PreTemplateHash, a.PreTemplateHash, "APPLY_PRE_TEMPLATE")
		match(ap.PreTemplateHash, inc.Spec.Baseline.TemplateHash, "BASELINE_TEMPLATE")
		match(ap.TargetTemplateHash, a.TargetTemplateHash, "APPLY_TARGET_TEMPLATE")
		match(ap.AttemptAnnotation, id.ActionID, "APPLY_ACTION")
		match(ap.Hostname, a.Hostname, "APPLY_HOSTNAME")
		if ap.PreTemplateHash != "" && ap.PreTemplateHash == ap.TargetTemplateHash {
			attr.add("FAIL", "APPLY_TEMPLATE_UNCHANGED")
		}
		if ap.PostRV == "" || ap.Ref == "" || (ap.Source != "update-response" && ap.Source != "fresh-get") {
			attr.add("UNPROVEN", "APPLY_CONFIRMATION_MISSING")
		}
		if ap.PreRV != "" && ap.PreRV == ap.PostRV {
			attr.add("FAIL", "APPLY_RV_UNCHANGED")
		}
		if !e.Decision.At.Equal(d.DecidedAt.Time) || a.PatchedAt == nil || !e.Patched.At.Equal(a.PatchedAt.Time) {
			attr.add("UNPROVEN", "CONTROLLER_TIME_LINK_MISSING")
		}
		if a.PodReadyAt == nil || !e.ReadySource.At.Equal(a.PodReadyAt.Time) {
			attr.add("UNPROVEN", "READY_SOURCE_LINK_MISSING")
		}
	}
	if !e.FaultRequested.At.Equal(in.InjectAt) {
		attr.add("FAIL", "FAULT_REQUEST_TIME_MISMATCH")
	}
	requireOrder(attr, e, e.FaultRequested, e.Decision, "FAULT_DECISION")
	requireOrder(attr, e, e.Decision, e.Patched, "DECISION_PATCHED")
	requireOrder(attr, e, e.Patched, e.Apply.Observed, "PATCHED_APPLY_CONFIRMATION")
	requireOrder(attr, e, e.Apply.Observed, e.ReadyObserved, "APPLY_READY_OBSERVED")
	r.PlacementDuration = delta(e, e.Apply.Observed, e.ReadySource)
	if r.PlacementDuration.Seconds != nil && *r.PlacementDuration.Seconds < 0 {
		r.PlacementDuration = DurationResult{Reason: "READY_SOURCE_PRECEDES_APPLY_OBSERVATION"}
	}
	if ok, known := ordered(e, e.ReadySource, e.ReadyObserved); known && !ok {
		attr.add("FAIL", "READY_SOURCE_AFTER_OBSERVATION")
	}
	// Source Ready before the recorded patch is a real contradiction if comparable. A later
	// Ready observation alone does not imply Service responses happened too early.
	if ok, known := ordered(e, e.Patched, e.ReadySource); known && !ok {
		attr.add("FAIL", "READY_BEFORE_PATCHED")
	}
	if len(e.Probe) != len(in.Probe) || len(in.Probe) == 0 {
		service.add("UNPROVEN", "PROBE_TIMING_MISSING")
		attr.add("UNPROVEN", "PROBE_TIME_LINK_MISSING")
	} else {
		var baseline, outage, observedFailure bool
		var baselineFirst, baselinePrev *Stamp
		baselineCount := 0
		failures, count := 0, 0
		var first, prev *Stamp
		for i, p := range in.Probe {
			t := e.Probe[i]
			if p.PathID != svc.PathID || p.URL != svc.URL {
				service.add("FAIL", "PROBE_PATH_MISMATCH")
			}
			if p.Status < 0 || p.Status > 599 || (p.Status > 0 && p.Status < 100) {
				attr.add("FAIL", "INVALID_HTTP_STATUS")
			}
			if !p.T.Equal(t.Start.At) || p.LatencyMs < 0 || p.LatencyMs > 60000 || !t.End.At.Equal(p.T.Add(time.Duration(p.LatencyMs)*time.Millisecond)) {
				attr.add("FAIL", "PROBE_TIME_LINK_MISMATCH")
			}
			requireOrder(attr, e, t.Start, t.End, "REQUEST_COMPLETION")
			if i > 0 {
				requireOrder(attr, e, e.Probe[i-1].End, t.Start, "PROBE_INPUT")
			}
			if t.Result != "response" && t.Result != "service-failure" && t.Result != "recorder-error" {
				service.add("UNPROVEN", "PROBE_RESULT_UNKNOWN")
			}
			healthy := p.OK && p.Status == 200 && p.Error == "" && t.Result == "response"
			before, known := ordered(e, t.End, e.FaultRequested)
			if healthy && known && before && p.PodUID != "" && p.Version == in.ExpectVersion && p.Arch != "" {
				if baselinePrev != nil {
					d := delta(e, *baselinePrev, t.Start)
					if d.Seconds == nil || *d.Seconds+*d.UncertaintySeconds > 3 {
						baselineFirst = nil
						baselineCount = 0
					}
				}
				if baselineFirst == nil {
					v := t.Start
					baselineFirst = &v
				}
				v := t.Start
				baselinePrev = &v
				baselineCount++
				d := delta(e, *baselineFirst, t.Start)
				if baselineCount >= 5 && d.Seconds != nil && *d.Seconds-*d.UncertaintySeconds >= 30 {
					baseline = true
				}
			} else {
				baselineFirst = nil
				baselinePrev = nil
				baselineCount = 0
			}
			after, known := ordered(e, e.FaultRequested, t.Start)
			failed := t.Result == "service-failure" && !p.OK && (p.Status == 0 || p.Status >= 400)
			if applied, k := ordered(e, e.Apply.Observed, t.Start); k && applied && failed {
				observedFailure = true
			}
			if known && after && failed {
				failures++
				if failures == 1 {
					s := t.Start
					r.FirstFailureAt = &s
				}
				if failures >= 2 && baseline {
					outage = true
				}
			} else if !outage {
				failures = 0
				r.FirstFailureAt = nil
			}
			eligible, k := ordered(e, e.Apply.Observed, t.Start)
			current := healthy && p.PodUID == id.PodUID && p.Node == in.ExpectNode && p.Version == in.ExpectVersion
			if len(inc.Status.Runtime.Attempts) == 1 {
				c := candidate(&inc.Status.Runtime.Attempts[0].Decision, in.ExpectNode)
				current = current && c != nil && c.Platform == "linux/"+p.Arch
			}
			if current {
				// Apply observation can lag actual application. Earlier requests reset
				// the window below, but are not by themselves causal contradictions.
				if !k {
					attr.add("UNPROVEN", "SERVICE_AFTER_APPLY_CLOCK_UNPROVEN")
				}
				// A response completed before the actual decision/patch cannot
				// belong to this action. These comparisons need clock evidence.
				requireOrder(attr, e, e.Decision, t.End, "SERVICE_DECISION")
				if ok, known := ordered(e, e.Patched, t.End); !known {
					attr.add("UNPROVEN", "SERVICE_PATCHED_CLOCK_UNPROVEN")
				} else if !ok {
					attr.add("FAIL", "SERVICE_BEFORE_PATCHED")
				}
				// Ready source is optional cross-clock duration evidence, but a
				// comparable response before that source is still a contradiction.
				if ok, known := ordered(e, e.ReadySource, t.End); known && !ok {
					attr.add("FAIL", "SERVICE_BEFORE_READY_SOURCE")
				}
			}
			if !current || !k || !eligible || !outage {
				count = 0
				first = nil
				prev = nil
				continue
			}
			if prev != nil {
				d := delta(e, *prev, t.Start)
				if d.Seconds == nil || *d.Seconds+*d.UncertaintySeconds > 3 {
					count = 0
					first = nil
				}
			}
			if first == nil {
				s := t.Start
				first = &s
			}
			count++
			s := t.Start
			prev = &s
			// Use request starts for the window: a slow last response cannot manufacture 30 seconds.
			d := delta(e, *first, t.Start)
			if count >= 5 && d.Seconds != nil && *d.Seconds-*d.UncertaintySeconds >= 30 && r.ServiceStableAt == nil {
				f, en := *first, t.End
				r.ServiceFirstHealthyAt = &f
				r.ServiceStableAt = &en
			}
		}
		if !baseline {
			service.add("UNPROVEN", "NORMAL_BASELINE_MISSING")
		}
		if !outage {
			service.add("UNPROVEN", "NO_OUTAGE_OBSERVED")
		}
		if r.ServiceStableAt == nil {
			status := "UNPROVEN"
			if observedFailure {
				status = "FAIL"
			}
			service.add(status, "SERVICE_NOT_STABLE")
		}
	}
	if r.ServiceStableAt != nil && r.FirstFailureAt != nil {
		r.ServiceRecoveryDuration = delta(e, *r.FirstFailureAt, *r.ServiceStableAt)
		if ok, known := ordered(e, e.ReadySource, *r.ServiceFirstHealthyAt); known && !ok {
			attr.add("FAIL", "SERVICE_BEFORE_READY_SOURCE")
		}
		requireOrder(attr, e, *r.FirstFailureAt, e.Decision, "FAILURE_DECISION")
	} else {
		r.ServiceRecoveryDuration = DurationResult{Reason: "NO_STABLE_RECOVERY"}
		attr.add("UNPROVEN", "RECOVERY_CAUSALITY_UNPROVEN")
	}
	actors := map[string]bool{}
	for _, c := range e.Coverage {
		if actors[c.Actor] {
			attr.add("FAIL", "DUPLICATE_ACTOR_COVERAGE")
		}
		actors[c.Actor] = true
		if !trueValue(c.Complete) || c.Ref == "" {
			attr.add("UNPROVEN", "ACTOR_COVERAGE_INCOMPLETE")
		}
		requireOrder(attr, e, c.Start, e.FaultRequested, "COVERAGE_START")
		if r.ServiceStableAt != nil {
			requireOrder(attr, e, *r.ServiceStableAt, c.End, "COVERAGE_END")
		}
	}
	for _, a := range []string{"watchdog", "admin", "cleanup", "desired-state"} {
		if !actors[a] {
			attr.add("UNPROVEN", "ACTOR_COVERAGE_MISSING")
		}
	}
	for _, v := range e.Interventions {
		if (v.Actor != "watchdog" && v.Actor != "admin" && v.Actor != "cleanup") || (v.Kind != "restore" && v.Kind != "desired-state") || v.TargetUID == "" || v.Ref == "" {
			attr.add("UNPROVEN", "INTERVENTION_IDENTITY_MISSING")
		}
		if v.Confirmed != nil {
			requireOrder(attr, e, v.Requested, *v.Confirmed, "RESTORE_CONFIRMATION")
		}
		if r.ServiceStableAt != nil {
			requireOrder(attr, e, *r.ServiceStableAt, v.Requested, "EXTERNAL_INTERVENTION")
		}
		if v.Actor == "cleanup" && e.Stop != nil {
			requireOrder(clean, e, e.Stop.Acknowledged, v.Requested, "STOP_BEFORE_RESTORE")
		}
	}
	if e.Stop == nil {
		clean.add("UNPROVEN", "STOP_PROOF_MISSING")
	} else {
		s := e.Stop
		f := s.LiveFence
		if s.Mode != modeEnforce && s.Mode != modeObserveOnly {
			clean.add("UNPROVEN", "STOP_MODE_MISSING")
		}
		if s.PolicyUID == "" || s.Ref == "" || s.Enabled == nil || s.Deleting == nil || s.ActiveIncidentAbsent == nil {
			clean.add("UNPROVEN", "STOP_PROOF_INCOMPLETE")
		}
		if s.PolicyUID == "" || s.Generation <= 0 || s.ObservedGeneration <= 0 {
			clean.add("UNPROVEN", "STOP_IDENTITY_GENERATION_MISSING")
		} else if s.PolicyUID != inc.Spec.PolicyRef.UID || s.Generation <= inc.Spec.PolicyGeneration || s.ObservedGeneration != s.Generation {
			clean.add("FAIL", "STOP_IDENTITY_GENERATION_MISMATCH")
		}
		if !(falseValue(s.Enabled) || s.Mode == modeObserveOnly) || !falseValue(s.Deleting) || !trueValue(s.ActiveIncidentAbsent) || s.QuiescedStatus != "True" || s.QuiescedReason != "FenceClosed" {
			clean.add("UNPROVEN", "STOP_NOT_QUIESCED")
		}
		// Only a state linked to this policy and stop generation is a
		// counterexample. Missing values and stale generations prove nothing.
		if s.PolicyUID != "" && s.PolicyUID == inc.Spec.PolicyRef.UID && s.Generation > inc.Spec.PolicyGeneration && s.ObservedGeneration == s.Generation {
			if falseValue(s.ActiveIncidentAbsent) {
				clean.add("FAIL", "STOP_ACTIVE_INCIDENT")
			}
			if s.QuiescedStatus == "False" {
				clean.add("FAIL", "STOP_QUIESCED_FALSE")
			}
		}
		if f.Version == 0 || f.PolicyUID == "" || f.PolicyName == "" || f.DeploymentUID == "" || f.ClosedThroughGeneration == 0 || s.AcknowledgedFence.Version == 0 || s.AcknowledgedFence.PolicyUID == "" || s.AcknowledgedFence.DeploymentUID == "" {
			clean.add("UNPROVEN", "STOP_FENCE_MISSING")
		} else if f != s.AcknowledgedFence || f.Version != 2 || f.PolicyUID != inc.Spec.PolicyRef.UID || f.PolicyName != inc.Spec.PolicyRef.Name || f.DeploymentUID != inc.Spec.Workload.UID || f.ClosedThroughGeneration < s.Generation {
			clean.add("FAIL", "STOP_FENCE_MISMATCH")
		}
		requireOrder(clean, e, s.Requested, s.Acknowledged, "STOP_ACK")
		if r.ServiceStableAt != nil {
			requireOrder(clean, e, *r.ServiceStableAt, s.Requested, "STABLE_STOP")
		}
	}
	if e.Cleanup == nil {
		clean.add("UNPROVEN", "CLEANUP_MISSING")
	} else {
		c := e.Cleanup
		if c.Status == "failed" {
			clean.add("FAIL", "CLEANUP_FAILED")
		} else if c.Status != "succeeded" || c.Ref == "" || !trueValue(c.WorkloadChecked) || !trueValue(c.OldPodsChecked) || !trueValue(c.ServiceChecked) {
			clean.add("UNPROVEN", "CLEANUP_INCOMPLETE")
		}
		if e.Stop != nil {
			requireOrder(clean, e, e.Stop.Acknowledged, c.Completed, "STOP_CLEANUP")
		}
		for _, v := range e.Interventions {
			requireOrder(clean, e, v.Requested, c.Completed, "INTERVENTION_CLEANUP")
			if v.Confirmed != nil {
				requireOrder(clean, e, *v.Confirmed, c.Completed, "RESTORE_CLEANUP")
			}
		}
	}
	r.OverallPass = service.Status == "PASS" && attr.Status == "PASS" && clean.Status == "PASS"
	return r
}

// jsonInputFields describes only the structs consumed by this offline CLI.
// Follow encoding/json's tagged/embedded field dominance, then name ordering
// (which also determines the first folded match). Maps and unknown objects have
// no typed fields. Custom scalar decoders such as Time and Quantity stay opaque.
type jsonInputField struct {
	name   string
	typ    reflect.Type
	depth  int
	tagged bool
}

func jsonInputFields(t reflect.Type) []jsonInputField {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct || reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	var all []jsonInputField
	var collect func(reflect.Type, int)
	collect = func(t reflect.Type, depth int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if !f.IsExported() && (!f.Anonymous || ft.Kind() != reflect.Struct) {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
				collect(ft, depth+1)
				continue
			}
			tagged := name != ""
			if name == "" {
				name = f.Name
			}
			all = append(all, jsonInputField{name, f.Type, depth, tagged})
		}
	}
	collect(t, 0)
	slices.SortFunc(all, func(a, b jsonInputField) int {
		if n := strings.Compare(a.name, b.name); n != 0 {
			return n
		}
		if a.depth != b.depth {
			return a.depth - b.depth
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return 0
	})
	var fields []jsonInputField
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].name == all[i].name {
			j++
		}
		if j == i+1 || all[i].depth != all[i+1].depth || all[i].tagged != all[i+1].tagged {
			fields = append(fields, all[i])
		}
		i = j
	}
	return fields
}

// Token validation runs before decode so duplicate information is never lost.
// Exact keys are unique in every object. Only destination struct fields also
// share encoding/json's Unicode simple-fold identity; free map keys do not.
func checkJSON(b []byte, target reflect.Type) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int, reflect.Type) error
	walk = func(depth int, typ reflect.Type) error {
		if depth > 64 {
			return fmt.Errorf("JSON nesting exceeds 64")
		}
		for typ != nil && typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			fields := jsonInputFields(typ)
			exact, typed := map[string]bool{}, map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key := k.(string)
				if exact[key] {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				exact[key] = true
				var child reflect.Type
				field := -1
				for i, f := range fields {
					if f.name == key {
						field = i
						break
					}
				}
				if field < 0 {
					for i, f := range fields {
						if strings.EqualFold(f.name, key) {
							field = i
							break
						}
					}
				}
				if field >= 0 {
					f := fields[field]
					if typed[f.name] {
						return fmt.Errorf("duplicate JSON field %q (typed field %q)", key, f.name)
					}
					typed[f.name] = true
					child = f.typ
				} else if typ != nil && typ.Kind() == reflect.Map {
					child = typ.Elem()
				}
				if err := walk(depth+1, child); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			var child reflect.Type
			if typ != nil && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array) {
				child = typ.Elem()
			}
			for d.More() {
				if err := walk(depth+1, child); err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0, target); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
func boundedFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInputBytes {
		return nil, fmt.Errorf("%s: input exceeds 16 MiB", path)
	}
	return b, nil
}
func strictDecode(b []byte, v any, closed bool) error {
	if err := checkJSON(b, reflect.TypeOf(v)); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return fmt.Errorf("null document")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if closed {
		d.DisallowUnknownFields()
	}
	return d.Decode(v)
}
func readEvidence(path string) (RuntimeEvidence, error) {
	var e RuntimeEvidence
	b, err := boundedFile(path)
	if err != nil {
		return e, fmt.Errorf("malformed evidence: %w", err)
	}
	return decodeEvidence(b)
}

func decodeEvidence(b []byte) (RuntimeEvidence, error) {
	var e RuntimeEvidence
	err := strictDecode(b, &e, true)
	if err == nil && (len(e.Probe) > maxProbeRecords || len(e.Clocks) > 64 || len(e.Coverage) > 16 || len(e.Interventions) > 1000 || len(e.Sources) > 16) {
		err = fmt.Errorf("evidence collection exceeds record limits (probe 10000, clocks 64, coverage/sources 16, interventions 1000)")
	}
	if err == nil && e.Version != evidenceVersion {
		err = fmt.Errorf("unsupported evidence version %q", e.Version)
	}
	if err == nil && e.EvidenceKind != "synthetic" && e.EvidenceKind != "observed" {
		err = fmt.Errorf("unsupported evidenceKind %q", e.EvidenceKind)
	}
	if err != nil {
		return e, fmt.Errorf("malformed evidence: %w", err)
	}
	return e, nil
}
func digestBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
