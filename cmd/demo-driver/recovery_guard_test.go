//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recoveryFake struct {
	target                           recoveryTarget
	state                            string
	calls                            [][]string
	resumes                          int
	loss, noEffect, wrongID, missing bool
	onCall                           func(string)
}

func (v *recoveryFake) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
	if len(args) != 4 || args[0] != "--connect" || args[1] != v.target.URI || args[3] != v.target.UUID {
		return "", errors.New("wrong fixed argv")
	}
	if e := boundary("vm-start-attempt"); e != nil {
		return "", e
	}
	if e := ctx.Err(); e != nil {
		return "", e
	}
	v.calls = append(v.calls, append([]string(nil), args...))
	if v.onCall != nil {
		v.onCall(args[2])
	}
	if e := boundary("vm-started"); e != nil {
		return "", e
	}
	switch args[2] {
	case "domuuid":
		if v.missing {
			return "", os.ErrNotExist
		}
		if v.wrongID {
			return "foreign", nil
		}
		return v.target.UUID, nil
	case "domstate":
		return v.state, nil
	case "resume":
		v.resumes++
		if !v.noEffect {
			v.state = "running"
		}
		if v.loss {
			return "", context.DeadlineExceeded
		}
		return "", nil
	}
	return "", errors.New("forbidden operation")
}
func recoverySetup(t *testing.T) (guardConfig, *guardStore, *guardState, *recoveryFake, *guardMoment) {
	t.Helper()
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	o, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	if e = os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	c := guardConfig{Version: "opsloop-guard/v1", Mode: "synthetic-local", Run: "unit-run", Epoch: "epoch-1", Target: recoveryTarget{"qemu:///system", finishTestUUID, "/placeholder/fake-command", strings.Repeat("0", 64)}, Allowed: "query-resume-only", Root: root, Owner: o, HardDeadline: n.Wall.Add(2 * time.Second), AuthorityExpires: n.Wall.Add(10 * time.Second), MaxTrialMS: 2000, HeartbeatMS: 500, TimeoutMS: 50}
	if e = c.validate(); e != nil {
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
	v := &recoveryFake{target: c.Target, state: "paused"}
	return c, g, s, v, &n
}
func recoveryEngine(n *guardMoment, v finishVMRunner) guardEngine {
	return guardEngine{func() (guardMoment, error) { return *n, nil }, v}
}
func recoveryAdvance(n *guardMoment, d time.Duration) { n.Tick += int64(d); n.Wall = n.Wall.Add(d) }
func TestRecoveryBackendFixedBoundary(t *testing.T) {
	for _, scene := range []string{"paused", "running", "shut off", "missing", "unexpected", "wrong-uuid", "loss", "timeout-no-effect"} {
		t.Run(scene, func(t *testing.T) {
			_, g, s, v, n := recoverySetup(t)
			switch scene {
			case "paused", "running", "shut off":
				v.state = scene
			case "missing":
				v.missing = true
			case "wrong-uuid":
				v.wrongID = true
			case "unexpected":
				v.state = "pmsuspended"
			case "loss":
				v.loss = true
			case "timeout-no-effect":
				v.loss = true
				v.noEffect = true
			}
			recoveryAdvance(n, 2*time.Second)
			e := recoveryEngine(n, v).step(context.Background(), g, s, false)
			good := scene == "paused" || scene == "running" || scene == "loss"
			if good && (e != nil || s.Phase != "EMERGENCY_RESTORE_CONFIRMED") {
				t.Fatalf("%s %v", s.Phase, e)
			}
			if !good && (e == nil || s.Phase != "ABORTED_MANUAL") {
				t.Fatalf("manual required: %s %v", s.Phase, e)
			}
			want := 0
			if scene == "paused" || scene == "loss" || scene == "timeout-no-effect" {
				want = 1
			}
			if v.resumes != want {
				t.Fatalf("resumes=%d want=%d", v.resumes, want)
			}
			if s.AutomaticSuccessEligible || !s.Emergency || s.ConsumerIntegration != "UNINTEGRATED" {
				t.Fatal("false normal success")
			}
			loaded, e := g.load(s.Config)
			if e != nil || loaded.Phase != s.Phase {
				t.Fatalf("durable receipt: %v", e)
			}
			if scene == "loss" && (len(v.calls) != 5 || v.calls[3][2] != "domuuid" || v.calls[4][2] != "domstate") {
				t.Fatal("missing fresh query after loss")
			}
			t.Logf("scene=%s calls=%d resumes=%d phase=%s", scene, len(v.calls), v.resumes, s.Phase)
		})
	}
}
func TestRecoveryBackendRejectsTargets(t *testing.T) {
	for _, uri := range []string{"", "qemu+ssh://host/system", "qemu://host/system", "test:///default", "qemu:///system?socket=/tmp/x", "qemu:///system;id"} {
		t.Run(uri, func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			c.Target.URI = uri
			b := recoveryEngine(n, v).backend(g, s)
			b.target = c.Target
			if _, e := b.state(context.Background()); e == nil || len(v.calls) != 0 {
				t.Fatal("invalid target ran")
			}
		})
	}
	_, g, s, v, n := recoverySetup(t)
	b := recoveryEngine(n, v).backend(g, s)
	for _, op := range []string{"start", "destroy", "reset", "undefine", "suspend", "list"} {
		if _, e := b.call(context.Background(), op); e == nil {
			t.Fatal(op)
		}
	}
	if len(v.calls) != 0 {
		t.Fatal("forbidden call")
	}
}
func TestRecoveryTimingAndRestart(t *testing.T) {
	for _, scene := range []string{"hard-with-heartbeat", "heartbeat-lost", "expired", "boot", "clock-back", "clock-forward", "monotonic-back", "restart", "owner-reused"} {
		t.Run(scene, func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			original := s.HardTick
			switch scene {
			case "expired":
				recoveryAdvance(n, 11*time.Second)
			case "boot":
				n.Boot = "other-boot"
			case "clock-back":
				n.Wall = n.Wall.Add(-3 * time.Second)
			case "clock-forward":
				n.Wall = n.Wall.Add(3 * time.Second)
			case "monotonic-back":
				n.Tick--
			case "heartbeat-lost":
				recoveryAdvance(n, time.Second)
			case "owner-reused":
				s.Config.Owner.Start = "0"
			default:
				recoveryAdvance(n, 2*time.Second)
				s.HeartbeatTick = n.Tick
			}
			if scene == "restart" {
				var e error
				s, e = g.load(c)
				if e != nil {
					t.Fatal(e)
				}
			}
			e := recoveryEngine(n, v).step(context.Background(), g, s, false)
			invalid := scene == "expired" || scene == "boot" || strings.HasPrefix(scene, "clock") || scene == "monotonic-back"
			if invalid {
				if e == nil || len(v.calls) != 0 {
					t.Fatal("uncertain/expired time issued commands")
				}
			} else if e != nil || v.resumes != 1 {
				t.Fatalf("restore: %v resumes=%d", e, v.resumes)
			}
			if s.HardTick != original {
				t.Fatal("deadline extended")
			}
		})
	}
}
func TestRecoveryPersistenceAndReplay(t *testing.T) {
	for _, scene := range []string{"arm-save", "intent-save", "started-save", "receipt-save", "restart-intent"} {
		t.Run(scene, func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			recoveryAdvance(n, 2*time.Second)
			switch scene {
			case "arm-save":
				c.Epoch = "epoch-2"
				s.Phase = "DISARMED"
				if e := g.event(s, *n, "disarmed", "", "test setup"); e != nil {
					t.Fatal(e)
				}
				g.close()
				next, e := openGuard(c)
				if e != nil {
					t.Fatal(e)
				}
				defer next.close()
				next.beforeSave = func(*guardState) error { return errors.New("disk") }
				n.Wall = n.Wall.Add(-time.Second)
				n.Tick -= int64(time.Second)
				if _, e = guardArm(next, c, *n); e == nil {
					t.Fatal("arm acknowledged before durability")
				}
				return
			case "intent-save":
				g.beforeSave = func(s *guardState) error {
					if s.ResumeIntent {
						return errors.New("disk")
					}
					return nil
				}
			case "started-save":
				g.beforeSave = func(s *guardState) error {
					for _, e := range s.Events {
						if e.Kind == "vm-started" && e.Operation == "resume" {
							return errors.New("disk")
						}
					}
					return nil
				}
			case "receipt-save":
				g.beforeSave = func(s *guardState) error {
					if s.Phase == "EMERGENCY_RESTORE_CONFIRMED" {
						return errors.New("disk")
					}
					return nil
				}
			case "restart-intent":
				s.ResumeIntent = true
				s.Emergency = true
				s.Phase = "RESTORING"
				if e := g.event(s, *n, "EMERGENCY_RESTORE", "", "test interruption"); e != nil {
					t.Fatal(e)
				}
				if e := g.event(s, *n, "resume-intent", "resume", "interrupted before Start"); e != nil {
					t.Fatal(e)
				}
			}
			if e := recoveryEngine(n, v).step(context.Background(), g, s, false); e == nil {
				t.Fatal("persistence/ambiguous intent reported success")
			}
			before := v.resumes
			g.beforeSave = nil
			loaded, e := g.load(c)
			if e != nil {
				t.Fatal(e)
			}
			if !loaded.closed() {
				_ = recoveryEngine(n, v).step(context.Background(), g, loaded, false)
			}
			// A failed pre-command intent write may be retried; an existing intent may not.
			if scene != "intent-save" && v.resumes != before {
				t.Fatal("ambiguous resume replayed")
			}
			if scene == "restart-intent" && v.resumes != 0 {
				t.Fatal("intent treated as new action")
			}
		})
	}
}
func TestRecoveryDisarmAndMessages(t *testing.T) {
	c, g, s, v, n := recoverySetup(t)
	engine := recoveryEngine(n, v)
	if e := engine.step(context.Background(), g, s, true); e == nil {
		t.Fatal("paused disarm")
	}
	v.state = "running"
	if e := engine.step(context.Background(), g, s, true); e != nil {
		t.Fatal(e)
	}
	if v.resumes != 0 || s.Phase != "DISARMED" || s.Emergency {
		t.Fatal("unnecessary emergency")
	}
	if _, e := guardArm(g, c, *n); e == nil {
		t.Fatal("closed rearm")
	}
	changed := c
	changed.Target.UUID = "00000000-0000-0000-0000-000000000000"
	if _, e := g.load(changed); e == nil {
		t.Fatal("foreign config")
	}
	changed = c
	changed.Epoch = "foreign"
	if _, e := g.load(changed); e == nil {
		t.Fatal("foreign epoch")
	}
}
func TestRecoveryPlanAndApproval(t *testing.T) {
	c, g, _, v, _ := recoverySetup(t)
	g.close()
	p := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(c)
	os.WriteFile(p, b, 0600)
	for _, args := range [][]string{{"plan", "-config", p}, {"arm", "-config", p}, {"watch", "-config", p}} {
		var out bytes.Buffer
		if e := runGuard(args, &out); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(out.String(), `"commands":0`) {
			t.Fatal(out.String())
		}
	}
	if len(v.calls) != 0 {
		t.Fatal("plan called backend")
	}
	c.Mode = "live-libvirt"
	if e := guardAuthorize(c, ""); e == nil {
		t.Fatal("live boundary absent")
	}
	if e := guardAuthorize(c, p); e == nil {
		t.Fatal("config is not authorization")
	}
}
func TestRecoveryPermitAtStart(t *testing.T) {
	_, g, s, v, n := recoverySetup(t)
	x := recoveryEngine(n, v)
	b := x.backend(g, s)
	// Time can expire after intent but before actual Start.
	original := b.record
	b.record = func(k, o, d string) error {
		e := original(k, o, d)
		if k == "command-intent" {
			recoveryAdvance(n, 11*time.Second)
		}
		return e
	}
	if e := b.resume(context.Background()); e == nil || len(v.calls) != 0 {
		t.Fatal("expired Start")
	}
}

func TestRecoveryStorageAndAuthorization(t *testing.T) {
	t.Run("active-target-and-bounded-lock", func(t *testing.T) {
		c, g, s, _, n := recoverySetup(t)
		began := time.Now()
		if other, e := openGuard(c); e != errGuardBusy {
			if other != nil {
				other.close()
			}
			t.Fatalf("lock=%v", e)
		}
		if time.Since(began) > time.Second {
			t.Fatal("unbounded lock")
		}
		next := c
		next.Epoch = "epoch-2"
		g.close()
		other, e := openGuard(next)
		if e != nil {
			t.Fatal(e)
		}
		defer other.close()
		if _, e = guardArm(other, next, *n); e == nil {
			t.Fatal("parallel armed target")
		}
		if s.Phase != "ARMED" {
			t.Fatal("lost original responsibility")
		}
	})
	t.Run("malformed-state", func(t *testing.T) {
		c, g, _, v, n := recoverySetup(t)
		if e := os.WriteFile(g.path, []byte(`{"phase":"ARMED"}`), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := g.load(c); e == nil {
			t.Fatal("accepted corrupt state")
		}
		if len(v.calls) != 0 {
			t.Fatal("unknown identity executed")
		}
		_ = n
	})
	t.Run("live-approval-binding", func(t *testing.T) {
		c, _, _, _, _ := recoverySetup(t)
		c.Mode = "live-libvirt"
		c.Target.Executable = "/placeholder/virsh"
		p := filepath.Join(t.TempDir(), "approval.json")
		a := guardLiveAuthorization{Version: "opsloop-guard-authorization/v1", ConfigHash: c.binding(), ApprovalReference: "synthetic-reference-not-live-authorization", Operator: "synthetic-test", Expires: c.AuthorityExpires, Scope: "dedicated-test-target-query-resume"}
		recoveryWrite(t, p, a)
		if e := guardAuthorize(c, p); e != nil {
			t.Fatal(e)
		}
		c.Epoch = "foreign"
		if e := guardAuthorize(c, p); e == nil {
			t.Fatal("approval reused for foreign epoch")
		}
	})
	t.Run("late-disarm-cannot-hide-emergency", func(t *testing.T) {
		_, g, s, v, n := recoverySetup(t)
		recoveryAdvance(n, time.Second)
		v.state = "running"
		if e := recoveryEngine(n, v).step(context.Background(), g, s, true); e == nil {
			t.Fatal("late disarm accepted")
		}
		if e := recoveryEngine(n, v).step(context.Background(), g, s, false); e != nil {
			t.Fatal(e)
		}
		if !s.Emergency || s.Phase != "EMERGENCY_RESTORE_CONFIRMED" {
			t.Fatal("emergency hidden")
		}
	})
}

func TestRecoveryRejectsChangedDurableDeadline(t *testing.T) {
	c, g, s, _, _ := recoverySetup(t)
	s.HardTick--
	if e := g.save(s); e != nil {
		t.Fatal(e)
	}
	if _, e := g.load(c); e == nil {
		t.Fatal("changed hard deadline accepted")
	}
}
