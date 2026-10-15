import type { ReactNode } from 'react';
import type { Arch } from '../../domain/types';
import styles from './primitives.module.css';

export type LampState = 'ok' | 'caution' | 'fault' | 'act' | 'off';

/** Round state lamp. The label is always rendered for screen readers. */
export function Lamp({ state, label, pulse = false }: { state: LampState; label: string; pulse?: boolean }) {
  return (
    <span className={styles.lamp} data-state={state} data-pulse={pulse || undefined} role="img" aria-label={label} />
  );
}

/** Architecture identity chip: square glyph for AMD64, hexagon for ARM64. */
export function ArchTag({ arch, size = 'md' }: { arch: Arch; size?: 'sm' | 'md' | 'lg' }) {
  return (
    <span className={styles.arch} data-arch={arch} data-size={size}>
      <svg viewBox="0 0 12 12" aria-hidden="true" className={styles.archGlyph}>
        {arch === 'amd64' ? (
          <rect x="1.5" y="1.5" width="9" height="9" rx="1" />
        ) : (
          <polygon points="6,0.8 10.6,3.4 10.6,8.6 6,11.2 1.4,8.6 1.4,3.4" />
        )}
      </svg>
      {arch.toUpperCase()}
    </span>
  );
}

/** Instrument frame used by every dashboard region. */
export function Instrument({
  title,
  aside,
  children,
  emphasized = false,
  className,
  labelledBy,
}: {
  title: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
  emphasized?: boolean;
  className?: string;
  labelledBy?: string;
}) {
  return (
    <section
      className={[styles.instrument, className].filter(Boolean).join(' ')}
      data-emphasized={emphasized || undefined}
      aria-labelledby={labelledBy}
    >
      <header className={styles.instrumentHead}>
        <h2 id={labelledBy} className={styles.instrumentTitle}>
          {title}
        </h2>
        {aside ? <div className={styles.instrumentAside}>{aside}</div> : null}
      </header>
      <div className={styles.instrumentBody}>{children}</div>
    </section>
  );
}

/** Raw contract value shown next to a friendly label, e.g. CPU_PRESSURE. */
export function EnumCode({ children }: { children: ReactNode }) {
  return <code className={styles.enum}>{children}</code>;
}
