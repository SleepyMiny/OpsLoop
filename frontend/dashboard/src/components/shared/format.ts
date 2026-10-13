const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

export function percent(ratio: number | null | undefined, digits = 0): string {
  if (ratio === null || ratio === undefined || Number.isNaN(ratio)) return '—';
  return `${(ratio * 100).toFixed(digits)}%`;
}

export function bytes(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '—';
  if (Math.abs(value) >= GiB) return `${(value / GiB).toFixed(1)} GiB`;
  return `${Math.round(value / MiB)} MiB`;
}

export function millicores(value: number): string {
  return value >= 1000 ? `${(value / 1000).toFixed(value % 1000 === 0 ? 0 : 2)} vCPU` : `${value}m`;
}

/** "1분 57초" style duration for visitor-facing copy. */
export function duration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || ms < 0) return '—';
  const total = Math.round(ms / 1000);
  const m = Math.floor(total / 60);
  const s = total % 60;
  if (m === 0) return `${s}초`;
  return s === 0 ? `${m}분` : `${m}분 ${s}초`;
}

/** "T+01:57" relative clock used on timelines. */
export function relClock(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `T+${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

/** Wall-clock HH:MM:SS for event logs. */
export function clock(epochMs: number): string {
  const d = new Date(epochMs);
  return [d.getHours(), d.getMinutes(), d.getSeconds()].map((n) => String(n).padStart(2, '0')).join(':');
}

export function shortHash(value: string, length = 12): string {
  const [algo, hex] = value.includes(':') ? value.split(':', 2) : [null, value];
  const short = (hex ?? '').slice(0, length);
  return algo ? `${algo}:${short}` : short;
}
