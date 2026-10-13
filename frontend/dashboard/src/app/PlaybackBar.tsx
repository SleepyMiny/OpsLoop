import type { DemoController, PlaybackState } from '../data/source';
import { relClock } from '../components/shared/format';
import styles from './app.module.css';

const SPEEDS = [1, 2, 4];

export function PlaybackBar({ demo, playback, hidden }: { demo: DemoController; playback: PlaybackState; hidden: boolean }) {
  const progress = playback.durationMs > 0 ? playback.simElapsedMs / playback.durationMs : 0;
  const fastForward = playback.pace >= 3;
  return (
    <div className={styles.playback} data-hidden={hidden || undefined}>
      <div className={styles.transport}>
        <button type="button" className={styles.iconButton} onClick={() => demo.previousChapter()} aria-label="이전 장면" title="이전 장면 (←)">
          <svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 4v12M16 4l-8 6 8 6z" fill="currentColor" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" /></svg>
        </button>
        <button
          type="button"
          className={styles.playButton}
          onClick={() => demo.togglePlay()}
          aria-label={playback.playing ? '일시 정지' : '재생'}
          title="재생/일시 정지 (Space)"
        >
          {playback.playing ? (
            <svg viewBox="0 0 20 20" aria-hidden="true"><path d="M6 4h3v12H6zM11 4h3v12h-3z" fill="currentColor" /></svg>
          ) : (
            <svg viewBox="0 0 20 20" aria-hidden="true"><path d="M6 4l10 6-10 6z" fill="currentColor" /></svg>
          )}
        </button>
        <button type="button" className={styles.iconButton} onClick={() => demo.nextChapter()} aria-label="다음 장면" title="다음 장면 (→)">
          <svg viewBox="0 0 20 20" aria-hidden="true"><path d="M15 4v12M4 4l8 6-8 6z" fill="currentColor" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" /></svg>
        </button>
      </div>

      <div className={styles.timeline}>
        <div className={styles.timelineMeta}>
          <span>{playback.chapters[playback.chapterIndex]?.label ?? ''}</span>
          <span className={styles.paceNote} data-active={fastForward || undefined}>
            {fastForward ? `대기 구간 빨리 감기 ×${playback.pace}` : '시뮬레이션 시간'}
          </span>
          <span>
            {relClock(playback.simElapsedMs)} / {relClock(playback.durationMs)}
          </span>
        </div>
        <div className={styles.track}>
          <div className={styles.trackFill} style={{ transform: `scaleX(${progress})` }} />
          {playback.chapters.map((c, i) => (
            <button
              key={`${c.atMs}-${c.label}`}
              type="button"
              className={styles.chapterTick}
              style={{ left: `${(c.atMs / playback.durationMs) * 100}%` }}
              aria-current={i === playback.chapterIndex ? 'step' : undefined}
              onClick={() => demo.seek(c.atMs)}
              title={c.label}
            >
              <span className="visually-hidden">{c.label}로 이동</span>
            </button>
          ))}
        </div>
      </div>

      <div className={styles.playbackOptions}>
        <div className={styles.speedGroup} role="group" aria-label="재생 속도">
          {SPEEDS.map((s) => (
            <button key={s} type="button" aria-pressed={playback.speed === s} onClick={() => demo.setSpeed(s)}>
              ×{s}
            </button>
          ))}
        </div>
        <label className={styles.loopToggle}>
          <input type="checkbox" checked={playback.autoLoop} onChange={(e) => demo.setAutoLoop(e.target.checked)} />
          자동 순환
        </label>
      </div>
    </div>
  );
}
