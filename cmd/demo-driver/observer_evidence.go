package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

// Receipts are a bounded data-only interface to a separate executor/clock
// witness. It cannot supply probes, API objects, apply proofs or an acceptance
// result. Times are retained verbatim; none are replaced by receipt-read time.
type ObserverReceipts struct {
	Version        string          `json:"version"`
	RunID          string          `json:"runID"`
	EvidenceKind   string          `json:"evidenceKind"`
	Provider       string          `json:"provider"`
	FaultRequested Stamp           `json:"faultRequested"`
	Decision       Stamp           `json:"decision"`
	Patched        Stamp           `json:"patched"`
	ReadySource    Stamp           `json:"readySource"`
	Clocks         []ClockBound    `json:"clocks"`
	CollectorClock *ClockBound     `json:"collectorClock"`
	Coverage       []ActorCoverage `json:"coverage"`
	Interventions  []Intervention  `json:"interventions"`
	StopRequested  *Stamp          `json:"stopRequested"`
	Cleanup        *CleanupProof   `json:"cleanup"`
}

func (o *observer) readReceipts() error {
	if o.c.ReceiptsFile == "" {
		o.r.issue("external-receipts-missing")
		return nil
	}
	b, e := observerLocalFile(o.c.ReceiptsFile, 1<<20)
	if e != nil {
		o.r.issue("external-receipts-unavailable")
		return nil
	}
	if strictDecode(b, &o.receipts, true) != nil {
		return errors.New("observer: invalid receipt schema")
	}
	x := o.receipts
	if x.Version != "opsloop-observer-receipts/v1" || x.RunID != o.c.RunID || x.EvidenceKind != o.c.EvidenceKind || x.Provider == "" {
		return errors.New("observer: receipt identity mismatch")
	}
	if e = o.r.write("receipts.json", b); e != nil {
		return e
	}
	o.evidence.FaultRequested = x.FaultRequested
	o.evidence.Coverage = x.Coverage
	o.evidence.Interventions = x.Interventions
	o.evidence.Cleanup = x.Cleanup
	o.evidence.Clocks = x.Clocks
	for i := range o.evidence.Coverage {
		o.evidence.Coverage[i].Ref = fmt.Sprintf("receipts.json#/coverage/%d", i)
	}
	for i := range o.evidence.Interventions {
		o.evidence.Interventions[i].Ref = fmt.Sprintf("receipts.json#/interventions/%d", i)
	}
	if o.evidence.Cleanup != nil {
		o.evidence.Cleanup.Ref = "receipts.json#/cleanup"
	}
	for i := range o.evidence.Clocks {
		if o.evidence.Clocks[i].Ref != "" {
			o.evidence.Clocks[i].Ref = fmt.Sprintf("receipts.json#/clocks/%d", i)
		}
	}
	if x.CollectorClock != nil {
		c := *x.CollectorClock
		if c.Ref != "" {
			c.Ref = "receipts.json#/collectorClock"
		}
		c.Recorder = o.r.manifest.Recorder
		c.Segment = o.r.manifest.Segment
		o.evidence.Clocks = append(o.evidence.Clocks, c)
	}
	return nil
}

func (o *observer) linkSourceTimes(a v1.Attempt) {
	link := func(actual time.Time, s Stamp, label string) Stamp {
		if actual.IsZero() {
			return Stamp{}
		}
		if stampValid(s) && s.At.Equal(actual) {
			return s
		}
		return Stamp{Recorder: label + "-unbounded", Segment: "unknown", At: actual.UTC()}
	}
	o.evidence.Decision = link(a.Decision.DecidedAt.Time, o.receipts.Decision, "controller")
	if a.PatchedAt != nil {
		o.evidence.Patched = link(a.PatchedAt.Time, o.receipts.Patched, "controller")
	}
	if a.PodReadyAt != nil {
		o.evidence.ReadySource = link(a.PodReadyAt.Time, o.receipts.ReadySource, "kubelet")
	}
}

func (o *observer) finalize(w io.Writer) error {
	r := o.r
	if r.finalized {
		return errors.New("observer: duplicate finalize")
	}
	// Capture the actual collection boundary before receipt I/O or serialization.
	// The final probe completion is already a captured Stamp; Now cannot extend it.
	if len(o.evidence.Probe) > 0 {
		r.manifest.CollectionHorizon = o.evidence.Probe[len(o.evidence.Probe)-1].End
	}
	if e := o.readReceipts(); e != nil {
		return e
	}
	if len(o.incident.Status.Runtime.Attempts) == 1 {
		o.linkSourceTimes(o.incident.Status.Runtime.Attempts[0])
	}
	o.validateReceiptHorizon()
	if o.receipts.StopRequested == nil {
		r.issue("stop-request-receipt-missing")
	}
	if o.evidence.Cleanup == nil {
		r.issue("cleanup-receipt-missing")
	}
	env := Env{RunID: o.c.RunID, RuntimeService: &o.evidence.Service, ImageRef: o.c.ImageRef, NetPath: o.c.PathID}
	var probe strings.Builder
	for _, p := range o.probes {
		b, e := json.Marshal(p)
		if e != nil {
			return e
		}
		probe.Write(b)
		probe.WriteByte('\n')
	}
	if e := r.write("probe.jsonl", []byte(probe.String())); e != nil {
		return e
	}
	for _, v := range []struct {
		name  string
		value any
	}{{"incident.json", o.incident}, {"pods.json", o.pods}, {"env.json", env}} {
		if e := r.writeJSON(v.name, v.value); e != nil {
			return e
		}
	}
	for _, role := range []string{"probe", "incident", "pods", "env"} {
		name := role + ".json"
		if role == "probe" {
			name = "probe.jsonl"
		}
		for _, f := range r.manifest.Files {
			if f.Ref == name {
				o.evidence.Sources = append(o.evidence.Sources, RawSource{Role: role, Ref: name, SHA256: f.SHA256})
			}
		}
	}
	o.selectProofs(env)
	if e := r.writeJSON("evidence.json", o.evidence); e != nil {
		return e
	}
	// Reserve the report destination exclusively. The existing report writes into
	// this private run directory; initial permissions survive its os.WriteFile.
	summary := filepath.Join(r.dir, "summary.json")
	f, e := os.OpenFile(summary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New("observer: summary already exists")
	}
	if e = f.Close(); e != nil {
		return e
	}
	path := func(s string) string { return filepath.Join(r.dir, s) }
	args := []string{"-acceptance", "runtime-v1", "-evidence", path("evidence.json"), "-probe", path("probe.jsonl"), "-incident", path("incident.json"), "-pods", path("pods.json"), "-env", path("env.json"), "-inject-at", o.evidence.FaultRequested.At.Format(time.RFC3339Nano), "-mode", modeEnforce, "-scenario", "runtime-observer", "-expect-node", o.c.ExpectNode, "-expect-version", o.c.ExpectVersion, "-platform-digests", strings.Join(o.c.PlatformDigests, ","), "-out", summary}
	reportErr := runReport(args, io.Discard)
	sf, se := os.OpenFile(summary, os.O_RDWR, 0600)
	if se != nil {
		return errors.New("observer: summary unavailable")
	}
	se = sf.Sync()
	ce := sf.Close()
	if se != nil || ce != nil {
		return errors.New("observer: summary sync failed")
	}
	b, e := observerLocalFile(summary, maxInputBytes)
	if e != nil {
		return e
	}
	var result AcceptanceResult
	if strictDecode(b, &result, true) != nil {
		return errors.New("observer: report did not produce a valid result")
	}
	r.manifest.Files = append(r.manifest.Files, observerFile{Ref: "summary.json", SHA256: digestBytes(b), Bytes: len(b)})
	r.manifest.Acceptance = "FAIL/UNPROVEN"
	if result.OverallPass {
		r.manifest.Acceptance = "PASS"
	}
	if e = r.finish(true); e != nil {
		return e
	}
	if _, e = fmt.Fprintf(w, "collector=%s acceptance=%s liveProof=false run=%s\n", r.manifest.Collector, r.manifest.Acceptance, o.c.RunID); e != nil {
		return e
	}
	return reportErr
}
