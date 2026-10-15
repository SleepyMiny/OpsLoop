import { memo, type KeyboardEvent } from 'react';
import { ArchTag, EnumCode, Lamp, type LampState } from '../shared/primitives';
import {
  SCORE_PART_IDS,
  type ColumnModel,
  type CriterionModel,
  type ReasonModel,
  type RowModel,
  type ScoreModel,
  type Verdict,
} from './model';
import styles from './candidates.module.css';

/**
 * One candidate. Rendered as an ARIA table row so the four criteria stay associated
 * with their column headers whatever the CSS layout does (aligned columns on wide
 * regions, a stacked card on narrow ones).
 */

function pct(x: number): string {
  return `${(x * 100).toFixed(2)}%`;
}

function Glyph({ kind }: { kind: 'check' | 'cross' | 'alert' }) {
  return (
    <svg viewBox="0 0 12 12" aria-hidden="true" className={styles.glyph} data-kind={kind}>
      {kind === 'check' ? (
        <path d="M2 6.4 4.8 9 10 3.2" />
      ) : kind === 'cross' ? (
        <path d="M3 3 9 9M9 3 3 9" />
      ) : (
        <>
          <path d="M6 1.6 11 10.4H1z" />
          <path d="M6 4.8v2.7M6 9v.1" />
        </>
      )}
    </svg>
  );
}

function glyphFor(state: CriterionModel['state']): 'check' | 'cross' | 'alert' | null {
  switch (state) {
    case 'pass':
      return 'check';
    case 'fail':
      return 'cross';
    case 'stale':
    case 'held':
    case 'missing':
      return 'alert';
    default:
      return null;
  }
}

function CriterionCell({ c, column }: { c: CriterionModel; column: ColumnModel }) {
  const glyph = glyphFor(c.state);
  const negative = c.id === 'requests' && c.fillFrom < c.marker;
  return (
    <div role="cell" className={styles.cell}>
      <div className={styles.crit} data-state={c.state} data-id={c.id}>
        <p className={styles.critLabel} aria-hidden="true">
          <span>{column.label}</span>
          <span className={styles.critRule}>{column.rule}</span>
        </p>
        <p className={styles.critValue}>
          <span className={styles.valueNum}>{c.valueText}</span>
          {glyph ? (
            <span className={styles.stateMark}>
              <Glyph kind={glyph} />
              {c.stateWord ? <span className={styles.stateWord}>{c.stateWord}</span> : null}
            </span>
          ) : null}
        </p>
        <span className={styles.track} aria-hidden="true">
          <span className={styles.zone} style={{ left: pct(c.zoneFrom), width: pct(c.zoneTo - c.zoneFrom) }} />
          <span
            className={styles.fill}
            data-negative={negative || undefined}
            style={{ left: pct(c.fillFrom), width: pct(c.fillTo - c.fillFrom) }}
          />
          <span className={styles.marker} style={{ left: pct(c.marker) }} />
        </span>
      </div>
    </div>
  );
}

function dismissOnEscape(event: KeyboardEvent<HTMLButtonElement>) {
  if (event.key === 'Escape') event.currentTarget.blur();
}

/** Reject reason chip. The full explanation shows on hover and on keyboard focus. */
function ReasonChip({ reason, id }: { reason: ReasonModel; id: string }) {
  return (
    <li className={styles.chipItem}>
      <button
        type="button"
        className={styles.chip}
        data-kind={reason.kind}
        aria-describedby={id}
        onKeyDown={dismissOnEscape}
      >
        <span className={styles.chipLabel}>{reason.label}</span>
        <span className={styles.chipCode}>
          <EnumCode>{reason.code}</EnumCode>
        </span>
      </button>
      <span className={styles.tip}>
        <span className={styles.tipHead} aria-hidden="true">
          {reason.label} <EnumCode>{reason.code}</EnumCode>
        </span>
        <span role="tooltip" id={id}>
          {reason.detail}
        </span>
      </span>
    </li>
  );
}

function ScoreView({ score }: { score: ScoreModel }) {
  return (
    <div className={styles.score}>
      <p className={styles.total}>
        <span className="visually-hidden">총점</span>
        {score.totalText}
        <span className={styles.unit}>점</span>
      </p>
      <div className={styles.stack} aria-hidden="true">
        <div className={styles.stackFill}>
          {SCORE_PART_IDS.map((id) => {
            const part = score.parts.find((p) => p.id === id);
            return part && part.points > 0 ? (
              <span key={id} className={styles.seg} data-part={id} style={{ width: `${part.points}%` }} />
            ) : null;
          })}
        </div>
        {score.penalty > 0 ? <span className={styles.penaltyTick} style={{ left: `${Math.max(0, score.total)}%` }} /> : null}
      </div>
      <ul className={styles.parts}>
        {score.parts.map((p) => (
          <li key={p.id}>
            <i className={styles.swatch} data-part={p.id} aria-hidden="true" />
            <span className={styles.partLabel}>{p.label}</span>
            {p.pointsText}
          </li>
        ))}
        {score.penalty > 0 ? (
          <li data-penalty="true">
            <i className={styles.swatch} data-part="penalty" aria-hidden="true" />
            감점 {score.penaltyText}
          </li>
        ) : null}
      </ul>
    </div>
  );
}

const VERDICT_WORD: Record<Exclude<Verdict, 'pending'>, string> = { pass: '통과', reject: '탈락', excluded: '제외' };
const VERDICT_LAMP: Record<Exclude<Verdict, 'pending'>, LampState> = { pass: 'ok', reject: 'fault', excluded: 'fault' };

function ResultCell({ row, idBase }: { row: RowModel; idBase: string }) {
  if (row.verdict === 'pending') {
    return (
      <div role="cell" className={styles.result}>
        <p className={styles.waitingCell}>판정 전</p>
      </div>
    );
  }
  return (
    <div role="cell" className={styles.result}>
      <div className={styles.resultFlow}>
        <p className={styles.verdict} data-verdict={row.verdict}>
          <span aria-hidden="true">
            <Lamp state={VERDICT_LAMP[row.verdict]} label="" />
          </span>
          {VERDICT_WORD[row.verdict]}
        </p>
        <div className={styles.resultBody}>
          {row.reasons.length > 0 ? (
            <ul className={styles.chips} aria-label="판정 이유">
              {row.reasons.map((reason) => (
                <ReasonChip key={reason.code} reason={reason} id={`${idBase}-${reason.code}`} />
              ))}
            </ul>
          ) : null}
          {row.score ? <ScoreView score={row.score} /> : null}
        </div>
      </div>
    </div>
  );
}

function RankTag({ row }: { row: RowModel }) {
  if (row.winner) return <span className={styles.tagWinner}>선택</span>;
  if (row.leader) {
    return (
      <span className={styles.tagLeader}>
        <span className={styles.now}>현재 </span>1순위
      </span>
    );
  }
  if (row.rank !== null) return <span className={styles.tagRank}>{row.rank}순위</span>;
  return null;
}

interface RowProps {
  row: RowModel;
  columns: ColumnModel[];
  idBase: string;
}

function CandidateRowView({ row, columns, idBase }: RowProps) {
  const staleSample = row.criteria.some((c) => c.state === 'stale');
  return (
    <div
      role="row"
      className={styles.row}
      data-verdict={row.verdict}
      data-winner={row.winner || undefined}
      data-leader={row.leader || undefined}
    >
      <div role="rowheader" className={styles.identity}>
        <p className={styles.nodeName} title={row.hostname}>
          {row.displayName}
        </p>
        <p className={styles.nodeMeta}>
          <ArchTag arch={row.arch} size="sm" />
          <RankTag row={row} />
          {staleSample && row.sampleAgeSeconds !== null && row.verdict !== 'excluded' ? (
            <span className={styles.nodeAge}>표본 {Math.round(row.sampleAgeSeconds)}초 전</span>
          ) : null}
        </p>
        <p className={styles.nodeSub}>{row.hostname}</p>
      </div>
      {row.criteria.map((c, i) => (
        <CriterionCell key={c.id} c={c} column={columns[i]!} />
      ))}
      <ResultCell row={row} idBase={idBase} />
    </div>
  );
}

/**
 * Snapshots arrive up to ~16 times a second during fast-forward, but a recorded
 * decision never changes. Comparing by content keeps unchanged rows from re-rendering.
 */
function sameRow(a: RowProps, b: RowProps): boolean {
  return (
    a.idBase === b.idBase &&
    JSON.stringify(a.row) === JSON.stringify(b.row) &&
    JSON.stringify(a.columns) === JSON.stringify(b.columns)
  );
}

export const CandidateRow = memo(CandidateRowView, sameRow);
