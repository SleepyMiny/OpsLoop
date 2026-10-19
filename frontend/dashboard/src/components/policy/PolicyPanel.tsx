import { memo } from 'react';
import type { RecoveryPolicy } from '../../domain/types';
import { shortHash } from '../shared/format';
import { EnumCode, Instrument, Lamp, type LampState } from '../shared/primitives';
import type { RegionProps } from '../types';
import {
  MODE_VIEW,
  activeCooldowns,
  ruleGroups,
  sameRuleSheet,
  weightShares,
  weightTotal,
  type ActiveCooldown,
  type RuleGroup,
} from './policyView';
import styles from './PolicyPanel.module.css';

export function PolicyPanel({ snapshot, emphasized }: RegionProps) {
  const { policy, observedAt } = snapshot;
  const cooldowns = activeCooldowns(policy.status.cooldowns, observedAt);

  return (
    <Instrument title="RecoveryPolicy" emphasized={emphasized ?? false} aside={<Identity policy={policy} />}>
      <div className={styles.root} tabIndex={0} role="region" aria-label="RecoveryPolicy 규칙">
        <Status policy={policy} cooldowns={cooldowns} />
        <RuleSheet policy={policy} />
      </div>
    </Instrument>
  );
}

// ---------------------------------------------------------------------------

/** Name, mode, generation and hash: two short lines in the instrument header. */
function Identity({ policy }: { policy: RecoveryPolicy }) {
  const { enabled, mode } = policy.spec;
  const view = MODE_VIEW[mode];
  const tone: LampState = !enabled ? 'off' : mode === 'Enforce' ? 'act' : 'caution';
  const stale = policy.status.observedGeneration !== policy.generation;
  return (
    <span className={styles.identity}>
      <span className={styles.line} title={enabled ? view.hint : '새 조치를 하지 않습니다'}>
        <Lamp state={tone} label={!enabled ? '꺼짐' : mode === 'Enforce' ? '조치함' : '조치 안 함'} />
        <strong className={styles.mode}>{enabled ? view.label : '꺼짐'}</strong>
        <span>{enabled ? view.short : '새 조치 없음'}</span>
        <EnumCode>{shortHash(policy.policyHash, 16)}</EnumCode>
      </span>
      <span className={styles.line}>
        <span className={styles.name}>
          <span className={styles.namespace}>{policy.namespace}/</span>
          {policy.name}
        </span>
        <span>
          generation <strong className={styles.num}>{policy.generation}</strong>
        </span>
        {stale ? (
          <span className={styles.stale}>
            <Lamp state="caution" label="주의" />
            반영 전 (observedGeneration {policy.status.observedGeneration})
          </span>
        ) : null}
      </span>
    </span>
  );
}

/** Live part of the sheet: it changes while the rules stay fixed. */
function Status({ policy, cooldowns }: { policy: RecoveryPolicy; cooldowns: ActiveCooldown[] }) {
  const active = policy.status.activeIncidentRef;
  const cooldownSeconds = policy.spec.budget.cooldownSeconds;
  return (
    <dl className={styles.status}>
      <div><dt>Controller 상태</dt><dd>{policy.status.conditions?.map((c) => `${c.type}=${c.status} / ${c.reason}`).join(' · ') || '기록 없음'}</dd></div>
      <div><dt>Circuit 기록</dt><dd>{policy.status.circuits?.map((c) => `${c.phase} / ${c.class}`).join(' · ') || '없음'}</dd></div>
      <div>
        <dt>활성 Incident</dt>
        <dd>
          {active ? (
            <>
              <Lamp state="act" pulse label="진행 중" />
              <EnumCode>{active}</EnumCode>
            </>
          ) : (
            <span className={styles.none}>없음</span>
          )}
        </dd>
      </div>
      <div>
        <dt>예약한 번호</dt>
        <dd>
          <strong className={styles.num}>{policy.status.incidentSequence}</strong>
        </dd>
      </div>
      <div>
        <dt>cooldown</dt>
        <dd>
          {cooldowns.length === 0 ? (
            <span className={styles.none}>없음</span>
          ) : (
            <ul className={styles.cooldowns}>
              {cooldowns.map((c) => (
                <li key={`${c.nodeName}-${c.until}`}>
                  <Lamp state="caution" label="cooldown" />
                  <span>{c.nodeName}</span>
                  <span className={styles.remaining}>
                    남은 <strong className={styles.num}>{c.remainingSeconds}</strong>초
                  </span>
                  <span className={styles.drain} aria-hidden="true">
                    <span style={{ transform: `scaleX(${Math.min(1, c.remainingSeconds / cooldownSeconds)})` }} />
                  </span>
                </li>
              ))}
            </ul>
          )}
        </dd>
      </div>
    </dl>
  );
}

/** Columns of the sheet: the bookends (detect, verify), then filter, budget, hand-off. The score band sits above. */
const COLUMNS: ReadonlyArray<readonly RuleGroup['id'][]> = [
  ['detection', 'verification'],
  ['filter'],
  ['budget'],
  ['handoff'],
];

/** The rules themselves. Fixed for a given policy hash, so it skips re-rendering on every snapshot. */
const RuleSheet = memo(
  function RuleSheet({ policy }: { policy: RecoveryPolicy }) {
    const groups = ruleGroups(policy);
    const score = groups.find((g) => g.id === 'score');
    const shares = weightShares(policy.spec.score);
    const total = weightTotal(policy.spec.score);
    return (
      <>
        {score ? (
          <section className={styles.score}>
            <h4 className={styles.scoreTitle}>{score.title}</h4>
            <div
              className={styles.weights}
              role="img"
              aria-label={`점수 가중치 ${shares.map((s) => `${s.label} ${s.weight.toFixed(2)}`).join(', ')}`}
            >
              {shares.map((s) => (
                <span key={s.key} className={styles.weight} data-key={s.key} style={{ flexGrow: s.share }}>
                  <span className={styles.weightLabel}>{s.label}</span>
                  <strong className={styles.weightValue}>{s.weight.toFixed(2)}</strong>
                </span>
              ))}
            </div>
            <dl className={styles.penalty}>
              {score.rows.map((row) => (
                <div key={row.label} title={row.field}>
                  <dt>{row.label}</dt>
                  <dd>{row.value}</dd>
                </div>
              ))}
            </dl>
            <p className={styles.weightNote}>
              가중치는 코드 상수가 아니라 정책 데이터입니다.
              {Math.abs(total - 1) > 1e-9 ? ` (합계 ${total.toFixed(2)})` : ''}
            </p>
          </section>
        ) : null}

        <div className={styles.sheet}>
          {COLUMNS.map((ids) => (
            <div key={ids.join('-')} className={styles.sheetCol}>
              {groups
                .filter((g) => ids.includes(g.id))
                .map((group) => (
                  <section key={group.id} className={styles.group}>
                    <h4 className={styles.groupTitle}>{group.title}</h4>
                    <dl className={styles.rows}>
                      {group.rows.map((row) => (
                        <div key={row.label} title={row.field}>
                          <dt>{row.label}</dt>
                          <dd>{row.value}</dd>
                        </div>
                      ))}
                    </dl>
                  </section>
                ))}
            </div>
          ))}
        </div>
      </>
    );
  },
  (a, b) => sameRuleSheet(a.policy, b.policy),
);
