//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
)

const runResultVersion = "opsloop-run-result/v1"

type FinalizeIdentity struct {
	Namespace     string          `json:"namespace"`
	Policy        string          `json:"policy"`
	PolicyUID     string          `json:"policyUID"`
	Deployment    string          `json:"deployment"`
	DeploymentUID string          `json:"deploymentUID"`
	Service       ServiceIdentity `json:"service"`
}

// Mapping is an explicit operator-supplied assertion, not authenticated target
// registration. NodeUID is never inferred from VMUUID or the selected Node.
type FinalizeMapping struct {
	NodeUID    string `json:"nodeUID"`
	NodeName   string `json:"nodeName"`
	GuardURI   string `json:"guardURI"`
	GuardUUID  string `json:"guardUUID"`
	FinishURI  string `json:"finishURI"`
	FinishUUID string `json:"finishUUID"`
	Basis      string `json:"basis"`
}
type FinalizeConfig struct {
	Version         string           `json:"version"`
	RunID           string           `json:"runID"`
	EvidenceKind    string           `json:"evidenceKind"`
	Guard           guardConfig      `json:"expectedGuard"`
	GuardBinding    string           `json:"expectedGuardBinding"`
	Identity        FinalizeIdentity `json:"identity"`
	Mapping         FinalizeMapping  `json:"targetMapping"`
	ObserverDir     string           `json:"observerDir"`
	GuardFile       string           `json:"guardFile"`
	OutputDir       string           `json:"outputDir"`
	ExpectNode      string           `json:"expectNode"`
	ExpectVersion   string           `json:"expectVersion"`
	PlatformDigests []string         `json:"platformDigests"`
}
type RunInputReference struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type RunResult struct {
	SchemaVersion       string              `json:"schemaVersion"`
	RunID               string              `json:"runID"`
	EvidenceKind        string              `json:"evidenceKind"`
	Status              string              `json:"status"`
	Reasons             []string            `json:"reasons"`
	RuntimeAcceptance   *AcceptanceResult   `json:"runtimeAcceptance"`
	GuardAssessment     GuardAssessment     `json:"guardAssessment"`
	OverallPass         bool                `json:"overallPass"`
	LiveProof           bool                `json:"liveProof"`
	ConsumerIntegration string              `json:"consumerIntegration"`
	GuardEpoch          string              `json:"guardEpoch"`
	GuardBinding        string              `json:"guardBinding"`
	Identity            FinalizeIdentity    `json:"identity"`
	TargetMapping       FinalizeMapping     `json:"targetMapping"`
	InputReferences     []RunInputReference `json:"inputReferences"`
	Limitations         []string            `json:"limitations"`
}

func finalPath(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }
func (c FinalizeConfig) validate() error {
	if c.Version != "opsloop-finalize/v1" || !guardID.MatchString(c.RunID) || (c.EvidenceKind != "synthetic" && c.EvidenceKind != "observed") || c.Guard.Run != c.RunID || c.GuardBinding != c.Guard.binding() || c.ExpectNode == "" || c.ExpectVersion == "" {
		return errors.New("FINALIZE_CONFIG_BINDING_INVALID")
	}
	if e := c.Guard.validate(); e != nil {
		return errors.New("FINALIZE_EXPECTED_GUARD_INVALID")
	}
	if c.EvidenceKind == "observed" && c.Guard.Mode != "live-libvirt" || c.EvidenceKind == "synthetic" && c.Guard.Mode != "synthetic-local" {
		return errors.New("FINALIZE_EVIDENCE_KIND_MISMATCH")
	}
	id := c.Identity
	if id.Namespace == "" || id.Policy == "" || id.PolicyUID == "" || id.Deployment == "" || id.DeploymentUID == "" || id.Service.Namespace != id.Namespace || id.Service.Name == "" || id.Service.UID == "" || id.Service.PathID == "" || id.Service.URL == "" {
		return errors.New("FINALIZE_EXPECTED_IDENTITY_REQUIRED")
	}
	if c.Mapping.GuardURI != c.Guard.Target.URI || c.Mapping.GuardUUID != c.Guard.Target.UUID || c.Mapping.NodeUID == "" || c.Mapping.NodeName == "" || c.Mapping.Basis == "" || c.Mapping.FinishURI == "" || !finishUUID.MatchString(c.Mapping.FinishUUID) {
		return errors.New("FINALIZE_TARGET_MAPPING_INVALID")
	}
	// Distinct synthetic URI aliases are permitted only as an explicit test map.
	if c.EvidenceKind == "observed" && (c.Mapping.FinishURI != c.Mapping.GuardURI || c.Mapping.FinishUUID != c.Mapping.GuardUUID) {
		return errors.New("FINALIZE_LIVE_TARGET_ALIAS_REJECTED")
	}
	for _, p := range []string{c.ObserverDir, c.GuardFile, c.OutputDir} {
		if !finalPath(p) {
			return errors.New("FINALIZE_ABSOLUTE_CLEAN_PATH_REQUIRED")
		}
	}
	if c.OutputDir == c.ObserverDir || strings.HasPrefix(c.OutputDir, c.ObserverDir+string(os.PathSeparator)) || c.OutputDir == c.Guard.Root || strings.HasPrefix(c.OutputDir, c.Guard.Root+string(os.PathSeparator)) {
		return errors.New("FINALIZE_OUTPUT_OVERLAPS_INPUT")
	}
	return nil
}

// Finalizer-local counted reader. Keep the shared Guard/Observer reader contract
// untouched. Every byte returned by Read, including discarded/error bytes and
// the optional one-byte limit probe, is charged to this snapshot.
var errFinalReadLimit = errors.New("finalizer input limit exceeded")

func finalRead(path string, limit int64) ([]byte, error) {
	b, _, e := finalReadCount(path, limit)
	return b, e
}

func finalReadBytes(r io.Reader, limit int64) ([]byte, int64, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	n := int64(len(b))
	if n > limit {
		return nil, n, errFinalReadLimit
	}
	if e != nil {
		return nil, n, e
	}
	return b, n, nil
}

func finalReadCount(path string, limit int64) ([]byte, int64, error) {
	if !finalPath(path) {
		return nil, 0, errors.New("unsafe path")
	}
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			return nil, 0, e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, 0, errors.New("unsafe parent")
		}
		if p == "/" {
			break
		}
	}
	before, e := os.Lstat(path)
	if e != nil {
		return nil, 0, e
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, 0, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return nil, 0, errors.New("invalid input file")
	}
	if st.Size() > limit {
		return nil, 0, errFinalReadLimit
	}
	b, n, e := finalReadBytes(f, limit)
	if e != nil {
		return nil, n, e
	}
	after, e := os.Lstat(path)
	if e != nil {
		return nil, n, e
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return nil, n, errors.New("input changed during read")
	}
	return b, n, nil
}

type finalSnapshot struct {
	BytesRead      int64
	BudgetExceeded bool
	Data           map[string][]byte
	Refs           []RunInputReference
	Problems       map[string]string
	Metadata       map[string]string
}

func captureFinal(c FinalizeConfig, configPath string) finalSnapshot {
	return captureFinalWith(c, configPath, 64<<20, finalReadCount)
}

func captureFinalWith(c FinalizeConfig, configPath string, budget int64, reader func(string, int64) ([]byte, int64, error)) finalSnapshot {
	s := finalSnapshot{Data: map[string][]byte{}, Problems: map[string]string{}, Metadata: map[string]string{}}
	read := func(role, path string, limit int64) []byte {
		if s.BudgetExceeded {
			s.Problems[role] = "INPUT_BUDGET_EXCEEDED"
			return nil
		}
		remaining := budget - s.BytesRead
		bound := min(limit, remaining)
		b, n, e := reader(path, bound)
		s.BytesRead += n
		if s.BytesRead > budget || (errors.Is(e, errFinalReadLimit) && bound == remaining) {
			s.BudgetExceeded = true
			s.Problems[role] = "INPUT_BUDGET_EXCEEDED"
			return nil
		}
		if e != nil {
			reason := "INPUT_FILE_INVALID"
			if os.IsNotExist(e) {
				reason = "INPUT_FILE_MISSING"
			}
			s.Problems[role] = reason
			return nil
		}
		s.Data[role] = b
		if st, err := os.Lstat(path); err == nil {
			if raw, ok := st.Sys().(*syscall.Stat_t); ok {
				s.Metadata[role] = fmt.Sprintf("%d:%d:%d:%d:%v:%v", raw.Dev, raw.Ino, raw.Mode, raw.Size, raw.Mtim, raw.Ctim)
			}
		}
		s.Refs = append(s.Refs, RunInputReference{role, path, digestBytes(b), len(b)})
		return b
	}
	read("config", configPath, 2<<20)
	read("guard", c.GuardFile, 2<<20)
	b := read("manifest", filepath.Join(c.ObserverDir, "manifest.json"), 2<<20)
	read("complete", filepath.Join(c.ObserverDir, "COMPLETE"), 2<<20)
	var m observerManifest
	if b == nil {
		return s
	}
	if strictDecode(b, &m, true) != nil || m.Version != "opsloop-observer/v1" || len(m.Files) > 8192 {
		s.Problems["manifest"] = "OBSERVER_MANIFEST_INVALID"
		return s
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		// Observer writes flat, explicit immutable filenames; no directory traversal.
		if f.Ref == "" || filepath.Base(f.Ref) != f.Ref || f.Ref == "." || f.Ref == "manifest.json" || f.Ref == "COMPLETE" || seen[f.Ref] || !recoveryHash.MatchString(f.SHA256) || f.Bytes < 0 {
			s.Problems["manifest"] = "OBSERVER_MANIFEST_REFERENCE_INVALID"
			return s
		}
		seen[f.Ref] = true
		b := read("observer/"+f.Ref, filepath.Join(c.ObserverDir, f.Ref), maxInputBytes)
		if b != nil && (digestBytes(b) != f.SHA256 || len(b) != f.Bytes) {
			s.Problems["observer/"+f.Ref] = "OBSERVER_FILE_HASH_MISMATCH"
		}
	}
	for _, name := range []string{"probe.jsonl", "incident.json", "pods.json", "env.json", "evidence.json", "summary.json", "receipts.json"} {
		if !seen[name] {
			s.Problems["observer/"+name] = "INPUT_FILE_MISSING"
			if s.BudgetExceeded {
				s.Problems["observer/"+name] = "INPUT_BUDGET_EXCEEDED"
			}
		}
	}
	return s
}

// Three bounded attempts, normally with two complete reads. A confirmed budget
// failure returns immediately without claiming a completed comparison. No mtime/latest-file run
// selection; byte equality and manifest hashes bind the captured revision.
func stableFinal(c FinalizeConfig, configPath string, config []byte, between func(int)) (finalSnapshot, bool) {
	return stableFinalWith(config, between, func() finalSnapshot { return captureFinal(c, configPath) })
}

func stableFinalWith(config []byte, between func(int), capture func() finalSnapshot) (finalSnapshot, bool) {
	var b finalSnapshot
	for attempt := 0; attempt < 3; attempt++ {
		a := capture()
		if a.BudgetExceeded {
			return a, false
		}
		if between != nil {
			between(attempt)
		}
		b = capture()
		if b.BudgetExceeded {
			return b, false
		}
		if reflect.DeepEqual(a, b) && bytes.Equal(b.Data["config"], config) {
			return b, true
		}
	}
	return b, false
}

func runtimeFinal(c FinalizeConfig, s finalSnapshot) (*AcceptanceResult, error) {
	get := func(name string) []byte { return s.Data["observer/"+name] }
	e, err := decodeEvidence(get("evidence.json"))
	if err != nil {
		return nil, errors.New("RUNTIME_EVIDENCE_INVALID")
	}
	in := ReportInput{Mode: modeEnforce, Scenario: "runtime-observer", ExpectNode: c.ExpectNode, ExpectVersion: c.ExpectVersion, PlatformDigests: c.PlatformDigests, SourceHashes: map[string]string{}, InjectAt: e.FaultRequested.At}
	for _, v := range []struct {
		role, name string
		value      any
	}{{"incident", "incident.json", &in.Incident}, {"pods", "pods.json", &in.Pods}, {"env", "env.json", &in.Env}} {
		b := get(v.name)
		if strictDecode(b, v.value, false) != nil {
			return nil, errors.New("RUNTIME_SOURCE_MALFORMED")
		}
		in.SourceHashes[v.role] = digestBytes(b)
	}
	in.Probe, err = ReadProbe(bytes.NewReader(get("probe.jsonl")))
	if err != nil {
		return nil, errors.New("RUNTIME_PROBE_MALFORMED")
	}
	in.SourceHashes["probe"] = digestBytes(get("probe.jsonl"))
	result := Acceptance(in, e)
	// Return the original detailed evaluation even if the integration binding fails.
	id := c.Identity
	if e.RunID != c.RunID || in.Env.RunID != c.RunID || e.EvidenceKind != c.EvidenceKind || e.Service != id.Service || e.Identity.PolicyUID != id.PolicyUID || e.Identity.DeploymentUID != id.DeploymentUID || in.Incident.Namespace != id.Namespace || in.Incident.Spec.PolicyRef.Name != id.Policy || in.Incident.Spec.PolicyRef.UID != id.PolicyUID || in.Incident.Spec.Workload.Name != id.Deployment || in.Incident.Spec.Workload.UID != id.DeploymentUID {
		return &result, errors.New("RUNTIME_CONFIG_IDENTITY_MISMATCH")
	}
	// The declared Kubernetes side must occur in this run's raw Incident. This
	// checks the supplied mapping's identity, not actual host/VM registration.
	mapped := false
	for _, attempt := range in.Incident.Status.Runtime.Attempts {
		for _, candidate := range attempt.Decision.Candidates {
			if candidate.NodeUID == c.Mapping.NodeUID && candidate.NodeName == c.Mapping.NodeName {
				mapped = true
			}
		}
	}
	if !mapped || (in.Incident.Spec.Trigger.NodeUID != "" && in.Incident.Spec.Trigger.NodeUID != c.Mapping.NodeUID) || (in.Incident.Spec.Trigger.NodeName != "" && in.Incident.Spec.Trigger.NodeName != c.Mapping.NodeName) {
		return &result, errors.New("RUNTIME_TARGET_MAPPING_MISMATCH")
	}
	for _, source := range e.Sources {
		name := source.Role + ".json"
		if source.Role == "probe" {
			name = "probe.jsonl"
		}
		if source.Ref != name || source.SHA256 != in.SourceHashes[source.Role] {
			return &result, errors.New("RUNTIME_RAW_SOURCE_BINDING_MISMATCH")
		}
	}
	var m observerManifest
	if strictDecode(s.Data["manifest"], &m, true) != nil || m.RunID != c.RunID || m.EvidenceKind != c.EvidenceKind {
		return &result, errors.New("OBSERVER_MANIFEST_BINDING_MISMATCH")
	}
	pins := map[string]string{
		"/apis/opsloop.io/v1alpha1/namespaces/" + id.Namespace + "/recoverypolicies/" + id.Policy: id.PolicyUID,
		"/apis/apps/v1/namespaces/" + id.Namespace + "/deployments/" + id.Deployment:              id.DeploymentUID,
		"/api/v1/namespaces/" + id.Namespace + "/services/" + id.Service.Name:                     id.Service.UID,
	}
	for path, uid := range pins {
		if m.Pins[path] != uid {
			return &result, errors.New("OBSERVER_PIN_BINDING_MISMATCH")
		}
	}
	// The Observer's copied receipt is already reflected in Runtime evidence.
	// Reuse that path and retain its raw hash; do not turn Guard into a receipt.
	var receipt ObserverReceipts
	if strictDecode(get("receipts.json"), &receipt, true) != nil {
		return &result, errors.New("RUNTIME_RECEIPT_MALFORMED")
	}
	if receipt.Version != "opsloop-observer-receipts/v1" || receipt.RunID != c.RunID || receipt.EvidenceKind != c.EvidenceKind || receipt.Provider == "" {
		return &result, errors.New("RUNTIME_RECEIPT_BINDING_MISMATCH")
	}
	for _, intervention := range e.Interventions {
		if intervention.Actor == "cleanup" && intervention.Kind == "restore" && intervention.TargetUID != c.Mapping.FinishUUID {
			return &result, errors.New("RUNTIME_RESTORE_TARGET_MAPPING_MISMATCH")
		}
	}
	// Preserve the prior summary and detect any change of runtime-v1 semantics.
	var original AcceptanceResult
	if strictDecode(get("summary.json"), &original, true) != nil || !reflect.DeepEqual(original, result) {
		return &result, errors.New("RUNTIME_REEVALUATION_DIFFERS_FROM_SUMMARY")
	}
	return &result, nil
}

func finalizeAssessment(c FinalizeConfig, s finalSnapshot, stable bool) RunResult {
	r := RunResult{SchemaVersion: runResultVersion, RunID: c.RunID, EvidenceKind: c.EvidenceKind, Status: "UNPROVEN", Reasons: []string{}, GuardEpoch: c.Guard.Epoch, GuardBinding: c.GuardBinding, Identity: c.Identity, TargetMapping: c.Mapping, ConsumerIntegration: "GUARD_HISTORY_AND_RUNTIME_V1_OFFLINE", InputReferences: s.Refs, Limitations: []string{
		"Offline byte consistency only; hashes do not authenticate log truth or operator signatures.",
		"Configured target mapping is an explicit assertion; actual target registration and live linkage are unverified.",
		"Historical Guard close is not current VM health, StopProof, CleanupProof or actor coverage.",
		"Guard events retain wall At and sequence; no elapsed/segment/offset or Runtime evidence is synthesized.",
		"Stable bounded snapshots cannot prevent source changes after the final read.",
	}}
	r.GuardAssessment = GuardAssessment{Status: "MISSING", Reasons: []string{"GUARD_RECORD_MISSING"}}
	if b := s.Data["guard"]; b != nil {
		r.GuardAssessment = assessGuard(b, c.Guard)
	}
	var runtimeErr error
	r.RuntimeAcceptance, runtimeErr = runtimeFinal(c, s)
	inputError := false
	missing := false
	keys := make([]string, 0, len(s.Problems))
	for k := range s.Problems {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		reason := s.Problems[k]
		r.Reasons = append(r.Reasons, k+":"+reason)
		if reason == "INPUT_FILE_MISSING" {
			missing = true
		} else {
			inputError = true
		}
	}
	if !stable && !s.BudgetExceeded {
		inputError = true
		r.Reasons = append(r.Reasons, "INPUT_CHANGED_DURING_FINALIZATION")
	}
	if runtimeErr != nil {
		r.Reasons = append(r.Reasons, runtimeErr.Error())
		absent := func(name string) bool { return s.Problems[name] == "INPUT_FILE_MISSING" }
		missingCause := false
		switch runtimeErr.Error() {
		case "RUNTIME_EVIDENCE_INVALID":
			missingCause = absent("observer/evidence.json") || absent("manifest")
		case "RUNTIME_SOURCE_MALFORMED":
			missingCause = absent("observer/incident.json") || absent("observer/pods.json") || absent("observer/env.json")
		case "RUNTIME_RECEIPT_MALFORMED":
			missingCause = absent("observer/receipts.json")
		case "RUNTIME_PROBE_MALFORMED":
			missingCause = absent("observer/probe.jsonl")
		case "OBSERVER_MANIFEST_BINDING_MISMATCH":
			missingCause = absent("manifest")
		case "RUNTIME_REEVALUATION_DIFFERS_FROM_SUMMARY":
			missingCause = absent("observer/summary.json")
		}
		if !missingCause {
			inputError = true
		}
	}
	if r.GuardAssessment.Status == "INVALID" {
		inputError = true
	}
	if s.Problems["guard"] != "" && s.Problems["guard"] != "INPUT_FILE_MISSING" {
		r.GuardAssessment = GuardAssessment{Status: "INVALID", Reasons: []string{"GUARD_FILE_INVALID"}}
	}
	var m observerManifest
	if strictDecode(s.Data["manifest"], &m, true) == nil {
		if m.State != "complete" || !bytes.Equal(s.Data["manifest"], s.Data["complete"]) {
			missing = true
			r.Reasons = append(r.Reasons, "OBSERVER_CAPTURE_NOT_COMPLETE")
		}
	}
	runtimePass := r.RuntimeAcceptance != nil && r.RuntimeAcceptance.OverallPass
	r.Reasons = append(r.Reasons, r.GuardAssessment.Reasons...)
	if !runtimePass {
		r.Reasons = append(r.Reasons, "RUNTIME_NOT_ACCEPTED")
	}
	switch {
	case inputError:
		r.Status = "INPUT_ERROR"
	case r.GuardAssessment.Status == "EMERGENCY":
		r.Status = "FAIL"
	case r.RuntimeAcceptance != nil && (r.RuntimeAcceptance.ServiceRecovery.Status == "FAIL" || r.RuntimeAcceptance.Attribution.Status == "FAIL" || r.RuntimeAcceptance.Cleanup.Status == "FAIL"):
		r.Status = "FAIL"
	case !missing && runtimePass && r.GuardAssessment.Status == "NORMAL_COMPLETION":
		r.Status = "PASS"
		r.OverallPass = true
	}
	return r
}

func runFinalize(args []string, w io.Writer) error {
	f := flag.NewFlagSet("finalize-run", flag.ContinueOnError)
	configPath := f.String("config", "", "predeclared integration configuration; no live actions")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || !finalPath(*configPath) {
		return errors.New("finalize-run: explicit absolute -config required")
	}
	b, e := finalRead(*configPath, 2<<20)
	if e != nil {
		return fmt.Errorf("finalize-run: configuration unavailable: %w", e)
	}
	var c FinalizeConfig
	if strictDecode(b, &c, true) != nil {
		return errors.New("finalize-run: malformed configuration")
	}
	if e = c.validate(); e != nil {
		return e
	}
	// A fresh output directory is the publication reservation. Neither a duplicate
	// invocation nor a failed/partial publication can reuse an older PASS.
	if e = guardPrivateDir(filepath.Dir(c.OutputDir)); e != nil {
		return errors.New("finalize-run: output parent must be private and existing")
	}
	if e = os.Mkdir(c.OutputDir, 0700); e != nil {
		return errors.New("finalize-run: OUTPUT_EXISTS_OR_UNAVAILABLE; no result published by this invocation")
	}
	s, stable := stableFinal(c, *configPath, b, nil)
	r := finalizeAssessment(c, s, stable)
	data, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	out := filepath.Join(c.OutputDir, "run-result.json")
	// Stage complete bytes then publish exclusively; only successful sync + link
	// publication permits a PASS line. Failed directories remain reserved.
	pending := filepath.Join(c.OutputDir, ".run-result.pending")
	if e = finishExclusive(pending, append(data, '\n')); e != nil {
		return errors.New("finalize-run: OUTPUT_WRITE_FAILED; no result published")
	}
	if e = os.Link(pending, out); e != nil {
		return errors.New("finalize-run: OUTPUT_PUBLISH_FAILED")
	}
	if e = syncFinishDir(c.OutputDir); e != nil {
		return errors.New("finalize-run: OUTPUT_SYNC_FAILED")
	}
	if _, e = fmt.Fprintf(w, "run-result=%s status=%s overallPass=%t liveProof=false; Runtime summary is subordinate\n", out, r.Status, r.OverallPass); e != nil {
		return e
	}
	if !r.OverallPass {
		return fmt.Errorf("finalize-run: %s: %s", r.Status, strings.Join(r.Reasons, ","))
	}
	return nil
}
