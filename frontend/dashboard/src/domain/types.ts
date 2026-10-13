/**
 * OpsLoop dashboard domain contract.
 *
 * Field names follow docs/architecture/architecture-v0.1.md (§5 RecoveryDecision,
 * §7 Runtime state machine, §8 RecoveryPolicy, §9 CRDs, §13 Engineering state
 * machine) using Kubernetes-style camelCase JSON keys. The Go types in
 * api/v1alpha1 are not committed yet; when they are, their json tags win and this
 * file is aligned to them (see docs/live-data-contract.md).
 *
 * All timestamps are epoch milliseconds. Ratios are 0..1. Bytes and millicores are
 * raw integers so the UI decides formatting.
 */

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------

export type Arch = 'amd64' | 'arm64';
export type NodeRole = 'control-plane' | 'worker';
/** Physical placement of a node in the hybrid testbed (README "목표 Testbed"). */
export type NodeSite = 'laptop-vm' | 'physical-server' | 'azure-vm';
export type NodeReadiness = 'Ready' | 'NotReady' | 'Unknown';
export type PressureCondition = 'MemoryPressure' | 'DiskPressure' | 'PIDPressure';

/**
 * A single Prometheus-derived sample. `value === null` means missing/NaN and must
 * never be read as 0% utilization (architecture §11).
 */
export interface MetricSample {
  value: number | null;
  /** Original sample timestamp, not the query evaluation time. */
  observedAt: number;
}

export type TaintEffect = 'NoSchedule' | 'PreferNoSchedule' | 'NoExecute';

export interface Taint {
  key: string;
  value?: string;
  effect: TaintEffect;
}

export interface Toleration {
  key?: string;
  operator?: 'Equal' | 'Exists';
  value?: string;
  /** Omitted means the toleration matches every effect. */
  effect?: TaintEffect;
}

export interface ResourceQuantity {
  cpuMillicores: number;
  memoryBytes: number;
}

export type PodRole = 'demo-api' | 'opsloop' | 'observability' | 'system';
export type PodPhase = 'Pending' | 'Running' | 'Succeeded' | 'Failed' | 'Unknown';
export type PodReason =
  | 'ContainerCreating'
  | 'CrashLoopBackOff'
  | 'Error'
  | 'Terminating'
  | 'NodeLost'
  | 'ImagePullBackOff';

export interface PodSummary {
  uid: string;
  name: string;
  namespace: string;
  role: PodRole;
  phase: PodPhase;
  ready: boolean;
  restarts: number;
  reason?: PodReason;
  /** OpsLoop action that created this Pod's template (opsloop.io/attempt-id). */
  attemptId?: string;
}

export interface ClusterNode {
  uid: string;
  name: string;
  /** Actual kubernetes.io/hostname label value; may differ from `name` (§10). */
  hostnameLabel: string;
  displayName: string;
  site: NodeSite;
  arch: Arch;
  role: NodeRole;
  readiness: NodeReadiness;
  unschedulable: boolean;
  /** Node taints (Kubernetes API). Omitted means none. */
  taints?: Taint[];
  pressure: PressureCondition[];
  /** opsloop.io/recovery-candidate=true */
  recoveryCandidate: boolean;
  tailscaleIp?: string;
  allocatable: ResourceQuantity;
  /** Sum of non-terminated Pod requests on this node (Kubernetes API authority). */
  requested: ResourceQuantity;
  usage: {
    /** 0..1, 60s idle-rate based (Prometheus authority). */
    cpu: MetricSample;
    /** 0..1, 1 - MemAvailable/MemTotal. */
    memory: MetricSample;
    memAvailableBytes: MetricSample;
  };
  /** Recent scrape history (oldest first) for sparklines; optional for live sources. */
  usageHistory?: UsagePoint[];
  pods: PodSummary[];
}

export interface UsagePoint {
  at: number;
  cpu: number | null;
  memory: number | null;
}

export interface ClusterState {
  name: string;
  k3sVersion: string;
  nodes: ClusterNode[];
}

// ---------------------------------------------------------------------------
// Workload & HTTP verification
// ---------------------------------------------------------------------------

export interface ImagePlatformManifest {
  platform: `linux/${Arch}`;
  digest: string;
}

export interface WorkloadStatus {
  uid: string;
  namespace: string;
  name: string;
  replicas: number;
  /** Current Pod template nodeSelector[kubernetes.io/hostname], if OpsLoop set one. */
  nodeSelectorHostname: string | null;
  imageIndexDigest: string;
  platforms: ImagePlatformManifest[];
  requests: ResourceQuantity;
  tolerations?: Toleration[];
  /**
   * Scheduling constraints OpsLoop does not model (e.g. podAntiAffinity,
   * topologySpreadConstraints). Any entry rejects every candidate (§8).
   */
  unsupportedConstraints?: string[];
}

export interface HttpProbe {
  at: number;
  ok: boolean;
  /** Node that served the response (Demo API metadata), when it answered. */
  servedBy?: string;
  arch?: Arch;
  latencyMs?: number;
}

export interface HttpAvailability {
  /** Fixed Service path probed by the Demo Driver from the Laptop Host. */
  endpoint: string;
  probes: HttpProbe[];
}

// ---------------------------------------------------------------------------
// RecoveryPolicy (§8)
// ---------------------------------------------------------------------------

export type PolicyMode = 'ObserveOnly' | 'Enforce';

export interface RecoveryPolicySpec {
  targetRef: { kind: 'Deployment'; name: string };
  enabled: boolean;
  mode: PolicyMode;
  candidateSelector: string;
  requiredPlatforms: Array<`linux/${Arch}`>;
  detection: {
    nodeUnreadyForSeconds: number;
    restartThreshold: number;
    restartWindowSeconds: number;
  };
  filter: {
    maxCPUUtilization: number;
    maxMemoryUtilization: number;
    minimumAvailableMemoryBytes: number;
    startupMemoryBudgetBytes: number;
    /** Fraction of allocatable kept free, e.g. 0.10. */
    requestedCapacityReserve: number;
    maxSampleAgeSeconds: number;
  };
  score: {
    cpuWeight: number;
    memoryWeight: number;
    requestsWeight: number;
    recentFailurePenalty: number;
    /**
     * Upper bound and history window for the penalty. §8 requires both to be
     * explicit but leaves the values open; the demo policy picks its own. Not in
     * the Go CRD yet: when absent, there is no cap and every failure counts.
     */
    recentFailurePenaltyCap?: number;
    failureHistoryWindowSeconds?: number;
  };
  budget: {
    maxActionsPerIncident: number;
    cooldownSeconds: number;
    placementTimeoutSeconds: number;
    verificationTimeoutSeconds: number;
    incidentDeadlineSeconds: number;
  };
  verification: {
    consecutiveSuccesses: number;
    stabilityWindowSeconds: number;
  };
  onApplicationExhausted: 'QuarantineScaleZero' | 'None';
  engineering: { enabled: boolean; profileRef: string };
  sameSignatureCircuit: boolean;
}

/** Cooldown applies to a failed workload–node pair (§8). */
export interface CooldownEntry {
  workloadUid: string;
  nodeName: string;
  until: number;
}

export interface RecoveryPolicyStatus {
  observedGeneration: number;
  activeIncidentRef: string | null;
  incidentSequence: number;
  cooldowns: CooldownEntry[];
  /** Policy-wide failed actions (Go status.recentFailures) for the score penalty. */
  recentFailures?: Array<{ nodeName: string; at: number }>;
}

export interface RecoveryPolicy {
  name: string;
  namespace: string;
  generation: number;
  policyHash: string;
  spec: RecoveryPolicySpec;
  status: RecoveryPolicyStatus;
}

// ---------------------------------------------------------------------------
// Classification & candidate evaluation (§6, §8)
// ---------------------------------------------------------------------------

export type FailureClass =
  | 'INFRASTRUCTURE_FAILURE'
  | 'RESOURCE_FAILURE'
  | 'CONFIGURATION_FAILURE'
  | 'APPLICATION_FAILURE'
  | 'UNKNOWN';

export const REJECT_REASONS = [
  'NODE_NOT_READY',
  'UNSCHEDULABLE',
  'ARCH_MISMATCH',
  'UNTOLERATED_TAINT',
  'MEMORY_PRESSURE',
  'CPU_PRESSURE',
  'DISK_PRESSURE',
  'PID_PRESSURE',
  'INSUFFICIENT_REQUESTED_CPU',
  'INSUFFICIENT_REQUESTED_MEMORY',
  'INSUFFICIENT_AVAILABLE_MEMORY',
  'METRICS_STALE',
  'METRICS_MISSING',
  'COOLDOWN',
  'UNSUPPORTED_CONSTRAINT',
] as const;
export type RejectReason = (typeof REJECT_REASONS)[number];

/** Dashboard-only marker for the node that failed; it is excluded, not scored. */
export type CandidateExclusion = 'FAILED_NODE' | 'NOT_A_CANDIDATE';

export interface CandidateMeasurements {
  cpuUtilization: number | null;
  memoryUtilization: number | null;
  memAvailableBytes: number | null;
  /** Requested headroom after adding the new Pod and the policy reserve. */
  requestedCpuHeadroom: number;
  requestedMemoryHeadroom: number;
  /** Age of the oldest sample used, seconds. null when missing. */
  sampleAgeSeconds: number | null;
}

export interface ScoreBreakdown {
  cpuHeadroom: number;
  memoryHeadroom: number;
  requestedHeadroom: number;
  /** Weighted contributions in points (0..100 scale before penalty). */
  weighted: { cpu: number; memory: number; requests: number };
  penalty: number;
  total: number;
}

export interface CandidateEvaluation {
  nodeUid: string;
  nodeName: string;
  hostname: string;
  arch: Arch;
  measurements: CandidateMeasurements;
  rejectReasons: RejectReason[];
  exclusion?: CandidateExclusion;
  /** Present only when the candidate passed every hard filter. */
  score?: ScoreBreakdown;
}

export type ActionType = 'relocation' | 'restart';

export interface RecoveryDecision {
  incidentId: string;
  actionId: string;
  policyGeneration: number;
  policyHash: string;
  workloadUid: string;
  baselineTemplateHash: string;
  imageIndexDigest: string;
  snapshotObservedAt: number;
  snapshotExpiresAt: number;
  candidates: CandidateEvaluation[];
  /**
   * Node the workload was running on when the decision was made (baseline state,
   * architecture §7 "실행 일관성" 1). Not in the §5 field list yet; proposed for the
   * Go contract so a route origin survives the old Pod being deleted.
   */
  sourceNodeName: string | null;
  selectedNodeUid: string | null;
  selectedHostname: string | null;
  actionType: ActionType | null;
  decisionReason: string;
}

// ---------------------------------------------------------------------------
// RecoveryIncident (§7, §9, §13)
// ---------------------------------------------------------------------------

export const RUNTIME_PHASES = [
  'MONITORING',
  'DETECTED',
  'CLASSIFIED',
  'COLLECTING_CANDIDATES',
  'FILTERING',
  'WAITING',
  'SCORING',
  'TARGET_SELECTED',
  'EXECUTING',
  'VERIFYING',
  'RECOVERED',
  'COOLDOWN',
  'RUNTIME_RECOVERY_EXHAUSTED',
  'EVIDENCE_FINALIZING',
  'ENGINEERING_HANDOFF',
  'FAILED',
  'ABORTED',
  'SUPERSEDED',
  'UNSUPPORTED_WORKLOAD',
  'MANUAL_REQUIRED',
  /** ObserveOnly terminal state in the Go CRD: decided, never acted. */
  'OBSERVED',
] as const;
export type RuntimePhase = (typeof RUNTIME_PHASES)[number];

export type WaitingReason = 'NO_CANDIDATE' | 'METRICS_UNAVAILABLE';

export type ActionResult = 'pending' | 'placed' | 'verified' | 'failed' | 'timed-out';

export interface ActionAttempt {
  actionId: string;
  type: ActionType;
  targetNode: string;
  targetArch: Arch;
  startedAt: number;
  finishedAt?: number;
  result: ActionResult;
  podUid?: string;
  /** Consecutive HTTP successes observed for this attempt's Pod. */
  httpSuccesses: number;
  failureReason?: string;
}

export type QuarantineState = 'QuarantineRequested' | 'QuarantineConfirmed' | 'QuarantineUnconfirmed';

export interface RuntimeTimings {
  /**
   * Fault injection time from the Demo Driver on the Laptop Host clock (§18
   * measurement start). Not part of the CRD; a live BFF fills it only when it can
   * join Demo Driver records, otherwise T_detect/T_recover fall back to the API
   * signal and are labelled accordingly.
   */
  faultInjectedAt?: number;
  /** Trigger observed by the API (NotReady / restart threshold). */
  triggeredAt: number;
  detectedAt?: number;
  decidedAt?: number;
  placedAt?: number;
  recoveredAt?: number;
}

export interface IncidentRuntimeStatus {
  phase: RuntimePhase;
  classification?: FailureClass;
  waitingReason?: WaitingReason;
  /** Normalized failure signature hash for Application failures. */
  signature?: string;
  decision?: RecoveryDecision;
  attempts: ActionAttempt[];
  budget: { used: number; max: number };
  timings: RuntimeTimings;
  oldPodsOutstanding: boolean;
  quarantine?: QuarantineState;
  bundleRef?: string;
}

export const ENGINEERING_PHASES = [
  'ENGINEERING_QUEUED',
  'VALIDATING_INPUT',
  'ANALYZING',
  'PATCHING',
  'BUILDING',
  'TESTING',
  'VERIFYING',
  'INDEPENDENT_REVIEW',
  'PR_READY',
  'PR_CREATED',
  'FEEDBACK',
  'ENGINEERING_FAILED',
  'INVALID_BUNDLE',
  'BLOCKED_EVIDENCE',
  'BLOCKED_ENVIRONMENT',
  'BLOCKED_NATIVE_VALIDATION',
  'STALE_BASE',
  'TIMED_OUT',
  'CANCELLED',
  'PUBLISH_RETRY_PENDING',
] as const;
export type EngineeringPhase = (typeof ENGINEERING_PHASES)[number];

export type VerificationStage =
  | 'build'
  | 'unit'
  | 'integration'
  | 'regression-fixture'
  | 'multi-arch-smoke'
  | 'independent-review';

export interface StageResult {
  stage: VerificationStage;
  iteration: number;
  outcome: 'pass' | 'fail' | 'running';
  summary: string;
}

export interface IncidentEngineeringStatus {
  phase: EngineeringPhase;
  jobId: string;
  iteration: number;
  maxIterations: number;
  branch?: string;
  baseCommit: string;
  stages: StageResult[];
  /** Demo shows a placeholder; a real URL only exists after the Publisher ran. */
  pullRequest?: { number: number; title: string; url: string | null; headSha: string };
}

export interface IncidentCondition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason: string;
  message: string;
  lastTransitionTime: number;
}

export type IncidentEventKind =
  | 'trigger'
  | 'classify'
  | 'evaluate'
  | 'decide'
  | 'act'
  | 'verify'
  | 'recovered'
  | 'wait'
  | 'fail'
  | 'evidence'
  | 'quarantine'
  | 'engineering';

/** Bounded action history entry; never contains log bodies (§9). */
export interface IncidentEvent {
  at: number;
  kind: IncidentEventKind;
  phase: RuntimePhase | EngineeringPhase;
  message: string;
}

export type IncidentTrigger =
  | { type: 'NodeNotReady'; nodeName: string; observedAt: number }
  /** restarts is absent when the source records only that the threshold was reached. */
  | { type: 'RepeatedRestart'; podName: string; restarts?: number; observedAt: number };

export interface RecoveryIncident {
  name: string;
  namespace: string;
  createdAt: number;
  spec: {
    workloadUid: string;
    trigger: IncidentTrigger;
    policyRef: string;
    policyHash: string;
  };
  status: {
    runtime: IncidentRuntimeStatus;
    engineering?: IncidentEngineeringStatus;
    conditions: IncidentCondition[];
    events: IncidentEvent[];
  };
}

// ---------------------------------------------------------------------------
// Snapshot delivered to the UI
// ---------------------------------------------------------------------------

export type DataSourceKind = 'demo' | 'live';

export interface Narration {
  /** Short headline for visitors, plain Korean. */
  title: string;
  /** One or two sentences explaining what OpsLoop is doing and why. */
  body: string;
  /** Which concept this step showcases, used to highlight UI regions. */
  focus: 'cluster' | 'pipeline' | 'candidates' | 'incident' | 'policy' | 'engineering';
}

export interface DashboardSnapshot {
  source: DataSourceKind;
  /** Wall-clock (demo: simulated) time this snapshot represents. */
  observedAt: number;
  cluster: ClusterState;
  workload: WorkloadStatus;
  http: HttpAvailability;
  policy: RecoveryPolicy;
  activeIncident: RecoveryIncident | null;
  recentIncidents: RecoveryIncident[];
  /** Demo-only storytelling. Live sources leave this undefined. */
  narration?: Narration;
}
