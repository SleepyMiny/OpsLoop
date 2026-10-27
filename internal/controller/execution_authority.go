package controller

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const ExecutionGrantAnnotation = "opsloop.io/execution-grant"
const ExecutionGrantBindingAnnotation = "opsloop.io/execution-grant-binding"
const ReasonExecutionAuthority = "EXECUTION_AUTHORITY_UNAVAILABLE"
const executionHeartbeatTTL = 10 * time.Second
const executionMaximumTrial = 600 * time.Second

// ExecutionScope is trusted startup configuration, not authority taken from a
// Policy. A zero scope permits observation/stop only. No credential is stored.
type ExecutionScope struct {
	ClusterUID       string `json:"clusterUID"`
	NamespaceUID     string `json:"namespaceUID"`
	Namespace        string `json:"namespace"`
	Policy           string `json:"policy"`
	PolicyUID        string `json:"policyUID"`
	PolicyGeneration int64  `json:"policyGeneration"`
	PolicyHash       string `json:"policyHash"`
	Deployment       string `json:"deployment"`
	DeploymentUID    string `json:"deploymentUID"`
	Incident         string `json:"incident"`
	RunID            string `json:"runID"`
	Epoch            string `json:"epoch"`
}

func (s ExecutionScope) Validate() error {
	if s == (ExecutionScope{}) {
		return nil
	}
	for _, v := range []string{s.ClusterUID, s.NamespaceUID, s.Namespace, s.Policy, s.PolicyUID, s.Deployment, s.DeploymentUID, s.Incident, s.RunID, s.Epoch} {
		if v == "" || len(v) > 253 {
			return errors.New("execution scope: incomplete binding")
		}
	}
	if s.PolicyGeneration < 1 || len(s.PolicyHash) != 64 {
		return errors.New("execution scope: invalid generation/hash")
	}
	if _, err := hex.DecodeString(s.PolicyHash); err != nil {
		return errors.New("execution scope: invalid hash")
	}
	return nil
}

type ExecutionGrantCore struct {
	Version   string         `json:"version"`
	Scope     ExecutionScope `json:"scope"`
	Instance  string         `json:"instance"`
	NotBefore time.Time      `json:"notBefore"`
	NotAfter  time.Time      `json:"notAfter"`
}
type ExecutionGrant struct {
	ExecutionGrantCore
	HeartbeatSequence uint64    `json:"heartbeatSequence"`
	HeartbeatAt       time.Time `json:"heartbeatAt"`
}

func decodeExecutionGrant(p *v1alpha1.RecoveryPolicy) (ExecutionGrant, error) {
	var g ExecutionGrant
	raw := p.Annotations[ExecutionGrantAnnotation]
	if len(raw) == 0 || len(raw) > 8192 {
		return g, errors.New("execution grant: absent/oversize")
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&g); err != nil {
		return g, errors.New("execution grant: invalid JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return g, errors.New("execution grant: trailing input")
	}
	return g, nil
}
func executionGrantBinding(p *v1alpha1.RecoveryPolicy) string {
	g, err := decodeExecutionGrant(p)
	if err != nil {
		return ""
	}
	raw, _ := json.Marshal(g.ExecutionGrantCore)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ExecutionAuthorizer is mandatory for workload execution. Tests for unrelated
// legacy contracts explicitly supply their own fixture implementation; the
// production command always constructs the real process-bound authorizer.
type ExecutionAuthorizer interface {
	Authorize(*v1alpha1.RecoveryPolicy, *v1alpha1.RecoveryIncident) error
}
type authoritySample struct {
	wall time.Time
	tick time.Duration
}
type ExecutionAuthority struct {
	mu          sync.Mutex
	scope       ExecutionScope
	instance    string
	sample      func() (authoritySample, error)
	seen        bool
	closed      string
	binding     string
	sequence    uint64
	heartbeatAt time.Time
	beatTick    time.Duration
	expiryTick  time.Duration
	last        authoritySample
	incidentUID string
}

func NewExecutionAuthority(scope ExecutionScope) (*ExecutionAuthority, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("execution identity: %w", err)
	}
	return &ExecutionAuthority{scope: scope, instance: hex.EncodeToString(b), sample: func() (authoritySample, error) {
		tick, err := executionBootTick()
		return authoritySample{time.Now().UTC(), tick}, err
	}}, nil
}

// Instance is a non-secret identifier. It changes on every process start, even
// if the same Pod/container filesystem or unexpired Policy survives a reboot.
func (a *ExecutionAuthority) Instance() string { return a.instance }
func (a *ExecutionAuthority) Authorize(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	deny := func(reason string) error { a.closed = reason; return errors.New(reason) }
	if a.closed != "" {
		return errors.New("execution authority retired: " + a.closed)
	}
	s := a.scope
	if s == (ExecutionScope{}) || p.Namespace != s.Namespace || p.Name != s.Policy || string(p.UID) != s.PolicyUID {
		return errors.New("execution authority: target not in trusted startup scope")
	}
	if p.DeletionTimestamp != nil || !p.Spec.Enabled || p.Spec.Mode != v1alpha1.PolicyModeEnforce || p.Generation != s.PolicyGeneration || v1alpha1.PolicyHash(p.Spec) != s.PolicyHash || p.Spec.TargetRef.Name != s.Deployment {
		return deny("execution authority: policy binding changed")
	}
	n, err := a.sample()
	if err != nil {
		return deny("execution authority: clock unavailable")
	}
	if a.seen && (n.tick < a.last.tick || n.wall.Before(a.last.wall)) {
		return deny("execution authority: clock reversed")
	}
	g, err := decodeExecutionGrant(p)
	if err != nil {
		return deny(err.Error())
	}
	if g.Version != "opsloop-execution-grant/v1" || g.Scope != s || g.Instance != a.instance {
		return deny("execution authority: scope/process mismatch")
	}
	if g.NotBefore.IsZero() || g.NotAfter.IsZero() || !g.NotAfter.After(g.NotBefore) || g.NotAfter.Sub(g.NotBefore) > executionMaximumTrial || n.wall.Before(g.NotBefore) || !n.wall.Before(g.NotAfter) {
		return deny("execution authority: outside fixed time window")
	}
	if g.HeartbeatSequence == 0 || g.HeartbeatAt.Before(g.NotBefore) || g.HeartbeatAt.After(n.wall) || n.wall.Sub(g.HeartbeatAt) >= executionHeartbeatTTL {
		return deny("execution authority: heartbeat absent/stale/future")
	}
	binding := executionGrantBinding(p)
	if inc != nil {
		if inc.Name != s.Incident || inc.Namespace != s.Namespace || inc.UID == "" || inc.Spec.PolicyRef.UID != s.PolicyUID || inc.Spec.PolicyGeneration != s.PolicyGeneration || inc.Spec.PolicyHash != s.PolicyHash || inc.Spec.Workload.Name != s.Deployment || inc.Spec.Workload.UID != s.DeploymentUID || inc.Annotations[ExecutionGrantBindingAnnotation] != binding {
			return deny("execution authority: incident binding mismatch")
		}
		if a.incidentUID != "" && a.incidentUID != string(inc.UID) {
			return deny("execution authority: incident UID changed")
		}
	} else if p.Status.ActiveIncident == nil && p.Status.IncidentSequence != 0 {
		return deny("execution authority: single incident already consumed")
	}
	if !a.seen {
		a.seen = true
		a.binding = binding
		a.expiryTick = n.tick + g.NotAfter.Sub(n.wall)
		a.sequence = g.HeartbeatSequence
		a.heartbeatAt = g.HeartbeatAt
		a.beatTick = n.tick
	} else {
		if binding != a.binding || n.tick >= a.expiryTick || n.tick-a.beatTick >= executionHeartbeatTTL {
			return deny("execution authority: fixed grant or heartbeat elapsed")
		}
		if g.HeartbeatSequence < a.sequence || g.HeartbeatAt.Before(a.heartbeatAt) {
			return deny("execution authority: heartbeat replay")
		}
		if g.HeartbeatSequence == a.sequence {
			if !g.HeartbeatAt.Equal(a.heartbeatAt) {
				return deny("execution authority: heartbeat timestamp rewritten")
			}
		} else {
			if !g.HeartbeatAt.After(a.heartbeatAt) {
				return deny("execution authority: heartbeat did not advance")
			}
			a.sequence = g.HeartbeatSequence
			a.heartbeatAt = g.HeartbeatAt
			a.beatTick = n.tick
		}
	}
	a.last = n
	if inc != nil {
		a.incidentUID = string(inc.UID)
	}
	return nil
}
func (r *PolicyReconciler) authorizeExecution(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) error {
	if r.Authority == nil {
		return errors.New("execution authority: no authorizer")
	}
	return r.Authority.Authorize(p, inc)
}
