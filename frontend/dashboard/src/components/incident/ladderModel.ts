import { RUNTIME_MAIN_PATH, RUNTIME_PHASE_LABEL } from '../../domain/labels';
import type { IncidentRuntimeStatus, RecoveryPolicySpec, RuntimePhase } from '../../domain/types';
import type { LampState } from '../shared/primitives';

/**
 * Pure model behind the Runtime signal ladder. The ladder is a view of one
 * RecoveryIncident's `status.runtime`; nothing here reads the clock or the DOM.
 *
 * Main path:   DETECTED ... VERIFYING, RECOVERED          (RUNTIME_MAIN_PATH)
 * Branch lane: WAITING, COOLDOWN       loop back to COLLECTING_CANDIDATES
 *              RUNTIME_RECOVERY_EXHAUSTED, EVIDENCE_FINALIZING, ENGINEERING_HANDOFF
 *              FAILED                  terminal operational failure
 */

export type StepState = 'done' | 'current' | 'pending' | 'failed';
export type LaneId =
  | 'WAITING'
  | 'COOLDOWN'
  | 'RUNTIME_RECOVERY_EXHAUSTED'
  | 'EVIDENCE_FINALIZING'
  | 'ENGINEERING_HANDOFF'
  | 'FAILED';
/** Phases that stop the incident without being part of the drawn lane. */
export type StopPhase = 'ABORTED' | 'SUPERSEDED' | 'UNSUPPORTED_WORKLOAD' | 'MANUAL_REQUIRED' | 'OBSERVED';
export type LaneState = 'off' | 'visited' | 'current';

export const LANE_ORDER: readonly LaneId[] = [
  'WAITING',
  'COOLDOWN',
  'RUNTIME_RECOVERY_EXHAUSTED',
  'EVIDENCE_FINALIZING',
  'ENGINEERING_HANDOFF',
  'FAILED',
];

/** Lamp colour of a branch station once it is lit. */
export const LANE_TONE: Record<LaneId | StopPhase, LampState> = {
  WAITING: 'caution',
  COOLDOWN: 'caution',
  RUNTIME_RECOVERY_EXHAUSTED: 'fault',
  EVIDENCE_FINALIZING: 'act',
  ENGINEERING_HANDOFF: 'act',
  FAILED: 'fault',
  ABORTED: 'caution',
  SUPERSEDED: 'caution',
  UNSUPPORTED_WORKLOAD: 'caution',
  MANUAL_REQUIRED: 'caution',
  OBSERVED: 'caution',
};

const STOP_PHASES: readonly RuntimePhase[] = ['ABORTED', 'SUPERSEDED', 'UNSUPPORTED_WORKLOAD', 'MANUAL_REQUIRED', 'OBSERVED'];
const EXIT_CHAIN: readonly LaneId[] = ['RUNTIME_RECOVERY_EXHAUSTED', 'EVIDENCE_FINALIZING', 'ENGINEERING_HANDOFF'];

const IDX = {
  detected: RUNTIME_MAIN_PATH.indexOf('DETECTED'),
  classified: RUNTIME_MAIN_PATH.indexOf('CLASSIFIED'),
  filtering: RUNTIME_MAIN_PATH.indexOf('FILTERING'),
  selected: RUNTIME_MAIN_PATH.indexOf('TARGET_SELECTED'),
  executing: RUNTIME_MAIN_PATH.indexOf('EXECUTING'),
  verifying: RUNTIME_MAIN_PATH.indexOf('VERIFYING'),
} as const;

export interface LadderStep {
  phase: RuntimePhase;
  state: StepState;
}
export interface LaneStation {
  phase: LaneId | StopPhase;
  state: LaneState;
}

export interface LadderModel {
  /** `idle` = no incident (or MONITORING): everything is waiting for a trigger. */
  mode: 'idle' | 'active';
  main: LadderStep[];
  lane: LaneStation[];
  /** True while the controller is still working; false once the phase is final. */
  inFlight: boolean;
}

const mainIndex = (phase: RuntimePhase): number => RUNTIME_MAIN_PATH.indexOf(phase);
export const isMainPathPhase = (phase: RuntimePhase): boolean => mainIndex(phase) >= 0;
export const isStopPhase = (phase: RuntimePhase): phase is StopPhase => STOP_PHASES.includes(phase);

/** The controller is still working on the incident (as opposed to a final phase). */
export const isInFlight = (phase: RuntimePhase): boolean =>
  phase !== 'MONITORING' &&
  phase !== 'RECOVERED' &&
  phase !== 'FAILED' &&
  phase !== 'ENGINEERING_HANDOFF' &&
  !isStopPhase(phase);

const isFailedAttempt = (result: string): boolean => result === 'failed' || result === 'timed-out';

/**
 * Highest main-path station the incident demonstrably reached, taken from facts in
 * `status.runtime` rather than from the phase name. Used for phases that branch off
 * the path (they do not say where they branched from).
 */
export function reachedIndex(rt: IncidentRuntimeStatus): number {
  let reached = IDX.detected;
  if (rt.classification) reached = Math.max(reached, IDX.classified);
  if (rt.decision) reached = Math.max(reached, IDX.filtering);
  if (rt.decision?.selectedHostname) reached = Math.max(reached, IDX.selected);
  if (rt.attempts.length > 0) reached = Math.max(reached, IDX.executing);
  const last = rt.attempts.at(-1);
  if (last && (last.result !== 'pending' || rt.timings.placedAt !== undefined)) {
    reached = Math.max(reached, IDX.verifying);
  }
  return reached;
}

/**
 * Index of the station whose result was a failure (a failed action), or -1.
 * A placement that never became Ready fails at EXECUTING, anything later at VERIFYING.
 */
export function failedIndex(rt: IncidentRuntimeStatus): number {
  const last = rt.attempts.at(-1);
  if (!last || !isFailedAttempt(last.result)) return -1;
  return rt.timings.placedAt === undefined ? IDX.executing : IDX.verifying;
}

function mainStates(phase: RuntimePhase, rt: IncidentRuntimeStatus): StepState[] {
  const count = RUNTIME_MAIN_PATH.length;
  const states: StepState[] = Array.from({ length: count }, () => 'pending');
  const fill = (upTo: number, currentIdx: number) => {
    for (let i = 0; i < count; i++) {
      if (i < upTo) states[i] = 'done';
      else if (i === currentIdx) states[i] = 'current';
    }
  };

  const idx = mainIndex(phase);
  if (phase === 'RECOVERED') {
    states.fill('done');
  } else if (idx >= 0) {
    fill(idx, idx);
  } else if (phase === 'WAITING') {
    // Waiting always follows FILTERING: the filter ran, nothing was eligible.
    fill(IDX.filtering + 1, -1);
  } else if (phase === 'COOLDOWN') {
    const failed = Math.max(failedIndex(rt), IDX.verifying);
    fill(failed, -1);
    states[failed] = 'failed';
  } else {
    // Exit chain, FAILED and the stop phases: reconstruct from facts.
    const failed = failedIndex(rt);
    if (failed >= 0) {
      fill(failed, -1);
      states[failed] = 'failed';
    } else {
      fill(reachedIndex(rt) + 1, -1);
    }
  }
  return states;
}

function laneVisited(phase: RuntimePhase, rt: IncidentRuntimeStatus): Set<LaneId> {
  const visited = new Set<LaneId>();
  if (rt.waitingReason !== undefined && phase !== 'WAITING') visited.add('WAITING');

  const failedAttempts = rt.attempts.filter((a) => isFailedAttempt(a.result));
  const last = rt.attempts.at(-1);
  const failedBeforeLast = failedAttempts.some((a) => a !== last);
  const retriedAfterLastFailure = last !== undefined && isFailedAttempt(last.result) && isMainPathPhase(phase);
  if (phase !== 'COOLDOWN' && (failedBeforeLast || retriedAfterLastFailure)) visited.add('COOLDOWN');

  const chainAt = EXIT_CHAIN.indexOf(phase as LaneId);
  if (chainAt > 0) for (const earlier of EXIT_CHAIN.slice(0, chainAt)) visited.add(earlier);
  return visited;
}

export function buildLadder(rt: IncidentRuntimeStatus | null | undefined): LadderModel {
  const idle = !rt || rt.phase === 'MONITORING';
  if (!rt || idle) {
    return {
      mode: 'idle',
      main: RUNTIME_MAIN_PATH.map((phase) => ({ phase, state: 'pending' })),
      lane: LANE_ORDER.map((phase) => ({ phase, state: 'off' })),
      inFlight: false,
    };
  }

  const states = mainStates(rt.phase, rt);
  const visited = laneVisited(rt.phase, rt);
  const lane: LaneStation[] = LANE_ORDER.map((phase) => ({
    phase,
    state: phase === rt.phase ? 'current' : visited.has(phase) ? 'visited' : 'off',
  }));
  if (isStopPhase(rt.phase)) lane.push({ phase: rt.phase, state: 'current' });

  return {
    mode: 'active',
    main: RUNTIME_MAIN_PATH.map((phase, i) => ({ phase, state: states[i]! })),
    lane,
    inFlight: isInFlight(rt.phase),
  };
}

export interface HttpProgress {
  /** Successes capped at the required count; the engine keeps counting past it. */
  shown: number;
  required: number;
  /** The consecutive-success requirement is met; the stability window may still be running. */
  met: boolean;
  /** "HTTP 연속 성공 3/5" */
  base: string;
  /** `base`, plus the remaining condition once the count is met. */
  text: string;
}

export function httpProgress(successes: number, spec: RecoveryPolicySpec['verification']): HttpProgress {
  const required = spec.consecutiveSuccesses;
  const met = successes >= required;
  const shown = Math.min(Math.max(0, successes), required);
  const base = `HTTP 연속 성공 ${shown}/${required}`;
  return { shown, required, met, base, text: met ? `${base} · 안정화 ${spec.stabilityWindowSeconds}초 확인 중` : base };
}

export interface LadderStatus {
  title: string;
  code: RuntimePhase;
  detail: string | null;
  tone: LampState;
  pulse: boolean;
}

/** One-line readout under the ladder: where the incident is and why. */
export function ladderStatus(rt: IncidentRuntimeStatus, spec: RecoveryPolicySpec): LadderStatus {
  const phase = rt.phase;
  const last = rt.attempts.at(-1);
  const { used, max } = rt.budget;
  const tone: LampState =
    phase === 'RECOVERED'
      ? 'ok'
      : phase === 'FAILED' || phase === 'RUNTIME_RECOVERY_EXHAUSTED'
        ? 'fault'
        : phase === 'WAITING' || phase === 'COOLDOWN' || isStopPhase(phase)
          ? 'caution'
          : 'act';

  let detail: string | null = null;
  switch (phase) {
    case 'WAITING':
      detail =
        rt.waitingReason === 'METRICS_UNAVAILABLE'
          ? '관측값 부족, 기다렸다가 재평가'
          : '적격 후보 없음, 기다렸다가 재평가';
      break;
    case 'COOLDOWN':
      detail = `조치 ${used}/${max} 후 재평가 (노드 cooldown ${spec.budget.cooldownSeconds}초)`;
      break;
    case 'EXECUTING':
      detail = last ? `nodeSelector를 ${last.targetNode}로 변경` : null;
      break;
    case 'VERIFYING':
      detail = last ? httpProgress(last.httpSuccesses, spec.verification).text : null;
      break;
    case 'OBSERVED':
      detail = 'ObserveOnly 관측 종료 · 실제 재배치/복구 조치 없음';
      break;
    case 'RECOVERED':
      detail = 'HTTP 검증과 안정화 통과';
      break;
    case 'RUNTIME_RECOVERY_EXHAUSTED':
      detail = `같은 장애가 조치 ${max}회 후에도 재현`;
      break;
    case 'EVIDENCE_FINALIZING':
      detail = '격리 전에 증거 Bundle 확정';
      break;
    case 'ENGINEERING_HANDOFF':
      detail = 'Engineering Runner가 이어받음';
      break;
    case 'FAILED':
      detail = '운영 조치로 해결 못 함, 사람의 판단 필요';
      break;
    default:
      detail = null;
  }

  return {
    title: RUNTIME_PHASE_LABEL[phase],
    code: phase,
    detail,
    tone,
    pulse: isInFlight(phase),
  };
}
