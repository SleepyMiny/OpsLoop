import { displayIncident } from '../../domain/displayIncident';
import { compareNodeNames, evaluateCandidates, failuresFromAttempts, type EvaluationInput } from '../../domain/evaluate';
import { REJECT_REASON_LABEL, RUNTIME_PHASE_LABEL, actionTypeLabel } from '../../domain/labels';
import type {
  ActionResult,
  Arch,
  CandidateEvaluation,
  CandidateExclusion,
  ClusterNode,
  DashboardSnapshot,
  IncidentRuntimeStatus,
  RecoveryDecision,
  RecoveryPolicySpec,
  RejectReason,
  RuntimePhase,
  WaitingReason,
} from '../../domain/types';
import { bytes, percent, shortHash } from '../shared/format';
import type { LampState } from '../shared/primitives';

/**
 * View model for the candidate board. Everything the component draws is derived here
 * as plain data so the rules (what is revealed at which phase, how a criterion is
 * judged) are unit-testable without a DOM.
 */

// ---------------------------------------------------------------------------
// Reveal stage
// ---------------------------------------------------------------------------

/**
 * The staged reveal. Each stage shows everything the previous one did.
 *   collect  measurements only
 *   filter   + pass/fail per criterion, verdicts, reject reasons
 *   score    + score bars and totals
 *   decide   + the selected target
 */
export type RevealStage = 'collect' | 'filter' | 'score' | 'decide';

const STAGE_ORDER: Record<RevealStage, number> = { collect: 0, filter: 1, score: 2, decide: 3 };

export function stageAtLeast(stage: RevealStage, min: RevealStage): boolean {
  return STAGE_ORDER[stage] >= STAGE_ORDER[min];
}

/**
 * Runtime phase to reveal stage. `null` means the Runtime has not reached candidate
 * collection (or never will), so no evaluation of this incident exists yet.
 */
export function revealStageForPhase(phase: RuntimePhase): RevealStage | null {
  switch (phase) {
    case 'MONITORING':
    case 'DETECTED':
    case 'CLASSIFIED':
      return null;
    case 'COLLECTING_CANDIDATES':
      return 'collect';
    case 'FILTERING':
    case 'WAITING':
      return 'filter';
    case 'SCORING':
      return 'score';
    default:
      return 'decide';
  }
}

// ---------------------------------------------------------------------------
// Criteria
// ---------------------------------------------------------------------------

export type CriterionId = 'cpu' | 'memory' | 'available' | 'requests';
export const CRITERION_IDS: readonly CriterionId[] = ['cpu', 'memory', 'available', 'requests'];

/**
 * pending  measured, not judged yet (collect stage)
 * pass     inside the policy limit
 * fail     violates the limit (a reject reason names it)
 * stale    sample older than maxSampleAge, so not trusted
 * missing  no sample at all; never drawn as 0
 * held     value exists but another metric is missing, so the evaluator does not judge it
 */
export type CriterionState = 'pending' | 'pass' | 'fail' | 'stale' | 'missing' | 'held';

export interface ColumnModel {
  id: CriterionId;
  label: string;
  /** Shortened name for a one-line header. */
  labelShort: string;
  rule: string;
  /** The same limit without words, for headers with little room. */
  ruleShort: string;
}

export interface CriterionModel {
  id: CriterionId;
  state: CriterionState;
  valueText: string;
  /** Short verdict word next to the value; empty when there is nothing to say. */
  stateWord: string;
  /** Filled extent along the track, 0..1. */
  fillFrom: number;
  fillTo: number;
  /** Where the policy limit sits on the track, 0..1. */
  marker: number;
  /** Part of the track that breaks the rule, 0..1. */
  zoneFrom: number;
  zoneTo: number;
}

const GiB = 1024 ** 3;

/** The request-headroom track spans -20%..90% so a shortfall can be drawn left of the 0 limit. */
export const REQUEST_AXIS = { lo: -0.2, hi: 0.9 } as const;

const STATE_WORD: Record<CriterionId, { pass: string; fail: string }> = {
  cpu: { pass: '이내', fail: '초과' },
  memory: { pass: '이내', fail: '초과' },
  available: { pass: '충분', fail: '부족' },
  requests: { pass: '여유', fail: '부족' },
};

const MISSING_TEXT = '관측값 없음';

/** A usable sample: present and a real number (NaN is treated as missing, like the evaluator does). */
function isPresent(value: number | null): value is number {
  return value !== null && Number.isFinite(value);
}

function clamp01(x: number): number {
  return Math.min(1, Math.max(0, x));
}

function axisPos(value: number, lo: number, hi: number): number {
  return clamp01((value - lo) / (hi - lo));
}

/**
 * Percent text that never contradicts the verdict: when rounding to a whole percent
 * would land on the limit (or on zero) while the value is not exactly there, show one
 * decimal instead of "85%" next to a failure.
 */
export function percentText(ratio: number, boundary?: number): string {
  const whole = Math.round(ratio * 100);
  const nearBoundary = boundary !== undefined && whole === Math.round(boundary * 100) && ratio !== boundary;
  const nearZero = whole === 0 && ratio !== 0;
  const text = nearBoundary || nearZero ? (ratio * 100).toFixed(1) : String(whole);
  return `${text.startsWith('-') ? '−' + text.slice(1) : text}%`;
}

function limitText(ratio: number): string {
  return percent(ratio, Number.isInteger(+(ratio * 100).toFixed(1)) ? 0 : 1);
}

export function columnModels(policy: RecoveryPolicySpec): ColumnModel[] {
  const f = policy.filter;
  const required = bytes(f.minimumAvailableMemoryBytes + f.startupMemoryBudgetBytes);
  return [
    {
      id: 'cpu',
      label: 'CPU 사용률',
      labelShort: 'CPU',
      rule: `기준 ${limitText(f.maxCPUUtilization)} 이하`,
      ruleShort: `≤ ${limitText(f.maxCPUUtilization)}`,
    },
    {
      id: 'memory',
      label: 'Memory 사용률',
      labelShort: 'Memory',
      rule: `기준 ${limitText(f.maxMemoryUtilization)} 이하`,
      ruleShort: `≤ ${limitText(f.maxMemoryUtilization)}`,
    },
    { id: 'available', label: '가용 Memory', labelShort: '가용', rule: `기준 ${required} 이상`, ruleShort: `≥ ${required}` },
    { id: 'requests', label: '요청 여유', labelShort: '요청', rule: '기준 0% 이상', ruleShort: '≥ 0%' },
  ];
}

/** Common axis for the available-memory bars so rows are comparable. */
export function availableAxisMax(values: ReadonlyArray<number | null>, requiredBytes: number): number {
  let max = requiredBytes * 2;
  for (const v of values) if (isPresent(v) && v > max) max = v;
  const step = max <= 4 * GiB ? 0.5 * GiB : GiB;
  return Math.ceil(max / step) * step;
}

export interface CriteriaContext {
  filter: RecoveryPolicySpec['filter'];
  availableAxisMax: number;
  /** True once the board has reached the FILTERING reveal. */
  judged: boolean;
}

/**
 * Judgement follows the evaluator's own reject reasons rather than re-comparing
 * numbers, so a bar can never disagree with the chips next to it.
 */
export function buildCriteria(c: CandidateEvaluation, ctx: CriteriaContext): CriterionModel[] {
  const m = c.resourceCondition?.appliedMeasurements ?? c.measurements;
  const reasons = new Set<RejectReason>(c.rejectReasons);
  const stale = reasons.has('METRICS_STALE');
  const missingAny = reasons.has('METRICS_MISSING');
  const required = ctx.filter.minimumAvailableMemoryBytes + ctx.filter.startupMemoryBudgetBytes;

  function metricState(value: number | null, violated: boolean): CriterionState {
    if (!isPresent(value)) return 'missing';
    if (!ctx.judged) return 'pending';
    if (stale) return 'stale';
    if (missingAny) return 'held';
    return violated ? 'fail' : 'pass';
  }

  function word(id: CriterionId, state: CriterionState): string {
    if (state === 'pass') return STATE_WORD[id].pass;
    if (state === 'fail') return STATE_WORD[id].fail;
    if (state === 'stale') return '오래됨';
    if (state === 'held') return '판정 보류';
    return '';
  }

  /** CPU and Memory utilization: lower is better, the limit is a ceiling. */
  function utilization(id: 'cpu' | 'memory', value: number | null, limit: number, violated: boolean): CriterionModel {
    const state = metricState(value, violated);
    const present = isPresent(value);
    return {
      id,
      state,
      valueText: present ? percentText(value, limit) : MISSING_TEXT,
      stateWord: word(id, state),
      fillFrom: 0,
      fillTo: present ? clamp01(value) : 0,
      marker: clamp01(limit),
      zoneFrom: clamp01(limit),
      zoneTo: 1,
    };
  }

  const cpu = utilization('cpu', m.cpuUtilization, ctx.filter.maxCPUUtilization, reasons.has('CPU_PRESSURE'));
  // MEMORY_PRESSURE also comes from the node's MemoryPressure condition; the bar
  // only turns red when the utilization itself is over the policy limit.
  const memory = utilization(
    'memory',
    m.memoryUtilization,
    ctx.filter.maxMemoryUtilization,
    reasons.has('MEMORY_PRESSURE') &&
      isPresent(m.memoryUtilization) &&
      m.memoryUtilization > ctx.filter.maxMemoryUtilization,
  );

  const availState = metricState(m.memAvailableBytes, reasons.has('INSUFFICIENT_AVAILABLE_MEMORY'));
  const availMarker = clamp01(required / ctx.availableAxisMax);
  const availableBytes = isPresent(m.memAvailableBytes) ? m.memAvailableBytes : null;
  const available: CriterionModel = {
    id: 'available',
    state: availState,
    valueText: availableBytes === null ? MISSING_TEXT : bytes(availableBytes),
    stateWord: word('available', availState),
    fillFrom: 0,
    fillTo: availableBytes === null ? 0 : clamp01(availableBytes / ctx.availableAxisMax),
    marker: availMarker,
    zoneFrom: 0,
    zoneTo: availMarker,
  };

  const headroom = Math.min(m.requestedCpuHeadroom, m.requestedMemoryHeadroom);
  const violatesRequests =
    reasons.has('INSUFFICIENT_REQUESTED_CPU') || reasons.has('INSUFFICIENT_REQUESTED_MEMORY') || headroom < 0;
  const reqState: CriterionState = !ctx.judged ? 'pending' : violatesRequests ? 'fail' : 'pass';
  const zero = axisPos(0, REQUEST_AXIS.lo, REQUEST_AXIS.hi);
  const at = axisPos(headroom, REQUEST_AXIS.lo, REQUEST_AXIS.hi);
  const requests: CriterionModel = {
    id: 'requests',
    state: reqState,
    valueText: percentText(headroom, 0),
    stateWord: word('requests', reqState),
    fillFrom: Math.min(zero, at),
    fillTo: Math.max(zero, at),
    marker: zero,
    zoneFrom: 0,
    zoneTo: zero,
  };

  return [cpu, memory, available, requests];
}

// ---------------------------------------------------------------------------
// Score
// ---------------------------------------------------------------------------

export type ScorePartId = 'cpu' | 'memory' | 'requests';
/** Fixed stacking order of the score bar, left to right. */
export const SCORE_PART_IDS: readonly ScorePartId[] = ['cpu', 'memory', 'requests'];

export interface ScoreModel {
  total: number;
  totalText: string;
  parts: Array<{ id: ScorePartId; label: string; points: number; pointsText: string }>;
  /** Points removed for recent failures; 0 when none. */
  penalty: number;
  penaltyText: string;
  /** Sum of the three weighted contributions before the penalty. */
  gross: number;
}

export const SCORE_PART_LABEL: Record<ScorePartId, string> = { cpu: 'CPU', memory: 'Memory', requests: '요청' };

export function buildScore(score: NonNullable<CandidateEvaluation['score']>): ScoreModel {
  const parts = SCORE_PART_IDS.map((id) => ({
    id,
    label: SCORE_PART_LABEL[id],
    points: score.weighted[id],
    pointsText: score.weighted[id].toFixed(1),
  }));
  return {
    total: score.total,
    totalText: score.total.toFixed(1),
    parts,
    penalty: score.penalty,
    penaltyText: `−${score.penalty}`,
    gross: parts.reduce((sum, p) => sum + p.points, 0),
  };
}

/** The formula line, with weights read from the policy instead of hard-coded. */
export function formulaText(score: RecoveryPolicySpec['score']): string {
  const w = (x: number) => x.toFixed(2);
  return `점수 = 100 × (${w(score.cpuWeight)}×CPU 여유 + ${w(score.memoryWeight)}×Memory 여유 + ${w(score.requestsWeight)}×요청 여유) − 최근 실패 감점(${penaltyText(score)})`;
}

/** "15점/회, 최대 45점, 최근 30분" — cap and window only when the policy sets them. */
export function penaltyText(score: RecoveryPolicySpec['score']): string {
  const parts = [`${score.recentFailurePenalty}점/회`];
  if (typeof score.recentFailurePenaltyCap === 'number') parts.push(`최대 ${score.recentFailurePenaltyCap}점`);
  if (typeof score.failureHistoryWindowSeconds === 'number') {
    const s = score.failureHistoryWindowSeconds;
    parts.push(`최근 ${s % 60 === 0 ? `${s / 60}분` : `${s}초`}`);
  }
  return parts.join(', ');
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

export type Verdict = 'pending' | 'pass' | 'reject' | 'excluded';

export interface ReasonModel {
  /** Raw enum value shown beside the label. */
  code: string;
  label: string;
  detail: string;
  /** Exclusions are not hard-filter reasons; they read differently in the UI. */
  kind: 'reason' | 'exclusion';
}

export interface RowModel {
  key: string;
  nodeName: string;
  displayName: string;
  hostname: string;
  arch: Arch;
  sampleAgeSeconds: number | null;
  criteria: CriterionModel[];
  verdict: Verdict;
  /** Exclusion first, then every reject reason; empty before the filter reveal. */
  reasons: ReasonModel[];
  score: ScoreModel | null;
  /** 1-based rank among scored candidates; null when not scored or not yet revealed. */
  rank: number | null;
  /** The target OpsLoop selected (decide stage only). */
  winner: boolean;
  /** Current best in a preview. Never uses the "selected" styling. */
  leader: boolean;
}

const EXCLUSION_LABEL: Record<CandidateExclusion, string> = {
  FAILED_NODE: '장애 노드 · 평가에서 제외',
  NOT_A_CANDIDATE: '후보 노드 아님 · 평가에서 제외',
};

function exclusionDetail(exclusion: CandidateExclusion, policy: RecoveryPolicySpec): string {
  return exclusion === 'FAILED_NODE'
    ? '이 Incident를 일으킨 노드입니다. 후보에서 빼고 점수도 매기지 않습니다.'
    : `${policy.candidateSelector} 라벨이 없는 노드라 복구 후보가 아닙니다.`;
}

export function buildReasons(
  c: CandidateEvaluation,
  policy: RecoveryPolicySpec,
  cooldownRemainingSeconds: number | null,
): ReasonModel[] {
  const list: ReasonModel[] = [];
  if (c.exclusion) {
    list.push({
      code: c.exclusion,
      label: EXCLUSION_LABEL[c.exclusion],
      detail: exclusionDetail(c.exclusion, policy),
      kind: 'exclusion',
    });
  }
  for (const reason of c.rejectReasons) {
    const text = REJECT_REASON_LABEL[reason];
    const cooling = reason === 'COOLDOWN' && cooldownRemainingSeconds !== null;
    list.push({
      code: reason,
      label: text.short,
      detail: cooling ? `${text.detail} 남은 시간은 약 ${cooldownRemainingSeconds}초입니다.` : text.detail,
      kind: 'reason',
    });
  }
  return list;
}

function verdictOf(c: CandidateEvaluation, revealed: boolean): Verdict {
  if (!revealed) return 'pending';
  if (c.exclusion) return 'excluded';
  return c.rejectReasons.length > 0 || !c.score ? 'reject' : 'pass';
}

interface RowsInput {
  candidates: CandidateEvaluation[];
  snapshot: DashboardSnapshot;
  stage: RevealStage;
  selectedNodeUid: string | null;
  leaderNodeUid: string | null;
}

export function buildRows(input: RowsInput): RowModel[] {
  const { candidates, snapshot, stage } = input;
  const policy = snapshot.policy.spec;
  const judged = stageAtLeast(stage, 'filter');
  const scored = stageAtLeast(stage, 'score');
  const decided = stage === 'decide';

  const byUid = new Map<string, ClusterNode>();
  const byName = new Map<string, ClusterNode>();
  for (const n of snapshot.cluster.nodes) {
    byUid.set(n.uid, n);
    byName.set(n.name, n);
  }
  const axisMax = availableAxisMax(
    candidates.map((c) => c.measurements.memAvailableBytes),
    policy.filter.minimumAvailableMemoryBytes + policy.filter.startupMemoryBudgetBytes,
  );

  const ranked = candidates
    .filter((c) => c.score !== undefined)
    .sort((a, b) => b.score!.total - a.score!.total || compareNodeNames(a.nodeName, b.nodeName))
    .map((c) => c.nodeUid);

  return candidates.map((c): RowModel => {
    const node = byUid.get(c.nodeUid) ?? byName.get(c.nodeName);
    const cooldown = snapshot.policy.status.cooldowns.find(
      (e) => e.workloadUid === snapshot.workload.uid && e.nodeName === c.nodeName && e.until > snapshot.observedAt,
    );
    const remaining = cooldown ? Math.ceil((cooldown.until - snapshot.observedAt) / 1000) : null;
    const rank = scored && c.score ? ranked.indexOf(c.nodeUid) + 1 : null;
    return {
      key: c.nodeUid,
      nodeName: c.nodeName,
      displayName: node?.displayName ?? c.nodeName,
      hostname: c.hostname,
      arch: c.arch,
      sampleAgeSeconds: c.measurements.sampleAgeSeconds,
      criteria: buildCriteria(c, { filter: policy.filter, availableAxisMax: axisMax, judged }),
      verdict: verdictOf(c, judged),
      reasons: judged ? buildReasons(c, policy, remaining) : [],
      score: scored && c.score ? buildScore(c.score) : null,
      rank,
      winner: decided && input.selectedNodeUid !== null && c.nodeUid === input.selectedNodeUid,
      leader: !decided && scored && input.leaderNodeUid !== null && c.nodeUid === input.leaderNodeUid,
    };
  });
}

// ---------------------------------------------------------------------------
// Board
// ---------------------------------------------------------------------------

/**
 * preview-idle     no Incident: the evaluator run live on current metrics
 * preview-pending  Incident exists but has not reached candidate collection
 * preview-ended    Incident ended without ever evaluating candidates
 * collecting       COLLECTING_CANDIDATES: live measurements, no verdicts
 * decision         a recorded decision, revealed up to the current phase
 */
export type BoardKind = 'preview-idle' | 'preview-pending' | 'preview-ended' | 'collecting' | 'decision';

export interface WaitingModel {
  tone: 'waiting' | 'ended';
  message: string;
  codes: string[];
}

export interface MetaModel {
  actionId: string | null;
  policyHash: string;
  /** Seconds the decision snapshot stays valid; null without a decision. */
  validSeconds: number | null;
}

export interface BoardModel {
  historical?: boolean;
  kind: BoardKind;
  stage: RevealStage;
  phase: RuntimePhase | null;
  incidentName: string | null;
  columns: ColumnModel[];
  rows: RowModel[];
  counts: { eligible: number; rejected: number; excluded: number };
  attempt: { no: number; max: number } | null;
  attemptResult: ActionResult | null;
  winner: { name: string; actionLabel: string } | null;
  leaderName: string | null;
  waiting: WaitingModel | null;
  meta: MetaModel;
  formula: string;
  aside: { label: string; code: string | null; lamp: LampState };
}

/** Current-state preview only. Persisted Controller decisions use decisionModel unchanged. */
export function evaluateNow(snapshot: DashboardSnapshot) {
  const incident = displayIncident(snapshot);
  const trigger = incident?.spec.trigger;
  const input: EvaluationInput = {
    nodes: snapshot.cluster.nodes,
    requireRawMetrics: snapshot.source !== 'demo',
    workload: snapshot.workload,
    policy: snapshot.policy.spec,
    now: snapshot.observedAt,
    cooldowns: snapshot.policy.status.cooldowns,
    recentFailures: [
      ...(snapshot.policy.status.recentFailures ?? []),
      ...failuresFromAttempts(incident?.status.runtime.attempts),
    ],
    ...(trigger?.type === 'NodeNotReady' ? { failedNodeName: trigger.nodeName } : {}),
  };
  return evaluateCandidates(input);
}

export function phaseLamp(phase: RuntimePhase): LampState {
  switch (phase) {
    case 'WAITING':
      return 'caution';
    case 'TARGET_SELECTED':
    case 'EXECUTING':
    case 'VERIFYING':
      return 'act';
    case 'RECOVERED':
      return 'ok';
    case 'FAILED':
    case 'RUNTIME_RECOVERY_EXHAUSTED':
    case 'MANUAL_REQUIRED':
      return 'fault';
    default:
      return 'off';
  }
}

function countRows(rows: RowModel[]) {
  return {
    eligible: rows.filter((r) => r.score !== null || r.verdict === 'pass').length,
    rejected: rows.filter((r) => r.verdict === 'reject').length,
    excluded: rows.filter((r) => r.verdict === 'excluded').length,
  };
}

function attemptNumber(rt: IncidentRuntimeStatus, decision: RecoveryDecision | undefined): number {
  if (decision) {
    const i = rt.attempts.findIndex((a) => a.actionId === decision.actionId);
    if (i >= 0) return i + 1;
  }
  return rt.attempts.length + 1;
}

function waitingModel(
  rt: IncidentRuntimeStatus,
  hasEligible: boolean,
  stage: RevealStage,
): WaitingModel | null {
  const reason: WaitingReason = rt.waitingReason ?? 'NO_CANDIDATE';
  if (rt.phase === 'WAITING') {
    return {
      tone: 'waiting',
      message:
        reason === 'METRICS_UNAVAILABLE'
          ? '관측값 부족 · 근거 없이 옮기지 않고 기다립니다'
          : '적격 후보 없음 · 점수와 상관없이 기다립니다',
      codes: ['WAITING', reason],
    };
  }
  if (stage === 'decide' && !hasEligible) {
    return {
      tone: 'ended',
      message: '적격 후보 없음 · 기다렸지만 끝내 후보가 나타나지 않았습니다',
      codes: [rt.phase, reason],
    };
  }
  return null;
}

function baseModel(snapshot: DashboardSnapshot) {
  return {
    columns: columnModels(snapshot.policy.spec),
    formula: formulaText(snapshot.policy.spec.score),
    policyHash: shortHash(snapshot.policy.policyHash, 16),
  };
}

function previewModel(snapshot: DashboardSnapshot, kind: Extract<BoardKind, `preview-${string}`>): BoardModel {
  const result = evaluateNow(snapshot);
  const rows = buildRows({
    candidates: result.candidates,
    snapshot,
    stage: 'score',
    selectedNodeUid: null,
    leaderNodeUid: result.selected?.nodeUid ?? null,
  });
  const base = baseModel(snapshot);
  const incident = displayIncident(snapshot);
  return {
    kind,
    stage: 'score',
    phase: incident?.status.runtime.phase ?? null,
    incidentName: incident?.name ?? null,
    columns: base.columns,
    rows,
    counts: countRows(rows),
    attempt: null,
    attemptResult: null,
    winner: null,
    leaderName: rows.find((r) => r.leader)?.displayName ?? null,
    waiting: null,
    meta: { actionId: null, policyHash: base.policyHash, validSeconds: null },
    formula: base.formula,
    aside: { label: '미리보기', code: null, lamp: 'off' },
  };
}

function collectingModel(snapshot: DashboardSnapshot, rt: IncidentRuntimeStatus, name: string): BoardModel {
  const result = evaluateNow(snapshot);
  const rows = buildRows({
    candidates: result.candidates,
    snapshot,
    stage: 'collect',
    selectedNodeUid: null,
    leaderNodeUid: null,
  });
  const base = baseModel(snapshot);
  return {
    kind: 'collecting',
    stage: 'collect',
    phase: rt.phase,
    incidentName: name,
    columns: base.columns,
    rows,
    counts: countRows(rows),
    attempt: { no: rt.attempts.length + 1, max: rt.budget.max },
    attemptResult: null,
    winner: null,
    leaderName: null,
    waiting: null,
    meta: { actionId: null, policyHash: base.policyHash, validSeconds: null },
    formula: base.formula,
    aside: { label: RUNTIME_PHASE_LABEL[rt.phase], code: rt.phase, lamp: phaseLamp(rt.phase) },
  };
}

function decisionModel(
  snapshot: DashboardSnapshot,
  rt: IncidentRuntimeStatus,
  name: string,
  decision: RecoveryDecision,
  stage: RevealStage,
): BoardModel {
  const rows = buildRows({
    candidates: decision.candidates,
    snapshot,
    stage,
    selectedNodeUid: decision.selectedNodeUid,
    leaderNodeUid: null,
  });
  const base = baseModel(snapshot);
  const winnerRow = rows.find((r) => r.winner);
  const attempt = rt.attempts.find((a) => a.actionId === decision.actionId);
  const hasEligible = decision.candidates.some((c) => c.score !== undefined);
  return {
    kind: 'decision',
    stage,
    phase: rt.phase,
    incidentName: name,
    columns: base.columns,
    rows,
    counts: countRows(rows),
    attempt: rt.phase === 'OBSERVED' && rt.attempts.length === 0 ? null : { no: attemptNumber(rt, decision), max: rt.budget.max },
    attemptResult: attempt?.result ?? null,
    winner: winnerRow ? { name: winnerRow.displayName, actionLabel: rt.phase === 'OBSERVED' ? 'ObserveOnly 선택 기록 · 배치 변경 없음' : actionTypeLabel(decision.actionType) } : null,
    leaderName: null,
    waiting: waitingModel(rt, hasEligible, stage),
    meta: {
      actionId: decision.actionId,
      policyHash: shortHash(decision.policyHash, 16),
      validSeconds: Math.max(0, Math.round((decision.snapshotExpiresAt - decision.snapshotObservedAt) / 1000)),
    },
    formula: base.formula,
    aside: { label: RUNTIME_PHASE_LABEL[rt.phase], code: rt.phase, lamp: phaseLamp(rt.phase) },
  };
}

export function buildBoardModel(snapshot: DashboardSnapshot): BoardModel {
  return {...boardModel(snapshot), historical: snapshot.source === 'replay'};
}
function boardModel(snapshot: DashboardSnapshot): BoardModel {
  const incident = displayIncident(snapshot);
  if (!incident) return previewModel(snapshot, 'preview-idle');

  const rt = incident.status.runtime;
  const stage = revealStageForPhase(rt.phase);
  if (stage === null) return previewModel(snapshot, 'preview-pending');
  if (stage === 'collect') return collectingModel(snapshot, rt, incident.name);
  if (!rt.decision) return previewModel(snapshot, 'preview-ended');
  return decisionModel(snapshot, rt, incident.name, rt.decision, stage);
}

// ---------------------------------------------------------------------------
// Strip copy
// ---------------------------------------------------------------------------

export const ATTEMPT_RESULT_LABEL: Record<ActionResult, string> = {
  pending: '조치 진행 중',
  placed: '새 Pod 배치됨',
  verified: 'HTTP 검증 통과',
  failed: '검증 실패 · cooldown',
  'timed-out': '시간 초과',
};

export interface StripText {
  lead: string;
  rest: string;
  side: string;
}

export function describeStrip(model: BoardModel): StripText {
  if (model.historical && model.kind.startsWith('preview-')) return {lead: '해당 보존 시점에 후보 Decision 없음', rest: '과거에 없는 관측값으로 미리보기를 만들지 않습니다.', side: 'Evidence Replay'};
  const attempt = model.attempt ? `attempt ${model.attempt.no}/${model.attempt.max}` : '';
  switch (model.kind) {
    case 'preview-idle':
      return {
        lead: '지금 장애가 나면?',
        rest: '(현재 지표 기준 미리보기 · Incident 없음)',
        side: model.leaderName ? `현재 1순위 ${model.leaderName}` : '현재 적격 후보 없음',
      };
    case 'preview-pending':
      return {
        lead: '후보 평가 전',
        rest: '(현재 지표 기준 미리보기 · Incident 진행 중)',
        side: model.leaderName ? `현재 1순위 ${model.leaderName}` : '현재 적격 후보 없음',
      };
    case 'preview-ended':
      return {
        lead: '이 Incident는 후보를 평가하지 않았습니다',
        rest: '(현재 지표 기준 미리보기)',
        side: model.leaderName ? `현재 1순위 ${model.leaderName}` : '현재 적격 후보 없음',
      };
    case 'collecting':
      return {
        lead: '근거 수집',
        rest: 'Kubernetes의 요청량과 Prometheus의 실사용량을 모읍니다. 판정은 아직 없습니다.',
        side: attempt,
      };
    case 'decision': {
      if (model.winner) {
        return {
          lead: `선택 ${model.winner.name}`,
          rest: model.winner.actionLabel,
          side: [attempt, model.attemptResult ? ATTEMPT_RESULT_LABEL[model.attemptResult] : ''].filter(Boolean).join(' / '),
        };
      }
      if (model.stage === 'score') {
        return { lead: '점수 계산', rest: '통과한 후보만 실제 자원 여유로 점수를 매깁니다.', side: attempt };
      }
      const c = model.counts;
      return {
        lead: 'Hard filter',
        rest: '조건을 하나라도 어기면 탈락하고, 이유는 모두 기록됩니다.',
        side: `${attempt ? attempt + ' / ' : ''}적격 ${c.eligible}, 탈락 ${c.rejected}, 제외 ${c.excluded}`,
      };
    }
  }
}
