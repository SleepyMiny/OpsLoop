import { describe, expect, it } from 'vitest';
import { DemoDataSource } from './demoSource';
import { DemoSimulation } from './engine';
import { DEMO_SCENARIOS } from './scenarios';

function run(id: string) {
  const scenario = DEMO_SCENARIOS.find((s) => s.id === id)!;
  const sim = new DemoSimulation(scenario.script);
  sim.advanceTo(sim.durationMs);
  return sim.snapshot();
}

describe('Demo scenarios reach the outcomes the architecture describes', () => {
  it('Demo A with Azure loaded recovers on the laptop VM (AMD64)', () => {
    const s = run('node-failure-laptop');
    const rt = s.activeIncident!.status.runtime;
    expect(rt.phase).toBe('RECOVERED');
    expect(rt.classification).toBe('INFRASTRUCTURE_FAILURE');
    expect(rt.decision!.selectedHostname).toBe('opsloop-control');
    const azure = rt.decision!.candidates.find((c) => c.arch === 'arm64')!;
    expect(azure.rejectReasons).toContain('CPU_PRESSURE');
    expect(rt.attempts.at(-1)!.httpSuccesses).toBeGreaterThanOrEqual(5);
    expect(rt.oldPodsOutstanding).toBe(true);
    expect(s.workload.nodeSelectorHostname).toBe('opsloop-control');
    expect(s.http.probes.at(-1)!.servedBy).toBe('opsloop-control');
  });

  it('Demo A with the laptop loaded recovers on Azure ARM64', () => {
    const s = run('node-failure-arm64');
    const rt = s.activeIncident!.status.runtime;
    expect(rt.phase).toBe('RECOVERED');
    expect(rt.decision!.selectedHostname).toBe('azure-arm64-worker');
    expect(rt.attempts.at(-1)!.targetArch).toBe('arm64');
    expect(s.http.probes.at(-1)!.arch).toBe('arm64');
  });

  it('Demo B exhausts the runtime budget, quarantines, and stops at a PR without claiming recovery', () => {
    const s = run('application-failure-pr');
    const inc = s.activeIncident!;
    expect(inc.status.runtime.classification).toBe('APPLICATION_FAILURE');
    expect(inc.status.runtime.budget).toEqual({ used: 2, max: 2 });
    expect(inc.status.runtime.attempts.every((a) => a.result === 'failed')).toBe(true);
    expect(inc.status.runtime.attempts.map((a) => a.type)).toEqual(['restart', 'relocation']);
    expect(inc.status.runtime.phase).toBe('ENGINEERING_HANDOFF');
    expect(inc.status.runtime.quarantine).toBe('QuarantineConfirmed');
    expect(s.workload.replicas).toBe(0);
    expect(inc.status.engineering!.phase).toBe('PR_CREATED');
    expect(inc.status.engineering!.pullRequest!.url).toBeNull();
    expect(inc.status.engineering!.stages.some((x) => x.outcome === 'fail')).toBe(true);
    expect(s.http.probes.at(-1)!.ok).toBe(false);
  });

  it('No-candidate waits and fails operationally without an Engineering handoff', () => {
    const s = run('no-candidate');
    const inc = s.activeIncident!;
    expect(inc.status.runtime.phase).toBe('FAILED');
    expect(inc.status.engineering).toBeUndefined();
    const reasons = inc.status.runtime.decision!.candidates.flatMap((c) => c.rejectReasons);
    expect(reasons).toEqual(expect.arrayContaining(['CPU_PRESSURE', 'METRICS_STALE', 'NODE_NOT_READY']));
    expect(inc.status.runtime.decision!.selectedHostname).toBeNull();
    expect(inc.status.runtime.timings.decidedAt).toBeUndefined();
  });

  it('every scenario runs without cue errors and reports §18 timings from fault injection', () => {
    for (const scenario of DEMO_SCENARIOS) {
      const sim = new DemoSimulation(scenario.script);
      sim.advanceTo(sim.durationMs);
      expect(sim.cueErrors, scenario.id).toBe(0);
      const t = sim.snapshot().activeIncident!.status.runtime.timings;
      expect(t.faultInjectedAt, scenario.id).toBeTypeOf('number');
      expect(t.faultInjectedAt!).toBeLessThan(t.triggeredAt);
    }
    const a1 = run('node-failure-laptop').activeIncident!.status.runtime.timings;
    expect(a1.detectedAt! - a1.faultInjectedAt!).toBe(60_000);
  });

  it('is deterministic, including after rewinding', () => {
    const scenario = DEMO_SCENARIOS[0]!;
    const a = new DemoSimulation(scenario.script);
    a.advanceTo(100_000);
    const b = new DemoSimulation(scenario.script);
    b.advanceTo(140_000);
    b.advanceTo(100_000);
    expect(JSON.stringify(b.snapshot())).toBe(JSON.stringify(a.snapshot()));
  });
});

describe('DemoDataSource playback', () => {
  function fakeClock() {
    let t = 0;
    let tick: (() => void) | null = null;
    return {
      now: () => t,
      setInterval: (fn: () => void) => {
        tick = fn;
        return 1;
      },
      clearInterval: () => {
        tick = null;
      },
      advance(ms: number) {
        for (let i = 0; i < ms / 100; i++) {
          t += 100;
          tick?.();
        }
      },
    };
  }

  it('returns stable snapshot references between changes', () => {
    const clock = fakeClock();
    const src = new DemoDataSource({ ...clock, autoPlay: false });
    src.start();
    expect(src.getSnapshot()).toBe(src.getSnapshot());
    expect(src.getStatus()).toBe(src.getStatus());
    expect(src.getPlayback()).toBe(src.getPlayback());
  });

  it('auto-loops to the next scenario and archives the finished incident', () => {
    const clock = fakeClock();
    const src = new DemoDataSource({ ...clock, speed: 8 });
    src.start();
    const first = src.getPlayback().scenarioId;
    for (let i = 0; i < 120 && src.getPlayback().scenarioId === first; i++) clock.advance(1000);
    expect(src.getPlayback().scenarioId).not.toBe(first);
    expect(src.getSnapshot().recentIncidents).toHaveLength(1);
    expect(src.getSnapshot().recentIncidents[0]!.status.runtime.phase).toBe('RECOVERED');
  });

  it('chapter navigation seeks forward and back', () => {
    const src = new DemoDataSource({ ...fakeClock(), autoPlay: false });
    src.nextChapter();
    src.nextChapter();
    const at = src.getPlayback().simElapsedMs;
    expect(at).toBeGreaterThan(0);
    src.previousChapter();
    expect(src.getPlayback().simElapsedMs).toBeLessThan(at);
  });
});
