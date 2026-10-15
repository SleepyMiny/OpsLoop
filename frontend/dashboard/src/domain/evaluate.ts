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
  /** Live previews require raw exporter inputs; legacy Demo ratios are presentation simulations. */
  requireRawMetrics?: boolean;
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
  if (Math.round(sample.observedAt) <= 0 || Math.round(sample.observedAt) >= 253402300800000) return null;
  const age = (now - Math.round(sample.observedAt)) / 1000;
  if (age < -MAX_CLOCK_SKEW_SECONDS) return Number.POSITIVE_INFINITY;
  return Math.max(0, age);
}

// All arithmetic that decides resource capacity uses exact integers. Fractional
// simulated requests round up like resource.Quantity.Value()/MilliValue().
function resourceInteger(x: unknown): bigint | null {
  return typeof x === 'number' && Number.isFinite(x) && x >= 0 && Number.isSafeInteger(Math.ceil(x))
    ? BigInt(Math.ceil(x)) : null;
}

function requestHeadroom(alloc: unknown, requested: unknown, extra: unknown, reserve: number) {
  const a = resourceInteger(alloc), r = resourceInteger(requested), e = resourceInteger(extra);
  if (a === null || a <= 0n || r === null || e === null || !Number.isFinite(reserve) || reserve < 0 || reserve > 1) return null;
  const remaining = a - a * BigInt(Math.round(reserve * 100)) / 100n - r - e;
  return { ratio: Number(remaining) / Number(a), permille: Number(remaining > 0n ? remaining * 1000n / a : 0n) };
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

  if (!node.hostnameLabel) reasons.add('UNSUPPORTED_CONSTRAINT');
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
  const finiteOrNull = (x: number | null): number | null =>
    typeof x === 'number' && Number.isFinite(x) ? x : null;
  const rawCPU = finiteOrNull(node.usage.cpu.value);
  // assessMetrics clamps then rounds to integer permille before filtering/scoring.
  const cpu = rawCPU === null ? null : Math.round(clamp01(rawCPU) * 1000) / 1000;
  let mem = finiteOrNull(node.usage.memory.value);
  const rawAvailable = finiteOrNull(node.usage.memAvailableBytes.value);
  // Check the raw float before truncation, exactly as Go assessMetrics does.
  const maxMemoryBytes = 2 ** 50;
  const availableValid = rawAvailable !== null && rawAvailable >= 0 && rawAvailable <= maxMemoryBytes;
  const memAvailable = availableValid ? Math.trunc(rawAvailable) : null;
  const rawTotal = node.usage.memTotalBytes;
  const hasRawTotal = rawTotal !== undefined;
  let memHeadroomPermille: number | undefined;
  let memoryPressure: boolean | undefined;
  if (hasRawTotal) {
    if (typeof rawTotal !== 'number' || !Number.isSafeInteger(Math.trunc(rawTotal)) || rawTotal < 1 || rawTotal > maxMemoryBytes ||
        memAvailable === null || !Number.isSafeInteger(memAvailable) || memAvailable < 0 || memAvailable > Math.trunc(rawTotal)) {
      reasons.add('METRICS_MISSING');
    } else {
      const total = BigInt(Math.trunc(rawTotal)), available = BigInt(memAvailable);
      mem = 1 - Number(available) / Number(total);
      memHeadroomPermille = Number((1000n * available + total / 2n) / total);
      memoryPressure = (total - available) * 100n > BigInt(Math.round(policy.filter.maxMemoryUtilization * 100)) * total;
    }
  } else if (input.requireRawMetrics) reasons.add('METRICS_MISSING');
  if (mem === null || mem < 0 || mem > 1 || memAvailable === null || memAvailable < 0 || !Number.isSafeInteger(memAvailable)) reasons.add('METRICS_MISSING');
  const count = node.usage.cpuSampleCount;
  if ((count !== undefined || input.requireRawMetrics) && (typeof count !== 'number' || !Number.isFinite(count) || count < 3 || count > 2147483647)) reasons.add('METRICS_MISSING');
  const up = node.usage.exporterUp;
  if ((up !== undefined || input.requireRawMetrics) && (typeof up !== 'number' || !Number.isFinite(up))) reasons.add('METRICS_MISSING');
  if (up === 0) reasons.add('METRICS_STALE');
  const metricsUsable = !reasons.has('METRICS_MISSING') && !reasons.has('METRICS_STALE');

  if (metricsUsable && cpu !== null && cpu > policy.filter.maxCPUUtilization) reasons.add('CPU_PRESSURE');
  if (metricsUsable && mem !== null && (memoryPressure ?? (mem > policy.filter.maxMemoryUtilization))) reasons.add('MEMORY_PRESSURE');
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
  const cpuRoom = requestHeadroom(node.allocatable.cpuMillicores, node.requested.cpuMillicores, workload.requests.cpuMillicores, reserve);
  const memRoom = requestHeadroom(node.allocatable.memoryBytes, node.requested.memoryBytes, workload.requests.memoryBytes, reserve);
  if (cpuRoom === null || cpuRoom.ratio < 0) reasons.add('INSUFFICIENT_REQUESTED_CPU');
  if (memRoom === null || memRoom.ratio < 0) reasons.add('INSUFFICIENT_REQUESTED_MEMORY');
  const requestedCpuHeadroom = cpuRoom?.ratio ?? 0;
  const requestedMemoryHeadroom = memRoom?.ratio ?? 0;

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
      memoryHeadroomPermille: memHeadroomPermille,
      requestedHeadroomPermille: Math.min(cpuRoom!.permille, memRoom!.permille),
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
  memoryHeadroomPermille?: number;
  requestedHeadroomPermille?: number;
  policy: RecoveryPolicySpec;
}): ScoreBreakdown {
  const { cpuWeight, memoryWeight, requestsWeight, recentFailurePenalty, recentFailurePenaltyCap } =
    args.policy.score;
  const cpuPermille = 1000 - Math.round(clamp01(args.cpuUtilization) * 1000);
  const memoryPermille = args.memoryHeadroomPermille ?? Math.round(clamp01(1 - args.memoryUtilization) * 1000);
  const requestedPermille = args.requestedHeadroomPermille ?? Math.floor(clamp01(args.requestedHeadroom) * 1000);
  const weightedMilli = {
    cpu: Math.round(cpuWeight * 100) * cpuPermille,
    memory: Math.round(memoryWeight * 100) * memoryPermille,
    requests: Math.round(requestsWeight * 100) * requestedPermille,
  };
  const penalty = Math.min(recentFailurePenalty * args.failures, recentFailurePenaltyCap ?? Number.POSITIVE_INFINITY);
  return {
    cpuHeadroom: cpuPermille / 1000,
    memoryHeadroom: memoryPermille / 1000,
    requestedHeadroom: requestedPermille / 1000,
    weighted: { cpu: weightedMilli.cpu / 1000, memory: weightedMilli.memory / 1000, requests: weightedMilli.requests / 1000 },
    penalty,
    total: (weightedMilli.cpu + weightedMilli.memory + weightedMilli.requests - penalty * 1000) / 1000,
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
