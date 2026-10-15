import { useEffect, useId, useState, type ReactNode } from 'react';
import type { Narration } from '../domain/types';
import styles from './app.module.css';

export type DetailTab = 'incident' | 'engineering' | 'policy';

const TABS: Array<{ id: DetailTab; label: string }> = [
  { id: 'incident', label: 'RecoveryIncident' },
  { id: 'engineering', label: 'Engineering' },
  { id: 'policy', label: 'RecoveryPolicy' },
];

/**
 * Follows the narration focus so an unattended booth always shows the relevant
 * record. A manual pick pins the tab until the scenario changes.
 */
export function DetailTabs({
  focus,
  scenarioKey,
  panels,
}: {
  focus: Narration['focus'] | undefined;
  scenarioKey: string;
  panels: Record<DetailTab, ReactNode>;
}) {
  const [pinned, setPinned] = useState<DetailTab | null>(null);
  const [auto, setAuto] = useState<DetailTab>('incident');
  const baseId = useId();

  useEffect(() => setPinned(null), [scenarioKey]);
  useEffect(() => {
    if (focus === 'engineering' || focus === 'policy' || focus === 'incident') setAuto(focus);
    else if (focus === 'cluster' && auto === 'engineering') setAuto('incident');
  }, [focus, auto]);

  const active = pinned ?? auto;
  return (
    <div className={styles.tabs}>
      <div role="tablist" aria-label="상세 기록" className={styles.tabList}>
        {TABS.map((t) => (
          <button
            key={t.id}
            id={`${baseId}-${t.id}-tab`}
            role="tab"
            type="button"
            aria-selected={active === t.id}
            aria-controls={`${baseId}-${t.id}-panel`}
            tabIndex={active === t.id ? 0 : -1}
            className={styles.tab}
            onClick={() => setPinned(t.id)}
            onKeyDown={(e) => {
              if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
              e.preventDefault();
              const i = TABS.findIndex((x) => x.id === active);
              const next = TABS[(i + (e.key === 'ArrowRight' ? 1 : TABS.length - 1)) % TABS.length]!;
              setPinned(next.id);
              document.getElementById(`${baseId}-${next.id}-tab`)?.focus();
            }}
          >
            {t.label}
          </button>
        ))}
        {pinned ? (
          <button type="button" className={styles.tabAuto} onClick={() => setPinned(null)}>
            해설 따라가기
          </button>
        ) : null}
      </div>
      {TABS.map((t) => (
        <div
          key={t.id}
          id={`${baseId}-${t.id}-panel`}
          role="tabpanel"
          aria-labelledby={`${baseId}-${t.id}-tab`}
          hidden={active !== t.id}
          className={styles.tabPanel}
        >
          {active === t.id ? panels[t.id] : null}
        </div>
      ))}
    </div>
  );
}
