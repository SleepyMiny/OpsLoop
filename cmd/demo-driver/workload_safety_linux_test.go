//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"io"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type workloadScene struct {
	c       workloadSafetyConfig
	fixture *finishScene
	api     *finishClient
	file    string
}

func newWorkloadScene(t *testing.T, variant string) *workloadScene {
	t.Helper()
	f := newFinishScene(t, variant)
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	owner, e := guardProcess(os.Getpid(), n.Boot)
	if e != nil {
		t.Fatal(e)
	}
	period := meta.Duration{Duration: 30 * time.Second}
	f.p.Spec.Detection.PodUnreadyFor = &period
	f.d.Status.ReadyReplicas = 1
	f.d.Status.ObservedGeneration = f.d.Generation
	b, _ := json.Marshal(f.p.Spec)
	var spec map[string]any
	_ = json.Unmarshal(b, &spec)
	dir := t.TempDir()
	root := filepath.Join(dir, "guard")
	if e = os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	c := workloadSafetyConfig{Version: "opsloop-workload-safety/v1", Mode: "synthetic-local", EvidenceKind: "synthetic", RunID: "workload-local", Epoch: "epoch-1", APIOrigin: f.c.APIOrigin, KubeconfigFile: filepath.Join(dir, "kubeconfig"), ClusterUID: f.clusterUID, Namespace: f.c.Namespace, NamespaceUID: f.namespaceUID, Policy: f.p.Name, PolicyUID: string(f.p.UID), PolicyGeneration: 2, PolicySpec: spec, Workload: f.d.Name, DeploymentUID: string(f.d.UID), Root: root, Owner: owner, HardDeadline: n.Wall.Add(30 * time.Second), AuthorityExpires: n.Wall.Add(65 * time.Second), MaxTrialMS: 30000, HeartbeatMS: 5000, TimeoutMS: 1000}
	ca, e := os.ReadFile(f.c.APICAFile)
	if e != nil {
		t.Fatal(e)
	}
	k := clientapi.Config{CurrentContext: "explicit", Clusters: map[string]*clientapi.Cluster{"one": {Server: c.APIOrigin, CertificateAuthorityData: ca}}, AuthInfos: map[string]*clientapi.AuthInfo{"one": {Token: finishTestToken}}, Contexts: map[string]*clientapi.Context{"explicit": {Cluster: "one", AuthInfo: "one"}}}
	b, e = clientcmd.Write(k)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(c.KubeconfigFile, b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.validate(); e != nil {
		t.Fatal(e)
	}
	api, e := newWorkloadClient(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(api.client.CloseIdleConnections)
	file := filepath.Join(dir, "config.json")
	observerTestJSON(t, file, c)
	return &workloadScene{c, f, api, file}
}
func (s *workloadScene) arm(t *testing.T) *workloadState {
	t.Helper()
	s.fixture.mu.Lock()
	s.fixture.p.Spec.Mode = v1.PolicyModeObserveOnly
	s.fixture.mu.Unlock()
	store, e := openWorkloadStore(s.c)
	if e != nil {
		t.Fatal(e)
	}
	defer store.close()
	n, _ := guardNow()
	state, e := armWorkload(s.c, store, s.api, n)
	if e != nil {
		t.Fatal(e)
	}
	return state
}
func (s *workloadScene) enforce() {
	s.fixture.mu.Lock()
	defer s.fixture.mu.Unlock()
	s.fixture.p.Spec.Mode = v1.PolicyModeEnforce
	s.fixture.p.Generation = 2
	s.fixture.p.ResourceVersion = "enforce-rv"
}
func (s *workloadScene) stop(t *testing.T) (*workloadState, error) {
	t.Helper()
	store, e := openWorkloadStore(s.c)
	if e != nil {
		t.Fatal(e)
	}
	defer store.close()
	state, e := store.load(s.c)
	if e != nil {
		t.Fatal(e)
	}
	e = stopWorkload(s.c, store, state, s.api, "TEST_FINISH")
	return state, e
}
func TestWorkloadSafetyFiniteStopAndNoDataPlaneClaim(t *testing.T) {
	for _, variant := range []string{"normal", "lost", "no-ack", "stale", "active", "fence", "409"} {
		t.Run(variant, func(t *testing.T) {
			s := newWorkloadScene(t, variant)
			s.arm(t)
			s.enforce()
			state, e := s.stop(t)
			if variant == "lost" || variant == "409" {
				if e == nil {
					t.Fatal("lost/conflict was treated as confirmed")
				}
				state, e = s.stop(t)
			}
			want := variant == "normal" || variant == "lost" || variant == "409"
			if (e == nil) != want || state.closed() != want {
				t.Fatalf("variant=%s err=%v state=%+v", variant, e, state)
			}
			if want && state.Proof == nil {
				t.Fatal("missing strong proof")
			}
			patches := 1
			if variant == "409" {
				patches = 2
			}
			if s.fixture.patches != patches {
				t.Fatal("unsafe mutation replay", s.fixture.patches)
			}
			if len(s.fixture.vm.calls) != 0 || s.fixture.serviceCalls != 0 {
				t.Fatal("workload guard touched VM/Service")
			}
			if variant == "lost" && state.StopResponseConfirmed {
				t.Fatal("lost response became actor confirmation")
			}
			var out bytes.Buffer
			if e = workloadOutput(&out, s.c, state); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(out.String(), `"overallPass":false`) || !strings.Contains(out.String(), `"serviceChecked":false`) || strings.Contains(out.String(), finishTestToken) {
				t.Fatal("invalid proof/credential claim")
			}
		})
	}
}
func TestWorkloadSafetyUnknownWriteNeverReplays(t *testing.T) {
	s := newWorkloadScene(t, "before-timeout")
	s.c.TimeoutMS = 50
	s.api.client.Timeout = 50 * time.Millisecond
	s.arm(t)
	s.enforce()
	_, e := s.stop(t)
	if e == nil {
		t.Fatal("timeout passed")
	}
	_, e = s.stop(t)
	if e == nil || s.fixture.patches != 1 {
		t.Fatal("unknown write replayed", e, s.fixture.patches)
	}
}
func TestWorkloadSafetyRejectsForeignIdentitySpecAndFence(t *testing.T) {
	for _, kind := range []string{"cluster", "namespace", "policy", "spec", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			s := newWorkloadScene(t, "normal")
			s.arm(t)
			s.enforce()
			s.fixture.mu.Lock()
			switch kind {
			case "cluster":
				s.fixture.clusterUID = "foreign"
			case "namespace":
				s.fixture.namespaceUID = "foreign"
			case "policy":
				s.fixture.p.UID = "foreign"
			case "spec":
				s.fixture.p.Spec.Detection.RestartThreshold++
			case "deployment":
				s.fixture.d.UID = "foreign"
			}
			s.fixture.mu.Unlock()
			state, e := s.stop(t)
			if e == nil || state.Phase == "STOP_CONFIRMED" {
				t.Fatal("foreign target accepted")
			}
			if kind != "deployment" && s.fixture.patches != 0 {
				t.Fatal("foreign Policy/cluster was mutated")
			}
			if kind == "deployment" && s.fixture.patches != 1 {
				t.Fatal("owned Policy stop improperly depended on changed Deployment")
			}
		})
	}
}
func TestWorkloadSafetyDurableDeadlineCannotRearmOrChange(t *testing.T) {
	s := newWorkloadScene(t, "normal")
	original := s.arm(t)
	store, e := openWorkloadStore(s.c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = armWorkload(s.c, store, s.api, mustWorkloadNow(t)); e == nil {
		t.Fatal("rearm extended trial")
	}
	store.close()
	c := s.c
	c.HardDeadline = c.HardDeadline.Add(time.Second)
	store, e = openWorkloadStore(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = store.load(c); e == nil {
		t.Fatal("changed config adopted")
	}
	store.close()
	n := mustWorkloadNow(t)
	n.Tick = original.ExpiryTick
	if original.permit(s.c, n) == nil {
		t.Fatal("expiry admitted")
	}
	n = mustWorkloadNow(t)
	n.Boot = "other"
	if original.permit(s.c, n) == nil {
		t.Fatal("boot mismatch admitted")
	}
}
func mustWorkloadNow(t *testing.T) guardMoment {
	t.Helper()
	n, e := guardNow()
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func TestWorkloadSafetyClientHasNoFallbackAndOnlyOwnedStopPath(t *testing.T) {
	s := newWorkloadScene(t, "normal")
	raw, e := os.ReadFile(s.c.KubeconfigFile)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"exec", "tokenFile", "proxy", "insecure", "impersonate"} {
		t.Run(kind, func(t *testing.T) {
			k, e := clientcmd.Load(raw)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "exec":
				k.AuthInfos["one"].Exec = &clientapi.ExecConfig{Command: "never-run"}
			case "tokenFile":
				k.AuthInfos["one"].TokenFile = "/must-not-read"
			case "proxy":
				k.Clusters["one"].ProxyURL = "http://must-not-dial"
			case "insecure":
				k.Clusters["one"].InsecureSkipTLSVerify = true
			case "impersonate":
				k.AuthInfos["one"].Impersonate = "other"
			}
			b, _ := clientcmd.Write(*k)
			if e = os.WriteFile(s.c.KubeconfigFile, b, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = newWorkloadClient(s.c); e == nil {
				t.Fatal("fallback accepted")
			}
		})
	}
	for _, path := range []string{s.c.path("deployment"), s.c.path("policy") + "/status", "/api/v1/nodes/other"} {
		_, _, problem := s.api.request(context.Background(), path, []byte(`[]`))
		if problem != "write-boundary" {
			t.Fatal("write escaped", path)
		}
	}
	if s.fixture.patches != 0 {
		t.Fatal("escaped requests reached API")
	}
}
func TestWorkloadSafetyPlanDoesNotReadCredentialsOrAuthorize(t *testing.T) {
	s := newWorkloadScene(t, "normal")
	s.c.KubeconfigFile = "/nonexistent-never-read"
	s.c.Mode = "hybrid-live"
	s.c.EvidenceKind = "observed"
	s.c.AuthorizationFile = "/never-authorize-plan"
	s.c.AuthorizationSHA256 = strings.Repeat("1", 64)
	observerTestJSON(t, s.file, s.c)
	var out bytes.Buffer
	if e := runWorkloadGuard([]string{"plan", "-config", s.file}, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"credentialReads":0`) || !strings.Contains(out.String(), `"APIRequests":0`) {
		t.Fatal(out.String())
	}
	if e := runWorkloadGuard([]string{"arm", "-config", s.file, "-execute", "-confirm", s.c.binding()}, io.Discard); e == nil {
		t.Fatal("missing separate authorization admitted")
	}
}
func TestWorkloadSafetySeparateExecutableWatch(t *testing.T) {
	cli := os.Getenv("WORKLOAD_SAFETY_CLI")
	if cli == "" {
		cli = filepath.Join(t.TempDir(), "demo-driver")
		command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-buildvcs=false", "-o", cli, ".")
		command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("build current CLI: %v %s", e, out)
		}
	}
	for _, reason := range []string{"OWNER_EXITED", "HARD_DEADLINE", "HEARTBEAT_EXPIRED", "RESTART_ORIGINAL_DEADLINE"} {
		t.Run(reason, func(t *testing.T) {
			s := newWorkloadScene(t, "normal")
			ownerCmd := exec.Command("/usr/bin/sleep", "30")
			if e := ownerCmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = ownerCmd.Process.Kill(); _ = ownerCmd.Wait() }()
			owner, e := guardProcess(ownerCmd.Process.Pid, mustWorkloadNow(t).Boot)
			if e != nil {
				t.Fatal(e)
			}
			s.c.Owner = owner
			if reason == "HARD_DEADLINE" || reason == "RESTART_ORIGINAL_DEADLINE" {
				now := mustWorkloadNow(t)
				s.c.MaxTrialMS = 2000
				s.c.HardDeadline = now.Wall.Add(2 * time.Second)
				s.c.AuthorityExpires = now.Wall.Add(32 * time.Second)
				s.c.HeartbeatMS = 2000
			}
			if reason == "HEARTBEAT_EXPIRED" {
				s.c.HeartbeatMS = 700
			}
			observerTestJSON(t, s.file, s.c)
			armed := s.arm(t)
			s.enforce()
			start := func() (*exec.Cmd, *bytes.Buffer) {
				out := &bytes.Buffer{}
				cmd := exec.Command(cli, "guard-workload", "watch", "-config", s.file, "-execute", "-confirm", s.c.binding())
				cmd.Stdout = out
				cmd.Stderr = out
				if e := cmd.Start(); e != nil {
					t.Fatal(e)
				}
				return cmd, out
			}
			cmd, out := start()
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				store, e := openWorkloadStore(s.c)
				ready := false
				if e == nil {
					state, err := store.load(s.c)
					if err != nil {
						store.close()
						t.Fatal(err)
					}
					ready = state.WatchTick > 0
					store.close()
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child watch never armed")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if reason == "OWNER_EXITED" {
				if e = ownerCmd.Process.Kill(); e != nil {
					t.Fatal(e)
				}
			}
			if reason == "RESTART_ORIGINAL_DEADLINE" {
				if e = cmd.Process.Kill(); e != nil {
					t.Fatal(e)
				}
				_ = cmd.Wait()
				cmd, out = start()
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case e = <-done:
				if e != nil {
					t.Fatalf("child failed: %v %s", e, out.String())
				}
			case <-time.After(7 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("independent child did not enforce original stop horizon")
			}
			store, e := openWorkloadStore(s.c)
			if e != nil {
				t.Fatal(e)
			}
			state, e := store.load(s.c)
			store.close()
			if e != nil {
				t.Fatal(e)
			}
			want := reason
			if reason == "RESTART_ORIGINAL_DEADLINE" {
				want = "HARD_DEADLINE"
			}
			if state.Phase != "STOP_CONFIRMED" || state.Reason != want || state.HardTick != armed.HardTick || state.ArmTick != armed.ArmTick || state.StopDispatchCount != 1 {
				t.Fatalf("incorrect independent stop: %+v", state)
			}
			if strings.Contains(out.String(), finishTestToken) || !strings.Contains(out.String(), `"overallPass":false`) {
				t.Fatal("credential or unsupported recovery claim")
			}
		})
	}
}

func TestWorkloadSafetyRejectsInsufficientStopAuthorityGrace(t *testing.T) {
	s := newWorkloadScene(t, "normal")
	for _, grace := range []time.Duration{time.Nanosecond, 250 * time.Millisecond, time.Second, 16 * time.Second} {
		t.Run(grace.String(), func(t *testing.T) {
			c := s.c
			c.AuthorityExpires = c.HardDeadline.Add(grace)
			if c.validate() == nil {
				t.Fatal("configuration cannot leave time for normal stop dispatch")
			}
		})
	}
	for _, grace := range []time.Duration{17 * time.Second, time.Minute} {
		c := s.c
		c.AuthorityExpires = c.HardDeadline.Add(grace)
		if e := c.validate(); e != nil {
			t.Fatal(e)
		}
	}
}
