import type {
  Arch,
  ClusterNode,
  NodeRole,
  NodeSite,
  PodRole,
  PodSummary,
  RecoveryPolicy,
  WorkloadStatus,
} from '../../domain/types';

/**
 * Demo Mode baseline modeled on the documented testbed (README "목표 Testbed",
 * docs/experiments/p1-b-local-platform.md). Capacities match the recorded
 * allocatable values; the Azure ARM64 worker is planned and not built yet, so its
 * node name is a placeholder. Loads are simulated, never measured.
 */

const GiB = 1024 ** 3;
const MiB = 1024 ** 2;
const Ki = 1024;

export const DEMO_NAMESPACE = 'opsloop-demo';
export const DEMO_WORKLOAD = 'demo-api';
export const DEMO_EPOCH = Date.UTC(2026, 9, 13, 5, 0, 0); // fixed so replays are identical

export interface SimNodeSeed {
  name: string;
  displayName: string;
  site: NodeSite;
  arch: Arch;
  role: NodeRole;
  cpuMillicores: number;
  memoryBytes: number;
  memTotalBytes: number;
  baseRequested: { cpuMillicores: number; memoryBytes: number };
  baseLoad: { cpu: number; memory: number };
  pods: Array<{ name: string; role: PodRole }>;
}

export const NODE_SEEDS: SimNodeSeed[] = [
  {
    name: 'opsloop-control',
    displayName: '노트북 VM',
    site: 'laptop-vm',
    arch: 'amd64',
    role: 'control-plane',
    cpuMillicores: 4000,
    memoryBytes: 8127240 * Ki,
    memTotalBytes: 8 * GiB,
    baseRequested: { cpuMillicores: 1450, memoryBytes: 2900 * MiB },
    baseLoad: { cpu: 0.31, memory: 0.52 },
    pods: [
      { name: 'coredns', role: 'system' },
      { name: 'metrics-server', role: 'system' },
      { name: 'local-path-provisioner', role: 'system' },
      { name: 'prometheus', role: 'observability' },
      { name: 'grafana', role: 'observability' },
      { name: 'kube-state-metrics', role: 'observability' },
      { name: 'node-exporter', role: 'observability' },
      { name: 'opsloop-controller', role: 'opsloop' },
    ],
  },
  {
    name: 'sarmubuntuserver',
    displayName: '물리 서버',
    site: 'physical-server',
    arch: 'amd64',
    role: 'worker',
    cpuMillicores: 4000,
    memoryBytes: 8072460 * Ki,
    memTotalBytes: 8 * GiB,
    baseRequested: { cpuMillicores: 150, memoryBytes: 180 * MiB },
    baseLoad: { cpu: 0.18, memory: 0.34 },
    pods: [{ name: 'node-exporter', role: 'observability' }],
  },
  {
    name: 'azure-arm64-worker',
    displayName: 'Azure ARM64 VM',
    site: 'azure-vm',
    arch: 'arm64',
    role: 'worker',
    cpuMillicores: 1930,
    memoryBytes: 3400 * MiB,
    memTotalBytes: 4 * GiB,
    baseRequested: { cpuMillicores: 150, memoryBytes: 160 * MiB },
    baseLoad: { cpu: 0.12, memory: 0.3 },
    pods: [{ name: 'node-exporter', role: 'observability' }],
  },
];

export function seedNode(seed: SimNodeSeed, uid: string, podUid: (name: string) => string): ClusterNode {
  const pods: PodSummary[] = seed.pods.map((p) => ({
    uid: podUid(`${seed.name}/${p.name}`),
    name: p.role === 'observability' && p.name === 'node-exporter' ? `node-exporter-${seed.name.slice(0, 5)}` : p.name,
    namespace: p.role === 'system' ? 'kube-system' : p.role === 'opsloop' ? 'opsloop-system' : 'monitoring',
    role: p.role,
    phase: 'Running',
    ready: true,
    restarts: 0,
  }));
  return {
    uid,
    name: seed.name,
    hostnameLabel: seed.name,
    displayName: seed.displayName,
    site: seed.site,
    arch: seed.arch,
    role: seed.role,
    readiness: 'Ready',
    unschedulable: false,
    pressure: [],
    recoveryCandidate: true,
    allocatable: { cpuMillicores: seed.cpuMillicores, memoryBytes: seed.memoryBytes },
    requested: { ...seed.baseRequested },
    usage: {
      cpu: { value: seed.baseLoad.cpu, observedAt: DEMO_EPOCH },
      memory: { value: seed.baseLoad.memory, observedAt: DEMO_EPOCH },
      memAvailableBytes: { value: (1 - seed.baseLoad.memory) * seed.memTotalBytes, observedAt: DEMO_EPOCH },
    },
    usageHistory: [],
    pods,
  };
}

export function baselineWorkload(): WorkloadStatus {
  return {
    uid: 'b7e4c2d1-demo-api',
    namespace: DEMO_NAMESPACE,
    name: DEMO_WORKLOAD,
    replicas: 1,
    nodeSelectorHostname: 'sarmubuntuserver',
    imageIndexDigest: 'sha256:5f1c9e0b7a42d8e3c6b1a9f07d2e4c8b3a6f5d1e9c0b7a4d2e8f3c6b1a9d0e7f',
    platforms: [
      { platform: 'linux/amd64', digest: 'sha256:2a7d4c9e1b8f3a6d0c5e9b2f7a4d1c8e' },
      { platform: 'linux/arm64', digest: 'sha256:9c3e6a1d8b5f2c7e4a0d9b6f3c8e1a5d' },
    ],
    requests: { cpuMillicores: 100, memoryBytes: 128 * MiB },
  };
}

/** Defaults from architecture §8; the hash is a demo placeholder. */
export function baselinePolicy(): RecoveryPolicy {
  return {
    name: 'demo-api-recovery',
    namespace: DEMO_NAMESPACE,
    generation: 3,
    policyHash: 'sha256:policy-demo-3f9a',
    spec: {
      targetRef: { kind: 'Deployment', name: DEMO_WORKLOAD },
      enabled: true,
      mode: 'Enforce',
      candidateSelector: 'opsloop.io/recovery-candidate=true',
      requiredPlatforms: ['linux/amd64', 'linux/arm64'],
      detection: { nodeUnreadyForSeconds: 20, restartThreshold: 3, restartWindowSeconds: 120 },
      filter: {
        maxCPUUtilization: 0.85,
        maxMemoryUtilization: 0.85,
        minimumAvailableMemoryBytes: 512 * MiB,
        startupMemoryBudgetBytes: 256 * MiB,
        requestedCapacityReserve: 0.1,
        maxSampleAgeSeconds: 30,
      },
      // Cap and window are demo choices: §8 requires them to be explicit but sets no values.
      score: {
        cpuWeight: 0.3,
        memoryWeight: 0.35,
        requestsWeight: 0.35,
        recentFailurePenalty: 15,
        recentFailurePenaltyCap: 45,
        failureHistoryWindowSeconds: 1800,
      },
      budget: {
        maxActionsPerIncident: 2,
        cooldownSeconds: 120,
        placementTimeoutSeconds: 90,
        verificationTimeoutSeconds: 60,
        incidentDeadlineSeconds: 600,
      },
      verification: { consecutiveSuccesses: 5, stabilityWindowSeconds: 30 },
      onApplicationExhausted: 'QuarantineScaleZero',
      engineering: { enabled: true, profileRef: 'demo-api-verification' },
      sameSignatureCircuit: true,
    },
    status: { observedGeneration: 3, activeIncidentRef: null, incidentSequence: 6, cooldowns: [] },
  };
}

export const MEM_TOTAL_BY_NODE: Record<string, number> = Object.fromEntries(
  NODE_SEEDS.map((s) => [s.name, s.memTotalBytes]),
);
export const BASE_REQUESTED_BY_NODE: Record<string, { cpuMillicores: number; memoryBytes: number }> =
  Object.fromEntries(NODE_SEEDS.map((s) => [s.name, s.baseRequested]));
