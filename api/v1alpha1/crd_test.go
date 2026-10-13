package v1alpha1

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/SleepyMiny/OpsLoop/internal/testenv"
)

func startEnv(t *testing.T) *envtest.Environment {
	t.Helper()
	return testenv.Start(t, filepath.Join("..", "..", "config", "crd", "bases"))
}

func TestMain(m *testing.M) { testenv.Main(m) }

func newTestClient(t *testing.T) (client.Client, string) {
	t.Helper()
	env := startEnv(t)
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(env.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "e1-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return c, ns.Name
}

func minimalPolicySpec() map[string]any {
	return map[string]any{
		"targetRef":         map[string]any{"name": "demo-api"},
		"candidateSelector": map[string]any{},
		"verification":      map[string]any{"serviceName": "demo-api"},
	}
}

// fullPolicySpec sets every field because the typed client sends zero values instead of omitting them.
func fullPolicySpec() RecoveryPolicySpec {
	return RecoveryPolicySpec{
		TargetRef:         TargetRef{Name: "demo-api"},
		Enabled:           true,
		Mode:              PolicyModeObserveOnly,
		CandidateSelector: metav1.LabelSelector{MatchLabels: map[string]string{"opsloop.io/recovery-candidate": "true"}},
		RequiredPlatforms: []string{"linux/amd64", "linux/arm64"},
		Detection: DetectionSpec{
			NodeUnreadyFor:   metav1.Duration{Duration: 20 * time.Second},
			RestartThreshold: 3,
			RestartWindow:    metav1.Duration{Duration: 120 * time.Second},
		},
		Filter: FilterSpec{
			MaxCPUUtilizationPercent:        85,
			MaxMemoryUtilizationPercent:     85,
			MinimumAvailableMemory:          resource.MustParse("512Mi"),
			StartupMemoryBudget:             resource.MustParse("256Mi"),
			RequestedCapacityReservePercent: 10,
			MaxSampleAge:                    metav1.Duration{Duration: 30 * time.Second},
		},
		Score: ScoreSpec{
			CPUWeight:               30,
			MemoryWeight:            35,
			RequestsWeight:          35,
			RecentFailurePenalty:    15,
			MaxRecentFailurePenalty: 45,
			RecentFailureWindow:     metav1.Duration{Duration: 30 * time.Minute},
		},
		Budget: BudgetSpec{
			MaxActionsPerIncident: 2,
			Cooldown:              metav1.Duration{Duration: 120 * time.Second},
			PlacementTimeout:      metav1.Duration{Duration: 90 * time.Second},
			VerificationTimeout:   metav1.Duration{Duration: 60 * time.Second},
			IncidentDeadline:      metav1.Duration{Duration: 10 * time.Minute},
		},
		Verification: VerificationSpec{
			ServiceName:          "demo-api",
			Port:                 8080,
			ConsecutiveSuccesses: 5,
			StabilityWindow:      metav1.Duration{Duration: 30 * time.Second},
		},
	}
}

func unstructuredPolicy(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "RecoveryPolicy",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

func TestE1PolicyDefaults(t *testing.T) {
	c, ns := newTestClient(t)
	ctx := context.Background()

	spec := minimalPolicySpec()
	spec["filter"] = map[string]any{"requestedCapacityReservePercent": int64(0)}
	obj := unstructuredPolicy(ns, "defaults", spec)
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create minimal policy: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	if v, found, err := unstructured.NestedInt64(got.Object, "spec", "filter", "requestedCapacityReservePercent"); err != nil || !found || v != 0 {
		t.Fatalf("explicit requestedCapacityReservePercent 0 not preserved: v=%d found=%v err=%v", v, found, err)
	}

	raw, err := json.Marshal(got.Object["spec"])
	if err != nil {
		t.Fatal(err)
	}
	var gotSpec RecoveryPolicySpec
	if err := json.Unmarshal(raw, &gotSpec); err != nil {
		t.Fatal(err)
	}
	want := fullPolicySpec()
	want.CandidateSelector = metav1.LabelSelector{}
	want.Filter.RequestedCapacityReservePercent = 0
	if !equality.Semantic.DeepEqual(want, gotSpec) {
		t.Fatalf("defaulted spec mismatch\n got: %s", raw)
	}
}

func TestE1PolicyValidation(t *testing.T) {
	c, ns := newTestClient(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		objName string
		mutate  func(spec map[string]any)
		wantErr string // empty means the create must succeed
	}{
		{"name of 40 characters accepted", strings.Repeat("a", 40), func(map[string]any) {}, ""},
		{"name of 41 characters", strings.Repeat("b", 41), func(map[string]any) {}, "name must be at most 40 characters"},
		{"weights not summing to 100", "weights", func(s map[string]any) {
			s["score"] = map[string]any{"cpuWeight": int64(30), "memoryWeight": int64(35), "requestsWeight": int64(30)}
		}, "weights must sum to 100"},
		{"mode typo", "mode", func(s map[string]any) { s["mode"] = "Enforced" }, "spec.mode"},
		{"maxSampleAge 0s", "sample-age", func(s map[string]any) {
			s["filter"] = map[string]any{"maxSampleAge": "0s"}
		}, "spec.filter.maxSampleAge"},
		{"three requiredPlatforms", "platforms", func(s map[string]any) {
			s["requiredPlatforms"] = []any{"linux/amd64", "linux/arm64", "linux/amd64"}
		}, "spec.requiredPlatforms"},
		{"zero quantities accepted", "zero-quantity", func(s map[string]any) {
			s["filter"] = map[string]any{"minimumAvailableMemory": int64(0), "startupMemoryBudget": "0"}
		}, ""},
		{"negative minimumAvailableMemory", "neg-min-mem", func(s map[string]any) {
			s["filter"] = map[string]any{"minimumAvailableMemory": "-1Mi"}
		}, "spec.filter.minimumAvailableMemory: Invalid value"},
		{"negative integer startupMemoryBudget", "neg-startup", func(s map[string]any) {
			s["filter"] = map[string]any{"startupMemoryBudget": int64(-1)}
		}, "spec.filter.startupMemoryBudget: Invalid value"},
		{"maxSampleAge above 10m", "sample-age-max", func(s map[string]any) {
			s["filter"] = map[string]any{"maxSampleAge": "11m"}
		}, "maxSampleAge must be at most 10m"},
		{"targetRef name of 254 characters", "target-long", func(s map[string]any) {
			s["targetRef"] = map[string]any{"name": strings.Repeat("d", 254)}
		}, "spec.targetRef.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalPolicySpec()
			tc.mutate(spec)
			err := c.Create(ctx, unstructuredPolicy(ns, tc.objName, spec))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				return
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want Invalid containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestE1PolicyTargetRefImmutable(t *testing.T) {
	c, ns := newTestClient(t)
	ctx := context.Background()

	obj := unstructuredPolicy(ns, "immutable", minimalPolicySpec())
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if err := unstructured.SetNestedField(obj.Object, false, "spec", "enabled"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, obj); err != nil {
		t.Fatalf("other spec update must be allowed: %v", err)
	}
	if err := unstructured.SetNestedField(obj.Object, "other-api", "spec", "targetRef", "name"); err != nil {
		t.Fatal(err)
	}
	err := c.Update(ctx, obj)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "targetRef is immutable") {
		t.Fatalf("want Invalid targetRef is immutable, got %v", err)
	}
}

func newIncident(ns string, now metav1.Time) *RecoveryIncident {
	policy := fullPolicySpec()
	return &RecoveryIncident{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-12345678-00001", Namespace: ns},
		Spec: RecoveryIncidentSpec{
			PolicyRef:        PolicyRef{Name: "demo", UID: "12345678-0000-0000-0000-000000000000"},
			Sequence:         1,
			PolicyGeneration: 1,
			PolicyHash:       PolicyHash(policy),
			Policy:           policy,
			Workload:         WorkloadRef{Name: "demo-api", UID: "workload-uid"},
			Trigger: Trigger{
				Class:       "INFRASTRUCTURE_FAILURE",
				Reason:      "NODE_NOT_READY",
				Signature:   "sig",
				RuleVersion: "classify-v1",
				NodeName:    "primary",
				ObservedAt:  now,
				Evidence:    []string{"NODE_NOT_READY pod=demo-api node=primary"},
			},
			Baseline:   Baseline{TemplateHash: "t", SpecHash: "s", Image: "demo-api:v1", ImageDigest: "sha256:0", Replicas: 1},
			DetectedAt: now,
			Deadline:   metav1.NewTime(now.Add(10 * time.Minute)),
		},
	}
}

func TestE1IncidentSpecImmutable(t *testing.T) {
	c, ns := newTestClient(t)
	ctx := context.Background()

	inc := newIncident(ns, metav1.NewTime(time.Now().Truncate(time.Second)))
	if err := c.Create(ctx, inc); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	inc.Labels = map[string]string{"opsloop.io/policy": "demo"}
	if err := c.Update(ctx, inc); err != nil {
		t.Fatalf("metadata update must be allowed: %v", err)
	}

	inc.Spec.Sequence = 2
	err := c.Update(ctx, inc)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "spec is immutable") {
		t.Fatalf("want Invalid spec is immutable, got %v", err)
	}
}

// E2 (zero candidate Nodes) writes a decision whose Candidates is nil; the apiserver must accept it.
func TestE1DecisionWithoutCandidates(t *testing.T) {
	c, ns := newTestClient(t)
	ctx := context.Background()

	now := metav1.NewTime(time.Now().Truncate(time.Second))
	inc := newIncident(ns, now)
	if err := c.Create(ctx, inc); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	d := &RecoveryDecision{
		IncidentID:         inc.Name,
		PolicyGeneration:   1,
		PolicyHash:         inc.Spec.PolicyHash,
		WorkloadUID:        "workload-uid",
		SnapshotObservedAt: now,
		SnapshotExpiresAt:  metav1.NewTime(now.Add(30 * time.Second)),
		ActionType:         "None",
		DecisionReason:     "NO_CANDIDATE",
		DecidedAt:          now,
	}
	inc.Status.Runtime = RuntimeStatus{
		Phase:              "WAITING",
		Reason:             "NO_CANDIDATE",
		LastTransitionTime: now,
		Decision:           d,
		Attempts:           []Attempt{{ActionID: inc.Name + "-a1", Outcome: "Cancelled", Decision: *d, ReservedAt: now}},
	}
	if err := c.Status().Update(ctx, inc); err != nil {
		t.Fatalf("status update with nil candidates must be allowed: %v", err)
	}
}
