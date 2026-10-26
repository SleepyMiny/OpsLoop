//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type labFinishScene struct {
	s *finishScene
	c guardConfig
	v *recoveryFake
}

func newLabFinish(t *testing.T, variant string, adjust func(*guardConfig)) *labFinishScene {
	t.Helper()
	s := newFinishScene(t, variant)
	dir := filepath.Dir(s.c.Executable)
	root := filepath.Join(dir, "guard-root")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	owner, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	s.c.Environment = "lab-live"
	s.c.Backend = "lab-libvirt"
	s.c.EvidenceKind = "synthetic"
	s.c.VMURI = "qemu:///system"
	s.c.Executable = filepath.Join(dir, "virsh")
	s.c.TimeoutMS = 1000
	s.c.DeadlineMS = 5000
	s.c.PollMS = 50
	c := guardConfig{Version: "opsloop-guard/v1", Mode: "live-libvirt", Run: s.c.RunID, Epoch: "lab-epoch", Target: recoveryTarget{s.c.VMURI, s.c.VMUUID, s.c.Executable, s.c.ExecutableSHA256}, Allowed: "query-resume-only", Root: root, Owner: owner, HardDeadline: n.Wall.Add(time.Minute), AuthorityExpires: n.Wall.Add(70 * time.Second), MaxTrialMS: 60000, HeartbeatMS: 30000, TimeoutMS: 1000}
	if adjust != nil {
		adjust(&c)
	}
	s.c.Live = &finishLiveConfig{GuardConfigFile: filepath.Join(dir, "guard-config.json"), GuardBinding: c.binding(), GuardRoot: root, Epoch: c.Epoch, GuardAuthorizationFile: filepath.Join(dir, "guard-approval.json"), AuthorizationFile: filepath.Join(dir, "finish-approval.json")}
	l := &labFinishScene{s: s, c: c, v: &recoveryFake{target: c.Target, state: "paused"}}
	l.writeApprovals(t)
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = guardArm(g, c, n); e != nil {
		g.close()
		t.Fatal(e)
	}
	g.close()
	return l
}
func (l *labFinishScene) writeApprovals(t *testing.T) {
	t.Helper()
	c := l.s.c
	recoveryWrite(t, c.Live.GuardConfigFile, l.c)
	recoveryWrite(t, c.Live.GuardAuthorizationFile, guardLiveAuthorization{Version: "opsloop-guard-authorization/v1", ConfigHash: l.c.binding(), ApprovalReference: "synthetic-guard-only", Operator: "synthetic-operator", Expires: l.c.AuthorityExpires, Scope: "dedicated-test-target-query-resume"})
	recoveryWrite(t, c.Live.AuthorizationFile, finishLiveAuthorization{Version: "opsloop-finish-authorization/v1", ConfigHash: c.binding(), ApprovalReference: "synthetic-normal-only", Operator: "synthetic-operator", Scope: "normal-finish-policy-stop-query-resume", NotBefore: time.Now().Add(-time.Minute), Expires: l.c.AuthorityExpires})
}
func (l *labFinishScene) execute(hook func(*finishJournal)) error {
	return executeFinish(context.Background(), l.s.c, io.Discard, observerWallClock{}, l.v, hook)
}
func (l *labFinishScene) state(t *testing.T) guardState { t.Helper(); return recoveryLoad(t, l.c) }
func (l *labFinishScene) mutate(t *testing.T, f func(*guardStore, *guardState)) {
	t.Helper()
	g, e := openGuard(l.c)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	s, e := g.load(l.c)
	if e != nil {
		t.Fatal(e)
	}
	f(g, s)
}
func (l *labFinishScene) export(t *testing.T, name string, err error) {
	t.Helper()
	out := os.Getenv("P3B_EXAMPLES")
	if out == "" {
		return
	}
	files, e := os.ReadDir(l.s.c.JournalDir)
	if e != nil {
		t.Fatal(e)
	}
	events := []finishEvent{}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "event-") {
			var v finishEvent
			b, _ := os.ReadFile(filepath.Join(l.s.c.JournalDir, f.Name()))
			if json.Unmarshal(b, &v) != nil {
				t.Fatal("event")
			}
			events = append(events, v)
		}
	}
	r := finishReceipt(t, l.s.c)
	data := map[string]any{"synthetic": true, "liveProof": false, "receipt": r, "events": events, "guard": l.state(t), "commands": l.v.calls, "resumeCalls": l.v.resumes, "patchCalls": l.s.patches, "errorPresent": err != nil}
	bindings, _ := filepath.Glob(l.s.c.JournalDir + ".binding-*.json")
	var companions []json.RawMessage
	for _, p := range bindings {
		b, _ := os.ReadFile(p)
		companions = append(companions, b)
	}
	data["bindings"] = companions
	receiptBytes, e := os.ReadFile(l.s.c.ReceiptFile)
	if e != nil {
		t.Fatal(e)
	}
	data["receiptBytes"] = receiptBytes
	var bindingBytes [][]byte
	for _, p := range bindings {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		bindingBytes = append(bindingBytes, b)
	}
	data["bindingBytes"] = bindingBytes
	configBytes, e := json.Marshal(l.s.c)
	if e != nil {
		t.Fatal(e)
	}
	data["configBytes"] = configBytes
	inputBytes := map[string][]byte{}
	for name, path := range map[string]string{"guard-config": l.s.c.Live.GuardConfigFile, "guard-authorization-synthetic": l.s.c.Live.GuardAuthorizationFile, "authorization-synthetic": l.s.c.Live.AuthorizationFile} {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		inputBytes[name] = b
	}
	data["inputBytes"] = inputBytes

	recoveryWrite(t, filepath.Join(out, name+".json"), data)
	b, _ := json.Marshal(data)
	if bytes.Contains(b, []byte(finishTestToken)) {
		t.Fatal("credential exported")
	}
}
func TestFinishLabNormalAndReentry(t *testing.T) {
	for _, variant := range []string{"normal", "409", "lost", "resume-loss", "running-noop", "observed-kind"} {
		t.Run(variant, func(t *testing.T) {
			apiVariant := variant
			if variant == "resume-loss" || variant == "running-noop" || variant == "observed-kind" {
				apiVariant = "normal"
			}
			l := newLabFinish(t, apiVariant, nil)
			if variant == "resume-loss" {
				l.v.loss = true
			}
			if variant == "running-noop" {
				l.v.state = "running"
			}
			if variant == "observed-kind" {
				l.s.c.EvidenceKind = "observed"
				l.writeApprovals(t)
			}
			if e := l.execute(nil); e != nil {
				labFailureEvents(t, l)
				t.Fatal(e)
			}
			r := finishReceipt(t, l.s.c)
			if r.Cleanup.Status != "succeeded" || r.StopRequested == nil || r.EvidenceKind != l.s.c.evidenceKind() {
				t.Fatalf("receipt %+v", r)
			}
			want := 1
			if variant == "running-noop" {
				want = 0
			}
			if l.v.resumes != want || len(r.Interventions) != want {
				t.Fatal("action accounting")
			}
			before, _ := os.ReadFile(l.s.c.ReceiptFile)
			if e := l.execute(nil); e != nil {
				t.Fatal("reentry", e)
			}
			after, _ := os.ReadFile(l.s.c.ReceiptFile)
			if !bytes.Equal(before, after) || l.v.resumes != want {
				t.Fatal("redelivery/replay")
			}
			state := l.state(t)
			if state.Phase != "ARMED" || state.Emergency || state.ResumeIntent || state.Config.Owner != l.c.Owner {
				t.Fatal("Guard responsibility changed")
			}
			if len(r.Coverage) != 0 || len(r.Clocks) != 0 || stampValid(r.FaultRequested) {
				t.Fatal("invented evidence")
			}
			if variant == "normal" || variant == "resume-loss" {
				l.export(t, variant, nil)
			}
		})
	}
}
func TestFinishLabPlanNoEffects(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	// Every dependent path is absent. Plan must still succeed without reading it.
	dir := filepath.Dir(l.s.c.Executable)
	l.s.c.BearerFile = filepath.Join(dir, "missing-token")
	l.s.c.APICAFile = filepath.Join(dir, "missing-ca")
	l.s.c.Live.GuardConfigFile = filepath.Join(dir, "missing-guard")
	l.s.c.Live.AuthorizationFile = filepath.Join(dir, "missing-approval")
	before := l.state(t)
	p := filepath.Join(dir, "plan.json")
	recoveryWrite(t, p, l.s.c)
	var out bytes.Buffer
	if e := runFinish([]string{"-config", p}, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "credentialsRead=0 network=0") || l.s.gets != 0 || l.s.patches != 0 || len(l.v.calls) != 0 {
		t.Fatal(out.String())
	}
	if _, e := os.Stat(l.s.c.JournalDir); !os.IsNotExist(e) {
		t.Fatal("plan wrote journal")
	}
	after := l.state(t)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("plan changed Guard")
	}
	if e := runFinish([]string{"-config", p, "-execute", "-confirm", "wrong"}, io.Discard); e == nil {
		t.Fatal("missing exact confirmation")
	}
}
func TestFinishLabRefusals(t *testing.T) {
	variants := []string{"origin-path", "origin-http", "origin-user", "service-api-authority", "tls", "untrusted-tls", "policy-uid", "deployment-uid", "root", "run", "epoch", "uuid", "config", "approval-expired", "approval-scope", "guard-approval", "heartbeat", "hard-deadline", "authority", "owner", "emergency", "restoring", "terminal", "lock", "no-ack", "fence", "active", "stale", "service-fail", "shutoff", "replace", "unknown"}
	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			apiVariant := "normal"
			for _, s := range []string{"no-ack", "fence", "active", "stale", "service-fail"} {
				if s == variant {
					apiVariant = s
				}
			}
			var owner *exec.Cmd
			if variant == "owner" {
				owner = exec.Command("/bin/sleep", "30")
				owner.Env = finishCleanEnv()
				if e := owner.Start(); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					if owner.ProcessState == nil {
						_ = owner.Process.Kill()
						_ = owner.Wait()
					}
				})
			}
			l := newLabFinish(t, apiVariant, func(c *guardConfig) {
				if owner != nil {
					var e error
					c.Owner, e = guardProcess(owner.Process.Pid, c.Owner.Boot)
					if e != nil {
						t.Fatal(e)
					}
				}
				if variant == "heartbeat" {
					c.HeartbeatMS = 50
				}
				if variant == "hard-deadline" {
					c.HardDeadline = time.Now().Add(500 * time.Millisecond)
				}
				if variant == "authority" {
					c.HardDeadline = time.Now().Add(500 * time.Millisecond)
					c.AuthorityExpires = c.HardDeadline.Add(20 * time.Millisecond)
				}
			})
			c := &l.s.c
			switch variant {
			case "origin-path":
				c.APIOrigin += "/apis"
			case "origin-http":
				c.APIOrigin = strings.Replace(c.APIOrigin, "https:", "http:", 1)
			case "origin-user":
				c.APIOrigin = strings.Replace(c.APIOrigin, "https://", "https://user@", 1)
			case "service-api-authority":
				c.ServiceURL = c.APIOrigin + "/identity"
			case "untrusted-tls":
				// The synthetic certificate pins IP SANs, not localhost DNS SAN.
				c.APIOrigin = strings.Replace(c.APIOrigin, "127.0.0.1", "localhost", 1)
			case "tls":
				os.WriteFile(c.APICAFile, []byte("invalid"), 0600)
			case "policy-uid":
				c.PolicyUID = "foreign"
			case "deployment-uid":
				c.DeploymentUID = "foreign"
			case "root":
				c.Live.GuardRoot = filepath.Join(filepath.Dir(c.Executable), "different-root")
			case "run":
				c.RunID = "foreign"
			case "epoch":
				c.Live.Epoch = "foreign"
			case "uuid":
				c.VMUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			case "config":
				c.Live.GuardBinding = strings.Repeat("1", 64)
			case "guard-approval":
				os.WriteFile(c.Live.GuardAuthorizationFile, []byte(`{}`), 0600)
			case "heartbeat":
				time.Sleep(80 * time.Millisecond)
			case "hard-deadline", "authority":
				time.Sleep(600 * time.Millisecond)
			case "owner":
				if e := owner.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				_ = owner.Wait()
			case "emergency", "restoring":
				l.mutate(t, func(g *guardStore, s *guardState) {
					n, _ := guardNow()
					if e := g.emergency(s, n, "synthetic emergency"); e != nil {
						t.Fatal(e)
					}
				})
			case "terminal":
				l.mutate(t, func(g *guardStore, s *guardState) {
					n, _ := guardNow()
					if e := (guardEngine{guardNow, l.v}).manual(g, s, "synthetic terminal"); e == nil {
						t.Fatal("manual expected")
					}
					_ = n
				})
			case "shutoff":
				l.v.state = "shut off"
			case "replace":
				l.v.wrongID = true
			case "unknown":
				l.v.state = "pmsuspended"
			}
			// Rebind normal approval for deliberate config changes, isolating each gate.
			if variant != "guard-approval" {
				l.writeApprovals(t)
			}
			if variant == "approval-expired" || variant == "approval-scope" {
				var a finishLiveAuthorization
				guardRead(c.Live.AuthorizationFile, &a)
				if variant == "approval-expired" {
					a.Expires = time.Now().Add(-time.Second)
				} else {
					a.Scope = "query-only"
				}
				recoveryWrite(t, c.Live.AuthorizationFile, a)
			}
			var held *guardStore
			if variant == "lock" {
				var e error
				held, e = openGuard(l.c)
				if e != nil {
					t.Fatal(e)
				}
				defer held.close()
			}
			err := l.execute(nil)
			if err == nil {
				t.Fatal("negative accepted")
			}
			want := 0
			if variant == "service-fail" {
				want = 1
			}
			if l.v.resumes != want {
				t.Fatalf("resumes=%d want=%d err=%v", l.v.resumes, want, err)
			}
			if variant == "emergency" {
				l.export(t, "guard-counterexample", err)
			}
		})
	}
}
func TestFinishLabFreshBoundaryAndPersistence(t *testing.T) {
	for _, variant := range []string{"stop-changes", "target-changes", "guard-changes", "approval-changes", "dispatch-interruption", "started-interruption", "receipt-failure", "binding-failure", "resume-no-effect"} {
		t.Run(variant, func(t *testing.T) {
			l := newLabFinish(t, "normal", nil)
			changed := false
			hook := func(j *finishJournal) {
				j.before = func(kind string) error {
					if variant == "target-changes" && kind == "resume-dispatch-durable" {
						l.v.state = "shut off"
					}
					if variant == "stop-changes" && kind == "resume-dispatch-durable" {
						l.s.mu.Lock()
						l.s.p.Spec.Enabled = true
						l.s.p.Generation++
						l.s.mu.Unlock()
					}
					if variant == "guard-changes" && kind == "resume-dispatch-durable" {
						// Inject a durable state change at the command boundary; fresh reload must see it.
						var s guardState
						guardRead(recoveryStatePath(l.c), &s)
						s.Emergency = true
						s.Phase = "RESTORING"
						s.Events = append(s.Events, recoveryEvent{len(s.Events) + 1, "recovery-guard", "EMERGENCY_RESTORE", "", "synthetic", time.Now().UTC()})
						recoveryWrite(t, recoveryStatePath(l.c), s)
					}
					if variant == "approval-changes" && kind == "resume-dispatch-durable" {
						os.WriteFile(l.s.c.Live.AuthorizationFile, []byte(`{}`), 0600)
					}
					if variant == "dispatch-interruption" && kind == "resume-dispatch-durable" {
						return errors.New("synthetic interruption")
					}
					if variant == "started-interruption" && kind == "vm-started" && j.last("resume-dispatch") != nil {
						return errors.New("synthetic interruption after Start")
					}
					if variant == "receipt-failure" && kind == "receipt-before-rename" {
						return errors.New("synthetic receipt failure")
					}
					if variant == "binding-failure" && kind == "binding-before-write" {
						return errors.New("synthetic binding failure")
					}
					// Verify API stop work never holds the target lock.
					if kind == "stop-response" && !changed {
						g, e := openGuard(l.c)
						if e != nil {
							t.Fatal("stop blocks Guard", e)
						}
						g.close()
						changed = true
					}
					return nil
				}
			}
			if variant == "resume-no-effect" {
				l.v.noEffect = true
				l.v.loss = true
			}
			if e := l.execute(hook); e == nil {
				t.Fatal("boundary failure accepted")
			}
			if variant == "stop-changes" || variant == "target-changes" || variant == "guard-changes" || variant == "approval-changes" || variant == "dispatch-interruption" || variant == "started-interruption" {
				if l.v.resumes != 0 {
					t.Fatal("unsafe resume")
				}
			}
			before := l.v.resumes
			// Reentry may reconcile running, but never repeat a prior dispatch.
			_ = l.execute(nil)
			if l.v.resumes != before {
				t.Fatal("resume replayed")
			}
			if variant == "dispatch-interruption" || variant == "resume-no-effect" {
				l.mutate(t, func(g *guardStore, s *guardState) {
					n, _ := guardNow()
					if e := g.emergency(s, n, "synthetic owner failure"); e != nil {
						t.Fatal(e)
					}
					if e := (guardEngine{guardNow, l.v}).step(context.Background(), g, s, false); e == nil {
						t.Fatal("unresolved normal attempt reused")
					}
					if s.Phase != "ABORTED_MANUAL" {
						t.Fatal(s.Phase)
					}
				})
				if l.v.resumes != before {
					t.Fatal("Guard repeated normal attempt")
				}
			}
		})
	}
}
func TestFinishLabGuardCompetition(t *testing.T) {
	for _, order := range []string{"guard-first", "normal-first"} {
		t.Run(order, func(t *testing.T) {
			l := newLabFinish(t, "normal", nil)
			if order == "normal-first" {
				if e := l.execute(nil); e != nil {
					t.Fatal(e)
				}
			}
			l.mutate(t, func(g *guardStore, s *guardState) {
				n, _ := guardNow()
				if e := g.emergency(s, n, "synthetic emergency"); e != nil {
					t.Fatal(e)
				}
				if e := (guardEngine{guardNow, l.v}).step(context.Background(), g, s, false); e != nil {
					t.Fatal(e)
				}
			})
			if e := l.execute(nil); e == nil {
				t.Fatal("emergency accepted as normal")
			}
			if l.v.resumes != 1 || !l.state(t).Emergency {
				t.Fatal("duplicate or hidden emergency")
			}
			if r := finishReceipt(t, l.s.c); r.Cleanup.Status == "succeeded" {
				t.Fatal("emergency masked")
			}
		})
	}
}
func TestFinishLabExecutableCLI(t *testing.T) {
	cli := os.Getenv("P3B_CLI")
	if cli == "" {
		t.Fatal("P3B_CLI must name a freshly built CLI; command test may not SKIP")
	}
	for _, variant := range []string{"normal", "lost-response"} {
		t.Run(variant, func(t *testing.T) {
			l := newLabFinish(t, "normal", nil)
			dir := filepath.Dir(l.s.c.Executable)
			target := recoveryFixture(t, dir, variant)
			code, e := os.ReadFile(target.Executable)
			if e != nil {
				t.Fatal(e)
			}
			code = []byte(strings.Replace(string(code), "import sys,os,json,time", "import sys,os,json,time\nassert set(os.environ) <= {'LANG','LC_ALL','LC_CTYPE'}", 1))
			if e = os.WriteFile(l.s.c.Executable, code, 0700); e != nil {
				t.Fatal(e)
			}
			l.s.c.ExecutableSHA256 = digestBytes(code)
			// The initially armed fake target hash is different: create a fresh independent
			// test scene by replacing only fixture state before any executor is launched.
			l.c.Target.SHA256 = l.s.c.ExecutableSHA256
			l.s.c.Live.GuardBinding = l.c.binding()
			l.mutateInitialFixture(t)
			config := filepath.Join(dir, "finish-cli.json")
			recoveryWrite(t, config, l.s.c)
			cmd := exec.Command(cli, "finish-run", "-config", config, "-execute", "-confirm", l.s.c.binding())
			cmd.Env = finishCleanEnv()
			b, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("CLI %s %v", b, e)
			}
			if bytes.Contains(b, []byte(finishTestToken)) {
				t.Fatal("credential leak")
			}
			_, resumes := recoveryCalls(t, dir)
			if resumes != 1 {
				t.Fatal("CLI resume", resumes)
			}

			rawConfig, e := os.ReadFile(config)
			if e != nil {
				t.Fatal(e)
			}
			companionPaths, e := filepath.Glob(l.s.c.JournalDir + ".binding-*.json")
			if e != nil || len(companionPaths) != 1 {
				t.Fatal("CLI binding missing")
			}
			companionBytes, e := os.ReadFile(companionPaths[0])
			if e != nil {
				t.Fatal(e)
			}
			var companion struct{ Inputs finishLiveInputs }
			if json.Unmarshal(companionBytes, &companion) != nil || companion.Inputs.InputSHA256 != digestBytes(rawConfig) {
				t.Fatal("CLI binding does not preserve exact config bytes")
			}
			before, _ := os.ReadFile(l.s.c.ReceiptFile)
			cmd = exec.Command(cli, "finish-run", "-config", config, "-execute", "-confirm", l.s.c.binding())
			cmd.Env = finishCleanEnv()
			if b, e = cmd.CombinedOutput(); e != nil {
				t.Fatalf("CLI reentry %s %v", b, e)
			}
			after, _ := os.ReadFile(l.s.c.ReceiptFile)
			_, resumes = recoveryCalls(t, dir)
			if !bytes.Equal(before, after) || resumes != 1 {
				t.Fatal("CLI replay/retimed")
			}
		})
	}
}
func (l *labFinishScene) mutateInitialFixture(t *testing.T) {
	t.Helper()
	l.writeApprovals(t)
	// Fixture construction only, under the actual target lock. No live command.
	g, e := openGuard(l.c)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	var s guardState
	if e = guardRead(g.path, &s); e != nil {
		t.Fatal(e)
	}
	s.Config = l.c
	s.Binding = l.c.binding()
	if e = g.save(&s); e != nil {
		t.Fatal(e)
	}
}

// Both actors execute their product boundaries. The channels choose overlap,
// while the existing target flock determines command ownership.
func TestFinishLabConcurrentGuard(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	held, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	go func() {
		first := true
		finished <- l.execute(func(j *finishJournal) {
			j.before = func(kind string) error {
				if first && kind == "guard-backend" {
					first = false
					close(held)
					select {
					case <-release:
					case <-time.After(2 * time.Second):
						return errors.New("test gate timeout")
					}
				}
				return nil
			}
		})
	}()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("normal did not reach target boundary")
	}
	if g, e := openGuard(l.c); e != errGuardBusy {
		if g != nil {
			g.close()
		}
		t.Fatalf("target lock not shared: %v", e)
	}
	guarded := make(chan error, 1)
	go func() {
		g, e := acquireGuard(l.c)
		if e != nil {
			guarded <- e
			return
		}
		defer g.close()
		s, e := g.load(l.c)
		if e != nil {
			guarded <- e
			return
		}
		n, e := guardNow()
		if e != nil {
			guarded <- e
			return
		}
		if e = g.emergency(s, n, "synthetic concurrent emergency"); e != nil {
			guarded <- e
			return
		}
		guarded <- (guardEngine{guardNow, l.v}).step(context.Background(), g, s, false)
	}()
	close(release)
	released = true
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("normal bounded operation hung")
	}
	select {
	case e := <-guarded:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Guard bounded operation hung")
	}
	if l.v.resumes != 1 || l.state(t).Phase != "EMERGENCY_RESTORE_CONFIRMED" {
		t.Fatal("duplicate resume or lost emergency")
	}
	if e := l.execute(nil); e == nil {
		t.Fatal("later normal reentry hid emergency")
	}
	if finishReceipt(t, l.s.c).Cleanup.Status != "pending" {
		t.Fatal("emergency promoted to normal")
	}
}
func TestFinishLabGuardDuringStop(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	fired := false
	e := l.execute(func(j *finishJournal) {
		j.before = func(kind string) error {
			if kind != "stop-response" || fired {
				return nil
			}
			fired = true
			g, e := openGuard(l.c)
			if e != nil {
				return e
			}
			defer g.close()
			s, e := g.load(l.c)
			if e != nil {
				return e
			}
			n, e := guardNow()
			if e != nil {
				return e
			}
			if e = g.emergency(s, n, "synthetic emergency while normal stops"); e != nil {
				return e
			}
			return (guardEngine{guardNow, l.v}).step(context.Background(), g, s, false)
		}
	})
	if e == nil || !fired || l.v.resumes != 1 || !l.state(t).Emergency {
		t.Fatal("Guard was blocked or emergency ignored", e)
	}
	if finishReceipt(t, l.s.c).Cleanup.Status != "pending" {
		t.Fatal("early emergency marked normal")
	}
}
func TestFinishLabCompanionCustody(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	if e := l.execute(nil); e != nil {
		t.Fatal(e)
	}
	receipt, e := os.ReadFile(l.s.c.ReceiptFile)
	if e != nil {
		t.Fatal(e)
	}
	bindings, e := filepath.Glob(l.s.c.JournalDir + ".binding-*.json")
	if e != nil || len(bindings) != 1 {
		t.Fatal("companion count")
	}
	b, e := os.ReadFile(bindings[0])
	if e != nil {
		t.Fatal(e)
	}
	var c struct {
		Version, RunID, Epoch, URI, UUID, GuardRoot, GuardBinding, ConfigBinding, ReceiptSHA256 string
		Inputs                                                                                  finishLiveInputs
		LiveProof                                                                               bool
	}
	if json.Unmarshal(b, &c) != nil || c.Version != "opsloop-finish-binding/v1" || c.RunID != l.s.c.RunID || c.Epoch != l.c.Epoch || c.URI != l.c.Target.URI || c.UUID != l.c.Target.UUID || c.GuardRoot != l.c.Root || c.GuardBinding != l.c.binding() || c.ConfigBinding != l.s.c.binding() || c.ReceiptSHA256 != digestBytes(receipt) || c.LiveProof {
		t.Fatal("binding contract")
	}
	raw, _ := json.Marshal(l.s.c)
	if c.Inputs.InputSHA256 != digestBytes(raw) {
		t.Fatal("raw input hash")
	}
	if e = os.WriteFile(bindings[0], []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = l.execute(nil); e == nil {
		t.Fatal("tampered companion accepted")
	}
	if l.v.resumes != 1 {
		t.Fatal("custody failure replayed resume")
	}
}

func labFailureEvents(t *testing.T, l *labFinishScene) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(l.s.c.JournalDir, "event-*.json"))
	for _, p := range files {
		b, _ := os.ReadFile(p)
		var e finishEvent
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		if e.Kind == "vm-result" || e.Kind == "resume-response" || e.Kind == "vm-start-attempt" || e.Kind == "vm-started" {
			elapsed := -1.0
			if e.At.ElapsedMS != nil {
				elapsed = *e.At.ElapsedMS
			}
			t.Logf("boundary seq=%d kind=%s elapsedMS=%v data=%s", e.Seq, e.Kind, elapsed, e.Data)
		}
	}
}

func TestFinishLabPermitDeadline(t *testing.T) {
	l := newLabFinish(t, "normal", nil)
	// Restore the original short command budget, while retaining Guard's outer
	// limit. Delay a durable command boundary to make expiry deterministic.
	l.s.c.TimeoutMS = 300
	l.writeApprovals(t)
	delayed := false
	e := l.execute(func(j *finishJournal) {
		j.before = func(kind string) error {
			if !delayed && kind == "guard-backend" && j.last("resume-dispatch") != nil {
				delayed = true
				time.Sleep(350 * time.Millisecond)
			}
			return nil
		}
	})
	if e == nil || !delayed || l.v.resumes != 0 {
		t.Fatal("expired pre-Start permit executed", e)
	}
	r := finishReceipt(t, l.s.c)
	if len(r.Interventions) != 0 || r.Cleanup.Status != "pending" {
		t.Fatal("expiry invented Start/success")
	}
	if e = l.execute(nil); e == nil || l.v.resumes != 0 {
		t.Fatal("expired dispatch replayed")
	}
}
