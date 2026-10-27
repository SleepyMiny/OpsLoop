package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixSave(t *testing.T, name string, c FinishConfig, before []byte) {
	t.Helper()
	root := os.Getenv("P2D_FIX_OUT")
	if root == "" {
		return
	}
	dir := filepath.Join(root, name)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if before != nil {
		if e := os.WriteFile(filepath.Join(dir, "before.json"), before, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if data, e := os.ReadFile(filepath.Join(filepath.Dir(c.Executable), "fixture-state.json")); e == nil {
		if e = os.WriteFile(filepath.Join(dir, "fixture-state.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	entries, e := os.ReadDir(c.JournalDir)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if entry.Name() == "lock" || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		b, e := os.ReadFile(filepath.Join(c.JournalDir, entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte(finishTestToken)) {
			t.Fatal("credential reflection")
		}
		if e = os.WriteFile(filepath.Join(dir, entry.Name()), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
func fixCountsAndPublications(t *testing.T, s *finishScene, state string) (int, int, int, int) {
	t.Helper()
	p, r := fixCounts(t, s, state)
	var st struct{ Calls [][]string }
	if e := readJSON(state, &st); e != nil {
		t.Fatal(e)
	}
	n := 0
	entries, _ := os.ReadDir(s.c.JournalDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "event-") {
			var event finishEvent
			if e := readJSON(filepath.Join(s.c.JournalDir, entry.Name()), &event); e != nil {
				t.Fatal(e)
			}
			if event.Kind == "receipt-published" {
				n++
			}
			if event.Kind == "vm-start-attempt" {
				var start finishVMStart
				if json.Unmarshal(event.Data, &start) != nil || !stampValid(start.BeforeStart) || stampValid(start.AfterStart) {
					t.Fatal("attempt contains false post-Start timestamp")
				}
			}
		}
	}
	return p, r, len(st.Calls), n
}
func fixContradict(s *finishScene, owner bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner {
		s.d.Annotations["opsloop.io/execution-fence"] = `{"version":2,"policyUID":"other"}`
	} else {
		s.p.Spec.Enabled = true
		s.p.Generation++
	}
}

func TestFinishFixArchiveFailures(t *testing.T) {
	for _, kind := range []string{"owner", "archive-collision", "collision", "foreign", "tamper", "symlink", "unknown", "archive-before", "archive-before-file-sync", "archive-after-file-sync", "archive-before-dir-sync", "archive-after-dir-sync", "archive-after-sync", "receipt-ready", "receipt-before-rename", "receipt-after-rename", "receipt-after-sync", "receipt-published", "receipt-published-durable"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			if kind == "owner" {
				s.c.DeadlineMS = 1500
				s.c.PollMS = 10
			}
			if b, code := fixCLI(t, s.c); code != 0 {
				t.Fatalf("initial %s", b)
			}
			before, e := os.ReadFile(s.c.ReceiptFile)
			if e != nil {
				t.Fatal(e)
			}
			fixContradict(s, kind == "owner")
			archive := filepath.Join(s.c.JournalDir, "archive-"+digestBytes(before)+".json")
			switch kind {
			case "collision":
				e = os.WriteFile(archive, []byte("collision"), 0600)
			case "foreign":
				var r ObserverReceipts
				json.Unmarshal(before, &r)
				r.RunID = "foreign"
				b, _ := json.Marshal(r)
				e = os.WriteFile(archive, b, 0600)
			case "tamper":
				e = os.WriteFile(archive, append(append([]byte{}, before...), '\n'), 0600)
			case "symlink":
				e = os.Symlink(s.c.ReceiptFile, archive)
			case "unknown":
				e = os.WriteFile(filepath.Join(s.c.JournalDir, "archive-"+strings.Repeat("0", 64)+".json"), before, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			fired := false
			err := executeFinish(context.Background(), s.c, io.Discard, observerWallClock{}, fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}, func(j *finishJournal) {
				j.before = func(k string) error {
					if kind == "archive-collision" && k == "archive-before" {
						fired = true
						return os.Mkdir(archive, 0700)
					}
					if k == kind {
						fired = true
						return errors.New("test persistence boundary failure")
					}
					return nil
				}
			})
			if err == nil {
				t.Fatal("contradiction/failed archive returned success")
			}
			after, _ := os.ReadFile(s.c.ReceiptFile)
			if !fired && strings.Contains(kind, "-") {
				t.Fatal("boundary not reached", kind, err)
			}
			replacePossible := kind == "owner" || kind == "receipt-after-rename" || kind == "receipt-after-sync" || kind == "receipt-published" || kind == "receipt-published-durable"
			if !replacePossible && !bytes.Equal(before, after) {
				t.Fatal("replaced before durable archive")
			}
			if replacePossible {
				b, e := os.ReadFile(archive)
				if e != nil || !bytes.Equal(b, before) {
					t.Fatal("lost original product archive", e)
				}
			}
			// Each state either verifies and continues its non-success verdict or fails
			// closed on publication mismatch. It must never replay or claim stale success.
			b, code := fixCLI(t, s.c)
			if code == 0 {
				t.Fatalf("stale success after boundary: %s", b)
			}
			p, r, calls, pubs := fixCountsAndPublications(t, s, state)
			if p != 1 || r != 1 {
				t.Fatal("duplicate effects", p, r)
			}
			t.Logf("boundary=%s fired=%t exit=nonzero PATCH=%d resume=%d commands=%d publications=%d originalSHA256=%s currentSHA256=%s", kind, fired, p, r, calls, pubs, digestBytes(before), digestBytes(after))
			fixSave(t, "archive-"+kind, s.c, before)
		})
	}
}

func TestFinishFixReceiptPrePublication(t *testing.T) {
	s := newFinishScene(t, "normal")
	state := installFinishFixture(t, s)
	e := executeFinish(context.Background(), s.c, io.Discard, observerWallClock{}, fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}, func(j *finishJournal) {
		j.before = func(k string) error {
			if k == "receipt-ready" {
				return errors.New("interrupted first publication")
			}
			return nil
		}
	})
	if e == nil {
		t.Fatal("no interruption")
	}
	if _, e = os.Lstat(s.c.ReceiptFile); !os.IsNotExist(e) {
		t.Fatal("receipt exists before first rename")
	}
	if b, code := fixCLI(t, s.c); code != 0 {
		t.Fatalf("first publication recovery %s", b)
	}
	p, r, calls, pubs := fixCountsAndPublications(t, s, state)
	if p != 1 || r != 1 || pubs != 1 {
		t.Fatal("replayed effects/publication", p, r, pubs)
	}
	t.Logf("first-ready interruption -> exit=0 PATCH=%d resume=%d commands=%d publications=%d", p, r, calls, pubs)
	fixSave(t, "first-publication", s.c, nil)
}

func TestFinishFixPreStart(t *testing.T) {
	for _, kind := range []string{"missing", "hash", "cancel", "start-failure", "normal", "nonzero", "timeout", "timeout-effect", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			// Product runner preflight sees the changed/missing executable only after
			// the initial UUID/paused query, at the resume write-ahead boundary.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reachedResumeBoundary := false
			hook := func(j *finishJournal) {
				j.before = func(k string) error {
					if k == "resume-dispatch-durable" {
						reachedResumeBoundary = true
						switch kind {
						case "missing":
							return os.Remove(s.c.Executable)
						case "hash":
							return os.WriteFile(s.c.Executable, []byte("changed"), 0700)
						case "cancel":
							cancel()
						case "start-failure":
							return os.Remove(filepath.Join(filepath.Dir(s.c.Executable), "interpreter"))
						case "deadline":
							time.Sleep(time.Duration(s.c.DeadlineMS) * time.Millisecond)
						}
					}
					return nil
				}
			}
			if kind == "start-failure" {
				// Valid hash and executable permission; missing interpreter fails Start.
				data, _ := os.ReadFile(s.c.Executable)
				interpreter := filepath.Join(filepath.Dir(s.c.Executable), "interpreter")
				if e := os.Symlink("/usr/bin/python3", interpreter); e != nil {
					t.Fatal(e)
				}
				data = bytes.Replace(data, []byte("#!/usr/bin/python3"), []byte("#!"+interpreter), 1)
				os.WriteFile(s.c.Executable, data, 0700)
				s.c.ExecutableSHA256 = digestBytes(data)
			}
			if kind == "nonzero" || kind == "timeout" || kind == "timeout-effect" {
				data, _ := os.ReadFile(s.c.Executable)
				// Timeout without effect executes before state update. The effect variant
				// blocks after applying it and is confirmed by independent fresh queries.
				if kind == "timeout" {
					data = bytes.Replace(data, []byte("if op=='resume':"), []byte("if op=='resume':\n import time;time.sleep(1.2)"), 1)
				} else if kind == "timeout-effect" {
					data = append(data, []byte("\nif op=='resume':\n import time;time.sleep(1.2)\n")...)
				} else {
					data = append(data, []byte("\nif op=='resume': sys.exit(7)\n")...)
				}
				os.WriteFile(s.c.Executable, data, 0700)
				s.c.ExecutableSHA256 = digestBytes(data)
				s.c.TimeoutMS = 150
			}
			if kind == "deadline" {
				s.c.DeadlineMS = 2000
				s.c.PollMS = 10
			}
			err := executeFinish(ctx, s.c, io.Discard, observerWallClock{}, fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}, hook)
			if kind == "deadline" && !reachedResumeBoundary {
				t.Fatal("deadline fixture did not reach the intended resume boundary")
			}
			// Every negative preflight case reaches the resume path; no actual
			// restore is requested when preflight or Start rejects it.
			r := finishReceipt(t, s.c)
			pre := kind == "missing" || kind == "hash" || kind == "cancel" || kind == "start-failure" || kind == "deadline"
			if pre {
				if err == nil || len(r.Interventions) != 0 {
					t.Fatal("fabricated pre-start request", err)
				}
			} else {
				if len(r.Interventions) != 1 || !stampValid(r.Interventions[0].Requested) {
					t.Fatal("missing actual Start evidence")
				}
				var event finishEvent
				if e := readJSON(filepath.Join(s.c.JournalDir, r.Interventions[0].Ref), &event); e != nil {
					t.Fatal(e)
				}
				var st finishVMStart
				json.Unmarshal(event.Data, &st)
				if event.Kind != "vm-started" || st.Operation != "resume" || st.UUID != s.c.VMUUID || st.Dispatch == 0 || st.Call <= st.Dispatch || !st.AfterStart.At.Equal(r.Interventions[0].Requested.At) {
					t.Fatal("request unrelated to Start")
				}
				if kind == "timeout" {
					if err == nil || r.Interventions[0].Confirmed != nil {
						t.Fatal("unconfirmed effect promoted")
					}
				} else if err != nil || r.Interventions[0].Confirmed == nil {
					t.Fatal("fresh independent effect not confirmed", err)
				}
			}
			p, resumes, calls, pubs := fixCountsAndPublications(t, s, state)
			if pre && resumes != 0 {
				t.Fatal("pre-start resume executed")
			}
			t.Logf("kind=%s nonzero=%t interventions=%d PATCH=%d resumeEffects=%d commands=%d publications=%d", kind, err != nil, len(r.Interventions), p, resumes, calls, pubs)
			fixSave(t, "dispatch-"+kind, s.c, nil)
		})
	}
}

// Abrupt process exits exercise the real runner/journal seam. An exit between
// Start and the durable marker may leave effect unknown; it never permits replay.
func TestFinishFixProcessBoundaries(t *testing.T) {
	if config := os.Getenv("P2D_FIX_CHILD"); config != "" {
		var c FinishConfig
		if readJSON(config, &c) != nil {
			os.Exit(91)
		}
		kind := os.Getenv("P2D_FIX_BOUNDARY")
		_ = executeFinish(context.Background(), c, io.Discard, observerWallClock{}, fixtureVMRunner{c.Executable, c.ExecutableSHA256}, func(j *finishJournal) {
			j.before = func(k string) error {
				if k == kind {
					if strings.HasPrefix(kind, "vm-") && (j.last("resume-dispatch") == nil || j.last("resume-response") != nil) {
						return nil
					}
					os.Exit(83)
				}
				return nil
			}
		})
		os.Exit(92)
	}
	// Harness adoption is only for a deliberately killed executor that cannot
	// clean up after os.Exit. Normal runner cleanup is asserted separately before
	// fallback cleanup; this harness never supplies product Start/effect evidence.
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); e != 0 {
		t.Fatal(e)
	}
	for _, kind := range []string{"vm-start-attempt", "vm-start-attempt-durable", "vm-started", "vm-started-durable", "archive-before", "archive-before-file-sync", "archive-after-file-sync", "archive-before-dir-sync", "archive-after-dir-sync", "archive-after-sync", "receipt-ready", "receipt-before-rename", "receipt-after-rename", "receipt-after-sync", "receipt-published", "receipt-published-durable"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			pidLog := filepath.Join(filepath.Dir(s.c.Executable), "owned-fixture-pids")
			data, e := os.ReadFile(s.c.Executable)
			if e != nil {
				t.Fatal(e)
			}
			data = bytes.Replace(data, []byte("import os,sys,json"), []byte(fmt.Sprintf("import os,sys,json\nwith open(%q,'a') as f: f.write(str(os.getpid())+'\\n')", pidLog)), 1)
			if e = os.WriteFile(s.c.Executable, data, 0700); e != nil {
				t.Fatal(e)
			}
			s.c.ExecutableSHA256 = digestBytes(data)
			t.Cleanup(func() { fixReapCrashFixtures(t, pidLog) })
			archiveCase := !strings.HasPrefix(kind, "vm-")
			var before []byte
			if archiveCase {
				if b, code := fixCLI(t, s.c); code != 0 {
					t.Fatal(string(b))
				}
				before, _ = os.ReadFile(s.c.ReceiptFile)
				fixContradict(s, false)
			}
			config := filepath.Join(filepath.Dir(s.c.Executable), "boundary-config.json")
			observerTestJSON(t, config, s.c)
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestFinishFixProcessBoundaries$")
			cmd.Env = append(finishCleanEnv(), "P2D_FIX_CHILD="+config, "P2D_FIX_BOUNDARY="+kind)
			b, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errorsAsExit(e, &exit) || exit.ExitCode() != 83 {
				t.Fatalf("boundary child %s %v", b, e)
			}
			// Started child is a bounded fixture with no descendants, even when CLI dies.
			if kind == "vm-started" || kind == "vm-started-durable" {
				time.Sleep(100 * time.Millisecond)
			}
			b, code := fixCLI(t, s.c)
			p, resumes, calls, pubs := fixCountsAndPublications(t, s, state)
			if p != 1 || resumes > 1 {
				t.Fatal("duplicate operation after process death", p, resumes)
			}
			r := finishReceipt(t, s.c)
			if archiveCase {
				if code == 0 {
					t.Fatal("stale success")
				}
				current, _ := os.ReadFile(s.c.ReceiptFile)
				if !bytes.Equal(current, before) {
					a, e := os.ReadFile(filepath.Join(s.c.JournalDir, "archive-"+digestBytes(before)+".json"))
					if e != nil || !bytes.Equal(a, before) {
						t.Fatal("original bytes lost")
					}
				}
			} else {
				startedMarker := false
				entries, _ := os.ReadDir(s.c.JournalDir)
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "event-") {
						var event finishEvent
						readJSON(filepath.Join(s.c.JournalDir, entry.Name()), &event)
						var st finishVMStart
						json.Unmarshal(event.Data, &st)
						if event.Kind == "vm-started" && st.Operation == "resume" {
							startedMarker = true
						}
					}
				}
				if !startedMarker && len(r.Interventions) != 0 {
					t.Fatal("missing Start marker promoted")
				}
				if strings.HasPrefix(kind, "vm-start-attempt") && (resumes != 0 || code == 0) {
					t.Fatal("pre Start replayed")
				}
			}
			t.Logf("process boundary=%s exit=%d PATCH=%d resume=%d commands=%d publications=%d output=%q", kind, code, p, resumes, calls, pubs, strings.TrimSpace(string(b)))
			fixSave(t, "process-"+kind, s.c, before)
		})
	}
}

func TestFinishFixReceiptAbnormal(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			if b, code := fixCLI(t, s.c); code != 0 {
				t.Fatal(string(b))
			}
			before, _ := os.ReadFile(s.c.ReceiptFile)
			if e := os.Remove(s.c.ReceiptFile); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "symlink":
				target := filepath.Join(filepath.Dir(s.c.Executable), "old-receipt.json")
				if e := os.WriteFile(target, before, 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, s.c.ReceiptFile); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Mkdir(s.c.ReceiptFile, 0700); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				if e := syscall.Mkfifo(s.c.ReceiptFile, 0600); e != nil {
					t.Fatal(e)
				}
			}
			info, _ := os.Lstat(s.c.ReceiptFile)
			b, code := fixCLI(t, s.c)
			if code == 0 {
				t.Fatalf("abnormal receipt accepted %s", b)
			}
			after, e := os.Lstat(s.c.ReceiptFile)
			if e != nil || after.Mode() != info.Mode() {
				t.Fatal("abnormal artifact changed")
			}
			p, r, calls, pubs := fixCountsAndPublications(t, s, state)
			if p != 1 || r != 1 || pubs != 1 {
				t.Fatal("abnormal receipt repeated effects/publication")
			}
			t.Logf("receipt=%s exit=%d PATCH=%d resume=%d commands=%d publications=%d fileMode=%s", kind, code, p, r, calls, pubs, after.Mode())
		})
	}
}

// Reap only known PIDs written by this test's approved fixture. The harness
// does not scan the host, kill unrelated groups, or hide runner cleanup failures.
func fixReapCrashFixtures(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Error(err)
		return
	}
	reaped := 0
	for _, line := range strings.Fields(string(b)) {
		pid, e := strconv.Atoi(line)
		if e != nil || pid <= 0 {
			t.Error("invalid owned PID")
			continue
		}
		until := time.Now().Add(1500 * time.Millisecond)
		killed := false
		for {
			var status syscall.WaitStatus
			got, e := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if e == syscall.ECHILD {
				break
			} // already reaped by the product's direct Wait
			if e != nil && e != syscall.EINTR {
				t.Error(e)
				break
			}
			if got == pid {
				reaped++
				break
			}
			if !time.Now().Before(until) {
				if killed {
					t.Error("owned crash fixture did not terminate/reap")
					break
				}
				_ = syscall.Kill(pid, syscall.SIGKILL)
				killed = true
				until = time.Now().Add(100 * time.Millisecond)
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Logf("harness-only crash orphan reap=%d (after product assertions)", reaped)
}
