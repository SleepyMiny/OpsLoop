package v1alpha1

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReadinessSchemaExplicitBoundsAndRetention(t *testing.T) {
	c, ns := newTestClient(t)
	for index, value := range []string{"15s", "30s", "5m", "14s", "301s", "nonsense"} {
		t.Run(value, func(t *testing.T) {
			spec := minimalPolicySpec()
			spec["detection"] = map[string]any{"podUnreadyFor": value}
			obj := unstructuredPolicy(ns, fmt.Sprintf("readiness-%d", index), spec)
			err := c.Create(context.Background(), obj)
			if index >= 3 {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("invalid duration %s accepted/error=%v", value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored := &RecoveryPolicy{}
			if err = c.Get(context.Background(), client.ObjectKeyFromObject(obj), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Spec.Detection.PodUnreadyFor == nil || stored.Spec.Detection.PodUnreadyFor.Duration <= 0 {
				t.Fatal("nullable opt-in was pruned")
			}
		})
	}
}
