// Derived from review ZIP repro/independent_review_test.go; assertions below
// enforce fixed expectations, not the original defect-observing assertions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only test-owned synthetic processes; adopt orphaned children so cleanup can reap them.
func TestFinishFixDeadline(t *testing.T) {
	for _, mode := range []string{"normal", "direct-wait", "child-parent-waits", "child-parent-exits", "child-stderr-exits", "child-io-grace"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "fixture")
			pidfile := filepath.Join(dir, "child.pid")
			code := fmt.Sprintf("#!/usr/bin/python3\nimport subprocess,time\nmode=%q\nif mode.startswith('child'):\n c=subprocess.Popen(['/usr/bin/python3','-c','import time; time.sleep(1.2)'],stdout=subprocess.DEVNULL if mode=='child-stderr-exits' else None)\n open(%q,'w').write(str(c.pid))\n if mode=='child-parent-waits': c.wait()\nif mode=='direct-wait': time.sleep(1.2)\nprint('running',flush=True)\n", mode, pidfile)
			if e := os.WriteFile(p, []byte(code), 0700); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() {
				if b, e := os.ReadFile(pidfile); e == nil {
					pid, _ := strconv.Atoi(string(b))
					_ = syscall.Kill(pid, syscall.SIGKILL)
					var ws syscall.WaitStatus
					_, _ = syscall.Wait4(pid, &ws, 0, nil)
				}
			})
			limit := 100 * time.Millisecond
			if mode == "child-io-grace" {
				limit = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			start := time.Now()
			out, err := (fixtureVMRunner{p, digestBytes([]byte(code))}).Run(ctx, []string{"--connect", "test:///opsloop-p2d", "domstate", finishTestUUID})
			elapsed := time.Since(start)
			t.Logf("mode=%s timeoutMs=%d elapsedMs=%.3f output=%q err=%v contextErr=%v fixtureSHA256=%s", mode, limit.Milliseconds(), float64(elapsed.Microseconds())/1000, out, err, ctx.Err(), digestBytes([]byte(code)))
			if elapsed > 3*time.Second {
				t.Fatal("outer bound exceeded")
			}
			switch mode {
			case "normal":
				if err != nil || out != "running" {
					t.Fatal("normal control")
				}
			case "direct-wait":
				if err == nil {
					t.Fatal("timeout control")
				}
			case "child-parent-waits", "child-parent-exits", "child-stderr-exits", "child-io-grace":
				if mode == "child-io-grace" && ctx.Err() != nil {
					t.Fatal("I/O grace case reached command deadline")
				}
				if err == nil || elapsed > 450*time.Millisecond || out != "" {
					t.Fatal("command/drain bound or late success")
				}
				data, e := os.ReadFile(pidfile)
				if e != nil {
					t.Fatal(e)
				}
				pid, _ := strconv.Atoi(string(data))
				// Assert product termination/reaping BEFORE the fallback harness cleanup.
				if e = syscall.Kill(pid, 0); e != syscall.ESRCH {
					t.Fatalf("product left child alive/unreaped: %v", e)
				}
				var ws syscall.WaitStatus
				if _, e = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); e != syscall.ECHILD {
					t.Fatalf("product did not reap adopted child: %v", e)
				}

			}
		})
	}
}

func fixCLI(t *testing.T, c FinishConfig) ([]byte, int) {
	t.Helper()
	config := filepath.Join(filepath.Dir(c.Executable), "independent-config.json")
	observerTestJSON(t, config, c)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("P2D_CLI"), "finish-run", "-config", config, "-execute", "-confirm", c.binding())
	cmd.Env = finishCleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 200 * time.Millisecond
	b, e := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		t.Fatal("CLI outer timeout", ctx.Err())
	}
	code := 0
	if e != nil {
		if x, ok := e.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else {
			t.Fatal(e)
		}
	}
	if bytes.Contains(b, []byte(finishTestToken)) {
		t.Fatal("credential output")
	}
	return b, code
}
func fixCounts(t *testing.T, s *finishScene, state string) (int, int) {
	t.Helper()
	var st struct{ Resumes int }
	if e := readJSON(state, &st); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.patches, st.Resumes
}
func TestFinishFixReceipt(t *testing.T) {
	for _, mode := range []string{"redelivery", "deleted", "hash-mismatch", "foreign", "contradicted"} {
		t.Run(mode, func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			b, code := fixCLI(t, s.c)
			if code != 0 {
				t.Fatalf("initial %d %s", code, b)
			}
			before, e := os.ReadFile(s.c.ReceiptFile)
			if e != nil {
				t.Fatal(e)
			}
			originalEvents := map[string]string{}
			entries, _ := os.ReadDir(s.c.JournalDir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "event-") {
					data, _ := os.ReadFile(filepath.Join(s.c.JournalDir, entry.Name()))
					originalEvents[entry.Name()] = digestBytes(data)
				}
			}
			switch mode {
			case "deleted":
				if e = os.Remove(s.c.ReceiptFile); e != nil {
					t.Fatal(e)
				}
			case "hash-mismatch":
				e = os.WriteFile(s.c.ReceiptFile, append(append([]byte{}, before...), '\n'), 0600)
			case "foreign":
				var r ObserverReceipts
				json.Unmarshal(before, &r)
				r.RunID = "foreign-run"
				observerTestJSON(t, s.c.ReceiptFile, r)
			case "contradicted":
				s.mu.Lock()
				s.p.Spec.Enabled = true
				s.p.Generation++
				s.mu.Unlock()
			}
			if e != nil {
				t.Fatal(e)
			}
			pre, e := os.ReadFile(s.c.ReceiptFile)
			preExists := e == nil
			b, code = fixCLI(t, s.c)
			after, readErr := os.ReadFile(s.c.ReceiptFile)
			p, r := fixCounts(t, s, state)
			var receipt ObserverReceipts
			_ = json.Unmarshal(after, &receipt)
			cleanup := "missing"
			if receipt.Cleanup != nil {
				cleanup = receipt.Cleanup.Status
			}
			t.Logf("mode=%s exit=%d receiptExists=%t cleanup=%s equalOriginal=%t unchangedInput=%t patches=%d resumes=%d originalSHA256=%s afterSHA256=%s output=%q", mode, code, readErr == nil, cleanup, bytes.Equal(before, after), preExists && bytes.Equal(pre, after), p, r, digestBytes(before), digestBytes(after), strings.TrimSpace(string(b)))
			if p != 1 || r != 1 {
				t.Fatal("duplicate effect")
			}
			if mode == "contradicted" {
				preserved := true
				for name, h := range originalEvents {
					data, _ := os.ReadFile(filepath.Join(s.c.JournalDir, name))
					if digestBytes(data) != h {
						preserved = false
					}
				}
				archived := false
				entries, _ := os.ReadDir(s.c.JournalDir)
				for _, entry := range entries {
					data, _ := os.ReadFile(filepath.Join(s.c.JournalDir, entry.Name()))
					if bytes.Equal(data, before) {
						archived = true
					}
				}
				t.Logf("contradiction originalEvents=%d originalEventsUnchanged=%t originalReceiptArchived=%t", len(originalEvents), preserved, archived)
				if !preserved || !archived {
					t.Fatal("original event/receipt preservation failed")
				}
				if root := os.Getenv("P2D_FIX_OUT"); root != "" {
					hashes, e := json.MarshalIndent(originalEvents, "", "  ")
					if e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(filepath.Join(root, "original-event-hashes.json"), hashes, 0600); e != nil {
						t.Fatal(e)
					}
					archive, e := os.ReadFile(filepath.Join(s.c.JournalDir, "archive-"+digestBytes(before)+".json"))
					if e != nil || !bytes.Equal(archive, before) {
						t.Fatal("product archive mismatch", e)
					}
					if e = os.WriteFile(filepath.Join(root, "product-archive.json"), archive, 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(filepath.Join(root, "contradiction-before.json"), before, 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(filepath.Join(root, "contradiction-after.json"), after, 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
			fixSave(t, "receipt-"+mode, s.c, before)
			switch mode {
			case "redelivery":
				if code != 0 || !bytes.Equal(before, after) {
					t.Fatal("redelivery control")
				}
			case "deleted":
				if code == 0 || !os.IsNotExist(readErr) {
					t.Fatal("missing published receipt must fail closed")
				}
			case "hash-mismatch", "foreign":
				if code == 0 || !bytes.Equal(pre, after) {
					t.Fatal("fail closed control")
				}
			case "contradicted":
				if code == 0 || cleanup != "pending" || bytes.Equal(before, after) {
					t.Fatal("contradiction replacement not reproduced")
				}
			}
		})
	}
}

// Whole CLI: a resume applies its synthetic effect, then its exited parent leaves
// inherited output open. The next CLI invocation must query and not resend resume.
func TestFinishFixDeadlineCLI(t *testing.T) {
	s := newFinishScene(t, "normal")
	state := installFinishFixture(t, s)
	data, e := os.ReadFile(s.c.Executable)
	if e != nil {
		t.Fatal(e)
	}
	data = append(data, []byte("\nif op=='resume':\n import subprocess\n c=subprocess.Popen(['/usr/bin/python3','-c','import time; time.sleep(1.2)'])\n c.wait()\n")...)
	if e = os.WriteFile(s.c.Executable, data, 0700); e != nil {
		t.Fatal(e)
	}
	s.c.ExecutableSHA256 = digestBytes(data)
	s.c.TimeoutMS = 1000
	s.c.DeadlineMS = 700
	s.c.PollMS = 10
	config := filepath.Join(filepath.Dir(s.c.Executable), "independent-config.json")
	observerTestJSON(t, config, s.c)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("P2D_CLI"), "finish-run", "-config", config, "-execute", "-confirm", s.c.binding())
	cmd.Env = finishCleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 200 * time.Millisecond
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(1100 * time.Millisecond):
		t.Fatal("finish did not return within deadline+cleanup")
	}
	elapsed := time.Since(start)
	if remain := 850*time.Millisecond - time.Since(start); remain > 0 {
		time.Sleep(remain)
	}
	f, e := os.OpenFile(filepath.Join(s.c.JournalDir, "lock"), os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lockErr == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	f.Close()
	e = runErr
	t.Logf("first elapsedMs=%.3f deadlineMs=700 lockAt850ms=%v err=%v output=%q", float64(elapsed.Microseconds())/1000, lockErr, e, strings.TrimSpace(out.String()))
	if ctx.Err() != nil || e == nil || elapsed > 1100*time.Millisecond || lockErr != nil {
		t.Fatal("finish deadline/lock release failed")
	}
	b, code := fixCLI(t, s.c)
	p, r := fixCounts(t, s, state)
	t.Logf("reentry exit=%d patches=%d resumes=%d output=%q", code, p, r, strings.TrimSpace(string(b)))
	if code != 0 || p != 1 || r != 1 {
		t.Fatal("effect reconciliation/no duplicate regression")
	}
	fixSave(t, "deadline-reentry", s.c, nil)
}

// A trusted negative fixture changes only its own temporary file after the
// initial domstate query. The runner must reject the next hash before exec.
func TestFinishFixPreExecReceipt(t *testing.T) {
	s := newFinishScene(t, "normal")
	state := installFinishFixture(t, s)
	data, e := os.ReadFile(s.c.Executable)
	if e != nil {
		t.Fatal(e)
	}
	data = append(data, []byte("\nif op=='domstate' and d['state']=='paused':\n with open(__file__,'a') as f: f.write('# changed after query\\n')\n")...)
	if e = os.WriteFile(s.c.Executable, data, 0700); e != nil {
		t.Fatal(e)
	}
	s.c.ExecutableSHA256 = digestBytes(data)
	b, code := fixCLI(t, s.c)
	p, r := fixCounts(t, s, state)
	receipt := finishReceipt(t, s.c)
	var st struct{ Calls [][]string }
	if e = readJSON(state, &st); e != nil {
		t.Fatal(e)
	}
	t.Logf("preExecHashRejection exit=%d patches=%d resumes=%d executableCalls=%v receiptInterventions=%d output=%q", code, p, r, st.Calls, len(receipt.Interventions), strings.TrimSpace(string(b)))
	if code == 0 || p != 1 || r != 0 || len(st.Calls) != 2 || len(receipt.Interventions) != 0 {
		t.Fatal("pre-exec rejection fabricated restore receipt")
	}
	if root := os.Getenv("P2D_FIX_OUT"); root != "" {
		data, e := os.ReadFile(s.c.ReceiptFile)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(root, "pre-exec-receipt.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
