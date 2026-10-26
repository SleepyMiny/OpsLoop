package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PolicyMode string

const (
	PolicyModeObserveOnly PolicyMode = "ObserveOnly"
	PolicyModeEnforce     PolicyMode = "Enforce"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeIncident.name`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="name must be at most 40 characters"
type RecoveryPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RecoveryPolicySpec   `json:"spec"`
	Status RecoveryPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RecoveryPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RecoveryPolicy `json:"items"`
}

type RecoveryPolicySpec struct {
	// Immutable so an active Incident and its Policy always name the same Deployment.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"
	TargetRef TargetRef `json:"targetRef"`
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// +optional
	// +kubebuilder:validation:Enum=ObserveOnly;Enforce
	// +kubebuilder:default=ObserveOnly
	Mode              PolicyMode           `json:"mode"`
	CandidateSelector metav1.LabelSelector `json:"candidateSelector"`
	// +optional
	// +kubebuilder:default={"linux/amd64","linux/arm64"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=linux/amd64;linux/arm64
	RequiredPlatforms []string `json:"requiredPlatforms"`
	// +optional
	// +kubebuilder:default={nodeUnreadyFor:"20s",restartThreshold:3,restartWindow:"120s"}
	Detection DetectionSpec `json:"detection"`
	// +optional
	// +kubebuilder:default={maxCPUUtilizationPercent:85,maxMemoryUtilizationPercent:85,minimumAvailableMemory:"512Mi",startupMemoryBudget:"256Mi",requestedCapacityReservePercent:10,maxSampleAge:"30s"}
	Filter FilterSpec `json:"filter"`
	// +optional
	// +kubebuilder:default={cpuWeight:30,memoryWeight:35,requestsWeight:35,recentFailurePenalty:15,maxRecentFailurePenalty:45,recentFailureWindow:"30m"}
	Score ScoreSpec `json:"score"`
	// +optional
	// +kubebuilder:default={maxActionsPerIncident:2,cooldown:"120s",placementTimeout:"90s",verificationTimeout:"60s",incidentDeadline:"10m"}
	Budget       BudgetSpec       `json:"budget"`
	Verification VerificationSpec `json:"verification"`
}

type TargetRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

type DetectionSpec struct {
	// PodUnreadyFor opts into relocating a previously healthy, running Pod whose
	// readiness failed while its Node remains Ready. Nil preserves legacy rules.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('15s') && duration(self) <= duration('5m')"
	PodUnreadyFor *metav1.Duration `json:"podUnreadyFor,omitempty"`
	// +optional
	// +kubebuilder:default="20s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	NodeUnreadyFor metav1.Duration `json:"nodeUnreadyFor"`
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	RestartThreshold int32 `json:"restartThreshold"`
	// +optional
	// +kubebuilder:default="120s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	RestartWindow metav1.Duration `json:"restartWindow"`
}

type FilterSpec struct {
	// +optional
	// +kubebuilder:default=85
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxCPUUtilizationPercent int32 `json:"maxCPUUtilizationPercent"`
	// +optional
	// +kubebuilder:default=85
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxMemoryUtilizationPercent int32 `json:"maxMemoryUtilizationPercent"`
	// +optional
	// +kubebuilder:default="512Mi"
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).compareTo(quantity('0')) >= 0",message="must not be negative"
	MinimumAvailableMemory resource.Quantity `json:"minimumAvailableMemory"`
	// +optional
	// +kubebuilder:default="256Mi"
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).compareTo(quantity('0')) >= 0",message="must not be negative"
	StartupMemoryBudget resource.Quantity `json:"startupMemoryBudget"`
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	RequestedCapacityReservePercent int32 `json:"requestedCapacityReservePercent"`
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('10m')",message="maxSampleAge must be at most 10m"
	MaxSampleAge metav1.Duration `json:"maxSampleAge"`
}

// +kubebuilder:validation:XValidation:rule="self.cpuWeight + self.memoryWeight + self.requestsWeight == 100",message="weights must sum to 100"
type ScoreSpec struct {
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	CPUWeight int32 `json:"cpuWeight"`
	// +optional
	// +kubebuilder:default=35
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MemoryWeight int32 `json:"memoryWeight"`
	// +optional
	// +kubebuilder:default=35
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	RequestsWeight int32 `json:"requestsWeight"`
	// Points deducted per recent failure.
	// +optional
	// +kubebuilder:default=15
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	RecentFailurePenalty int32 `json:"recentFailurePenalty"`
	// +optional
	// +kubebuilder:default=45
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxRecentFailurePenalty int32 `json:"maxRecentFailurePenalty"`
	// +optional
	// +kubebuilder:default="30m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	RecentFailureWindow metav1.Duration `json:"recentFailureWindow"`
}

type BudgetSpec struct {
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	MaxActionsPerIncident int32 `json:"maxActionsPerIncident"`
	// +optional
	// +kubebuilder:default="120s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	Cooldown metav1.Duration `json:"cooldown"`
	// +optional
	// +kubebuilder:default="90s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	PlacementTimeout metav1.Duration `json:"placementTimeout"`
	// +optional
	// +kubebuilder:default="60s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	VerificationTimeout metav1.Duration `json:"verificationTimeout"`
	// +optional
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	IncidentDeadline metav1.Duration `json:"incidentDeadline"`
}

type VerificationSpec struct {
	// Service whose EndpointSlice must report the attempt Pod ready.
	// +kubebuilder:validation:MinLength=1
	ServiceName string `json:"serviceName"`
	// Pod port probed directly on the Pod IP.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	ConsecutiveSuccesses int32 `json:"consecutiveSuccesses"`
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')"
	StabilityWindow metav1.Duration `json:"stabilityWindow"`
}

type RecoveryPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Increased only by compare-and-swap status updates.
	IncidentSequence int64              `json:"incidentSequence,omitempty"`
	ActiveIncident   *ActiveIncidentRef `json:"activeIncident,omitempty"`
	// Cross-incident cooldown and penalty ledger, appended only on release.
	// +kubebuilder:validation:MaxItems=20
	RecentFailures []NodeFailure `json:"recentFailures,omitempty"`
	// Never removed automatically; a full list blocks new incidents.
	// +kubebuilder:validation:MaxItems=10
	Circuits []Circuit `json:"circuits,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ActiveIncidentRef struct {
	// "<policy>-<first 8 chars of policy UID>-<%05d sequence>".
	Name     string `json:"name"`
	Sequence int64  `json:"sequence"`
	// Set after creation is confirmed, so a crash before creation is distinguishable from a later deletion.
	IncidentUID string `json:"incidentUID,omitempty"`
	// Deployment UID observed when this reservation was made. Empty legacy pending reservations are discarded.
	WorkloadUID string      `json:"workloadUID,omitempty"`
	ReservedAt  metav1.Time `json:"reservedAt"`
	// Reservation intent used to recreate the same Incident after a restart.
	Trigger  Trigger  `json:"trigger"`
	Baseline Baseline `json:"baseline"`
}

type NodeFailure struct {
	// Upsert key so one failed attempt is never penalized twice.
	ActionID string      `json:"actionID"`
	NodeName string      `json:"nodeName"`
	NodeUID  string      `json:"nodeUID"`
	At       metav1.Time `json:"at"`
	Reason   string      `json:"reason"`
	Incident string      `json:"incident"`
}

type Circuit struct {
	Signature string `json:"signature"`
	Class     string `json:"class"`
	Incident  string `json:"incident"`
	// Terminal phase or INCIDENT_LOST.
	Phase    string      `json:"phase"`
	OpenedAt metav1.Time `json:"openedAt"`
}

func init() {
	SchemeBuilder.Register(&RecoveryPolicy{}, &RecoveryPolicyList{})
}
