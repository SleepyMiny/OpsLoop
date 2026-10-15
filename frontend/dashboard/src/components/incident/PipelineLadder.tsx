import { useMemo } from 'react';
import { RUNTIME_PHASE_LABEL } from '../../domain/labels';
import type { RecoveryPolicySpec, RuntimePhase } from '../../domain/types';
import { EnumCode, Instrument, Lamp, type LampState } from '../shared/primitives';
import type { RegionProps } from '../types';
import {
  LANE_TONE,
  buildLadder,
  ladderStatus,
  type LaneId,
  type LaneState,
  type LaneStation,
  type LadderStep,
  type StepState,
} from './ladderModel';
import styles from './PipelineLadder.module.css';

const STEP_LAMP: Record<StepState, { tone: LampState; word: string }> = {
  done: { tone: 'ok', word: '완료' },
  current: { tone: 'act', word: '진행 중' },
  pending: { tone: 'off', word: '아직 아님' },
  failed: { tone: 'fault', word: '실패' },
};

const LANE_WORD: Record<LaneState, string> = {
  off: '지나지 않음',
  visited: '거쳐 왔음',
  current: '지금 여기',
};

/** Lane labels are short: the lane is one line, and the phase's full label shows in the status readout. */
const LANE_LABEL: Partial<Record<RuntimePhase, string>> = {
  WAITING: '대기',
  RUNTIME_RECOVERY_EXHAUSTED: '예산 소진',
  EVIDENCE_FINALIZING: '증거 확정',
  FAILED: '운영 실패',
};

/** Branch stations that send the incident back to candidate collection. */
const LOOPS_BACK: ReadonlySet<string> = new Set<LaneId>(['WAITING', 'COOLDOWN']);
/** First station of each lane group, used for the divider before it. */
const GROUP_START: ReadonlySet<string> = new Set<LaneId>(['RUNTIME_RECOVERY_EXHAUSTED', 'FAILED']);

export function PipelineLadder({ snapshot, emphasized }: RegionProps) {
  const incident = snapshot.activeIncident;
  const runtime = incident?.status.runtime ?? null;
  const spec = snapshot.policy.spec;

  const model = useMemo(() => buildLadder(runtime), [runtime]);
  const status = useMemo(() => (runtime && model.mode === 'active' ? ladderStatus(runtime, spec) : null), [runtime, model.mode, spec]);

  const aside = runtime && model.mode === 'active' ? <Budget used={runtime.budget.used} max={runtime.budget.max} /> : null;

  return (
    <Instrument title="복구 단계" emphasized={emphasized ?? false} aside={aside}>
      <div className={styles.root} data-mode={model.mode}>
        {status ? (
          <p className={styles.status}>
            <Lamp state={status.tone} pulse={status.pulse} label={status.pulse ? '진행 중' : '현재 상태'} />
            <strong className={styles.statusTitle}>{status.title}</strong>
            <EnumCode>{status.code}</EnumCode>
            {status.detail ? <span className={styles.statusDetail}>{status.detail}</span> : null}
          </p>
        ) : (
          <IdleStatus spec={spec} />
        )}

        <ol className={styles.track} aria-label="Runtime 복구 단계 순서">
          {model.main.map((step, i) => (
            <Station key={step.phase} step={step} index={i} next={model.main[i + 1]} />
          ))}
        </ol>

        {model.mode === 'active' ? (
          <ol className={styles.lane} aria-label="되돌아가거나 끝나는 경로">
            {model.lane.map((station) => (
              <LaneItem key={station.phase} station={station} inFlight={model.inFlight} />
            ))}
          </ol>
        ) : (
          <IdleTriggers spec={spec} />
        )}
      </div>
    </Instrument>
  );
}

function Budget({ used, max }: { used: number; max: number }) {
  return (
    <span className={styles.budget}>
      <span>Runtime 조치</span>
      <strong className={styles.budgetNumber}>
        {used}/{max}
      </strong>
      <span className={styles.pips} aria-hidden="true">
        {Array.from({ length: max }, (_, i) => (
          <span key={i} className={styles.pip} data-spent={i < used || undefined} />
        ))}
      </span>
    </span>
  );
}

function Station({ step, index, next }: { step: LadderStep; index: number; next: LadderStep | undefined }) {
  const lamp = STEP_LAMP[step.state];
  return (
    <li
      className={styles.station}
      data-state={step.state}
      data-first={index === 0 || undefined}
      data-last={next === undefined || undefined}
      data-next-reached={(next !== undefined && next.state !== 'pending') || undefined}
      aria-current={step.state === 'current' ? 'step' : undefined}
    >
      <span className={styles.mark} data-glyph={step.state}>
        <Lamp state={lamp.tone} pulse={step.state === 'current'} label={lamp.word} />
      </span>
      <span className={styles.label}>{RUNTIME_PHASE_LABEL[step.phase]}</span>
      <span className={styles.stepCode}>
        <EnumCode>{step.phase}</EnumCode>
      </span>
    </li>
  );
}

function LaneItem({ station, inFlight }: { station: LaneStation; inFlight: boolean }) {
  const lit = station.state !== 'off';
  const tone = LANE_TONE[station.phase];
  const phase = station.phase as RuntimePhase;
  return (
    <li
      className={styles.laneItem}
      data-state={station.state}
      data-tone={lit ? tone : undefined}
      data-group-start={GROUP_START.has(station.phase) || undefined}
      aria-current={station.state === 'current' ? 'step' : undefined}
    >
      <Lamp
        state={lit ? tone : 'off'}
        pulse={station.state === 'current' && inFlight}
        label={LANE_WORD[station.state]}
      />
      <span className={styles.laneText}>
        <span className={styles.laneLabel}>
          {station.phase === 'ENGINEERING_HANDOFF' ? (
            <>
              <span className={styles.laneExtra}>Engineering </span>인계
            </>
          ) : (
            (LANE_LABEL[phase] ?? RUNTIME_PHASE_LABEL[phase])
          )}
        </span>
        <span className={styles.laneCode}>
          <EnumCode>{station.phase}</EnumCode>
        </span>
        {LOOPS_BACK.has(station.phase) ? (
          <span className={styles.loop} title="근거 수집으로 되돌아갑니다">
            <svg viewBox="0 0 16 16" aria-hidden="true" className={styles.loopIcon}>
              <path d="M13 8a5 5 0 1 1-1.8-3.8" />
              <path d="M13.2 1.8v3.6H9.6" />
            </svg>
            <span className="visually-hidden">근거 수집으로 되돌아갑니다</span>
          </span>
        ) : null}
      </span>
    </li>
  );
}

function IdleStatus({ spec }: { spec: RecoveryPolicySpec }) {
  const note = !spec.enabled
    ? '정책이 꺼져 있어 새 조치를 하지 않습니다.'
    : spec.mode === 'ObserveOnly'
      ? '관찰 전용 모드라 판단만 기록하고 배치는 바꾸지 않습니다.'
      : '아래 신호가 보이면 이 순서로 복구를 시작합니다.';
  return (
    <p className={styles.status}>
      <Lamp state="act" pulse label="감시 중" />
      <strong className={styles.statusTitle}>{RUNTIME_PHASE_LABEL.MONITORING}</strong>
      <EnumCode>MONITORING</EnumCode>
      <span className={styles.statusDetail}>{note}</span>
    </p>
  );
}

function IdleTriggers({ spec }: { spec: RecoveryPolicySpec }) {
  const { nodeUnreadyForSeconds, restartThreshold, restartWindowSeconds } = spec.detection;
  return (
    <ul className={styles.triggers} aria-label="감시하는 신호">
      <li>
        <strong>노드 NotReady {nodeUnreadyForSeconds}초 지속</strong>
        <span>Kubernetes API 기준</span>
      </li>
      <li>
        <strong>
          {restartWindowSeconds}초 동안 재시작 {restartThreshold}회
        </strong>
        <span>Pod 재시작 횟수 기준</span>
      </li>
    </ul>
  );
}
