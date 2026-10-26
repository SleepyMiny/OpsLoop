package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This executable is a synthetic virsh-argv server. It never imports libvirt or
// calls virsh; the absolute Python interpreter only reads this test's state.
func installFinishFixture(t *testing.T, s *finishScene) string {
	t.Helper()
	s.c.TimeoutMS = 1000
	s.c.DeadlineMS = 10000
	s.c.PollMS = 100
	state := filepath.Join(filepath.Dir(s.c.Executable), "fixture-state.json")
	code := fmt.Sprintf(`#!/usr/bin/python3
import os,sys,json
assert set(os.environ) <= {'LANG','LC_ALL','LC_CTYPE'}, 'inherited environment'
assert sys.argv[1:3] == ['--connect','test:///opsloop-p2d']
assert len(sys.argv)==5 and sys.argv[4]==%q
p=%q
try:
 d=json.load(open(p))
except FileNotFoundError:
 d={'state':'paused','resumes':0,'calls':[]}
op=sys.argv[3]
assert op in ['domuuid','domstate','resume']
d['calls'].append(sys.argv[1:])
if op=='resume':
 d['resumes']+=1
 d['state']='running'
with open(p,'w') as f: json.dump(d,f);f.flush();os.fsync(f.fileno())
print(%q if op=='domuuid' else d['state'] if op=='domstate' else 'resumed')
`, finishTestUUID, state, finishTestUUID)
	if e := os.WriteFile(s.c.Executable, []byte(code), 0700); e != nil {
		t.Fatal(e)
	}
	s.c.ExecutableSHA256 = digestBytes([]byte(code))
	return state
}
func finishCleanEnv() []string { return []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"} }
func runFinishCLI(t *testing.T, c FinishConfig, args ...string) ([]byte, error) {
	t.Helper()
	cli := os.Getenv("P2D_CLI")
	if cli == "" {
		t.Skip("set P2D_CLI to the newly built local binary for executable-boundary tests")
	}
	config := filepath.Join(filepath.Dir(c.Executable), "finish-config.json")
	observerTestJSON(t, config, c)
	argv := append([]string{"finish-run", "-config", config}, args...)
	cmd := exec.Command(cli, argv...)
	cmd.Env = finishCleanEnv()
	b, e := cmd.CombinedOutput()
	if bytes.Contains(b, []byte(finishTestToken)) {
		t.Fatal("credential in CLI output")
	}
	return b, e
}
func TestFinishExecutableCLI(t *testing.T) {
	s := newFinishScene(t, "normal")
	state := installFinishFixture(t, s)
	if b, e := runFinishCLI(t, s.c); e != nil || !bytes.Contains(b, []byte("writes=0 vmCommands=0")) {
		t.Fatalf("plan %s %v", b, e)
	}
	if _, e := os.Stat(state); !os.IsNotExist(e) || s.patches != 0 {
		t.Fatal("plan launched fixture or wrote API")
	}
	if b, e := runFinishCLI(t, s.c, "-execute", "-confirm", s.c.binding()); e != nil {
		t.Fatalf("execute %s %v", b, e)
	}
	before, _ := os.ReadFile(s.c.ReceiptFile)
	if b, e := runFinishCLI(t, s.c, "-execute", "-confirm", s.c.binding()); e != nil {
		t.Fatalf("redelivery %s %v", b, e)
	}
	after, _ := os.ReadFile(s.c.ReceiptFile)
	var st struct {
		Resumes int
		Calls   [][]string
	}
	if e := readJSON(state, &st); e != nil {
		t.Fatal(e)
	}
	if st.Resumes != 1 || s.patches != 1 || !bytes.Equal(before, after) {
		t.Fatal("CLI redelivery repeated action")
	}
	entries, _ := os.ReadDir(s.c.JournalDir)
	for _, entry := range entries {
		b, _ := os.ReadFile(filepath.Join(s.c.JournalDir, entry.Name()))
		if bytes.Contains(b, []byte(finishTestToken)) {
			t.Fatal("credential in journal")
		}
	}
	t.Logf("actual CLI HTTP patches=%d fixture resumes=%d argv commands=%d", s.patches, st.Resumes, len(st.Calls))
	// An executable hash change is blocked before the altered program runs.
	if e := os.WriteFile(s.c.Executable, []byte("#!/bin/false\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := (fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}).Run(context.Background(), []string{"--connect", s.c.VMURI, "resume", s.c.VMUUID}); e == nil {
		t.Fatal("changed executable accepted")
	}
}

type finishCrashClock struct {
	j    *finishJournal
	kind string
}

func (c *finishCrashClock) Now() time.Time {
	if c.kind == "stop-before-command" && c.j != nil && c.j.last("stop-dispatch") != nil && c.j.last("stop-response") == nil {
		os.Exit(83)
	}
	return time.Now()
}
func (c *finishCrashClock) Sleep(ctx context.Context, d time.Duration) error {
	return observerWallClock{}.Sleep(ctx, d)
}
func TestFinishCrashProcess(t *testing.T) {
	if config := os.Getenv("P2D_CHILD_CONFIG"); config != "" {
		var c FinishConfig
		if e := readJSON(config, &c); e != nil {
			os.Exit(84)
		}
		kind := os.Getenv("P2D_CRASH_KIND")
		clock := &finishCrashClock{kind: kind}
		e := executeFinish(context.Background(), c, io.Discard, clock, fixtureVMRunner{c.Executable, c.ExecutableSHA256}, func(j *finishJournal) {
			clock.j = j
			j.before = func(k string) error {
				fire := k == kind
				if kind == "resume-before-command" {
					fire = k == "vm-call" && j.last("resume-dispatch") != nil && j.last("resume-response") == nil
				}
				if fire {
					os.Exit(83)
				}
				return nil
			}
		})
		if e != nil {
			os.Exit(85)
		}
		os.Exit(86)
	}
	if os.Getenv("P2D_CLI") == "" {
		t.Skip("set P2D_CLI for process-crash executable reentry tests")
	}
	for _, kind := range []string{"stop-intent", "stop-dispatch", "stop-before-command", "stop-response", "stop-confirmed", "resume-intent", "resume-dispatch", "resume-before-command", "resume-response", "resume-confirmed", "postchecks-confirmed", "cleanup-confirmed", "receipt-ready", "receipt-published"} {
		t.Run(kind, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			config := filepath.Join(filepath.Dir(s.c.Executable), "child-config.json")
			observerTestJSON(t, config, s.c)
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(exe, "-test.run=^TestFinishCrashProcess$")
			cmd.Env = append(finishCleanEnv(), "P2D_CHILD_CONFIG="+config, "P2D_CRASH_KIND="+kind)
			b, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errorsAsExit(e, &exit) || exit.ExitCode() != 83 {
				t.Fatalf("child did not interrupt: %s %v", b, e)
			}
			before := s.patches
			b, e = runFinishCLI(t, s.c, "-execute", "-confirm", s.c.binding())
			uncertain := kind == "stop-before-command" || kind == "resume-before-command"
			// stop-response crash happens after applied HTTP effect; querying confirms it.
			if kind == "stop-response" {
				uncertain = false
			}
			if (e != nil) != uncertain {
				t.Fatalf("reentry %s %v", b, e)
			}
			if before > 0 && s.patches != before {
				t.Fatal("HTTP replay after process death")
			}
			var st struct{ Resumes int }
			if data, e := os.ReadFile(state); e == nil {
				json.Unmarshal(data, &st)
			}
			if st.Resumes > 1 || s.patches > 1 {
				t.Fatal("duplicate effect after process death")
			}
			if uncertain {
				r := finishReceipt(t, s.c)
				if r.Cleanup.Status == "succeeded" {
					t.Fatal("unknown effect hidden")
				}
			}
			t.Logf("crash=%s HTTP patches=%d fixture resumes=%d reentryUnconfirmed=%t", kind, s.patches, st.Resumes, uncertain)
		})
	}
}
func errorsAsExit(err error, exit **exec.ExitError) bool {
	v, ok := err.(*exec.ExitError)
	if ok {
		*exit = v
	}
	return ok
}
func TestFinishReceiptAndLock(t *testing.T) {
	for _, variant := range []string{"other-receipt", "locked", "disk-failure", "receipt-failure", "unknown-dispatch"} {
		t.Run(variant, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			switch variant {
			case "other-receipt":
				if e := s.execute(nil); e != nil {
					t.Fatal(e)
				}
				r := finishReceipt(t, s.c)
				r.RunID = "other"
				observerTestJSON(t, s.c.ReceiptFile, r)
				before := s.patches
				if s.execute(nil) == nil || s.patches != before {
					t.Fatal("foreign receipt accepted")
				}
			case "locked":
				j, e := openFinishJournal(s.c, observerWallClock{}, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer j.close()
				if s.execute(nil) == nil || s.patches != 0 {
					t.Fatal("lock accepted")
				}
			case "disk-failure":
				err := s.execute(func(j *finishJournal) {
					j.before = func(kind string) error {
						if kind == "stop-dispatch" {
							p := filepath.Join(j.dir, fmt.Sprintf("event-%06d.json", len(j.events)+1))
							return os.WriteFile(p, []byte("torn"), 0600)
						}
						return nil
					}
				})
				if err == nil || s.patches != 0 {
					t.Fatal("write failure did not block dispatch")
				}
				if s.execute(nil) == nil {
					t.Fatal("torn journal ignored")
				}
			case "receipt-failure":
				err := s.execute(func(j *finishJournal) {
					j.before = func(kind string) error {
						if kind == "cleanup-confirmed" {
							return os.Mkdir(s.c.ReceiptFile, 0700)
						}
						return nil
					}
				})
				if err == nil || s.vm.resumes != 1 || s.patches != 1 {
					t.Fatal("post-effect receipt error hidden", err)
				}
			case "unknown-dispatch":
				err := s.execute(func(j *finishJournal) {
					j.before = func(kind string) error {
						if kind == "stop-response" {
							return fmt.Errorf("simulated response ledger failure")
						}
						return nil
					}
				})
				if err == nil {
					t.Fatal("journal failure hidden")
				}
				if e := s.execute(nil); e != nil {
					t.Fatal(e)
				}
				r := finishReceipt(t, s.c)
				if r.StopRequested != nil {
					t.Fatal("crashed request timestamp invented")
				}
			}
		})
	}
}
func TestFinishClientWriteBoundary(t *testing.T) {
	s := newFinishScene(t, "normal")
	api, e := newFinishClient(s.c.APIOrigin, s.c.APICAFile, finishTestToken, s.c.path("recoverypolicies", s.c.Policy), map[string]bool{}, s.c.timeout())
	if e != nil {
		t.Fatal(e)
	}
	defer api.client.CloseIdleConnections()
	for _, path := range []string{"/api/v1/secrets", s.c.path("deployments", s.c.Workload), s.c.path("recoverypolicies", "other")} {
		_, status, problem := api.request(context.Background(), path, []byte(`{}`))
		if status != 0 || problem != "write-boundary" {
			t.Fatal("write escaped boundary")
		}
	}
	if s.patches != 0 {
		t.Fatal("boundary touched API")
	}
	// The Observer's independent transport still rejects PATCH at its own guard.
	client, g, e := observerClient(s.c.APIOrigin, s.c.APICAFile, finishTestToken, s.c.timeout())
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()
	path := s.c.path("recoverypolicies", s.c.Policy)
	g.allowed[path] = true
	r, _ := http.NewRequest("PATCH", s.c.APIOrigin+path, strings.NewReader(`{}`))
	if _, e = client.Do(r); e == nil {
		t.Fatal("Observer write boundary relaxed")
	}
}

func TestFinishCredentialReflection(t *testing.T) {
	s := newFinishScene(t, "normal")
	s.p.ResourceVersion = finishTestToken
	if e := s.execute(nil); e == nil {
		t.Fatal("credential reflection accepted")
	}
	if s.patches != 0 || s.vm.resumes != 0 {
		t.Fatal("mutation after logging failure")
	}
	entries, _ := os.ReadDir(s.c.JournalDir)
	for _, entry := range entries {
		b, _ := os.ReadFile(filepath.Join(s.c.JournalDir, entry.Name()))
		if bytes.Contains(b, []byte(finishTestToken)) {
			t.Fatal("credential persisted")
		}
	}
}
