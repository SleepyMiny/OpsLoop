import { ENGINEERING_MAIN_PATH } from '../../domain/labels';
import type {
  EngineeringPhase,
  IncidentCondition,
  IncidentEngineeringStatus,
  StageResult,
  VerificationStage,
} from '../../domain/types';
import type { LampState } from '../shared/primitives';

/** Pure model behind the Engineering Runner track. */

export type TrackState = 'done' | 'current' | 'pending' | 'failed';
export type FeedbackState = 'idle' | 'taken' | 'current';

export interface TrackStep {
  phase: EngineeringPhase;
  state: TrackState;
}

export interface TerminalPhase {
  phase: EngineeringPhase;
  tone: LampState;
}

export interface TrackModel {
  steps: TrackStep[];
  feedback: FeedbackState;
  /** Set when the Runner stopped on a phase that is not on the main path. */
  terminal: TerminalPhase | null;
}

/** The track station each verification stage belongs to. */
export const STAGE_PHASE: Record<VerificationStage, EngineeringPhase> = {
  build: 'BUILDING',
  unit: 'TESTING',
  integration: 'TESTING',
  'regression-fixture': 'TESTING',
  'multi-arch-smoke': 'VERIFYING',
  'independent-review': 'INDEPENDENT_REVIEW',
};

const PATH = ENGINEERING_MAIN_PATH;
const at = (phase: EngineeringPhase): number => PATH.indexOf(phase);

/** Off-path phases that end the run, and how they should be coloured. */
const TERMINAL_TONE: Partial<Record<EngineeringPhase, LampState>> = {
  ENGINEERING_FAILED: 'fault',
  INVALID_BUNDLE: 'fault',
  BLOCKED_EVIDENCE: 'caution',
  BLOCKED_ENVIRONMENT: 'caution',
  BLOCKED_NATIVE_VALIDATION: 'caution',
  STALE_BASE: 'caution',
  TIMED_OUT: 'fault',
  CANCELLED: 'caution',
  PUBLISH_RETRY_PENDING: 'caution',
};

export const isTerminalPhase = (phase: EngineeringPhase): boolean => phase in TERMINAL_TONE;

/** Highest station reached by the newest iteration's stages (-1 when none ran). */
function reachedByStages(stages: StageResult[], iteration: number): number {
  let reached = -1;
  for (const s of stages) {
    if (s.iteration === iteration) reached = Math.max(reached, at(STAGE_PHASE[s.stage]));
  }
  return reached;
}

function failedStageIndex(eng: IncidentEngineeringStatus): number {
  const failed = eng.stages.filter((s) => s.iteration === eng.iteration && s.outcome === 'fail');
  if (failed.length === 0) return -1;
  return Math.max(...failed.map((s) => at(STAGE_PHASE[s.stage])));
}

export function buildTrack(eng: IncidentEngineeringStatus | undefined): TrackModel {
  const count = PATH.length;
  const steps = (states: TrackState[]): TrackStep[] => PATH.map((phase, i) => ({ phase, state: states[i] ?? 'pending' }));
  const blank = (): TrackState[] => Array.from({ length: count }, () => 'pending');

  if (!eng) return { steps: steps(blank()), feedback: 'idle', terminal: null };

  const hadFailure = eng.stages.some((s) => s.outcome === 'fail');
  const loopTaken = eng.iteration > 1 || hadFailure;
  const states = blank();
  const idx = at(eng.phase);

  if (eng.phase === 'PR_CREATED') {
    states.fill('done');
    return { steps: steps(states), feedback: loopTaken ? 'taken' : 'idle', terminal: null };
  }

  if (idx >= 0) {
    for (let i = 0; i < count; i++) states[i] = i < idx ? 'done' : i === idx ? 'current' : 'pending';
    return { steps: steps(states), feedback: loopTaken ? 'taken' : 'idle', terminal: null };
  }

  if (eng.phase === 'FEEDBACK') {
    const failed = failedStageIndex(eng);
    const stop = failed >= 0 ? failed : Math.max(reachedByStages(eng.stages, eng.iteration), at('BUILDING'));
    for (let i = 0; i < count; i++) states[i] = i < stop ? 'done' : 'pending';
    states[stop] = 'failed';
    return { steps: steps(states), feedback: 'current', terminal: null };
  }

  // Off-path terminal phases: reconstruct how far the run got.
  const reached = Math.max(
    reachedByStages(eng.stages, eng.iteration),
    eng.iteration > 0 ? at('ANALYZING') : at('VALIDATING_INPUT'),
  );
  for (let i = 0; i < count; i++) states[i] = i < reached ? 'done' : 'pending';
  const tone = TERMINAL_TONE[eng.phase] ?? 'caution';
  states[reached] = tone === 'fault' ? 'failed' : 'done';
  return {
    steps: steps(states),
    feedback: loopTaken ? 'taken' : 'idle',
    terminal: { phase: eng.phase, tone },
  };
}

// --- Iterations -----------------------------------------------------------

export type IterationOutcome = 'pass' | 'fail' | 'running';

export interface IterationGroup {
  iteration: number;
  stages: StageResult[];
  outcome: IterationOutcome;
  /** First failed stage, whose summary is what the next analysis receives. */
  failure: StageResult | null;
}

export function groupByIteration(stages: StageResult[]): IterationGroup[] {
  const byIteration = new Map<number, StageResult[]>();
  for (const s of stages) {
    const list = byIteration.get(s.iteration);
    if (list) list.push(s);
    else byIteration.set(s.iteration, [s]);
  }
  return [...byIteration.entries()]
    .sort(([a], [b]) => a - b)
    .map(([iteration, list]) => {
      const failure = list.find((s) => s.outcome === 'fail') ?? null;
      const outcome: IterationOutcome = failure ? 'fail' : list.some((s) => s.outcome === 'running') ? 'running' : 'pass';
      return { iteration, stages: list, outcome, failure };
    });
}

// --- Gate and runtime state -----------------------------------------------

/** Condition recorded when the incident must not go to Engineering. */
export function engineeringBlock(conditions: IncidentCondition[]): IncidentCondition | null {
  return conditions.find((c) => c.type === 'EngineeringEligible' && c.status === 'False') ?? null;
}

export const PR_PHASES: readonly EngineeringPhase[] = ['PR_READY', 'PR_CREATED'];
