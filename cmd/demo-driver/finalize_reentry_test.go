//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func finalFixExport(t *testing.T, name string, b []byte) {
	t.Helper()
	dir := os.Getenv("P2F_FIX_REPRO")
	if dir == "" {
		return
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
		t.Fatal(e)
	}
}

// Uses the real collector/Finish fixture, Guard arm/store/backend/step/load and
// separately built production CLI. No terminal JSON editing, no live backend.
func TestFinalizeReentryCLI(t *testing.T) {
	for _, scene := range []string{"normal", "retry-normal", "interrupted-only", "retry-paused", "retry-emergency", "retry-manual", "unconfirmed-then-running", "shut-off-then-running"} {
		t.Run(scene, func(t *testing.T) {
			finishObserverRuntime(t, "normal", func(oc ObserverConfig) func(*finishGateVM) {
				fc, g, state, n := finalPrepare(t, oc)
				return func(vm *finishGateVM) {
					config := filepath.Join(fc.Guard.Root, "finalize-config.json")
					mapped := &finalMappedVM{vm: vm, target: fc.Guard.Target}
					recoveryAdvance(n, 86*time.Second)
					stepErr := ""
					if scene != "normal" {
						injected := false
						g.beforeSave = func(s *guardState) error {
							e := s.Events[len(s.Events)-1]
							if !injected && e.Kind == "command-intent" && e.Operation == "domstate" {
								injected = true
								return errors.New("review injected beforeSave domstate intent")
							}
							return nil
						}
						e := recoveryEngine(n, mapped).step(context.Background(), g, state, true)
						if e == nil || !strings.Contains(e.Error(), "durable evidence failure") || !injected {
							t.Fatalf("missing injected failure: %v", e)
						}
						stepErr = e.Error()
						b, e := os.ReadFile(g.path)
						if e != nil {
							t.Fatal(e)
						}
						finalFixExport(t, scene+"-interrupted-guard.json", b)
						g.close()
						reopened, e := openGuard(fc.Guard)
						if e != nil {
							t.Fatal(e)
						}
						g = reopened
						defer g.close()
						state, e = g.load(fc.Guard)
						if e != nil {
							t.Fatal(e)
						}
						if state.Phase != "ARMED" || len(state.Events) != 5 {
							t.Fatalf("unexpected durable prefix: %s %d", state.Phase, len(state.Events))
						}
						recoveryAdvance(n, time.Second)
					}
					switch scene {
					case "normal", "retry-normal":
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, true); e != nil {
							t.Fatal(e)
						}
					case "unconfirmed-then-running", "shut-off-then-running":
						var e error
						fake := &recoveryFake{target: fc.Guard.Target, state: "shut off", wrongID: scene == "unconfirmed-then-running"}
						g.beforeSave = func(next *guardState) error {
							if next.Events[len(next.Events)-1].Kind == "manual-review" {
								return errors.New("injected manual save failure")
							}
							return nil
						}
						if e := recoveryEngine(n, fake).step(context.Background(), g, state, true); e == nil {
							t.Fatal("expected counterevidence")
						}
						g.close()
						g, e = openGuard(fc.Guard)
						if e != nil {
							t.Fatal(e)
						}
						defer g.close()
						state, e = g.load(fc.Guard)
						if e != nil {
							t.Fatal(e)
						}
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, true); e != nil {
							t.Fatal(e)
						}
					case "retry-paused", "retry-manual":
						fake := &recoveryFake{target: fc.Guard.Target, state: "paused"}
						if scene == "retry-manual" {
							fake.state = "shut off"
						}
						if e := recoveryEngine(n, fake).step(context.Background(), g, state, true); e == nil {
							t.Fatal("expected non-normal close")
						}
					case "retry-emergency":
						recoveryAdvance(n, 14*time.Second)
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, false); e != nil {
							t.Fatal(e)
						}
					}
					loaded, e := g.load(fc.Guard)
					if e != nil {
						t.Fatal(e)
					}
					gb, e := os.ReadFile(g.path)
					if e != nil {
						t.Fatal(e)
					}
					a := assessGuard(gb, fc.Guard)
					before := captureFinal(fc, config)
					output, code := finalCLI(t, config)
					r := finalReadResult(t, fc)
					afterInputs := captureFinal(fc, config)
					if !reflect.DeepEqual(before, afterInputs) {
						t.Fatal("CLI modified inputs")
					}
					var prior AcceptanceResult
					if strictDecode(before.Data["observer/summary.json"], &prior, true) != nil || r.RuntimeAcceptance == nil || !reflect.DeepEqual(prior, *r.RuntimeAcceptance) {
						t.Fatal("runtime details changed")
					}
					if !reflect.DeepEqual(r.InputReferences, before.Refs) {
						t.Fatal("input references changed")
					}
					refs, _ := json.MarshalIndent(before.Refs, "", "  ")
					finalFixExport(t, scene+"-input-hashes.json", refs)
					if r.RuntimeAcceptance == nil || !r.RuntimeAcceptance.OverallPass || r.LiveProof {
						t.Fatalf("runtime control failed: %+v", r)
					}
					wantStatus, wantGuard, wantCount := "PASS", "NORMAL_COMPLETION", 10
					switch scene {
					case "retry-normal":
						wantStatus, wantGuard, wantCount = "PASS", "NORMAL_COMPLETION", 14
					case "interrupted-only":
						wantStatus, wantGuard, wantCount = "UNPROVEN", "INCOMPLETE", 5
					case "retry-paused":
						wantStatus, wantGuard, wantCount = "UNPROVEN", "INCOMPLETE", 13
					case "retry-emergency":
						wantStatus, wantGuard, wantCount = "FAIL", "EMERGENCY", 15
					case "retry-manual":
						wantStatus, wantGuard, wantCount = "UNPROVEN", "MANUAL", 14
					case "unconfirmed-then-running":
						wantStatus, wantGuard, wantCount = "INPUT_ERROR", "INVALID", 18
					case "shut-off-then-running":
						wantStatus, wantGuard, wantCount = "INPUT_ERROR", "INVALID", 22
					}
					if r.Status != wantStatus || a.Status != wantGuard || len(loaded.Events) != wantCount || ((code == 0) != (wantStatus == "PASS")) {
						t.Fatalf("unexpected: %s %s count=%d code=%d", r.Status, a.Status, len(loaded.Events), code)
					}
					if scene == "retry-normal" && loaded.Phase != "DISARMED" {
						t.Fatal("producer did not disarm")
					}
					after, _ := os.ReadFile(g.path)
					if !bytes.Equal(gb, after) {
						t.Fatal("CLI modified guard")
					}
					summary := map[string]any{"scene": scene, "firstStepError": stepErr, "producerLoadValid": true, "producerPhase": loaded.Phase, "events": len(loaded.Events), "consumer": a, "cliExit": code, "cliOutput": string(output), "overallStatus": r.Status, "runtimePass": r.RuntimeAcceptance.OverallPass, "liveProof": r.LiveProof, "guardSHA256": digestBytes(gb), "guardBytes": len(gb), "resumeCalls": mapped.resumes}
					data, _ := json.MarshalIndent(summary, "", "  ")
					finalFixExport(t, scene+"-result.json", data)
					finalFixExport(t, scene+"-guard.json", gb)
					t.Log(string(data))
					if scene == "normal" {
						finalBudgetCLI(t, fc)
					}
				}
			})
		})
	}
}

// Focused producer/consumer controls distinguish an interrupted read-only prefix
// from persisted explicit counterevidence. This test makes no network calls.
func TestFinalizeQueryPrefixes(t *testing.T) {
	for failAt := 2; failAt <= 10; failAt++ {
		t.Run(fmt.Sprintf("save-%d", failAt), func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			v.state = "running"
			injected := false
			g.beforeSave = func(next *guardState) error {
				if !injected && len(next.Events) == failAt {
					injected = true
					return errors.New("review interrupted query save")
				}
				return nil
			}
			if e := recoveryEngine(n, v).step(context.Background(), g, s, true); e == nil {
				t.Fatal("expected failure")
			}
			g.close()
			g, e := openGuard(c)
			if e != nil {
				t.Fatal(e)
			}
			defer g.close()
			s, e = g.load(c)
			if e != nil {
				t.Fatal(e)
			}
			prefix := len(s.Events)
			interrupted, _ := os.ReadFile(g.path)
			if assessGuard(interrupted, c).Status != "INCOMPLETE" {
				t.Fatal("interrupted prefix accepted")
			}
			finalFixExport(t, fmt.Sprintf("prefix-%d-interrupted-guard.json", failAt), interrupted)
			recoveryAdvance(n, time.Millisecond)
			if e = recoveryEngine(n, v).step(context.Background(), g, s, true); e != nil {
				t.Fatal(e)
			}
			if _, e = g.load(c); e != nil {
				t.Fatal(e)
			}
			b, e := os.ReadFile(g.path)
			if e != nil {
				t.Fatal(e)
			}
			a := assessGuard(b, c)
			want := "NORMAL_COMPLETION"
			if a.Status != want {
				t.Fatalf("unexpected %s", a.Status)
			}
			finalFixExport(t, fmt.Sprintf("prefix-%d-guard.json", failAt), b)
			t.Logf("failedSaveSequence=%d durablePrefix=%d finalEvents=%d producer=%s loadValid=true consumer=%s", failAt, prefix, len(s.Events), s.Phase, a.Status)
		})
	}
	for _, scene := range []string{"paused-then-running", "unconfirmed-uuid-manual-save-fails", "shut-off-manual-save-fails"} {
		t.Run(scene, func(t *testing.T) {
			c, g, s, v, n := recoverySetup(t)
			if scene != "paused-then-running" {
				g.beforeSave = func(next *guardState) error {
					if next.Events[len(next.Events)-1].Kind == "manual-review" {
						return errors.New("review interrupted manual save")
					}
					return nil
				}
				v.state = "shut off"
				v.wrongID = scene == "unconfirmed-uuid-manual-save-fails"
			}
			if e := recoveryEngine(n, v).step(context.Background(), g, s, true); e == nil {
				t.Fatal("expected failure")
			}
			g.close()
			g, e := openGuard(c)
			if e != nil {
				t.Fatal(e)
			}
			defer g.close()
			s, e = g.load(c)
			if e != nil {
				t.Fatal(e)
			}
			v.state = "running"
			v.wrongID = false
			recoveryAdvance(n, time.Millisecond)
			if e = recoveryEngine(n, v).step(context.Background(), g, s, true); e != nil {
				t.Fatal(e)
			}
			if _, e = g.load(c); e != nil {
				t.Fatal(e)
			}
			b, _ := os.ReadFile(g.path)
			a := assessGuard(b, c)
			want := "INVALID"
			if scene == "paused-then-running" {
				want = "NORMAL_COMPLETION"
			}
			if a.Status != want {
				t.Fatal(a)
			}
			finalFixExport(t, scene+"-guard.json", b)
			t.Logf("scene=%s producer=%s loadValid=true events=%d consumer=%s", scene, s.Phase, len(s.Events), a.Status)
		})
	}
}

// Deliberately manipulated negative fixtures, derived from a real normal close.
// None of these edited bytes is used as a positive control.
func TestFinalizeHistoryContradictions(t *testing.T) {
	c, g, s, v, n := recoverySetup(t)
	v.state = "running"
	if e := recoveryEngine(n, v).step(context.Background(), g, s, true); e != nil {
		t.Fatal(e)
	}
	for _, scene := range []string{"unknown-kind", "unknown-op", "actor", "sequence", "order", "spliced-pair", "final-paused", "wrong-uuid", "unconfirmed", "shut-off", "manual", "effect", "resume", "phase-only", "hard-tick", "heartbeat", "authority", "clock"} {
		t.Run(scene, func(t *testing.T) {
			bad := *s
			bad.Events = append([]recoveryEvent(nil), s.Events...)
			switch scene {
			case "unknown-kind":
				bad.Events[2].Kind = "unrecognized"
			case "unknown-op":
				bad.Events[2].Operation = "other"
			case "actor":
				bad.Events[2].Actor = "other"
			case "sequence":
				bad.Events[2].Sequence++
			case "order":
				bad.Events[2].Kind = "vm-started"
			case "spliced-pair":
				// A fresh UUID intent interrupts a prior complete UUID query,
				// followed directly by the old state command. This is no pair.
				bad.Events = append(append(append([]recoveryEvent(nil), s.Events[:5]...), s.Events[1]), s.Events[5:]...)
				for i := range bad.Events {
					bad.Events[i].Sequence = i + 1
				}
			case "final-paused":
				bad.Events[8].Detail = "paused"
			case "wrong-uuid":
				bad.Events[4].Detail = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			case "unconfirmed":
				bad.Events[4].Detail = "unconfirmed"
			case "shut-off":
				bad.Events[8].Detail = "shut off"
			case "manual":
				bad.Events[2].Kind = "manual-review"
			case "effect":
				bad.Events[2].Kind = "effect-confirmed"
			case "resume":
				bad.Events[2].Kind = "resume-intent"
			case "phase-only":
				bad.Events = bad.Events[:1]
			case "hard-tick":
				bad.LastTick = bad.HardTick
			case "heartbeat":
				bad.LastTick = bad.HeartbeatTick + bad.Config.HeartbeatMS*int64(time.Millisecond)
			case "authority":
				bad.Events[9].At = bad.Config.AuthorityExpires
			case "clock":
				bad.Events[9].At = bad.Events[9].At.Add(3 * time.Second)
			}
			b, _ := json.Marshal(bad)
			if a := assessGuard(b, c); a.Status == "NORMAL_COMPLETION" {
				t.Fatal("counterevidence accepted", scene, a)
			}
		})
	}
}
