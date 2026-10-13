import type {
  ActionAttempt,
  ActionType,
  Taint,
  Toleration,
  CandidateEvaluation,
  ClusterNode,
  CooldownEntry,
  MetricSample,
  RejectReason,
  RecoveryPolicySpec,
  ScoreBreakdown,
  WorkloadStatus,
} from './types';

/**
 * Hard filter + score from architecture §8, implemented as a pure function so the
 * Demo Mode decision is computed from simulated metrics instead of being scripted.
 * The Go Candidate Evaluator remains the authority; this mirrors the documented
 * formula for visualization and is unit-tested against the same rules.
 */
export interface EvaluationInput {
  nodes: ClusterNode[];
  workload: WorkloadStatus;
  policy: RecoveryPolicySpec;
  now: number;
  /** Node whose failure triggered the incident (Node Failure). Excluded, not scored. */
  failedNodeName?: string;
  /** Node currently running the workload; selecting it means `restart`. */
  currentNodeName?: string;
  cooldowns?: CooldownEntry[];
  /** Failed OpsLoop actions (node + finish time); the penalty counts those inside the window. */
  recentFailures?: Array<{ nodeName: string; at: number }>;
}

export interface EvaluationResult {
  candidates: CandidateEvaluation[];
  selected: CandidateEvaluation | null;
  actionType: ActionType | null;
}

const PRESSURE_TO_REASON = {
  MemoryPressure: 'MEMORY_PRESSURE',
  DiskPressure: 'DISK_PRESSURE',
  PIDPressure: 'PID_PRESSURE',
} as const satisfies Record<string, RejectReason>;

/** A sample from the future beyond this skew is treated as untrustworthy (stale). */
const MAX_CLOCK_SKEW_SECONDS = 5;

/**
 * Age of a sample in seconds, or null when the sample cannot be trusted at all
 * (missing/NaN value, non-numeric timestamp). Timestamps further in the future
 * than the skew allowance return Infinity so they fail the freshness check.
 */
function sampleAgeSeconds(sample: MetricSample, now: number): number | null {
  if (typeof sample.value !== 'number' || !Number.isFinite(sample.value)) return null;
  if (typeof sample.observedAt !== 'number' || !Number.isFinite(sample.observedAt)) return null;
  const age = (now - sample.observedAt) / 1000;
  if (age < -MAX_CLOCK_SKEW_SECONDS) return Number.POSITIVE_INFINITY;
  return Math.max(0, age);
}

function finitePositive(x: unknown): x is number {
  return typeof x === 'number' && Number.isFinite(x) && x > 0;
}

function finiteNonNegative(x: unknown): x is number {
  return typeof x === 'number' && Number.isFinite(x) && x >= 0;
}

function tolerates(taint: Taint, tolerations: Toleration[]): boolean {
  return tolerations.some((t) => {
    if (t.effect && t.effect !== taint.effect) return false;
    if (t.operator === 'Exists') return t.key === undefined || t.key === taint.key;
    return t.key === taint.key && (t.value ?? '') === (taint.value ?? '');
  });
}

/** Stable, locale-independent name order for tie-breaks (§8). */
export function compareNodeNames(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

function clamp01(x: number): number {
  return Math.min(1, Math.max(0, x));
}

function round(x: number, digits = 4): number {
  const f = 10 ** digits;
  return Math.round(x * f) / f;
}

export function evaluateCandidate(node: ClusterNode, input: EvaluationInput): CandidateEvaluation {
  const { workload, policy, now } = input;
  const reasons = new Set<RejectReason>();

  if (node.readiness !== 'Ready') reasons.add('NODE_NOT_READY');
  if (node.unschedulable) reasons.add('UNSCHEDULABLE');

  const platform = `linux/${node.arch}` as const;
  const imageSupports = workload.platforms.some((p) => p.platform === platform);
  if (!imageSupports || !policy.requiredPlatforms.includes(platform)) reasons.add('ARCH_MISMATCH');

  const untolerated = (node.taints ?? []).filter(
    (t) => t.effect !== 'PreferNoSchedule' && !tolerates(t, workload.tolerations ?? []),
  );
  if (untolerated.length > 0) reasons.add('UNTOLERATED_TAINT');
  // Constraints OpsLoop cannot evaluate are rejected explicitly, never approximated (§8).
  if ((workload.unsupportedConstraints ?? []).length > 0) reasons.add('UNSUPPORTED_CONSTRAINT');

  for (const condition of node.pressure) {
    const reason = PRESSURE_TO_REASON[condition];
    if (reason) reasons.add(reason);
  }

  // Metrics: missing, NaN, non-numeric or stale samples are never treated as 0% utilization.
  const samples = [node.usage.cpu, node.usage.memory, node.usage.memAvailableBytes];
  const ages = samples.map((s) => sampleAgeSeconds(s, now));
  if (ages.some((a) => a === null)) reasons.add('METRICS_MISSING');
  if (ages.some((a) => a !== null && a > policy.filter.maxSampleAgeSeconds)) reasons.add('METRICS_STALE');
  const metricsUsable = !reasons.has('METRICS_MISSING') && !reasons.has('METRICS_STALE');

  const finiteOrNull = (x: number | null): number | null =>
    typeof x === 'number' && Number.isFinite(x) ? x : null;
  const cpu = finiteOrNull(node.usage.cpu.value);
  const mem = finiteOrNull(node.usage.memory.value);
  const memAvailable = finiteOrNull(node.usage.memAvailableBytes.value);

  if (metricsUsable && cpu !== null && cpu > policy.filter.maxCPUUtilization) reasons.add('CPU_PRESSURE');
  if (metricsUsable && mem !== null && mem > policy.filter.maxMemoryUtilization) reasons.add('MEMORY_PRESSURE');
  if (
    metricsUsable &&
    memAvailable !== null &&
    memAvailable < policy.filter.minimumAvailableMemoryBytes + policy.filter.startupMemoryBudgetBytes
  ) {
    reasons.add('INSUFFICIENT_AVAILABLE_MEMORY');
  }

  // Requests: existing non-terminated Pods + the new surge Pod + policy reserve.
  // Unparseable quantities (e.g. an unconverted resource.Quantity string) cannot
  // prove capacity, so they reject instead of passing as NaN.
  const reserve = policy.filter.requestedCapacityReserve;
  const headroom = (alloc: unknown, requested: unknown, extra: unknown): number | null =>
    finitePositive(alloc) && finiteNonNegative(requested) && finiteNonNegative(extra)
      ? (alloc * (1 - reserve) - (requested + extra)) / alloc
      : null;
  const cpuHeadroomRaw = headroom(
    node.allocatable.cpuMillicores,
    node.requested.cpuMillicores,
    workload.requests.cpuMillicores,
  );
  const memHeadroomRaw = headroom(
    node.allocatable.memoryBytes,
    node.requested.memoryBytes,
    workload.requests.memoryBytes,
  );
  if (cpuHeadroomRaw === null || cpuHeadroomRaw < 0) reasons.add('INSUFFICIENT_REQUESTED_CPU');
  if (memHeadroomRaw === null || memHeadroomRaw < 0) reasons.add('INSUFFICIENT_REQUESTED_MEMORY');
  const requestedCpuHeadroom = cpuHeadroomRaw ?? 0;
  const requestedMemoryHeadroom = memHeadroomRaw ?? 0;

  const cooling = (input.cooldowns ?? []).some(
    (c) => c.workloadUid === workload.uid && c.nodeName === node.name && c.until > now,
  );
  if (cooling) reasons.add('COOLDOWN');

  const validAges = ages.filter((a): a is number => a !== null && Number.isFinite(a));
  const evaluation: CandidateEvaluation = {
    nodeUid: node.uid,
    nodeName: node.name,
    hostname: node.hostnameLabel,
    arch: node.arch,
    measurements: {
      cpuUtilization: cpu,
      memoryUtilization: mem,
      memAvailableBytes: memAvailable,
      requestedCpuHeadroom: round(requestedCpuHeadroom),
      requestedMemoryHeadroom: round(requestedMemoryHeadroom),
      sampleAgeSeconds: validAges.length === samples.length ? round(Math.max(...validAges), 1) : null,
    },
    rejectReasons: [...reasons],
  };

  if (!node.recoveryCandidate) evaluation.exclusion = 'NOT_A_CANDIDATE';
  if (node.name === input.failedNodeName) evaluation.exclusion = 'FAILED_NODE';

  if (reasons.size === 0 && !evaluation.exclusion && cpu !== null && mem !== null) {
    const windowSeconds = policy.score.failureHistoryWindowSeconds;
    const windowMs = typeof windowSeconds === 'number' ? windowSeconds * 1000 : Number.POSITIVE_INFINITY;
    const failures = (input.recentFailures ?? []).filter(
      (f) => f.nodeName === node.name && f.at <= now && now - f.at <= windowMs,
    ).length;
    const score = scoreCandidate({
      cpuUtilization: cpu,
      memoryUtilization: mem,
      requestedHeadroom: Math.min(requestedCpuHeadroom, requestedMemoryHeadroom),
      failures,
      policy,
    });
    if (Number.isFinite(score.total)) evaluation.score = score;
  }
  return evaluation;
}

export function scoreCandidate(args: {
  cpuUtilization: number;
  memoryUtilization: number;
  requestedHeadroom: number;
  failures: number;
  policy: RecoveryPolicySpec;
}): ScoreBreakdown {
  const { cpuWeight, memoryWeight, requestsWeight, recentFailurePenalty, recentFailurePenaltyCap } =
    args.policy.score;
  const cpuHeadroom = clamp01(1 - args.cpuUtilization);
  const memoryHeadroom = clamp01(1 - args.memoryUtilization);
  const requestedHeadroom = clamp01(args.requestedHeadroom);
  const weighted = {
    cpu: round(100 * cpuWeight * cpuHeadroom, 2),
    memory: round(100 * memoryWeight * memoryHeadroom, 2),
    requests: round(100 * requestsWeight * requestedHeadroom, 2),
  };
  const penalty = Math.min(recentFailurePenalty * args.failures, recentFailurePenaltyCap ?? Number.POSITIVE_INFINITY);
  return {
    cpuHeadroom: round(cpuHeadroom),
    memoryHeadroom: round(memoryHeadroom),
    requestedHeadroom: round(requestedHeadroom),
    weighted,
    penalty,
    total: round(weighted.cpu + weighted.memory + weighted.requests - penalty, 2),
  };
}

/** Failed OpsLoop actions of an incident as penalty input (node + finish time). */
export function failuresFromAttempts(attempts: ActionAttempt[] | undefined): Array<{ nodeName: string; at: number }> {
  return (attempts ?? [])
    .filter((a) => a.result === 'failed')
    .map((a) => ({ nodeName: a.targetNode, at: a.finishedAt ?? a.startedAt }));
}

/** Highest score wins; ties resolve by stable node-name order (§8). */
export function evaluateCandidates(input: EvaluationInput): EvaluationResult {
  const candidates = input.nodes.map((node) => evaluateCandidate(node, input));
  const eligible = candidates
    .filter((c): c is CandidateEvaluation & { score: ScoreBreakdown } => c.score !== undefined)
    .sort((a, b) => b.score.total - a.score.total || compareNodeNames(a.nodeName, b.nodeName));
  const selected = eligible[0] ?? null;
  const actionType: ActionType | null =
    selected === null ? null : selected.nodeName === input.currentNodeName ? 'restart' : 'relocation';
  return { candidates, selected, actionType };
}
