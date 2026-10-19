import type { DashboardSnapshot, DataSourceKind } from '../domain/types';
import type { SourceStatus } from '../data/source';
import styles from './app.module.css';

/** The source and its evidence limits stay visible beside every panel. */
export function SourceNotice({kind, snapshot, status}: {
  kind: DataSourceKind;
  snapshot: DashboardSnapshot | null;
  status: SourceStatus;
}) {
  const replay = snapshot?.replay;
  const warnings = snapshot?.diagnostics?.warnings ?? [];
  return (
    <section className={styles.sourceNotice} aria-label="데이터 출처" data-source={kind}>
      <strong>
        {kind === 'demo'
          ? '시뮬레이션 · 자동 복구 장면은 실측 결과가 아닙니다.'
          : kind === 'replay'
            ? '과거 실증 · ObserveOnly 장애 감지 기록입니다. 자동 복구를 검증한 기록이 아닙니다.'
            : '현재 실측 · 읽기 전용 관측입니다. 연결 실패 시 Demo로 대체하지 않습니다.'}
      </strong>
      {kind === 'live' && status.state !== 'ok' ? (
        <p role="status">
          {status.state === 'connecting' ? '연결 중' : status.message}
          {' · 표시 중인 이전 데이터가 있다면 현재 상태로 해석하지 마세요.'}
        </p>
      ) : null}
      {replay && snapshot ? (
        <>
          <p>{replay.title} · {replay.frameLabel} · {new Date(snapshot.observedAt).toISOString()}</p>
          <details>
            <summary>기록 범위 · 증거 시각 및 출처</summary>
            <ul>{replay.limitations.map((line) => <li key={line}>{line}</li>)}</ul>
            <ul>
              {replay.milestones.map((m) => (
                <li key={m.label}>
                  {m.label}: {m.at !== undefined ? new Date(m.at).toISOString() : ''}
                  {m.elapsedSeconds !== undefined ? ` (${m.elapsedSeconds.toFixed(2)}초)` : ''}
                </li>
              ))}
            </ul>
            <ul>{replay.provenance.map((p) => <li key={p.file}>{p.file}: <code>{p.sha256}</code></li>)}</ul>
          </details>
        </>
      ) : null}
      {warnings.length ? (
        <details>
          <summary>관측 제한 {warnings.length}건</summary>
          <ul>{warnings.map((w) => <li key={w}>{w}</li>)}</ul>
        </details>
      ) : null}
    </section>
  );
}
