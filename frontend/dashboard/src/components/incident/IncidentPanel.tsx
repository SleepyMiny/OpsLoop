import { memo } from 'react';
import { FAILURE_CLASS_LABEL, RUNTIME_PHASE_LABEL } from '../../domain/labels';
import type {
  ActionAttempt,
  IncidentCondition,
  IncidentEvent,
  RecoveryIncident,
  RecoveryPolicy,
} from '../../domain/types';
import { clock, duration, shortHash } from '../shared/format';
import { ArchTag, EnumCode, Instrument, Lamp } from '../shared/primitives';
import type { RegionProps } from '../types';
import {
  ACTION_TYPE_LABEL,
  ATTEMPT_RESULT_VIEW,
  EVENT_KIND_LABEL,
  QUARANTINE_VIEW,
  attemptOrdinal,
  conditionTone,
  describeRecent,
  describeTrigger,
  deriveTimings,
  eventTone,
  newestFirst,
  sameEvents,
  TONE_WORD,
} from './incidentView';
import { httpProgress, ladderStatus } from './ladderModel';
import styles from './IncidentPanel.module.css';

export function IncidentPanel({ snapshot, emphasized }: RegionProps) {
  const { activeIncident: incident, policy, recentIncidents } = snapshot;
  return (
    <Instrument title="RecoveryIncident" emphasized={emphasized ?? false}>
      <div className={styles.frame}>
        <div className={styles.scroller} tabIndex={0} role="region" aria-label="RecoveryIncident 기록">
          {incident ? <Record incident={incident} policy={policy} /> : <Idle policy={policy} />}
          <Recent incidents={recentIncidents} />
        </div>
      </div>
    </Instrument>
  );
}

// ---------------------------------------------------------------------------

function Record({ incident, policy }: { incident: RecoveryIncident; policy: RecoveryPolicy }) {
  const { runtime, conditions, events } = incident.status;
  const { spec } = incident;
  const trigger = describeTrigger(spec.trigger);
  const phase = ladderStatus(runtime, policy.spec);
  const timings = deriveTimings(runtime.timings);
  const verification = policy.spec.verification;
  const quarantine = runtime.quarantine ? QUARANTINE_VIEW[runtime.quarantine] : null;

  return (
    <div className={styles.record}>
      <div className={styles.band}>
        <header className={styles.identity}>
          <p className={styles.phaseLine}>
            <span className={styles.phaseGroup}>
              <Lamp state={phase.tone} pulse={phase.pulse} label={phase.pulse ? '진행 중' : '현재 상태'} />
              <strong className={styles.phase}>{RUNTIME_PHASE_LABEL[runtime.phase]}</strong>
              <EnumCode>{runtime.phase}</EnumCode>
            </span>
            <span className={styles.classification}>
              {runtime.classification ? (
                <>
                  {FAILURE_CLASS_LABEL[runtime.classification]} <EnumCode>{runtime.classification}</EnumCode>
                </>
              ) : (
                '아직 분류 전'
              )}
            </span>
          </p>
          <p className={styles.nameLine}>
            <span className={styles.name}>
              <span className={styles.namespace}>{incident.namespace}/</span>
              {incident.name}
            </span>
            <span className={styles.trigger}>
              <span className={styles.term}>트리거</span> {trigger.label}{' '}
              <span className={styles.subject}>{trigger.subject}</span>
              {trigger.extra ? <> {trigger.extra}</> : null} <EnumCode>{trigger.code}</EnumCode>
            </span>
          </p>
        </header>

        <section className={styles.timings} aria-label="단계별 소요 시간">
          <ul className={styles.timingList}>
            {timings.map((t) => (
              <li key={t.key} className={styles.timing} data-missing={t.ms === null || undefined} title={t.hint}>
                <span className={styles.timingValue}>{duration(t.ms)}</span>
                <span className={styles.timingMeta}>
                  <span className={styles.timingLabel}>{t.label}</span>{' '}
                  <span className={styles.timingCode}>{t.code}</span>
                </span>
              </li>
            ))}
          </ul>
          <p className={styles.caption}>시뮬레이션 시간 · 실측 아님</p>
        </section>
      </div>

      <div className={styles.lower}>
        <div className={styles.colMain}>
          <section className={styles.secAttempts}>
            <h3 className={styles.sectionTitle}>
              Runtime 조치{' '}
              <span className={styles.count}>
                {runtime.budget.used}/{runtime.budget.max}
              </span>
            </h3>
            <Attempts attempts={runtime.attempts} verification={verification} />
          </section>

          <section className={styles.secFlags}>
            <h3 className={styles.sectionTitle}>기록 상태</h3>
            <dl className={styles.flags}>
              <div>
                <dt>정책</dt>
                <dd>
                  {spec.policyRef} <EnumCode>{shortHash(spec.policyHash, 16)}</EnumCode>
                </dd>
              </div>
              {runtime.signature ? (
                <div>
                  <dt>Signature</dt>
                  <dd>
                    <EnumCode>{runtime.signature}</EnumCode>
                  </dd>
                </div>
              ) : null}
              <div>
                <dt>이전 Pod</dt>
                <dd>
                  <Lamp
                    state={runtime.oldPodsOutstanding ? 'caution' : 'off'}
                    label={runtime.oldPodsOutstanding ? TONE_WORD.caution : TONE_WORD.off}
                  />
                  {runtime.oldPodsOutstanding ? '종료 미확인' : '추적 중인 Pod 없음'}
                  <EnumCode>oldPodsOutstanding={String(runtime.oldPodsOutstanding)}</EnumCode>
                </dd>
              </div>
              <div>
                <dt>격리</dt>
                <dd>
                  {quarantine ? (
                    <>
                      <Lamp state={quarantine.tone} label={TONE_WORD[quarantine.tone]} />
                      {quarantine.label}
                      <EnumCode>{runtime.quarantine}</EnumCode>
                    </>
                  ) : (
                    <>
                      <Lamp state="off" label={TONE_WORD.off} />
                      격리하지 않음
                    </>
                  )}
                </dd>
              </div>
              <div>
                <dt>증거 Bundle</dt>
                <dd>{runtime.bundleRef ? <EnumCode>{runtime.bundleRef}</EnumCode> : '아직 없음'}</dd>
              </div>
            </dl>
          </section>
        </div>

        <div className={styles.colLog}>
          <section className={styles.secEvents}>
            <h3 className={styles.sectionTitle}>이벤트 기록 (최근 순)</h3>
            <EventLog events={events} />
          </section>

          <section className={styles.secConditions}>
            <h3 className={styles.sectionTitle}>Conditions</h3>
            <Conditions conditions={conditions} />
          </section>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------

function Attempts({
  attempts,
  verification,
}: {
  attempts: ActionAttempt[];
  verification: RecoveryPolicy['spec']['verification'];
}) {
  if (attempts.length === 0) {
    return <p className={styles.empty}>아직 배치를 바꾸지 않았습니다. 대상을 정한 뒤에야 첫 조치가 기록됩니다.</p>;
  }
  return (
    <ol className={styles.attempts}>
      {attempts.map((a, i) => {
        const view = ATTEMPT_RESULT_VIEW[a.result];
        const http = httpProgress(a.httpSuccesses, verification);
        return (
          <li key={a.actionId} className={styles.attempt} title={a.actionId}>
            <p className={styles.attemptHead}>
              <Lamp state={view.tone} pulse={a.result === 'pending' || a.result === 'placed'} label={view.label} />
              <strong>{attemptOrdinal(a, i)}</strong>
              <span>{ACTION_TYPE_LABEL[a.type]}</span>
              <EnumCode>{a.type}</EnumCode>
            </p>
            <p className={styles.attemptResult}>
              <span className={styles.attemptTarget}>
                <ArchTag arch={a.targetArch} size="sm" />
                {a.targetNode}
              </span>
              <span>{view.label}</span>
              <span className={styles.http}>{a.result === 'placed' ? http.text : http.base}</span>
              {a.failureReason ? <span className={styles.failure}>실패 이유: {a.failureReason}</span> : null}
            </p>
          </li>
        );
      })}
    </ol>
  );
}

function Conditions({ conditions }: { conditions: IncidentCondition[] }) {
  if (conditions.length === 0) return <p className={styles.empty}>아직 기록된 Condition이 없습니다.</p>;
  return (
    <ul className={styles.conditions}>
      {conditions.map((c) => (
        <li key={c.type} className={styles.condition}>
          <Lamp state={conditionTone(c)} label={TONE_WORD[conditionTone(c)]} />
          <div>
            <p>
              <strong>{c.type}</strong> <EnumCode>{c.status}</EnumCode> <EnumCode>{c.reason}</EnumCode>
              <span className={styles.when}>{clock(c.lastTransitionTime)}</span>
            </p>
            <p className={styles.message}>{c.message}</p>
          </div>
        </li>
      ))}
    </ul>
  );
}

/** Events are append-only, so the log re-renders only when something was added. */
const EventLog = memo(
  function EventLog({ events }: { events: IncidentEvent[] }) {
    if (events.length === 0) return <p className={styles.empty}>아직 기록된 이벤트가 없습니다.</p>;
    return (
      <ol className={styles.events}>
        {newestFirst(events).map((e, i) => (
          <li key={`${e.at}-${events.length - i}`} className={styles.event}>
            <span className={styles.when}>{clock(e.at)}</span>
            <span className={styles.kind}>
              <Lamp state={eventTone(e.kind)} label={TONE_WORD[eventTone(e.kind)]} />
              {EVENT_KIND_LABEL[e.kind]}
            </span>
            <span className={styles.message}>{e.message}</span>
          </li>
        ))}
      </ol>
    );
  },
  (a, b) => sameEvents(a.events, b.events),
);

function Recent({ incidents }: { incidents: RecoveryIncident[] }) {
  if (incidents.length === 0) return null;
  return (
    <section className={styles.recent}>
      <h3 className={styles.sectionTitle}>이번 시연에서 먼저 다룬 Incident</h3>
      <ul className={styles.recentList}>
        {incidents.map((incident) => {
          const r = describeRecent(incident);
          return (
            <li key={incident.name} className={styles.recentItem}>
              <Lamp state={r.tone} label={TONE_WORD[r.tone]} />
              <EnumCode>{r.name}</EnumCode>
              <span>{r.outcome}</span>
              <span className={styles.recentClass}>
                {r.classification ? FAILURE_CLASS_LABEL[r.classification] : '분류 전'}
              </span>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function Idle({ policy }: { policy: RecoveryPolicy }) {
  return (
    <div className={styles.idle}>
      <h3 className={styles.idleTitle}>지금 열려 있는 Incident가 없습니다</h3>
      <p>
        Incident는 장애 하나당 하나씩 만드는 기록입니다. 감지·분류·조치·검증의 근거와 결과가 여기에 쌓입니다.
      </p>
      <p>
        장애를 확정하면 이름을 먼저 예약하고 그 뒤에 만듭니다. Controller가 중간에 재시작돼도 같은 장애에 Incident나
        조치가 중복되지 않습니다.
      </p>
      <p className={styles.idleMeta}>
        지금까지 예약한 번호 <strong>{policy.status.incidentSequence}</strong>번 <EnumCode>incidentSequence</EnumCode>
      </p>
    </div>
  );
}
