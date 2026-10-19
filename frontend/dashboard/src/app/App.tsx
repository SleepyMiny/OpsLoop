import { useCallback, useMemo, useState } from 'react';
import { CandidateBoard } from '../components/candidates/CandidateBoard';
import { ClusterMimic } from '../components/cluster/ClusterMimic';
import { EngineeringTrack } from '../components/engineering/EngineeringTrack';
import { IncidentPanel } from '../components/incident/IncidentPanel';
import { PipelineLadder } from '../components/incident/PipelineLadder';
import { PolicyPanel } from '../components/policy/PolicyPanel';
import type { DashboardDataSource, SourceStatus } from '../data/source';
import { useDashboard } from '../data/useDashboard';
import { DetailTabs } from './DetailTabs';
import { Header } from './Header';
import { useIdle, usePresenterKeys, useTheme, useWakeLock, type Theme } from './hooks';
import { Narration } from './Narration';
import { PlaybackBar } from './PlaybackBar';
import { RealityDialog } from './RealityDialog';
import { SourceNotice } from './SourceNotice';
import { RegionBoundary } from './RegionBoundary';
import styles from './app.module.css';

const NEXT_THEME: Record<Theme, Theme> = { auto: 'dark', dark: 'light', light: 'auto' };

export function App({ source, kiosk }: { source: DashboardDataSource; kiosk: boolean }) {
  const { snapshot, status, playback } = useDashboard(source);
  const [theme, setTheme] = useTheme();
  const [realityOpen, setRealityOpen] = useState(false);
  const idle = useIdle(5000, kiosk);
  useWakeLock(kiosk);

  const actions = useMemo(
    () => ({
      toggleReality: () => setRealityOpen((o) => !o),
      cycleTheme: () => setTheme(NEXT_THEME[theme]),
    }),
    [setTheme, theme],
  );
  usePresenterKeys(source.demo, actions);
  const closeReality = useCallback(() => setRealityOpen(false), []);

  const scenario = source.demo?.listScenarios().find((s) => s.id === playback?.scenarioId);
  const focus = snapshot?.narration?.focus;
  const resetKey = snapshot?.observedAt;

  return (
    <div className={styles.app} data-kiosk={kiosk || undefined} data-idle={idle || undefined}>
      <Header
        source={source}
        status={status}
        playback={playback}
        theme={theme}
        onCycleTheme={actions.cycleTheme}
        onOpenReality={() => setRealityOpen(true)}
      />

      <SourceNotice kind={source.kind} snapshot={snapshot} status={status} />
      {snapshot ? (
        <main className={styles.stage}>
          <div className={styles.hero}>
            <RegionBoundary name="클러스터" resetKey={resetKey}>
              <ClusterMimic snapshot={snapshot} emphasized={focus === 'cluster'} />
            </RegionBoundary>
          </div>
          <div className={styles.story}>
            <Narration source={source.kind} narration={snapshot.narration} scenario={scenario} />
            <RegionBoundary name="복구 단계" resetKey={resetKey}>
              <PipelineLadder snapshot={snapshot} emphasized={focus === 'pipeline'} />
            </RegionBoundary>
          </div>
          <div className={styles.decision}>
            <RegionBoundary name="후보 평가" resetKey={resetKey}>
              <CandidateBoard snapshot={snapshot} emphasized={focus === 'candidates'} />
            </RegionBoundary>
          </div>
          <div className={styles.detail}>
            <DetailTabs
              focus={focus}
              scenarioKey={playback?.scenarioId ?? snapshot.replay?.experimentId ?? 'live'}
              panels={{
                incident: (
                  <RegionBoundary name="Incident" resetKey={resetKey}>
                    <IncidentPanel snapshot={snapshot} emphasized={focus === 'incident'} />
                  </RegionBoundary>
                ),
                engineering: (
                  <RegionBoundary name="Engineering" resetKey={resetKey}>
                    <EngineeringTrack snapshot={snapshot} emphasized={focus === 'engineering'} />
                  </RegionBoundary>
                ),
                policy: (
                  <RegionBoundary name="Policy" resetKey={resetKey}>
                    <PolicyPanel snapshot={snapshot} emphasized={focus === 'policy'} />
                  </RegionBoundary>
                ),
              }}
            />
          </div>
        </main>
      ) : (
        <LiveUnavailable status={status} />
      )}

      {source.demo && playback ? <PlaybackBar demo={source.demo} playback={playback} hidden={kiosk && idle} /> : null}
      <RealityDialog open={realityOpen} onClose={closeReality} source={source.kind} />
    </div>
  );
}

function LiveUnavailable({ status }: { status: SourceStatus }) {
  const message =
    status.state === 'unconfigured' || status.state === 'error' || status.state === 'stale'
      ? status.message
      : '첫 응답을 기다리는 중입니다.';
  return (
    <main className={styles.unavailable}>
      <h2>Live 데이터에 연결하지 못했습니다</h2>
      <p>{message}</p>
      <p>
        읽기 전용 BFF 실행 상태와 VITE_OPSLOOP_API_BASE 설정을 확인하세요. Demo와 Replay는 명시적으로 선택할 수 있습니다.
      </p>
      <a className={styles.toolButton} href="?source=demo">
        Demo Mode로 보기
      </a>
    </main>
  );
}
