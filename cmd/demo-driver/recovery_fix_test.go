//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryRearm(t *testing.T, c guardConfig, n guardMoment) (*guardStore, *guardState) {
	t.Helper()
	c.Root = t.TempDir()
	if e := os.Chmod(c.Root, 0700); e != nil {
		t.Fatal(e)
	}
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(g.close)
	s, e := guardArm(g, c, n)
	if e != nil {
		t.Fatal(e)
	}
	return g, s
}
func recoveryHas(s *guardState, kind string) bool {
	for _, e := range s.Events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestRecoveryDisarmFreshBoundary(t *testing.T) {
	for _, scene := range []string{"normal", "heartbeat-before", "heartbeat-during", "hard-before", "hard-during", "owner-before", "owner-during", "authority-before", "authority-during", "clock-skew-before", "clock-skew-during", "boot-during", "tick-back-during", "clock-read-error-during", "final-clock-read-error", "final-clock-invalid", "restoring", "emergency", "disarmed-save-error"} {
		// Keep command timers in the same deterministic clock domain as the
		// injected state transitions. Real journal fsync latency must not select
		// an unrelated command timeout before the intended heartbeat boundary.
		// The 50ms command timeout and all durable Guard operations are retained.
		t.Run(scene, recoveryClockTest(func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			v.state = "running"
			var owner *recoveryChild
			if strings.HasPrefix(scene, "owner-") {
				owner = recoveryStart(t, "/bin/sleep", []string{"30"}, nil)
				var e error
				c.Owner, e = guardProcess(owner.cmd.Process.Pid, n.Boot)
				if e != nil {
					t.Fatal(e)
				}
				g, s = recoveryRearm(t, c, *n)
			}
			if strings.HasPrefix(scene, "authority-") {
				c.AuthorityExpires = n.Wall.Add(2005 * time.Millisecond)
				g, s = recoveryRearm(t, c, *n)
			}
			switch scene {
			case "heartbeat-before":
				recoveryAdvance(n, 510*time.Millisecond)
			case "heartbeat-during":
				recoveryAdvance(n, 490*time.Millisecond)
			case "hard-before", "authority-before":
				recoveryAdvance(n, 2010*time.Millisecond)
				s.HeartbeatTick = n.Tick
				s.LastTick = n.Tick
			case "hard-during":
				recoveryAdvance(n, 1990*time.Millisecond)
				s.HeartbeatTick = n.Tick
				s.LastTick = n.Tick
			case "authority-during":
				// Keep the final fake time at 2010ms (>2005ms expiry), while
				// leaving 505ms real budget for the two durable observations.
				// The former 15ms real budget depended on tmpfs fsync speed.
				recoveryAdvance(n, 1500*time.Millisecond)
				s.HeartbeatTick = n.Tick
				s.LastTick = n.Tick
			case "owner-before":
				if e := owner.cmd.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				owner.wait(t)
			case "clock-skew-before":
				n.Wall = n.Wall.Add(3 * time.Second)
			case "restoring", "emergency":
				if e := g.emergency(s, *n, "fixture interruption"); e != nil {
					t.Fatal(e)
				}
				if scene == "emergency" {
					s.Phase = "ARMED"
				}
			}
			if e := g.save(s); e != nil {
				t.Fatal(e)
			}
			clockErr, finalReady := false, false
			v.onCall = func(op string) {
				if op != "domstate" {
					return
				}
				switch scene {
				case "heartbeat-during", "hard-during":
					recoveryAdvance(n, 20*time.Millisecond)
				case "authority-during":
					recoveryAdvance(n, 510*time.Millisecond)
				case "owner-during":
					if e := owner.cmd.Process.Kill(); e != nil {
						t.Fatal(e)
					}
					owner.wait(t)
				case "clock-skew-during":
					n.Wall = n.Wall.Add(3 * time.Second)
				case "boot-during":
					n.Boot = "different-test-boot"
				case "tick-back-during":
					n.Tick--
				case "clock-read-error-during":
					clockErr = true
				}
			}
			g.beforeSave = func(next *guardState) error {
				last := next.Events[len(next.Events)-1]
				if last.Kind == "command-result" && last.Operation == "domstate" {
					finalReady = true
				}
				if scene == "disarmed-save-error" && last.Kind == "disarmed" {
					return errors.New("injected disk failure")
				}
				return nil
			}
			x := guardEngine{func() (guardMoment, error) {
				if clockErr || (scene == "final-clock-read-error" && finalReady) {
					return guardMoment{}, errors.New("injected clock read failure")
				}
				got := *n
				if scene == "final-clock-invalid" && finalReady {
					got.Wall = got.Wall.Add(3 * time.Second)
				}
				return got, nil
			}, v}
			err := x.step(context.Background(), g, s, true)
			loaded, e := g.load(s.Config)
			if e != nil {
				t.Fatal(e)
			}
			if scene == "normal" {
				if err != nil || loaded.Phase != "DISARMED" || loaded.Emergency {
					t.Fatalf("normal phase=%s err=%v", loaded.Phase, err)
				}
			} else {
				if err == nil || loaded.Phase == "DISARMED" || recoveryHas(loaded, "disarmed") || s.Phase == "DISARMED" {
					t.Fatalf("unsafe normal release: phase=%s err=%v", loaded.Phase, err)
				}
			}
			wantCalls := 2
			if strings.HasSuffix(scene, "-before") || scene == "restoring" || scene == "emergency" {
				wantCalls = 0
			}
			if len(v.calls) != wantCalls || v.resumes != 0 {
				t.Fatalf("calls=%d want=%d resumes=%d err=%v phase=%s events=%+v", len(v.calls), wantCalls, v.resumes, err, loaded.Phase, loaded.Events)
			}
			if scene == "hard-during" && n.Tick-s.HeartbeatTick != int64(20*time.Millisecond) {
				t.Fatal("hard control lost valid heartbeat")
			}
			uncertain := strings.HasPrefix(scene, "authority-") || strings.HasPrefix(scene, "clock-skew-") || scene == "boot-during" || scene == "tick-back-during" || scene == "final-clock-invalid"
			unrecordable := scene == "clock-read-error-during" || scene == "final-clock-read-error" || scene == "disarmed-save-error"
			if uncertain && (loaded.Phase != "ABORTED_MANUAL" || !recoveryHas(loaded, "manual-review")) {
				t.Fatal("missing manual evidence")
			}
			if unrecordable && (loaded.Phase != "ARMED" || recoveryHas(loaded, "manual-review")) {
				t.Fatal("fabricated terminal/manual event")
			}
			if scene != "normal" && !uncertain && !unrecordable && (!loaded.Emergency || loaded.Phase != "RESTORING") {
				t.Fatal("emergency responsibility lost")
			}
			// Successful backend observations remain facts even if disarm is refused.
			if wantCalls == 2 && scene != "clock-read-error-during" {
				found := false
				for _, ev := range loaded.Events {
					if ev.Kind == "command-result" && ev.Operation == "domstate" && ev.Detail == "running" {
						found = true
					}
				}
				if !found {
					t.Fatal("lost running observation")
				}
			}
			before := len(v.calls)
			v.onCall = nil
			// Reload through the actual store and drive the same watch step, without
			// changing evidence or terminal state after the operation under test.
			restartErr := x.step(context.Background(), g, loaded, false)
			if scene != "normal" && (loaded.Phase == "DISARMED" || recoveryHas(loaded, "disarmed")) {
				t.Fatal("restart hid rejection")
			}
			if uncertain || unrecordable || scene == "normal" {
				if len(v.calls) != before {
					t.Fatal("new command after uncertainty/record failure")
				}
			} else if restartErr != nil || loaded.Phase != "EMERGENCY_RESTORE_CONFIRMED" || !loaded.Emergency {
				t.Fatalf("watch failed: %s %v", loaded.Phase, restartErr)
			}
			if v.resumes != 0 || loaded.AutomaticSuccessEligible || loaded.ConsumerIntegration != "UNINTEGRATED" {
				t.Fatal("false resume/receipt")
			}
			t.Logf("scene=%s disarmError=%v durablePhase=%s emergency=%t disarmCalls=%d watchCalls=%d resumes=%d restartError=%v", scene, err, s.Phase, s.Emergency, before, len(v.calls)-before, v.resumes, restartErr)
		}))
	}
}

// The same initial heartbeat state has a different safe result when the command
// budget, rather than the heartbeat, expires first. Advance synctest time at the
// durable Start boundary; no real sleep or host scheduling selects this branch.
func TestRecoveryDisarmHeartbeatCommandDeadline(t *testing.T) {
	recoveryClockTest(func(t *testing.T) {
		c, g, s, v, n := recoverySetup(t)
		v.state = "running"
		recoveryAdvance(n, 490*time.Millisecond)
		if err := g.save(s); err != nil {
			t.Fatal(err)
		}
		injected := false
		g.beforeSave = func(next *guardState) error {
			event := next.Events[len(next.Events)-1]
			if !injected && event.Kind == "vm-start-attempt" && event.Operation == "domuuid" {
				injected = true
				time.Sleep(time.Duration(c.TimeoutMS)*time.Millisecond + time.Nanosecond)
			}
			return nil
		}
		x := recoveryEngine(n, v)
		err := x.step(context.Background(), g, s, true)
		loaded, loadErr := g.load(c)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if !injected || err == nil || len(v.calls) != 0 || v.resumes != 0 || loaded.Phase != "ABORTED_MANUAL" || recoveryHas(loaded, "disarmed") || loaded.AutomaticSuccessEligible {
			t.Fatalf("command deadline did not fail closed: injected=%v err=%v calls=%d resumes=%d phase=%s events=%+v", injected, err, len(v.calls), v.resumes, loaded.Phase, loaded.Events)
		}
		if n.Tick-loaded.HeartbeatTick != int64(490*time.Millisecond) {
			t.Fatal("command deadline control changed the injected heartbeat clock")
		}
		if !recoveryHas(loaded, "vm-start-attempt") || !recoveryHas(loaded, "command-result") || !recoveryHas(loaded, "manual-review") {
			t.Fatal("command timeout lost its durable attempt/result/manual-review evidence")
		}
		g.beforeSave = nil
		if err := x.step(context.Background(), g, loaded, false); err == nil || len(v.calls) != 0 || v.resumes != 0 || loaded.Phase != "ABORTED_MANUAL" {
			t.Fatal("reload retried a command or converted uncertain termination to success")
		}
		t.Logf("timeoutMS=%d heartbeatAgeMS=490 calls=0 resumes=0 phase=%s; original heartbeat-during still requires two observations", c.TimeoutMS, loaded.Phase)
	})(t)
}

// Establish unresolved intent through the product path, with a pre-Start disk
// failure. The test never inserts a resume intent or terminal evidence by hand.
func recoveryPriorIntent(t *testing.T, g *guardStore, s *guardState, v *recoveryFake, n *guardMoment) {
	t.Helper()
	g.beforeSave = func(next *guardState) error {
		e := next.Events[len(next.Events)-1]
		if e.Kind == "command-intent" && e.Operation == "resume" {
			return errors.New("fixture interruption before Start")
		}
		return nil
	}
	if e := recoveryEngine(n, v).step(context.Background(), g, s, false); e == nil {
		t.Fatal("fixture unexpectedly succeeded")
	}
	g.beforeSave = nil
	loaded, e := g.load(s.Config)
	if e != nil {
		t.Fatal(e)
	}
	*s = *loaded
	if !s.ResumeIntent || !s.Emergency || v.resumes != 0 {
		t.Fatal("fixture lacks durable intent")
	}
}

func TestRecoveryEventCreationTimes(t *testing.T) {
	for _, scene := range []string{"fixed-clock-control", "wall-back-control", "resume-intent", "manual-query", "manual-prior-intent", "manual-effect"} {
		t.Run(scene, func(t *testing.T) {
			_, g, s, v, n := recoverySetup(t)
			recoveryAdvance(n, 2*time.Second)
			switch scene {
			case "manual-query":
				v.wrongID = true
			case "manual-effect":
				v.noEffect = true
			case "manual-prior-intent":
				recoveryPriorIntent(t, g, s, v, n)
			}
			start := len(s.Events)
			reads := 0
			lastRead := guardMoment{}
			previousEventRead := 0
			checked := 0
			x := guardEngine{func() (guardMoment, error) { reads++; lastRead = *n; return *n, nil }, v}
			v.onCall = func(string) {
				if scene != "fixed-clock-control" {
					recoveryAdvance(n, 10*time.Millisecond)
					if scene == "wall-back-control" {
						n.Wall = n.Wall.Add(-20 * time.Millisecond)
					}
				}
			}
			g.beforeSave = func(next *guardState) error {
				ev := next.Events[len(next.Events)-1]
				if ev.Kind == "resume-intent" || ev.Kind == "manual-review" {
					if !ev.At.Equal(n.Wall) || !ev.At.Equal(lastRead.Wall) || reads <= previousEventRead {
						t.Fatalf("stale creation time: %s at=%s creation=%s reads=%d previous=%d", ev.Kind, ev.At, n.Wall, reads, previousEventRead)
					}
					checked++
					t.Logf("scene=%s kind=%s sequence=%d At=%s creationClock=%s clockRead=%d", scene, ev.Kind, ev.Sequence, ev.At.Format(time.RFC3339Nano), lastRead.Wall.Format(time.RFC3339Nano), reads)
				}
				previousEventRead = reads
				return nil
			}
			err := x.step(context.Background(), g, s, false)
			want := 1
			if scene == "manual-effect" {
				want = 2
			}
			if checked != want {
				t.Fatalf("checked events=%d want=%d", checked, want)
			}
			manual := strings.HasPrefix(scene, "manual-")
			if (err != nil) != manual {
				t.Fatalf("err=%v", err)
			}
			loaded, e := g.load(s.Config)
			if e != nil {
				t.Fatal(e)
			}
			for i := start; i < len(loaded.Events); i++ {
				if loaded.Events[i].Sequence != i+1 {
					t.Fatal("sequence")
				}
			}
			if manual && loaded.Phase != "ABORTED_MANUAL" {
				t.Fatal("manual not durable")
			}
		})
	}
}

func TestRecoveryEventCreationFailures(t *testing.T) {
	for _, scene := range []string{"resume-intent", "manual-query", "manual-prior-intent", "manual-effect", "query-result", "resume-started", "effect-confirmed"} {
		for _, failure := range []string{"clock", "save"} {
			t.Run(scene+"/"+failure, func(t *testing.T) {
				_, g, s, v, n := recoverySetup(t)
				recoveryAdvance(n, 2*time.Second)
				switch scene {
				case "manual-query":
					v.wrongID = true
				case "manual-effect":
					v.noEffect = true
				case "manual-prior-intent":
					recoveryPriorIntent(t, g, s, v, n)
				}
				armed, failed := false, false
				failedReads := 0
				callsAtFailure := 0
				eventsAtFailure := 0
				target := func(ev recoveryEvent) bool {
					switch scene {
					case "resume-intent":
						return ev.Kind == "resume-intent"
					case "query-result":
						return ev.Kind == "command-result" && ev.Operation == "domstate"
					case "resume-started":
						return ev.Kind == "vm-started" && ev.Operation == "resume"
					case "effect-confirmed":
						return ev.Kind == "effect-confirmed"
					default:
						return ev.Kind == "manual-review"
					}
				}
				x := guardEngine{func() (guardMoment, error) {
					if failure == "clock" && armed {
						failed = true
						armed = false
						failedReads++
						callsAtFailure = len(v.calls)
						eventsAtFailure = len(s.Events)
						return guardMoment{}, errors.New("injected event clock failure")
					}
					return *n, nil
				}, v}
				v.onCall = func(op string) {
					recoveryAdvance(n, 10*time.Millisecond)
					if failure == "clock" && (scene == "resume-started" && op == "resume") {
						armed = true
					}
				}
				g.beforeSave = func(next *guardState) error {
					ev := next.Events[len(next.Events)-1]
					if failure == "save" && target(ev) {
						failed = true
						callsAtFailure = len(v.calls)
						eventsAtFailure = len(s.Events)
						return errors.New("injected event write failure")
					}
					if failure == "clock" && scene == "query-result" && ev.Kind == "vm-started" && ev.Operation == "domstate" {
						armed = true
					}
					if failure == "clock" && ev.Kind == "command-result" {
						switch scene {
						case "resume-intent", "manual-prior-intent":
							armed = ev.Operation == "domstate"
						case "manual-query":
							armed = ev.Operation == "domuuid"
						case "manual-effect", "effect-confirmed":
							armed = ev.Operation == "domstate" && v.resumes == 1
						}
					}
					return nil
				}
				err := x.step(context.Background(), g, s, false)
				if err == nil || !failed || (failure == "clock" && failedReads != 1) {
					t.Fatalf("failure ignored: err=%v injected=%t reads=%d", err, failed, failedReads)
				}
				if len(v.calls) != callsAtFailure {
					t.Fatal("follow-up command after event failure")
				}
				loaded, e := g.load(s.Config)
				if e != nil {
					t.Fatal(e)
				}
				if loaded.Phase != "RESTORING" || !loaded.Emergency || recoveryHas(loaded, "manual-review") || recoveryHas(loaded, "effect-confirmed") {
					t.Fatalf("fabricated completion: %s", loaded.Phase)
				}
				for _, ev := range loaded.Events {
					if target(ev) {
						t.Fatal("failed event became durable")
					}
				}
				if scene == "resume-intent" && (loaded.ResumeIntent || v.resumes != 0) {
					t.Fatal("effect before intent")
				}
				if (scene == "manual-prior-intent" || scene == "manual-effect" || scene == "resume-started") && !loaded.ResumeIntent {
					t.Fatal("lost durable intent")
				}
				if scene == "manual-prior-intent" && v.resumes != 0 {
					t.Fatal("replayed intent")
				}
				t.Logf("scene=%s failure=%s error=%v phase=%s calls=%d resumes=%d intent=%t durableEvents=%d eventsAtFailure=%d", scene, failure, err, loaded.Phase, len(v.calls), v.resumes, loaded.ResumeIntent, len(loaded.Events), eventsAtFailure)
			})
		}
	}
}

func TestRecoveryCLIOwnerDuringQuery(t *testing.T) {
	cli := recoveryCLI(t)
	for _, scene := range []string{"normal-control", "owner-exits-during-domstate"} {
		t.Run(scene, func(t *testing.T) {
			c, old, _, _, _ := recoverySetup(t)
			old.close()
			dir := t.TempDir()
			owner := recoveryStart(t, "/bin/sleep", []string{"30"}, nil)
			n, e := guardNow()
			if e != nil {
				t.Fatal(e)
			}
			c.Owner, e = guardProcess(owner.cmd.Process.Pid, n.Boot)
			if e != nil {
				t.Fatal(e)
			}
			c.HardDeadline = n.Wall.Add(5 * time.Second)
			c.AuthorityExpires = n.Wall.Add(10 * time.Second)
			c.MaxTrialMS = 5000
			c.HeartbeatMS = 5000
			c.TimeoutMS = 2000
			code := fmt.Sprintf(`#!/usr/bin/python3
import sys,os,time,json
root=%q
uuid=%q
assert sys.argv[1:3]==['--connect','qemu:///system'] and sys.argv[4:]==[uuid]
assert not any(k in os.environ for k in ['HOME','KUBECONFIG','LIBVIRT_DEFAULT_URI','HTTP_PROXY','HTTPS_PROXY','AWS_ACCESS_KEY_ID'])
op=sys.argv[3]
assert op in ['domuuid','domstate']
with open(root+'/calls','a') as f:
 f.write(json.dumps({'op':op,'uuid':uuid,'uri':sys.argv[2]})+'\n');f.flush();os.fsync(f.fileno())
if op=='domuuid': print(uuid)
elif op=='domstate':
 open(root+'/entered','w').close()
 until=time.monotonic()+1.5
 while not os.path.exists(root+'/release'):
  if time.monotonic()>until: sys.exit(21)
  time.sleep(0.005)
 print('running')
`, dir, finishTestUUID)
			c.Target.Executable = filepath.Join(dir, "trusted-synthetic-command")
			c.Target.SHA256 = digestBytes([]byte(code))
			if e = os.WriteFile(c.Target.Executable, []byte(code), 0700); e != nil {
				t.Fatal(e)
			}
			g, s := recoveryRearm(t, c, n)
			c = s.Config
			g.close()
			config := filepath.Join(dir, "config.json")
			recoveryWrite(t, config, c)
			proc := recoveryStart(t, cli, recoveryArgs("disarm", config, c), nil)
			recoveryEventually(t, func() bool { _, e := os.Stat(filepath.Join(dir, "entered")); return e == nil })
			normal := scene == "normal-control"
			if !normal {
				if e = owner.cmd.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				owner.wait(t)
			}
			if e = os.WriteFile(filepath.Join(dir, "release"), []byte("go"), 0600); e != nil {
				t.Fatal(e)
			}
			proc.wait(t)
			got := recoveryLoad(t, c)
			calls, resumes := recoveryCalls(t, dir)
			wantPhase := "RESTORING"
			if normal {
				wantPhase = "DISARMED"
			}
			if (proc.err == nil) != normal || got.Phase != wantPhase || got.Emergency == normal || calls != 2 || resumes != 0 {
				t.Fatalf("CLI result: phase=%s emergency=%t err=%v calls=%d resumes=%d", got.Phase, got.Emergency, proc.err, calls, resumes)
			}
			preWatch := got
			watch := recoveryStart(t, cli, recoveryArgs("watch", config, c), nil)
			watch.wait(t)
			got = recoveryLoad(t, c)
			calls, resumes = recoveryCalls(t, dir)
			if normal {
				if watch.err != nil || got.Phase != "DISARMED" || calls != 2 {
					t.Fatal("normal watch")
				}
			} else if watch.err == nil || got.Phase != "EMERGENCY_RESTORE_CONFIRMED" || !got.Emergency || calls != 4 {
				t.Fatal("emergency hidden on watch", got.Phase, watch.err, calls)
			}
			if resumes != 0 || got.ResumeIntent || got.AutomaticSuccessEligible || got.ConsumerIntegration != "UNINTEGRATED" {
				t.Fatal("false resume/receipt")
			}
			t.Logf("scene=%s disarmExit=%v disarmPhase=%s disarmEmergency=%t disarmCalls=2 watchExit=%v watchPhase=%s totalCalls=%d resumes=%d", scene, proc.err, preWatch.Phase, preWatch.Emergency, watch.err, got.Phase, calls, resumes)
			if out := os.Getenv("P2E_FIX_EVIDENCE"); out != "" {
				recoveryWrite(t, filepath.Join(out, scene+".json"), map[string]any{"scene": scene, "disarmState": preWatch, "watchState": got, "disarmExitSuccess": proc.err == nil, "watchExitSuccess": watch.err == nil, "disarmCalls": 2, "totalCalls": calls, "resumes": resumes, "synthetic": true, "liveProof": false})
			}
		})
	}
}

func TestRecoveryResumePermitClockFailureStopsOperation(t *testing.T) {
	_, g, s, v, n := recoverySetup(t)
	recoveryAdvance(n, 2*time.Second)
	failNext, failed := false, false
	g.beforeSave = func(next *guardState) error {
		if next.Events[len(next.Events)-1].Kind == "resume-intent" {
			failNext = true
		}
		return nil
	}
	x := guardEngine{func() (guardMoment, error) {
		if failNext {
			failNext = false
			failed = true
			return guardMoment{}, errors.New("injected resume permit clock failure")
		}
		return *n, nil
	}, v}
	err := x.step(context.Background(), g, s, false)
	loaded, e := g.load(s.Config)
	if e != nil {
		t.Fatal(e)
	}
	if err == nil || !failed || len(v.calls) != 2 || v.resumes != 0 || !loaded.ResumeIntent || !loaded.Emergency || loaded.Phase != "RESTORING" || recoveryHas(loaded, "manual-review") {
		t.Fatalf("failed clock allowed follow-up: err=%v phase=%s calls=%d resumes=%d", err, loaded.Phase, len(v.calls), v.resumes)
	}
}
