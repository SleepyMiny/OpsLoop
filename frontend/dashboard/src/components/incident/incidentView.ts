import type {
  ActionAttempt,
  ActionResult,
  ActionType,
  IncidentCondition,
  IncidentEvent,
  IncidentEventKind,
  IncidentTrigger,
  QuarantineState,
  RecoveryIncident,
  RuntimeTimings,
} from '../../domain/types';
import { ENGINEERING_PHASE_LABEL, RUNTIME_PHASE_LABEL } from '../../domain/labels';
import type { LampState } from '../shared/primitives';

/** Pure derivations for the RecoveryIncident viewer. */

// --- Timings --------------------------------------------------------------

export interface TimingCell {
  key: 'detect' | 'decide' | 'place' | 'recover';
  /** Contract-style name, shown small. */
  code: 'T_detect' | 'T_decide' | 'T_place' | 'T_recover';
  label: string;
  hint: string;
  /** Milliseconds, or null while the later timestamp does not exist yet. */
  ms: number | null;
}

const span = (from: number | undefined, to: number | undefined): number | null =>
  from === undefined || to === undefined || to < from ? null : to - from;

/**
 * Where T_detect / T_recover start. §18 defines both from fault injection (Demo
 * Driver clock); without that record they fall back to the API signal and the UI
 * says so instead of presenting a shorter number under the §18 name.
 */
export type TimingBasis = 'injection' | 'api-signal';

export function timingBasis(t: RuntimeTimings): TimingBasis {
  return typeof t.faultInjectedAt === 'number' ? 'injection' : 'api-signal';
}

export function timingCaption(source: 'demo' | 'live' | 'replay', basis: TimingBasis): string {
  const base = basis === 'injection' ? '장애 주입 기준(§18)' : 'API 장애 신호 기준 · 장애 주입 시각 없음';
  return source === 'demo' ? `시뮬레이션 시간 · 실측 아님 · ${base}` : source === 'replay' ? `과거 실증 기록 · ${base}` : base;
}

/** T_decide and T_place run stage to stage; T_detect and T_recover start at fault injection (§18). */
export function deriveTimings(t: RuntimeTimings): TimingCell[] {
  const injection = timingBasis(t) === 'injection';
  const start = injection ? t.faultInjectedAt : t.triggeredAt;
  return [
    {
      key: 'detect',
      code: 'T_detect',
      label: '감지까지',
      hint: injection ? '장애 주입부터 Incident 확정까지' : 'API 장애 신호부터 Incident 확정까지(장애 주입 시각 없음)',
      ms: span(start, t.detectedAt),
    },
    {
      key: 'decide',
      code: 'T_decide',
      label: '결정까지',
      hint: '감지 뒤 대상 노드를 정하기까지',
      ms: span(t.detectedAt, t.decidedAt),
    },
    {
      key: 'place',
      code: 'T_place',
      label: '배치까지',
      hint: '결정 뒤 새 Pod가 Ready가 되기까지',
      ms: span(t.decidedAt, t.placedAt),
    },
    {
      key: 'recover',
      code: 'T_recover',
      label: '복구까지',
      hint: injection ? '장애 주입부터 HTTP 검증을 통과하기까지' : 'API 장애 신호부터 HTTP 검증 통과까지(장애 주입 시각 없음)',
      ms: span(start, t.recoveredAt),
    },
  ];
}

// --- Trigger --------------------------------------------------------------

export interface TriggerView {
  code: string;
  label: string;
  /** Node or Pod the trigger refers to. */
  subject: string;
  detail: string;
  /** Extra measured fact, e.g. the restart count. */
  extra: string | null;
}

export function describeTrigger(trigger: IncidentTrigger): TriggerView {
  if (trigger.type === 'ImagePull') return {code: trigger.reason ?? trigger.type, label: '이미지 Pull 실패', subject: trigger.podName, detail: 'Kubernetes 이미지 Pull 오류를 Controller가 분류했습니다.', extra: null};
  if (trigger.type === 'FailureSignal') return {code: trigger.reason ?? trigger.type, label: trigger.reason, subject: trigger.subject, detail: '기록된 원본 장애 신호', extra: null};
  if (trigger.type === 'NodeNotReady') {
    return {
      code: trigger.reason ?? trigger.type,
      label: '노드 응답 없음',
      subject: trigger.nodeName,
      detail: 'Kubernetes API가 Ready가 아니라고 보고했습니다.',
      extra: null,
    };
  }
  return {
    code: trigger.reason ?? trigger.type,
    label: '반복 재시작',
    subject: trigger.podName,
    detail:
      trigger.restarts === undefined
        ? '정해진 시간 안에 재시작 기준 횟수에 도달했습니다.'
        : `정해진 시간 안에 ${trigger.restarts}번 다시 시작했습니다.`,
    extra: trigger.restarts === undefined ? '재시작 기준 도달' : `${trigger.restarts}회 재시작`,
  };
}

// --- Attempts -------------------------------------------------------------

export const ACTION_TYPE_LABEL: Record<ActionType, string> = {
  restart: '같은 노드에서 재시작',
  relocation: '다른 노드로 이동',
};

export const ATTEMPT_RESULT_VIEW: Record<ActionResult, { label: string; tone: LampState }> = {
  pending: { label: '배치 중', tone: 'caution' },
  placed: { label: '배치됨, 검증 중', tone: 'act' },
  verified: { label: '검증 통과', tone: 'ok' },
  failed: { label: '실패', tone: 'fault' },
  'timed-out': { label: '시간 초과', tone: 'fault' },
};

/** "…-a2" from "demo-api-recovery-0007-a2" for compact rows; full id stays available. */
export function attemptOrdinal(attempt: ActionAttempt, index: number): string {
  const m = /-a(\d+)$/.exec(attempt.actionId);
  return `조치 ${m ? m[1] : index + 1}`;
}

// --- Flags ----------------------------------------------------------------

export const QUARANTINE_VIEW: Record<QuarantineState, { label: string; tone: LampState }> = {
  QuarantineRequested: { label: '격리 요청됨, 반영 확인 전', tone: 'caution' },
  QuarantineConfirmed: { label: '격리 확인됨 (replicas=0)', tone: 'act' },
  QuarantineUnconfirmed: { label: '격리 확인 못 함', tone: 'fault' },
};

// --- Conditions & events --------------------------------------------------

export function conditionTone(c: Pick<IncidentCondition, 'status'>): LampState {
  if (c.status === 'True') return 'ok';
  if (c.status === 'False') return 'caution';
  return 'off';
}

const EVENT_TONE: Record<IncidentEventKind, LampState> = {
  trigger: 'caution',
  classify: 'act',
  evaluate: 'off',
  decide: 'act',
  act: 'act',
  verify: 'off',
  recovered: 'ok',
  wait: 'caution',
  fail: 'fault',
  evidence: 'act',
  quarantine: 'caution',
  engineering: 'act',
};

export const EVENT_KIND_LABEL: Record<IncidentEventKind, string> = {
  trigger: '신호',
  classify: '분류',
  evaluate: '평가',
  decide: '결정',
  act: '조치',
  verify: '검증',
  recovered: '복구',
  wait: '대기',
  fail: '실패',
  evidence: '증거',
  quarantine: '격리',
  engineering: 'Engineering',
};

export const eventTone = (kind: IncidentEventKind): LampState => EVENT_TONE[kind];

/** Spoken name of a lamp colour, for rows whose visible text is something else. */
export const TONE_WORD: Record<LampState, string> = {
  ok: '정상',
  caution: '주의',
  fault: '실패',
  act: 'OpsLoop 동작',
  off: '해당 없음',
};

/** Newest first. The engine appends in time order, so this is a reversed copy. */
export function newestFirst(events: IncidentEvent[]): IncidentEvent[] {
  return events.slice().reverse();
}

/**
 * Event history is append-only and bounded, so (length, first, last) identifies it.
 * Lets the log skip re-rendering while snapshots stream in (up to 10 Hz).
 */
export function sameEvents(a: IncidentEvent[], b: IncidentEvent[]): boolean {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  if (a.length === 0) return true;
  const a0 = a[0]!;
  const b0 = b[0]!;
  const aN = a[a.length - 1]!;
  const bN = b[b.length - 1]!;
  return a0.at === b0.at && aN.at === bN.at && aN.message === bN.message && aN.phase === bN.phase;
}

// --- Recent incidents -----------------------------------------------------

export interface RecentView {
  name: string;
  outcome: string;
  /** Where the Incident ended up: Engineering phase if it got that far. */
  outcomeCode: string;
  tone: LampState;
  classification: RecoveryIncident['status']['runtime']['classification'];
}

export function describeRecent(incident: RecoveryIncident): RecentView {
  const { runtime, engineering } = incident.status;
  if (engineering) {
    const done = engineering.phase === 'PR_CREATED' || engineering.phase === 'PR_READY';
    return {
      name: incident.name,
      outcome: ENGINEERING_PHASE_LABEL[engineering.phase],
      outcomeCode: engineering.phase,
      tone: done ? 'act' : 'fault',
      classification: runtime.classification,
    };
  }
  return {
    name: incident.name,
    outcome: RUNTIME_PHASE_LABEL[runtime.phase],
    outcomeCode: runtime.phase,
    tone: runtime.phase === 'RECOVERED' ? 'ok' : runtime.phase === 'FAILED' ? 'fault' : 'caution',
    classification: runtime.classification,
  };
}
