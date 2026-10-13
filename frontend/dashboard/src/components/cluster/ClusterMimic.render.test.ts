import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { DemoSimulation } from '../../data/demo/engine';
import { DEMO_SCENARIOS } from '../../data/demo/scenarios';
import { ClusterMimic } from './ClusterMimic';

/**
 * Smoke test: the hero must render every state the Demo Mode engine can reach.
 * Layout-dependent parts (band, tick row) need a browser and are not drawn here.
 */
describe('ClusterMimic renders every demo state', () => {
  for (const scenario of DEMO_SCENARIOS) {
    it(scenario.id, () => {
      const sim = new DemoSimulation(scenario.script);
      for (let t = 0; t <= sim.durationMs; t += 2000) {
        sim.advanceTo(t);
        const html = renderToStaticMarkup(createElement(ClusterMimic, { snapshot: sim.snapshot(), emphasized: false }));
        expect(html).toContain('노트북 VM');
        expect(html).toContain('opsloop-demo/demo-api');
      }
    });
  }

  it('states the route as a sentence for the stacked layout', () => {
    const scenario = DEMO_SCENARIOS.find((s) => s.id === 'node-failure-laptop')!;
    const sim = new DemoSimulation(scenario.script);
    sim.advanceTo(150_000);
    const html = renderToStaticMarkup(createElement(ClusterMimic, { snapshot: sim.snapshot() }));
    expect(html).toContain('물리 서버에서 노트북 VM으로 이동 · 검증 완료');
    expect(html).toContain('이전 Pod 추적 중');
  });
});
