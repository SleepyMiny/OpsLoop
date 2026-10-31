import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { buildBoardModel, evaluateNow } from '../../components/candidates/model';
import { CandidateBoard } from '../../components/candidates/CandidateBoard';
import { ClusterMimic } from '../../components/cluster/ClusterMimic';
import { EngineeringTrack } from '../../components/engineering/EngineeringTrack';
import { IncidentPanel } from '../../components/incident/IncidentPanel';
import { PipelineLadder } from '../../components/incident/PipelineLadder';
import { PolicyPanel } from '../../components/policy/PolicyPanel';
import { DemoSimulation } from '../demo/engine';
import { DEMO_SCENARIOS } from '../demo/scenarios';
import type { CrdRecoveryIncident, CrdRecoveryPolicy, CrdRecoveryPolicySpec } from './crd';
import { incidentFromCrd, parseDuration, parseMemoryQuantity, policyFromCrd } from './fromCrd';
import { parseSnapshot } from './httpSource';

/**
 * Fixtures follow the Go json tags of api/v1alpha1 (backend commit f47c7d1):
 * percent/permille/milli integers, RFC 3339 times, Go durations, Quantity strings.
 */
const policySpec = (): CrdRecoveryPolicySpec => ({
  targetRef: { name: 'demo-api' },
  enabled: true,
  mode: 'Enforce',
  candidateSelector: { matchLabels: { 'opsloop.io/recovery-candidate': 'true' } },
  requiredPlatforms: ['linux/amd64', 'linux/arm64'],
  detection: { nodeUnreadyFor: '20s', restartThreshold: 3, restartWindow: '120s' },
  filter: {
    maxCPUUtilizationPercent: 85,
    maxMemoryUtilizationPercent: 85,
    minimumAvailableMemory: '512Mi',
    startupMemoryBudget: '256Mi',
    requestedCapacityReservePercent: 10,
    maxSampleAge: '30s',
  },
  score: {
    cpuWeight: 30,
    memoryWeight: 35,
    requestsWeight: 35,
    recentFailurePenalty: 15,
    maxRecentFailurePenalty: 45,
    recentFailureWindow: '30m',
  },
  budget: {
    maxActionsPerIncident: 2,
    cooldown: '120s',
    placementTimeout: '90s',
    verificationTimeout: '60s',
    incidentDeadline: '10m',
  },
  verification: { serviceName: 'demo-api', port: 8080, consecutiveSuccesses: 5, stabilityWindow: '30s' },
});

const crdPolicy = (): CrdRecoveryPolicy => ({
  apiVersion: 'opsloop.io/v1alpha1',
  kind: 'RecoveryPolicy',
  metadata: { name: 'demo-api-recovery', namespace: 'opsloop-demo', generation: 3, uid: 'pol-1' },
  spec: policySpec(),
  status: {
    observedGeneration: 3,
    incidentSequence: 7,
    recentFailures: [
      {
        actionID: 'demo-api-recovery-0006-a1',
        nodeName: 'azure-arm64-worker',
        nodeUID: 'n-3',
        at: '2026-10-13T04:59:00Z',
        reason: 'HTTP_VERIFICATION_FAILED',
        incident: 'demo-api-recovery-0006',
      },
    ],
  },
});

const measurements = (cpuPermille: number | undefined, memAvailGi: number) => ({
  ...(cpuPermille !== undefined ? { cpuUtilizationPermille: cpuPermille, cpuSampleTime: '2026-10-13T05:01:20Z' } : {}),
  memAvailableBytes: memAvailGi * 1024 ** 3,
  memTotalBytes: 8 * 1024 ** 3,
  memSampleTime: '2026-10-13T05:01:20Z',
  allocatableCPUMilli: 4000,
  allocatableMemoryBytes: 8 * 1024 ** 3,
  requestedCPUMilli: 1450,
  requestedMemoryBytes: 3 * 1024 ** 3,
  newPodCPUMilli: 100,
  newPodMemoryBytes: 128 * 1024 ** 2,
  reserveCPUMilli: 400,
  reserveMemoryBytes: 0.8 * 1024 ** 3,
});

const decision = () => ({
  incidentID: 'demo-api-recovery-0007',
  actionID: 'demo-api-recovery-0007-a1',
  policyGeneration: 3,
  policyHash: 'sha256:abc123def4567890',
  workloadUID: 'wl-1',
  baselineTemplateHash: 'tmpl-1',
  imageIndexDigest: 'sha256:index',
  queryVersion: 'q1',
  snapshotObservedAt: '2026-10-13T05:01:25Z',
  snapshotExpiresAt: '2026-10-13T05:01:55Z',
  candidates: [
    {
      nodeName: 'opsloop-control',
      nodeUID: 'n-1',
      hostname: 'opsloop-control',
      platform: 'linux/amd64',
      eligible: true,
      measurements: measurements(310, 3.8),
      score: {
        cpuHeadroomPermille: 690,
        memoryHeadroomPermille: 475,
        requestedHeadroomPermille: 512,
        recentFailures: 0,
        penaltyMilli: 0,
        scoreMilli: 54_245,
      },
    },
    {
      nodeName: 'sarmubuntuserver',
      nodeUID: 'n-2',
      hostname: 'sarmubuntuserver',
      platform: 'linux/amd64',
      eligible: false,
      rejectReasons: ['NODE_NOT_READY', 'METRICS_STALE'],
      measurements: measurements(undefined, 5),
    },
    {
      nodeName: 'azure-arm64-worker',
      nodeUID: 'n-3',
      hostname: 'azure-arm64-worker',
      platform: 'linux/arm64',
      eligible: false,
      rejectReasons: ['CPU_PRESSURE'],
      measurements: measurements(930, 1.2),
    },
  ],
  selectedNodeUID: 'n-1',
  selectedNodeName: 'opsloop-control',
  selectedHostname: 'opsloop-control',
  actionType: 'Relocation' as const,
  decisionReason: 'SELECTED_HIGHEST_SCORE' as const,
  decidedAt: '2026-10-13T05:01:32Z',
});

const crdIncident = (phase: CrdRecoveryIncident['status'] extends infer S ? S extends { runtime?: { phase?: infer P } } ? P : never : never): CrdRecoveryIncident => ({
  apiVersion: 'opsloop.io/v1alpha1',
  kind: 'RecoveryIncident',
  metadata: { name: 'demo-api-recovery-0007', namespace: 'opsloop-demo', creationTimestamp: '2026-10-13T05:01:14Z' },
  spec: {
    policyRef: { name: 'demo-api-recovery', uid: 'pol-1' },
    sequence: 7,
    policyGeneration: 3,
    policyHash: 'sha256:abc123def4567890',
    policy: policySpec(),
    workload: { name: 'demo-api', uid: 'wl-1' },
    trigger: {
      class: 'INFRASTRUCTURE_FAILURE',
      reason: 'NodeNotReady',
      signature: '',
      ruleVersion: 'rules-v1',
      nodeName: 'sarmubuntuserver',
      nodeUID: 'n-2',
      observedAt: '2026-10-13T05:00:54Z',
    },
    baseline: {
      templateHash: 'tmpl-1',
      specHash: 'spec-1',
      image: 'acr.example/demo-api@sha256:index',
      imageDigest: 'sha256:index',
      replicas: 1,
      hostname: 'sarmubuntuserver',
    },
    detectedAt: '2026-10-13T05:01:14Z',
    deadline: '2026-10-13T05:11:14Z',
  },
  status: {
    runtime: {
      phase,
      reason: phase === 'RECOVERED' ? 'HttpVerified' : 'InProgress',
      lastTransitionTime: '2026-10-13T05:02:14Z',
      ...(phase === 'RECOVERED' ? { completedAt: '2026-10-13T05:02:14Z' } : {}),
      decision: decision(),
      attempts: [
        {
          actionID: 'demo-api-recovery-0007-a1',
          outcome: phase === 'RECOVERED' ? 'Succeeded' : 'Patched',
          nodeName: 'opsloop-control',
          nodeUID: 'n-1',
          hostname: 'opsloop-control',
          decision: decision(),
          reservedAt: '2026-10-13T05:01:33Z',
          patchedAt: '2026-10-13T05:01:34Z',
          podReadyAt: '2026-10-13T05:01:43Z',
          podUID: 'pod-9',
          podName: 'demo-api-abc12',
          consecutiveSuccesses: 7,
          ...(phase === 'RECOVERED' ? { completedAt: '2026-10-13T05:02:14Z' } : {}),
        },
      ],
      oldPodsOutstanding: [{ name: 'demo-api-old', uid: 'pod-1', nodeName: 'sarmubuntuserver', terminating: false }],
    },
  },
});

describe('Go duration and quantity parsing', () => {
  it('parses metav1.Duration strings to seconds', () => {
    expect(parseDuration('20s', 'x')).toBe(20);
    expect(parseDuration('1m30s', 'x')).toBe(90);
    expect(parseDuration('30m', 'x')).toBe(1800);
    expect(parseDuration('500ms', 'x')).toBe(0.5);
    expect(parseDuration('1h', 'x')).toBe(3600);
    expect(() => parseDuration('20', 'p.d')).toThrow(/p\.d/);
    expect(() => parseDuration('20sec', 'p.d')).toThrow();
  });

  it('parses memory quantities to bytes', () => {
    expect(parseMemoryQuantity('512Mi', 'x')).toBe(512 * 1024 ** 2);
    expect(parseMemoryQuantity('1Gi', 'x')).toBe(1024 ** 3);
    expect(parseMemoryQuantity('1.5Gi', 'x')).toBe(1.5 * 1024 ** 3);
    expect(parseMemoryQuantity('268435456', 'x')).toBe(268435456);
    expect(parseMemoryQuantity('500M', 'x')).toBe(500e6);
    expect(() => parseMemoryQuantity('512MB', 'q')).toThrow(/q/);
  });
});

describe('policyFromCrd', () => {
  it('converts percent weights and thresholds to the 0..1 view model', () => {
    const p = policyFromCrd(crdPolicy(), { workloadUid: 'wl-1' });
    expect(p.spec.score).toMatchObject({ cpuWeight: 0.3, memoryWeight: 0.35, requestsWeight: 0.35 });
    expect(p.spec.score.recentFailurePenaltyCap).toBe(45);
    expect(p.spec.score.failureHistoryWindowSeconds).toBe(1800);
    expect(p.spec.filter).toMatchObject({
      maxCPUUtilization: 0.85,
      minimumAvailableMemoryBytes: 512 * 1024 ** 2,
      requestedCapacityReserve: 0.1,
      maxSampleAgeSeconds: 30,
    });
    expect(p.spec.candidateSelector).toBe('opsloop.io/recovery-candidate=true');
    expect(p.spec.engineering.enabled).toBe(false);
  });

  it('derives workload–node cooldowns from status.recentFailures', () => {
    const p = policyFromCrd(crdPolicy(), { workloadUid: 'wl-1' });
    expect(p.status.cooldowns).toEqual([
      { workloadUid: 'wl-1', nodeName: 'azure-arm64-worker', until: Date.parse('2026-10-13T04:59:00Z') + 120_000 },
    ]);
    expect(p.status.recentFailures).toHaveLength(1);
  });
});

describe('incidentFromCrd', () => {
  it('preserves measured resource values and separately displays run-only applied inputs', () => {
    const crd = crdIncident('WAITING');
    const candidate = crd.status!.runtime!.decision!.candidates![0]!;
    candidate.resourceCondition = {runID: 'run-1', epoch: 'epoch-1', policyUID: 'pol-1', configSHA256: 'a'.repeat(64), expiresAt: '2026-10-13T05:10:00Z', appliedMeasurements: {...candidate.measurements, cpuUtilizationPermille: 950, memAvailableBytes: 1024}};
    const before = JSON.stringify(crd);
    const converted = incidentFromCrd(crd).status.runtime.decision!.candidates[0]!;
    expect(converted.measurements.cpuUtilization).toBe(0.31);
    expect(converted.resourceCondition!.appliedMeasurements.cpuUtilization).toBe(0.95);
    expect(converted.resourceCondition!.appliedMeasurements.memAvailableBytes).toBe(1024);
    expect(JSON.stringify(crd)).toBe(before);
  });

  it('converts a recovered Node Failure incident', () => {
    const inc = incidentFromCrd(crdIncident('RECOVERED'), {
      incident: 'demo-api-recovery-0007',
      faultInjectedAt: '2026-10-13T05:00:14Z',
    });
    const rt = inc.status.runtime;
    expect(rt.phase).toBe('RECOVERED');
    expect(rt.classification).toBe('INFRASTRUCTURE_FAILURE');
    expect(rt.timings).toEqual({
      faultInjectedAt: Date.parse('2026-10-13T05:00:14Z'),
      triggeredAt: Date.parse('2026-10-13T05:00:54Z'),
      detectedAt: Date.parse('2026-10-13T05:01:14Z'),
      decidedAt: Date.parse('2026-10-13T05:01:32Z'),
      placedAt: Date.parse('2026-10-13T05:01:43Z'),
      recoveredAt: Date.parse('2026-10-13T05:02:14Z'),
    });
    expect(rt.attempts[0]).toMatchObject({ result: 'verified', targetArch: 'amd64', httpSuccesses: 7, type: 'relocation' });
    expect(rt.oldPodsOutstanding).toBe(true);
    expect(rt.decision!.sourceNodeName).toBe('sarmubuntuserver');
    expect(inc.status.events.length).toBeGreaterThan(3);
  });

  it('converts permille measurements and milli-point scores', () => {
    const d = incidentFromCrd(crdIncident('VERIFYING')).status.runtime.decision!;
    const laptop = d.candidates.find((c) => c.nodeName === 'opsloop-control')!;
    expect(laptop.measurements.cpuUtilization).toBeCloseTo(0.31);
    expect(laptop.measurements.memoryUtilization).toBeCloseTo(1 - 3.8 / 8);
    expect(laptop.score!.total).toBeCloseTo(54.245);
    expect(laptop.score!.weighted.cpu).toBeCloseTo(100 * 0.3 * 0.69);
    const physical = d.candidates.find((c) => c.nodeName === 'sarmubuntuserver')!;
    expect(physical.exclusion).toBe('FAILED_NODE');
    expect(physical.measurements.cpuUtilization).toBeNull();
    expect(physical.measurements.sampleAgeSeconds).toBeNull();
    expect(d.candidates.find((c) => c.arch === 'arm64')!.rejectReasons).toEqual(['CPU_PRESSURE']);
    expect(d.actionType).toBe('relocation');
  });

  it('maps the empty phase to DETECTED and keeps T_* on the API-signal basis without a driver report', () => {
    const inc = incidentFromCrd(crdIncident(''));
    expect(inc.status.runtime.phase).toBe('DETECTED');
    expect(inc.status.runtime.timings.faultInjectedAt).toBeUndefined();
  });

  it('rejects unknown reject reasons and bad timestamps with the field path', () => {
    const bad = crdIncident('VERIFYING');
    bad.status!.runtime!.decision!.candidates![2]!.rejectReasons = ['TOO_FAR_AWAY'];
    expect(() => incidentFromCrd(bad)).toThrow(/candidates\[2\]\.rejectReasons\[0\]/);
    const time = crdIncident('VERIFYING');
    time.spec.detectedAt = 'yesterday';
    expect(() => incidentFromCrd(time)).toThrow(/spec\.detectedAt/);
  });
});

describe('parseSnapshot with CRD objects from a BFF', () => {
  function liveBody(phase: 'RECOVERED' | 'OBSERVED' | 'WAITING') {
    const demo = JSON.parse(JSON.stringify(new DemoSimulation(DEMO_SCENARIOS[0]!.script).snapshot()));
    delete demo.narration;
    return {
      ...demo,
      source: 'live',
      observedAt: Date.parse('2026-10-13T05:02:20Z'),
      workload: { ...demo.workload, uid: 'wl-1' },
      policy: crdPolicy(),
      activeIncident: crdIncident(phase),
      recentIncidents: [],
      driverReports: [{ incident: 'demo-api-recovery-0007', faultInjectedAt: '2026-10-13T05:00:14Z' }],
    };
  }

  it('preserves a stored Controller decision when current Live metrics cannot be evaluated', () => {
    const snapshot = parseSnapshot(liveBody('OBSERVED'));
    const recorded = structuredClone(snapshot.activeIncident!.status.runtime.decision!);
    // The older BFF payload lacks the raw inputs required for a trustworthy preview.
    expect(evaluateNow(snapshot).selected).toBeNull();
    expect(evaluateNow(snapshot).candidates.every(c => c.rejectReasons.includes('METRICS_MISSING'))).toBe(true);
    const model = buildBoardModel(snapshot);
    expect(model.kind).toBe('decision');
    expect(model.rows.find(r => r.winner)?.nodeName).toBe('opsloop-control');
    expect(model.rows.find(r => r.winner)?.score?.total).toBe(54.245);
    expect(snapshot.activeIncident!.status.runtime.decision).toEqual(recorded);
  });

  it('converts exponent and fractional byte policy quantities through the Live adapter', () => {
    const body = liveBody('OBSERVED');
    body.policy.spec.filter.minimumAvailableMemory = '1e6';
    body.policy.spec.filter.startupMemoryBudget = '1m';
    const snapshot = parseSnapshot(body);
    expect(snapshot.policy.spec.filter.minimumAvailableMemoryBytes).toBe(1000000);
    expect(snapshot.policy.spec.filter.startupMemoryBudgetBytes).toBe(1);
    body.policy.spec.filter.minimumAvailableMemory = '9007199254740991.1';
    expect(() => parseSnapshot(body)).toThrow(/minimumAvailableMemory.*safe integer/);
  });

  it('rejects invalid raw metric metadata rather than presenting it as normal', () => {
    const body = liveBody('OBSERVED');
    body.cluster.nodes[0].usage.memTotalBytes = Number.NaN;
    expect(() => parseSnapshot(body)).toThrow(/memTotalBytes/);
  });

  it('converts the CRDs and every region renders the live snapshot', () => {
    for (const phase of ['RECOVERED', 'OBSERVED', 'WAITING'] as const) {
      const snapshot = parseSnapshot(liveBody(phase));
      expect(snapshot.source).toBe('live');
      expect(snapshot.policy.spec.score.cpuWeight).toBe(0.3);
      expect(snapshot.policy.policyHash).toBe('sha256:abc123def4567890');
      expect(snapshot.activeIncident!.status.runtime.timings.faultInjectedAt).toBeTypeOf('number');
      for (const Region of [ClusterMimic, CandidateBoard, PipelineLadder, IncidentPanel, EngineeringTrack, PolicyPanel]) {
        const html = renderToStaticMarkup(createElement(Region, { snapshot }));
        expect(html.length, `${Region.name} ${phase}`).toBeGreaterThan(100);
        expect(html, `${Region.name} ${phase}`).not.toContain('NaN');
        expect(html, `${Region.name} ${phase}`).not.toContain('시뮬레이션 시간');
      }
    }
  });
});

describe('Actual failure reason precedence', () => {
  it('keeps NODE_NOT_READY when Controller also records podName', () => {
    const crd = crdIncident('OBSERVED');
    crd.spec.trigger.reason = 'NODE_NOT_READY'; crd.spec.trigger.podName = 'failed-pod';
    expect(incidentFromCrd(crd).spec.trigger).toMatchObject({type:'NodeNotReady',nodeName:crd.spec.trigger.nodeName});
  });
  it('distinguishes IMAGE_PULL and REPEATED_EXIT; unknown signals stay explicit', () => {
    for (const [reason, type] of [['IMAGE_PULL','ImagePull'],['REPEATED_EXIT','RepeatedRestart'],['OOM_KILLED','FailureSignal']]) {
      const crd = crdIncident('MANUAL_REQUIRED'); crd.spec.trigger.reason = reason!;
      expect(incidentFromCrd(crd).spec.trigger.type).toBe(type);
    }
  });
});
