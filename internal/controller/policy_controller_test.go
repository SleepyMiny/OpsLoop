package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
)

const (
	policyName  = "demo-api"
	candidateIP = "10.0.0.5"
	testLabel   = "opsloop.io/test"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	c      client.Client
	ns     string
	now    time.Time
	r      *PolicyReconciler
	deploy *appsv1.Deployment
}

// newFixture builds one namespace with a Deployment whose single Pod runs on a Node NotReady for 30s.
// metricsIP selects which candidate IP the fake Prometheus reports healthy; empty returns no series.
func newFixture(t *testing.T, metricsIP string) *fixture {
	t.Helper()
	env := startEnv(t)
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(env.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: context.Background(), c: c, now: time.Now().UTC().Truncate(time.Second)}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ctl-"}}
	f.must(c.Create(f.ctx, ns))
	f.ns = ns.Name
	f.r = &PolicyReconciler{Client: c, APIReader: c, Now: func() time.Time { return f.now }, Prom: fakeProm(f, metricsIP)}

	primary := f.node("primary", corev1.ConditionFalse, "")

	one := int32(1)
	surge, unavail := intstr.FromInt32(1), intstr.FromInt32(0)
	labels := map[string]string{"app.kubernetes.io/name": "demo-api"}
	rl := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	podSpec := corev1.PodSpec{
		NodeSelector: map[string]string{hostnameLabel: primary.Name},
		Containers: []corev1.Container{{
			Name:      "demo-api",
			Image:     "demo-api@sha256:0000000000000000000000000000000000000000000000000000000000000001",
			Resources: corev1.ResourceRequirements{Requests: rl, Limits: rl},
		}},
	}
	revision := map[string]string{"deployment.kubernetes.io/revision": "1"}
	f.deploy = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: f.ns, Annotations: revision},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavail},
			},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec},
		},
	}
	f.must(c.Create(f.ctx, f.deploy))
	ctrlRef := func(obj metav1.Object, kind string) []metav1.OwnerReference {
		yes := true
		return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: obj.GetName(), UID: obj.GetUID(), Controller: &yes}}
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api-1", Namespace: f.ns, Annotations: revision, OwnerReferences: ctrlRef(f.deploy, "Deployment")},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: f.deploy.Spec.Template,
		},
	}
	f.must(c.Create(f.ctx, rs))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api-1-a", Namespace: f.ns, Labels: labels, OwnerReferences: ctrlRef(rs, "ReplicaSet")},
		Spec:       *podSpec.DeepCopy(),
	}
	pod.Spec.NodeName = primary.Name
	f.must(c.Create(f.ctx, pod))

	f.must(c.Create(f.ctx, f.newPolicy(policyName)))
	return f
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// node creates a cluster-scoped Node unique to this test; a non-empty ip makes it a selectable candidate.
func (f *fixture) node(suffix string, ready corev1.ConditionStatus, ip string) *corev1.Node {
	f.t.Helper()
	name := f.ns + "-" + suffix
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		hostnameLabel:        name,
		"kubernetes.io/os":   "linux",
		"kubernetes.io/arch": "amd64",
	}}}
	if ip != "" {
		n.Labels[testLabel] = f.ns
	}
	f.must(f.c.Create(f.ctx, n))
	if ready == corev1.ConditionTrue {
		// Admission adds the not-ready taint on create; a node lifecycle controller would remove it once Ready.
		n.Spec.Taints = nil
		f.must(f.c.Update(f.ctx, n))
	}
	n.Status.Conditions = []corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             ready,
		LastTransitionTime: metav1.NewTime(f.now.Add(-30 * time.Second)),
		LastHeartbeatTime:  metav1.NewTime(f.now.Add(-30 * time.Second)),
	}}
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("110")}
	n.Status.Capacity = n.Status.Allocatable
	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	}
	f.must(f.c.Status().Update(f.ctx, n))
	return n
}

// newPolicy sets every spec field because the typed client sends zero values instead of omitting them.
func (f *fixture) newPolicy(name string) *v1alpha1.RecoveryPolicy {
	return &v1alpha1.RecoveryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: v1alpha1.RecoveryPolicySpec{
			TargetRef:         v1alpha1.TargetRef{Name: "demo-api"},
			Enabled:           true,
			Mode:              v1alpha1.PolicyModeObserveOnly,
			CandidateSelector: metav1.LabelSelector{MatchLabels: map[string]string{testLabel: f.ns}},
			RequiredPlatforms: []string{"linux/amd64", "linux/arm64"},
			Detection: v1alpha1.DetectionSpec{
				NodeUnreadyFor:   metav1.Duration{Duration: 20 * time.Second},
				RestartThreshold: 3,
				RestartWindow:    metav1.Duration{Duration: 120 * time.Second},
			},
			Filter: v1alpha1.FilterSpec{
				MaxCPUUtilizationPercent:        85,
				MaxMemoryUtilizationPercent:     85,
				MinimumAvailableMemory:          resource.MustParse("512Mi"),
				StartupMemoryBudget:             resource.MustParse("256Mi"),
				RequestedCapacityReservePercent: 10,
				MaxSampleAge:                    metav1.Duration{Duration: 30 * time.Second},
			},
			Score: v1alpha1.ScoreSpec{
				CPUWeight:               30,
				MemoryWeight:            35,
				RequestsWeight:          35,
				RecentFailurePenalty:    15,
				MaxRecentFailurePenalty: 45,
				RecentFailureWindow:     metav1.Duration{Duration: 30 * time.Minute},
			},
			Budget: v1alpha1.BudgetSpec{
				MaxActionsPerIncident: 2,
				Cooldown:              metav1.Duration{Duration: 120 * time.Second},
				PlacementTimeout:      metav1.Duration{Duration: 90 * time.Second},
				VerificationTimeout:   metav1.Duration{Duration: 60 * time.Second},
				IncidentDeadline:      metav1.Duration{Duration: 10 * time.Minute},
			},
			Verification: v1alpha1.VerificationSpec{
				ServiceName:          "demo-api",
				Port:                 8080,
				ConsecutiveSuccesses: 5,
				StabilityWindow:      metav1.Duration{Duration: 30 * time.Second},
			},
		},
	}
}

// fakeProm answers every node-exporter query with one healthy series for ip, or with no series when ip is empty.
func fakeProm(f *fixture, ip string) promv1.API {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		ts := fmt.Sprintf("%.3f", float64(f.now.UnixMilli())/1000)
		var val string
		switch {
		case ip == "":
		case strings.HasPrefix(q, "1 - avg"):
			val = "0.2"
		case strings.HasPrefix(q, "min by (instance) (timestamp("), strings.HasPrefix(q, "timestamp("):
			val = ts
		case strings.HasPrefix(q, "min by (instance) (count_over_time("):
			val = "6"
		case strings.HasPrefix(q, "node_memory_MemAvailable_bytes"):
			val = "2147483648"
		case strings.HasPrefix(q, "node_memory_MemTotal_bytes"):
			val = "4294967296"
		case strings.HasPrefix(q, "up{"):
			val = "1"
		}
		result := ""
		if val != "" {
			result = fmt.Sprintf(`{"metric":{"instance":"%s:9100"},"value":[%s,"%s"]}`, ip, ts, val)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, result)
	}))
	f.t.Cleanup(srv.Close)
	c, err := promapi.NewClient(promapi.Config{Address: srv.URL})
	if err != nil {
		f.t.Fatal(err)
	}
	return promv1.NewAPI(c)
}

func (f *fixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: policyName}})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > monitorPeriod {
		f.t.Fatalf("RequeueAfter %v outside (0, %v]", res.RequeueAfter, monitorPeriod)
	}
	return res
}

func (f *fixture) policy() *v1alpha1.RecoveryPolicy {
	f.t.Helper()
	p := &v1alpha1.RecoveryPolicy{}
	f.must(f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: policyName}, p))
	return p
}

func (f *fixture) incidents() []v1alpha1.RecoveryIncident {
	f.t.Helper()
	var list v1alpha1.RecoveryIncidentList
	f.must(f.c.List(f.ctx, &list, client.InNamespace(f.ns)))
	return list.Items
}

func (f *fixture) onlyIncident() *v1alpha1.RecoveryIncident {
	f.t.Helper()
	incs := f.incidents()
	if len(incs) != 1 {
		f.t.Fatalf("want 1 incident, got %d", len(incs))
	}
	return &incs[0]
}

func (f *fixture) liveDeployment() *appsv1.Deployment {
	f.t.Helper()
	d := &appsv1.Deployment{}
	f.must(f.c.Get(f.ctx, client.ObjectKeyFromObject(f.deploy), d))
	return d
}

// reserveOnly leaves the state of a crash right after the reservation CAS and before Incident creation.
func (f *fixture) reserveOnly() *v1alpha1.RecoveryPolicy {
	f.t.Helper()
	p := f.policy()
	d := f.liveDeployment()
	res, digest, err := f.r.monitor(f.ctx, p, d, f.now)
	f.must(err)
	if !res.Detected {
		f.t.Fatal("fixture trigger not detected")
	}
	reservation(p, d, res.Trigger, digest, f.now)
	f.must(f.r.writePolicy(f.ctx, p))
	return p
}

func wantPhase(t *testing.T, inc *v1alpha1.RecoveryIncident, phase, reason string) {
	t.Helper()
	if rt := inc.Status.Runtime; rt.Phase != phase || rt.Reason != reason {
		t.Fatalf("incident %s phase/reason = %s/%s, want %s/%s", inc.Name, rt.Phase, rt.Reason, phase, reason)
	}
}

func wantCondition(t *testing.T, p *v1alpha1.RecoveryPolicy, reason string) {
	t.Helper()
	c := p.Status.Conditions
	if len(c) != 1 || c[0].Type != CondMonitoring || c[0].Reason != reason {
		t.Fatalf("conditions = %+v, want only %s with reason %s", c, CondMonitoring, reason)
	}
}

func TestE2ReserveCreateEvaluateInOneReconcile(t *testing.T) {
	f := newFixture(t, "")
	f.reconcile()

	inc := f.onlyIncident()
	p := f.policy()
	if prefix := fmt.Sprintf("%s-%s-", policyName, string(p.UID)[:8]); !strings.HasPrefix(inc.Name, prefix) {
		t.Fatalf("incident name %s lacks policy UID prefix %s", inc.Name, prefix)
	}
	if inc.Spec.Sequence != 1 || p.Status.IncidentSequence != 1 {
		t.Fatalf("sequence incident=%d policy=%d, want 1", inc.Spec.Sequence, p.Status.IncidentSequence)
	}
	if a := p.Status.ActiveIncident; a == nil || a.IncidentUID != string(inc.UID) {
		t.Fatalf("activeIncident = %+v, want incidentUID %s", a, inc.UID)
	}
	wantPhase(t, inc, PhaseWaiting, candidates.DecisionNoCandidate)
	if d := inc.Status.Runtime.Decision; d == nil || d.DecisionReason != candidates.DecisionNoCandidate {
		t.Fatalf("decision = %+v, want NO_CANDIDATE", d)
	}
	wantCondition(t, p, CondIncidentActive)
}

func TestE3ReservationResumesAfterCrash(t *testing.T) {
	t.Run("incident not created", func(t *testing.T) {
		f := newFixture(t, "")
		reserved := f.reserveOnly()
		for range 3 {
			f.reconcile()
		}
		inc := f.onlyIncident()
		p := f.policy()
		if inc.Name != reserved.Status.ActiveIncident.Name {
			t.Fatalf("incident %s, want reserved name %s", inc.Name, reserved.Status.ActiveIncident.Name)
		}
		if p.Status.IncidentSequence != 1 || p.Status.ActiveIncident.Sequence != 1 {
			t.Fatalf("sequence changed: %+v", p.Status)
		}
		if p.Status.ActiveIncident.IncidentUID != string(inc.UID) {
			t.Fatalf("incidentUID %q, want %s", p.Status.ActiveIncident.IncidentUID, inc.UID)
		}
	})
	t.Run("incident created with empty status", func(t *testing.T) {
		f := newFixture(t, "")
		p := f.reserveOnly()
		f.must(f.c.Create(f.ctx, incidentFor(p, f.liveDeployment())))
		f.reconcile()
		inc := f.onlyIncident()
		wantPhase(t, inc, PhaseWaiting, candidates.DecisionNoCandidate)
		if got := f.policy().Status.ActiveIncident.IncidentUID; got != string(inc.UID) {
			t.Fatalf("incidentUID %q, want %s", got, inc.UID)
		}
	})
}

func TestE4ObserveOnlyRecordsDecisionWithoutChangingDeployment(t *testing.T) {
	f := newFixture(t, candidateIP)
	healthy := f.node("cand-a", corev1.ConditionTrue, candidateIP)
	cordoned := f.node("cand-b", corev1.ConditionTrue, "10.0.0.6")
	cordoned.Spec.Unschedulable = true
	f.must(f.c.Update(f.ctx, cordoned))
	before := f.liveDeployment()

	f.reconcile()

	inc := f.onlyIncident()
	wantPhase(t, inc, PhaseObserved, candidates.DecisionSelected)
	d := inc.Status.Runtime.Decision
	if d == nil || d.SelectedNodeName != healthy.Name || d.SelectedNodeUID != string(healthy.UID) {
		t.Fatalf("decision = %+v, want selection %s", d, healthy.Name)
	}
	if d.PolicyHash != v1alpha1.PolicyHash(f.policy().Spec) || d.PolicyHash != inc.Spec.PolicyHash {
		t.Fatalf("decision policyHash %s does not match policy", d.PolicyHash)
	}
	if len(d.Candidates) != 2 {
		t.Fatalf("want both candidates recorded, got %d", len(d.Candidates))
	}
	for _, c := range d.Candidates {
		switch c.NodeName {
		case healthy.Name:
			if !c.Eligible || c.Score == nil {
				t.Fatalf("healthy candidate not scored: %+v", c)
			}
		case cordoned.Name:
			if c.Eligible || !slices.Contains(c.RejectReasons, candidates.RejectUnschedulable) {
				t.Fatalf("cordoned candidate reasons %v", c.RejectReasons)
			}
		}
	}

	after := f.liveDeployment()
	if after.ResourceVersion != before.ResourceVersion || after.Generation != before.Generation ||
		v1alpha1.TemplateHash(after.Spec.Template) != v1alpha1.TemplateHash(before.Spec.Template) {
		t.Fatal("ObserveOnly changed the Deployment")
	}
	p := f.policy()
	if p.Status.ActiveIncident != nil {
		t.Fatalf("activeIncident not released: %+v", p.Status.ActiveIncident)
	}
	if len(p.Status.Circuits) != 1 || p.Status.Circuits[0].Phase != PhaseObserved || p.Status.Circuits[0].Signature != inc.Spec.Trigger.Signature {
		t.Fatalf("circuits = %+v, want one OBSERVED", p.Status.Circuits)
	}
	wantCondition(t, p, CondActive)
	if inc.Status.Runtime.CompletedAt == nil {
		t.Fatal("terminal incident lacks completedAt")
	}

	f.reconcile()
	f.onlyIncident()
	wantCondition(t, f.policy(), CondCircuitOpen)
}

func TestE5ReleaseRequiresSameSequenceAndUID(t *testing.T) {
	f := newFixture(t, "")
	f.reconcile()
	inc := f.onlyIncident()
	setPhase(inc, PhaseAborted, ReasonPolicyDisabled, "", f.now)
	f.must(f.c.Status().Update(f.ctx, inc))

	for name, mutate := range map[string]func(*v1alpha1.ActiveIncidentRef){
		"sequence": func(a *v1alpha1.ActiveIncidentRef) { a.Sequence++ },
		"uid":      func(a *v1alpha1.ActiveIncidentRef) { a.IncidentUID = "other" },
	} {
		p := f.policy()
		mutate(p.Status.ActiveIncident)
		if _, err := f.r.release(f.ctx, p, inc, f.now); err != nil {
			t.Fatal(err)
		}
		if f.policy().Status.ActiveIncident == nil {
			t.Fatalf("released despite different %s", name)
		}
	}

	// Terminal Incident with activeIncident still set: crash between the terminal write and release.
	f.reconcile()
	if a := f.policy().Status.ActiveIncident; a != nil {
		t.Fatalf("activeIncident not released: %+v", a)
	}
	f.onlyIncident()
}

func TestE17PolicyDeletedAbortsAndRecreatedPolicyGetsNewName(t *testing.T) {
	f := newFixture(t, "")
	f.reconcile()
	old := f.onlyIncident()
	before := f.liveDeployment()

	f.must(f.c.Delete(f.ctx, f.policy()))
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: policyName}}); err != nil {
		t.Fatal(err)
	}
	aborted := f.onlyIncident()
	wantPhase(t, aborted, PhaseAborted, ReasonPolicyDeleted)
	if f.liveDeployment().ResourceVersion != before.ResourceVersion {
		t.Fatal("Policy deletion changed the Deployment")
	}

	f.must(f.c.Create(f.ctx, f.newPolicy(policyName)))
	f.reconcile()
	incs := f.incidents()
	if len(incs) != 2 {
		t.Fatalf("want old and new incident, got %d", len(incs))
	}
	p := f.policy()
	for _, inc := range incs {
		if inc.Name == old.Name {
			if inc.ResourceVersion != aborted.ResourceVersion {
				t.Fatal("old incident changed after Policy recreation")
			}
			continue
		}
		if !strings.HasPrefix(inc.Name, fmt.Sprintf("%s-%s-", policyName, string(p.UID)[:8])) || inc.Spec.Sequence != 1 {
			t.Fatalf("new incident %s seq %d", inc.Name, inc.Spec.Sequence)
		}
	}
}

func TestE20DeletedIncidentIsNotRecreated(t *testing.T) {
	f := newFixture(t, "")
	f.reconcile()
	inc := f.onlyIncident()
	f.must(f.c.Delete(f.ctx, inc))

	f.reconcile()
	if n := len(f.incidents()); n != 0 {
		t.Fatalf("incident recreated (%d)", n)
	}
	p := f.policy()
	if p.Status.ActiveIncident != nil {
		t.Fatal("activeIncident not released")
	}
	if len(p.Status.Circuits) != 1 || p.Status.Circuits[0].Phase != PhaseIncidentLost || p.Status.Circuits[0].Signature != inc.Spec.Trigger.Signature {
		t.Fatalf("circuits = %+v, want one INCIDENT_LOST", p.Status.Circuits)
	}

	f.reconcile()
	if n := len(f.incidents()); n != 0 {
		t.Fatalf("incident created behind open circuit (%d)", n)
	}
	wantCondition(t, f.policy(), CondCircuitOpen)
}

func TestE22CircuitCapacityBlocksNewIncidents(t *testing.T) {
	f := newFixture(t, "")
	p := f.policy()
	for i := range v1alpha1.MaxCircuits {
		p.Status.Circuits = append(p.Status.Circuits, v1alpha1.Circuit{
			Signature: fmt.Sprintf("sig-%d", i), Class: "UNKNOWN", Incident: "old", Phase: PhaseFailed, OpenedAt: metav1.NewTime(f.now),
		})
	}
	f.must(f.r.writePolicy(f.ctx, p))

	f.reconcile()
	if n := len(f.incidents()); n != 0 {
		t.Fatalf("incident created at circuit capacity (%d)", n)
	}
	p = f.policy()
	wantCondition(t, p, CondCircuitCapacity)
	if len(p.Status.Circuits) != v1alpha1.MaxCircuits {
		t.Fatalf("circuits changed to %d", len(p.Status.Circuits))
	}
}

func TestE23DuplicatePolicyAbortsActiveIncident(t *testing.T) {
	f := newFixture(t, "")
	f.reconcile()
	f.must(f.c.Create(f.ctx, f.newPolicy(policyName+"-2")))

	f.reconcile()
	wantPhase(t, f.onlyIncident(), PhaseAborted, ReasonDuplicatePolicy)
	if a := f.policy().Status.ActiveIncident; a != nil {
		t.Fatalf("activeIncident not released: %+v", a)
	}
}
