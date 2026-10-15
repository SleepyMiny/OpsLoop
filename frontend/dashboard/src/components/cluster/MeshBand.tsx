import type { CSSProperties } from 'react';
import { EnumCode, Lamp } from '../shared/primitives';
import { ACTION_VERB, routeStageText, type RouteView, type StationView } from './clusterModel';
import { bandRoute, meshLinks } from './geometry';
import styles from './cluster.module.css';

/** Centre of station column `p`, as CSS (the band defines --col from its own width). */
const colX = (p: number) => `calc(var(--col) * ${p + 0.5} + var(--station-gap) * ${p})`;
/** Distance covering `span` column steps. */
const colSpan = (span: number) => `calc((var(--col) + var(--station-gap)) * ${span})`;

/**
 * The Tailscale mesh drawn under the stations, and the one deliberate motion of
 * the dashboard: when OpsLoop selects a target, the route lamp draws from the
 * node the workload leaves to the node it chose. Everything is placed with CSS
 * against the station grid, so it never depends on a JS measurement.
 */
export function MeshBand({
  stations,
  route,
  routeLabel,
}: {
  stations: StationView[];
  route: RouteView | null;
  /** Plain sentence of the route for the accessible name. */
  routeLabel: string | null;
}) {
  const n = stations.length;
  const up = stations.map((s) => s.node.readiness === 'Ready');
  const toIndex = route ? stations.findIndex((s) => s.node.name === route.toName) : -1;
  const fromIndex = route?.fromName ? stations.findIndex((s) => s.node.name === route.fromName) : -1;
  const geometry = route ? bandRoute(fromIndex >= 0 ? fromIndex : null, toIndex, n) : null;
  // Remount on a new action so the route draws again; phase changes of the same action do not replay it.
  const routeKey = route ? `${route.key}:${route.fromName ?? '-'}:${route.toName}` : 'none';
  const verified = route?.stage === 'verified' || undefined;

  const meshLabel = `Tailscale mesh: 노드 ${n}개${up.some((u) => !u) ? ', 응답 없는 노드의 연결은 점선' : ''}.`;
  const label =
    route && routeLabel ? `${meshLabel} 복구 경로: ${routeLabel}, ${routeStageText(route)}.` : meshLabel;

  return (
    <div className={styles.band} style={{ '--n': n } as CSSProperties} role="img" aria-label={label}>
      {stations.map((s, i) => (
        <span key={`stub-${s.node.uid}`} className={styles.stub} style={{ left: colX(i) }} />
      ))}
      {n > 0 ? <span className={styles.meshLabel}>Tailscale mesh</span> : null}
      {meshLinks(up).map((link) => (
        <span
          key={link.key}
          className={styles.meshLink}
          data-kind={link.kind}
          data-broken={link.broken || undefined}
          style={{ left: colX(link.from), width: colSpan(link.span) }}
        />
      ))}

      {route && geometry && geometry.kind !== 'target-only' ? (
        <span
          key={`route-${routeKey}`}
          className={styles.route}
          data-kind={geometry.kind}
          data-dir={geometry.dir}
          data-verified={verified}
          style={geometry.kind === 'loop' ? { left: colX(geometry.to) } : { left: colX(geometry.start), width: colSpan(geometry.span) }}
        />
      ) : null}
      {route && geometry?.kind === 'straight' ? (
        <span
          key={`arrow-${routeKey}`}
          className={styles.routeArrow}
          data-dir={geometry.dir}
          data-verified={verified}
          style={{ left: colX(geometry.to) }}
        />
      ) : null}

      {stations.map((s, i) => {
        const role = i === toIndex ? 'target' : i === fromIndex ? 'origin' : undefined;
        return (
          <span
            key={`stop-${s.node.uid}`}
            className={styles.stop}
            data-role={role}
            data-down={!up[i] || undefined}
            data-verified={(role === 'target' && verified) || undefined}
            style={{ left: colX(i) }}
          />
        );
      })}

      {route && geometry ? (
        <span
          key={`plate-${routeKey}`}
          className={styles.routePlate}
          data-align={geometry.plateAlign}
          data-kind={geometry.kind}
          data-verified={verified}
          style={{
            left: colX(geometry.plateAt),
            maxWidth:
              geometry.plateAlign === 'center'
                ? `calc(${colSpan(geometry.span)} - var(--stop-r) * 2 - 2rem)`
                : 'calc(var(--col) + var(--station-gap) * 0.5 - 2.5rem)',
          }}
        >
          <Lamp state={verified ? 'ok' : 'act'} label={verified ? '검증 완료' : 'OpsLoop 조치'} />
          <strong>{ACTION_VERB[route.actionType]}</strong>
          <span className={styles.routeCode}>
            <EnumCode>{route.actionType}</EnumCode>
          </span>
          <span className={styles.routeStage}>{routeStageText(route)}</span>
        </span>
      ) : null}
    </div>
  );
}
