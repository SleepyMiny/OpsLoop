import { describe, expect, it } from 'vitest';
import { bandRoute, meshLinks, sparklinePath, tickCells } from './geometry';

const count = (s: string, ch: string) => s.split(ch).length - 1;

describe('mesh band', () => {
  it('links every pair: neighbours on the bus, the far pair as an arc, dashed when a node is down', () => {
    const links = meshLinks([true, false, true]);
    expect(links.map((l) => [l.key, l.kind, l.span])).toEqual([
      ['0-1', 'bus', 1],
      ['0-2', 'arc', 2],
      ['1-2', 'bus', 1],
    ]);
    expect(links.filter((l) => l.broken).map((l) => l.key)).toEqual(['0-1', '1-2']);
  });

  it('draws a relocation between neighbours along the bus, revealed from the origin side', () => {
    expect(bandRoute(1, 0, 3)).toMatchObject({ kind: 'straight', start: 0, span: 1, dir: -1, to: 0, plateAt: 0.5, plateAlign: 'center' });
    expect(bandRoute(1, 2, 3)).toMatchObject({ kind: 'straight', start: 1, dir: 1, plateAt: 1.5 });
  });

  it('loops back onto the same node for a restart, plate beside it on the side with room', () => {
    expect(bandRoute(1, 1, 3)).toMatchObject({ kind: 'loop', to: 1, plateAlign: 'after' });
    expect(bandRoute(2, 2, 3)).toMatchObject({ kind: 'loop', plateAlign: 'before' });
  });

  it('uses the arc for non-adjacent nodes and only marks the target when the origin is unknown', () => {
    expect(bandRoute(0, 2, 3)).toMatchObject({ kind: 'arc', start: 0, span: 2, dir: 1, plateAt: 2, plateAlign: 'before' });
    expect(bandRoute(null, 0, 3)).toMatchObject({ kind: 'target-only', plateAlign: 'after' });
    expect(bandRoute(0, 5, 3)).toBeNull();
  });
});

describe('sparkline', () => {
  it('breaks the line at missing samples instead of drawing 0%', () => {
    const d = sparklinePath([0.5, 0.5, null, 0.25, 0.25]);
    expect(count(d, 'M')).toBe(2);
    expect(d).not.toMatch(/ 100(?![.\d])/);
    expect(sparklinePath([])).toBe('');
  });

  it('keeps a lone sample visible', () => {
    expect(sparklinePath([null, 0.5, null])).toBe('M50 50l0.01 0');
  });
});

describe('HTTP tick row', () => {
  const probe = (ok: boolean, arch?: 'amd64' | 'arm64') => ({ at: 0, ok, ...(arch ? { arch } : {}) });

  it('right-aligns probes so the newest sits at "now", leaving older slots empty', () => {
    const cells = tickCells([probe(true, 'amd64'), probe(false), probe(true, 'arm64'), probe(true)], 6);
    expect(cells.map((c) => c.state)).toEqual(['empty', 'empty', 'ok', 'fail', 'ok', 'ok']);
    expect(cells[2]).toMatchObject({ arch: 'amd64' });
    expect(cells[4]).toMatchObject({ arch: 'arm64' });
    expect(cells[5]).not.toHaveProperty('arch');
  });

  it('keeps only the last N probes', () => {
    const cells = tickCells(Array.from({ length: 60 }, (_, i) => probe(i >= 55)), 45);
    expect(cells).toHaveLength(45);
    expect(cells.filter((c) => c.state === 'ok')).toHaveLength(5);
  });
});
