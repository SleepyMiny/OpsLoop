/**
 * Wire types for the Go CRDs `opsloop.io/v1alpha1` (api/v1alpha1 at commit
 * f47c7d1 on the backend branch). Only the fields the dashboard reads are listed;
 * json tags are copied verbatim. Times are RFC 3339 strings (metav1.Time),
 * durations are Go duration strings (metav1.Duration), memory is a
 * resource.Quantity string in the policy and int64 bytes in measurements, ratios
 * are permille and scores milli-points.
 */

export interface CrdObjectMeta {
  name: string;
  namespace?: string;
  uid?: string;
  generation?: number;
  creationTimestamp?: string;
}

// --- RecoveryPolicy --------------------------------------------------------

export interface CrdLabelSelector {
  matchLabels?: Record<string, string>;
  matchExpressions?: Array<{ key: string; operator: string; values?: string[] }>;
}

export interface CrdRecoveryPolicySpec {
  targetRef: { name: string };
  enabled: boolean;
  mode: 'ObserveOnly' | 'Enforce';
  candidateSelector: CrdLabelSelector;
  requiredPlatforms: string[];
  detection: { nodeUnreadyFor: string; restartThreshold: number; restartWindow: string };
  filter: {
    maxCPUUtilizationPercent: number;
    maxMemoryUtilizationPercent: number;
    minimumAvailableMemory: string;
    startupMemoryBudget: string;
    requestedCapacityReservePercent: number;
    maxSampleAge: string;
  };
  score: {
    cpuWeight: number;
    memoryWeight: number;
    requestsWeight: number;
    recentFailurePenalty: number;
    maxRecentFailurePenalty: number;
    recentFailureWindow: string;
  };
  budget: {
    maxActionsPerIncident: number;
    cooldown: string;
    placementTimeout: string;
    verificationTimeout: string;
    incidentDeadline: string;
  };
  verification: { serviceName?: string; port?: number; consecutiveSuccesses: number; stabilityWindow: string };
}

export interface CrdNodeFailure {
  actionID: string;
  nodeName: string;
  nodeUID: string;
  at: string;
  reason: string;
  incident: string;
}

export interface CrdRecoveryPolicy {
  apiVersion?: string;
  kind: 'RecoveryPolicy';
  metadata: CrdObjectMeta;
  spec: CrdRecoveryPolicySpec;
  status?: {
    observedGeneration?: number;
    incidentSequence?: number;
    activeIncident?: { name: string; sequence: number };
    recentFailures?: CrdNodeFailure[];
    circuits?: Array<{ signature: string; class: string; incident: string; phase: string; openedAt: string }>;
  };
}

// --- RecoveryIncident ------------------------------------------------------

export interface CrdTrigger {
  class: string;
  reason: string;
  signature: string;
  ruleVersion: string;
  nodeName?: string;
  nodeUID?: string;
  podName?: string;
  podUID?: string;
  observedAt: string;
  evidence?: string[];
}

export interface CrdMeasurements {
  cpuUtilizationPermille?: number;
  cpuSampleCount?: number;
  cpuSampleTime?: string;
  memAvailableBytes?: number;
  memTotalBytes?: number;
  memSampleTime?: string;
  allocatableCPUMilli: number;
  allocatableMemoryBytes: number;
  requestedCPUMilli: number;
  requestedMemoryBytes: number;
  newPodCPUMilli: number;
  newPodMemoryBytes: number;
  reserveCPUMilli: number;
  reserveMemoryBytes: number;
}

export interface CrdCandidate {
  nodeName: string;
  nodeUID: string;
  hostname: string;
  platform: string;
  eligible: boolean;
  rejectReasons?: string[];
  details?: string[];
  measurements: CrdMeasurements;
  score?: {
    cpuHeadroomPermille: number;
    memoryHeadroomPermille: number;
    requestedHeadroomPermille: number;
    recentFailures: number;
    penaltyMilli: number;
    scoreMilli: number;
  };
}

export interface CrdDecision {
  incidentID: string;
  actionID?: string;
  policyGeneration: number;
  policyHash: string;
  workloadUID: string;
  baselineTemplateHash: string;
  imageIndexDigest: string;
  queryVersion: string;
  snapshotObservedAt: string;
  snapshotExpiresAt: string;
  candidates?: CrdCandidate[];
  selectedNodeUID?: string;
  selectedNodeName?: string;
  selectedHostname?: string;
  actionType: 'Relocation' | 'None';
  decisionReason: 'SELECTED_HIGHEST_SCORE' | 'NO_CANDIDATE' | 'METRICS_UNAVAILABLE';
  decidedAt: string;
}

export type CrdAttemptOutcome = 'Reserved' | 'Patched' | 'Succeeded' | 'Failed' | 'Superseded' | 'Cancelled';

export interface CrdAttempt {
  actionID: string;
  outcome: CrdAttemptOutcome;
  failureReason?: string;
  nodeName: string;
  nodeUID: string;
  hostname: string;
  decision: CrdDecision;
  reservedAt: string;
  patchedAt?: string;
  podReadyAt?: string;
  firstHealthyAt?: string;
  completedAt?: string;
  podName?: string;
  podUID?: string;
  podNodeName?: string;
  consecutiveSuccesses?: number;
  endpointReady?: boolean;
  lastProbeError?: string;
}

export type CrdRuntimePhase =
  | ''
  | 'WAITING'
  | 'EXECUTING'
  | 'VERIFYING'
  | 'RECOVERED'
  | 'FAILED'
  | 'ABORTED'
  | 'SUPERSEDED'
  | 'UNSUPPORTED_WORKLOAD'
  | 'MANUAL_REQUIRED'
  | 'OBSERVED';

export interface CrdRecoveryIncident {
  apiVersion?: string;
  kind: 'RecoveryIncident';
  metadata: CrdObjectMeta;
  spec: {
    policyRef: { name: string; uid: string };
    sequence: number;
    policyGeneration: number;
    policyHash: string;
    policy: CrdRecoveryPolicySpec;
    workload: { name: string; uid: string };
    trigger: CrdTrigger;
    baseline: { templateHash: string; specHash: string; image: string; imageDigest: string; replicas: number; hostname?: string };
    detectedAt: string;
    deadline: string;
  };
  status?: {
    runtime?: {
      phase?: CrdRuntimePhase;
      reason?: string;
      message?: string;
      lastTransitionTime?: string;
      completedAt?: string;
      decision?: CrdDecision;
      attempts?: CrdAttempt[];
      oldPodsOutstanding?: Array<{ name: string; uid: string; nodeName: string; terminating: boolean }>;
    };
  };
}

/**
 * Demo Driver measurement joined by the BFF (§18). The CRD deliberately does not
 * carry fault-injection time; without this the UI labels T_detect/T_recover as
 * measured from the API signal.
 */
export interface DriverReport {
  incident: string;
  faultInjectedAt?: string;
}
