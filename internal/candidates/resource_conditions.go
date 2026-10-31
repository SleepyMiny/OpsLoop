package candidates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceConditions is trusted, opt-in startup configuration. It never changes
// Prometheus observations or Node status and is not an execution permission.
type ResourceConditions struct {
	Schema        string                  `json:"schema"`
	RunID         string                  `json:"runID"`
	Epoch         string                  `json:"epoch"`
	PolicyUID     string                  `json:"policyUID"`
	DeploymentUID string                  `json:"deploymentUID"`
	NotBefore     time.Time               `json:"notBefore"`
	ExpiresAt     time.Time               `json:"expiresAt"`
	Nodes         []NodeResourceCondition `json:"nodes"`
}
type NodeResourceCondition struct {
	NodeName                    string `json:"nodeName"`
	NodeUID                     string `json:"nodeUID"`
	CPUUtilizationFloorPermille *int32 `json:"cpuUtilizationFloorPermille,omitempty"`
	MemoryAvailableCeilingBytes *int64 `json:"memoryAvailableCeilingBytes,omitempty"`
}

func ParseResourceConditions(raw []byte) (*ResourceConditions, error) {
	if len(raw) == 0 || len(raw) > 16384 {
		return nil, errors.New("resource conditions: empty or oversized configuration")
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return nil, errors.New("resource conditions: ambiguous JSON")
	}
	var c ResourceConditions
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, errors.New("resource conditions: invalid JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("resource conditions: trailing JSON")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
func (c *ResourceConditions) Validate() error {
	if c == nil {
		return errors.New("resource conditions: absent configuration")
	}
	if c.Schema != "opsloop-resource-conditions/v1" {
		return errors.New("resource conditions: unknown schema")
	}
	for _, id := range []string{c.RunID, c.Epoch, c.PolicyUID, c.DeploymentUID} {
		if id == "" || len(id) > 253 {
			return errors.New("resource conditions: incomplete binding")
		}
	}
	if c.NotBefore.IsZero() || c.ExpiresAt.IsZero() || !c.ExpiresAt.After(c.NotBefore) || c.ExpiresAt.Sub(c.NotBefore) > 600*time.Second {
		return errors.New("resource conditions: invalid fixed time window")
	}
	if len(c.Nodes) == 0 || len(c.Nodes) > v1alpha1.MaxCandidates {
		return errors.New("resource conditions: invalid node count")
	}
	names, uids := map[string]bool{}, map[string]bool{}
	for _, n := range c.Nodes {
		if n.NodeName == "" || len(n.NodeName) > 253 || n.NodeUID == "" || len(n.NodeUID) > 253 || names[n.NodeName] || uids[n.NodeUID] {
			return errors.New("resource conditions: ambiguous node binding")
		}
		names[n.NodeName], uids[n.NodeUID] = true, true
		if n.CPUUtilizationFloorPermille == nil && n.MemoryAvailableCeilingBytes == nil {
			return errors.New("resource conditions: empty node condition")
		}
		if n.CPUUtilizationFloorPermille != nil && (*n.CPUUtilizationFloorPermille < 0 || *n.CPUUtilizationFloorPermille > 1000) || n.MemoryAvailableCeilingBytes != nil && *n.MemoryAvailableCeilingBytes < 0 {
			return errors.New("resource conditions: invalid resource bound")
		}
	}
	return nil
}
func (c *ResourceConditions) SHA256() string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ValidateResourceInput refuses an entire experiment when its configuration or
// any targeted original measurement cannot be established. It never fills gaps.
func ValidateResourceInput(in Input) error {
	c := in.ResourceConditions
	if c == nil {
		return nil
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.RunID != in.RunID || c.Epoch != in.Epoch || c.PolicyUID != in.PolicyUID || in.Deployment == nil || c.DeploymentUID != string(in.Deployment.UID) {
		return errors.New("resource conditions: execution binding mismatch")
	}
	if in.Now.Before(c.NotBefore) || !in.Now.Before(c.ExpiresAt) {
		return errors.New("resource conditions: outside fixed time window")
	}
	if len(in.Nodes) > v1alpha1.MaxCandidates {
		return errors.New("resource conditions: candidate set exceeds complete evidence bound")
	}
	if in.MetricsErr != nil {
		return errors.New("resource conditions: original metrics unavailable")
	}
	for _, expected := range c.Nodes {
		found := false
		for i := range in.Nodes {
			n := &in.Nodes[i]
			if n.Name != expected.NodeName {
				continue
			}
			if found || string(n.UID) != expected.NodeUID || n.DeletionTimestamp != nil {
				return errors.New("resource conditions: node identity changed")
			}
			found = true
			var measured v1alpha1.Measurements
			missing, stale, _ := assessMetrics(in.Metrics, n, in.Now, in.Spec.Filter.MaxSampleAge.Duration, &measured)
			if missing || stale {
				return errors.New("resource conditions: original metrics incomplete or stale")
			}
		}
		if !found {
			return errors.New("resource conditions: bound node absent")
		}
	}
	return nil
}

func applyResourceCondition(in Input, c *v1alpha1.CandidateEvaluation) *v1alpha1.Measurements {
	if in.ResourceConditions == nil {
		return &c.Measurements
	}
	config := in.ResourceConditions
	applied := c.Measurements.DeepCopy()
	for _, n := range config.Nodes {
		if n.NodeUID != c.NodeUID || n.NodeName != c.NodeName {
			continue
		}
		if n.CPUUtilizationFloorPermille != nil && applied.CPUUtilizationPermille != nil {
			*applied.CPUUtilizationPermille = max(*applied.CPUUtilizationPermille, *n.CPUUtilizationFloorPermille)
		}
		if n.MemoryAvailableCeilingBytes != nil && applied.MemAvailableBytes != nil {
			*applied.MemAvailableBytes = min(*applied.MemAvailableBytes, *n.MemoryAvailableCeilingBytes)
		}
	}
	c.ResourceCondition = &v1alpha1.AppliedResourceCondition{RunID: config.RunID, Epoch: config.Epoch, PolicyUID: config.PolicyUID, ConfigSHA256: config.SHA256(), ExpiresAt: metav1.NewTime(config.ExpiresAt), AppliedMeasurements: *applied}
	return &c.ResourceCondition.AppliedMeasurements
}

// encoding/json accepts duplicate (including case-folded) field names. Reject
// them before decoding because two operators must not read different scopes.
func uniqueJSONKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			s = strings.ToLower(s)
			if seen[s] {
				return errors.New("duplicate field")
			}
			seen[s] = true
			if err := uniqueJSONKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONKeys(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}
