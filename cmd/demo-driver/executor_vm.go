package main

import (
	"context"
	"errors"
	"regexp"
)

var finishUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Normal finish-run remains restricted to the hash-approved test executable.
// Recovery Guard uses a separate backend and approval boundary.
type finishVMRunner interface {
	RunStarted(context.Context, []string, func(string) error) (string, error)
}

// Call/Dispatch are immutable event references in the same action journal.
// AfterStart is the local upper observation of Start success, not VM effect time.
type finishVMStart struct {
	Call, Dispatch          int
	Operation, UUID         string
	BeforeStart, AfterStart Stamp
}

func (x *finisher) vm(ctx context.Context, op string) (string, error) {
	if op != "domuuid" && op != "domstate" && op != "resume" {
		return "", errors.New("finish: VM operation blocked")
	}
	args := []string{"--connect", x.c.VMURI, op, x.c.VMUUID}
	if e := x.j.add("vm-call", struct{ Operation, UUID string }{op, x.c.VMUUID}); e != nil {
		return "", e
	}
	callCtx, cancel := context.WithTimeout(ctx, x.c.timeout())
	defer cancel()
	call := x.j.last("vm-call").Seq
	dispatch := 0
	if d := x.j.last("resume-dispatch"); op == "resume" && d != nil {
		dispatch = d.Seq
	}
	started := false
	var before Stamp
	boundary := func(kind string) error {
		if kind == "vm-started" {
			started = true
		}
		at := x.j.capture()
		var after Stamp
		if kind == "vm-start-attempt" {
			before = at
		} else if kind == "vm-started" {
			after = at
		}
		return x.j.add(kind, finishVMStart{Call: call, Dispatch: dispatch, Operation: op, UUID: x.c.VMUUID, BeforeStart: before, AfterStart: after})
	}
	var out string
	var e error
	if x.c.Live != nil {
		if x.guardCall == nil {
			return "", errors.New("finish: live command requires target coordination")
		}
		out, e = x.guardCall(callCtx, op, boundary)
	} else {
		out, e = x.runner.RunStarted(callCtx, args, boundary)
	}
	// Never log arbitrary executable output. Only the validated UUID/state below.
	result := "unconfirmed"
	if e == nil && (out == x.c.VMUUID || out == "running" || out == "paused" || out == "shut off") {
		result = out
	}
	if op == "resume" && e == nil {
		result = "exit-zero-not-confirmation"
	}
	if je := x.j.add("vm-result", struct {
		Operation, Result, Problem string
		Call                       int
		Started                    bool
	}{op, result, finishVMProblem(e), call, started}); je != nil {
		return "", je
	}
	return out, e
}
func (x *finisher) vmState(ctx context.Context) (string, error) {
	uuid, e := x.vm(ctx, "domuuid")
	if e != nil || uuid != x.c.VMUUID {
		return "", errors.New("finish: VM UUID unconfirmed")
	}
	state, e := x.vm(ctx, "domstate")
	if e != nil || (state != "paused" && state != "running") {
		return "", errors.New("finish: VM state requires manual intervention")
	}
	return state, nil
}

// Only controlled runner diagnostics are journaled, never arbitrary output.
func finishVMProblem(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "context canceled", "context deadline exceeded", "fixture unavailable", "fixture invalid", "fixture hash mismatch", "fixture Start failed", "fixture process ownership unavailable", "fixture group termination failed", "fixture group reap failed", "fixture group cleanup unconfirmed", "fixture command failed or timed out":
		return err.Error()
	}
	return "command-or-journal-unconfirmed"
}
