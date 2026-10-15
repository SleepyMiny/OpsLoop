import { describe, expect, it } from 'vitest';
import { DemoSimulation } from '../../data/demo/engine';
import { DEMO_SCENARIOS } from '../../data/demo/scenarios';
import type { ClusterNode, DashboardSnapshot, HttpAvailability, PodSummary } from '../../domain/types';
import {
  buildClusterView,
  demoPodView,
  deriveRoute,
  httpView,
  metricNotice,
  orderNodes,
  quarantineView,
  routeSentence,
  sampleStatus,
  servingArch,
  withRo,
} from './clusterModel';

function simAt(id: string, seconds: number): DashboardSnapshot {
  const scenario = DEMO_SCENARIOS.find((s) => s.id === id);
  if (!scenario) throw new Error(`unknown scenario ${id}`);
  const sim = new DemoSimulation(scenario.script);
  sim.advanceTo(seconds * 1000);
  return sim.snapshot();
}

function pod(partial: Partial<PodSummary>): PodSummary {
  return {
    uid: 'p1',
    name: 'demo-api-x',
    namespace: 'opsloop-demo',
    role: 'demo-api',
    phase: 'Running',
    ready: true,
    restarts: 0,
    ...partial,
  };
}

describe('station order', () => {
  it('lays out by site and role, never by node name', () => {
    const base = simAt('node-failure-laptop', 0).cluster.nodes;
    const shuffled = [base[2]!, base[0]!, base[1]!];
    const renamed = shuffled.map((n, i) => ({ ...n, name: `zz-${i}` }));
    expect(orderNodes(renamed).map((n) => n.site)).toEqual(['laptop-vm', 'physical-server', 'azure-vm']);
  });
});

describe('metric freshness (architecture §11)', () => {
  const now = 100_000;
  it('treats null and NaN as missing, never as 0%', () => {
    expect(sampleStatus({ value: null, observedAt: now }, now, 30)).toEqual({ kind: 'missing' });
    expect(sampleStatus({ value: Number.NaN, observedAt: now }, now, 30)).toEqual({ kind: 'missing' });
  });

  it('marks samples older than maxSampleAgeSeconds as stale but keeps the value', () => {
    expect(sampleStatus({ value: 0.4, observedAt: now - 30_000 }, now, 30).kind).toBe('fresh');
    expect(sampleStatus({ value: 0.4, observedAt: now - 42_000 }, now, 30)).toEqual({ kind: 'stale', value: 0.4, ageSec: 42 });
  });

  it('writes one caption per node, missing before stale, oldest age wins', () => {
    const stale = (ageSec: number) => ({ kind: 'stale' as const, value: 0.3, ageSec });
    expect(metricNotice([stale(31.4), stale(42.2)])?.text).toBe('관측값 42초 전 · 판단에 쓰지 않음');
    expect(metricNotice([stale(40), { kind: 'missing' }])?.text).toBe('관측값 없음');
    expect(metricNotice([{ kind: 'fresh', value: 0.2, ageSec: 3 }, { kind: 'fresh', value: 0.2, ageSec: 3 }])).toBeNull();
  });

  it('shows the powered-off primary as stale in the node-failure scenario', () => {
    const view = buildClusterView(simAt('node-failure-laptop', 60));
    const primary = view.stations.find((s) => s.node.site === 'physical-server')!;
    expect(primary.notice?.kind).toBe('stale');
    expect(primary.gauges[0].level).toBe('stale');
    expect(primary.readiness.label).toBe('응답 없음');
    // History after power-off has gaps, not zeros.
    expect(primary.gauges[0].history.at(-1)).toBeNull();
  });

  it('flags utilization above the policy threshold', () => {
    const view = buildClusterView(simAt('node-failure-laptop', 40));
    const azure = view.stations.find((s) => s.node.arch === 'arm64')!;
    expect(azure.gauges[0].level).toBe('over');
    expect(azure.gauges[0].threshold).toBe(0.85);
  });
});

describe('recovery route', () => {
  it('is absent before a target is selected and while waiting', () => {
    expect(deriveRoute(simAt('node-failure-laptop', 85))).toBeNull();
    expect(deriveRoute(simAt('no-candidate', 120))).toBeNull();
  });

  it('runs from the failed node to the selected node and reads verified once RECOVERED', () => {
    const selected = deriveRoute(simAt('node-failure-laptop', 93));
    expect(selected).toMatchObject({
      fromName: 'sarmubuntuserver',
      toName: 'opsloop-control',
      actionType: 'relocation',
      stage: 'selected',
    });
    const verifying = deriveRoute(simAt('node-failure-laptop', 110));
    expect(verifying?.stage).toBe('verifying');
    expect(verifying?.httpSuccesses).toBeGreaterThan(0);
    const done = deriveRoute(simAt('node-failure-laptop', 150));
    expect(done).toMatchObject({ stage: 'verified', fromName: 'sarmubuntuserver', toName: 'opsloop-control' });
  });

  it('reaches the ARM64 worker in the ARM scenario', () => {
    const route = deriveRoute(simAt('node-failure-arm64', 100));
    expect(route).toMatchObject({ fromName: 'sarmubuntuserver', toName: 'azure-arm64-worker', stage: 'executing' });
  });

  it('starts a retry from the previous attempt node, and a restart loops on one node', () => {
    const first = simAt('application-failure-pr', 60);
    const r1 = deriveRoute(first)!;
    const attempt1 = first.activeIncident!.status.runtime.attempts[0]!;
    if (r1.actionType === 'restart') expect(r1.fromName).toBe(r1.toName);
    else expect(r1.fromName).toBe('sarmubuntuserver');

    const second = simAt('application-failure-pr', 80);
    const r2 = deriveRoute(second)!;
    expect(second.activeIncident!.status.runtime.attempts).toHaveLength(2);
    expect(r2.fromName).toBe(attempt1.targetNode);
    expect(r2.actionType).toBe('relocation');
    expect(r2.toName).not.toBe(attempt1.targetNode);
  });

  it('keeps the origin after the crashed Pod is deleted, from decision.sourceNodeName', () => {
    const later = simAt('application-failure-pr', 62);
    expect(later.cluster.nodes.flatMap((n) => n.pods).some((p) => p.name === 'demo-api-7c9d4-x2kq8')).toBe(false);
    expect(deriveRoute(later)!.fromName).toBe('sarmubuntuserver');
  });

  it('falls back to the trigger and attempts when a source leaves sourceNodeName null', () => {
    const nodeFailure = structuredClone(simAt('node-failure-laptop', 96));
    nodeFailure.activeIncident!.status.runtime.decision!.sourceNodeName = null;
    expect(deriveRoute(nodeFailure)!.fromName).toBe('sarmubuntuserver');

    const retry = structuredClone(simAt('application-failure-pr', 80));
    retry.activeIncident!.status.runtime.decision!.sourceNodeName = null;
    const firstTarget = retry.activeIncident!.status.runtime.attempts[0]!.targetNode;
    expect(deriveRoute(retry)!.fromName).toBe(firstTarget);
  });

  it('resolves a hostname label that differs from the node name (§10)', () => {
    const snap = structuredClone(simAt('node-failure-laptop', 93));
    const laptop = snap.cluster.nodes.find((n) => n.name === 'opsloop-control')!;
    laptop.hostnameLabel = 'opsloop-control.tail';
    snap.activeIncident!.status.runtime.decision!.selectedHostname = 'opsloop-control.tail';
    expect(deriveRoute(snap)?.toName).toBe('opsloop-control');
  });

  it('says where the workload goes in plain Korean', () => {
    const snap = simAt('node-failure-laptop', 93);
    expect(routeSentence(deriveRoute(snap)!, snap.cluster.nodes)).toBe('물리 서버에서 노트북 VM으로 이동');
    const arm = simAt('node-failure-arm64', 100);
    expect(routeSentence(deriveRoute(arm)!, arm.cluster.nodes)).toBe('물리 서버에서 Azure ARM64 VM으로 이동');
  });
});

describe('Korean particle', () => {
  it('chooses 로 or 으로 from the final sound', () => {
    expect(withRo('물리 서버')).toBe('물리 서버로');
    expect(withRo('서울')).toBe('서울로');
    expect(withRo('부산')).toBe('부산으로');
    expect(withRo('노트북 VM')).toBe('노트북 VM으로');
    expect(withRo('worker-1')).toBe('worker-1로');
    expect(withRo('node-3')).toBe('node-3으로');
  });
});

describe('demo-api Pod token', () => {
  it('ghosts a NodeLost Pod and names oldPodsOutstanding while the incident tracks it', () => {
    expect(demoPodView(pod({ reason: 'NodeLost', ready: false }), true)).toMatchObject({
      presence: 'ghost',
      label: '이전 Pod 추적 중',
      code: 'oldPodsOutstanding',
    });
    expect(demoPodView(pod({ reason: 'NodeLost', ready: false }), false).code).toBe('NodeLost');
  });

  it('maps the remaining lifecycle states to lamps', () => {
    expect(demoPodView(pod({}), false).tone).toBe('ok');
    expect(demoPodView(pod({ phase: 'Pending', ready: false, reason: 'ContainerCreating' }), false).tone).toBe('caution');
    const crash = demoPodView(pod({ ready: false, reason: 'CrashLoopBackOff', restarts: 3 }), false);
    expect(crash.tone).toBe('fault');
    expect(crash.label).toContain('3회');
    expect(demoPodView(pod({ ready: false, reason: 'Terminating' }), false).presence).toBe('fading');
    expect(demoPodView(pod({ attemptId: 'inc-a1' }), false).attemptId).toBe('inc-a1');
  });

  it('keeps the old Pod ghosted on the dead node after recovery', () => {
    const view = buildClusterView(simAt('node-failure-laptop', 150));
    const primary = view.stations.find((s) => s.node.site === 'physical-server')!;
    expect(primary.demoPods.map((p) => p.code)).toEqual(['oldPodsOutstanding']);
    const laptop = view.stations.find((s) => s.node.site === 'laptop-vm')!;
    expect(laptop.demoPods[0]).toMatchObject({ tone: 'ok' });
    expect(laptop.demoPods[0]?.attemptId).toMatch(/-a1$/);
  });
});

describe('HTTP availability', () => {
  const probes = (pattern: string, start = 0): HttpAvailability => ({
    endpoint: 'http://x/',
    probes: [...pattern].map((c, i) => ({
      at: start + i * 2000,
      ok: c === '1',
      ...(c === '1' ? { arch: 'amd64' as const, latencyMs: 9 } : {}),
    })),
  });

  it('computes availability, the window, and how long the service has been down', () => {
    const view = httpView(probes('1111000'), 14_000, 45);
    expect(view.availability).toBeCloseTo(4 / 7);
    expect(view.windowSec).toBe(14);
    expect(view.downForSec).toBe(6);
    expect(view.downWholeWindow).toBe(false);
    expect(view.lastLatencyMs).toBeNull();
  });

  it('reports latency while answering and a whole-window outage', () => {
    expect(httpView(probes('0011'), 8000, 45).lastLatencyMs).toBe(9);
    const down = httpView(probes('000'), 6000, 45);
    expect(down.downWholeWindow).toBe(true);
    expect(down.downForSec).toBe(6);
  });

  it('keeps only the last N probes', () => {
    expect(httpView(probes('1'.repeat(60)), 120_000, 45).total).toBe(45);
  });
});

describe('workload strip inputs', () => {
  it('highlights the architecture that serves now, and none during an outage', () => {
    expect(servingArch(simAt('node-failure-arm64', 160))).toBe('arm64');
    expect(servingArch(simAt('node-failure-laptop', 150))).toBe('amd64');
    expect(servingArch(simAt('node-failure-laptop', 70))).toBeNull();
  });

  it('trusts HTTP over Kubernetes: nothing is serving while requests fail, even if the node is still Ready', () => {
    const snap = simAt('node-failure-laptop', 30);
    expect(snap.cluster.nodes.find((n) => n.name === 'sarmubuntuserver')!.readiness).toBe('Ready');
    expect(snap.http.probes.at(-1)!.ok).toBe(false);
    expect(servingArch(snap)).toBeNull();
  });

  it('marks quarantine prominently with replicas=0', () => {
    const snap = simAt('application-failure-pr', 140);
    expect(quarantineView(snap)).toMatchObject({ state: 'QuarantineConfirmed', label: '격리됨 · replicas=0' });
    expect(quarantineView(simAt('node-failure-laptop', 150))).toBeNull();
  });
});

describe('screen-reader summary', () => {
  it('names every node, the route and the HTTP state', () => {
    const view = buildClusterView(simAt('node-failure-laptop', 110));
    for (const node of simAt('node-failure-laptop', 110).cluster.nodes as ClusterNode[]) {
      expect(view.summary).toContain(node.displayName);
    }
    expect(view.summary).toContain('복구 경로: 물리 서버에서 노트북 VM으로 이동');
    expect(view.summary).toMatch(/HTTP 최근 \d+초 성공률/);
  });
});
