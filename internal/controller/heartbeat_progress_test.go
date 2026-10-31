package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type heartbeatBarrier struct {
	path string
	done chan error
}
type heartbeatControllerBridge struct {
	t               *testing.T
	root, config    string
	sequence        int
	terminal        map[string]any
	terminalStop    func()
	terminalCleanup func()
}

func newHeartbeatControllerBridge(t *testing.T) *heartbeatControllerBridge {
	t.Helper()
	root := t.TempDir()
	cfg := startEnv(t).Config
	c := clientcmdapi.NewConfig()
	c.Clusters["local"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	c.AuthInfos["local"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	c.Contexts["local"] = &clientcmdapi.Context{Cluster: "local", AuthInfo: "local"}
	c.CurrentContext = "local"
	raw, err := clientcmd.Write(*c)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "local-kubeconfig")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return &heartbeatControllerBridge{t: t, root: root, config: file}
}
func (b *heartbeatControllerBridge) pulse(e *enforceFixture, f *exhibitionFixture, label string, advance func(), want bool) {
	b.t.Helper()
	b.sequence++
	e.now = e.now.Add(time.Second)
	requests := make(chan heartbeatBarrier)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || (r.URL.Path != "/advance" && r.URL.Path != "/authority" && r.URL.Path != "/stop" && r.URL.Path != "/cleanup") {
			http.Error(w, "invalid local barrier", 400)
			return
		}
		q := heartbeatBarrier{r.URL.Path, make(chan error, 1)}
		requests <- q
		if err := <-q.done; err != nil {
			http.Error(w, "local authority rejected", 409)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer server.Close()
	p := e.policy()
	// Synthetic fixture starts with an explicitly owned, open fence, as the
	// stored live target does; no implicit absence is treated as permission.
	if b.sequence == 1 {
		d := e.liveDeployment()
		putFence(d, &executionFence{Version: 1, PolicyUID: string(p.UID), PolicyName: p.Name, DeploymentUID: string(d.UID), ClosedThroughGeneration: p.Generation - 1})
		e.must(e.c.Update(e.ctx, d))
	}
	var envelope SignedExhibitionPermit
	e.must(json.Unmarshal([]byte(p.Annotations[ExhibitionPermitAnnotation]), &envelope))
	var grant ExecutionGrant
	e.must(json.Unmarshal([]byte(p.Annotations[ExecutionGrantAnnotation]), &grant))
	f.grant = grant
	spec := e.liveDeployment().Spec.DeepCopy()
	baseline, _, err := reproductionTemplates(f.permit.ApprovedReproduction)
	e.must(err)
	spec.Template = baseline
	rawPolicy := &unstructured.Unstructured{}
	rawPolicy.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("RecoveryPolicy"))
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(p), rawPolicy))
	data := map[string]any{"label": label, "scope": grant.Scope, "policySpec": rawPolicy.Object["spec"], "sourceDeploymentSpec": spec, "payload": f.permit, "envelope": envelope, "grant": grant, "binding": executionGrantBinding(p), "observedAt": e.now, "barrier": server.URL, "expectedSuccess": want, "sharedRoot": b.root}
	if b.terminal != nil {
		data["terminalFixture"] = b.terminal
	}
	raw, err := json.Marshal(data)
	e.must(err)
	input := filepath.Join(b.root, fmt.Sprintf("input-%d.json", b.sequence))
	e.must(os.WriteFile(input, raw, 0600))
	module := "tests.heartbeat_controller_envtest"
	if b.terminal != nil {
		module = "tests.terminal_handoff_envtest"
	}
	cmd := exec.Command("python3", "-m", module, b.config, filepath.Join(os.Getenv("KUBEBUILDER_ASSETS"), "kubectl"), input)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + b.root, "LANG=C", "LC_ALL=C", "PYTHONDONTWRITEBYTECODE=1"}
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := cmd.CombinedOutput(); done <- result{out, err} }()
	advanced := false
	for {
		select {
		case q := <-requests:
			if q.path == "/advance" {
				if advanced {
					q.done <- fmt.Errorf("duplicate barrier")
					continue
				}
				advanced = true
				advance()
				q.done <- nil
			} else if q.path == "/stop" && b.terminalStop != nil {
				b.terminalStop()
				q.done <- nil
			} else if q.path == "/cleanup" && b.terminalCleanup != nil {
				b.terminalCleanup()
				q.done <- nil
			} else if b.terminal != nil {
				if f.a.Instance() != f.grant.Instance {
					q.done <- fmt.Errorf("instance changed")
				} else {
					q.done <- nil
				}
			} else {
				q.done <- f.a.Authorize(e.policy(), nil)
			}
		case result := <-done:
			if result.err != nil {
				b.t.Fatalf("%s Python/controller bridge: %v\n%s", label, result.err, result.out)
			}
			b.t.Log(string(result.out))
			p = e.policy()
			e.must(json.Unmarshal([]byte(p.Annotations[ExecutionGrantAnnotation]), &f.grant))
			return
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			b.t.Fatal("local bridge deadline")
		}
	}
}

// Same startup fixture, same signed permit, same durable Python run custody.
// Kubelet/rollout statuses and Guard clock are synthetic; Controller reconcile,
// classification, crypto authority and Python TLS PATCH use the real local API.
func TestHeartbeatControllerStartupThroughFirstAction(t *testing.T) {
	bridge := newHeartbeatControllerBridge(t)
	hook := func(e *enforceFixture, f *exhibitionFixture, stage string, next func()) {
		if stage == "bound" {
			pod := &corev1.Pod{}
			e.must(e.c.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "new-reproduction-p2"}, pod))
			ready := pod.Status.DeepCopy()
			pod.Status.ContainerStatuses[0].Ready = false
			for n := range pod.Status.Conditions {
				pod.Status.Conditions[n].Status = corev1.ConditionFalse
			}
			e.must(e.c.Status().Update(e.ctx, pod))
			bridge.pulse(e, f, "identity-bound-not-ready", next, true)
			e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(pod), pod))
			pod.Status = *ready
			e.must(e.c.Status().Update(e.ctx, pod))
			bridge.pulse(e, f, "readiness-false-to-true", next, true)
			return
		}
		bridge.pulse(e, f, stage, next, true)
	}
	e, source, f := newReproductionEnforceDurationStages(t, true, true, hook)
	unreadyReproduction(e, source, f)
	bridge.pulse(e, f, "known-unready-before-action", func() {}, true)
	e.reconcile()
	inc := e.onlyIncident()
	e.wantOutcomes(inc, recovery.OutcomePatched)
	if inc.Spec.Trigger.PodUID != string(source.UID) {
		t.Fatal("action source changed")
	}
	bridge.pulse(e, f, "same-incident-progress", func() { e.reconcile() }, true)
	var list v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &list, client.InNamespace(e.ns)))
	if len(list.Items) != 1 || len(e.onlyIncident().Status.Runtime.Attempts) != 1 {
		t.Fatal("extra incident/action")
	}
}

func TestHeartbeatControllerReadinessAndRevocationBlocksAction(t *testing.T) {
	bridge := newHeartbeatControllerBridge(t)
	hook := func(e *enforceFixture, f *exhibitionFixture, stage string, next func()) {
		if stage != "bound" {
			bridge.pulse(e, f, stage, next, true)
			return
		}
		pod := &corev1.Pod{}
		e.must(e.c.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "new-reproduction-p2"}, pod))
		ready := pod.Status.DeepCopy()
		pod.Status.ContainerStatuses[0].Ready = false
		for n := range pod.Status.Conditions {
			pod.Status.Conditions[n].Status = corev1.ConditionFalse
		}
		e.must(e.c.Status().Update(e.ctx, pod))
		bridge.pulse(e, f, "identity-before-revocation", next, true)
		e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(pod), pod))
		pod.Status = *ready
		e.must(e.c.Status().Update(e.ctx, pod))
		bridge.pulse(e, f, "same-readiness-change-with-permit-revoked", func() {
			next()
			p := e.policy()
			delete(p.Annotations, ExhibitionPermitAnnotation)
			e.must(e.c.Update(e.ctx, p))
		}, false)
	}
	e, source, f := newReproductionEnforceStages(t, true, hook)
	if f.a.Authorize(e.policy(), nil) == nil {
		t.Fatal("revoked permit remained authorized")
	}
	e.must(e.c.Get(e.ctx, client.ObjectKeyFromObject(source), source))
	source.Status.ContainerStatuses[0].Ready = false
	for n := range source.Status.Conditions {
		source.Status.Conditions[n].Status = corev1.ConditionFalse
		source.Status.Conditions[n].LastTransitionTime.Time = e.now.Add(-31 * time.Second)
	}
	e.must(e.c.Status().Update(e.ctx, source))
	e.reconcile()
	var list v1alpha1.RecoveryIncidentList
	e.must(e.c.List(e.ctx, &list, client.InNamespace(e.ns)))
	if len(list.Items) != 0 || e.liveDeployment().Spec.Template.Annotations[recovery.AttemptAnnotation] != "" {
		t.Fatal("revocation allowed subsequent action")
	}
}
