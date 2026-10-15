import { describe, expect, it } from 'vitest';
import { baselinePolicy, baselineWorkload, NODE_SEEDS, seedNode } from '../../data/demo/baseline';
import { DemoSimulation } from '../../data/demo/engine';
import { DEMO_SCENARIOS } from '../../data/demo/scenarios';
import { evaluateCandidates } from '../../domain/evaluate';
import {
  RUNTIME_PHASES,
  type CandidateEvaluation,
  type ClusterNode,
  type RuntimePhase,
} from '../../domain/types';
import {
  availableAxisMax,
  buildBoardModel,
  buildCriteria,
  columnModels,
  describeStrip,
  formulaText,
  percentText,
  revealStageForPhase,
  stageAtLeast,
  type CriterionModel,
} from './model';

const MiB = 1024 ** 2;
const GiB = 1024 ** 3;
const MINUS = '−';
const policy = baselinePolicy().spec;
const workload = baselineWorkload();
const NOW = 1_000_000;

function nodes(): ClusterNode[] {
  return NODE_SEEDS.map((seed, i) => {
    const node = seedNode(seed, `uid-${i}`, (n) => n);
    node.usage.cpu.observedAt = NOW;
    node.usage.memory.observedAt = NOW;
    node.usage.memAvailableBytes.observedAt = NOW;
    return node;
  });
}

function candidate(
  over: Partial<CandidateEvaluation['measurements']> = {},
  reasons: CandidateEvaluation['rejectReasons'] = [],
): CandidateEvaluation {
  return {
    nodeUid: 'u',
    nodeName: 'n',
    hostname: 'n',
    arch: 'amd64',
    measurements: {
      cpuUtilization: 0.3,
      memoryUtilization: 0.5,
      memAvailableBytes: 3 * GiB,
      requestedCpuHeadroom: 0.4,
      requestedMemoryHeadroom: 0.6,
      sampleAgeSeconds: 2,
      ...over,
    },
    rejectReasons: reasons,
  };
}

function criteria(c: CandidateEvaluation, judged = true): Record<string, CriterionModel> {
  const list = buildCriteria(c, { filter: policy.filter, availableAxisMax: 6 * GiB, judged });
  return Object.fromEntries(list.map((x) => [x.id, x]));
}

describe('revealStageForPhase', () => {
  it('stays unevaluated until candidates are collected', () => {
    expect(revealStageForPhase('MONITORING')).toBeNull();
    expect(revealStageForPhase('DETECTED')).toBeNull();
    expect(revealStageForPhase('CLASSIFIED')).toBeNull();
  });

  it('reveals measurements, then verdicts, then scores, then the target', () => {
    expect(revealStageForPhase('COLLECTING_CANDIDATES')).toBe('collect');
    expect(revealStageForPhase('FILTERING')).toBe('filter');
    expect(revealStageForPhase('WAITING')).toBe('filter');
    expect(revealStageForPhase('SCORING')).toBe('score');
    expect(revealStageForPhase('TARGET_SELECTED')).toBe('decide');
  });

  it('keeps the target visible through every later phase', () => {
    const later = RUNTIME_PHASES.slice(RUNTIME_PHASES.indexOf('TARGET_SELECTED'));
    for (const phase of later) expect(revealStageForPhase(phase)).toBe('decide');
  });

  it('orders stages so a later stage includes the earlier reveals', () => {
    expect(stageAtLeast('decide', 'filter')).toBe(true);
    expect(stageAtLeast('collect', 'filter')).toBe(false);
    expect(stageAtLeast('score', 'score')).toBe(true);
  });
});

describe('criterion derivation', () => {
  it('passes inside every limit and keeps values neutral before the filter reveal', () => {
    const c = candidate();
    expect(Object.values(criteria(c)).map((x) => x.state)).toEqual(['pass', 'pass', 'pass', 'pass']);
    expect(Object.values(criteria(c, false)).map((x) => x.state)).toEqual([
      'pending',
      'pending',
      'pending',
      'pending',
    ]);
  });

  it('follows the evaluator reject reasons, not a second comparison of the numbers', () => {
    const c = candidate({ cpuUtilization: 0.93 }, ['CPU_PRESSURE']);
    const result = criteria(c);
    expect(result.cpu!.state).toBe('fail');
    expect(result.cpu!.stateWord).toBe('초과');
    expect(result.memory!.state).toBe('pass');
  });

  it('marks available memory short of limit + startup budget as failing', () => {
    const c = candidate({ memAvailableBytes: 600 * MiB }, ['INSUFFICIENT_AVAILABLE_MEMORY']);
    const result = criteria(c);
    expect(result.available!.state).toBe('fail');
    expect(result.available!.valueText).toBe('600 MiB');
    expect(result.available!.marker).toBeCloseTo((768 * MiB) / (6 * GiB), 5);
  });

  it('uses the tighter of CPU and Memory request headroom and draws a shortfall left of the 0 limit', () => {
    const c = candidate({ requestedCpuHeadroom: 0.4, requestedMemoryHeadroom: -0.12 }, [
      'INSUFFICIENT_REQUESTED_MEMORY',
    ]);
    const req = criteria(c).requests!;
    expect(req.state).toBe('fail');
    expect(req.valueText).toBe(`${MINUS}12%`);
    expect(req.fillTo).toBeCloseTo(req.marker, 5);
    expect(req.fillFrom).toBeLessThan(req.marker);
  });

  it('shows a missing sample as "관측값 없음" with an empty fill, never 0%', () => {
    const c = candidate({ cpuUtilization: null, memAvailableBytes: null }, ['METRICS_MISSING']);
    const result = criteria(c);
    expect(result.cpu!.state).toBe('missing');
    expect(result.cpu!.valueText).toBe('관측값 없음');
    expect(result.cpu!.fillTo).toBe(0);
    expect(result.available!.state).toBe('missing');
    // Present values are not judged while another metric is missing (matches the evaluator).
    expect(result.memory!.state).toBe('held');
    // Request headroom comes from the Kubernetes API, not Prometheus, so it is still judged.
    expect(result.requests!.state).toBe('pass');
  });

  it('flags a stale sample as caution instead of pass or fail', () => {
    const c = candidate({ cpuUtilization: 0.95, sampleAgeSeconds: 45 }, ['METRICS_STALE']);
    const result = criteria(c);
    expect(result.cpu!.state).toBe('stale');
    expect(result.cpu!.stateWord).toBe('오래됨');
    expect(result.requests!.state).toBe('pass');
  });

  it('treats NaN like a missing sample', () => {
    const c = candidate({ cpuUtilization: Number.NaN }, ['METRICS_MISSING']);
    const cpu = criteria(c).cpu!;
    expect(cpu.state).toBe('missing');
    expect(cpu.valueText).toBe('관측값 없음');
    expect(cpu.fillTo).toBe(0);
  });

  it('still shows a missing sample while measurements are unjudged', () => {
    const c = candidate({ memoryUtilization: null }, ['METRICS_MISSING']);
    expect(criteria(c, false).memory!.state).toBe('missing');
  });
});

describe('formatting', () => {
  it('rounds to whole percents but never lets rounding contradict the limit', () => {
    expect(percentText(0.428)).toBe('43%');
    expect(percentText(0.853, 0.85)).toBe('85.3%');
    expect(percentText(0.85, 0.85)).toBe('85%');
    expect(percentText(-0.004, 0)).toBe(`${MINUS}0.4%`);
    expect(percentText(0, 0)).toBe('0%');
  });

  it('writes the thresholds the policy actually carries', () => {
    const cols = columnModels(policy);
    expect(cols.map((c) => c.rule)).toEqual(['기준 85% 이하', '기준 85% 이하', '기준 768 MiB 이상', '기준 0% 이상']);
    expect(cols.map((c) => c.ruleShort)).toEqual(['≤ 85%', '≤ 85%', '≥ 768 MiB', '≥ 0%']);
    const custom = columnModels({
      ...policy,
      filter: { ...policy.filter, maxCPUUtilization: 0.9, maxMemoryUtilization: 0.825 },
    });
    expect(custom[0]!.rule).toBe('기준 90% 이하');
    expect(custom[1]!.rule).toBe('기준 82.5% 이하');
  });

  it('builds the score formula from policy weights', () => {
    expect(formulaText(policy.score)).toBe(
      `점수 = 100 × (0.30×CPU 여유 + 0.35×Memory 여유 + 0.35×요청 여유) ${MINUS} 최근 실패 감점(15점/회, 최대 45점, 최근 30분)`,
    );
    expect(
      formulaText({ cpuWeight: 0.5, memoryWeight: 0.25, requestsWeight: 0.25, recentFailurePenalty: 10, recentFailurePenaltyCap: 30, failureHistoryWindowSeconds: 600 }),
    ).toContain('0.50×CPU 여유 + 0.25×Memory 여유 + 0.25×요청 여유');
  });

  it('puts the available-memory axis above every value on whole steps', () => {
    expect(availableAxisMax([5.3 * GiB, 3.8 * GiB, null], 768 * MiB)).toBe(6 * GiB);
    expect(availableAxisMax([null, null], 768 * MiB)).toBe(1.5 * GiB);
    expect(availableAxisMax([6.2 * GiB], 768 * MiB)).toBe(7 * GiB);
  });
});

type Snap = ReturnType<DemoSimulation['snapshot']>;

/**
 * Steps a scenario forward until the snapshot satisfies `until`, so the tests follow the
 * phase instead of hard-coded seconds (cue times in the scripts are free to move).
 */
function boardWhen(scenarioId: string, until: (s: Snap) => boolean, stepMs = 500) {
  const scenario = DEMO_SCENARIOS.find((s) => s.id === scenarioId)!;
  const sim = new DemoSimulation(scenario.script);
  for (let t = 0; t <= sim.durationMs; t += stepMs) {
    sim.advanceTo(t);
    const snapshot = sim.snapshot();
    if (until(snapshot)) return { snapshot, model: buildBoardModel(snapshot) };
  }
  throw new Error(`${scenarioId}: condition never reached`);
}

const phaseIs = (phase: RuntimePhase) => (s: Snap) => s.activeIncident?.status.runtime.phase === phase;
const noIncident = (s: Snap) => s.activeIncident === null;
const attempts = (s: Snap) => s.activeIncident?.status.runtime.attempts.length ?? 0;
const decisionId = (s: Snap) => s.activeIncident?.status.runtime.decision?.actionId ?? '';

describe('buildBoardModel over the Demo A (laptop) timeline', () => {
  const A = 'node-failure-laptop';

  it('previews the live evaluator when there is no Incident, without a verdict', () => {
    const { model } = boardWhen(A, noIncident);
    expect(model.kind).toBe('preview-idle');
    expect(model.rows.some((r) => r.winner)).toBe(false);
    expect(model.rows.filter((r) => r.leader)).toHaveLength(1);
    expect(model.rows.every((r) => r.score !== null)).toBe(true);
    const strip = describeStrip(model);
    expect(strip.lead).toBe('지금 장애가 나면?');
    expect(strip.rest).toContain('미리보기');
    expect(strip.rest).toContain('Incident 없음');
    expect(strip.side).toMatch(/^현재 1순위 /);
    expect(model.meta.actionId).toBeNull();
  });

  it('excludes the failed node in the pre-evaluation preview and still previews', () => {
    const { model } = boardWhen(A, phaseIs('DETECTED'));
    expect(model.kind).toBe('preview-pending');
    expect(model.rows.find((r) => r.verdict === 'excluded')!.reasons[0]!.code).toBe('FAILED_NODE');
  });

  it('reveals nothing but measurements while collecting', () => {
    const { model } = boardWhen(A, phaseIs('COLLECTING_CANDIDATES'));
    expect(model.kind).toBe('collecting');
    expect(model.stage).toBe('collect');
    for (const row of model.rows) {
      expect(row.verdict).toBe('pending');
      expect(row.reasons).toEqual([]);
      expect(row.score).toBeNull();
      expect(row.criteria.every((c) => c.state === 'pending' || c.state === 'missing')).toBe(true);
    }
    expect(model.meta.actionId).toBeNull();
  });

  it('reveals pass/fail and every reject reason while filtering, but no scores', () => {
    const { model } = boardWhen(A, phaseIs('FILTERING'));
    expect(model.stage).toBe('filter');
    expect(model.rows.every((r) => r.score === null)).toBe(true);
    const failed = model.rows.find((r) => r.verdict === 'excluded')!;
    expect(failed.reasons.map((r) => r.code)).toEqual(['FAILED_NODE', 'NODE_NOT_READY', 'METRICS_STALE']);
    expect(failed.reasons[0]!.label).toBe('장애 노드 · 평가에서 제외');
    const azure = model.rows.find((r) => r.arch === 'arm64')!;
    expect(azure.verdict).toBe('reject');
    expect(azure.reasons.map((r) => r.code)).toEqual(['CPU_PRESSURE']);
    expect(azure.reasons[0]!.detail).toContain('85%');
    expect(model.counts).toEqual({ eligible: 1, rejected: 1, excluded: 1 });
  });

  it('reveals scores while scoring and the target only once selected', () => {
    const scoring = boardWhen(A, phaseIs('SCORING')).model;
    expect(scoring.stage).toBe('score');
    expect(scoring.rows.filter((r) => r.score)).toHaveLength(1);
    expect(scoring.rows.some((r) => r.winner)).toBe(false);

    const selected = boardWhen(A, phaseIs('TARGET_SELECTED')).model;
    expect(selected.stage).toBe('decide');
    const winner = selected.rows.find((r) => r.winner)!;
    expect(winner.displayName).toBe('노트북 VM');
    expect(winner.rank).toBe(1);
    expect(describeStrip(selected).lead).toBe('선택 노트북 VM');
    expect(selected.winner!.actionLabel).toBe('다른 노드로 옮깁니다(relocation).');
    expect(selected.meta.actionId).toMatch(/-a1$/);
    expect(selected.meta.validSeconds).toBe(30);
  });

  it('scores add up to the total and use the policy weights', () => {
    const { model, snapshot } = boardWhen(A, phaseIs('TARGET_SELECTED'));
    const score = model.rows.find((r) => r.winner)!.score!;
    const sum = score.parts.reduce((s, p) => s + p.points, 0);
    expect(score.gross).toBeCloseTo(sum, 6);
    expect(score.total).toBeCloseTo(sum - score.penalty, 1);
    expect(model.formula).toContain(snapshot.policy.spec.score.cpuWeight.toFixed(2));
  });
});

describe('buildBoardModel over Demo B (re-evaluation) and the negative scenario', () => {
  const B = 'application-failure-pr';
  const N = 'no-candidate';

  it('shows attempt 2, a fresh collection, and COOLDOWN on the failed node', () => {
    const first = boardWhen(B, phaseIs('TARGET_SELECTED')).model;
    expect(first.attempt).toEqual({ no: 1, max: 2 });
    expect(first.meta.actionId).toMatch(/-a1$/);

    const failed = boardWhen(B, phaseIs('COOLDOWN')).model;
    expect(failed.attempt).toEqual({ no: 1, max: 2 });
    expect(failed.attemptResult).toBe('failed');
    expect(describeStrip(failed).side).toContain('검증 실패');

    // The recorded decision of attempt 1 must not be shown as the evidence of attempt 2.
    const collecting = boardWhen(B, (s) => phaseIs('COLLECTING_CANDIDATES')(s) && attempts(s) === 1).model;
    expect(collecting.kind).toBe('collecting');
    expect(collecting.attempt).toEqual({ no: 2, max: 2 });
    expect(collecting.meta.actionId).toBeNull();

    const second = boardWhen(B, (s) => phaseIs('TARGET_SELECTED')(s) && decisionId(s).endsWith('-a2')).model;
    expect(second.attempt).toEqual({ no: 2, max: 2 });
    expect(second.meta.actionId).toMatch(/-a2$/);
    const cooling = second.rows.filter((r) => r.reasons.some((x) => x.code === 'COOLDOWN'));
    expect(cooling).toHaveLength(1);
    expect(cooling[0]!.verdict).toBe('reject');
    expect(cooling[0]!.reasons.find((x) => x.code === 'COOLDOWN')!.detail).toMatch(/남은 시간은 약 \d+초/);
    expect(second.rows.find((r) => r.winner)!.nodeName).not.toBe(cooling[0]!.nodeName);
  });

  it('says there is no eligible candidate while WAITING, with WAITING and NO_CANDIDATE codes', () => {
    const { model } = boardWhen(N, phaseIs('WAITING'));
    expect(model.phase).toBe('WAITING');
    expect(model.waiting).toEqual({
      tone: 'waiting',
      message: '적격 후보 없음 · 점수와 상관없이 기다립니다',
      codes: ['WAITING', 'NO_CANDIDATE'],
    });
    expect(model.rows.every((r) => r.score === null && !r.winner)).toBe(true);
    const staleAzure = model.rows.find((r) => r.arch === 'arm64')!;
    expect(staleAzure.reasons.map((r) => r.code)).toContain('METRICS_STALE');
    expect(staleAzure.criteria.some((c) => c.state === 'stale')).toBe(true);
  });

  it('does not announce waiting while the filter is still being revealed', () => {
    const { model } = boardWhen(N, phaseIs('FILTERING'));
    expect(model.waiting).toBeNull();
  });

  it('keeps the no-candidate message after the deadline instead of inventing a target', () => {
    const { model } = boardWhen(N, phaseIs('FAILED'), 2000);
    expect(model.waiting?.tone).toBe('ended');
    expect(model.waiting?.codes).toEqual(['FAILED', 'NO_CANDIDATE']);
    expect(model.rows.some((r) => r.winner)).toBe(false);
  });
});

describe('preview matches the evaluator', () => {
  it('ranks the same leader as evaluateCandidates on the snapshot inputs', () => {
    const { snapshot, model } = boardWhen('node-failure-laptop', noIncident);
    const direct = evaluateCandidates({
      nodes: snapshot.cluster.nodes,
      workload: snapshot.workload,
      policy: snapshot.policy.spec,
      now: snapshot.observedAt,
      cooldowns: snapshot.policy.status.cooldowns,
    });
    expect(model.rows.find((r) => r.leader)!.key).toBe(direct.selected!.nodeUid);
  });

  it('is a plain function of the inputs (no clock, no randomness)', () => {
    const list = nodes();
    const r1 = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    const r2 = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(JSON.stringify(r1)).toBe(JSON.stringify(r2));
  });
});
