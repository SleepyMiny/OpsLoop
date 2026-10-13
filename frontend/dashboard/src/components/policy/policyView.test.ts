import { describe, expect, it } from 'vitest';
import { baselinePolicy } from '../../data/demo/baseline';
import { activeCooldowns, ruleGroups, sameRuleSheet, secondsLabel, weightShares, weightTotal } from './policyView';

describe('activeCooldowns', () => {
  const now = 1_000_000;

  it('keeps only cooldowns that end after the snapshot time, soonest first', () => {
    const list = activeCooldowns(
      [
        { nodeName: 'late', until: now + 90_000 },
        { nodeName: 'over', until: now - 1 },
        { nodeName: 'exact', until: now },
        { nodeName: 'soon', until: now + 1_200 },
      ],
      now,
    );
    expect(list.map((c) => c.nodeName)).toEqual(['soon', 'late']);
  });

  it('rounds the remaining time up so a live cooldown never reads 0초', () => {
    expect(activeCooldowns([{ nodeName: 'n', until: now + 1 }], now)[0]!.remainingSeconds).toBe(1);
    expect(activeCooldowns([{ nodeName: 'n', until: now + 115_000 }], now)[0]!.remainingSeconds).toBe(115);
    expect(activeCooldowns([{ nodeName: 'n', until: now + 115_001 }], now)[0]!.remainingSeconds).toBe(116);
  });

  it('is empty when nothing is cooling down', () => {
    expect(activeCooldowns([], now)).toEqual([]);
  });
});

describe('weightShares', () => {
  it('turns the policy weights into proportions that sum to one', () => {
    const shares = weightShares(baselinePolicy().spec.score);
    expect(shares.map((s) => s.key)).toEqual(['cpu', 'memory', 'requests']);
    expect(shares.reduce((sum, s) => sum + s.share, 0)).toBeCloseTo(1);
    expect(shares[0]!.share).toBeCloseTo(0.3);
    expect(shares[1]!.share).toBeCloseTo(0.35);
  });

  it('normalises weights that do not add up to one, and survives all zeros', () => {
    const shares = weightShares({ cpuWeight: 1, memoryWeight: 1, requestsWeight: 2, recentFailurePenalty: 15 });
    expect(shares.map((s) => s.share)).toEqual([0.25, 0.25, 0.5]);
    expect(weightShares({ cpuWeight: 0, memoryWeight: 0, requestsWeight: 0, recentFailurePenalty: 0 })[0]!.share).toBeCloseTo(1 / 3);
    expect(weightTotal(baselinePolicy().spec.score)).toBeCloseTo(1);
  });
});

describe('secondsLabel', () => {
  it('keeps short spans in seconds and long whole-minute spans in minutes', () => {
    expect(secondsLabel(20)).toBe('20초');
    expect(secondsLabel(120)).toBe('120초');
    expect(secondsLabel(600)).toBe('10분');
    expect(secondsLabel(330)).toBe('330초');
  });
});

describe('ruleGroups', () => {
  const groups = ruleGroups(baselinePolicy());
  const rows = (id: string) => Object.fromEntries(groups.find((g) => g.id === id)!.rows.map((r) => [r.label, r.value]));

  it('lists the groups in reading order', () => {
    expect(groups.map((g) => g.title)).toEqual(['감지', '후보 거르기', '점수 가중치', '예산·시간', '검증', '격리·Engineering·circuit']);
  });

  it('formats thresholds with their units', () => {
    expect(rows('detection')).toMatchObject({ '노드 NotReady 지속': '20초', '반복 재시작': '120초 동안 3회' });
    expect(rows('filter')).toMatchObject({
      'CPU 사용률 상한': '85%',
      'Memory 사용률 상한': '85%',
      '최소 가용 Memory': '512 MiB',
      '시작 예산 Memory': '256 MiB',
      '요청 여유 예약': '10%',
      '관측값 최대 나이': '30초',
    });
    expect(rows('budget')).toMatchObject({ '조치 예산': 'Incident당 2회', '노드 cooldown': '120초', '전체 deadline': '10분' });
    expect(rows('verification')).toMatchObject({ 'HTTP 연속 성공': '5회', '안정화 구간': '30초' });
  });

  it('describes quarantine, engineering and the circuit from the spec', () => {
    expect(rows('handoff')).toMatchObject({
      '소진 후 격리': 'replicas=0',
      Engineering: '사용',
      '검증 프로필': 'demo-api-verification',
      '같은 signature 차단': '켜짐',
    });
    const off = baselinePolicy();
    off.spec.onApplicationExhausted = 'None';
    off.spec.engineering.enabled = false;
    off.spec.sameSignatureCircuit = false;
    const r = Object.fromEntries(ruleGroups(off).find((g) => g.id === 'handoff')!.rows.map((x) => [x.label, x.value]));
    expect(r).toMatchObject({ '소진 후 격리': '하지 않음', Engineering: '사용 안 함', '같은 signature 차단': '꺼짐' });
    expect(r['검증 프로필']).toBeUndefined();
  });
});

describe('sameRuleSheet', () => {
  it('ignores live status but notices a new hash, generation or mode', () => {
    const a = baselinePolicy();
    const b = structuredClone(a);
    b.status.cooldowns.push({ nodeName: 'n', until: 1 });
    b.status.incidentSequence += 1;
    expect(sameRuleSheet(a, b)).toBe(true);

    expect(sameRuleSheet(a, { ...b, policyHash: 'sha256:other' })).toBe(false);
    expect(sameRuleSheet(a, { ...b, generation: a.generation + 1 })).toBe(false);
    const observe = structuredClone(a);
    observe.spec.mode = 'ObserveOnly';
    expect(sameRuleSheet(a, observe)).toBe(false);
  });
});
