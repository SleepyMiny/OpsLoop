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
	"syscall"
	"time"
)

// Each immutable, fsynced event is a write-ahead boundary. A torn event blocks
// reentry; it is never discarded or interpreted as permission to repeat a write.
// flock coordinates this local adapter only, not administrators/other writers.
type finishEvent struct {
	Seq      int             `json:"seq"`
	ActionID string          `json:"actionID"`
	Kind     string          `json:"kind"`
	At       Stamp           `json:"at"`
	Data     json.RawMessage `json:"data"`
}
type finishJournal struct {
	dir, binding, segment string
	lock                  *os.File
	events                []finishEvent
	clock                 observerClock
	origin                time.Time
	forbidden             []byte
	// A test may fail any persistence boundary. Production always uses disk.
	failed bool
	before func(string) error
}

func syncFinishDir(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func finishExclusive(path string, b []byte, checkpoint ...func(string) error) error {
	seam := func(kind string) error {
		if len(checkpoint) > 0 {
			return checkpoint[0](kind)
		}
		return nil
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = seam("archive-before-file-sync")
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = seam("archive-after-file-sync")
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = seam("archive-before-dir-sync")
	}
	if e == nil {
		e = syncFinishDir(filepath.Dir(path))
	}
	if e == nil {
		e = seam("archive-after-dir-sync")
	}
	return e
}
func openFinishJournal(c FinishConfig, clock observerClock, forbidden []byte) (*finishJournal, error) {
	dir := c.JournalDir
	if e := os.Mkdir(dir, 0700); e == nil {
		if e = syncFinishDir(filepath.Dir(dir)); e != nil {
			return nil, errors.New("finish: journal parent sync failed")
		}
	} else if !os.IsExist(e) {
		return nil, errors.New("finish: journal unavailable")
	}
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("finish: private journal directory required")
	}
	f, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("finish: journal locked")
	}
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		f.Close()
		return nil, e
	}
	j := &finishJournal{dir: dir, binding: c.binding(), segment: hex.EncodeToString(id[:]), lock: f, clock: clock, origin: clock.Now(), forbidden: forbidden}
	fail := func() (*finishJournal, error) {
		j.close()
		return nil, errors.New("finish: journal corrupt or identity mismatch; manual review required")
	}
	var archives []string
	entries, e := os.ReadDir(dir)
	if e != nil {
		return fail()
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "archive-") {
			archives = append(archives, entry.Name())
			continue
		}
		if strings.HasPrefix(entry.Name(), "receipt-") && strings.HasSuffix(entry.Name(), ".tmp") {
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fail()
			}
			b, err := observerLocalFile(filepath.Join(dir, entry.Name()), 1<<20)
			var r ObserverReceipts
			if err != nil || strictDecode(b, &r, true) != nil || r.RunID != c.RunID || r.EvidenceKind != c.evidenceKind() {
				return fail()
			}
			continue
		}
		if entry.Name() == "lock" || entry.Name() == "receipts.json" {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "event-") || entry.Type()&os.ModeSymlink != 0 {
			return fail()
		}
		b, e := observerLocalFile(filepath.Join(dir, entry.Name()), 1<<20)
		if e != nil {
			return fail()
		}
		var v finishEvent
		if strictDecode(b, &v, true) != nil || v.Seq != len(j.events)+1 || v.ActionID != j.binding || entry.Name() != fmt.Sprintf("event-%06d.json", v.Seq) || !stampValid(v.At) {
			return fail()
		}
		j.events = append(j.events, v)
	}
	for _, name := range archives {
		if _, err := j.archiveBytes(c, name); err != nil {
			return fail()
		}
	}
	if info, err := os.Lstat(c.ReceiptFile); err == nil {
		if !info.Mode().IsRegular() {
			return fail()
		}
		b, err := observerLocalFile(c.ReceiptFile, 1<<20)
		var r ObserverReceipts
		if err != nil || strictDecode(b, &r, true) != nil || r.RunID != c.RunID || r.EvidenceKind != c.evidenceKind() {
			return fail()
		}
		if published := j.last("receipt-published"); published != nil {
			var v struct{ Status, SHA256 string }
			if json.Unmarshal(published.Data, &v) != nil || v.SHA256 != digestBytes(b) {
				return fail()
			}
		}
	} else if !os.IsNotExist(err) {
		return fail()
	} else if j.last("receipt-published") != nil {
		j.close()
		return nil, errors.New("finish: published receipt missing; undelivered, manual review required")
	}
	if len(j.events) == 0 {
		if e = j.add("run-bound", struct {
			Binding, Environment, RunID, ClusterUID, Namespace, NamespaceUID, Policy, PolicyUID, Workload, DeploymentUID, VMURI, VMUUID, APIOrigin string
			PolicyGeneration, DeadlineMS                                                                                                           int64
		}{j.binding, c.Environment, c.RunID, c.ClusterUID, c.Namespace, c.NamespaceUID, c.Policy, c.PolicyUID, c.Workload, c.DeploymentUID, c.VMURI, c.VMUUID, c.APIOrigin, c.PolicyGeneration, c.DeadlineMS}); e != nil {
			j.close()
			return nil, e
		}
	}
	return j, nil
}
func (j *finishJournal) close() {
	if j.lock != nil {
		_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
		_ = j.lock.Close()
	}
}
func (j *finishJournal) add(kind string, v any) error {
	return j.addWhileActive(nil, kind, v)
}

// Only cleanup confirmation supplies ctx. Its decision is made after the
// pre-record boundary and before capturing the event time. Once accepted,
// writing/syncing that fact and publishing it may finish after cancellation.
// Rejecting a new decision is not journal corruption: failure can still be
// recorded and prior true events remain immutable.
func (j *finishJournal) addWhileActive(ctx context.Context, kind string, v any) (err error) {
	if j.failed {
		return errors.New("finish: journal previously failed; no further actions allowed")
	}
	if j.before != nil {
		if e := j.before(kind); e != nil {
			j.failed = true
			return e
		}
	}
	if ctx != nil {
		if e := ctx.Err(); e != nil {
			return e
		}
	}
	defer func() {
		if err != nil {
			j.failed = true
		}
	}()
	at := j.clock.Now()
	ms := at.Sub(j.origin).Seconds() * 1000
	if ms < 0 {
		return errors.New("finish: clock moved backwards")
	}
	data, e := json.Marshal(v)
	if e != nil {
		return e
	}
	event := finishEvent{Seq: len(j.events) + 1, ActionID: j.binding, Kind: kind, At: Stamp{Recorder: "finish-executor", Segment: j.segment, Sequence: int64(len(j.events) + 1), ElapsedMS: &ms, At: at.UTC()}, Data: data}
	b, e := json.MarshalIndent(event, "", "  ")
	if e != nil {
		return e
	}
	if len(j.forbidden) > 0 && strings.Contains(string(b), string(j.forbidden)) {
		return errors.New("finish: credential reflection blocked")
	}
	if len(j.events) >= 5000 {
		return errors.New("finish: journal event limit")
	}
	if ctx != nil {
		if e = j.boundary(kind + "-before-write"); e != nil {
			return e
		}
	}
	if e = finishExclusive(filepath.Join(j.dir, fmt.Sprintf("event-%06d.json", event.Seq)), b); e != nil {
		return errors.New("finish: journal write failed; effects may be unconfirmed, recovery/manual review required")
	}
	j.events = append(j.events, event)
	return j.boundary(kind + "-durable")
}
func (j *finishJournal) last(kind string) *finishEvent {
	for i := len(j.events) - 1; i >= 0; i-- {
		if j.events[i].Kind == kind {
			return &j.events[i]
		}
	}
	return nil
}

func (j *finishJournal) capture() Stamp {
	at := j.clock.Now()
	ms := at.Sub(j.origin).Seconds() * 1000
	return Stamp{Recorder: "finish-executor", Segment: j.segment, Sequence: int64(len(j.events) + 1), ElapsedMS: &ms, At: at.UTC()}
}

// boundary exposes persistence/Start seams to local interruption tests. No
// boundary grants permission to repeat an ambiguous external operation.
func (j *finishJournal) boundary(kind string) error {
	if j.failed {
		return errors.New("finish: journal previously failed")
	}
	if j.before != nil {
		if e := j.before(kind); e != nil {
			j.failed = true
			return e
		}
	}
	return nil
}
func (j *finishJournal) archiveBytes(c FinishConfig, name string) ([]byte, error) {
	h := strings.TrimSuffix(strings.TrimPrefix(name, "archive-"), ".json")
	decoded, err := hex.DecodeString(h)
	if err != nil || len(decoded) != 32 || name != "archive-"+h+".json" {
		return nil, errors.New("invalid archive name")
	}
	known := false
	for _, event := range j.events {
		if event.Kind == "receipt-published" {
			var v struct{ Status, SHA256 string }
			if json.Unmarshal(event.Data, &v) == nil && v.SHA256 == h {
				known = true
			}
		}
	}
	if !known {
		return nil, errors.New("archive lacks publication in this action")
	}
	path := filepath.Join(j.dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("archive must be regular file")
	}
	b, err := observerLocalFile(path, 1<<20)
	var r ObserverReceipts
	if err != nil || digestBytes(b) != h || strictDecode(b, &r, true) != nil || r.RunID != c.RunID || r.EvidenceKind != c.evidenceKind() {
		return nil, errors.New("archive hash/identity mismatch")
	}
	return b, nil
}
func (x *finisher) archivePublication() error {
	published := x.j.last("receipt-published")
	if published == nil {
		return nil
	}
	var v struct{ Status, SHA256 string }
	if json.Unmarshal(published.Data, &v) != nil {
		return errors.New("finish: invalid publication")
	}
	b, err := observerLocalFile(x.c.ReceiptFile, 1<<20)
	if err != nil || digestBytes(b) != v.SHA256 {
		return errors.New("finish: prior receipt unavailable; manual review required")
	}
	if err = x.j.boundary("archive-before"); err != nil {
		return err
	}
	name := "archive-" + v.SHA256 + ".json"
	path := filepath.Join(x.j.dir, name)
	if err = finishExclusive(path, b, x.j.boundary); err != nil && !os.IsExist(err) {
		return errors.New("finish: receipt archive persistence failed; current verdict non-success, manual review required")
	}
	archived, err := x.j.archiveBytes(x.c, name)
	if err != nil || !bytes.Equal(archived, b) {
		return errors.New("finish: receipt archive collision; current verdict non-success, manual review required")
	}
	// Even an existing identical archive must be synced before replacement.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = syncFinishDir(x.j.dir); err != nil {
		return err
	}
	return x.j.boundary("archive-after-sync")
}
