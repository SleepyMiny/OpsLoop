import { useId, useMemo } from 'react';
import { EnumCode, Instrument, Lamp } from '../shared/primitives';
import type { RegionProps } from '../types';
import { CandidateRow } from './CandidateRow';
import {
  buildBoardModel,
  describeStrip,
  SCORE_PART_IDS,
  SCORE_PART_LABEL,
  stageAtLeast,
  type BoardModel,
} from './model';
import styles from './candidates.module.css';

/**
 * 복구 후보 평가. Shows why OpsLoop picks one recovery node: a hard filter that records
 * every violated rule, then a score from real CPU/Memory and Kubernetes requests with
 * weights read from the RecoveryPolicy. Demo Mode data is simulated, never measured.
 */
export function CandidateBoard({ snapshot, emphasized }: RegionProps) {
  const model = useMemo(() => buildBoardModel(snapshot), [snapshot]);
  const uid = useId();
  const stripId = `${uid}-strip`;

  return (
    <Instrument
      title="복구 후보 평가"
      aside={<BoardAside model={model} />}
      emphasized={emphasized ?? false}
      className={styles.board}
      labelledBy={`${uid}-title`}
    >
      <div className={styles.body} data-kind={model.kind} data-stage={model.stage}>
        {model.waiting ? <WaitingBand waiting={model.waiting} id={stripId} /> : <Strip model={model} id={stripId} />}
        {model.rows.length === 0 ? (
          <p className={styles.empty}>
            {snapshot.source === 'replay' ? '이 시점에는 저장된 후보 평가가 없습니다.' : `평가할 노드가 없습니다. ${snapshot.policy.spec.candidateSelector} 라벨을 확인하세요.`}
          </p>
        ) : (
          <CandidateTable model={model} idBase={uid} describedBy={stripId} />
        )}
        <Foot model={model} />
      </div>
    </Instrument>
  );
}

/**
 * An ARIA table: the column headers stay associated with every cell whatever the CSS
 * does (aligned columns on wide regions, stacked cards on narrow ones).
 */
function CandidateTable({ model, idBase, describedBy }: { model: BoardModel; idBase: string; describedBy: string }) {
  return (
    <div role="table" aria-label="후보 노드별 평가" aria-describedby={describedBy} className={styles.table}>
      <div role="rowgroup" className={styles.headGroup}>
        <div role="row" className={styles.head}>
          <div role="columnheader" className={styles.headCell}>
            <span className={styles.headLabel}>후보 노드</span>
          </div>
          {model.columns.map((col) => (
            <div role="columnheader" key={col.id} className={styles.headCell}>
              <span className={styles.headLabel}>
                <span className={styles.labelLong}>{col.label}</span>
                <span className={styles.labelShort} aria-hidden="true">
                  {col.labelShort}
                </span>
              </span>
              <span className={styles.headRule}>
                <span className={styles.ruleLong}>{col.rule}</span>
                <span className={styles.ruleShort} aria-hidden="true">
                  {col.ruleShort}
                </span>
              </span>
            </div>
          ))}
          <div role="columnheader" className={styles.headCell}>
            <span className={styles.headLabel}>판정과 점수</span>
            {stageAtLeast(model.stage, 'score') ? <ScoreLegend /> : null}
          </div>
        </div>
      </div>
      <div role="rowgroup" className={styles.rows}>
        {model.rows.map((row) => (
          <CandidateRow key={row.key} row={row} columns={model.columns} idBase={`${idBase}-${row.key}`} />
        ))}
      </div>
    </div>
  );
}

/** Names the three score colors once, so each row only needs the numbers. */
function ScoreLegend() {
  return (
    <span className={styles.legend} aria-hidden="true">
      {SCORE_PART_IDS.map((id) => (
        <span key={id}>
          <i className={styles.swatch} data-part={id} />
          {SCORE_PART_LABEL[id]}
        </span>
      ))}
    </span>
  );
}

function BoardAside({ model }: { model: BoardModel }) {
  const { aside } = model;
  return (
    <div className={styles.aside}>
      <Meta model={model} className={styles.metaAside} />
      <span className={styles.phase}>
        {aside.lamp !== 'off' ? (
          <span aria-hidden="true">
            <Lamp state={aside.lamp} label="" />
          </span>
        ) : null}
        <span>{aside.label}</span>
        {aside.code ? <EnumCode>{aside.code}</EnumCode> : null}
      </span>
    </div>
  );
}

/** Which decision this board shows: action, policy in force, how long the snapshot stays valid. */
function Meta({ model, className }: { model: BoardModel; className?: string }) {
  const { meta } = model;
  return (
    <dl className={className}>
      {meta.actionId ? (
        <div>
          <dt>Action</dt>
          <dd>
            <EnumCode>{meta.actionId}</EnumCode>
          </dd>
        </div>
      ) : null}
      <div>
        <dt>정책</dt>
        <dd>
          <EnumCode>{meta.policyHash}</EnumCode>
        </dd>
      </div>
      {meta.validSeconds !== null ? (
        <div>
          <dt>snapshot 유효</dt>
          <dd>{meta.validSeconds}초</dd>
        </div>
      ) : null}
    </dl>
  );
}

function Strip({ model, id }: { model: BoardModel; id: string }) {
  const text = describeStrip(model);
  return (
    <div className={styles.strip} data-kind={model.kind} data-decided={model.winner ? true : undefined} id={id}>
      <p className={styles.stripText}>
        <strong className={styles.stripLead}>{text.lead}</strong> <span className={styles.stripRest}>{text.rest}</span>
      </p>
      {text.side ? <p className={styles.stripSide}>{text.side}</p> : null}
    </div>
  );
}

/** Nobody is eligible: say so loudly, because waiting is a decision, not a malfunction. */
function WaitingBand({ waiting, id }: { waiting: NonNullable<BoardModel['waiting']>; id: string }) {
  return (
    <div className={styles.band} data-tone={waiting.tone} role="status" id={id}>
      <span aria-hidden="true">
        <Lamp state={waiting.tone === 'waiting' ? 'caution' : 'fault'} label="" />
      </span>
      <strong className={styles.bandMessage}>{waiting.message}</strong>
      <span className={styles.bandCodes}>
        {waiting.codes.map((code) => (
          <EnumCode key={code}>{code}</EnumCode>
        ))}
      </span>
    </div>
  );
}

function Foot({ model }: { model: BoardModel }) {
  return (
    <footer className={styles.foot}>
      <p className={styles.formula} data-active={stageAtLeast(model.stage, 'score') || undefined}>
        {model.formula}
      </p>
      <Meta model={model} className={styles.metaFoot} />
    </footer>
  );
}
