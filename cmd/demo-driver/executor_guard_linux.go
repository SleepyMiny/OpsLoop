//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

type finishLiveAuthorization struct {
	Version           string    `json:"version"`
	ConfigHash        string    `json:"configHash"`
	ApprovalReference string    `json:"approvalReference"`
	Operator          string    `json:"operator"`
	Scope             string    `json:"scope"`
	NotBefore         time.Time `json:"notBefore"`
	Expires           time.Time `json:"expires"`
}

func finishGuardConfig(c FinishConfig) (guardConfig, error) {
	var g guardConfig
	if c.Live == nil {
		return g, errors.New("finish: lab configuration missing")
	}
	if e := guardRead(c.Live.GuardConfigFile, &g); e != nil {
		return g, e
	}
	if e := g.validate(); e != nil {
		return g, e
	}
	if g.Mode != "live-libvirt" || g.Root != c.Live.GuardRoot || g.Run != c.RunID || g.Epoch != c.Live.Epoch || g.binding() != c.Live.GuardBinding || g.Target != (recoveryTarget{c.VMURI, c.VMUUID, c.Executable, c.ExecutableSHA256}) || c.TimeoutMS > g.TimeoutMS {
		return g, errors.New("finish: Guard root/run/epoch/config/target mismatch")
	}
	return g, nil
}
func authorizeFinishLive(c FinishConfig) error {
	if c.Live == nil {
		return nil
	}
	g, e := finishGuardConfig(c)
	if e != nil {
		return e
	}
	if e = guardAuthorize(g, c.Live.GuardAuthorizationFile); e != nil {
		return e
	}
	var a finishLiveAuthorization
	if e = guardRead(c.Live.AuthorizationFile, &a); e != nil {
		return e
	}
	n := time.Now()
	if a.Version != "opsloop-finish-authorization/v1" || a.ConfigHash != c.binding() || a.Operator == "" || a.ApprovalReference == "" || a.Scope != "normal-finish-policy-stop-query-resume" || a.NotBefore.IsZero() || n.Before(a.NotBefore) || !n.Before(a.Expires) || !a.Expires.After(a.NotBefore) || a.Expires.After(g.AuthorityExpires) {
		return errors.New("finish: separate normal restoration approval missing/mismatched/expired")
	}
	return nil
}
func finishLiveInputHashes(c FinishConfig, input string) (finishLiveInputs, error) {
	l := c.Live
	r := finishLiveInputs{InputSHA256: input}
	for _, v := range []struct {
		path string
		dst  *string
	}{{l.GuardConfigFile, &r.GuardConfigSHA256}, {l.GuardAuthorizationFile, &r.GuardAuthorizationSHA256}, {l.AuthorizationFile, &r.AuthorizationSHA256}} {
		b, e := guardFileBytes(v.path, 2<<20)
		if e != nil {
			return r, e
		}
		*v.dst = digestBytes(b)
	}
	var a finishLiveAuthorization
	if e := guardRead(l.AuthorizationFile, &a); e != nil {
		return r, e
	}
	r.ApprovalReference = a.ApprovalReference
	return r, nil
}

// No Guard ownership, deadline, heartbeat or phase is changed by normal Finish.
func (x *finisher) freshFinishGuard(g *guardStore, c guardConfig) error {
	if x.j.failed {
		return errors.New("finish: journal failed")
	}
	if e := authorizeFinishLive(x.c); e != nil {
		return e
	}
	if e := x.bindLiveInputs(); e != nil {
		return e
	}
	s, e := g.load(c)
	if e != nil {
		return e
	}
	n, e := guardNow()
	if e != nil {
		return e
	}
	reason, e := s.check(n)
	if e != nil {
		return e
	}
	if reason != "" || s.Phase != "ARMED" || s.Emergency || s.ResumeIntent || s.closed() {
		return errors.New("finish: fresh Guard rejects normal restoration")
	}
	// Observation only: no borrowed source/fault time and no disarm assertion.
	return x.j.add("guard-observed", struct {
		Binding, Phase         string
		HeartbeatSequence      uint64
		HardTick, ObservedTick int64
	}{s.Binding, s.Phase, s.HeartbeatSequence, s.HardTick, n.Tick})
}

// Finish waits on the same target flock as Guard, but its own cancellation and
// operation budget must also bound contention. No background acquirer survives
// the caller, and only a descriptor acquired by this call is ever closed.
func acquireFinishGuard(ctx context.Context, c guardConfig) (*guardStore, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	wait, cancel := context.WithTimeout(ctx, 6*time.Duration(c.TimeoutMS)*time.Millisecond+2*time.Second)
	defer cancel()
	for {
		if e := wait.Err(); e != nil {
			return nil, e
		}
		g, e := openGuard(c)
		if e == nil {
			if e = wait.Err(); e != nil {
				g.close()
				return nil, e
			}
			return g, nil
		}
		if e != errGuardBusy {
			return nil, e
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return nil, wait.Err()
		case <-timer.C:
		}
	}
}

func (x *finisher) checkLiveGuard(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if x.c.Live == nil {
		return nil
	}
	c, e := finishGuardConfig(x.c)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Duration(c.TimeoutMS)*time.Millisecond+time.Second)
	defer cancel()
	g, e := acquireFinishGuard(ctx, c)
	if e != nil {
		return e
	}
	defer g.close()
	if e = x.freshFinishGuard(g, c); e != nil {
		return e
	}
	return ctx.Err()
}

type finishNormalAttempt struct {
	Version       string `json:"version"`
	GuardBinding  string `json:"guardBinding"`
	FinishBinding string `json:"finishBinding"`
	Run           string `json:"run"`
	Epoch         string `json:"epoch"`
	URI           string `json:"uri"`
	UUID          string `json:"uuid"`
}

func normalAttemptPath(g *guardStore) string { return g.path + ".finish-resume" }

// Guard reads this under the very same target lock before its own resume.
// A write-ahead attempt does not prove Start or effect. Paused after such an
// attempt is unresolved/manual; neither actor may replay an uncertain command.
func guardNormalAttempt(g *guardStore, c guardConfig) (*finishNormalAttempt, error) {
	var a finishNormalAttempt
	if e := guardRead(normalAttemptPath(g), &a); e != nil {
		if os.IsNotExist(e) {
			return nil, nil
		}
		return nil, e
	}
	if a.Version != "opsloop-finish-normal-attempt/v1" || a.GuardBinding != c.binding() || !recoveryHash.MatchString(a.FinishBinding) || a.Run != c.Run || a.Epoch != c.Epoch || a.URI != c.Target.URI || a.UUID != c.Target.UUID {
		return nil, errors.New("guard: normal attempt binding invalid")
	}
	return &a, nil
}
func (x *finisher) restoreLab(parent context.Context) (string, error) {
	reject := func(e error) (string, error) { return "RESTORE_UNCONFIRMED", e }
	c, e := finishGuardConfig(x.c)
	if e != nil {
		return reject(e)
	}
	// Kubernetes stop polling has already completed without a target lock.
	if _, e = x.guard(parent); e != nil {
		return "STOP_CONTRADICTED", e
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Duration(c.TimeoutMS)*time.Millisecond+time.Second)
	defer cancel()
	g, e := acquireFinishGuard(ctx, c)
	if e != nil {
		return reject(e)
	}
	defer g.close()
	permit := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := x.freshFinishGuard(g, c); e != nil {
			return e
		}
		return ctx.Err()
	}
	if e = permit(); e != nil {
		return reject(e)
	}
	x.guardCall = func(ctx context.Context, op string, boundary func(string) error) (string, error) {
		commandPermit := func() error {
			if e := permit(); e != nil {
				return e
			}
			if op == "resume" {
				if _, e := x.guard(ctx); e != nil {
					return e
				}
				current, e := x.vmState(ctx)
				if e != nil || current != "paused" {
					return errors.New("finish: target no longer confirmed paused before Start")
				}
				return permit()
			}
			return nil
		}
		b := recoveryBackend{target: c.Target, runner: x.runner, timeout: x.c.timeout(), permit: commandPermit, record: func(kind, op, detail string) error {
			if kind == "vm-start-attempt" || kind == "vm-started" {
				return boundary(kind)
			}
			return x.j.add("guard-backend", struct{ Kind, Operation, Detail string }{kind, op, detail})
		}}
		return b.call(ctx, op)
	}
	defer func() { x.guardCall = nil }()
	state, e := x.vmState(ctx)
	if e != nil {
		return reject(e)
	}
	attempt, e := guardNormalAttempt(g, c)
	if e != nil {
		return reject(e)
	}
	if attempt != nil && attempt.FinishBinding != x.c.binding() {
		return reject(errors.New("finish: foreign normal attempt"))
	}
	if x.j.last("resume-dispatch") != nil && attempt == nil {
		return reject(errors.New("finish: missing shared normal attempt"))
	}
	if state == "paused" && x.j.last("resume-dispatch") == nil {
		if attempt != nil {
			return reject(errors.New("finish: prior normal attempt unresolved; no replay"))
		}
		if _, e = x.guard(ctx); e != nil {
			return "STOP_CONTRADICTED", e
		}
		// Re-query immediately before dispatch; changed UUID/state is not repaired.
		state, e = x.vmState(ctx)
		if e != nil {
			return reject(e)
		}
		if state != "paused" {
			return reject(errors.New("finish: target changed before resume"))
		}
		if e = x.j.add("resume-intent", struct{ UUID string }{x.c.VMUUID}); e != nil {
			return reject(e)
		}
		a := finishNormalAttempt{"opsloop-finish-normal-attempt/v1", c.binding(), x.c.binding(), c.Run, c.Epoch, c.Target.URI, c.Target.UUID}
		b, _ := json.Marshal(a)
		if e = finishExclusive(normalAttemptPath(g), b); e != nil {
			return reject(e)
		}
		if e = x.j.add("resume-dispatch", struct{ UUID string }{x.c.VMUUID}); e != nil {
			return reject(e)
		}
		// The backend repeats the fresh permit immediately before command Start.
		_, commandErr := x.vm(ctx, "resume")
		if x.j.failed {
			return reject(commandErr)
		}
		if e = x.j.add("resume-response", struct{ ExitOK bool }{commandErr == nil}); e != nil {
			return reject(e)
		}
	} else if state == "running" && x.j.last("resume-dispatch") == nil {
		if e = x.j.add("running-noop", struct{ UUID string }{x.c.VMUUID}); e != nil {
			return reject(e)
		}
	}
	// Query first after lost response/reentry. An unresolved paused state is never
	// permission to send another resume, even if Start was not observed.
	state, e = x.vmState(ctx)
	if e != nil || state != "running" {
		return reject(errors.New("finish: resume effect unconfirmed; no replay"))
	}
	if e = permit(); e != nil {
		return reject(e)
	}
	if x.j.last("resume-dispatch") != nil && x.j.last("resume-confirmed") == nil {
		if e = x.j.add("resume-confirmed", struct{ UUID, State string }{x.c.VMUUID, state}); e != nil {
			return reject(e)
		}
	}
	return "", nil
}
