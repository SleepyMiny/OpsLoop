import { describe, expect, it } from 'vitest';
import vectors from './__fixtures__/go-candidates.json';
import { baselineWorkload } from '../data/demo/baseline';
import { policyFromCrd, policySpecFromCrd, parseMemoryQuantity } from '../data/live/fromCrd';
import type { CrdDecision, CrdRecoveryPolicySpec } from '../data/live/crd';
import { evaluateCandidates } from './evaluate';
import type { ClusterNode } from './types';

interface Vector {
  name: string;
  input: {
    Now: string; Spec: CrdRecoveryPolicySpec;
    RecentFailures: Array<{ actionID: string; nodeUID: string; nodeName: string; at: string; reason: string; incident: string }> | null;
    Nodes: Array<{ metadata: { name: string; uid: string; labels: Record<string, string> }; status: { addresses: Array<{ address: string }>; allocatable: { cpu: string; memory: string } } }>;
    Metrics: Record<string, Record<string, Array<{ Value: number }>>>;
  };
  decision: CrdDecision;
}

// The same raw Go Input and its actual Evaluate output, exported by contracts/export_test.go.
// This adapter only translates units/shape. Expected scores and eligibility are never recomputed here.
describe('Go Controller candidate contract', () => {
  for (const vector of vectors as unknown as Vector[]) {
    it(vector.name, () => {
      const i = vector.input;
      const now = Date.parse(i.Now);
      const policy = policySpecFromCrd(i.Spec);
      const nodes: ClusterNode[] = i.Nodes.map(n => {
        const ip = n.status.addresses[0]!.address;
        const val = (key: string) => i.Metrics[key]?.[ip]?.[0]?.Value ?? null;
        const memAvail = val('MemAvail'); const memTotal = val('MemTotal');
        const memory = memAvail !== null && memTotal !== null ? 1 - memAvail / memTotal : null;
        const usage = {
          cpu: { value: val('CPUUtil'), observedAt: (val('CPUTime') ?? 0) * 1000 },
          memory: { value: memory, observedAt: (val('MemAvailTime') ?? 0) * 1000 },
          memAvailableBytes: { value: memAvail, observedAt: (val('MemAvailTime') ?? 0) * 1000 },
          memTotalBytes: memTotal, cpuSampleCount: val('CPUCount'), exporterUp: val('Up'),
        };
        const rawCPU = n.status.allocatable.cpu;
        return {
          uid: n.metadata.uid, name: n.metadata.name, hostnameLabel: n.metadata.labels['kubernetes.io/hostname'] ?? '',
          displayName: n.metadata.name, site: 'physical-server', arch: 'amd64', role: 'worker', readiness: 'Ready',
          pressure: [], unschedulable: false, recoveryCandidate: true, pods: [], usage,
          allocatable: { cpuMillicores: rawCPU.endsWith('m') ? Number(rawCPU.slice(0, -1)) : Number(rawCPU) * 1000, memoryBytes: parseMemoryQuantity(n.status.allocatable.memory, 'node.allocatable.memory') },
          requested: { cpuMillicores: 0, memoryBytes: 0 },
        };
      });
      const workload = { ...baselineWorkload(), uid: 'wl-uid', requests: { cpuMillicores: 100, memoryBytes: 128 * 1024 ** 2 } };
      const p = policyFromCrd({ kind: 'RecoveryPolicy', apiVersion: 'opsloop.io/v1alpha1', metadata: { name: 'p' }, spec: i.Spec, status: { recentFailures: i.RecentFailures ?? [] } }, { workloadUid: workload.uid });
      const actual = evaluateCandidates({ requireRawMetrics: true, nodes, policy, workload, now, cooldowns: p.status.cooldowns, recentFailures: p.status.recentFailures });
      expect(actual.selected?.nodeUid ?? null).toBe(vector.decision.selectedNodeUID ?? null);
      for (const expected of vector.decision.candidates ?? []) {
        const c = actual.candidates.find(c => c.nodeUid === expected.nodeUID)!;
        expect([...c.rejectReasons].sort(), expected.nodeName).toEqual([...(expected.rejectReasons ?? [])].sort());
        expect(c.score !== undefined, expected.nodeName).toBe(expected.eligible);
        expect(c.score?.total ?? null, expected.nodeName).toBe(expected.score ? expected.score.scoreMilli / 1000 : null);
        if (expected.score) {
          expect(c.score!.cpuHeadroom).toBe(expected.score.cpuHeadroomPermille / 1000);
          expect(c.score!.memoryHeadroom).toBe(expected.score.memoryHeadroomPermille / 1000);
          expect(c.score!.requestedHeadroom).toBe(expected.score.requestedHeadroomPermille / 1000);
          expect(c.score!.penalty).toBe(expected.score.penaltyMilli / 1000);
        }
      }
    });
  }
});
