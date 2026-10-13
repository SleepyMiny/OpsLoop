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
    const src = new HttpDataSource({ baseUrl: 'http://bff.test/', pollMs: 5, fetchImpl });
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
