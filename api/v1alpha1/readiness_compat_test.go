package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadinessOptInPreservesLegacyJSONAndCopies(t *testing.T) {
	d := DetectionSpec{NodeUnreadyFor: metav1.Duration{Duration: 20 * time.Second}, RestartThreshold: 3, RestartWindow: metav1.Duration{Duration: 120 * time.Second}}
	b, _ := json.Marshal(d)
	if string(b) != `{"nodeUnreadyFor":"20s","restartThreshold":3,"restartWindow":"2m0s"}` {
		t.Fatalf("legacy detection bytes changed: %s", b)
	}
	p := RecoveryPolicySpec{Detection: d}
	before := PolicyHash(p)
	p.Detection.PodUnreadyFor = &metav1.Duration{Duration: 30 * time.Second}
	if PolicyHash(p) == before {
		t.Fatal("opt-in not in Policy hash")
	}
	copy := p.DeepCopy()
	copy.Detection.PodUnreadyFor.Duration = time.Minute
	if p.Detection.PodUnreadyFor.Duration != 30*time.Second {
		t.Fatal("DeepCopy aliases nullable opt-in")
	}
	p.Detection.PodUnreadyFor = nil
	b, _ = json.Marshal(p)
	if PolicyHash(p) != before || strings.Contains(string(b), "podUnreadyFor") {
		t.Fatal("legacy Policy hash/omission changed")
	}
}
