import type { CSSProperties } from 'react';
import { percent } from '../shared/format';
import { ArchTag, EnumCode, Lamp } from '../shared/primitives';
import type { GaugeView, PodView, QuietGroup, RouteStage, StationView } from './clusterModel';
import { sparklinePath } from './geometry';
import styles from './cluster.module.css';

/** One station on the mimic: nameplate, CPU/Memory recorders, and the Pods it hosts. */
export function NodeStation({ station, routeStage }: { station: StationView; routeStage: RouteStage | null }) {
  const { node, readiness, gauges, notice, demoPods, quiet, routeRole } = station;
  const [cpu, mem] = gauges;
  return (
    <article
      className={styles.station}
      data-down={node.readiness !== 'Ready' || undefined}
      data-route={routeRole ?? undefined}
      data-route-stage={routeRole && routeRole !== 'origin' ? (routeStage ?? undefined) : undefined}
      aria-label={`${node.displayName} 노드`}
    >
      <header className={styles.plate}>
        <Lamp state={readiness.tone} label={`노드 상태 ${readiness.label}`} />
        <h3 className={styles.stationName}>{node.displayName}</h3>
        <span className={styles.plateArch}>
          <ArchTag arch={node.arch} />
        </span>
        <p className={styles.plateSub}>
          <span>{station.siteLabel}</span>
          <span className={styles.plateRole} data-role={node.role}>
            {station.roleLabel}
          </span>
        </p>
      </header>

      <div className={styles.meta}>
        <span className={styles.readiness} data-tone={readiness.tone}>
          {readiness.label}
        </span>
        {readiness.code ? (
          <span className={styles.metaCode}>
            <EnumCode>{readiness.code}</EnumCode>
          </span>
        ) : null}
        {/* Compact layouts move the architecture here so the node name keeps the plate width. */}
        <span className={styles.metaArch}>
          <ArchTag arch={node.arch} size="sm" />
        </span>
        {station.flags.map((flag) => (
          <span key={flag} className={styles.flag}>
            {flag}
          </span>
        ))}
        <span className={styles.metaName}>
          <EnumCode>{node.name}</EnumCode>
        </span>
        {quiet.length > 0 ? <QuietPods groups={quiet} /> : null}
      </div>

      <div className={styles.gauges}>
        <GaugeRow gauge={cpu} row={1} />
        <GaugeRow gauge={mem} row={2} />
        {notice ? (
          <p className={styles.notice} data-kind={notice.kind}>
            <span className={styles.noticeLamp}>
              <Lamp state="caution" label="주의" />
            </span>
            <span>
              {notice.lead}
              {notice.rest ? <span className={styles.noticeRest}> · {notice.rest}</span> : null}
            </span>
          </p>
        ) : null}
      </div>

      {demoPods.length > 0 ? (
        <ul className={styles.demoPods} aria-label="Demo 워크로드 Pod">
          {demoPods.map((pod) => (
            <DemoPodToken key={pod.uid} pod={pod} slim={demoPods.length > 1 && pod.presence !== 'solid'} />
          ))}
        </ul>
      ) : null}
    </article>
  );
}

function GaugeRow({ gauge, row }: { gauge: GaugeView; row: 1 | 2 }) {
  const fill = gauge.value === null ? 0 : Math.min(1, Math.max(0, gauge.value));
  const limitY = 100 - gauge.threshold * 100;
  const rowStyle: CSSProperties = { gridRow: row };
  return (
    <>
      <span className={styles.gaugeName} style={rowStyle}>
        {gauge.label}
      </span>
      <span className={styles.gaugeValue} style={rowStyle} data-level={gauge.level}>
        {percent(gauge.value)}
      </span>
      <span className={styles.chart} style={rowStyle} data-level={gauge.level}>
        <svg className={styles.chartSvg} viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
          <line className={styles.chartLimit} x1="0" x2="100" y1={limitY} y2={limitY} />
          <path className={styles.chartLine} d={sparklinePath(gauge.history)} />
        </svg>
        {gauge.level === 'over' ? <span className={styles.overTag}>상한 {percent(gauge.threshold)} 초과</span> : null}
      </span>
      <span
        className={styles.level}
        style={{ ...rowStyle, ['--fill' as string]: fill, ['--limit' as string]: gauge.threshold }}
        data-level={gauge.level}
        aria-hidden="true"
      >
        <span className={styles.levelFill} />
        <span className={styles.levelLimit} />
      </span>
    </>
  );
}

/** A slim token is the outgoing Pod sharing a node with its replacement (restart): one quiet line. */
function DemoPodToken({ pod, slim }: { pod: PodView; slim: boolean }) {
  return (
    <li
      className={styles.demoPod}
      data-tone={pod.tone}
      data-presence={pod.presence}
      data-slim={slim || undefined}
      title={[pod.name, pod.code, pod.attemptId].filter(Boolean).join(' · ')}
    >
      <span className={styles.demoPodMain}>
        <Lamp state={pod.tone} label={pod.label} />
        <span className={styles.demoPodName}>{slim ? '이전 Pod' : 'Demo 워크로드'}</span>
        <span className={styles.demoPodState}>{pod.label}</span>
      </span>
      {!slim && (pod.code || pod.attemptId) ? (
        <span className={styles.demoPodCodes}>
          {pod.code ? <EnumCode>{pod.code}</EnumCode> : null}
          {pod.attemptId ? <EnumCode>{pod.attemptId}</EnumCode> : null}
        </span>
      ) : null}
    </li>
  );
}

function QuietPods({ groups }: { groups: QuietGroup[] }) {
  const summary = groups.map((g) => `${g.label} ${g.pods.length}개`).join(', ');
  return (
    <span className={styles.quiet} role="img" aria-label={`그 밖의 Pod: ${summary}`}>
      {groups.map((g) => (
        <span key={g.role} className={styles.quietGroup} title={`${g.label}: ${g.pods.map((p) => p.name).join(', ')}`}>
          {g.pods.map((p) => (
            <span key={p.uid} className={styles.quietToken} data-tone={p.tone} />
          ))}
        </span>
      ))}
    </span>
  );
}
