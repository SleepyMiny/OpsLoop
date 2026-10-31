//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type recoveryChild struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  string
}

func recoveryStart(t *testing.T, path string, args []string, env []string) *recoveryChild {
	t.Helper()
	p := filepath.Join(t.TempDir(), "process.log")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = append([]string{"LANG=C", "LC_ALL=C"}, env...)
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		f.Close()
		t.Fatal(e)
	}
	child := &recoveryChild{cmd: cmd, done: make(chan struct{}), log: p}
	go func() { child.err = cmd.Wait(); f.Close(); close(child.done) }()
	t.Cleanup(func() {
		select {
		case <-child.done:
		default:
			cmd.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(3 * time.Second):
				t.Error("child cleanup timeout")
			}
		}
	})
	return child
}
func (c *recoveryChild) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(8 * time.Second):
		b, _ := os.ReadFile(c.log)
		t.Fatalf("process timeout: %s", b)
	}
}
func recoveryEventually(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bounded condition timeout")
}
func recoveryCLI(t *testing.T) string {
	t.Helper()
	p := os.Getenv("P2E_CLI")
	if p == "" {
		t.Skip("P2E_CLI required for separate built CLI integration")
	}
	return p
}
func recoveryWrite(t *testing.T, p string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func recoveryFixture(t *testing.T, dir, scene string) recoveryTarget {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "vm-state"), []byte("paused"), 0600)
	// The fake implements only the very same production argv. It alone supplies
	// resume effects; harness cleanup never changes vm-state or supplies evidence.
	code := fmt.Sprintf(`#!/usr/bin/python3
import sys,os,json,time
root=%q
scene=%q
uuid=%q
assert sys.argv[1:3]==['--connect','qemu:///system'] and sys.argv[4:]==[uuid]
op=sys.argv[3]
assert op in ['domuuid','domstate','resume']
assert not any(k in os.environ for k in ['HOME','KUBECONFIG','LIBVIRT_DEFAULT_URI','HTTP_PROXY','HTTPS_PROXY','AWS_ACCESS_KEY_ID'])
with open(root+'/calls','a') as f:
 f.write(json.dumps({'op':op,'uuid':uuid,'uri':sys.argv[2]})+'\n');f.flush();os.fsync(f.fileno())
if op=='domuuid': print(uuid)
elif op=='domstate': print(open(root+'/vm-state').read())
else:
 if scene!='unconfirmed':
  with open(root+'/vm-state','w') as f: f.write('running');f.flush();os.fsync(f.fileno())
 if scene in ['lost-response','unconfirmed']: time.sleep(2)
`, dir, scene, finishTestUUID)
	path := filepath.Join(dir, "synthetic-command")
	if e := os.WriteFile(path, []byte(code), 0700); e != nil {
		t.Fatal(e)
	}
	return recoveryTarget{"qemu:///system", finishTestUUID, path, digestBytes([]byte(code))}
}
func recoveryCalls(t *testing.T, dir string) (int, int) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, "calls"))
	if os.IsNotExist(e) {
		return 0, 0
	}
	if e != nil {
		t.Fatal(e)
	}
	n, r := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var v map[string]string
		if e = json.Unmarshal([]byte(line), &v); e != nil {
			t.Fatal(e)
		}
		if v["uuid"] != finishTestUUID || v["uri"] != "qemu:///system" {
			t.Fatal("foreign target")
		}
		n++
		if v["op"] == "resume" {
			r++
		}
	}
	return n, r
}
func recoveryStatePath(c guardConfig) string {
	return filepath.Join(c.Root, digestBytes([]byte(c.Target.URI+"\n"+c.Target.UUID)), digestBytes([]byte(c.Run+"\n"+c.Epoch))+".json")
}
func recoveryLoad(t *testing.T, c guardConfig) guardState {
	t.Helper()
	var s guardState
	if e := guardRead(recoveryStatePath(c), &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func recoveryArgs(action, path string, c guardConfig, extra ...string) []string {
	return append([]string{"guard", action, "-config", path, "-execute", "-confirm", c.binding()}, extra...)
}

// A separate synthetic executor can perform a cooperating normal restoration.
// This is NOT a new normal finish-run backend: the production stop contract is
// exercised by TestFinish, unchanged. The helper is confined to this test binary.
func TestRecoveryProcessHelper(t *testing.T) {
	path := os.Getenv("P2E_HELPER_CONFIG")
	if path == "" {
		return
	}
	var c guardConfig
	if e := guardRead(path, &c); e != nil {
		t.Fatal(e)
	}
	gate := os.Getenv("P2E_HELPER_GATE")
	if mode := os.Getenv("P2E_HELPER_BOUNDARY"); mode != "" {
		recoveryWatchBoundaryHelper(t, c, gate, mode)
		return
	}
	recoveryEventually(t, func() bool { _, e := os.Stat(gate); return e == nil })
	var g *guardStore
	recoveryEventually(t, func() bool { var e error; g, e = openGuard(c); return e == nil })
	defer g.close()
	b := recoveryBackend{target: c.Target, runner: boundedVMRunner{c.Target.Executable, c.Target.SHA256}, timeout: time.Duration(c.TimeoutMS) * time.Millisecond, permit: func() error { return nil }, record: func(k, o, d string) error { return nil }}
	state, e := b.state(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if state == "paused" {
		if e = b.resume(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	state, e = b.state(context.Background())
	if e != nil || state != "running" {
		t.Fatal("normal effect unconfirmed", e)
	}
	// Stay alive while the parent sends explicit epoch-bound disarm.
	g.close()
	if e := os.WriteFile(gate+".confirmed", []byte("fresh running"), 0600); e != nil {
		t.Fatal(e)
	}
	time.Sleep(2 * time.Second)
}
func TestRecoverySeparateProcesses(t *testing.T) {
	cli := recoveryCLI(t)
	for _, scene := range []string{"plan", "normal", "executor-kill", "hang", "restart", "two-guards", "normal-race", "lost-response", "unconfirmed"} {
		t.Run(scene, func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			root := filepath.Join(dir, "guard")
			os.Mkdir(root, 0700)
			target := recoveryFixture(t, dir, scene)
			owner := recoveryStart(t, "/bin/sleep", []string{"30"}, nil)
			now, e := guardNow()
			if e != nil {
				t.Fatal(e)
			}
			identity, e := guardProcess(owner.cmd.Process.Pid, now.Boot)
			if e != nil {
				t.Fatal(e)
			}
			c := guardConfig{Version: "opsloop-guard/v1", Mode: "synthetic-local", Run: "process-" + scene, Epoch: "epoch-1", Target: target, Allowed: "query-resume-only", Root: root, Owner: identity, HardDeadline: now.Wall.Add(4 * time.Second), AuthorityExpires: now.Wall.Add(9 * time.Second), MaxTrialMS: 4000, HeartbeatMS: 1500, TimeoutMS: 300}
			config := filepath.Join(dir, "config.json")
			recoveryWrite(t, config, c)
			if scene == "plan" {
				p := recoveryStart(t, cli, []string{"guard", "arm", "-config", config}, nil)
				p.wait(t)
				if p.err != nil {
					t.Fatal(p.err)
				}
				if n, r := recoveryCalls(t, dir); n != 0 || r != 0 {
					t.Fatal("plan executed")
				}
				if _, e := os.Stat(recoveryStatePath(c)); !os.IsNotExist(e) {
					t.Fatal("plan armed")
				}
				return
			}
			var normal *recoveryChild
			gate := filepath.Join(dir, "normal-go")
			if scene == "normal" || scene == "normal-race" {
				// Config is read by helper after Start; owner identity still binds sleep.
				exe, e := os.Executable()
				if e != nil {
					t.Fatal(e)
				}
				normal = recoveryStart(t, exe, []string{"-test.run=^TestRecoveryProcessHelper$"}, []string{"P2E_HELPER_CONFIG=" + config, "P2E_HELPER_GATE=" + gate})
			}
			var guard *recoveryChild
			var armedDiagnostic *guardState
			armDetected, guardWaitObserved, ownerKillRequested := false, false, false
			if scene == "executor-kill" {
				// Log on PASS and failure before harness cleanup or TempDir removal.
				// This adds no startup I/O and never treats ARMED as a watch tick.
				defer func() {
					recoveryLogCLIDiagnostic(t, c, cli, owner, guard, armedDiagnostic, armDetected, guardWaitObserved, ownerKillRequested)
				}()
			}
			guard = recoveryStart(t, cli, recoveryArgs("arm", config, c), nil)
			recoveryEventually(t, func() bool { b, _ := os.ReadFile(guard.log); return strings.Contains(string(b), `"phase":"ARMED"`) })
			before := recoveryLoad(t, c)
			if scene == "executor-kill" {
				armDetected = true
				armedDiagnostic = &before
			}
			if n, r := recoveryCalls(t, dir); n != 0 || r != 0 {
				t.Fatal("arm itself queried/restored")
			}
			// Valid heartbeat then stale sequence, foreign epoch and changed UUID all fail.
			if scene == "normal" {
				for i, seq := range []string{"1", "1"} {
					p := recoveryStart(t, cli, recoveryArgs("heartbeat", config, c, "-sequence", seq), nil)
					p.wait(t)
					if (p.err == nil) != (i == 0) {
						t.Fatal("stale heartbeat acceptance", p.err)
					}
				}
				for _, foreign := range []string{"epoch", "uuid"} {
					cc := c
					if foreign == "epoch" {
						cc.Epoch = "other"
					} else {
						cc.Target.UUID = "00000000-0000-0000-0000-000000000000"
					}
					cp := filepath.Join(dir, foreign+".json")
					recoveryWrite(t, cp, cc)
					for _, action := range []string{"heartbeat", "disarm"} {
						p := recoveryStart(t, cli, recoveryArgs(action, cp, cc, "-sequence", "2"), nil)
						p.wait(t)
						if p.err == nil {
							t.Fatal("foreign message accepted")
						}
					}
				}
				os.WriteFile(gate, []byte("go"), 0600)
				recoveryEventually(t, func() bool { _, e := os.Stat(gate + ".confirmed"); return e == nil })
				p := recoveryStart(t, cli, recoveryArgs("disarm", config, c), nil)
				p.wait(t)
				if p.err != nil {
					b, _ := os.ReadFile(p.log)
					t.Fatalf("disarm: %s", b)
				}
			} else if scene == "normal-race" {
				os.WriteFile(gate, []byte("go"), 0600)
				owner.cmd.Process.Kill()
				owner.wait(t)
			} else if scene == "hang" {
				owner.cmd.Process.Signal(syscall.SIGSTOP)
			} else {
				if scene == "restart" {
					guard.cmd.Process.Kill()
					guard.wait(t)
					after := recoveryLoad(t, c)
					if after.HardTick != before.HardTick || after.ArmTick != before.ArmTick {
						t.Fatal("deadline reset")
					}
					guard = recoveryStart(t, cli, recoveryArgs("watch", config, c), nil)
				}
				var second *recoveryChild
				if scene == "two-guards" {
					second = recoveryStart(t, cli, recoveryArgs("watch", config, c), nil)
				}
				if e = guard.cmd.Process.Signal(syscall.Signal(0)); e != nil {
					t.Fatal("guard not independent/alive")
				}
				if scene == "executor-kill" {
					ownerKillRequested = true
				}
				owner.cmd.Process.Kill()
				owner.wait(t)
				if second != nil {
					second.wait(t)
				}
			}
			guard.wait(t)
			guardWaitObserved = true
			if normal != nil {
				normal.wait(t)
				if normal.err != nil {
					b, _ := os.ReadFile(normal.log)
					t.Fatalf("normal helper: %s", b)
				}
			}
			s := recoveryLoad(t, c)
			calls, resumes := recoveryCalls(t, dir)
			if resumes != 1 {
				t.Fatalf("resume effects requested %d calls %d", resumes, calls)
			}
			if scene == "normal" {
				if guard.err != nil || s.Phase != "DISARMED" || s.Emergency || s.ResumeIntent {
					t.Fatal("normal mislabeled emergency", s.Phase, guard.err)
				}
			} else if scene == "unconfirmed" {
				if s.Phase != "ABORTED_MANUAL" || guard.err == nil {
					t.Fatal("false restore success")
				}
			} else {
				if s.Phase != "EMERGENCY_RESTORE_CONFIRMED" || guard.err == nil {
					t.Fatal("missing emergency fact", s.Phase)
				}
			}
			if s.AutomaticSuccessEligible || s.ConsumerIntegration != "UNINTEGRATED" || s.HardTick != before.HardTick {
				t.Fatal("false receipt or deadline extension")
			}
			t.Logf("scene=%s calls=%d resumes=%d phase=%s independentGuardPID=%d executorPID=%d apiCalls=0 harnessResumes=0", scene, calls, resumes, s.Phase, guard.cmd.Process.Pid, owner.cmd.Process.Pid)
			if out := os.Getenv("P2E_EXAMPLES"); out != "" && (scene == "normal" || scene == "executor-kill" || scene == "unconfirmed") {
				os.MkdirAll(out, 0700)
				recoveryWrite(t, filepath.Join(out, scene+".json"), map[string]any{"scene": scene, "state": s, "commandCalls": calls, "resumeCalls": resumes, "apiCalls": 0, "harnessResumeCalls": 0, "guardPID": guard.cmd.Process.Pid, "executorPID": owner.cmd.Process.Pid, "synthetic": true, "liveProof": false})
			}
		})
	}
}
