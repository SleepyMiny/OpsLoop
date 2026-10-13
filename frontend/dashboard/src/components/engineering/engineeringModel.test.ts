import { describe, expect, it } from 'vitest';
import { DemoSimulation } from '../../data/demo/engine';
import { DEMO_SCENARIOS } from '../../data/demo/scenarios';
import type { DashboardSnapshot, EngineeringPhase, IncidentEngineeringStatus, StageResult } from '../../domain/types';
import { buildTrack, engineeringBlock, groupByIteration } from './engineeringModel';
import { runtimeAlongside } from './runtimeState';

function playUntil(id: string, predicate: (s: DashboardSnapshot) => boolean): DashboardSnapshot {
  const scenario = DEMO_SCENARIOS.find((s) => s.id === id)!;
  const sim = new DemoSimulation(scenario.script);
  for (let t = 0; t <= sim.durationMs; t += 500) {
    sim.advanceTo(t);
    const snap = sim.snapshot();
    if (predicate(snap)) return snap;
  }
  throw new Error(`predicate never held in ${id}`);
}

const engPhase =
  (phase: EngineeringPhase, iteration?: number) =>
  (s: DashboardSnapshot): boolean => {
    const e = s.activeIncident?.status.engineering;
    return e?.phase === phase && (iteration === undefined || e.iteration === iteration);
  };

const states = (e: IncidentEngineeringStatus | undefined) => buildTrack(e).steps.map((s) => s.state);

const stage = (s: Partial<StageResult> & Pick<StageResult, 'stage'>): StageResult => ({
  iteration: 1,
  outcome: 'pass',
  summary: '',
  ...s,
});

function eng(partial: Partial<IncidentEngineeringStatus> & { phase: EngineeringPhase }): IncidentEngineeringStatus {
  return { jobId: 'job', iteration: 1, maxIterations: 3, baseCommit: 'abc', stages: [], ...partial };
}

describe('buildTrack', () => {
  it('is all pending, with the loop idle, before any handoff', () => {
    const track = buildTrack(undefined);
    expect(track.steps.every((s) => s.state === 'pending')).toBe(true);
    expect(track.feedback).toBe('idle');
    expect(track.terminal).toBeNull();
  });

  it('marks stations before the current phase done', () => {
    const snap = playUntil('application-failure-pr', engPhase('PATCHING', 1));
    expect(states(snap.activeIncident!.status.engineering)).toEqual([
      'done', 'done', 'done', 'current', 'pending', 'pending', 'pending', 'pending', 'pending', 'pending',
    ]);
    expect(buildTrack(snap.activeIncident!.status.engineering).feedback).toBe('idle');
  });

  it('shows the failing stage and the loop as current during FEEDBACK', () => {
    const snap = playUntil('application-failure-pr', engPhase('FEEDBACK'));
    const track = buildTrack(snap.activeIncident!.status.engineering);
    expect(track.feedback).toBe('current');
    // The regression fixture failed in TESTING (index 5).
    expect(track.steps[5]).toEqual({ phase: 'TESTING', state: 'failed' });
    expect(track.steps.slice(0, 5).every((s) => s.state === 'done')).toBe(true);
    expect(track.steps.slice(6).every((s) => s.state === 'pending')).toBe(true);
  });

  it('remembers the loop was taken once iteration 2 runs', () => {
    const snap = playUntil('application-failure-pr', engPhase('ANALYZING', 2));
    const track = buildTrack(snap.activeIncident!.status.engineering);
    expect(track.feedback).toBe('taken');
    expect(track.steps[2]!.state).toBe('current');
  });

  it('finishes every station on PR_CREATED', () => {
    const snap = playUntil('application-failure-pr', engPhase('PR_CREATED'));
    const track = buildTrack(snap.activeIncident!.status.engineering);
    expect(track.steps.every((s) => s.state === 'done')).toBe(true);
    expect(track.feedback).toBe('taken');
  });

  it('keeps PR_READY as the current station', () => {
    const track = buildTrack(eng({ phase: 'PR_READY', iteration: 2 }));
    expect(track.steps[8]!.state).toBe('current');
    expect(track.steps[9]!.state).toBe('pending');
  });

  it('reports an off-path stop as a terminal phase with how far the run got', () => {
    const track = buildTrack(
      eng({ phase: 'ENGINEERING_FAILED', iteration: 3, stages: [stage({ stage: 'build', iteration: 3 }), stage({ stage: 'unit', iteration: 3, outcome: 'fail' })] }),
    );
    expect(track.terminal).toEqual({ phase: 'ENGINEERING_FAILED', tone: 'fault' });
    expect(track.steps[5]!.state).toBe('failed'); // TESTING
    expect(track.steps[6]!.state).toBe('pending');

    const blocked = buildTrack(eng({ phase: 'BLOCKED_EVIDENCE', iteration: 0 }));
    expect(blocked.terminal!.tone).toBe('caution');
    expect(blocked.steps.some((s) => s.state === 'failed')).toBe(false);
  });
});

describe('groupByIteration', () => {
  it('groups stages by iteration with an outcome and the first failure', () => {
    const groups = groupByIteration([
      stage({ stage: 'build', iteration: 1 }),
      stage({ stage: 'regression-fixture', iteration: 1, outcome: 'fail', summary: '빈 집합' }),
      stage({ stage: 'build', iteration: 2 }),
      stage({ stage: 'unit', iteration: 2, outcome: 'running' }),
    ]);
    expect(groups.map((g) => [g.iteration, g.outcome])).toEqual([
      [1, 'fail'],
      [2, 'running'],
    ]);
    expect(groups[0]!.failure!.summary).toBe('빈 집합');
    expect(groups[1]!.failure).toBeNull();
  });

  it('orders iterations numerically and tolerates no stages', () => {
    expect(groupByIteration([])).toEqual([]);
    const groups = groupByIteration([stage({ stage: 'build', iteration: 2 }), stage({ stage: 'build', iteration: 1 })]);
    expect(groups.map((g) => g.iteration)).toEqual([1, 2]);
  });

  it('passes an iteration only when every reported stage passed', () => {
    const groups = groupByIteration([stage({ stage: 'build' }), stage({ stage: 'unit' })]);
    expect(groups[0]!.outcome).toBe('pass');
  });
});

describe('engineeringBlock', () => {
  it('finds the EngineeringEligible=False condition the no-candidate scenario records', () => {
    const snap = playUntil('no-candidate', (s) => s.activeIncident?.status.runtime.phase === 'FAILED');
    const block = engineeringBlock(snap.activeIncident!.status.conditions);
    expect(block).not.toBeNull();
    expect(block!.reason).toBe('NoCandidate');
    expect(snap.activeIncident!.status.engineering).toBeUndefined();
  });

  it('ignores other conditions', () => {
    expect(
      engineeringBlock([{ type: 'Recovered', status: 'False', reason: 'x', message: 'm', lastTransitionTime: 0 }]),
    ).toBeNull();
  });
});

describe('runtimeAlongside', () => {
  it('says the Demo API is quarantined once the handoff confirmed replicas=0', () => {
    const snap = playUntil('application-failure-pr', engPhase('PR_CREATED'));
    expect(runtimeAlongside(snap)).toEqual({ tone: 'fault', state: '격리 중', fact: 'replicas=0' });
  });

  it('does not call the service recovered because a PR exists', () => {
    const snap = playUntil('application-failure-pr', engPhase('PR_CREATED'));
    expect(runtimeAlongside(snap).tone).not.toBe('ok');
  });

  it('shows a healthy watch state with no incident', () => {
    const snap = playUntil('node-failure-laptop', () => true);
    expect(runtimeAlongside(snap)).toMatchObject({ tone: 'ok', fact: 'replicas=1' });
  });
});
