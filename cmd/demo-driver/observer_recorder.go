package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type observerClock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}
type observerWallClock struct{}

func (observerWallClock) Now() time.Time { return time.Now() }
func (observerWallClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type observerFile struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type observerManifest struct {
	Version           string               `json:"version"`
	RunID             string               `json:"runID"`
	Source            string               `json:"source"`
	EvidenceKind      string               `json:"evidenceKind"`
	Recorder          string               `json:"recorder"`
	Segment           string               `json:"segment"`
	State             string               `json:"state"`
	Collector         string               `json:"collector"`
	Acceptance        string               `json:"acceptance"`
	LiveProof         bool                 `json:"liveProof"`
	Issues            []string             `json:"issues"`
	Diagnostics       []observerDiagnostic `json:"diagnostics"`
	CollectionHorizon Stamp                `json:"collectionHorizon"`
	Files             []observerFile       `json:"files"`
	Pins              map[string]string    `json:"pinnedUIDs"`
}
type observerSink interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type observerRecorder struct {
	open          func(string) (observerSink, error)
	forbidden     []byte
	totalBytes    int
	dir           string
	clock         observerClock
	origin        time.Time
	last          time.Time
	seq           int64
	rawSeq        int
	contextAt     Stamp
	contextRef    string
	contextTarget string
	finalized     bool
	manifest      observerManifest
}

func newObserverRecorder(c ObserverConfig, clock observerClock) (*observerRecorder, error) {
	// Mkdir (not MkdirAll) plus exclusive file creation prevents mixing an old
	// summary with a new/partial run. No resume or overwrite is supported.
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return nil, e
	}
	dir := filepath.Join(c.OutputRoot, c.RunID)
	if e := os.Mkdir(dir, 0700); e != nil {
		return nil, errors.New("observer: run directory exists or cannot be created")
	}
	r := &observerRecorder{dir: dir, clock: clock, origin: clock.Now(), manifest: observerManifest{Version: "opsloop-observer/v1", RunID: c.RunID, Source: c.Source, EvidenceKind: c.EvidenceKind, Recorder: "observer-" + hex.EncodeToString(id[:8]), Segment: hex.EncodeToString(id[8:]), State: "incomplete", Collector: "incomplete", Acceptance: "not-run", Pins: map[string]string{}, Issues: []string{}}}
	if e := r.writeJSON("started.json", r.manifest); e != nil {
		return nil, e
	}
	return r, nil
}
func (r *observerRecorder) stamp(ctx context.Context) (Stamp, error) {
	// Millisecond report schema: wait for a distinct observable tick rather than
	// invent elapsed values. Time is sampled during capture, never on reprocessing.
	now := r.clock.Now()
	for !now.UTC().Truncate(time.Millisecond).After(r.last) {
		if e := r.clock.Sleep(ctx, time.Millisecond); e != nil {
			return Stamp{}, e
		}
		now = r.clock.Now()
	}
	r.last = now.UTC().Truncate(time.Millisecond)
	r.seq++
	ms := float64(now.Sub(r.origin).Nanoseconds() / int64(time.Millisecond))
	return Stamp{Recorder: r.manifest.Recorder, Segment: r.manifest.Segment, Sequence: r.seq, ElapsedMS: &ms, At: r.last}, nil
}
func (r *observerRecorder) write(name string, b []byte) error {
	if len(b) > maxInputBytes || r.totalBytes+len(b) > 64<<20 {
		return errors.New("observer: evidence storage budget exceeded")
	}
	if len(r.forbidden) > 0 && bytes.Contains(b, r.forbidden) {
		return errors.New("observer: credential reflection blocked")
	}
	var f observerSink
	var e error
	if r.open != nil {
		f, e = r.open(filepath.Join(r.dir, name))
	} else {
		f, e = os.OpenFile(filepath.Join(r.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if e != nil {
		return errors.New("observer: exclusive evidence create failed")
	}
	n, e := f.Write(b)
	if e == nil && n != len(b) {
		e = errors.New("short write")
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("observer: evidence write failed; partial file preserved")
	}
	r.totalBytes += len(b)
	r.manifest.Files = append(r.manifest.Files, observerFile{Ref: name, SHA256: digestBytes(b), Bytes: len(b)})
	return nil
}
func (r *observerRecorder) writeJSON(name string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return errors.New("observer: serialization failed")
	}
	return r.write(name, append(b, '\n'))
}
func (r *observerRecorder) raw(kind, path string, at Stamp, status int, problem string, v any) (string, error) {
	r.rawSeq++
	ref := fmt.Sprintf("raw-%04d.json", r.rawSeq)
	e := r.writeJSON(ref, struct {
		Kind       string `json:"kind"`
		Path       string `json:"path"`
		Observed   Stamp  `json:"observed"`
		HTTPStatus int    `json:"httpStatus"`
		Problem    string `json:"problem,omitempty"`
		Projection any    `json:"projection,omitempty"`
	}{kind, path, at, status, problem, v})
	r.contextAt, r.contextRef, r.contextTarget = at, ref, path
	return ref, e
}
func (r *observerRecorder) issue(s string) {
	proof := "apply"
	if strings.HasPrefix(s, "external-receipts") || strings.HasPrefix(s, "stop-request") || strings.HasPrefix(s, "cleanup-receipt") {
		proof = "receipts"
	}
	class := "contradiction"
	if strings.HasPrefix(s, "service-measurement:") {
		class = "gap"
	}
	if s == "attempt-identity-changed" || s == "collection-interrupted-or-write-failed" {
		class = "fatal"
	}
	r.diagnose(s, class, r.contextTarget, r.contextAt, r.contextRef, proof)
}
func (r *observerRecorder) diagnose(s, class, target string, at Stamp, ref, proof string) {
	r.manifest.Diagnostics = append(r.manifest.Diagnostics, observerDiagnostic{Reason: s, Class: class, Target: target, Observed: at, Ref: ref, Proof: proof})
	if class == "link" {
		return
	}
	for _, old := range r.manifest.Issues {
		if old == s {
			return
		}
	}
	r.manifest.Issues = append(r.manifest.Issues, s)
}
func (r *observerRecorder) pin(path, uid string) error {
	if uid == "" {
		return errors.New("object-uid-missing")
	}
	if old, ok := r.manifest.Pins[path]; ok && old != uid {
		return errors.New("target-uid-replaced")
	}
	r.manifest.Pins[path] = uid
	return nil
}
func (r *observerRecorder) finish(complete bool) error {
	if r.finalized {
		return errors.New("observer: duplicate finalize")
	}
	r.finalized = true
	if complete {
		r.manifest.State = "complete"
		r.manifest.Collector = "complete"
		if len(r.manifest.Issues) > 0 {
			r.manifest.Collector = "complete-with-gaps"
		}
	}
	// manifest is written last. COMPLETE certifies successful file capture, never
	// acceptance or live recovery. started.json survives crashes and write errors.
	if e := r.writeJSON("manifest.json", r.manifest); e != nil {
		return e
	}
	if complete {
		if e := os.Link(filepath.Join(r.dir, "manifest.json"), filepath.Join(r.dir, "COMPLETE")); e != nil {
			return errors.New("observer: completion publication failed")
		}
		return nil
	}
	return nil
}
