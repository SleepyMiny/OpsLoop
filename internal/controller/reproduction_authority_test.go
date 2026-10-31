package controller

import (
	"encoding/json"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture only: independently signs synthetic v3 roles, never operating authority.
func promoteReproductionPermit(p *ExhibitionPermit, baseline corev1.PodTemplateSpec) {
	r := p.CircuitReplay
	p.Schema = "opsloop-exhibition-permit/v3"
	p.CircuitEvidence = &CircuitEvidence{WorkloadImage: r.WorkloadImage, Signature: r.Signature, PriorRunID: r.PriorRunID, PriorEpoch: r.PriorEpoch, PriorIncidentUID: r.PriorIncidentUID, IncidentSHA256: r.IncidentSHA256, BundleSHA256: r.BundleSHA256, CompletionSHA256: r.CompletionSHA256, PolicyUID: p.Grant.Scope.PolicyUID, DeploymentUID: p.Grant.Scope.DeploymentUID}
	p.PreviousCompletionSHA256 = strings.Repeat("f", 64)
	p.PredecessorCompletion = &PredecessorCompletion{RunID: "failed-previous", Epoch: "failed-epoch", PolicyUID: p.Grant.Scope.PolicyUID, DeploymentUID: p.Grant.Scope.DeploymentUID, CompletionSHA256: p.PreviousCompletionSHA256, RetirementSHA256: strings.Repeat("d", 64)}
	v := &ApprovedReproduction{Schema: "opsloop-approved-reproduction/v1", Kind: "DEMO_B_EMPTY_ORDER_READINESS", PolicyUID: p.Grant.Scope.PolicyUID, DeploymentUID: p.Grant.Scope.DeploymentUID, WorkloadImage: r.WorkloadImage, ContainerName: baseline.Spec.Containers[0].Name}
	v.Input.Name = "DEMO_B_REPLAY"
	v.Input.Value = "empty-order-after-30s"
	v.Classification.Class = classification.ClassApplication
	v.Classification.Reason = classification.ReasonPodNotReady
	v.Classification.RuleVersion = classification.ReadinessRuleVersion
	v.BaselineTemplate, _ = json.Marshal(baseline)
	fault := baseline.DeepCopy()
	for i := range fault.Spec.Containers[0].Env {
		if fault.Spec.Containers[0].Env[i].Name == v.Input.Name {
			fault.Spec.Containers[0].Env[i].Value = v.Input.Value
		}
	}
	v.Template, _ = json.Marshal(fault)
	p.ApprovedReproduction = v
	p.NewExecutionAuthorization = &NewExecutionAuthorization{AuthorizationID: p.AuthorizationID, ApprovalSHA256: r.ApprovalSHA256, CircuitEvidenceSHA256: evidenceProjectionHash(p.CircuitEvidence), PredecessorCompletionSHA256: evidenceProjectionHash(p.PredecessorCompletion), ApprovedReproductionSHA256: evidenceProjectionHash(v)}
}

type reproductionFixture struct {
	a      *ExhibitionAuthority
	p      *v1alpha1.RecoveryPolicy
	permit ExhibitionPermit
	d      *appsv1.Deployment
	pod    *corev1.Pod
	rs     *appsv1.ReplicaSet
	node   *corev1.Node
	now    time.Time
}

func newPythonReproduction(t *testing.T) *reproductionFixture {
	t.Helper()
	p := &v1alpha1.RecoveryPolicy{Spec: v1alpha1.RecoveryPolicySpec{Enabled: true, Mode: v1alpha1.PolicyModeEnforce, TargetRef: v1alpha1.TargetRef{Name: "engineering-demo"}}}
	p.Spec.Budget.MaxActionsPerIncident = 1
	code := `import json,sys,os
from pathlib import Path
from tests.test_exhibition_reproduction import V3Fixture
os.chmod(sys.argv[1],0o700)
f=V3Fixture(Path(sys.argv[1]),sys.argv[2]);p=f.payload();e=f.issue_v2(p)
print(json.dumps({'permit':p,'envelope':e,'public':f.public,'deployment':f.objects['b-deployment']}))`
	cmd := exec.Command("python3", "-c", code, t.TempDir(), v1alpha1.PolicyHash(p.Spec))
	cmd.Dir = filepath.Join("..", "..")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issuer %v %s", err, raw)
	}
	var got struct {
		Permit     ExhibitionPermit
		Envelope   SignedExhibitionPermit
		Public     string
		Deployment appsv1.Deployment
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	s := got.Permit.Grant.Scope
	a, err := NewExhibitionAuthority(ExhibitionAuthorityConfig{Schema: "opsloop-exhibition-authority/v1", PublicKey: got.Public, Targets: []ExhibitionTarget{{s.ClusterUID, s.NamespaceUID, s.Namespace, s.Policy, s.PolicyUID, s.Deployment, s.DeploymentUID}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &reproductionFixture{a: a, p: p, permit: got.Permit, d: &got.Deployment, now: got.Permit.Grant.NotBefore}
	a.instance = got.Permit.Grant.Instance
	a.sample = func() (authoritySample, error) { return authoritySample{f.now, 100 * time.Second}, nil }
	p.ObjectMeta = metav1.ObjectMeta{Name: s.Policy, Namespace: s.Namespace, UID: types.UID(s.PolicyUID), Generation: s.PolicyGeneration - 2}
	p.Status.IncidentSequence = got.Permit.BaseIncidentSequence
	p.Spec.Enabled = false
	p.Status.Conditions = []metav1.Condition{{Type: CondQuiesced, Status: metav1.ConditionTrue, Reason: "FenceClosed", ObservedGeneration: p.Generation}}
	a.ObserveStopped(p)
	p.Spec.Enabled = true
	p.Generation = s.PolicyGeneration
	envelope, _ := json.Marshal(got.Envelope)
	grant, _ := json.Marshal(ExecutionGrant{ExecutionGrantCore: got.Permit.Grant, HeartbeatAt: f.now, HeartbeatSequence: 1})
	p.Annotations = map[string]string{ExhibitionPermitAnnotation: string(envelope), ExecutionGrantAnnotation: string(grant)}
	if err = a.Authorize(p, nil); err != nil {
		t.Fatal(err)
	}
	_, fault, err := reproductionTemplates(got.Permit.ApprovedReproduction)
	if err != nil {
		t.Fatal(err)
	}
	f.d.Spec.Template = fault
	yes := true
	f.rs = &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "new-rs", Namespace: s.Namespace, UID: "new-rs-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: f.d.Name, UID: f.d.UID, Controller: &yes}}}, Spec: appsv1.ReplicaSetSpec{Template: fault}}
	f.node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ready-node", UID: "ready-node-uid"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	f.pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "current-p2", Namespace: s.Namespace, UID: "current-p2-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: f.rs.Name, UID: f.rs.UID, Controller: &yes}}}, Spec: *fault.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, ContainerID: "containerd://current-incarnation", ImageID: got.Permit.CircuitEvidence.WorkloadImage, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(f.now)}, {Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(f.now)}}}}
	f.pod.Spec.NodeName = f.node.Name
	return f
}
func (f *reproductionFixture) observe() error {
	return f.a.observeReproduction(f.p, f.d, []corev1.Pod{*f.pod}, []appsv1.ReplicaSet{*f.rs}, map[string]*corev1.Node{f.node.Name: f.node}, f.now)
}
func (f *reproductionFixture) trigger(t *testing.T) v1alpha1.Trigger {
	t.Helper()
	if err := f.observe(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(16 * time.Second)
	grant, _ := json.Marshal(ExecutionGrant{ExecutionGrantCore: f.permit.Grant, HeartbeatAt: f.now, HeartbeatSequence: 2})
	f.p.Annotations[ExecutionGrantAnnotation] = string(grant)
	f.pod.Status.ContainerStatuses[0].Ready = false
	for i := range f.pod.Status.Conditions {
		f.pod.Status.Conditions[i].Status = corev1.ConditionFalse
	}
	det := v1alpha1.DetectionSpec{PodUnreadyFor: &metav1.Duration{Duration: 15 * time.Second}}
	result := classification.Classify(classification.Input{Now: f.now, Detection: det, WorkloadUID: f.d.UID, ImageDigest: strings.Split(f.permit.CircuitEvidence.WorkloadImage, "@")[1], Pods: []corev1.Pod{*f.pod}, Nodes: map[string]*corev1.Node{f.node.Name: f.node}, HealthyPods: map[types.UID]bool{f.pod.UID: true}})
	if !result.Detected {
		t.Fatalf("not detected %+v", result)
	}
	if err := f.a.bindReproductionTrigger(f.p, result.Trigger, strings.Split(f.permit.CircuitEvidence.WorkloadImage, "@")[1]); err != nil {
		t.Fatal(err)
	}
	return result.Trigger
}
func (f *reproductionFixture) incident(t *testing.T, tr v1alpha1.Trigger) *v1alpha1.RecoveryIncident {
	t.Helper()
	s := f.permit.Grant.Scope
	f.p.Status.IncidentSequence++
	inc := &v1alpha1.RecoveryIncident{ObjectMeta: metav1.ObjectMeta{Name: s.Incident, Namespace: s.Namespace, UID: "current-incident", Annotations: map[string]string{ExecutionGrantBindingAnnotation: executionGrantBinding(f.p)}}, Spec: v1alpha1.RecoveryIncidentSpec{Sequence: f.p.Status.IncidentSequence, PolicyRef: v1alpha1.PolicyRef{Name: s.Policy, UID: s.PolicyUID}, PolicyGeneration: s.PolicyGeneration, PolicyHash: s.PolicyHash, Workload: v1alpha1.WorkloadRef{Name: s.Deployment, UID: s.DeploymentUID}, Trigger: tr}}
	f.p.Status.ActiveIncident = &v1alpha1.ActiveIncidentRef{Name: inc.Name, Sequence: inc.Spec.Sequence, IncidentUID: string(inc.UID)}
	if err := f.a.AnnotateIncident(f.p, inc); err != nil {
		t.Fatal(err)
	}
	return inc
}
func TestReproductionPythonIssuerClassifierCurrentOccurrence(t *testing.T) {
	f := newPythonReproduction(t)
	if f.a.AllowsCircuit(f.p, f.permit.CircuitEvidence.Signature, f.d) {
		t.Fatal("history became authority")
	}
	tr := f.trigger(t)
	if tr.Signature == f.permit.CircuitEvidence.Signature {
		t.Fatal("historical and current conflated")
	}
	if !f.a.AllowsCircuit(f.p, tr.Signature, f.d) {
		t.Fatal("current approved occurrence denied")
	}
	inc := f.incident(t, tr)
	if err := f.a.Authorize(f.p, inc); err != nil {
		t.Fatal(err)
	}
	if err := f.a.validateCurrentReproductionSource(inc, f.pod, f.rs, f.node); err != nil {
		t.Fatal(err)
	}
	inc.Annotations[CurrentOccurrenceAnnotation] = "{}"
	if f.a.Authorize(f.p, inc) == nil {
		t.Fatal("tampered occurrence accepted")
	}
}
func TestReproductionSourceMutationRejects(t *testing.T) {
	for _, kind := range []string{"pod", "rs-owner", "restart", "image-id", "container-name", "env", "node", "retired", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newPythonReproduction(t)
			tr := f.trigger(t)
			inc := f.incident(t, tr)
			switch kind {
			case "pod":
				f.pod.UID = "different"
			case "rs-owner":
				f.rs.UID = "different"
				f.pod.OwnerReferences[0].UID = f.rs.UID
			case "restart":
				f.pod.Status.ContainerStatuses[0].ContainerID = "other"
			case "image-id":
				f.pod.Status.ContainerStatuses[0].ImageID = "other"
			case "container-name":
				f.pod.Status.ContainerStatuses[0].Name = "other"
			case "env":
				f.pod.Spec.Containers[0].Env[0].Value = "other"
			case "node":
				f.node.UID = "other"
			case "retired":
				f.a.current.retired = true
			case "expired":
				f.now = f.permit.Grant.NotAfter.Add(time.Second)
			}
			if kind == "expired" {
				if f.a.Authorize(f.p, inc) == nil {
					t.Fatal("expired accepted")
				}
			} else if f.a.validateCurrentReproductionSource(inc, f.pod, f.rs, f.node) == nil {
				t.Fatal("source mutation accepted")
			}
		})
	}
}
func TestReproductionUnreadyIdentityDoesNotArmOrAllowReplacement(t *testing.T) {
	f := newPythonReproduction(t)
	f.pod.Status.ContainerStatuses[0].Ready = false
	if err := f.observe(); err != nil {
		t.Fatal(err)
	}
	if f.a.current.occurrence == nil || !f.a.current.occurrence.HealthyObservedAt.IsZero() {
		t.Fatal("unready identity must bind without manufacturing healthy evidence")
	}
	f.pod.Status.ContainerStatuses[0].Ready = true
	if err := f.observe(); err != nil {
		t.Fatal(err)
	}
	f.pod.UID = "replacement"
	if f.observe() == nil {
		t.Fatal("replacement rebound")
	}
}

func TestReproductionSignedContractMutations(t *testing.T) {
	for _, kind := range []string{"policy", "deployment", "image", "container", "input", "class", "env", "probe", "v1", "v2", "hash", "instance", "run", "grant-revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := newExhibitionFixture(t)
			d := f.replayDeployment()
			c := &d.Spec.Template.Spec.Containers[0]
			c.Env = []corev1.EnvVar{{Name: "DEMO_B_REPLAY"}}
			c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
			promoteReproductionPermit(&f.permit, d.Spec.Template)
			v := f.permit.ApprovedReproduction
			switch kind {
			case "policy":
				v.PolicyUID = "other"
			case "deployment":
				v.DeploymentUID = "other"
			case "image":
				v.WorkloadImage = "other"
			case "container":
				v.ContainerName = "other"
			case "input":
				v.Input.Value = "other"
			case "class":
				v.Classification.Reason = "HTTP_500"
			case "env", "probe":
				var x corev1.PodTemplateSpec
				_ = json.Unmarshal(v.Template, &x)
				if kind == "env" {
					x.Spec.Containers[0].Env = append(x.Spec.Containers[0].Env, corev1.EnvVar{Name: "unexpected"})
				} else {
					x.Spec.Containers[0].ReadinessProbe = nil
				}
				v.Template, _ = json.Marshal(x)
			case "v1", "v2":
				f.permit.Schema = "opsloop-exhibition-permit/" + kind
			case "hash":
				f.permit.NewExecutionAuthorization.ApprovedReproductionSHA256 = strings.Repeat("0", 64)
			case "instance":
				f.permit.Grant.Instance = "other"
			case "run":
				f.permit.Grant.Scope.RunID = "other"
			}
			// A signer cannot authorize a broadened recipe merely by recomputing its hash.
			if kind != "hash" {
				f.permit.NewExecutionAuthorization.ApprovedReproductionSHA256 = evidenceProjectionHash(v)
			}
			f.publish(t)
			if kind == "grant-revoked" {
				delete(f.p.Annotations, ExecutionGrantAnnotation)
			}
			if f.a.Authorize(f.p, nil) == nil {
				t.Fatal("invalid signed v3 recipe/binding accepted")
			}
		})
	}
}
func TestReproductionUnrelatedTriggerRejected(t *testing.T) {
	for _, mode := range []string{"http500", "nodefailure", "old-signature", "wrong-source", "no-healthy"} {
		t.Run(mode, func(t *testing.T) {
			f := newPythonReproduction(t)
			tr := f.trigger(t)
			switch mode {
			case "http500":
				tr.Reason = "HTTP_500"
			case "nodefailure":
				tr.Class = classification.ClassInfrastructure
			case "old-signature":
				tr.Signature = f.permit.CircuitEvidence.Signature
			case "wrong-source":
				tr.PodUID = "other"
			case "no-healthy":
				f.a.current.occurrence = nil
			}
			if f.a.bindReproductionTrigger(f.p, tr, strings.Split(f.permit.CircuitEvidence.WorkloadImage, "@")[1]) == nil {
				t.Fatal("unapproved occurrence accepted")
			}
		})
	}
}
