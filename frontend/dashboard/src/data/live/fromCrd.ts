import { parseMemoryQuantity } from './quantity';
export { parseMemoryQuantity } from './quantity';
import type {
  ActionAttempt,
  ActionResult,
  Arch,
  CandidateEvaluation,
  FailureClass,
  IncidentEvent,
  IncidentTrigger,
  RecoveryDecision,
  RecoveryIncident,
  RecoveryPolicy,
  RecoveryPolicySpec,
  RejectReason,
  RuntimePhase,
} from '../../domain/types';
import { REJECT_REASONS } from '../../domain/types';
import type {
  CrdAttempt,
  CrdAttemptOutcome,
  CrdCandidate,
  CrdDecision,
  CrdLabelSelector,
  CrdRecoveryIncident,
  CrdRecoveryPolicy,
  CrdRecoveryPolicySpec,
  CrdRuntimePhase,
  DriverReport,
} from './crd';

/**
 * Converts the Go CRD wire format into the dashboard's view model. Components
 * keep working on ratios 0..1, epoch ms and plain numbers; every unit change lives
 * here. Anything that cannot be converted throws with the field path, so the
 * live source shows an error instead of rendering a wrong number.
 */

// --- Scalar conversions ----------------------------------------------------

export function parseTime(value: string | undefined, path: string): number {
  const ms = typeof value === 'string' ? Date.parse(value) : Number.NaN;
  if (!Number.isFinite(ms)) throw new Error(`${path} must be an RFC 3339 time`);
  return ms;
}

function optionalTime(value: string | undefined, path: string): number | undefined {
  return value === undefined || value === '' ? undefined : parseTime(value, path);
}

const DURATION_UNITS: Record<string, number> = { ns: 1e-9, us: 1e-6, µs: 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };

/** Go duration string ("20s", "1m30s", "500ms") to seconds. */
export function parseDuration(value: string, path: string): number {
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let consumed = 0;
  for (let m = re.exec(value); m; m = re.exec(value)) {
    total += Number(m[1]) * DURATION_UNITS[m[2]!]!;
    consumed = re.lastIndex;
  }
  if (value === '0') return 0;
  if (consumed === 0 || consumed !== value.length) throw new Error(`${path} must be a Go duration (e.g. 30s)`);
  return total;
}


function archOf(platform: string, path: string): Arch {
  if (platform === 'linux/amd64') return 'amd64';
  if (platform === 'linux/arm64') return 'arm64';
  throw new Error(`${path} must be linux/amd64 or linux/arm64`);
}

function selectorText(selector: CrdLabelSelector): string {
  const labels = Object.entries(selector.matchLabels ?? {}).map(([k, v]) => `${k}=${v}`);
  const exprs = (selector.matchExpressions ?? []).map(
    (e) => `${e.key} ${e.operator}${e.values?.length ? ` (${e.values.join(',')})` : ''}`,
  );
  return [...labels, ...exprs].join(', ') || '(모든 노드)';
}

const REJECT_SET = new Set<string>(REJECT_REASONS);

// --- RecoveryPolicy --------------------------------------------------------

export function policySpecFromCrd(spec: CrdRecoveryPolicySpec, path = 'policy.spec'): RecoveryPolicySpec {
  const d = (v: string, p: string) => parseDuration(v, `${path}.${p}`);
  return {
    targetRef: { kind: 'Deployment', name: spec.targetRef.name },
    enabled: spec.enabled,
    mode: spec.mode,
    candidateSelector: selectorText(spec.candidateSelector),
    requiredPlatforms: spec.requiredPlatforms.map((p, i) => `linux/${archOf(p, `${path}.requiredPlatforms[${i}]`)}` as const),
    detection: {
      nodeUnreadyForSeconds: d(spec.detection.nodeUnreadyFor, 'detection.nodeUnreadyFor'),
      restartThreshold: spec.detection.restartThreshold,
      restartWindowSeconds: d(spec.detection.restartWindow, 'detection.restartWindow'),
    },
    filter: {
      maxCPUUtilization: spec.filter.maxCPUUtilizationPercent / 100,
      maxMemoryUtilization: spec.filter.maxMemoryUtilizationPercent / 100,
      minimumAvailableMemoryBytes: parseMemoryQuantity(spec.filter.minimumAvailableMemory, `${path}.filter.minimumAvailableMemory`),
      startupMemoryBudgetBytes: parseMemoryQuantity(spec.filter.startupMemoryBudget, `${path}.filter.startupMemoryBudget`),
      requestedCapacityReserve: spec.filter.requestedCapacityReservePercent / 100,
      maxSampleAgeSeconds: d(spec.filter.maxSampleAge, 'filter.maxSampleAge'),
    },
    score: {
      cpuWeight: spec.score.cpuWeight / 100,
      memoryWeight: spec.score.memoryWeight / 100,
      requestsWeight: spec.score.requestsWeight / 100,
      recentFailurePenalty: spec.score.recentFailurePenalty,
      recentFailurePenaltyCap: spec.score.maxRecentFailurePenalty,
      failureHistoryWindowSeconds: d(spec.score.recentFailureWindow, 'score.recentFailureWindow'),
    },
    budget: {
      maxActionsPerIncident: spec.budget.maxActionsPerIncident,
      cooldownSeconds: d(spec.budget.cooldown, 'budget.cooldown'),
      placementTimeoutSeconds: d(spec.budget.placementTimeout, 'budget.placementTimeout'),
      verificationTimeoutSeconds: d(spec.budget.verificationTimeout, 'budget.verificationTimeout'),
      incidentDeadlineSeconds: d(spec.budget.incidentDeadline, 'budget.incidentDeadline'),
    },
    verification: {
      consecutiveSuccesses: spec.verification.consecutiveSuccesses,
      stabilityWindowSeconds: d(spec.verification.stabilityWindow, 'verification.stabilityWindow'),
    },
    // Not in the v1alpha1 CRD (Demo A scope): no quarantine and no Engineering handoff.
    onApplicationExhausted: 'None',
    engineering: { enabled: false, profileRef: '' },
    sameSignatureCircuit: true,
  };
}

/**
 * @param workloadUid UID of the policy's target Deployment; NodeFailure entries
 *   are per policy (one workload), so cooldowns inherit it.
 * @param policyHash Hash of the current spec if the BFF provides it (the CRD
 *   status does not carry one); otherwise the active incident's snapshot hash.
 */
export function policyFromCrd(
  crd: CrdRecoveryPolicy,
  options: { workloadUid: string; policyHash?: string },
): RecoveryPolicy {
  const spec = policySpecFromCrd(crd.spec);
  const failures = crd.status?.recentFailures ?? [];
  const cooldowns = failures.map((f, i) => ({
    workloadUid: options.workloadUid,
    nodeName: f.nodeName,
    until: parseTime(f.at, `policy.status.recentFailures[${i}].at`) + spec.budget.cooldownSeconds * 1000,
  }));
  return {
    name: crd.metadata.name,
    namespace: crd.metadata.namespace ?? '',
    generation: crd.metadata.generation ?? 0,
    policyHash: options.policyHash ?? '',
    spec,
    status: {
      observedGeneration: crd.status?.observedGeneration ?? 0,
      activeIncidentRef: crd.status?.activeIncident?.name ?? null,
      incidentSequence: crd.status?.incidentSequence ?? 0,
      cooldowns,
      conditions: (crd.status?.conditions ?? []).map((c) => ({...c, lastTransitionTime: parseTime(c.lastTransitionTime, 'policy.status.conditions.lastTransitionTime')})),
      circuits: (crd.status?.circuits ?? []).map((c) => ({...c, openedAt: parseTime(c.openedAt, 'policy.status.circuits.openedAt')})),
      recentFailures: failures.map((f, i) => ({
        nodeName: f.nodeName,
        at: parseTime(f.at, `policy.status.recentFailures[${i}].at`),
      })),
    },
  };
}

// --- RecoveryIncident ------------------------------------------------------

const OUTCOME_RESULT: Record<CrdAttemptOutcome, ActionResult> = {
  Reserved: 'pending',
  Patched: 'pending',
  Succeeded: 'verified',
  Failed: 'failed',
  Superseded: 'failed',
  Cancelled: 'failed',
};

/** Phase "" is DETECTED; TARGET_SELECTED is not persisted by the Controller. */
export function phaseFromCrd(phase: CrdRuntimePhase | undefined): RuntimePhase {
  if (!phase) return 'DETECTED';
  return phase;
}

function candidateFromCrd(
  c: CrdCandidate,
  ctx: { observedAt: number; weights: RecoveryPolicySpec['score']; failedNode?: string },
  path: string,
): CandidateEvaluation {
  const m = c.measurements;
  const cpu = typeof m.cpuUtilizationPermille === 'number' ? m.cpuUtilizationPermille / 1000 : null;
  const memAvail = typeof m.memAvailableBytes === 'number' ? m.memAvailableBytes : null;
  const memTotal = typeof m.memTotalBytes === 'number' && m.memTotalBytes > 0 ? m.memTotalBytes : null;
  const ages = [m.cpuSampleTime, m.memSampleTime].map((t, i) =>
    t ? (ctx.observedAt - parseTime(t, `${path}.measurements.${i ? 'memSampleTime' : 'cpuSampleTime'}`)) / 1000 : null,
  );
  const headroom = (alloc: number, reserve: number, requested: number, extra: number) =>
    alloc > 0 ? (alloc - reserve - requested - extra) / alloc : 0;
  const rejectReasons = (c.rejectReasons ?? []).map((r, i) => {
    if (!REJECT_SET.has(r)) throw new Error(`${path}.rejectReasons[${i}] is not a known reason`);
    return r as RejectReason;
  });
  const evaluation: CandidateEvaluation = {
    nodeUid: c.nodeUID,
    nodeName: c.nodeName,
    hostname: c.hostname,
    arch: archOf(c.platform, `${path}.platform`),
    measurements: {
      cpuUtilization: cpu,
      memoryUtilization: memAvail !== null && memTotal !== null ? 1 - memAvail / memTotal : null,
      memAvailableBytes: memAvail,
      requestedCpuHeadroom: headroom(m.allocatableCPUMilli, m.reserveCPUMilli, m.requestedCPUMilli, m.newPodCPUMilli),
      requestedMemoryHeadroom: headroom(
        m.allocatableMemoryBytes,
        m.reserveMemoryBytes,
        m.requestedMemoryBytes,
        m.newPodMemoryBytes,
      ),
      sampleAgeSeconds: ages.every((a) => a !== null) ? Math.max(0, ...(ages as number[])) : null,
    },
    rejectReasons,
  };
  if (c.resourceCondition) {
    const condition = c.resourceCondition;
    if (![condition.runID, condition.epoch, condition.policyUID].every(v => typeof v === 'string' && v.length > 0) || !/^[a-f0-9]{64}$/.test(condition.configSHA256)) throw new Error(`${path}.resourceCondition binding missing`);
    parseTime(condition.expiresAt, `${path}.resourceCondition.expiresAt`);
    const applied = candidateFromCrd({...c, resourceCondition: undefined, measurements: condition.appliedMeasurements}, ctx, `${path}.resourceCondition`);
    evaluation.resourceCondition = {...condition, appliedMeasurements: applied.measurements};
  }
  if (ctx.failedNode && c.nodeName === ctx.failedNode) evaluation.exclusion = 'FAILED_NODE';
  if (c.score) {
    const h = {
      cpu: c.score.cpuHeadroomPermille / 1000,
      memory: c.score.memoryHeadroomPermille / 1000,
      requests: c.score.requestedHeadroomPermille / 1000,
    };
    evaluation.score = {
      cpuHeadroom: h.cpu,
      memoryHeadroom: h.memory,
      requestedHeadroom: h.requests,
      weighted: {
        cpu: 100 * ctx.weights.cpuWeight * h.cpu,
        memory: 100 * ctx.weights.memoryWeight * h.memory,
        requests: 100 * ctx.weights.requestsWeight * h.requests,
      },
      penalty: c.score.penaltyMilli / 1000,
      total: c.score.scoreMilli / 1000,
    };
  }
  return evaluation;
}

function decisionFromCrd(
  d: CrdDecision,
  ctx: { weights: RecoveryPolicySpec['score']; failedNode?: string; sourceNodeName: string | null },
  path: string,
): RecoveryDecision {
  const observedAt = parseTime(d.snapshotObservedAt, `${path}.snapshotObservedAt`);
  return {
    incidentId: d.incidentID,
    actionId: d.actionID ?? '',
    policyGeneration: d.policyGeneration,
    policyHash: d.policyHash,
    workloadUid: d.workloadUID,
    baselineTemplateHash: d.baselineTemplateHash,
    imageIndexDigest: d.imageIndexDigest,
    snapshotObservedAt: observedAt,
    snapshotExpiresAt: parseTime(d.snapshotExpiresAt, `${path}.snapshotExpiresAt`),
    candidates: (d.candidates ?? []).map((c, i) =>
      candidateFromCrd(c, { observedAt, weights: ctx.weights, ...(ctx.failedNode ? { failedNode: ctx.failedNode } : {}) }, `${path}.candidates[${i}]`),
    ),
    sourceNodeName: ctx.sourceNodeName,
    selectedNodeUid: d.selectedNodeUID || null,
    selectedHostname: d.selectedHostname || null,
    actionType: d.actionType === 'Relocation' ? 'relocation' : null,
    decisionReason: d.decisionReason,
  };
}

function attemptFromCrd(a: CrdAttempt, decision: RecoveryDecision | undefined, path: string): ActionAttempt {
  const arch = decision?.candidates.find((c) => c.nodeName === a.nodeName)?.arch;
  const finishedAt = optionalTime(a.completedAt, `${path}.completedAt`);
  const placed = a.outcome === 'Patched' && a.podReadyAt !== undefined;
  return {
    actionId: a.actionID,
    type: 'relocation',
    targetNode: a.nodeName,
    targetArch: arch ?? 'amd64',
    startedAt: parseTime(a.reservedAt, `${path}.reservedAt`),
    ...(finishedAt !== undefined ? { finishedAt } : {}),
    result: placed ? 'placed' : OUTCOME_RESULT[a.outcome],
    ...(a.podUID ? { podUid: a.podUID } : {}),
    httpSuccesses: a.consecutiveSuccesses ?? 0,
    ...(a.failureReason || a.lastProbeError ? { failureReason: a.failureReason || a.lastProbeError } : {}),
  };
}

/** Bounded event list derived only from CRD timestamps (the CRD stores no event log). */
function deriveEvents(crd: CrdRecoveryIncident, phase: RuntimePhase): IncidentEvent[] {
  const events: IncidentEvent[] = [];
  const push = (at: number | undefined, kind: IncidentEvent['kind'], p: IncidentEvent['phase'], message: string) => {
    if (at !== undefined) events.push({ at, kind, phase: p, message });
  };
  const t = crd.spec.trigger;
  push(parseTime(t.observedAt, 'spec.trigger.observedAt'), 'trigger', 'DETECTED', `${t.reason} ${t.nodeName ?? t.podName ?? ''}`.trim());
  push(parseTime(crd.spec.detectedAt, 'spec.detectedAt'), 'classify', 'CLASSIFIED', `${t.class} (${t.ruleVersion})`);
  (crd.status?.runtime?.attempts ?? []).forEach((a, i) => {
    const p = `status.runtime.attempts[${i}]`;
    push(optionalTime(a.decision?.decidedAt, `${p}.decision.decidedAt`), 'decide', 'TARGET_SELECTED', `${a.hostname} 선택 (${a.decision?.decisionReason ?? ''})`);
    push(optionalTime(a.patchedAt, `${p}.patchedAt`), 'act', 'EXECUTING', `nodeSelector → ${a.hostname}, attempt ${a.actionID}`);
    push(optionalTime(a.podReadyAt, `${p}.podReadyAt`), 'act', 'VERIFYING', `${a.podName ?? 'Pod'} Ready`);
    push(optionalTime(a.completedAt, `${p}.completedAt`), a.outcome === 'Succeeded' ? 'recovered' : 'verify', phase, `attempt ${a.actionID}: ${a.outcome}${a.failureReason ? ` (${a.failureReason})` : ''}`);
  });
  return events.sort((x, y) => x.at - y.at).slice(-40);
}

export function incidentFromCrd(crd: CrdRecoveryIncident, driver?: DriverReport): RecoveryIncident {
  const rt = crd.status?.runtime ?? {};
  const phase = phaseFromCrd(rt.phase);
  const policySpec = policySpecFromCrd(crd.spec.policy, 'spec.policy');
  const t = crd.spec.trigger;
  const triggeredAt = parseTime(t.observedAt, 'spec.trigger.observedAt');
  const sourceNodeName = crd.spec.baseline.hostname ?? t.nodeName ?? null;
  const failedNode = t.reason.includes('NotReady') || t.class === 'INFRASTRUCTURE_FAILURE' ? t.nodeName : undefined;
  const ctx = { weights: policySpec.score, sourceNodeName, ...(failedNode ? { failedNode } : {}) };
  const decision = rt.decision ? decisionFromCrd(rt.decision, ctx, 'status.runtime.decision') : undefined;
  const crdAttempts = rt.attempts ?? [];
  const attempts = crdAttempts.map((a, i) =>
    attemptFromCrd(a, decisionFromCrd(a.decision, ctx, `status.runtime.attempts[${i}].decision`), `status.runtime.attempts[${i}]`),
  );
  const first = crdAttempts[0];
  const recoveredAt = phase === 'RECOVERED' ? optionalTime(rt.completedAt, 'status.runtime.completedAt') : undefined;
  const faultInjectedAt =
    driver && driver.incident === crd.metadata.name ? optionalTime(driver.faultInjectedAt, 'driver.faultInjectedAt') : undefined;

  const trigger: IncidentTrigger = t.reason === 'NODE_NOT_READY'
    ? { type: 'NodeNotReady', nodeName: t.nodeName ?? '', observedAt: triggeredAt }
    : t.reason === 'IMAGE_PULL'
      ? { type: 'ImagePull', podName: t.podName ?? '', observedAt: triggeredAt }
      : t.reason === 'REPEATED_EXIT' || t.reason === 'REPEATED_RESTART' || t.reason === 'RESTART_THRESHOLD'
        ? { type: 'RepeatedRestart', podName: t.podName ?? '', observedAt: triggeredAt }
        : { type: 'FailureSignal', reason: t.reason, subject: t.podName ?? t.nodeName ?? '', observedAt: triggeredAt };
  const waitingReason =
    phase === 'WAITING'
      ? rt.decision?.decisionReason === 'METRICS_UNAVAILABLE'
        ? ('METRICS_UNAVAILABLE' as const)
        : ('NO_CANDIDATE' as const)
      : undefined;

  return {
    uid: crd.metadata.uid,
    name: crd.metadata.name,
    namespace: crd.metadata.namespace ?? '',
    createdAt: optionalTime(crd.metadata.creationTimestamp, 'metadata.creationTimestamp') ?? triggeredAt,
    spec: {
      workloadUid: crd.spec.workload.uid,
      trigger: {...trigger, reason: t.reason},
      policyRef: crd.spec.policyRef.name,
      policyHash: crd.spec.policyHash,
    },
    status: {
      runtime: {
        phase,
        classification: t.class as FailureClass,
        ...(waitingReason ? { waitingReason } : {}),
        ...(t.signature ? { signature: t.signature } : {}),
        ...(decision ? { decision } : {}),
        attempts,
        budget: { used: crdAttempts.length, max: policySpec.budget.maxActionsPerIncident },
        timings: {
          ...(faultInjectedAt !== undefined ? { faultInjectedAt } : {}),
          triggeredAt,
          detectedAt: parseTime(crd.spec.detectedAt, 'spec.detectedAt'),
          ...(rt.decision?.decidedAt ? { decidedAt: parseTime(rt.decision.decidedAt, 'status.runtime.decision.decidedAt') } : first?.decision?.decidedAt ? { decidedAt: parseTime(first.decision.decidedAt, 'status.runtime.attempts[0].decision.decidedAt') } : {}),
          ...(first?.podReadyAt ? { placedAt: parseTime(first.podReadyAt, 'status.runtime.attempts[0].podReadyAt') } : {}),
          ...(recoveredAt !== undefined ? { recoveredAt } : {}),
        },
        oldPodsOutstanding: (rt.oldPodsOutstanding ?? []).length > 0,
      },
      conditions: rt.reason
        ? [
            {
              type: 'Runtime',
              status: 'True',
              reason: rt.reason,
              message: rt.message ?? '',
              lastTransitionTime: optionalTime(rt.lastTransitionTime, 'status.runtime.lastTransitionTime') ?? triggeredAt,
            },
          ]
        : [],
      events: deriveEvents(crd, phase),
    },
  };
}

export function isCrdPolicy(x: unknown): x is CrdRecoveryPolicy {
  return typeof x === 'object' && x !== null && (x as { kind?: unknown }).kind === 'RecoveryPolicy';
}

export function isCrdIncident(x: unknown): x is CrdRecoveryIncident {
  return typeof x === 'object' && x !== null && (x as { kind?: unknown }).kind === 'RecoveryIncident';
}
