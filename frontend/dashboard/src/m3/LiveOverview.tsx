import type { DashboardSnapshot } from '../domain/types';
import type { SourceStatus } from '../data/source';
import styles from './m3.module.css';

const statusLabels = { connecting: '연결 중', ok: '연결됨', stale: 'STALE · 데이터 지연', error: 'DISCONNECTED · 연결 끊김', unconfigured: '미설정' };

export function LiveOverview({ snapshot, status, label }: { snapshot: DashboardSnapshot | null; status: SourceStatus; label: string }) {
  const scope = new URLSearchParams(window.location.search).get('scope') === 'b' ? 'b' : 'a';
  const pods = snapshot?.cluster.nodes.flatMap((node) => node.pods.filter((pod) => pod.namespace === snapshot.workload.namespace && pod.role === 'demo-api')) ?? [];
  const current = status.state === 'ok';
  return <section className={styles.content} aria-label="Live Overview">
    <div className={styles.pageHeading}><div><p className={styles.eyebrow}>CURRENT OBSERVATION / READ ONLY</p><h1>Hybrid Kubernetes · Live Overview</h1><p>{label}</p></div><span className={styles.historyPill}>{statusLabels[status.state]}</span></div>
    <nav className={styles.runSelector} aria-label="Live 관측 대상"><a href="?source=live&scope=a" aria-current={scope === 'a' ? 'page' : undefined}>Demo A · Runtime</a><a href="?source=live&scope=b" aria-current={scope === 'b' ? 'page' : undefined}>Demo B · Engineering</a><span>Namespace와 Deployment UID로 관측 대상을 구분합니다</span></nav>
    <div className={styles.panel}><dl className={styles.outcomes}>
      <div><dt>데이터 최종 관측 시각</dt><dd>{snapshot ? new Date(snapshot.observedAt).toISOString() : '아직 관측 없음'}</dd></div>
      <div><dt>Node Ready</dt><dd>{snapshot ? `${snapshot.cluster.nodes.filter((n) => n.readiness === 'Ready').length} / ${snapshot.cluster.nodes.length}${current ? '' : ' · 이전 관측'}` : '미확인'}</dd></div>
      <div><dt>Prometheus 지표</dt><dd>{snapshot?.diagnostics ? snapshot.diagnostics.metricsAvailable ? current ? '관측됨' : '이전 관측' : '사용 불가 / 미확인' : '미확인'}</dd></div>
      <div><dt>관측 대상</dt><dd>{snapshot ? `${snapshot.workload.namespace}/${snapshot.workload.name}` : '연결 대기'}</dd></div>
      <div><dt>Namespace 내 Demo 역할 Pod Ready</dt><dd>{snapshot ? pods.length ? `${pods.filter((p) => p.ready).length} / ${pods.length}${current ? '' : ' · 이전 관측'}` : '관측된 Pod 없음' : '미확인'}</dd></div>
      <div><dt>Policy</dt><dd>{snapshot ? `${snapshot.policy.spec.enabled ? '활성' : '비활성'} · ${snapshot.policy.spec.mode}${current ? '' : ' · 이전 관측'}` : '미확인'}</dd></div>
      <div><dt>Guard 프로세스 상태</dt><dd>별도 실시간 관측 없음</dd></div>
      <div><dt>Service 직접 HTTP</dt><dd>{snapshot?.http.probes.length ? '하단 probe 시각 확인' : '직접 probe 없음 · Pod Ready와 구분'}</dd></div>
      <div><dt>Deployment UID</dt><dd><code>{snapshot?.workload.uid ?? '미확인'}</code></dd></div>
    </dl></div>
    {!current ? <p className={styles.warning} role="status">{status.state === 'connecting' ? '현재 관측을 기다리는 중입니다.' : 'message' in status ? status.message : ''} 과거 성공 기록으로 현재 상태를 대신하지 않습니다. <a href="?source=replay">기록 Replay 열기</a></p> : null}
  </section>;
}
