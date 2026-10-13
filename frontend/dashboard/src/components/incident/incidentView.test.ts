import { describe, expect, it } from 'vitest';
import type { IncidentEvent, RecoveryIncident } from '../../domain/types';
import {
  ACTION_TYPE_LABEL,
  conditionTone,
  deriveTimings,
  describeRecent,
  describeTrigger,
  newestFirst,
  sameEvents,
} from './incidentView';

describe('deriveTimings', () => {
  it('measures each stage from the previous one and recovery from the trigger', () => {
    const cells = deriveTimings({
      triggeredAt: 1_000,
      detectedAt: 21_000,
      decidedAt: 30_000,
      placedAt: 52_000,
      recoveredAt: 120_000,
    });
    expect(cells.map((c) => [c.code, c.ms])).toEqual([
      ['T_detect', 20_000],
      ['T_decide', 9_000],
      ['T_place', 22_000],
      ['T_recover', 119_000],
    ]);
  });

  it('leaves later stages empty until their timestamp exists', () => {
    const cells = deriveTimings({ triggeredAt: 0, detectedAt: 20_000 });
    expect(cells.map((c) => c.ms)).toEqual([20_000, null, null, null]);
  });

  it('does not invent a duration when the engine has not decided yet', () => {
    const cells = deriveTimings({ triggeredAt: 0, detectedAt: 20_000, placedAt: 50_000 });
    expect(cells.find((c) => c.key === 'place')!.ms).toBeNull();
  });

  it('never returns a negative span', () => {
    expect(deriveTimings({ triggeredAt: 10_000, detectedAt: 5_000 })[0]!.ms).toBeNull();
  });
});

describe('describeTrigger', () => {
  it('names the node for NodeNotReady', () => {
    const t = describeTrigger({ type: 'NodeNotReady', nodeName: 'sarmubuntuserver', observedAt: 0 });
    expect(t).toMatchObject({ code: 'NodeNotReady', subject: 'sarmubuntuserver', extra: null });
  });

  it('keeps the restart count for RepeatedRestart', () => {
    const t = describeTrigger({ type: 'RepeatedRestart', podName: 'demo-api-x', restarts: 3, observedAt: 0 });
    expect(t).toMatchObject({ code: 'RepeatedRestart', subject: 'demo-api-x', extra: '3회 재시작' });
  });
});

describe('events', () => {
  const ev = (at: number, message = 'm'): IncidentEvent => ({ at, kind: 'act', phase: 'EXECUTING', message });

  it('lists newest first without mutating the source', () => {
    const events = [ev(1), ev(2), ev(3)];
    expect(newestFirst(events).map((e) => e.at)).toEqual([3, 2, 1]);
    expect(events.map((e) => e.at)).toEqual([1, 2, 3]);
  });

  it('treats a re-cloned but unchanged history as the same', () => {
    const a = [ev(1), ev(2), ev(3)];
    expect(sameEvents(a, a.map((e) => ({ ...e })))).toBe(true);
  });

  it('notices appended, dropped and rewritten entries', () => {
    const a = [ev(1), ev(2)];
    expect(sameEvents(a, [...a, ev(3)])).toBe(false);
    expect(sameEvents(a, [ev(2), ev(3)])).toBe(false);
    expect(sameEvents(a, [ev(1), ev(2, 'changed')])).toBe(false);
    expect(sameEvents([], [])).toBe(true);
  });
});

describe('labels and tones', () => {
  it('maps condition status to a lamp tone', () => {
    expect(conditionTone({ status: 'True' })).toBe('ok');
    expect(conditionTone({ status: 'False' })).toBe('caution');
    expect(conditionTone({ status: 'Unknown' })).toBe('off');
  });

  it('labels both action types', () => {
    expect(ACTION_TYPE_LABEL.restart).toContain('재시작');
    expect(ACTION_TYPE_LABEL.relocation).toContain('이동');
  });
});

describe('describeRecent', () => {
  const base = (status: Partial<RecoveryIncident['status']>, phase: RecoveryIncident['status']['runtime']['phase']): RecoveryIncident => ({
    name: 'demo-api-recovery-0006',
    namespace: 'opsloop-demo',
    createdAt: 0,
    spec: { workloadUid: 'w', trigger: { type: 'NodeNotReady', nodeName: 'n', observedAt: 0 }, policyRef: 'p', policyHash: 'sha256:x' },
    status: {
      runtime: {
        phase,
        classification: 'INFRASTRUCTURE_FAILURE',
        attempts: [],
        budget: { used: 1, max: 2 },
        timings: { triggeredAt: 0 },
        oldPodsOutstanding: false,
      },
      conditions: [],
      events: [],
      ...status,
    },
  });

  it('summarises a recovered incident by its runtime phase', () => {
    expect(describeRecent(base({}, 'RECOVERED'))).toMatchObject({ outcomeCode: 'RECOVERED', tone: 'ok' });
  });

  it('prefers the engineering outcome when the incident reached it', () => {
    const incident = base(
      { engineering: { phase: 'PR_CREATED', jobId: 'j', iteration: 2, maxIterations: 3, baseCommit: 'abc', stages: [] } },
      'ENGINEERING_HANDOFF',
    );
    expect(describeRecent(incident)).toMatchObject({ outcomeCode: 'PR_CREATED', tone: 'act' });
  });
});
