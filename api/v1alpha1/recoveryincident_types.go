package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RecoveryIncident is named "<policy>-<first 8 chars of policy UID>-<%05d>" and labeled
// opsloop.io/policy=<policy name>. It has no ownerReference so evidence survives Policy deletion.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.runtime.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.runtime.reason`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.trigger.class`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.runtime.decision.selectedNodeName`
type RecoveryIncident struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RecoveryIncidentSpec   `json:"spec"`
	Status RecoveryIncidentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RecoveryIncidentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RecoveryIncident `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type RecoveryIncidentSpec struct {
	PolicyRef        PolicyRef `json:"policyRef"`
	Sequence         int64     `json:"sequence"`
	PolicyGeneration int64     `json:"policyGeneration"`
	// PolicyHash(policy.spec).
	PolicyHash string `json:"policyHash"`
	// Policy spec snapshot taken at detection.
	Policy     RecoveryPolicySpec `json:"policy"`
	Workload   WorkloadRef        `json:"workload"`
	Trigger    Trigger            `json:"trigger"`
	Baseline   Baseline           `json:"baseline"`
	DetectedAt metav1.Time        `json:"detectedAt"`
	// detectedAt + incidentDeadline; never extended.
	Deadline metav1.Time `json:"deadline"`
}

type PolicyRef struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type WorkloadRef struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type Trigger struct {
	// +kubebuilder:validation:Enum=INFRASTRUCTURE_FAILURE;RESOURCE_FAILURE;CONFIGURATION_FAILURE;APPLICATION_FAILURE;UNKNOWN
	Class       string      `json:"class"`
	Reason      string      `json:"reason"`
	Signature   string      `json:"signature"`
	RuleVersion string      `json:"ruleVersion"`
	NodeName    string      `json:"nodeName,omitempty"`
	NodeUID     string      `json:"nodeUID,omitempty"`
	PodName     string      `json:"podName,omitempty"`
	PodUID      string      `json:"podUID,omitempty"`
	ObservedAt  metav1.Time `json:"observedAt"`
	// Every matched rule as "RULE pod=... node=..."; never log bodies.
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=256
	Evidence []string `json:"evidence,omitempty"`
}

type Baseline struct {
	// TemplateHash(deployment.spec.template).
	TemplateHash string `json:"templateHash"`
	// SpecHash(deployment.spec): desired state outside the template.
	SpecHash    string `json:"specHash"`
	Image       string `json:"image"`
	ImageDigest string `json:"imageDigest"`
	// Recorded only; change detection uses SpecHash.
	Replicas int32  `json:"replicas"`
	Hostname string `json:"hostname,omitempty"`
}

type RecoveryIncidentStatus struct {
	Runtime RuntimeStatus `json:"runtime,omitempty"`
}

type RuntimeStatus struct {
	// WorkloadDispatchActionID is write-ahead evidence for the same Incident UID
	// and reserved action. It must persist before dispatch; it grants no authority.
	// An unconfirmed dispatch resumes through a hold, never by replaying the write.
	// +kubebuilder:validation:MaxLength=128
	WorkloadDispatchActionID string `json:"workloadDispatchActionID,omitempty"`

	// SafetyHaltReason persists an unfinished hold/settlement. It grants no write
	// permission; only the live Deployment fence is execution authority.
	// +kubebuilder:validation:MaxLength=256
	SafetyHaltReason string `json:"safetyHaltReason,omitempty"`

	// Empty means DETECTED.
	// +kubebuilder:validation:Enum=WAITING;EXECUTING;VERIFYING;RECOVERED;FAILED;ABORTED;SUPERSEDED;UNSUPPORTED_WORKLOAD;MANUAL_REQUIRED;OBSERVED
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	Message            string       `json:"message,omitempty"`
	LastTransitionTime metav1.Time  `json:"lastTransitionTime,omitempty"`
	CompletedAt        *metav1.Time `json:"completedAt,omitempty"`
	// Last applied template hash; empty means nothing applied, so read spec.baseline.templateHash.
	ExpectedTemplateHash string            `json:"expectedTemplateHash,omitempty"`
	Decision             *RecoveryDecision `json:"decision,omitempty"`
	// +kubebuilder:validation:MaxItems=5
	Attempts []Attempt `json:"attempts,omitempty"`
	// +kubebuilder:validation:MaxItems=5
	OldPodsOutstanding []PodRef `json:"oldPodsOutstanding,omitempty"`
}

type RecoveryDecision struct {
	IncidentID string `json:"incidentID"`
	// "<incident>-a<n>"; empty for ObserveOnly and WAITING.
	ActionID             string      `json:"actionID,omitempty"`
	PolicyGeneration     int64       `json:"policyGeneration"`
	PolicyHash           string      `json:"policyHash"`
	WorkloadUID          string      `json:"workloadUID"`
	BaselineTemplateHash string      `json:"baselineTemplateHash"`
	ImageIndexDigest     string      `json:"imageIndexDigest"`
	QueryVersion         string      `json:"queryVersion"`
	SnapshotObservedAt   metav1.Time `json:"snapshotObservedAt"`
	SnapshotExpiresAt    metav1.Time `json:"snapshotExpiresAt"`
	// Optional so a decision with zero candidates (nil slice, null) is not rejected as missing.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Candidates       []CandidateEvaluation `json:"candidates"`
	SelectedNodeUID  string                `json:"selectedNodeUID,omitempty"`
	SelectedNodeName string                `json:"selectedNodeName,omitempty"`
	// Actual kubernetes.io/hostname label value of the selected Node.
	SelectedHostname string `json:"selectedHostname,omitempty"`
	// +kubebuilder:validation:Enum=Relocation;None
	ActionType string `json:"actionType"`
	// +kubebuilder:validation:Enum=SELECTED_HIGHEST_SCORE;NO_CANDIDATE;METRICS_UNAVAILABLE
	DecisionReason string      `json:"decisionReason"`
	DecidedAt      metav1.Time `json:"decidedAt"`
}

type CandidateEvaluation struct {
	NodeName string `json:"nodeName"`
	NodeUID  string `json:"nodeUID"`
	Hostname string `json:"hostname"`
	// "<kubernetes.io/os>/<kubernetes.io/arch>".
	Platform string `json:"platform"`
	Eligible bool   `json:"eligible"`
	// +kubebuilder:validation:MaxItems=15
	// +kubebuilder:validation:items:Enum=NODE_NOT_READY;UNSCHEDULABLE;UNTOLERATED_TAINT;MEMORY_PRESSURE;DISK_PRESSURE;PID_PRESSURE;CPU_PRESSURE;ARCH_MISMATCH;INSUFFICIENT_REQUESTED_CPU;INSUFFICIENT_REQUESTED_MEMORY;INSUFFICIENT_AVAILABLE_MEMORY;METRICS_MISSING;METRICS_STALE;COOLDOWN;UNSUPPORTED_CONSTRAINT
	RejectReasons []string `json:"rejectReasons,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	Details      []string     `json:"details,omitempty"`
	Measurements Measurements `json:"measurements"`
	// Set only when eligible.
	Score *ScoreBreakdown `json:"score,omitempty"`
}

// Measurements uses nil pointers for "not measured"; never fill them with zero.
type Measurements struct {
	CPUUtilizationPermille *int32 `json:"cpuUtilizationPermille,omitempty"`
	CPUSampleCount         *int32 `json:"cpuSampleCount,omitempty"`
	// Oldest raw sample timestamp.
	CPUSampleTime          *metav1.Time `json:"cpuSampleTime,omitempty"`
	MemAvailableBytes      *int64       `json:"memAvailableBytes,omitempty"`
	MemTotalBytes          *int64       `json:"memTotalBytes,omitempty"`
	MemSampleTime          *metav1.Time `json:"memSampleTime,omitempty"`
	AllocatableCPUMilli    int64        `json:"allocatableCPUMilli"`
	AllocatableMemoryBytes int64        `json:"allocatableMemoryBytes"`
	// Sum over existing non-terminated Pods.
	RequestedCPUMilli    int64 `json:"requestedCPUMilli"`
	RequestedMemoryBytes int64 `json:"requestedMemoryBytes"`
	NewPodCPUMilli       int64 `json:"newPodCPUMilli"`
	NewPodMemoryBytes    int64 `json:"newPodMemoryBytes"`
	ReserveCPUMilli      int64 `json:"reserveCPUMilli"`
	ReserveMemoryBytes   int64 `json:"reserveMemoryBytes"`
}

type ScoreBreakdown struct {
	CPUHeadroomPermille       int32 `json:"cpuHeadroomPermille"`
	MemoryHeadroomPermille    int32 `json:"memoryHeadroomPermille"`
	RequestedHeadroomPermille int32 `json:"requestedHeadroomPermille"`
	RecentFailures            int32 `json:"recentFailures"`
	PenaltyMilli              int64 `json:"penaltyMilli"`
	// 100 points = 100000.
	ScoreMilli int64 `json:"scoreMilli"`
}

type Attempt struct {
	// "<incident>-a<n>"; also the opsloop.io/attempt-id template annotation value.
	ActionID string `json:"actionID"`
	// Cancelled closes a reservation that never changed the Deployment.
	// +kubebuilder:validation:Enum=Reserved;Patched;Succeeded;Failed;Superseded;Cancelled
	Outcome       string `json:"outcome"`
	FailureReason string `json:"failureReason,omitempty"`
	NodeName      string `json:"nodeName"`
	NodeUID       string `json:"nodeUID"`
	Hostname      string `json:"hostname"`
	// Expected template hash right before the change.
	PreTemplateHash string `json:"preTemplateHash"`
	// TemplateHash(Mutate(copy of pre template)), computed at reservation.
	TargetTemplateHash string `json:"targetTemplateHash"`
	// Live resourceVersion sent with the Update.
	PreResourceVersion string           `json:"preResourceVersion,omitempty"`
	Decision           RecoveryDecision `json:"decision"`
	ReservedAt         metav1.Time      `json:"reservedAt"`
	PatchedAt          *metav1.Time     `json:"patchedAt,omitempty"`
	// Ready condition lastTransitionTime of the attempt Pod.
	PodReadyAt *metav1.Time `json:"podReadyAt,omitempty"`
	// Start of the current consecutive-success run.
	FirstHealthyAt         *metav1.Time `json:"firstHealthyAt,omitempty"`
	CompletedAt            *metav1.Time `json:"completedAt,omitempty"`
	ReplicaSetName         string       `json:"replicaSetName,omitempty"`
	ReplicaSetUID          string       `json:"replicaSetUID,omitempty"`
	PodName                string       `json:"podName,omitempty"`
	PodUID                 string       `json:"podUID,omitempty"`
	PodNodeName            string       `json:"podNodeName,omitempty"`
	PodRestartCountAtReady int32        `json:"podRestartCountAtReady,omitempty"`
	ConsecutiveSuccesses   int32        `json:"consecutiveSuccesses,omitempty"`
	EndpointReady          bool         `json:"endpointReady,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	LastProbeError string `json:"lastProbeError,omitempty"`
}

type PodRef struct {
	Name        string `json:"name"`
	UID         string `json:"uid"`
	NodeName    string `json:"nodeName"`
	Terminating bool   `json:"terminating"`
}

func init() {
	SchemeBuilder.Register(&RecoveryIncident{}, &RecoveryIncidentList{})
}
