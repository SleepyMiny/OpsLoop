//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Regressions for P3B-IR-01/02: product Finish, real flock, synthetic API only.
func finishDeadlineResult(t *testing.T, v map[string]any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	t.Log(string(b))
	if root := os.Getenv("P3B_FIX_EVIDENCE"); root != "" {
		if e := os.WriteFile(filepath.Join(root, fmt.Sprintf("%s.json", v["id"])), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
func finishDeadlineJournalReleased(t *testing.T, c FinishConfig) bool {
	t.Helper()
	f, e := os.OpenFile(filepath.Join(c.JournalDir, "lock"), os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if e == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	return e == nil
}
func TestFinishLabDeadlineTargetLock(t *testing.T) {
	for _, tc := range []struct{ id, stage, mode string }{
		{"A-deadline-release", "restore", "deadline"}, {"A-cancel-release", "restore", "cancel"},
		{"B-deadline-release", "final", "deadline"}, {"B-cancel-release", "final", "cancel"},
		{"C-canceled-free-final", "final", "already-canceled"}, {"C-canceled-free-restore", "restore", "already-canceled"},
		{"D-valid-acquire", "restore", "valid"},
		{"E-restore-held", "restore", "held"}, {"E-final-held", "final", "held"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			l := newLabFinish(t, "normal", nil)
			guardBefore, _ := os.ReadFile(recoveryStatePath(l.c))
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deadline time.Time
			if tc.mode == "deadline" {
				deadline = time.Now().Add(2 * time.Second)
				parent, cancel = context.WithDeadline(context.Background(), deadline)
				defer cancel()
			}
			var g *guardStore
			var trigger time.Time
			var jref *finishJournal
			var released chan struct{}
			fired := false
			snapshots := 0
			hook := func(j *finishJournal) {
				jref = j
				j.before = func(kind string) error {
					hit := false
					if tc.stage == "restore" && j.last("stop-confirmed") != nil && kind == "snapshot-durable" {
						snapshots++
						hit = snapshots == 2
					}
					if tc.stage == "final" && j.last("postchecks-confirmed") != nil && kind == "snapshot-durable" {
						snapshots++
						hit = snapshots == 2
					}
					if !hit || fired {
						return nil
					}
					fired = true
					trigger = time.Now()
					if parent.Err() != nil {
						t.Fatal("expired before target boundary")
					}
					if tc.mode == "already-canceled" {
						cancel()
						return nil
					}
					var e error
					g, e = openGuard(l.c)
					if e != nil {
						t.Fatal(e)
					}
					if tc.mode == "held" {
						time.AfterFunc(60*time.Millisecond, cancel)
						return nil
					}
					delay := 300 * time.Millisecond
					if tc.mode == "deadline" {
						delay = time.Until(deadline) + 250*time.Millisecond
					}
					if tc.mode == "cancel" {
						time.AfterFunc(60*time.Millisecond, cancel)
					}
					if tc.mode == "valid" {
						delay = 50 * time.Millisecond
					}
					released = make(chan struct{})
					holder := g
					go func() { time.Sleep(delay); holder.close(); close(released) }()
					return nil
				}
			}
			start := time.Now()
			err := executeFinish(parent, l.s.c, io.Discard, observerWallClock{}, l.v, hook)
			end := time.Now()
			if !fired {
				t.Fatal("target seam not reached", err)
			}
			releasedJournal := finishDeadlineJournalReleased(t, l.s.c)
			if !releasedJournal {
				t.Fatal("journal lock leaked while target still held")
			}
			if tc.mode != "valid" {
				if err == nil || jref.last("cleanup-confirmed") != nil {
					t.Fatal("expired Finish succeeded", err)
				}
				if !errors.Is(err, parent.Err()) {
					t.Fatal("lost cancellation cause", err, parent.Err())
				}
				canceledAt := trigger.Add(60 * time.Millisecond)
				if tc.mode == "deadline" {
					canceledAt = deadline
				}
				if tc.mode == "already-canceled" {
					canceledAt = trigger
				}
				if end.Sub(canceledAt) > 500*time.Millisecond {
					t.Fatalf("slow cancellation response: %s", end.Sub(canceledAt))
				}
				if released != nil {
					select {
					case <-released:
						t.Fatal("Finish waited for holder release after cancellation")
					default:
					}
				}
			}
			heldStill := false
			if tc.mode == "held" {
				other, e := openGuard(l.c)
				heldStill = e == errGuardBusy
				if other != nil {
					other.close()
				}
				g.close()
			} else if released != nil {
				<-released
			}
			r := finishReceipt(t, l.s.c)
			commandsAfterCancel := 0
			var cleanupAt, incompleteAt time.Time
			for _, ev := range jref.events {
				if ev.Kind == "cleanup-confirmed" {
					cleanupAt = ev.At.At
				}
				if ev.Kind == "incomplete" {
					incompleteAt = ev.At.At
				}
				if ev.Kind == "vm-started" && tc.mode == "deadline" && ev.At.At.After(deadline) {
					commandsAfterCancel++
				}
			}
			releasedJournal = finishDeadlineJournalReleased(t, l.s.c)
			if !releasedJournal {
				t.Fatal("journal leaked")
			}
			if tc.stage == "restore" && tc.mode != "valid" && len(l.v.calls) != 0 {
				t.Fatal("commands after cancellation")
			}
			if tc.mode == "valid" && (err != nil || l.v.resumes != 1) {
				t.Fatal("normal control failed", err)
			}
			if tc.mode == "held" && !heldStill {
				t.Fatal("other actor lock disturbed")
			}
			if tc.mode != "valid" {
				if r.Cleanup.Status != "pending" {
					t.Fatal("non-success receipt required")
				}
				if tc.stage == "final" && (l.v.resumes != 1 || len(r.Interventions) != 1 || r.Interventions[0].Confirmed == nil) {
					t.Fatal("prior resume facts lost")
				}
				assertFinishPendingCompanion(t, l.s.c)
			}
			guardAfter, _ := os.ReadFile(recoveryStatePath(l.c))
			if !bytes.Equal(guardBefore, guardAfter) {
				t.Fatal("Guard state modified")
			}
			if tc.mode == "valid" && r.Cleanup.Status != "succeeded" {
				t.Fatal("normal receipt")
			}

			ep := ""
			if err != nil {
				ep = err.Error()
			}
			pe := ""
			if parent.Err() != nil {
				pe = parent.Err().Error()
			}
			cleanupMS := 0.0
			if !incompleteAt.IsZero() {
				cleanupMS = float64(end.Sub(incompleteAt).Microseconds()) / 1000
			}
			finishDeadlineResult(t, map[string]any{"id": tc.id, "error": ep, "parentError": pe, "elapsedMS": float64(end.Sub(start).Microseconds()) / 1000, "fromLockBoundaryMS": float64(end.Sub(trigger).Microseconds()) / 1000, "cleanupStatus": r.Cleanup.Status, "cleanupConfirmed": !cleanupAt.IsZero(), "cleanupAfterDeadline": !deadline.IsZero() && !cleanupAt.IsZero() && cleanupAt.After(deadline), "commandStarts": len(l.v.calls), "resumes": l.v.resumes, "patches": l.s.patches, "commandsAfterDeadline": commandsAfterCancel, "journalReleased": releasedJournal, "holderStillLockedBeforeOwnClose": heldStill, "failureRecordingAndPublicationMS": cleanupMS, "safeCancellation": tc.mode == "valid" || parent.Err() != nil && err != nil})
		})
	}
}

// Full freshly built CLI: hold the target flock in the separate test process
// during the final Service HTTP response, after restoreLab released its lock.
func TestFinishLabDeadlineCLI(t *testing.T) {
	cli := os.Getenv("P3B_CLI")
	if cli == "" {
		t.Fatal("fresh P3B_CLI required")
	}
	l := newLabFinish(t, "normal", nil)
	dir := filepath.Dir(l.s.c.Executable)
	target := recoveryFixture(t, dir, "normal")
	code, e := os.ReadFile(target.Executable)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(l.s.c.Executable, code, 0700); e != nil {
		t.Fatal(e)
	}
	l.s.c.ExecutableSHA256 = digestBytes(code)
	l.c.Target.SHA256 = l.s.c.ExecutableSHA256
	l.s.c.Live.GuardBinding = l.c.binding()
	started := time.Now()
	releaseAt := started
	releaseTimes := make(chan time.Time, 1)
	entered := make(chan time.Time, 1)
	released := make(chan struct{})
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Service bearer leak")
		}
		g, e := openGuard(l.c)
		if e != nil {
			t.Error(e)
			w.WriteHeader(503)
			return
		}
		entered <- time.Now()
		due := <-releaseTimes
		go func() { time.Sleep(time.Until(due)); g.close(); close(released) }()
		json.NewEncoder(w).Encode(map[string]string{"node": l.s.c.ExpectedNode, "pod": l.s.c.ExpectedPod.Name, "podUID": l.s.c.ExpectedPod.UID, "version": l.s.c.ExpectedVersion})
	}))
	defer svc.Close()
	l.s.c.ServiceURL = svc.URL + "/identity"
	l.mutateInitialFixture(t)
	cfg := filepath.Join(dir, "review-cli.json")
	recoveryWrite(t, cfg, l.s.c)
	cmd := exec.Command(cli, "finish-run", "-config", cfg, "-execute", "-confirm", l.s.c.binding())
	cmd.Env = finishCleanEnv()
	started = time.Now()
	releaseAt = started.Add(time.Duration(l.s.c.DeadlineMS)*time.Millisecond + 400*time.Millisecond)
	releaseTimes <- releaseAt
	out, err := cmd.CombinedOutput()
	ended := time.Now()
	if err == nil || !strings.Contains(string(out), "finish=GUARD_REJECTED") || strings.Contains(string(out), "CLEANUP_SUCCEEDED") {
		t.Fatalf("deadline CLI must fail: %v %s", err, out)
	}
	if !ended.Before(releaseAt) {
		t.Fatal("CLI waited for late target unlock")
	}
	if !finishDeadlineJournalReleased(t, l.s.c) {
		t.Fatal("journal held after CLI exit")
	}

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Service boundary not reached", string(out), err)
	}
	<-released
	calls, resumes := recoveryCalls(t, dir)
	r := finishReceipt(t, l.s.c)
	j, e := openFinishJournal(l.s.c, observerWallClock{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	confirmed := j.last("cleanup-confirmed")
	if confirmed != nil || r.Cleanup.Status != "pending" || resumes != 1 || len(r.Interventions) != 1 || r.Interventions[0].Confirmed == nil {
		t.Fatal("false success or lost resume fact")
	}
	assertFinishPendingCompanion(t, l.s.c)
	var incompleteMS float64
	if ev := j.last("incomplete"); ev != nil && ev.At.ElapsedMS != nil {
		incompleteMS = *ev.At.ElapsedMS
	}

	if root := os.Getenv("P3B_FIX_EVIDENCE"); root != "" {
		var selected []finishEvent
		for _, ev := range j.events {
			switch ev.Kind {
			case "run-bound", "stop-confirmed", "resume-confirmed", "postchecks-confirmed", "cleanup-confirmed", "incomplete", "receipt-published", "binding-published":
				selected = append(selected, ev)
			}
		}
		data, _ := json.MarshalIndent(map[string]any{"fullJournal": false, "omittedEvents": len(j.events) - len(selected), "events": selected}, "", "  ")
		if e = os.WriteFile(filepath.Join(root, "B-cli-selected-events.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
		data, e = os.ReadFile(l.s.c.ReceiptFile)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(root, "B-cli-receipt.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
		bs, _ := filepath.Glob(l.s.c.JournalDir + ".binding-*.json")
		if len(bs) != 1 {
			t.Fatal("binding count")
		}
		data, e = os.ReadFile(bs[0])
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(root, "B-cli-companion.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	j.close()
	ep := ""
	if err != nil {
		ep = err.Error()
	}
	finishDeadlineResult(t, map[string]any{"id": "B-full-CLI-deadline-release", "error": ep, "stdout": string(out), "elapsedMS": float64(ended.Sub(started).Microseconds()) / 1000, "configuredDeadlineMS": l.s.c.DeadlineMS, "holderReleaseAfterLaunchMS": float64(releaseAt.Sub(started).Microseconds()) / 1000, "cleanupStatus": r.Cleanup.Status, "cleanupConfirmed": confirmed != nil, "commandStarts": calls, "resumes": resumes, "patches": l.s.patches, "journalReleased": finishDeadlineJournalReleased(t, l.s.c), "nonzero": err != nil, "incompleteElapsedMS": incompleteMS, "failurePublicationAndExitMS": float64(ended.Sub(started).Microseconds())/1000 - incompleteMS})
}

func assertFinishPendingCompanion(t *testing.T, c FinishConfig) {
	t.Helper()
	paths, e := filepath.Glob(c.JournalDir + ".binding-*.json")
	if e != nil || len(paths) != 1 {
		t.Fatal("companion count", e, len(paths))
	}
	var binding struct {
		Status, ReceiptSHA256 string
		LiveProof             bool
	}
	b, e := os.ReadFile(paths[0])
	if e != nil || json.Unmarshal(b, &binding) != nil {
		t.Fatal("companion")
	}
	receipt, e := os.ReadFile(c.ReceiptFile)
	if e != nil {
		t.Fatal(e)
	}
	if binding.Status == "CLEANUP_SUCCEEDED" || binding.LiveProof || binding.ReceiptSHA256 != digestBytes(receipt) {
		t.Fatal("false success companion", binding)
	}
}

func TestFinishLabDeadlineDecision(t *testing.T) {
	for _, scene := range []string{"after-lock", "after-fresh", "late-decision", "late-write", "late-publication"} {
		t.Run(scene, func(t *testing.T) {
			l := newLabFinish(t, "normal", nil)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			fired := false
			var jref *finishJournal
			var decisionAt, writeStarted time.Time
			err := executeFinish(ctx, l.s.c, io.Discard, observerWallClock{}, l.v, func(j *finishJournal) {
				jref = j
				j.before = func(kind string) error {
					if fired {
						return nil
					}
					boundary := "cleanup-confirmed"
					switch scene {
					case "after-lock":
						boundary = "guard-observed"
					case "after-fresh":
						boundary = "guard-observed-durable"
					case "late-write":
						boundary = "cleanup-confirmed-before-write"
					case "late-publication":
						boundary = "receipt-before-rename"
					}
					if kind != boundary || j.last("postchecks-confirmed") == nil {
						return nil
					}
					fired = true
					if ctx.Err() != nil {
						t.Fatal("setup consumed deadline before boundary")
					}
					if scene == "after-lock" || scene == "after-fresh" {
						cancel()
					} else {
						decisionAt = time.Now()
						time.Sleep(time.Until(deadline) + 20*time.Millisecond)
						writeStarted = time.Now()
					}
					return nil
				}
			})
			if !fired {
				t.Fatal("decision boundary not reached", err)
			}
			success := scene == "late-write" || scene == "late-publication"
			r := finishReceipt(t, l.s.c)
			if (err == nil) != success || (r.Cleanup.Status == "succeeded") != success {
				t.Fatal("wrong decision/publication result", err, r.Cleanup.Status)
			}
			if l.v.resumes != 1 || len(r.Interventions) != 1 || r.Interventions[0].Confirmed == nil {
				t.Fatal("resume history lost")
			}
			confirmed := jref.last("cleanup-confirmed")
			if success {
				if confirmed == nil || !confirmed.At.At.Before(deadline) || !writeStarted.After(deadline) || !decisionAt.Before(deadline) {
					t.Fatal("late write not distinguished from late decision")
				}
			} else {
				if confirmed != nil {
					t.Fatal("late success decision")
				}
				assertFinishPendingCompanion(t, l.s.c)
			}
			if !finishDeadlineJournalReleased(t, l.s.c) {
				t.Fatal("journal lock leak")
			}
			finishDeadlineResult(t, map[string]any{"id": "decision-" + scene, "success": success, "cleanup": r.Cleanup.Status, "parentExpired": ctx.Err() != nil, "resumes": l.v.resumes, "acceptedFactPreserved": success && confirmed != nil, "writeAfterDeadline": writeStarted.After(deadline)})
		})
	}
}

func TestFinishLabDeadlinePriorSuccess(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	if e := l.execute(nil); e != nil {
		t.Fatal(e)
	}
	receipt, _ := os.ReadFile(l.s.c.ReceiptFile)
	j, e := openFinishJournal(l.s.c, observerWallClock{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	prior := *j.last("cleanup-confirmed")
	oldBytes, _ := os.ReadFile(filepath.Join(j.dir, fmt.Sprintf("event-%06d.json", prior.Seq)))
	j.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	err := executeFinish(ctx, l.s.c, io.Discard, observerWallClock{}, l.v, func(j *finishJournal) {
		j.before = func(k string) error {
			if !fired && k == "guard-observed" {
				fired = true
				cancel()
			}
			return nil
		}
	})
	if err == nil || !fired || l.v.resumes != 1 || finishReceipt(t, l.s.c).Cleanup.Status != "pending" {
		t.Fatal("reentry hid cancellation or replayed", err)
	}
	after, _ := os.ReadFile(filepath.Join(l.s.c.JournalDir, fmt.Sprintf("event-%06d.json", prior.Seq)))
	if !bytes.Equal(oldBytes, after) {
		t.Fatal("true history rewritten")
	}
	archived, e := os.ReadFile(filepath.Join(l.s.c.JournalDir, "archive-"+digestBytes(receipt)+".json"))
	if e != nil || !bytes.Equal(archived, receipt) {
		t.Fatal("original receipt not archived")
	}
	paths, _ := filepath.Glob(l.s.c.JournalDir + ".binding-*.json")
	successes := 0
	for _, p := range paths {
		var b struct{ Status string }
		data, _ := os.ReadFile(p)
		json.Unmarshal(data, &b)
		if b.Status == "CLEANUP_SUCCEEDED" {
			successes++
		}
	}
	if successes != 1 || len(paths) != 2 {
		t.Fatal("new success companion or lost old companion")
	}
	if !finishDeadlineJournalReleased(t, l.s.c) {
		t.Fatal("journal leak")
	}
}

func TestFinishLabDeadlineAcquireRace(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	before, _ := os.ReadFile(recoveryStatePath(l.c))
	holder, e := openGuard(l.c)
	if e != nil {
		t.Fatal(e)
	}
	lockPath := filepath.Join(holder.dir, "target.lock")
	original, e := os.Stat(lockPath)
	if e != nil {
		t.Fatal(e)
	}
	holder.close()
	countFDs := func() int {
		files, e := os.ReadDir("/proc/self/fd")
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		for _, f := range files {
			p, _ := os.Readlink(filepath.Join("/proc/self/fd", f.Name()))
			if p == lockPath {
				n++
			}
		}
		return n
	}
	fds := countFDs()
	canceled, acquired := 0, 0
	for i := 0; i < 64; i++ {
		g, e := openGuard(l.c)
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; cancel() }()
		go func() { defer wg.Done(); <-gate; g.close() }()
		close(gate)
		got, err := acquireFinishGuard(ctx, l.c)
		wg.Wait()
		if err != nil {
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("wrong canceled acquisition", err)
			}
			canceled++
		} else {
			if got == nil {
				t.Fatal("nil acquisition")
			}
			acquired++
			got.close()
		}
		next, e := openGuard(l.c)
		if e != nil {
			t.Fatal("acquisition leaked", e)
		}
		canceledCtx, stop := context.WithCancel(context.Background())
		stop()
		if other, e := acquireFinishGuard(canceledCtx, l.c); e != context.Canceled || other != nil {
			t.Fatal("canceled holder disturbed")
		}
		if other, e := openGuard(l.c); e != errGuardBusy {
			if other != nil {
				other.close()
			}
			t.Fatal("other actor lock was released")
		}
		next.close()
	}
	after, _ := os.ReadFile(recoveryStatePath(l.c))
	current, e := os.Stat(lockPath)
	if e != nil || !os.SameFile(original, current) || !bytes.Equal(before, after) || countFDs() != fds {
		t.Fatal("inode/state/descriptor changed")
	}
	finishDeadlineResult(t, map[string]any{"id": "acquire-race", "iterations": 64, "canceled": canceled, "acquired": acquired, "targetFDsBefore": fds, "targetFDsAfter": countFDs(), "inodePreserved": true, "guardStatePreserved": true})
}

func TestFinishLabDeadlineAcquireOwnLimit(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	c := l.c
	c.TimeoutMS = 10 // lock helper's original 2060ms cap, independent of loaded Guard state
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	start := time.Now()
	got, err := acquireFinishGuard(context.Background(), c)
	elapsed := time.Since(start)
	if got != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed < 2060*time.Millisecond || elapsed > 2560*time.Millisecond {
		t.Fatal("own acquisition cap", elapsed, err)
	}
	if other, e := openGuard(c); e != errGuardBusy {
		if other != nil {
			other.close()
		}
		t.Fatal("holder lost lock")
	}
}
