import type { DashboardSnapshot } from '../domain/types';
import type { SourceStatus } from '../data/source';
import styles from './m3.module.css';

const statusLabels = { connecting: '연결 중', ok: '연결됨', stale: 'STALE · 데이터 지연', error: 'DISCONNECTED · 연결 끊김', unconfigured: '미설정' };

export function LiveOverview({ snapshot, status, label }: { snapshot: DashboardSnapshot | null; status: SourceStatus; label: string }) {
  const scope = new URLSearchParams(window.location.search).get('scope') === 'b' ? 'b' : 'a';
  const pods = snapshot?.cluster.nodes.flatMap((node) => node.pods.filter((pod) => pod.namespace === snapshot.workload.namespace && pod.role === 'demo-api')) ?? [];
  const current = status.state === 'ok';
  return <section className={styles.liveCompact} aria-label="Live Overview">
    <div className={styles.liveLine}><h2>Hybrid Kubernetes · 실시간 토폴로지</h2><span className={styles.historyPill}>{statusLabels[status.state]}</span><span>{snapshot ? `${snapshot.workload.namespace}/${snapshot.workload.name}` : '관측 대기'}</span><span>Node Ready {snapshot ? `${snapshot.cluster.nodes.filter(n => n.readiness === 'Ready').length}/${snapshot.cluster.nodes.length}` : '미확인'}</span><nav aria-label="Live 관측 대상"><a href="?source=live&scope=a" aria-current={scope === 'a' ? 'page' : undefined}>Demo A</a><a href="?source=live&scope=b" aria-current={scope === 'b' ? 'page' : undefined}>Demo B</a></nav></div>
    <details><summary>관측 시각 · Policy · UID · 데이터 품질</summary><p>{label}</p>
    <div className={styles.panel}><dl className={styles.outcomes}>
      <div><dt>데이터 최종 관측 시각</dt><dd>{snapshot ? new Date(snapshot.observedAt).toISOString() : '아직 관측 없음'}</dd></div>
      <div><dt>Node Ready</dt><dd>{snapshot ? `${snapshot.cluster.nodes.filter((n) => n.readiness === 'Ready').length} / ${snapshot.cluster.nodes.length}${current ? '' : ' · 이전 관측'}` : '미확인'}</dd></div>
      <div><dt>Prometheus 지표</dt><dd>{snapshot?.diagnostics ? snapshot.diagnostics.metricsAvailable ? current ? '관측됨' : '이전 관측' : '사용 불가 / 미확인' : '미확인'}</dd></div>
      <div><dt>관측 대상</dt><dd>{snapshot ? `${snapshot.workload.namespace}/${snapshot.workload.name}` : '연결 대기'}</dd></div>
      <div><dt>Namespace 내 Demo 역할 Pod Ready</dt><dd>{snapshot ? pods.length ? `${pods.filter((p) => p.ready).length} / ${pods.length}${current ? '' : ' · 이전 관측'}` : '관측된 Pod 없음' : '미확인'}</dd></div>
      <div><dt>Policy</dt><dd>{snapshot ? `${snapshot.policy.spec.enabled ? '활성' : '비활성'} · ${snapshot.policy.spec.mode}${current ? '' : ' · 이전 관측'}` : '미확인'}</dd></div>
      <div><dt>Guard 프로세스 상태</dt><dd>별도 실시간 관측 없음</dd></div>
      <div><dt>Service 직접 HTTP</dt><dd>{snapshot?.http.probes.length ? '하단 probe 시각 확인' : '직접 probe 없음 · 서비스 상태 미확인'}</dd></div>
      <div><dt>Deployment UID</dt><dd><code>{snapshot?.workload.uid ?? '미확인'}</code></dd></div>
    </dl></div></details>
    {!current ? <p className={styles.warning} role="status">{status.state === 'connecting' ? '현재 관측을 기다리는 중입니다.' : 'message' in status ? status.message : ''} 관측 API 연결과 대상 서비스 장애는 별개입니다. 과거 성공 기록으로 현재 상태를 대신하지 않습니다. <a href="?source=replay">기록 Replay 열기</a></p> : null}
  </section>;
}
