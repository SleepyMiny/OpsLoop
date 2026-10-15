import type { Arch, HttpProbe } from '../../domain/types';

/**
 * Layout rules for the mimic band and the HTTP tick row, in abstract units so
 * CSS places them: a band position `p` is the centre of station column `p`
 * (CSS turns it into calc(col * (p + .5) + gap * p)). No DOM measurement, so the
 * drawing always follows the grid, at every size and on the first frame.
 */

export interface MeshLink {
  key: string;
  /** Left station index. */
  from: number;
  /** Number of column steps to the right station. */
  span: number;
  /** Neighbours share the bus; farther pairs get an arc under it. */
  kind: 'bus' | 'arc';
  /** Either end is not Ready: drawn dashed as a broken link. */
  broken: boolean;
}

/** Tailscale is a full mesh: every pair of nodes is linked. */
export function meshLinks(up: readonly boolean[]): MeshLink[] {
  const links: MeshLink[] = [];
  for (let i = 0; i < up.length; i++) {
    for (let j = i + 1; j < up.length; j++) {
      links.push({ key: `${i}-${j}`, from: i, span: j - i, kind: j === i + 1 ? 'bus' : 'arc', broken: !up[i] || !up[j] });
    }
  }
  return links;
}

export interface BandRoute {
  kind: 'straight' | 'arc' | 'loop' | 'target-only';
  /** Leftmost station index the drawn route covers. */
  start: number;
  span: number;
  /** Travel direction: 1 to the right, -1 to the left (the draw reveals from the origin side). */
  dir: 1 | -1;
  to: number;
  /** Station position (may be fractional) where the route plate sits. */
  plateAt: number;
  /**
   * centre: on the bus between the two stations (straight route).
   * after/before: beside the destination, on the side with room, for loops,
   * arcs (whose midpoint is another station) and unknown origins.
   */
  plateAlign: 'center' | 'after' | 'before';
}

export function bandRoute(fromIndex: number | null, toIndex: number, count: number): BandRoute | null {
  if (toIndex < 0 || toIndex >= count) return null;
  const beside: BandRoute['plateAlign'] = toIndex === count - 1 && count > 1 ? 'before' : 'after';
  if (fromIndex === null || fromIndex < 0 || fromIndex >= count) {
    return { kind: 'target-only', start: toIndex, span: 0, dir: 1, to: toIndex, plateAt: toIndex, plateAlign: beside };
  }
  if (fromIndex === toIndex) {
    return { kind: 'loop', start: toIndex, span: 0, dir: 1, to: toIndex, plateAt: toIndex, plateAlign: beside };
  }
  const start = Math.min(fromIndex, toIndex);
  const span = Math.abs(toIndex - fromIndex);
  const dir = toIndex > fromIndex ? 1 : -1;
  return span === 1
    ? { kind: 'straight', start, span, dir, to: toIndex, plateAt: (fromIndex + toIndex) / 2, plateAlign: 'center' }
    : { kind: 'arc', start, span, dir, to: toIndex, plateAt: toIndex, plateAlign: beside };
}

/**
 * Sparkline in a 100×100 box (drawn with preserveAspectRatio="none" and a
 * non-scaling stroke). `null` breaks the line: a gap, never a drop to 0%.
 */
export function sparklinePath(values: ReadonlyArray<number | null>): string {
  const n = values.length;
  if (n === 0) return '';
  const step = n > 1 ? 100 / (n - 1) : 0;
  const r1 = (x: number) => Math.round(x * 10) / 10;
  let d = '';
  let pen = false;
  values.forEach((v, i) => {
    if (v === null || Number.isNaN(v)) {
      pen = false;
      return;
    }
    const x = n > 1 ? r1(i * step) : 100;
    const y = r1(100 - Math.min(1, Math.max(0, v)) * 100);
    if (pen) {
      d += `L${x} ${y}`;
    } else {
      // A lone sample still shows as a short tick.
      const next = values[i + 1];
      d += `M${x} ${y}` + (next === null || next === undefined ? 'l0.01 0' : '');
      pen = true;
    }
  });
  return d;
}

export type TickCell = { key: number; state: 'empty' } | { key: number; state: 'ok'; arch?: Arch } | { key: number; state: 'fail' };

/**
 * HTTP probe row with a fixed number of slots, newest on the right. Missing
 * history stays empty on the left so "now" is always the right edge.
 */
export function tickCells(probes: readonly HttpProbe[], slots: number): TickCell[] {
  const recent = probes.slice(-slots);
  const offset = slots - recent.length;
  return Array.from({ length: slots }, (_, i): TickCell => {
    const p = recent[i - offset];
    if (!p) return { key: i, state: 'empty' };
    if (!p.ok) return { key: i, state: 'fail' };
    return p.arch ? { key: i, state: 'ok', arch: p.arch } : { key: i, state: 'ok' };
  });
}
