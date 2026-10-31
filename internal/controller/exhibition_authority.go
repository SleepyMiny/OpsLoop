package controller

// Repeated exhibitions use the same Policy and Controller process. The static
// trust boundary contains only exact workloads and an offline issuer public key;
// each consumable grant must additionally have an issuer-signed permit.
import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ExhibitionPermitAnnotation = "opsloop.io/exhibition-permit"
const ExhibitionRunAnnotation = "opsloop.io/exhibition-run"
const ExhibitionEpochAnnotation = "opsloop.io/exhibition-epoch"
const ExhibitionPermitBindingAnnotation = "opsloop.io/exhibition-permit-sha256"

type ExhibitionTarget struct {
	ClusterUID    string `json:"clusterUID"`
	NamespaceUID  string `json:"namespaceUID"`
	Namespace     string `json:"namespace"`
	Policy        string `json:"policy"`
	PolicyUID     string `json:"policyUID"`
	Deployment    string `json:"deployment"`
	DeploymentUID string `json:"deploymentUID"`
}
type ExhibitionAuthorityConfig struct {
	Schema    string             `json:"schema"`
	PublicKey string             `json:"publicKey"`
	Targets   []ExhibitionTarget `json:"targets"`
}
type ExhibitionModelBudget struct {
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoningEffort"`
	AuthMode        string    `json:"authMode"`
	MaxModelCalls   int32     `json:"maxModelCalls"`
	TotalSeconds    int32     `json:"totalSeconds"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// CircuitReplay is an explicit, single-run exception, not a recovered circuit.
// Evidence is revalidated by the host issuer; its hashes are signed here.
type CircuitReplay struct {
	Purpose          string `json:"purpose"`
	WorkloadImage    string `json:"workloadImage"`
	Signature        string `json:"signature"`
	PriorRunID       string `json:"priorRunID"`
	PriorEpoch       string `json:"priorEpoch"`
	PriorIncidentUID string `json:"priorIncidentUID"`
	IncidentSHA256   string `json:"incidentSHA256"`
	BundleSHA256     string `json:"bundleSHA256"`
	CompletionSHA256 string `json:"completionSHA256"`
	ApprovalSHA256   string `json:"approvalSHA256"`
}
type CircuitEvidence struct {
	WorkloadImage    string `json:"workloadImage"`
	Signature        string `json:"signature"`
	PriorRunID       string `json:"priorRunID"`
	PriorEpoch       string `json:"priorEpoch"`
	PriorIncidentUID string `json:"priorIncidentUID"`
	IncidentSHA256   string `json:"incidentSHA256"`
	BundleSHA256     string `json:"bundleSHA256"`
	CompletionSHA256 string `json:"completionSHA256"`
	PolicyUID        string `json:"policyUID"`
	DeploymentUID    string `json:"deploymentUID"`
}
type PredecessorCompletion struct {
	RunID            string `json:"runID"`
	Epoch            string `json:"epoch"`
	PolicyUID        string `json:"policyUID"`
	DeploymentUID    string `json:"deploymentUID"`
	CompletionSHA256 string `json:"completionSHA256"`
	RetirementSHA256 string `json:"retirementSHA256"`
}
type NewExecutionAuthorization struct {
	ApprovedReproductionSHA256  string `json:"approvedReproductionSHA256,omitempty"`
	AuthorizationID             string `json:"authorizationID"`
	ApprovalSHA256              string `json:"approvalSHA256"`
	CircuitEvidenceSHA256       string `json:"circuitEvidenceSHA256"`
	PredecessorCompletionSHA256 string `json:"predecessorCompletionSHA256"`
}
type ExhibitionPermit struct {
	ApprovedReproduction      *ApprovedReproduction          `json:"approvedReproduction,omitempty"`
	CircuitEvidence           *CircuitEvidence               `json:"circuitEvidence,omitempty"`
	PredecessorCompletion     *PredecessorCompletion         `json:"predecessorCompletion,omitempty"`
	NewExecutionAuthorization *NewExecutionAuthorization     `json:"newExecutionAuthorization,omitempty"`
	CircuitReplay             *CircuitReplay                 `json:"circuitReplay,omitempty"`
	Schema                    string                         `json:"schema"`
	Grant                     ExecutionGrantCore             `json:"grant"`
	AuthorizationID           string                         `json:"authorizationID"`
	Ordinal                   uint64                         `json:"ordinal"`
	BaseIncidentSequence      int64                          `json:"baseIncidentSequence"`
	MaxIncidents              int32                          `json:"maxIncidents"`
	MaxActions                int32                          `json:"maxActions"`
	PreviousCompletionSHA256  string                         `json:"previousCompletionSHA256"`
	AllowedCircuitSignatures  []string                       `json:"allowedCircuitSignatures"`
	ResourceConditions        *candidates.ResourceConditions `json:"resourceConditions,omitempty"`
	ModelBudget               *ExhibitionModelBudget         `json:"modelBudget,omitempty"`
}
type SignedExhibitionPermit struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type stoppedExhibitionTarget struct{ generation, sequence int64 }
type activeExhibition struct {
	permit     ExhibitionPermit
	hash       string
	authority  *ExecutionAuthority
	retired    bool
	occurrence *CurrentOccurrence
	// Process-bound: a restart changes instance and cannot reuse this permit.
	sourceWaitStarted  time.Time
	sourceWaitDeadline time.Time
}
type ExhibitionAuthority struct {
	mu        sync.Mutex
	config    ExhibitionAuthorityConfig
	publicKey ed25519.PublicKey
	instance  string
	sample    func() (authoritySample, error)
	current   *activeExhibition
	highest   uint64
	stopped   map[string]stoppedExhibitionTarget
	used      map[string]bool
}

// strictAuthorityJSON rejects duplicate fields, unknown fields, case aliases and
// trailing input; signing an ambiguous JSON object must never select a meaning.
func strictAuthorityJSON(raw []byte, out any) error {
	if len(raw) == 0 || len(raw) > 32768 {
		return errors.New("authority JSON: absent/oversize")
	}
	if err := uniqueJSONFields(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("authority JSON: trailing input")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var keys func(any, reflect.Type) error
	keys = func(v any, t reflect.Type) error {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == reflect.TypeOf(json.RawMessage{}) {
			return nil
		}
		if v == nil {
			return nil
		}
		switch x := v.(type) {
		case map[string]any:
			if t.Kind() == reflect.Map && t.Key().Kind() == reflect.String {
				for _, child := range x {
					if err := keys(child, t.Elem()); err != nil {
						return err
					}
				}
				return nil
			}
			if t.Kind() != reflect.Struct {
				return errors.New("authority JSON: unexpected object")
			}
			fields := map[string]reflect.Type{}
			var add func(reflect.Type)
			add = func(t reflect.Type) {
				for i := 0; i < t.NumField(); i++ {
					f := t.Field(i)
					name := strings.Split(f.Tag.Get("json"), ",")[0]
					if f.Anonymous && name == "" {
						add(f.Type)
						continue
					}
					if name != "" && name != "-" {
						fields[name] = f.Type
					}
				}
			}
			add(t)
			for k, c := range x {
				ft, ok := fields[k]
				if !ok {
					return errors.New("authority JSON: non-exact field")
				}
				if err := keys(c, ft); err != nil {
					return err
				}
			}
		case []any:
			if t.Kind() != reflect.Slice {
				return errors.New("authority JSON: unexpected array")
			}
			for _, c := range x {
				if err := keys(c, t.Elem()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return keys(value, reflect.TypeOf(out))
}
func ParseExhibitionAuthorityConfig(raw []byte) (ExhibitionAuthorityConfig, error) {
	var c ExhibitionAuthorityConfig
	if err := strictAuthorityJSON(raw, &c); err != nil {
		return c, err
	}
	_, err := NewExhibitionAuthority(c)
	return c, err
}
func targetFor(s ExecutionScope) ExhibitionTarget {
	return ExhibitionTarget{s.ClusterUID, s.NamespaceUID, s.Namespace, s.Policy, s.PolicyUID, s.Deployment, s.DeploymentUID}
}
func NewExhibitionAuthority(c ExhibitionAuthorityConfig) (*ExhibitionAuthority, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(c.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || c.Schema != "opsloop-exhibition-authority/v1" || len(c.Targets) == 0 || len(c.Targets) > 16 {
		return nil, errors.New("exhibition authority: invalid trust configuration")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		for _, v := range []string{t.ClusterUID, t.NamespaceUID, t.Namespace, t.Policy, t.PolicyUID, t.Deployment, t.DeploymentUID} {
			if v == "" || len(v) > 253 {
				return nil, errors.New("exhibition authority: incomplete target")
			}
		}
		if seen[t.Namespace+"/"+t.Policy] || seen[t.PolicyUID] {
			return nil, errors.New("exhibition authority: duplicate target")
		}
		seen[t.Namespace+"/"+t.Policy] = true
		seen[t.PolicyUID] = true
	}
	legacy, err := NewExecutionAuthority(ExecutionScope{})
	if err != nil {
		return nil, err
	}
	return &ExhibitionAuthority{config: ExhibitionAuthorityConfig{Schema: c.Schema, PublicKey: c.PublicKey, Targets: append([]ExhibitionTarget(nil), c.Targets...)}, publicKey: key, instance: legacy.instance, sample: legacy.sample, stopped: map[string]stoppedExhibitionTarget{}, used: map[string]bool{}}, nil
}
func (a *ExhibitionAuthority) Instance() string { return a.instance }
func (a *ExhibitionAuthority) decode(p *v1alpha1.RecoveryPolicy) (ExhibitionPermit, string, error) {
	var envelope SignedExhibitionPermit
	var permit ExhibitionPermit
	if err := strictAuthorityJSON([]byte(p.Annotations[ExhibitionPermitAnnotation]), &envelope); err != nil {
		return permit, "", err
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		return permit, "", errors.New("exhibition permit: invalid payload")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(a.publicKey, raw, signature) {
		return permit, "", errors.New("exhibition permit: invalid signature")
	}
	if err := strictAuthorityJSON(raw, &permit); err != nil {
		return permit, "", err
	}
	s := permit.Grant.Scope
	if (permit.Schema != "opsloop-exhibition-permit/v1" && permit.Schema != "opsloop-exhibition-permit/v2" && permit.Schema != "opsloop-exhibition-permit/v3") || permit.Grant.Version != "opsloop-execution-grant/v1" || permit.Grant.Instance != a.instance || s.Validate() != nil || !slices.Contains(a.config.Targets, targetFor(s)) || permit.AuthorizationID == "" || len(permit.AuthorizationID) > 128 || permit.Ordinal == 0 || permit.BaseIncidentSequence < 0 || permit.BaseIncidentSequence >= 1<<62 || permit.MaxIncidents != 1 || permit.MaxActions != 1 {
		return permit, "", errors.New("exhibition permit: scope/budget/process mismatch")
	}
	if s.Incident != fmt.Sprintf("%s-%s-%05d", s.Policy, firstUID(s.PolicyUID), permit.BaseIncidentSequence+1) {
		return permit, "", errors.New("exhibition permit: sequence name mismatch")
	}
	if len(permit.PreviousCompletionSHA256) != 64 {
		return permit, "", errors.New("exhibition permit: completion attestation absent")
	}
	if _, err := hex.DecodeString(permit.PreviousCompletionSHA256); err != nil {
		return permit, "", errors.New("exhibition permit: completion attestation invalid")
	}
	if len(permit.AllowedCircuitSignatures) > v1alpha1.MaxCircuits {
		return permit, "", errors.New("exhibition permit: circuit scope oversized")
	}
	seen := map[string]bool{}
	for _, s := range permit.AllowedCircuitSignatures {
		if len(s) != 64 || seen[s] {
			return permit, "", errors.New("exhibition permit: circuit signature invalid")
		}
		if _, err := hex.DecodeString(s); err != nil {
			return permit, "", errors.New("exhibition permit: circuit signature invalid")
		}
		seen[s] = true
	}
	if err := validateEvidenceRoles(permit); err != nil {
		return permit, "", err
	}
	if err := validateCircuitReplay(permit); err != nil {
		return permit, "", err
	}
	if b := permit.ModelBudget; b != nil {
		if b.Model != "gpt-6-luna" || b.ReasoningEffort != "medium" || b.AuthMode != "chatgpt" || b.MaxModelCalls != 1 || b.TotalSeconds < 1 || b.TotalSeconds > 900 || !b.ExpiresAt.After(permit.Grant.NotBefore) || b.ExpiresAt.After(permit.Grant.NotBefore.Add(time.Duration(b.TotalSeconds)*time.Second)) {
			return permit, "", errors.New("exhibition permit: model budget invalid")
		}
	}
	if permit.ResourceConditions != nil {
		if err := BindResourceConditions(s, permit.ResourceConditions); err != nil {
			return permit, "", err
		}
		if permit.ResourceConditions.NotBefore.Before(permit.Grant.NotBefore) || permit.ResourceConditions.ExpiresAt.After(permit.Grant.NotAfter) {
			return permit, "", errors.New("exhibition permit: resource time outside grant")
		}
	}
	sum := sha256.Sum256(raw)
	return permit, hex.EncodeToString(sum[:]), nil
}
func firstUID(uid string) string {
	if len(uid) < 8 {
		return uid
	}
	return uid[:8]
}

// ObserveStopped is called only AFTER the reconciler has re-read the Deployment,
// durably closed its fence and settled the active reservation. Status supplied by
// an enabled Policy, an old generation or a browser cannot invoke this hook.
func (a *ExhibitionAuthority) ObserveStopped(p *v1alpha1.RecoveryPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.Spec.Enabled || p.Status.ActiveIncident != nil || p.DeletionTimestamp != nil {
		return
	}
	confirmed := false
	for _, c := range p.Status.Conditions {
		if c.Type == CondQuiesced && c.Status == metav1.ConditionTrue && c.Reason == "FenceClosed" && c.ObservedGeneration == p.Generation {
			confirmed = true
		}
	}
	if !confirmed {
		return
	}
	matched := false
	for _, t := range a.config.Targets {
		if t.Namespace == p.Namespace && t.Policy == p.Name && t.PolicyUID == string(p.UID) && t.Deployment == p.Spec.TargetRef.Name {
			matched = true
		}
	}
	if !matched {
		return
	}
	a.stopped[string(p.UID)] = stoppedExhibitionTarget{p.Generation, p.Status.IncidentSequence}
	if a.current != nil && a.current.permit.Grant.Scope.PolicyUID == string(p.UID) && p.Generation > a.current.permit.Grant.Scope.PolicyGeneration {
		a.current.retired = true
	}
}
func (a *ExhibitionAuthority) Authorize(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	permit, hash, err := a.decode(p)
	if err != nil {
		// Removing or corrupting the active signed grant revokes this run.
		if a.current != nil && a.current.permit.Grant.Scope.PolicyUID == string(p.UID) {
			a.current.authority.closed = "signed permit revoked or invalid"
		}
		return err
	}
	grant, err := decodeExecutionGrant(p)
	if err != nil {
		if a.current != nil && a.current.hash == hash {
			a.current.authority.closed = "execution grant revoked or invalid"
		}
		return err
	}
	if grant.ExecutionGrantCore != permit.Grant {
		if a.current != nil && a.current.hash == hash {
			a.current.authority.closed = "execution grant core changed"
		}
		return errors.New("exhibition permit: grant core changed")
	}
	if p.Spec.Budget.MaxActionsPerIncident != permit.MaxActions {
		return errors.New("exhibition permit: policy action budget differs")
	}
	if a.current == nil || hash != a.current.hash {
		if permit.Ordinal <= a.highest || a.used[permit.AuthorizationID] || a.used[permit.Grant.Scope.RunID] || a.used[permit.Grant.Scope.Epoch] {
			return errors.New("exhibition permit: consumed authority/run/epoch")
		}
		if a.current != nil && !a.current.retired {
			return errors.New("exhibition permit: preceding run stop unconfirmed")
		}
		stop, ok := a.stopped[string(p.UID)]
		if !ok || stop.generation >= p.Generation || stop.sequence != permit.BaseIncidentSequence || p.Status.IncidentSequence != permit.BaseIncidentSequence || p.Status.ActiveIncident != nil || inc != nil {
			return errors.New("exhibition permit: stopped baseline sequence unconfirmed")
		}
		child := &ExecutionAuthority{scope: permit.Grant.Scope, instance: a.instance, sample: a.sample, baseIncidentSequence: permit.BaseIncidentSequence}
		// Consume the independently verified permit before its first grant check.
		// A stale first heartbeat cannot later revive the same authorization.
		a.current = &activeExhibition{permit: permit, hash: hash, authority: child}
		a.highest = permit.Ordinal
		a.used[permit.AuthorizationID] = true
		a.used[permit.Grant.Scope.RunID] = true
		a.used[permit.Grant.Scope.Epoch] = true
	} else if a.current.retired {
		return errors.New("exhibition permit: retired grant")
	}
	if p.Status.IncidentSequence < permit.BaseIncidentSequence || p.Status.IncidentSequence > permit.BaseIncidentSequence+1 {
		return errors.New("exhibition permit: incident count outside run budget")
	}
	if inc != nil {
		if permit.Schema == "opsloop-exhibition-permit/v3" {
			if !a.currentOccurrenceMatches(inc) {
				return errors.New("reproduction current occurrence changed")
			}
		} else if permit.CircuitReplay != nil && inc.Spec.Trigger.Signature != permit.CircuitReplay.Signature {
			return errors.New("circuit replay incident signature changed")
		}
		if inc.Spec.Sequence != permit.BaseIncidentSequence+1 || inc.Annotations[ExhibitionRunAnnotation] != permit.Grant.Scope.RunID || inc.Annotations[ExhibitionEpochAnnotation] != permit.Grant.Scope.Epoch || inc.Annotations[ExhibitionPermitBindingAnnotation] != hash || len(inc.Status.Runtime.Attempts) > int(permit.MaxActions) {
			return errors.New("exhibition permit: incident run/budget mismatch")
		}
	}
	return a.current.authority.Authorize(p, inc)
}
func validateCircuitReplay(p ExhibitionPermit) error {
	x := p.CircuitReplay
	if x == nil {
		if len(p.AllowedCircuitSignatures) != 0 {
			return errors.New("explicit circuit replay approval required")
		}
		return nil
	}
	if x.Purpose != "ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION" || len(p.AllowedCircuitSignatures) != 1 || p.AllowedCircuitSignatures[0] != x.Signature || (p.Schema != "opsloop-exhibition-permit/v2" && p.Schema != "opsloop-exhibition-permit/v3" && x.CompletionSHA256 != p.PreviousCompletionSHA256) {
		return errors.New("circuit replay scope mismatch")
	}
	for _, v := range []string{x.Signature, x.IncidentSHA256, x.BundleSHA256, x.CompletionSHA256, x.ApprovalSHA256} {
		if len(v) != 64 {
			return errors.New("circuit replay evidence hash invalid")
		}
		if _, err := hex.DecodeString(v); err != nil {
			return errors.New("circuit replay evidence hash invalid")
		}
	}
	for _, v := range []string{x.PriorRunID, x.PriorEpoch, x.PriorIncidentUID} {
		if len(v) == 0 || len(v) > 253 {
			return errors.New("circuit replay previous identity absent")
		}
	}
	if x.PriorRunID == p.Grant.Scope.RunID || x.PriorEpoch == p.Grant.Scope.Epoch {
		return errors.New("circuit replay old run")
	}
	parts := strings.Split(x.WorkloadImage, "@sha256:")
	if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 || strings.ContainsAny(x.WorkloadImage, " \n\r\t") {
		return errors.New("circuit replay image not immutable")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return errors.New("circuit replay image digest invalid")
	}
	return nil
}
func replayImageMatches(p ExhibitionPermit, d *appsv1.Deployment) bool {
	if p.CircuitReplay == nil {
		return true
	}
	return d != nil && string(d.UID) == p.Grant.Scope.DeploymentUID && d.Name == p.Grant.Scope.Deployment && d.Namespace == p.Grant.Scope.Namespace && d.DeletionTimestamp == nil && len(d.Spec.Template.Spec.Containers) == 1 && len(d.Spec.Template.Spec.InitContainers) == 0 && d.Spec.Template.Spec.Containers[0].Image == p.CircuitReplay.WorkloadImage
}
func (a *ExhibitionAuthority) ValidateReplayWorkload(p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	permit, hash, err := a.decode(p)
	if err != nil {
		return err
	}
	if permit.CircuitReplay == nil {
		return nil
	}
	if a.current == nil || a.current.hash != hash || a.current.retired || !replayImageMatches(permit, d) {
		if a.current != nil {
			a.current.authority.closed = "circuit replay workload changed"
		}
		return errors.New("circuit replay workload changed")
	}
	return nil
}
func (a *ExhibitionAuthority) ValidateReplayTrigger(p *v1alpha1.RecoveryPolicy, signature string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	permit, _, err := a.decode(p)
	if err != nil {
		return err
	}
	if permit.Schema == "opsloop-exhibition-permit/v3" {
		if a.current == nil || a.current.occurrence == nil || a.current.occurrence.Signature != signature || a.current.retired || a.current.authority.closed != "" {
			return errors.New("current reproduction occurrence required")
		}
		return nil
	}
	if permit.CircuitReplay != nil && permit.CircuitReplay.Signature != signature {
		return errors.New("circuit replay trigger outside approved signature")
	}
	return nil
}
func (a *ExhibitionAuthority) AllowsCircuit(p *v1alpha1.RecoveryPolicy, signature string, workload ...*appsv1.Deployment) bool {
	// Do not let a stale cached boolean outlive heartbeat/expiry/revocation checks.
	if a.Authorize(p, nil) != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current != nil && !a.current.retired && a.current.authority.closed == "" && a.current.permit.CircuitReplay != nil && len(workload) == 1 && replayImageMatches(a.current.permit, workload[0]) && a.current.permit.Grant.Scope.PolicyUID == string(p.UID) && a.current.permit.Grant.Scope.PolicyGeneration == p.Generation && p.Status.IncidentSequence == a.current.permit.BaseIncidentSequence && (a.current.permit.CircuitReplay.Signature == signature && a.current.permit.Schema != "opsloop-exhibition-permit/v3" || a.current.permit.Schema == "opsloop-exhibition-permit/v3" && a.current.occurrence != nil && a.current.occurrence.Signature == signature)
}

func (a *ExhibitionAuthority) ResourceConditions(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) (*candidates.ResourceConditions, error) {
	if err := a.Authorize(p, inc); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Return an independent copy; candidate evaluation cannot mutate authority.
	raw, _ := json.Marshal(a.current.permit.ResourceConditions)
	var c *candidates.ResourceConditions
	_ = json.Unmarshal(raw, &c)
	return c, nil
}
func (a *ExhibitionAuthority) AnnotateIncident(p *v1alpha1.RecoveryPolicy, inc *v1alpha1.RecoveryIncident) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || a.current.retired || a.current.permit.Grant.Scope.PolicyUID != string(p.UID) || inc.Name != a.current.permit.Grant.Scope.Incident || inc.Spec.Sequence != a.current.permit.BaseIncidentSequence+1 {
		return errors.New("exhibition incident: no current permit")
	}
	if inc.Annotations == nil {
		inc.Annotations = map[string]string{}
	}
	inc.Annotations[ExhibitionRunAnnotation] = a.current.permit.Grant.Scope.RunID
	inc.Annotations[ExhibitionEpochAnnotation] = a.current.permit.Grant.Scope.Epoch
	inc.Annotations[ExhibitionPermitBindingAnnotation] = a.current.hash
	if a.current.permit.Schema == "opsloop-exhibition-permit/v3" {
		if a.current.occurrence == nil || a.current.occurrence.Signature != inc.Spec.Trigger.Signature {
			return errors.New("current occurrence not bound")
		}
		raw, _ := json.Marshal(a.current.occurrence)
		inc.Annotations[CurrentOccurrenceAnnotation] = string(raw)
	}
	return nil
}

// v2 explicitly distinguishes read-only circuit provenance from the consumed
// immediate predecessor. No v1 field is inferred or upgraded into this contract.
func evidenceProjectionHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var sorted map[string]any
	if json.Unmarshal(raw, &sorted) != nil {
		return ""
	}
	raw, err = json.Marshal(sorted)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func validateEvidenceRoles(p ExhibitionPermit) error {
	c, prev, auth := p.CircuitEvidence, p.PredecessorCompletion, p.NewExecutionAuthorization
	if p.Schema == "opsloop-exhibition-permit/v1" {
		if c != nil || prev != nil || auth != nil || p.ApprovedReproduction != nil {
			return errors.New("v1 evidence roles forbidden")
		}
		return nil
	}
	if (p.Schema != "opsloop-exhibition-permit/v2" && p.Schema != "opsloop-exhibition-permit/v3") || c == nil || prev == nil || auth == nil || p.CircuitReplay == nil {
		return errors.New("v2 evidence roles required")
	}
	r, s := p.CircuitReplay, p.Grant.Scope
	if c.WorkloadImage != r.WorkloadImage || c.Signature != r.Signature || c.PriorRunID != r.PriorRunID || c.PriorEpoch != r.PriorEpoch || c.PriorIncidentUID != r.PriorIncidentUID || c.IncidentSHA256 != r.IncidentSHA256 || c.BundleSHA256 != r.BundleSHA256 || c.CompletionSHA256 != r.CompletionSHA256 {
		return errors.New("circuit evidence projection changed")
	}
	if c.PolicyUID != s.PolicyUID || c.DeploymentUID != s.DeploymentUID || prev.PolicyUID != s.PolicyUID || prev.DeploymentUID != s.DeploymentUID {
		return errors.New("evidence role target changed")
	}
	if prev.RunID == "" || len(prev.RunID) > 253 || prev.Epoch == "" || len(prev.Epoch) > 253 || prev.RunID == s.RunID || prev.Epoch == s.Epoch {
		return errors.New("predecessor identity invalid")
	}
	for _, h := range []string{prev.CompletionSHA256, prev.RetirementSHA256} {
		if len(h) != 64 {
			return errors.New("predecessor hash invalid")
		}
		if _, err := hex.DecodeString(h); err != nil {
			return errors.New("predecessor hash invalid")
		}
	}
	if prev.CompletionSHA256 != p.PreviousCompletionSHA256 || auth.AuthorizationID != p.AuthorizationID || auth.ApprovalSHA256 != r.ApprovalSHA256 || auth.CircuitEvidenceSHA256 != evidenceProjectionHash(c) || auth.PredecessorCompletionSHA256 != evidenceProjectionHash(prev) {
		return errors.New("new execution authorization binding changed")
	}
	if p.Schema == "opsloop-exhibition-permit/v3" {
		if err := validateReproductionScope(p); err != nil {
			return err
		}
	} else if p.ApprovedReproduction != nil || auth.ApprovedReproductionSHA256 != "" {
		return errors.New("reproduction contract version required")
	}
	return nil
}
