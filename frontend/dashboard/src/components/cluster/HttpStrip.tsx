import type { CSSProperties } from 'react';
import { percent } from '../shared/format';
import { EnumCode, Lamp } from '../shared/primitives';
import type { HttpView } from './clusterModel';
import { tickCells } from './geometry';
import styles from './cluster.module.css';

/**
 * What a client outside the cluster sees: the Demo Driver on the Laptop Host
 * probes the Service, newest probe on the right.
 */
export function HttpStrip({ http, slots }: { http: HttpView; slots: number }) {
  const last = http.last;
  const answering = last?.ok ?? false;
  const lastText = last === null ? '기록 없음' : answering ? '응답 중' : '응답 없음';
  const status =
    last === null
      ? null
      : answering
        ? http.lastLatencyMs !== null
          ? `${http.lastLatencyMs} ms`
          : null
        : http.downWholeWindow
          ? `${http.downForSec}초 이상`
          : `${http.downForSec}초째`;

  return (
    <section className={styles.http} aria-label="HTTP 가용성">
      <div className={styles.host}>
        <span className={styles.hostName}>노트북 호스트</span>
        <span className={styles.hostNote}>클러스터 밖의 Demo Driver</span>
      </div>

      <div className={styles.probes}>
        <p className={styles.probeHead}>
          <span className={styles.probeTitle}>HTTP 요청</span>
          <span className={styles.probeEndpoint}>
            <EnumCode>{http.endpoint}</EnumCode>
          </span>
          {http.windowSec !== null ? <span className={styles.probeAxis}>{http.windowSec}초 전부터 지금까지</span> : null}
        </p>
        {http.total > 0 ? (
          <ProbeTicks http={http} slots={slots} />
        ) : (
          <p className={styles.probeEmpty}>아직 요청 기록이 없습니다.</p>
        )}
      </div>

      <p className={styles.availability}>
        <span className={styles.availabilityValue}>{percent(http.availability)}</span>
        <span className={styles.availabilityNote}>
          {http.windowSec !== null ? `최근 ${http.windowSec}초 성공률` : '성공률'}
        </span>
      </p>

      <p className={styles.lastProbe} data-tone={last === null ? 'off' : answering ? 'ok' : 'fault'}>
        <Lamp state={last === null ? 'off' : answering ? 'ok' : 'fault'} label={lastText} />
        <span className={styles.lastProbeText}>{lastText}</span>
        {status ? <span className={styles.lastProbeDetail}>{status}</span> : null}
      </p>
    </section>
  );
}

/**
 * One grid cell per probe slot: success rises above the baseline and carries the
 * serving architecture as a shape (square AMD64, hexagon ARM64), failure drops
 * below it. Plain CSS cells, so bars keep their minimum height at any width.
 */
function ProbeTicks({ http, slots }: { http: HttpView; slots: number }) {
  const fails = http.total - http.okCount;
  return (
    <div
      className={styles.ticks}
      style={{ '--slots': slots } as CSSProperties}
      role="img"
      aria-label={`최근 요청 ${http.total}회 중 성공 ${http.okCount}회, 실패 ${fails}회. 위로 솟은 막대는 성공, 아래로 내려간 막대는 실패입니다.`}
    >
      {tickCells(http.probes, slots).map((cell) => (
        <span
          key={cell.key}
          className={styles.tick}
          data-state={cell.state}
          data-arch={cell.state === 'ok' ? cell.arch : undefined}
        >
          {cell.state === 'ok' && cell.arch ? <span className={styles.tickGlyph} /> : null}
        </span>
      ))}
    </div>
  );
}
