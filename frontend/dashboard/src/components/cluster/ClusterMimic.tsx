import { useMemo, type CSSProperties } from 'react';
import { EnumCode, Instrument, Lamp } from '../shared/primitives';
import type { RegionProps } from '../types';
import { buildClusterView, PROBE_SLOTS, routeSentence } from './clusterModel';
import { HttpStrip } from './HttpStrip';
import { MeshBand } from './MeshBand';
import { NodeStation } from './NodeStation';
import { WorkloadStrip } from './WorkloadStrip';
import styles from './cluster.module.css';

const TITLE_ID = 'cluster-mimic-title';

/**
 * Hero region: a control-room mimic of the hybrid k3s testbed. Stations sit on
 * the Tailscale mesh; when OpsLoop moves the workload the route lamp lights from
 * the node it leaves to the node it chose, and the HTTP row below shows what a
 * client outside the cluster experienced.
 */
export function ClusterMimic({ snapshot, emphasized }: RegionProps) {
  const view = useMemo(() => buildClusterView(snapshot), [snapshot]);
  const { cluster } = snapshot;
  const routeLabel = view.route ? routeSentence(view.route, cluster.nodes) : null;
  const stationsStyle = { '--n': Math.max(1, view.stations.length) } as CSSProperties;

  return (
    <Instrument
      title="Hybrid k3s 클러스터"
      labelledBy={TITLE_ID}
      emphasized={emphasized ?? false}
      aside={
        <span className={styles.aside}>
          <EnumCode>{cluster.name}</EnumCode>
          <span>k3s {cluster.k3sVersion}</span>
        </span>
      }
    >
      <div className={styles.root}>
        <p className="visually-hidden">{view.summary}</p>
        <div className={styles.layout}>
          <div className={styles.cluster}>
            {view.stations.length > 0 ? (
              <>
                <div className={styles.stations} style={stationsStyle}>
                  {view.stations.map((station) => (
                    <NodeStation key={station.node.uid} station={station} routeStage={view.route?.stage ?? null} />
                  ))}
                </div>
                <MeshBand stations={view.stations} route={view.route} routeLabel={routeLabel} />
              </>
            ) : (
              <p className={styles.empty}>노드 정보가 아직 없습니다. 데이터 연결 상태를 확인하세요.</p>
            )}
            {view.route && view.routeText ? (
              <p className={styles.routeLine} data-verified={view.route.stage === 'verified' || undefined}>
                <Lamp state={view.route.stage === 'verified' ? 'ok' : 'act'} label="복구 경로" />
                <span>{view.routeText}</span>
              </p>
            ) : null}
            <WorkloadStrip workload={snapshot.workload} servingArch={view.servingArch} quarantine={view.quarantine} />
          </div>
          <HttpStrip http={view.http} slots={PROBE_SLOTS} />
        </div>
      </div>
    </Instrument>
  );
}
