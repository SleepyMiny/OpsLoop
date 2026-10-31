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
	"syscall"
	"testing"
	"time"
)

// A test-owned deadline fires only after the real command runner has successfully
// called Start. The command-phase timer starts at the real Start observation,
// before its journal I/O; zero requests a synthetic deadline-cause cancellation.
// Neither mode fabricates Start evidence or changes production deadlines.
type finishStartedTimeout struct {
	base       finishVMRunner
	operation  string
	afterStart time.Duration // test-only command-phase timer; zero is immediate cause injection
}

func (r finishStartedTimeout) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
	if args[2] != r.operation {
		return r.base.RunStarted(ctx, args, boundary)
	}
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	out, err := r.base.RunStarted(call, args, func(kind string) error {
		if kind == "vm-started" && r.afterStart > 0 {
			timer = time.AfterFunc(r.afterStart, func() { cancel(context.DeadlineExceeded) })
		}
		e := boundary(kind)
		if e == nil && kind == "vm-started" && r.afterStart == 0 {
			cancel(context.DeadlineExceeded)
		}
		return e
	})
	if err != nil && context.Cause(call) == context.DeadlineExceeded {
		return "", context.DeadlineExceeded
	}
	return out, err
}

// The old owner-only 1500ms setup budget can expire before the ownership change
// being tested. An explicit preparation barrier crosses that old deadline; it
// is not a hypothesis about the unavailable original CI journal.
func TestFinishOwnerPreparationBoundary(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "old-budget", false: "shared-budget"}[old], func(t *testing.T) {
			s := newFinishScene(t, "normal")
			state := installFinishFixture(t, s)
			if old {
				s.c.DeadlineMS = 1500
				s.c.PollMS = 10
			}
			var output bytes.Buffer
			err := executeFinish(context.Background(), s.c, &output, observerWallClock{}, fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}, func(*finishJournal) {
				barrier, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
				defer cancel()
				<-barrier.Done()
			})
			if old {
				if err == nil || s.patches != 0 {
					t.Fatalf("old preparation crossed expiry: err=%v patches=%d", err, s.patches)
				}
				t.Logf("old setup rejected before ownership mutation: %s", output.String())
				return
			}
			if err != nil {
				t.Fatalf("shared setup: %s: %v", output.String(), err)
			}
			before, e := os.ReadFile(s.c.ReceiptFile)
			if e != nil {
				t.Fatal(e)
			}
			fixContradict(s, true)
			now := time.Now()
			clock := &observerTestClock{t: now, start: now}
			if err = executeFinish(context.Background(), s.c, io.Discard, clock, fixtureVMRunner{s.c.Executable, s.c.ExecutableSHA256}, nil); err == nil {
				t.Fatal("owner contradiction accepted")
			}
			archived, e := os.ReadFile(filepath.Join(s.c.JournalDir, "archive-"+digestBytes(before)+".json"))
			if e != nil || !bytes.Equal(archived, before) {
				t.Fatal("original completion not preserved", e)
			}
			patches, resumes, _, _ := fixCountsAndPublications(t, s, state)
			if patches != 1 || resumes != 1 {
				t.Fatal("contradiction repeated effects", patches, resumes)
			}
			t.Logf("shared preparation -> owner rejection: patches=%d resumes=%d executableSHA256=%s originalReceiptSHA256=%s", patches, resumes, s.c.ExecutableSHA256, digestBytes(before))
		})
	}
}

// A blocked initial identity command is a distinct safe failure path. The OS
// command really starts, then its result is unconfirmed; Guard must not resume.
func TestRecoveryInitialQueryDeadline(t *testing.T) {
	dir := t.TempDir()
	target := recoveryFixture(t, dir, "executor-kill")
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	owner, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	c := guardConfig{Version: "opsloop-guard/v1", Mode: "synthetic-local", Run: "query-deadline", Epoch: "epoch-1", Target: target, Allowed: "query-resume-only", Root: filepath.Join(dir, "guard"), Owner: owner, HardDeadline: n.Wall.Add(2 * time.Second), AuthorityExpires: n.Wall.Add(10 * time.Second), MaxTrialMS: 2000, HeartbeatMS: 500, TimeoutMS: 300}
	if e = os.Mkdir(c.Root, 0700); e != nil {
		t.Fatal(e)
	}
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	s, e := guardArm(g, c, n)
	if e != nil {
		t.Fatal(e)
	}
	recoveryAdvance(&n, 3*time.Second)
	engine := guardEngine{now: func() (guardMoment, error) { return n, nil }, runner: finishStartedTimeout{base: boundedVMRunner{target.Executable, target.SHA256}, operation: "domuuid"}}
	if e = engine.step(context.Background(), g, s, false); e == nil || s.Phase != "ABORTED_MANUAL" {
		t.Fatal("unconfirmed identity accepted", e, s.Phase)
	}
	starts, resume := 0, 0
	for _, v := range s.Events {
		if v.Kind == "vm-started" {
			starts++
		}
		if v.Operation == "resume" {
			resume++
		}
	}
	if starts != 1 || resume != 0 {
		t.Fatal("initial query/resume ledger mismatch", starts, resume)
	}
	data, _ := json.Marshal(s.Events)
	t.Logf("ownerPID=%d ownerStart=%s sourceSHA256=%s phase=%s events=%s", owner.PID, owner.Start, target.SHA256, s.Phase, data)
}

// Only this test binary implements the staged helper. It reuses the production
// arm/step/store/command core, not the CLI watch loop. Other separate-process
// scenes still exercise that loop. A uses the real clock; only B injects time.
type recoveryBoundaryAck struct {
	Stage    string      `json:"stage"`
	Binding  string      `json:"binding"`
	Mode     string      `json:"mode"`
	Helper   guardOwner  `json:"helper"`
	Real     guardMoment `json:"real"`
	Observed guardMoment `json:"observed"`
	State    guardState  `json:"state"`
	Error    string      `json:"error,omitempty"`
	Exit     int         `json:"expectedHelperExit"`
}

func recoveryBoundaryWrite(t *testing.T, path string, value any) {
	t.Helper()
	data, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	if e = finishExclusive(path, data); e != nil {
		t.Fatal(e)
	}
}
func recoveryBoundaryWait(t *testing.T, path string) {
	t.Helper()
	recoveryEventually(t, func() bool { _, e := os.Stat(path); return e == nil })
}
func recoveryWatchBoundaryHelper(t *testing.T, c guardConfig, gate, mode string) {
	if mode != "owner-exit" && mode != "heartbeat-late" && mode != "hard-deadline" && mode != "authority-expired" {
		t.Fatal("unknown synthetic boundary")
	}
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	self, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	if e = (boundedVMRunner{c.Target.Executable, c.Target.SHA256}).preflight(); e != nil {
		t.Fatal(e)
	}
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	s, e := guardArm(g, c, n)
	g.close()
	if e != nil {
		t.Fatal(e)
	}
	recoveryBoundaryWrite(t, gate+".armed", recoveryBoundaryAck{Stage: "armed", Binding: s.Binding, Mode: mode, Helper: self, Real: n, Observed: n, State: *s})
	recoveryBoundaryWait(t, gate+".observe")
	var released map[string]string
	if e := guardRead(gate+".observe", &released); e != nil || released["binding"] != c.binding() {
		t.Fatal("observe gate binding changed", e)
	}
	g, e = openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	s, e = g.load(c)
	if e != nil {
		g.close()
		t.Fatal(e)
	}
	real, e := guardNow()
	if e != nil {
		g.close()
		t.Fatal(e)
	}
	observed := real
	if mode != "owner-exit" {
		tick := s.HeartbeatTick + c.HeartbeatMS*int64(time.Millisecond)
		if mode == "hard-deadline" {
			tick = s.HardTick
		}
		if mode == "authority-expired" {
			tick = s.ExpiryTick
		}
		observed = guardMoment{Boot: s.Boot, Tick: tick, Wall: s.ArmWall.Add(time.Duration(tick - s.ArmTick))}
	}
	clock := guardNow
	if mode != "owner-exit" {
		clock = func() (guardMoment, error) { return observed, nil }
	}
	engine := guardEngine{now: clock, runner: boundedVMRunner{c.Target.Executable, c.Target.SHA256}}
	stepErr := engine.step(context.Background(), g, s, false)
	if mode == "owner-exit" {
		after, e := guardNow()
		if e != nil {
			g.close()
			t.Fatal(e)
		}
		reason, checkErr := s.check(after)
		if stepErr != nil || s.Phase != "ARMED" || reason != "" || checkErr != nil {
			g.close()
			t.Fatalf("first observation not valid; no owner kill permitted: step=%v reason=%s check=%v state=%+v", stepErr, reason, checkErr, s)
		}
		g.close()
		recoveryBoundaryWrite(t, gate+".observed", recoveryBoundaryAck{Stage: "first-valid-observation", Binding: s.Binding, Mode: mode, Helper: self, Real: after, Observed: after, State: *s})
		recoveryBoundaryWait(t, gate+".owner-killed")
		var killed map[string]string
		if e := guardRead(gate+".owner-killed", &killed); e != nil || killed["binding"] != c.binding() || killed["ownerStart"] != c.Owner.Start {
			t.Fatal("owner gate binding changed", e)
		}
		g, e = openGuard(c)
		if e != nil {
			t.Fatal(e)
		}
		s, e = g.load(c)
		if e != nil {
			g.close()
			t.Fatal(e)
		}
		observed, e = guardNow()
		if e != nil {
			g.close()
			t.Fatal(e)
		}
		if e = s.permit(observed); e != nil {
			g.close()
			t.Fatalf("owner exit outside restoration authority: %v", e)
		}
		// Apply A's freshness preconditions to the exact first clock sample
		// consumed by step. Later command/record samples remain real time.
		firstDecision := true
		engine.now = func() (guardMoment, error) {
			moment, err := guardNow()
			if err != nil {
				return moment, err
			}
			if firstDecision {
				firstDecision = false
				observed = moment
				if err = s.permit(moment); err != nil {
					return moment, err
				}
				if moment.Tick >= s.HardTick || moment.Tick-s.HeartbeatTick >= c.HeartbeatMS*int64(time.Millisecond) {
					return moment, errors.New("A owner-exit first decision exceeded heartbeat/hard boundary; no reclassification")
				}
				reason, err := s.check(moment)
				if err != nil || reason != "owner-exited" {
					return moment, errors.New("A owner-exit identity not confirmed")
				}
			}
			return moment, nil
		}
		stepErr = engine.step(context.Background(), g, s, false)
	}
	if !s.closed() {
		g.close()
		t.Fatalf("boundary step did not close; no second step/reclassification permitted: mode=%s err=%v state=%+v", mode, stepErr, s)
	}
	before, e := os.ReadFile(g.path)
	if e != nil {
		g.close()
		t.Fatal(e)
	}
	beforeEvents := len(s.Events)
	replayErr := engine.step(context.Background(), g, s, false)
	after, e := os.ReadFile(g.path)
	g.close()
	if e != nil {
		t.Fatal(e)
	}
	if replayErr == nil || len(s.Events) != beforeEvents || !bytes.Equal(before, after) {
		t.Fatal("closed epoch reentry changed journal or issued work", replayErr)
	}
	finalReal, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	message := ""
	if stepErr != nil {
		message = stepErr.Error()
	}
	recoveryBoundaryWrite(t, gate+".result", recoveryBoundaryAck{Stage: "closed-reentry-rejected", Binding: s.Binding, Mode: mode, Helper: self, Real: finalReal, Observed: observed, State: *s, Error: message, Exit: 1})
	// Like CLI watch on an emergency closed epoch, normal process exit is nonzero.
	// The journal/result is already durable; no t.Cleanup is needed in this child.
	os.Exit(1)
}
func recoveryExecutorBoundary(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "guard")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	target := recoveryFixture(t, dir, "executor-kill")
	owner := recoveryStart(t, "/bin/sleep", []string{"30"}, nil)
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	identity, e := guardProcess(owner.cmd.Process.Pid, n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	c := guardConfig{Version: "opsloop-guard/v1", Mode: "synthetic-local", Run: "boundary-" + mode, Epoch: "epoch-1", Target: target, Allowed: "query-resume-only", Root: root, Owner: identity, HardDeadline: n.Wall.Add(4 * time.Second), AuthorityExpires: n.Wall.Add(9 * time.Second), MaxTrialMS: 4000, HeartbeatMS: 1500, TimeoutMS: 300}
	config := filepath.Join(dir, "config.json")
	recoveryWrite(t, config, c)
	gate := filepath.Join(dir, "watch-boundary")
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	helper := recoveryStart(t, exe, []string{"-test.run=^TestRecoveryProcessHelper$"}, []string{"P2E_HELPER_CONFIG=" + config, "P2E_HELPER_GATE=" + gate, "P2E_HELPER_BOUNDARY=" + mode})
	t.Cleanup(func() {
		if t.Failed() {
			for _, suffix := range []string{".armed", ".observed", ".result"} {
				if b, e := os.ReadFile(gate + suffix); e == nil {
					t.Logf("retained synthetic boundary%s=%s", suffix, b)
				}
			}
			if b, e := os.ReadFile(helper.log); e == nil {
				t.Logf("helper PID=%d synthetic log=%s", helper.cmd.Process.Pid, b)
			}
		}
	})
	recoveryBoundaryWait(t, gate+".armed")
	var armed recoveryBoundaryAck
	if e = guardRead(gate+".armed", &armed); e != nil {
		t.Fatal(e)
	}
	if armed.Binding != c.binding() || armed.State.Config.binding() != c.binding() || armed.Helper.PID != helper.cmd.Process.Pid {
		t.Fatal("armed identity mismatch")
	}
	if calls, resumes := recoveryCalls(t, dir); calls != 0 || resumes != 0 {
		t.Fatal("arming issued commands", calls, resumes)
	}
	recoveryBoundaryWrite(t, gate+".observe", map[string]string{"binding": c.binding()})
	ownerWait := "alive during boundary"
	var ownerExit *int
	var parentBeforeKill *guardMoment
	if mode == "owner-exit" {
		recoveryBoundaryWait(t, gate+".observed")
		var ack recoveryBoundaryAck
		if e = guardRead(gate+".observed", &ack); e != nil {
			t.Fatal(e)
		}
		current, e := guardNow()
		if e != nil {
			t.Fatal(e)
		}
		currentOwner, e := guardProcess(owner.cmd.Process.Pid, current.Boot)
		reason, checkErr := ack.State.check(current)
		if ack.Binding != c.binding() || ack.Stage != "first-valid-observation" || currentOwner != identity || e != nil || reason != "" || checkErr != nil {
			t.Fatal("not valid before owner kill; no switching to late scenario", e, reason, checkErr)
		}
		if calls, resumes := recoveryCalls(t, dir); calls != 0 || resumes != 0 {
			t.Fatal("healthy watch issued commands", calls, resumes)
		}
		if e = owner.cmd.Process.Kill(); e != nil {
			t.Fatal(e)
		}
		owner.wait(t)
		if owner.cmd.ProcessState == nil || owner.cmd.ProcessState.ExitCode() != -1 {
			t.Fatal("owner kill not confirmed", owner.cmd.ProcessState)
		}
		code := owner.cmd.ProcessState.ExitCode()
		ownerExit = &code
		ownerWait = owner.err.Error()
		parentBeforeKill = &current
		recoveryBoundaryWrite(t, gate+".owner-killed", map[string]string{"binding": c.binding(), "ownerStart": identity.Start})
	}
	helper.wait(t)
	var result recoveryBoundaryAck
	if e = guardRead(gate+".result", &result); e != nil {
		output, _ := os.ReadFile(helper.log)
		t.Fatalf("helper result: %v log=%s", e, output)
	}
	if out := os.Getenv("P2E_BOUNDARY_OUT"); out != "" {
		if e = os.MkdirAll(out, 0700); e != nil {
			t.Fatal(e)
		}
		recoveryBoundaryWrite(t, filepath.Join(out, mode+".json"), map[string]any{"ack": result, "actualHelperExit": helper.cmd.ProcessState.ExitCode(), "actualOwnerExit": ownerExit, "ownerWait": ownerWait, "parentBeforeKill": parentBeforeKill})
	}
	if helper.err == nil || helper.cmd.ProcessState == nil || helper.cmd.ProcessState.ExitCode() != 1 || result.Exit != 1 || result.Binding != c.binding() || result.State.Config.binding() != c.binding() || result.Helper != armed.Helper {
		t.Fatal("helper termination or binding mismatch", helper.err)
	}
	s := result.State
	calls, resumes := recoveryCalls(t, dir)
	if s.ArmTick != armed.State.ArmTick || s.HardTick != armed.State.HardTick || s.ExpiryTick != armed.State.ExpiryTick || s.HeartbeatTick != armed.State.HeartbeatTick {
		t.Fatal("original time authority changed")
	}
	if mode == "authority-expired" {
		if s.Phase != "ABORTED_MANUAL" || calls != 0 || resumes != 0 || result.Error == "" {
			t.Fatal("expired authority allowed work", s.Phase, calls, resumes, result.Error)
		}
	} else {
		if s.Phase != "EMERGENCY_RESTORE_CONFIRMED" || !s.Emergency || resumes != 1 || calls != 5 || result.Error != "" {
			t.Fatal("valid restoration not confirmed", s.Phase, calls, resumes, result.Error)
		}
	}
	starts, results, intents := 0, 0, 0
	emergencyReason := ""
	manualReason := ""
	for i, event := range s.Events {
		if event.Kind == "manual-review" {
			manualReason = event.Detail
		}
		if event.Kind == "EMERGENCY_RESTORE" {
			emergencyReason = event.Detail
		}
		if event.Sequence != i+1 {
			t.Fatal("journal sequence changed")
		}
		if event.Kind == "vm-started" {
			starts++
		}
		if event.Kind == "command-result" {
			results++
		}
		if event.Kind == "resume-intent" {
			intents++
		}
	}
	wantReason := map[string]string{"owner-exit": "owner-exited", "heartbeat-late": "heartbeat-lost", "hard-deadline": "hard-deadline"}[mode]
	if mode != "authority-expired" && emergencyReason != wantReason {
		t.Fatal("boundary cause changed", mode, emergencyReason, wantReason)
	}
	if mode == "authority-expired" && (emergencyReason != "" || manualReason != "guard: restoration authority expired" || result.Error != "guard: guard: restoration authority expired") {
		t.Fatal("expired authority did not preserve its exact manual reason", emergencyReason, manualReason, result.Error)
	}
	if starts != calls || results != calls || intents != resumes || s.AutomaticSuccessEligible {
		t.Fatal("untruthful command or normal-success evidence", starts, results, intents, calls, resumes)
	}
	raw, _ := json.Marshal(result)
	t.Logf("mode=%s ownerWait=%s helperExit=%d helperWait=%v commands=%d resumes=%d ackAndJournal=%s", mode, ownerWait, helper.cmd.ProcessState.ExitCode(), helper.err, calls, resumes, raw)
}
func TestRecoveryBeforeFirstObservationBoundaries(t *testing.T) {
	for _, mode := range []string{"owner-exit", "heartbeat-late", "hard-deadline", "authority-expired"} {
		t.Run(mode, func(t *testing.T) { recoveryExecutorBoundary(t, mode) })
	}
}

const recoveryCLIDiagnosticMarker = "OPSLOOP_GUARD_CLI_DIAGNOSTIC "

// Diagnostic collection is read-only and runs at the actual test outcome, before
// harness cleanup. Missing observations remain UNKNOWN. No stdout/config/env or
// executable paths are serialized, and no Guard/Owner process is adopted by PID.
func recoveryCLIDiagnostic(c guardConfig, cli string, owner, guard *recoveryChild, armed *guardState, armDetected, guardWaitObserved, ownerKillRequested bool) map[string]any {
	process := func(child *recoveryChild) map[string]any {
		result := map[string]any{"pid": "UNKNOWN", "startTicks": "UNKNOWN_NOT_RECORDED", "wait": "UNKNOWN_OR_ACTIVE", "numericExit": "UNKNOWN", "signal": "UNKNOWN"}
		if child == nil {
			return result
		}
		result["pid"] = child.cmd.Process.Pid
		select {
		case <-child.done:
			result["wait"] = "COMPLETED"
			if child.cmd.ProcessState != nil {
				result["numericExit"] = child.cmd.ProcessState.ExitCode()
				if status, ok := child.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						result["signal"] = status.Signal().String()
					} else {
						result["signal"] = "NONE"
					}
				}
			}
		default:
		}
		return result
	}
	hashFile := func(path string) string {
		b, e := os.ReadFile(path)
		if e != nil {
			return "UNKNOWN_UNREADABLE"
		}
		return digestBytes(b)
	}
	result := map[string]any{"schema": "opsloop-guard-cli-watch-diagnostic/v1", "synthetic": true, "path": "production-cli-arm-and-watch", "scene": "executor-kill", "run": c.Run, "epoch": c.Epoch, "bindingSHA256": c.binding(), "snapshotStage": "test-outcome-before-harness-cleanup", "armDetected": armDetected, "guardWaitObservedBeforeFailure": guardWaitObserved, "ownerKillRequested": ownerKillRequested,
		"firstWatchObservation": "NOT_OBSERVED_BY_PRODUCTION_JOURNAL", "firstWatchClock": map[string]any{"wall": "NOT_OBSERVED", "monotonic": "NOT_OBSERVED"}, "armedRecord": "NOT_OBSERVED", "firstWatchRemaining": map[string]any{"heartbeat": "UNKNOWN", "hard": "UNKNOWN", "restorationAuthority": "UNKNOWN"},
		"ownerExpected": map[string]any{"pid": c.Owner.PID, "startTicks": c.Owner.Start, "bootSHA256": digestBytes([]byte(c.Owner.Boot))}, "ownerWait": process(owner), "guardWait": process(guard),
		"targetExecutableExpectedSHA256": c.Target.SHA256, "targetExecutableObservedSHA256": hashFile(c.Target.Executable), "guardCLIBinaryObservedSHA256": hashFile(cli), "guardGoSourceRevision": "UNKNOWN_USE_SEPARATE_CI_CHECKOUT_AND_SOURCE_SCAN",
		"fixedLimits": map[string]any{"maxTrialMS": c.MaxTrialMS, "heartbeatMS": c.HeartbeatMS, "commandTimeoutMS": c.TimeoutMS, "hardDeadline": c.HardDeadline, "restorationAuthorityExpires": c.AuthorityExpires}, "uncertainty": []string{"ARMED does not prove a first healthy watch observation", "per-target-command numeric exit and PID are not recorded by the current command journal"}}
	if guard == nil {
		result["path"] = "diagnostic-collector-no-cli-child"
		result["observationKind"] = "NO_CLI_CHILD_OBSERVED"
	} else {
		result["observationKind"] = "ACTUAL_PRODUCTION_CLI_CHILD"
	}
	if armed != nil {
		result["armedRecord"] = map[string]any{"armWall": armed.ArmWall, "armTick": armed.ArmTick, "hardTick": armed.HardTick, "expiryTick": armed.ExpiryTick, "heartbeatTick": armed.HeartbeatTick}
	}
	data, e := os.ReadFile(recoveryStatePath(c))
	if e != nil {
		result["journal"] = "UNKNOWN_UNREADABLE"
		return result
	}
	result["journalSHA256"] = digestBytes(data)
	var state guardState
	if json.Unmarshal(data, &state) != nil {
		result["journal"] = "UNKNOWN_UNDECODABLE"
		return result
	}
	result["finalPhase"] = state.Phase
	result["lastRecordedTick"] = state.LastTick
	result["automaticSuccessEligible"] = state.AutomaticSuccessEligible
	commands := map[string]map[string]any{}
	for _, op := range []string{"domuuid", "domstate", "resume"} {
		commands[op] = map[string]any{"intent": 0, "startAttempt": 0, "started": 0, "result": 0, "numericExit": "UNKNOWN_NOT_RECORDED", "pid": "UNKNOWN_NOT_RECORDED", "resultDetails": []string{}}
	}
	summaries := []map[string]any{}
	firstUnconfirmed := "NONE_RECORDED"
	for _, event := range state.Events {
		if row := commands[event.Operation]; row != nil {
			key := map[string]string{"command-intent": "intent", "vm-start-attempt": "startAttempt", "vm-started": "started", "command-result": "result"}[event.Kind]
			if key != "" {
				row[key] = row[key].(int) + 1
			}
			if event.Kind == "command-result" {
				detail := event.Detail
				switch detail {
				case "unconfirmed", "exit-zero-not-confirmation", "running", "paused", "shut off", finishTestUUID:
				default:
					detail = "UNKNOWN_DETAIL_NOT_EXPORTED"
				}
				row["resultDetails"] = append(row["resultDetails"].([]string), detail)
				if detail == "unconfirmed" && firstUnconfirmed == "NONE_RECORDED" {
					firstUnconfirmed = event.Operation
				}
			}
		}
		if len(summaries) < 128 {
			item := map[string]any{"sequence": event.Sequence, "kind": event.Kind, "operation": event.Operation, "at": event.At}
			if event.Kind == "EMERGENCY_RESTORE" || event.Kind == "manual-review" {
				detail := event.Detail
				switch detail {
				case "owner-exited", "hard-deadline", "heartbeat-lost", "prior-emergency", "identity/state unconfirmed", "restore unconfirmed; manual review", "guard: restoration authority expired":
				default:
					detail = "UNKNOWN_DETAIL_NOT_EXPORTED"
				}
				item["reason"] = detail
			}
			summaries = append(summaries, item)
		}
	}
	result["commands"] = commands
	result["firstUnconfirmedOperation"] = firstUnconfirmed
	result["journalEvents"] = summaries
	result["journalEventSummaryTruncated"] = len(state.Events) > len(summaries)
	return result
}
func recoveryLogCLIDiagnostic(t *testing.T, c guardConfig, cli string, owner, guard *recoveryChild, armed *guardState, armDetected, guardWaitObserved, ownerKillRequested bool) {
	t.Helper()
	data, e := json.Marshal(recoveryCLIDiagnostic(c, cli, owner, guard, armed, armDetected, guardWaitObserved, ownerKillRequested))
	if e != nil {
		t.Logf("%s{\"collection\":\"UNKNOWN_MARSHAL_FAILURE\"}", recoveryCLIDiagnosticMarker)
		return
	}
	t.Logf("%s%s", recoveryCLIDiagnosticMarker, data)
}
func TestRecoveryCLIDiagnosticFailureHelper(t *testing.T) {
	if os.Getenv("P2E_DIAGNOSTIC_FAILURE") != "synthetic" {
		return
	}
	defer recoveryLogCLIDiagnostic(t, guardConfig{Run: "synthetic-diagnostic-failure", Epoch: "epoch-1"}, "", nil, nil, nil, false, false, false)
	t.Fatal("synthetic deliberate failure for diagnostic capture")
}
func TestRecoveryCLIDiagnosticOnFailure(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestRecoveryCLIDiagnosticFailureHelper$")
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "P2E_DIAGNOSTIC_FAILURE=synthetic"}
	data, e := cmd.CombinedOutput()
	if e == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatal("forced failure did not fail", e)
	}
	if !bytes.Contains(data, []byte(recoveryCLIDiagnosticMarker)) || !bytes.Contains(data, []byte("NOT_OBSERVED_BY_PRODUCTION_JOURNAL")) || !bytes.Contains(data, []byte("synthetic deliberate failure")) {
		t.Fatal("forced failure diagnostic was not retained")
	}
	if !bytes.Contains(data, []byte("diagnostic-collector-no-cli-child")) || !bytes.Contains(data, []byte("NO_CLI_CHILD_OBSERVED")) {
		t.Fatal("negative diagnostic masqueraded as production CLI evidence")
	}
	if bytes.Contains(data, []byte("P2E_DIAGNOSTIC_FAILURE=")) {
		t.Fatal("diagnostic serialized environment")
	}
	t.Log("forced-failure child retained structured diagnostic and truthful UNKNOWN fields")
}
