import type { CooldownEntry, PolicyMode, RecoveryPolicy, RecoveryPolicySpec } from '../../domain/types';
import { bytes, percent } from '../shared/format';

/** Pure derivations for the RecoveryPolicy rule sheet. */

// --- Cooldowns ------------------------------------------------------------

export interface ActiveCooldown {
  nodeName: string;
  /** Whole seconds left, rounded up so "0초" never shows while a cooldown is live. */
  remainingSeconds: number;
  until: number;
}

/** Only cooldowns that end after `observedAt`, soonest first. */
export function activeCooldowns(cooldowns: CooldownEntry[], observedAt: number): ActiveCooldown[] {
  return cooldowns
    .filter((c) => c.until > observedAt)
    .map((c) => ({ nodeName: c.nodeName, until: c.until, remainingSeconds: Math.ceil((c.until - observedAt) / 1000) }))
    .sort((a, b) => a.until - b.until || a.nodeName.localeCompare(b.nodeName));
}

// --- Score weights --------------------------------------------------------

export interface WeightShare {
  key: 'cpu' | 'memory' | 'requests';
  label: string;
  /** Short form for the proportion bar. */
  short: string;
  weight: number;
  /** 0..1 share of the three weights, for the proportion bar. */
  share: number;
}

export function weightShares(score: RecoveryPolicySpec['score']): WeightShare[] {
  const parts: Array<Pick<WeightShare, 'key' | 'label' | 'short' | 'weight'>> = [
    { key: 'cpu', label: 'CPU 여유', short: 'CPU', weight: score.cpuWeight },
    { key: 'memory', label: 'Memory 여유', short: 'Memory', weight: score.memoryWeight },
    { key: 'requests', label: '요청 여유', short: '요청', weight: score.requestsWeight },
  ];
  const total = parts.reduce((sum, p) => sum + Math.max(0, p.weight), 0);
  return parts.map((p) => ({ ...p, share: total > 0 ? Math.max(0, p.weight) / total : 1 / parts.length }));
}

export const weightTotal = (score: RecoveryPolicySpec['score']): number =>
  score.cpuWeight + score.memoryWeight + score.requestsWeight;

// --- Units ----------------------------------------------------------------

/** "90초", or whole minutes ("10분") once it is long enough that minutes read better. */
export function secondsLabel(seconds: number): string {
  if (seconds >= 300 && seconds % 60 === 0) return `${seconds / 60}분`;
  return `${seconds}초`;
}

export const MODE_VIEW: Record<PolicyMode, { label: string; short: string; hint: string }> = {
  Enforce: { label: 'Enforce', short: '실제 조치', hint: '판단하고 실제로 조치합니다' },
  ObserveOnly: { label: 'ObserveOnly', short: '관찰만', hint: '판단만 기록하고 바꾸지 않습니다' },
};

// --- Rule sheet -----------------------------------------------------------

export interface RuleRow {
  label: string;
  value: string;
  /** Contract field the value comes from, shown small. */
  field?: string;
}

export interface RuleGroup {
  id: 'detection' | 'filter' | 'score' | 'budget' | 'verification' | 'handoff';
  title: string;
  rows: RuleRow[];
}

/** Groups of the sheet in reading order. The score group's rows are only the penalty; its bar is drawn separately. */
export function ruleGroups(policy: RecoveryPolicy): RuleGroup[] {
  const s = policy.spec;
  return [
    {
      id: 'detection',
      title: '감지',
      rows: [
        { label: '노드 NotReady 지속', value: secondsLabel(s.detection.nodeUnreadyForSeconds), field: 'nodeUnreadyFor' },
        {
          label: '반복 재시작',
          value: `${s.detection.restartWindowSeconds}초 동안 ${s.detection.restartThreshold}회`,
          field: 'restartThreshold',
        },
      ],
    },
    {
      id: 'filter',
      title: '후보 거르기',
      rows: [
        { label: 'CPU 사용률 상한', value: percent(s.filter.maxCPUUtilization), field: 'maxCPUUtilization' },
        { label: 'Memory 사용률 상한', value: percent(s.filter.maxMemoryUtilization), field: 'maxMemoryUtilization' },
        { label: '최소 가용 Memory', value: bytes(s.filter.minimumAvailableMemoryBytes), field: 'minimumAvailableMemory' },
        { label: '시작 예산 Memory', value: bytes(s.filter.startupMemoryBudgetBytes), field: 'startupMemoryBudget' },
        { label: '요청 여유 예약', value: percent(s.filter.requestedCapacityReserve), field: 'requestedCapacityReserve' },
        { label: '관측값 최대 나이', value: secondsLabel(s.filter.maxSampleAgeSeconds), field: 'maxSampleAge' },
      ],
    },
    {
      id: 'score',
      title: '점수 가중치',
      rows: [
        { label: '최근 실패 감점', value: `실패 1회당 ${s.score.recentFailurePenalty}점`, field: 'recentFailurePenalty' },
        {
          label: '감점 상한',
          value: typeof s.score.recentFailurePenaltyCap === 'number' ? `${s.score.recentFailurePenaltyCap}점` : '정하지 않음',
          field: 'recentFailurePenaltyCap',
        },
        {
          label: '실패 이력 구간',
          value:
            typeof s.score.failureHistoryWindowSeconds === 'number'
              ? secondsLabel(s.score.failureHistoryWindowSeconds)
              : '정하지 않음',
          field: 'failureHistoryWindowSeconds',
        },
      ],
    },
    {
      id: 'budget',
      title: '예산·시간',
      rows: [
        { label: '조치 예산', value: `Incident당 ${s.budget.maxActionsPerIncident}회`, field: 'maxActionsPerIncident' },
        { label: '노드 cooldown', value: secondsLabel(s.budget.cooldownSeconds), field: 'cooldown' },
        { label: '배치 제한 시간', value: secondsLabel(s.budget.placementTimeoutSeconds), field: 'placementTimeout' },
        { label: '검증 제한 시간', value: secondsLabel(s.budget.verificationTimeoutSeconds), field: 'verificationTimeout' },
        { label: '전체 deadline', value: secondsLabel(s.budget.incidentDeadlineSeconds), field: 'incidentDeadline' },
      ],
    },
    {
      id: 'verification',
      title: '검증',
      rows: [
        { label: 'HTTP 연속 성공', value: `${s.verification.consecutiveSuccesses}회`, field: 'consecutiveSuccesses' },
        { label: '안정화 구간', value: secondsLabel(s.verification.stabilityWindowSeconds), field: 'stabilityWindow' },
      ],
    },
    {
      id: 'handoff',
      title: '격리·Engineering·circuit',
      rows: [
        {
          label: '소진 후 격리',
          value: s.onApplicationExhausted === 'QuarantineScaleZero' ? 'replicas=0' : '하지 않음',
          field: 'onApplicationExhausted',
        },
        {
          label: 'Engineering',
          value: s.engineering.enabled ? '사용' : '사용 안 함',
          field: 'engineering.enabled',
        },
        ...(s.engineering.enabled
          ? [{ label: '검증 프로필', value: s.engineering.profileRef, field: 'engineering.profileRef' }]
          : []),
        {
          label: '같은 signature 차단',
          value: s.sameSignatureCircuit ? '켜짐' : '꺼짐',
          field: 'sameSignatureCircuit: 새 image나 운영자 reset 전까지 재시도하지 않습니다',
        },
      ],
    },
  ];
}

/** True when two policies render the same rule sheet (hash and generation identify the spec). */
export function sameRuleSheet(a: RecoveryPolicy, b: RecoveryPolicy): boolean {
  return a === b || (a.policyHash === b.policyHash && a.generation === b.generation && a.spec.mode === b.spec.mode);
}
