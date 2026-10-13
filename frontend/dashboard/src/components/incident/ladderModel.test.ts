import { describe, expect, it } from 'vitest';
import { DemoSimulation } from '../../data/demo/engine';
import { DEMO_SCENARIOS } from '../../data/demo/scenarios';
import type { DashboardSnapshot, IncidentRuntimeStatus, RuntimePhase } from '../../domain/types';
import { buildLadder, failedIndex, httpProgress, ladderStatus, reachedIndex } from './ladderModel';

/** Plays a scenario forward until `predicate` holds, so tests do not hard-code cue times. */
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

const phaseIs =
  (phase: RuntimePhase) =>
  (s: DashboardSnapshot): boolean =>
    s.activeIncident?.status.runtime.phase === phase;

const mainStates = (rt: IncidentRuntimeStatus) => buildLadder(rt).main.map((s) => s.state);
const laneStates = (rt: IncidentRuntimeStatus) =>
  Object.fromEntries(buildLadder(rt).lane.map((l) => [l.phase, l.state]));

function runtime(partial: Partial<IncidentRuntimeStatus> & { phase: RuntimePhase }): IncidentRuntimeStatus {
  return {
    attempts: [],
    budget: { used: 0, max: 2 },
    timings: { triggeredAt: 0, detectedAt: 0 },
    oldPodsOutstanding: false,
    ...partial,
  };
}

describe('buildLadder', () => {
  it('is idle without an incident or while MONITORING', () => {
    expect(buildLadder(null).mode).toBe('idle');
    expect(buildLadder(runtime({ phase: 'MONITORING' })).mode).toBe('idle');
    expect(buildLadder(null).main.every((s) => s.state === 'pending')).toBe(true);
  });

  it('marks stations before the current phase done and the rest pending', () => {
    const snap = playUntil('node-failure-laptop', phaseIs('FILTERING'));
    const states = mainStates(snap.activeIncident!.status.runtime);
    // DETECTED, CLASSIFIED, COLLECTING_CANDIDATES done; FILTERING current; the rest pending.
    expect(states).toEqual(['done', 'done', 'done', 'current', 'pending', 'pending', 'pending', 'pending', 'pending']);
  });

  it('finishes every station on RECOVERED and nothing is in flight', () => {
    const snap = playUntil('node-failure-laptop', phaseIs('RECOVERED'));
    const model = buildLadder(snap.activeIncident!.status.runtime);
    expect(model.main.every((s) => s.state === 'done')).toBe(true);
    expect(model.inFlight).toBe(false);
  });

  it('puts WAITING after FILTERING with the waiting lane lit', () => {
    const snap = playUntil('no-candidate', phaseIs('WAITING'));
    const rt = snap.activeIncident!.status.runtime;
    expect(mainStates(rt)).toEqual(['done', 'done', 'done', 'done', 'pending', 'pending', 'pending', 'pending', 'pending']);
    expect(laneStates(rt).WAITING).toBe('current');
    expect(buildLadder(rt).inFlight).toBe(true);
    expect(ladderStatus(rt, snap.policy.spec).detail).toContain('후보');
  });

  it('shows COOLDOWN after a failed verification and remembers it once re-evaluating', () => {
    const cooling = playUntil('application-failure-pr', phaseIs('COOLDOWN'));
    const rt = cooling.activeIncident!.status.runtime;
    const states = mainStates(rt);
    expect(states[7]).toBe('failed'); // VERIFYING
    expect(states.slice(0, 7).every((s) => s === 'done')).toBe(true);
    expect(laneStates(rt).COOLDOWN).toBe('current');
    expect(ladderStatus(rt, cooling.policy.spec).detail).toContain(`조치 ${rt.budget.used}/${rt.budget.max} 후 재평가`);

    const retry = playUntil(
      'application-failure-pr',
      (s) => s.activeIncident?.status.runtime.phase === 'COLLECTING_CANDIDATES' && s.activeIncident.status.runtime.attempts.length === 1,
    );
    expect(laneStates(retry.activeIncident!.status.runtime).COOLDOWN).toBe('visited');
  });

  it('walks the exit chain: exhausted, evidence, handoff', () => {
    const exhausted = playUntil('application-failure-pr', phaseIs('RUNTIME_RECOVERY_EXHAUSTED'));
    let lane = laneStates(exhausted.activeIncident!.status.runtime);
    expect(lane.RUNTIME_RECOVERY_EXHAUSTED).toBe('current');
    expect(lane.EVIDENCE_FINALIZING).toBe('off');

    const evidence = playUntil('application-failure-pr', phaseIs('EVIDENCE_FINALIZING'));
    lane = laneStates(evidence.activeIncident!.status.runtime);
    expect(lane.RUNTIME_RECOVERY_EXHAUSTED).toBe('visited');
    expect(lane.EVIDENCE_FINALIZING).toBe('current');

    const handoff = playUntil('application-failure-pr', phaseIs('ENGINEERING_HANDOFF'));
    const rt = handoff.activeIncident!.status.runtime;
    lane = laneStates(rt);
    expect(lane.ENGINEERING_HANDOFF).toBe('current');
    expect(lane.EVIDENCE_FINALIZING).toBe('visited');
    expect(mainStates(rt)[7]).toBe('failed');
    // The handoff is a final Runtime phase: nothing should pulse any more.
    expect(buildLadder(rt).inFlight).toBe(false);
    expect(ladderStatus(rt, handoff.policy.spec).pulse).toBe(false);
  });

  it('ends the no-candidate incident at FAILED without an exit chain', () => {
    const snap = playUntil('no-candidate', phaseIs('FAILED'));
    const rt = snap.activeIncident!.status.runtime;
    const lane = laneStates(rt);
    expect(lane.FAILED).toBe('current');
    expect(lane.WAITING).toBe('visited');
    expect(lane.RUNTIME_RECOVERY_EXHAUSTED).toBe('off');
    expect(mainStates(rt).filter((s) => s === 'failed')).toHaveLength(0);
  });

  it('adds a stop station for phases outside the drawn lane', () => {
    const model = buildLadder(runtime({ phase: 'MANUAL_REQUIRED', classification: 'UNKNOWN' }));
    expect(model.lane.at(-1)).toEqual({ phase: 'MANUAL_REQUIRED', state: 'current' });
    expect(model.inFlight).toBe(false);
  });
});

describe('reachedIndex / failedIndex', () => {
  it('derives progress from facts, not phase names', () => {
    expect(reachedIndex(runtime({ phase: 'FAILED' }))).toBe(0);
    expect(reachedIndex(runtime({ phase: 'FAILED', classification: 'UNKNOWN' }))).toBe(1);
  });

  it('puts a failure that never placed a Pod at EXECUTING and a later one at VERIFYING', () => {
    const attempt = {
      actionId: 'a',
      type: 'relocation' as const,
      targetNode: 'n',
      targetArch: 'amd64' as const,
      startedAt: 0,
      result: 'failed' as const,
      httpSuccesses: 0,
    };
    expect(failedIndex(runtime({ phase: 'COOLDOWN', attempts: [attempt] }))).toBe(6);
    expect(failedIndex(runtime({ phase: 'COOLDOWN', attempts: [attempt], timings: { triggeredAt: 0, placedAt: 5 } }))).toBe(7);
    expect(failedIndex(runtime({ phase: 'VERIFYING', attempts: [{ ...attempt, result: 'placed' }] }))).toBe(-1);
  });
});

describe('ladderStatus', () => {
  it('reports verification progress against the policy', () => {
    const snap = playUntil('node-failure-laptop', phaseIs('VERIFYING'));
    const status = ladderStatus(snap.activeIncident!.status.runtime, snap.policy.spec);
    expect(status.title).toBe('HTTP 검증');
    expect(status.detail).toMatch(/^HTTP 연속 성공 \d+\/5$/);
    expect(status.pulse).toBe(true);
  });
});

describe('httpProgress', () => {
  const verification = { consecutiveSuccesses: 5, stabilityWindowSeconds: 30 };

  it('shows progress below the requirement as is', () => {
    expect(httpProgress(3, verification)).toMatchObject({ shown: 3, required: 5, met: false, text: 'HTTP 연속 성공 3/5' });
  });

  it('never shows more than the requirement, however long the engine keeps counting', () => {
    const p = httpProgress(8, verification);
    expect(p.shown).toBe(5);
    expect(p.base).toBe('HTTP 연속 성공 5/5');
  });

  it('names the remaining condition once the count is met', () => {
    expect(httpProgress(5, verification).text).toBe('HTTP 연속 성공 5/5 · 안정화 30초 확인 중');
    expect(httpProgress(12, verification).met).toBe(true);
  });

  it('clamps a negative count to zero', () => {
    expect(httpProgress(-2, verification).shown).toBe(0);
  });

  it('feeds the ladder readout while VERIFYING', () => {
    const snap = playUntil('node-failure-laptop', (s) => {
      const a = s.activeIncident?.status.runtime.attempts.at(-1);
      return s.activeIncident?.status.runtime.phase === 'VERIFYING' && (a?.httpSuccesses ?? 0) > 5;
    });
    const detail = ladderStatus(snap.activeIncident!.status.runtime, snap.policy.spec).detail!;
    expect(detail).toContain('5/5');
    expect(detail).not.toMatch(/[6-9]\/5/);
    expect(detail).toContain('안정화');
  });
});
