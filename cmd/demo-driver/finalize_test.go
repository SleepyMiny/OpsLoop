//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Uses only explicit test mapping, never product URI equivalence. All calls go
// to an in-memory test VM that is also used by the normal Finish flow.
type finalMappedVM struct {
	vm      *finishGateVM
	target  recoveryTarget
	resumes int
}

func (v *finalMappedVM) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
	if len(args) != 4 || args[0] != "--connect" || args[1] != v.target.URI || args[3] != v.target.UUID {
		return "", errors.New("test mapping mismatch")
	}
	mapped := append([]string(nil), args...)
	mapped[1] = "test:///opsloop-p2d"
	mapped[3] = finishTestUUID
	if args[2] == "resume" {
		v.resumes++
	}
	return v.vm.RunStarted(ctx, mapped, boundary)
}
func finalCLI(t *testing.T, config string) ([]byte, int) {
	t.Helper()
	cli := os.Getenv("P2F_CLI")
	if cli == "" {
		t.Fatal("P2F_CLI must point to a freshly built production CLI")
	}
	cmd := exec.Command(cli, "finalize-run", "-config", config)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "KUBECONFIG=" + filepath.Join(t.TempDir(), "NO_CONFIG"), "LANG=C"}
	b, e := cmd.CombinedOutput()
	code := 0
	if e != nil {
		var exit *exec.ExitError
		if !errors.As(e, &exit) {
			t.Fatal(e)
		}
		code = exit.ExitCode()
	}
	return b, code
}
func finalReadResult(t *testing.T, c FinalizeConfig) RunResult {
	t.Helper()
	var r RunResult
	if e := readJSON(filepath.Join(c.OutputDir, "run-result.json"), &r); e != nil {
		t.Fatal(e)
	}
	return r
}
func finalPrepare(t *testing.T, oc ObserverConfig) (FinalizeConfig, *guardStore, *guardState, *guardMoment) {
	t.Helper()
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	owner, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	n.Wall = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	os.Chmod(root, 0700)
	c := guardConfig{Version: "opsloop-guard/v1", Mode: "synthetic-local", Run: oc.RunID, Epoch: "p2f-epoch", Target: recoveryTarget{"qemu:///system", finishTestUUID, "/synthetic/not-executed", strings.Repeat("0", 64)}, Allowed: "query-resume-only", Root: root, Owner: owner, HardDeadline: n.Wall.Add(100 * time.Second), AuthorityExpires: n.Wall.Add(110 * time.Second), MaxTrialMS: 100000, HeartbeatMS: 100000, TimeoutMS: 50}
	if e = c.validate(); e != nil {
		t.Fatal(e)
	}
	g, e := openGuard(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(g.close)
	// Config is fixed BEFORE Guard arm and BEFORE collection. Expected values never
	// come from a terminal state or completed Runtime evidence.
	original := baseInput(t).Incident
	fc := FinalizeConfig{Version: "opsloop-finalize/v1", RunID: oc.RunID, EvidenceKind: oc.EvidenceKind, Guard: c, GuardBinding: c.binding(), Identity: FinalizeIdentity{Namespace: oc.Namespace, Policy: oc.Policy, PolicyUID: original.Spec.PolicyRef.UID, Deployment: oc.Workload, DeploymentUID: "w", Service: ServiceIdentity{Namespace: oc.Namespace, Name: oc.Service, UID: "service-uid", PathID: oc.PathID, URL: oc.ServiceURL}}, Mapping: FinalizeMapping{NodeUID: "n0", NodeName: "sarmubuntuserver", GuardURI: c.Target.URI, GuardUUID: c.Target.UUID, FinishURI: "test:///opsloop-p2d", FinishUUID: finishTestUUID, Basis: "EXPLICIT_SYNTHETIC_TEST_MAPPING; in-memory shared VM; registration unverified"}, ObserverDir: filepath.Join(oc.OutputRoot, oc.RunID), GuardFile: g.path, OutputDir: finalTestOutput(t, "result"), ExpectNode: oc.ExpectNode, ExpectVersion: oc.ExpectVersion, PlatformDigests: oc.PlatformDigests}
	if e = fc.validate(); e != nil {
		t.Fatal(e)
	}
	observerTestJSON(t, filepath.Join(root, "finalize-config.json"), fc)
	state, e := guardArm(g, c, n)
	if e != nil {
		t.Fatal(e)
	}
	return fc, g, state, &n
}

func TestFinalizeCollectedCLI(t *testing.T) {
	scenes := []struct{ name, runtime, guard, status string }{
		{"normal", "normal", "normal", "PASS"},
		{"normal-bounded-wall-skew", "normal", "normal-skew", "PASS"},
		{"emergency-running", "normal", "running", "FAIL"},
		{"emergency-resume-after-stable", "normal", "resume", "FAIL"},
		{"guard-normal-runtime-unproven", "no-clock-witness", "normal", "UNPROVEN"},
		{"guard-normal-runtime-fail", "early-restore", "normal", "FAIL"},
		{"guard-armed", "normal", "armed", "UNPROVEN"},
		{"guard-restoring", "normal", "restoring", "FAIL"},
		{"guard-manual", "normal", "manual", "UNPROVEN"},
	}
	for _, scene := range scenes {
		t.Run(scene.name, func(t *testing.T) {
			finishObserverRuntime(t, scene.runtime, func(oc ObserverConfig) func(*finishGateVM) {
				fc, g, state, n := finalPrepare(t, oc)
				config := filepath.Join(fc.Guard.Root, "finalize-config.json")
				return func(vm *finishGateVM) {
					before := captureFinal(fc, config)
					mapped := &finalMappedVM{vm: vm, target: fc.Guard.Target}
					switch scene.guard {
					case "normal-skew":
						recoveryAdvance(n, 99*time.Second)
						n.Wall = n.Wall.Add(1500 * time.Millisecond)
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, true); e != nil {
							t.Fatal(e)
						}
					case "normal":
						recoveryAdvance(n, 86*time.Second)
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, true); e != nil {
							t.Fatal(e)
						}
					case "running":
						recoveryAdvance(n, 101*time.Second)
						if e := recoveryEngine(n, mapped).step(context.Background(), g, state, false); e != nil {
							t.Fatal(e)
						}
						if mapped.resumes != 0 {
							t.Fatal("existing running caused resume")
						}
					case "resume":
						recoveryAdvance(n, 101*time.Second)
						fake := &recoveryFake{target: fc.Guard.Target, state: "paused"}
						if e := recoveryEngine(n, fake).step(context.Background(), g, state, false); e != nil {
							t.Fatal(e)
						}
						if fake.resumes != 1 {
							t.Fatal("emergency resume missing")
						}
					case "restoring":
						recoveryAdvance(n, 101*time.Second)
						if e := g.emergency(state, *n, "synthetic interrupted restoration"); e != nil {
							t.Fatal(e)
						}
					case "manual":
						recoveryAdvance(n, 86*time.Second)
						fake := &recoveryFake{target: fc.Guard.Target, state: "shut off"}
						if e := recoveryEngine(n, fake).step(context.Background(), g, state, true); e == nil {
							t.Fatal("manual expected")
						}
					}
					guardBefore, _ := os.ReadFile(g.path)
					output, code := finalCLI(t, config)
					t.Logf("finalizer exit=%d output=%s", code, output)
					r := finalReadResult(t, fc)
					wantCode := 0
					if scene.status != "PASS" {
						wantCode = 1
					}
					if code != wantCode || r.Status != scene.status || r.OverallPass != (scene.status == "PASS") || r.LiveProof {
						t.Fatalf("status=%s code=%d output=%s reasons=%v", r.Status, code, output, r.Reasons)
					}
					if r.RuntimeAcceptance == nil {
						t.Fatal("runtime details lost")
					}
					var prior AcceptanceResult
					strictDecode(before.Data["observer/summary.json"], &prior, true)
					if !reflect.DeepEqual(*r.RuntimeAcceptance, prior) {
						t.Fatal("runtime details altered")
					}
					after := captureFinal(fc, config)
					for role, b := range before.Data {
						if role != "guard" && !bytes.Equal(b, after.Data[role]) {
							t.Fatal("input changed", role)
						}
					}
					guardAfter, _ := os.ReadFile(g.path)
					if !bytes.Equal(guardBefore, guardAfter) || state.AutomaticSuccessEligible || state.ConsumerIntegration != "UNINTEGRATED" {
						t.Fatal("Guard input changed")
					}
					t.Logf("CLI status=%s exit=%d runtimePass=%t guard=%s resume=%d reasons=%v", r.Status, code, r.RuntimeAcceptance.OverallPass, r.GuardAssessment.Status, mapped.resumes, r.Reasons)
					finalExport(t, scene.name, fc, r, output, code)
					if scene.name == "normal" {
						finalBoundaries(t, fc, config)
					}
				}
			})
		})
	}
}

func finalExport(t *testing.T, name string, c FinalizeConfig, r RunResult, output []byte, code int) {
	t.Helper()
	root := os.Getenv("P2F_EXAMPLES")
	if root == "" {
		return
	}
	// Core samples only. Large raw histories remain referenced by hash; regenerate
	// complete input using this integration test, never edit terminal evidence.
	if name != "normal" && name != "emergency-running" && name != "guard-normal-runtime-unproven" {
		return
	}
	dir := filepath.Join(root, name)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"evidence.json", "summary.json", "receipts.json", "probe.jsonl", "incident.json", "pods.json", "env.json"} {
		b, e := os.ReadFile(filepath.Join(c.ObserverDir, name))
		if e == nil {
			os.WriteFile(filepath.Join(dir, name), b, 0600)
		}
	}
	b, _ := os.ReadFile(c.GuardFile)
	os.WriteFile(filepath.Join(dir, "guard-state.json"), b, 0600)
	observerTestJSON(t, filepath.Join(dir, "integration-config.json"), c)
	observerTestJSON(t, filepath.Join(dir, "run-result.json"), r)
	observerTestJSON(t, filepath.Join(dir, "cli.json"), map[string]any{"argv": []string{"demo-driver", "finalize-run", "-config", filepath.Join(c.Guard.Root, "finalize-config.json")}, "exitCode": code, "output": string(output), "provenance": "production collector -> normal Finish + explicit synthetic witness -> real Guard transition code -> freshly built production finalizer CLI; only core files exported, full snapshot regenerated by tests"})
}

func finalBoundaries(t *testing.T, base FinalizeConfig, originalConfig string) {
	t.Helper()
	// Duplicate production CLI must fail and leave the completed output untouched.
	old, _ := os.ReadFile(filepath.Join(base.OutputDir, "run-result.json"))
	out, code := finalCLI(t, originalConfig)
	t.Logf("duplicate production CLI exit=%d output=%s", code, out)
	now, _ := os.ReadFile(filepath.Join(base.OutputDir, "run-result.json"))
	if code == 0 || !bytes.Equal(old, now) || !bytes.Contains(out, []byte("OUTPUT_EXISTS")) {
		t.Fatal("duplicate publication", code, string(out))
	}
	t.Run("negative-fixtures", func(t *testing.T) {
		for _, scene := range []string{"missing", "truncated", "wrong-run", "wrong-epoch", "wrong-target", "guard-binding", "phase-only", "resume-contradiction", "event-order", "raw-hash", "manifest-hash", "runtime-binding", "summary-forged", "symlink", "directory", "oversize", "duplicate-key", "unsupported-version", "receipt-emergency-contradiction", "mapping-mismatch", "missing-and-binding", "missing-runtime", "missing-cleanup", "missing-coverage"} {
			t.Run(scene, func(t *testing.T) {
				c := base
				c.OutputDir = finalTestOutput(t, "result")
				// A small fixture copy is test-only; the actual normal scene above is never edited.
				c.GuardFile = filepath.Join(t.TempDir(), "guard.json")
				gb, _ := os.ReadFile(base.GuardFile)
				os.WriteFile(c.GuardFile, gb, 0600)
				var state guardState
				strictDecode(gb, &state, true)
				switch scene {
				case "mapping-mismatch":
					c.Mapping.NodeUID = "unrelated-node"
				case "missing-and-binding":
					os.Remove(c.GuardFile)
					c.Identity.PolicyUID = "foreign-policy"
				case "missing-runtime":
					c.ObserverDir = filepath.Join(t.TempDir(), "unavailable")
				case "missing":
					os.Remove(c.GuardFile)
				case "truncated":
					os.WriteFile(c.GuardFile, []byte("{\"config\":"), 0600)
				case "wrong-run":
					state.Config.Run = "other-run"
				case "wrong-epoch":
					state.Config.Epoch = "other-epoch"
				case "wrong-target":
					state.Config.Target.UUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
				case "guard-binding":
					state.Binding = strings.Repeat("f", 64)
				case "phase-only":
					state.Events = state.Events[:1]
				case "resume-contradiction":
					state.ResumeIntent = true
					state.Events = append(state.Events, recoveryEvent{len(state.Events) + 1, "recovery-guard", "resume-intent", "resume", "", state.ArmWall})
				case "event-order":
					state.Events[1], state.Events[2] = state.Events[2], state.Events[1]
					for i := range state.Events {
						state.Events[i].Sequence = i + 1
					}
				case "symlink":
					os.Remove(c.GuardFile)
					os.Symlink(base.GuardFile, c.GuardFile)
				case "directory":
					os.Remove(c.GuardFile)
					os.Mkdir(c.GuardFile, 0700)
				case "oversize":
					os.WriteFile(c.GuardFile, bytes.Repeat([]byte(" "), (2<<20)+1), 0600)
				case "duplicate-key":
					os.WriteFile(c.GuardFile, append([]byte("{\"phase\":\"DISARMED\","), gb[1:]...), 0600)
				case "unsupported-version":
					state.Config.Version = "opsloop-guard/v2"
				case "receipt-emergency-contradiction":
					state.Emergency = true
					state.Phase = "EMERGENCY_RESTORE_CONFIRMED"
					state.Events = append(state.Events[:1], recoveryEvent{2, "recovery-guard", "EMERGENCY_RESTORE", "", "", state.ArmWall}, recoveryEvent{3, "recovery-guard", "effect-confirmed", "", "", state.ArmWall})
				case "raw-hash", "manifest-hash", "runtime-binding", "summary-forged", "missing-cleanup", "missing-coverage":
					c.ObserverDir = filepath.Join(t.TempDir(), "observer")
					os.Mkdir(c.ObserverDir, 0700)
					entries, _ := os.ReadDir(base.ObserverDir)
					for _, entry := range entries {
						b, _ := os.ReadFile(filepath.Join(base.ObserverDir, entry.Name()))
						os.WriteFile(filepath.Join(c.ObserverDir, entry.Name()), b, 0600)
					}
					name := "probe.jsonl"
					if scene == "manifest-hash" {
						name = "evidence.json"
					}
					if scene == "summary-forged" {
						name = "summary.json"
					}
					b, _ := os.ReadFile(filepath.Join(c.ObserverDir, name))
					b = append(b, ' ')
					if scene == "runtime-binding" || scene == "missing-cleanup" || scene == "missing-coverage" {
						name = "evidence.json"
						b, _ = os.ReadFile(filepath.Join(c.ObserverDir, name))
						var e RuntimeEvidence
						strictDecode(b, &e, true)
						if scene == "runtime-binding" {
							e.RunID = "foreign"
						}
						if scene == "missing-cleanup" {
							e.Cleanup = nil
						}
						if scene == "missing-coverage" {
							e.Coverage = nil
						}
						b, _ = json.Marshal(e)
					}
					if scene == "summary-forged" {
						var a AcceptanceResult
						strictDecode(b, &a, true)
						a.OverallPass = false
						b, _ = json.Marshal(a)
					}
					os.WriteFile(filepath.Join(c.ObserverDir, name), b, 0600)
					if scene == "runtime-binding" || scene == "summary-forged" || scene == "missing-cleanup" || scene == "missing-coverage" {
						finalRehash(t, c.ObserverDir, name, b)
					}
				}
				if slices.Contains([]string{"wrong-run", "wrong-epoch", "wrong-target", "guard-binding", "phase-only", "resume-contradiction", "event-order", "unsupported-version", "receipt-emergency-contradiction"}, scene) {
					observerTestJSON(t, c.GuardFile, state)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				observerTestJSON(t, path, c)
				out, code := finalCLI(t, path)
				r := finalReadResult(t, c)
				if code == 0 || r.OverallPass || r.LiveProof {
					t.Fatal("negative accepted", scene, string(out))
				}
				want := "INPUT_ERROR"
				if scene == "missing" || scene == "missing-runtime" {
					want = "UNPROVEN"
				}
				if scene == "resume-contradiction" || scene == "receipt-emergency-contradiction" {
					want = "FAIL"
				}
				if r.Status != want {
					t.Fatalf("%s want=%s got=%s %v", scene, want, r.Status, r.Reasons)
				}
				if scene == "missing-cleanup" && (r.RuntimeAcceptance == nil || r.RuntimeAcceptance.Cleanup.Status == "PASS") {
					t.Fatal("Guard filled cleanup gap")
				}
				if scene == "missing-coverage" && (r.RuntimeAcceptance == nil || r.RuntimeAcceptance.Attribution.Status == "PASS") {
					t.Fatal("Guard filled actor coverage gap")
				}
				t.Logf("manipulated negative=%s exit=%d status=%s guard=%s reasons=%v", scene, code, r.Status, r.GuardAssessment.Status, r.Reasons)
			})
		}
	})
	t.Run("snapshot-changes", func(t *testing.T) {
		for _, scene := range []string{"changed-bytes-settle", "same-bytes-replaced"} {
			t.Run(scene, func(t *testing.T) {
				c := base
				c.GuardFile = filepath.Join(t.TempDir(), "guard.json")
				gb, e := os.ReadFile(base.GuardFile)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(c.GuardFile, gb, 0600); e != nil {
					t.Fatal(e)
				}
				config := filepath.Join(t.TempDir(), "config.json")
				observerTestJSON(t, config, c)
				b, e := os.ReadFile(config)
				if e != nil {
					t.Fatal(e)
				}
				changes := 0
				s, ok := stableFinal(c, config, b, func(i int) {
					if i != 0 {
						t.Fatal("changed pair retried", i)
					}
					data := append([]byte(nil), gb...)
					if scene == "changed-bytes-settle" {
						data = append(data, ' ')
					}
					pending := c.GuardFile + ".replacement"
					if e := os.WriteFile(pending, data, 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.Rename(pending, c.GuardFile); e != nil {
						t.Fatal(e)
					}
					changes++ // Replacement completed between the two captures.
				})
				r := finalizeAssessment(c, s, ok)
				if changes != 1 || ok || r.OverallPass || r.Status != "INPUT_ERROR" || !slices.Contains(r.Reasons, "INPUT_CHANGED_DURING_FINALIZATION") {
					t.Fatal("observed replacement accepted", changes, r)
				}
				// Fresh explicit evaluation of now-stable input is allowed; the
				// failed invocation itself was never retried or published as PASS.
				s, ok = stableFinal(c, config, b, nil)
				r = finalizeAssessment(c, s, ok)
				if !ok || !r.OverallPass {
					t.Fatal("stable control failed", r)
				}
			})
		}
	})
	t.Run("production-cli-concurrent-input", func(t *testing.T) {
		c := base
		c.OutputDir = finalTestOutput(t, "result")
		dir := t.TempDir()
		c.GuardFile = filepath.Join(dir, "guard.json")
		// Every visible revision is incomplete. Thus the real CLI must refuse
		// even if its two reads happen between replacements of the same version.
		// Exact between-capture replacement is tested synchronously above;
		// a sleeping writer of valid whitespace variants cannot prove that seam.
		if e := os.WriteFile(c.GuardFile, []byte("{"), 0600); e != nil {
			t.Fatal(e)
		}
		config := filepath.Join(t.TempDir(), "config.json")
		observerTestJSON(t, config, c)
		stop := make(chan struct{})
		done := make(chan error, 1)
		defer func() {
			close(stop)
			if e := <-done; e != nil {
				t.Error("writer failed", e)
			}
		}()
		started := make(chan struct{})
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					done <- nil
					return
				default:
				}
				pending := filepath.Join(dir, "changing")
				b := []byte("{")
				if i%2 != 0 {
					b = []byte("{\n")
				}
				e := os.WriteFile(pending, b, 0600)
				if e == nil {
					e = os.Rename(pending, c.GuardFile)
				}
				if i == 0 {
					close(started)
				}
				if e != nil {
					done <- e
					return
				}
			}
		}()
		<-started
		output, code := finalCLI(t, config)
		r := finalReadResult(t, c)
		if code == 0 || r.OverallPass || r.LiveProof || r.Status != "INPUT_ERROR" {
			t.Fatal("incomplete concurrent input accepted", code, string(output), r.Reasons)
		}
		t.Logf("production CLI concurrent incomplete input exit=%d status=%s reasons=%v", code, r.Status, r.Reasons)
	})
	t.Run("output-occupied", func(t *testing.T) {
		c := base
		c.OutputDir = finalTestOutput(t, "occupied")
		os.Mkdir(c.OutputDir, 0700)
		os.WriteFile(filepath.Join(c.OutputDir, ".run-result.pending"), []byte("partial"), 0600)
		config := filepath.Join(t.TempDir(), "config.json")
		observerTestJSON(t, config, c)
		out, code := finalCLI(t, config)
		t.Logf("occupied output production CLI exit=%d output=%s", code, out)
		if code == 0 || !bytes.Contains(out, []byte("OUTPUT_EXISTS")) {
			t.Fatal("occupied output", string(out))
		}
	})
	t.Run("output-write-failure", func(t *testing.T) {
		c := base
		c.OutputDir = finalTestOutput(t, "result")
		config := filepath.Join(t.TempDir(), "config.json")
		observerTestJSON(t, config, c)
		// RLIMIT_FSIZE gives deterministic production CLI write failure with no root,
		// filesystem mounts, service or permission/environment inheritance.
		cli := os.Getenv("P2F_CLI")
		cmd := exec.Command("/bin/bash", "-c", "ulimit -f 0; exec \"$1\" finalize-run -config \"$2\"", "p2f-output-limit", cli, config)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "KUBECONFIG=/nonexistent"}
		out, e := cmd.CombinedOutput()
		t.Logf("output failure production CLI error=%v output=%s", e, out)
		if e == nil || bytes.Contains(out, []byte("status=PASS")) {
			t.Fatal("write failure accepted", string(out))
		}
		if _, e = os.Stat(filepath.Join(c.OutputDir, "run-result.json")); !os.IsNotExist(e) {
			t.Fatal("partial final output published")
		}
		// Retrying a failed reserved output cannot borrow a previously valid result.
		_, code := finalCLI(t, config)
		if code == 0 {
			t.Fatal("failed output reused")
		}
	})
}
func finalRehash(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	var m observerManifest
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	strictDecode(mb, &m, true)
	for i := range m.Files {
		if m.Files[i].Ref == name {
			m.Files[i].SHA256 = digestBytes(b)
			m.Files[i].Bytes = len(b)
		}
	}
	observerTestJSON(t, filepath.Join(dir, "manifest.json"), m)
	mb, _ = os.ReadFile(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "COMPLETE"), mb, 0600)
}

func finalTestOutput(t *testing.T, name string) string {
	t.Helper()
	parent := t.TempDir()
	if e := os.Chmod(parent, 0700); e != nil {
		t.Fatal(e)
	}
	return filepath.Join(parent, name)
}
