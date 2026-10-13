import { useEffect, useState } from 'react';
import type { DemoController } from '../data/source';

export type Theme = 'auto' | 'light' | 'dark';

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      const saved = localStorage.getItem('opsloop.theme');
      return saved === 'light' || saved === 'dark' ? saved : 'auto';
    } catch {
      return 'auto';
    }
  });
  useEffect(() => {
    const root = document.documentElement;
    if (theme === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', theme);
    try {
      localStorage.setItem('opsloop.theme', theme);
    } catch {
      /* storage unavailable (private window, kiosk policy) — theme still applies */
    }
  }, [theme]);
  return [theme, setTheme];
}

/** Keeps the display awake while the booth runs; silently skipped where unsupported. */
export function useWakeLock(enabled: boolean): void {
  useEffect(() => {
    if (!enabled || !('wakeLock' in navigator)) return;
    let lock: WakeLockSentinel | null = null;
    let cancelled = false;
    const acquire = async () => {
      try {
        lock = await navigator.wakeLock.request('screen');
        if (cancelled) await lock.release();
      } catch {
        /* denied or not visible; retried on visibility change */
      }
    };
    const onVisible = () => {
      if (document.visibilityState === 'visible') void acquire();
    };
    void acquire();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      cancelled = true;
      document.removeEventListener('visibilitychange', onVisible);
      void lock?.release().catch(() => undefined);
    };
  }, [enabled]);
}

/** True after `ms` without pointer or key activity; used to tuck controls away in kiosk mode. */
export function useIdle(ms: number, enabled: boolean): boolean {
  const [idle, setIdle] = useState(false);
  useEffect(() => {
    if (!enabled) {
      setIdle(false);
      return;
    }
    let timer = window.setTimeout(() => setIdle(true), ms);
    const wake = () => {
      setIdle(false);
      window.clearTimeout(timer);
      timer = window.setTimeout(() => setIdle(true), ms);
    };
    const events = ['pointermove', 'pointerdown', 'keydown', 'touchstart'] as const;
    events.forEach((e) => window.addEventListener(e, wake, { passive: true }));
    return () => {
      window.clearTimeout(timer);
      events.forEach((e) => window.removeEventListener(e, wake));
    };
  }, [ms, enabled]);
  return idle;
}

export function toggleFullscreen(): void {
  if (document.fullscreenElement) void document.exitFullscreen().catch(() => undefined);
  else void document.documentElement.requestFullscreen?.().catch(() => undefined);
}

/** Presenter shortcuts. Ignored while typing or when a modifier is held. */
export function usePresenterKeys(
  demo: DemoController | undefined,
  actions: { toggleReality: () => void; cycleTheme: () => void },
): void {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey) return;
      const target = e.target as HTMLElement | null;
      if (target?.closest('input, textarea, select, [contenteditable="true"]')) return;
      const arrow = e.key === 'ArrowLeft' || e.key === 'ArrowRight';
      if (arrow && target?.closest('[role="tablist"]')) return;
      const key = e.key.toLowerCase();
      if (key === 'f') toggleFullscreen();
      else if (key === '?' || key === 'i') actions.toggleReality();
      else if (key === 't') actions.cycleTheme();
      if (!demo) return;
      if (key === ' ' && !target?.closest('button')) {
        e.preventDefault();
        demo.togglePlay();
      } else if (key === 'arrowright') demo.nextChapter();
      else if (key === 'arrowleft') demo.previousChapter();
      else if (key === 'r') demo.restart();
      else if (key === 'l') demo.setAutoLoop(!demo.getPlayback().autoLoop);
      else if (/^[1-9]$/.test(key)) {
        const scenario = demo.listScenarios()[Number(key) - 1];
        if (scenario) demo.select(scenario.id);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [demo, actions]);
}
