import { describe, expect, it } from 'vitest';
import { baselinePolicy, baselineWorkload, NODE_SEEDS, seedNode } from '../data/demo/baseline';
import { compareNodeNames, evaluateCandidates, scoreCandidate } from './evaluate';
import type { ClusterNode } from './types';

const NOW = 1_000_000;
const policy = baselinePolicy().spec;
const workload = baselineWorkload();

function nodes(): ClusterNode[] {
  return NODE_SEEDS.map((seed, i) => {
    const node = seedNode(seed, `uid-${i}`, (n) => n);
    node.usage.cpu.observedAt = NOW;
    node.usage.memory.observedAt = NOW;
    node.usage.memAvailableBytes.observedAt = NOW;
    return node;
  });
}

function byName(list: ClusterNode[], name: string): ClusterNode {
  const node = list.find((n) => n.name === name);
  if (!node) throw new Error(name);
  return node;
}

describe('scoreCandidate (architecture §8 formula)', () => {
  it('weights headrooms 0.30 / 0.35 / 0.35 on a 0..100 scale', () => {
    const s = scoreCandidate({ cpuUtilization: 0.2, memoryUtilization: 0.4, requestedHeadroom: 0.5, failures: 0, policy });
    expect(s.weighted).toEqual({ cpu: 24, memory: 21, requests: 17.5 });
    expect(s.total).toBe(62.5);
  });

  it('subtracts 15 points per recent failure', () => {
    const s = scoreCandidate({ cpuUtilization: 0.2, memoryUtilization: 0.4, requestedHeadroom: 0.5, failures: 2, policy });
    expect(s.penalty).toBe(30);
    expect(s.total).toBe(32.5);
  });
});

describe('evaluateCandidates hard filter', () => {
  it('excludes the failed node and rejects a CPU-loaded candidate', () => {
    const list = nodes();
    byName(list, 'sarmubuntuserver').readiness = 'Unknown';
    byName(list, 'azure-arm64-worker').usage.cpu.value = 0.93;
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW, failedNodeName: 'sarmubuntuserver' });

    const failed = r.candidates.find((c) => c.nodeName === 'sarmubuntuserver')!;
    expect(failed.exclusion).toBe('FAILED_NODE');
    expect(failed.rejectReasons).toContain('NODE_NOT_READY');
    expect(failed.score).toBeUndefined();

    const azure = r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!;
    expect(azure.rejectReasons).toEqual(['CPU_PRESSURE']);
    expect(r.selected?.nodeName).toBe('opsloop-control');
    expect(r.actionType).toBe('relocation');
  });

  it('never treats missing or stale metrics as 0% utilization', () => {
    const list = nodes();
    byName(list, 'azure-arm64-worker').usage.cpu = { value: null, observedAt: NOW };
    byName(list, 'opsloop-control').usage.memory.observedAt = NOW - 31_000;
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).toContain('METRICS_MISSING');
    expect(r.candidates.find((c) => c.nodeName === 'opsloop-control')!.rejectReasons).toContain('METRICS_STALE');
    expect(r.selected?.nodeName).toBe('sarmubuntuserver');
  });

  it('records every violated rule for one candidate', () => {
    const list = nodes();
    const azure = byName(list, 'azure-arm64-worker');
    azure.unschedulable = true;
    azure.usage.memory.value = 0.95;
    azure.usage.memAvailableBytes.value = 200 * 1024 ** 2;
    azure.pressure = ['MemoryPressure'];
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    const reasons = r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons;
    expect(reasons).toEqual(
      expect.arrayContaining(['UNSCHEDULABLE', 'MEMORY_PRESSURE', 'INSUFFICIENT_AVAILABLE_MEMORY']),
    );
    expect(reasons.filter((x) => x === 'MEMORY_PRESSURE')).toHaveLength(1);
  });

  it('rejects ARM64 when the image index has no linux/arm64 manifest', () => {
    const amdOnly = { ...workload, platforms: workload.platforms.filter((p) => p.platform === 'linux/amd64') };
    const r = evaluateCandidates({ nodes: nodes(), workload: amdOnly, policy, now: NOW });
    expect(r.candidates.find((c) => c.arch === 'arm64')!.rejectReasons).toContain('ARCH_MISMATCH');
  });

  it('rejects when requests plus the 10% reserve exceed allocatable', () => {
    const list = nodes();
    byName(list, 'azure-arm64-worker').requested.cpuMillicores = 1700;
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).toContain(
      'INSUFFICIENT_REQUESTED_CPU',
    );
  });

  it('applies cooldown and returns no selection when nothing is eligible', () => {
    const list = nodes();
    const cooldowns = list.map((n) => ({ workloadUid: workload.uid, nodeName: n.name, until: NOW + 1 }));
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW, cooldowns });
    expect(r.candidates.every((c) => c.rejectReasons.includes('COOLDOWN'))).toBe(true);
    expect(r.selected).toBeNull();
    expect(r.actionType).toBeNull();
  });

  it('labels selecting the current node as restart', () => {
    const list = nodes();
    for (const n of list) {
      n.usage.cpu.value = 0.2;
      n.usage.memory.value = 0.3;
      n.allocatable = { cpuMillicores: 4000, memoryBytes: 8 * 1024 ** 3 };
      n.requested = { cpuMillicores: 0, memoryBytes: 0 };
    }
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW, currentNodeName: 'azure-arm64-worker' });
    expect(r.selected?.nodeName).toBe('azure-arm64-worker');
    expect(r.actionType).toBe('restart');
  });
});

function equalize(list: ClusterNode[]): ClusterNode[] {
  for (const n of list) {
    n.usage.cpu.value = 0.2;
    n.usage.memory.value = 0.3;
    n.usage.memAvailableBytes.value = 4 * 1024 ** 3;
    n.allocatable = { cpuMillicores: 4000, memoryBytes: 8 * 1024 ** 3 };
    n.requested = { cpuMillicores: 0, memoryBytes: 0 };
  }
  return list;
}

describe('evaluateCandidates tie-break and penalty', () => {
  it('breaks exact ties by byte order of node names, independent of the current node', () => {
    const r = evaluateCandidates({ nodes: equalize(nodes()), workload, policy, now: NOW });
    expect(r.selected?.nodeName).toBe('azure-arm64-worker');
    expect(r.actionType).toBe('relocation');
    expect(compareNodeNames('Zeta', 'alpha')).toBeLessThan(0); // uppercase sorts first in byte order
    expect(compareNodeNames('node_a', 'node-a')).toBeGreaterThan(0);
  });

  it('caps the penalty and ignores failures outside the history window', () => {
    const list = equalize(nodes());
    const at = (secondsAgo: number) => NOW - secondsAgo * 1000;
    const failures = [
      ...[10, 20, 30, 40].map((s) => ({ nodeName: 'azure-arm64-worker', at: at(s) })),
      { nodeName: 'opsloop-control', at: at(policy.score.failureHistoryWindowSeconds + 1) },
    ];
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW, recentFailures: failures });
    const azure = r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!;
    const laptop = r.candidates.find((c) => c.nodeName === 'opsloop-control')!;
    expect(azure.score!.penalty).toBe(policy.score.recentFailurePenaltyCap);
    expect(laptop.score!.penalty).toBe(0);
  });

  it('applies cooldown only to the same workload', () => {
    const cooldowns = [{ workloadUid: 'other-workload', nodeName: 'opsloop-control', until: NOW + 60_000 }];
    const r = evaluateCandidates({ nodes: nodes(), workload, policy, now: NOW, cooldowns });
    expect(r.candidates.find((c) => c.nodeName === 'opsloop-control')!.rejectReasons).not.toContain('COOLDOWN');
  });
});

describe('evaluateCandidates rejects what it cannot evaluate (§8, §11)', () => {
  it('treats a non-numeric sample timestamp as missing, not fresh', () => {
    const list = nodes();
    (byName(list, 'azure-arm64-worker').usage.cpu as { observedAt: unknown }).observedAt = '2026-10-13T05:00:00Z';
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).toContain('METRICS_MISSING');
  });

  it('treats a sample from the future beyond clock skew as stale', () => {
    const list = nodes();
    byName(list, 'azure-arm64-worker').usage.memory.observedAt = NOW + 3_600_000;
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).toContain('METRICS_STALE');
  });

  it('treats NaN utilization as missing', () => {
    const list = nodes();
    byName(list, 'opsloop-control').usage.cpu.value = Number.NaN;
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    const laptop = r.candidates.find((c) => c.nodeName === 'opsloop-control')!;
    expect(laptop.rejectReasons).toContain('METRICS_MISSING');
    expect(laptop.score).toBeUndefined();
  });

  it('rejects unparsed quantity strings and zero allocatable instead of scoring NaN', () => {
    const list = nodes();
    (byName(list, 'azure-arm64-worker').allocatable as { cpuMillicores: unknown }).cpuMillicores = '1930m';
    byName(list, 'opsloop-control').allocatable = { cpuMillicores: 0, memoryBytes: 0 };
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).toContain(
      'INSUFFICIENT_REQUESTED_CPU',
    );
    expect(r.candidates.find((c) => c.nodeName === 'opsloop-control')!.rejectReasons).toEqual(
      expect.arrayContaining(['INSUFFICIENT_REQUESTED_CPU', 'INSUFFICIENT_REQUESTED_MEMORY']),
    );
    expect(r.candidates.every((c) => c.score === undefined || Number.isFinite(c.score.total))).toBe(true);
    expect(r.selected?.nodeName).toBe('sarmubuntuserver');
  });

  it('rejects NoSchedule/NoExecute taints the workload does not tolerate', () => {
    const list = nodes();
    byName(list, 'opsloop-control').taints = [{ key: 'node-role.kubernetes.io/control-plane', effect: 'NoSchedule' }];
    byName(list, 'azure-arm64-worker').taints = [{ key: 'cost', value: 'high', effect: 'PreferNoSchedule' }];
    const r = evaluateCandidates({ nodes: list, workload, policy, now: NOW });
    expect(r.candidates.find((c) => c.nodeName === 'opsloop-control')!.rejectReasons).toContain('UNTOLERATED_TAINT');
    expect(r.candidates.find((c) => c.nodeName === 'azure-arm64-worker')!.rejectReasons).not.toContain(
      'UNTOLERATED_TAINT',
    );

    const tolerant = { ...workload, tolerations: [{ key: 'node-role.kubernetes.io/control-plane', operator: 'Exists' as const }] };
    const r2 = evaluateCandidates({ nodes: list, workload: tolerant, policy, now: NOW });
    expect(r2.candidates.find((c) => c.nodeName === 'opsloop-control')!.rejectReasons).not.toContain('UNTOLERATED_TAINT');
  });

  it('rejects every candidate when the workload has a constraint OpsLoop does not model', () => {
    const constrained = { ...workload, unsupportedConstraints: ['podAntiAffinity'] };
    const r = evaluateCandidates({ nodes: nodes(), workload: constrained, policy, now: NOW });
    expect(r.candidates.every((c) => c.rejectReasons.includes('UNSUPPORTED_CONSTRAINT'))).toBe(true);
    expect(r.selected).toBeNull();
  });
});
