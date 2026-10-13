import { describe, expect, it } from 'vitest';
import { createDataSource } from '../createDataSource';
import { DemoSimulation } from '../demo/engine';
import { DEMO_SCENARIOS } from '../demo/scenarios';
import { HttpDataSource, parseSnapshot } from './httpSource';

const sample = () => new DemoSimulation(DEMO_SCENARIOS[0]!.script).snapshot();

describe('parseSnapshot', () => {
  it('forces source=live and drops demo narration', () => {
    const parsed = parseSnapshot(JSON.parse(JSON.stringify(sample())));
    expect(parsed.source).toBe('live');
    expect(parsed.narration).toBeUndefined();
  });

  it('rejects payloads that would break rendering', () => {
    expect(() => parseSnapshot({ observedAt: 1 })).toThrow(/cluster/);
    expect(() => parseSnapshot([])).toThrow();
  });

  it('rejects unconverted Kubernetes formats and unknown enums with a field path', () => {
    const quantity = JSON.parse(JSON.stringify(sample()));
    quantity.cluster.nodes[0].allocatable.cpuMillicores = '3900m';
    expect(() => parseSnapshot(quantity)).toThrow(/cluster\.nodes\[0\]\.allocatable/);

    const time = JSON.parse(JSON.stringify(sample()));
    time.cluster.nodes[1].usage.cpu.observedAt = '2026-10-13T05:00:00Z';
    expect(() => parseSnapshot(time)).toThrow(/cluster\.nodes\[1\]\.usage\.cpu/);

    const pressure = JSON.parse(JSON.stringify(sample()));
    pressure.cluster.nodes[2].pressure = ['NetworkUnavailable'];
    expect(() => parseSnapshot(pressure)).toThrow(/pressure/);
  });
});

describe('HttpDataSource', () => {
  it('reports unconfigured instead of falling back to demo data', () => {
    const src = new HttpDataSource({ baseUrl: undefined });
    src.start();
    expect(src.getStatus().state).toBe('unconfigured');
    expect(src.getSnapshot()).toBeNull();
  });

  it('keeps the last good snapshot and reports the error', async () => {
    let calls = 0;
    const body = JSON.parse(JSON.stringify(sample()));
    const fetchImpl = (async () => {
      calls += 1;
      if (calls === 1) return new Response(JSON.stringify(body), { status: 200 });
      return new Response('down', { status: 503 });
    }) as typeof fetch;
    const src = new HttpDataSource({ baseUrl: 'http://bff.test/', pollMs: 5, fetchImpl, now: () => body.observedAt });
    const updates: string[] = [];
    src.subscribe(() => updates.push(src.getStatus().state));
    src.start();
    await new Promise((r) => setTimeout(r, 40));
    src.stop();
    expect(updates[0]).toBe('ok');
    expect(updates).toContain('error');
    expect(src.getSnapshot()?.source).toBe('live');
  });
});

describe('createDataSource', () => {
  it('defaults to Demo Mode with no env or params', () => {
    expect(createDataSource({ search: '', env: {} }).kind).toBe('demo');
  });

  it('only takes the API base from env, never from the URL', () => {
    const src = createDataSource({ search: '?source=live&api=http://evil.test', env: {} });
    expect(src.kind).toBe('live');
    expect(src.getStatus().state).toBe('unconfigured');
  });
});

describe('HttpDataSource lifecycle', () => {
  const body = () => JSON.parse(JSON.stringify(sample()));

  it('reports a stale snapshot instead of "connected" when observedAt is old', async () => {
    const payload = body();
    const fetchImpl = (async () => new Response(JSON.stringify(payload), { status: 200 })) as typeof fetch;
    const src = new HttpDataSource({
      baseUrl: 'http://bff.test',
      pollMs: 1000,
      fetchImpl,
      now: () => payload.observedAt + 60_000,
    });
    const seen: string[] = [];
    src.subscribe(() => seen.push(src.getStatus().state));
    src.start();
    await new Promise((r) => setTimeout(r, 20));
    src.stop();
    expect(seen[0]).toBe('stale');
  });

  it('start → stop → start keeps a single polling loop and does not report the abort as an error', async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    let calls = 0;
    const payload = body();
    const fetchImpl = ((_url: string, init?: RequestInit) =>
      new Promise<Response>((resolve, reject) => {
        calls += 1;
        inFlight += 1;
        maxInFlight = Math.max(maxInFlight, inFlight);
        const t = setTimeout(() => {
          inFlight -= 1;
          resolve(new Response(JSON.stringify(payload), { status: 200 }));
        }, 30);
        init?.signal?.addEventListener('abort', () => {
          clearTimeout(t);
          inFlight -= 1;
          reject(new DOMException('aborted', 'AbortError'));
        });
      })) as typeof fetch;
    const src = new HttpDataSource({ baseUrl: 'http://bff.test', pollMs: 10, fetchImpl, now: () => payload.observedAt });
    const seen: string[] = [];
    src.subscribe(() => seen.push(src.getStatus().state));
    src.start();
    src.stop();
    src.start();
    await new Promise((r) => setTimeout(r, 200));
    src.stop();
    expect(seen).not.toContain('error');
    expect(seen).toContain('ok');
    expect(calls).toBeGreaterThan(2);
    // Two loops would overlap their 30 ms requests; one loop never has two in flight.
    expect(maxInFlight).toBe(1);
  });
});
