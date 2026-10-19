import type { DashboardDataSource, PlaybackState, SourceStatus } from '../data/source';
import { relClock } from '../components/shared/format';
import type { Theme } from './hooks';
import { toggleFullscreen } from './hooks';
import styles from './app.module.css';

const TAG_LABEL = { 'demo-a': 'Demo A', 'demo-b': 'Demo B', negative: '경계 사례' } as const;
const LIVE_STATUS_LABEL: Record<SourceStatus['state'], string> = {
  connecting: '연결 중',
  ok: '연결됨',
  stale: '데이터 지연',
  error: '연결 끊김',
  unconfigured: '연결 안 됨',
};

export function Header({
  source,
  status,
  playback,
  theme,
  onCycleTheme,
  onOpenReality,
}: {
  source: DashboardDataSource;
  status: SourceStatus;
  playback: PlaybackState | null;
  theme: Theme;
  onCycleTheme: () => void;
  onOpenReality: () => void;
}) {
  const scenarios = source.demo?.listScenarios() ?? [];
  return (
    <header className={styles.header}>
      <div className={styles.brand}>
        <svg viewBox="0 0 32 32" className={styles.mark} aria-hidden="true">
          <path d="M26 16a10 10 0 1 1-4.2-8.15" fill="none" stroke="currentColor" strokeWidth="3.2" strokeLinecap="round" />
          <path d="M18.5 4.5h5.2v5.2" fill="none" stroke="currentColor" strokeWidth="3.2" strokeLinecap="round" strokeLinejoin="round" />
          <circle cx="16" cy="16" r="3.4" fill="var(--c-act)" />
        </svg>
        <div>
          <h1 className={styles.wordmark}>OpsLoop</h1>
          <p className={styles.tagline}>장애를 감지하고, 근거로 옮기고, 검증합니다</p>
        </div>
      </div>

      {source.kind === 'demo' ? (
        <p className={styles.modeBadge} data-mode="demo">
          <strong>Demo Mode</strong>
          <span>시뮬레이션 데이터</span>
        </p>
      ) : source.kind === 'replay' ? (
        <p className={styles.modeBadge} data-mode="replay"><strong>Evidence Replay</strong><span>과거 실환경 기록 · 현재 상태 아님</span></p>
      ) : (
        <p className={styles.modeBadge} data-mode="live" data-state={status.state}>
          <strong>Live</strong>
          <span>{LIVE_STATUS_LABEL[status.state]}</span>
        </p>
      )}

      {source.demo && playback ? (
        <nav className={styles.scenarios} aria-label="시연 시나리오">
          {scenarios.map((s, i) => (
            <button
              key={s.id}
              type="button"
              className={styles.scenario}
              aria-pressed={s.id === playback.scenarioId}
              onClick={() => source.demo?.select(s.id)}
              title={`${s.summary} (단축키 ${i + 1})`}
            >
              <span className={styles.scenarioTag} data-tag={s.tag}>
                {TAG_LABEL[s.tag]}
              </span>
              <span className={styles.scenarioTitle}>{s.title}</span>
              <span className={styles.scenarioShort}>{s.shortTitle}</span>
            </button>
          ))}
        </nav>
      ) : (
        <div className={styles.scenarios}>
          {source.replay ? <ReplayControls replay={source.replay} /> : null}
        </div>
      )}

      <div className={styles.headerTools}>
        <nav aria-label="데이터 모드" className={styles.modeLinks}>
          {(['demo', 'live', 'replay'] as const).map((mode) => <a key={mode} href={`?source=${mode}`} aria-current={source.kind === mode ? 'page' : undefined}>{mode === 'demo' ? 'Demo' : mode === 'live' ? 'Live' : 'Replay'}</a>)}
        </nav>
        {playback ? (
          <span className={styles.simClock} title="시나리오 시작부터의 시뮬레이션 시간">
            {relClock(playback.simElapsedMs)}
          </span>
        ) : null}
        <button type="button" className={styles.toolButton} onClick={onOpenReality}>
          무엇이 실제인가요?
        </button>
        <button
          type="button"
          className={styles.iconButton}
          onClick={onCycleTheme}
          aria-label={`화면 테마: ${theme === 'auto' ? '시스템' : theme === 'light' ? '밝게' : '어둡게'}`}
          title="테마 전환 (T)"
        >
          <svg viewBox="0 0 20 20" aria-hidden="true">
            <circle cx="10" cy="10" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.8" />
            <path d="M10 3.5a6.5 6.5 0 0 1 0 13z" fill="currentColor" />
          </svg>
        </button>
        <button type="button" className={styles.iconButton} onClick={toggleFullscreen} aria-label="전체 화면" title="전체 화면 (F)">
          <svg viewBox="0 0 20 20" aria-hidden="true">
            <path d="M3 8V3h5M12 3h5v5M17 12v5h-5M8 17H3v-5" fill="none" stroke="currentColor" strokeWidth="1.8" />
          </svg>
        </button>
      </div>
    </header>
  );
}

function ReplayControls({replay}: {replay: NonNullable<DashboardDataSource['replay']>}) {
  const state = replay.getState();
  return <div className={styles.replayControls}>
    <label>실증 기록 <select value={state.experimentId} onChange={(e) => replay.select(e.target.value)}>{replay.listExperiments().map((e) => <option value={e.id} key={e.id}>{e.title}</option>)}</select></label>
    <label>보존 시점 <select value={state.frameIndex} onChange={(e) => replay.setFrame(Number(e.target.value))}>{state.frameLabels.map((label, i) => <option value={i} key={i}>{label}</option>)}</select></label>
  </div>;
}
