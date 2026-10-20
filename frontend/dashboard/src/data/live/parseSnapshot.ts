import type { DashboardSnapshot } from '../../domain/types';
import type { DriverReport } from './crd';
import { incidentFromCrd, isCrdIncident, isCrdPolicy, policyFromCrd } from './fromCrd';

/**
 * Structural validation of the fields the dashboard computes with: timestamps
 * and quantities must already be numbers (epoch ms, bytes, millicores), enums
 * must be known values. A payload that fails is rejected with a path-specific
 * message instead of being rendered with NaN or an unknown label. It is not a
 * full schema validator; the BFF remains responsible for content.
 */
export function parseSnapshot(input: unknown): DashboardSnapshot {
  if (!isRecord(input)) throw new Error('snapshot is not an object');
  // Work on a copy: CRD conversion replaces fields and must not mutate the response.
  const body: Record<string, unknown> = { ...input };
  const { observedAt, cluster, workload, http } = body;
  if (!isFiniteNumber(observedAt)) throw new Error('snapshot.observedAt must be epoch ms');
  if (!isRecord(cluster) || !Array.isArray(cluster.nodes)) throw new Error('snapshot.cluster.nodes missing');
  cluster.nodes.forEach((node, i) => validateNode(node, `cluster.nodes[${i}]`));
  if (!isRecord(workload) || typeof workload.name !== 'string') throw new Error('snapshot.workload missing');
  validateQuantity(workload.requests, 'workload.requests');
  normalizeCrdObjects(body, workload);
  const policy = body.policy;
  if (!isRecord(policy) || !isRecord(policy.spec)) throw new Error('snapshot.policy.spec missing');
  validatePolicy(policy);
  validateOptionalArray(workload.tolerations, 'workload.tolerations', isRecord);
  validateOptionalArray(workload.unsupportedConstraints, 'workload.unsupportedConstraints', (x) => typeof x === 'string');
  if (!isRecord(http) || !Array.isArray(http.probes)) throw new Error('snapshot.http.probes missing');
  // BFF-only join fields are consumed by the conversion above.
  delete body.driverReports;
  delete body.policyHash;
  const snapshot = body as unknown as DashboardSnapshot;
  // Live data never carries demo narration, and is always labeled live.
  const { narration: _ignored, ...rest } = snapshot;
  return {
    ...rest,
    source: 'live',
    activeIncident: snapshot.activeIncident ?? null,
    recentIncidents: Array.isArray(snapshot.recentIncidents) ? snapshot.recentIncidents : [],
  };
}

/**
 * The BFF may forward the Go CRD objects (kind RecoveryPolicy / RecoveryIncident)
 * as-is; they are converted to the view model here, in place, before validation.
 * `driverReports` carries Demo Driver fault-injection times (§18) and
 * `policyHash` the hash of the live policy spec, neither of which the CRDs hold.
 */
function normalizeCrdObjects(body: Record<string, unknown>, workload: Record<string, unknown>): void {
  const reports = Array.isArray(body.driverReports) ? (body.driverReports as DriverReport[]) : [];
  const report = (name: string) => reports.find((r) => r.incident === name);
  const convert = (x: unknown) => (isCrdIncident(x) ? incidentFromCrd(x, report(x.metadata.name)) : x);
  body.activeIncident = convert(body.activeIncident ?? null);
  if (Array.isArray(body.recentIncidents)) body.recentIncidents = body.recentIncidents.map(convert);
  if (isCrdPolicy(body.policy)) {
    const active = body.activeIncident as { spec?: { policyHash?: string } } | null;
    const hash = typeof body.policyHash === 'string' ? body.policyHash : active?.spec?.policyHash;
    body.policy = policyFromCrd(body.policy, {
      workloadUid: String(workload.uid ?? ''),
      ...(hash ? { policyHash: hash } : {}),
    });
  }
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x);
}

function isFiniteNumber(x: unknown): x is number {
  return typeof x === 'number' && Number.isFinite(x);
}

const ARCHES = new Set(['amd64', 'arm64']);
const READINESS = new Set(['Ready', 'NotReady', 'Unknown']);
const PRESSURES = new Set(['MemoryPressure', 'DiskPressure', 'PIDPressure']);

function validateQuantity(q: unknown, path: string): void {
  if (!isRecord(q) || !isFiniteNumber(q.cpuMillicores) || !isFiniteNumber(q.memoryBytes)) {
    throw new Error(`${path} must be numeric {cpuMillicores, memoryBytes} (convert resource.Quantity in the BFF)`);
  }
}

function validateSample(s: unknown, path: string): void {
  if (!isRecord(s) || !(s.value === null || isFiniteNumber(s.value)) || !isFiniteNumber(s.observedAt)) {
    throw new Error(`${path} must be {value: number|null, observedAt: epoch ms}`);
  }
}

function validateNode(node: unknown, path: string): void {
  if (!isRecord(node) || typeof node.name !== 'string') throw new Error(`${path}.name missing`);
  if (!ARCHES.has(node.arch as string)) throw new Error(`${path}.arch must be amd64 or arm64`);
  if (!READINESS.has(node.readiness as string)) throw new Error(`${path}.readiness is not a known value`);
  if (!Array.isArray(node.pressure) || node.pressure.some((p) => !PRESSURES.has(p as string))) {
    throw new Error(`${path}.pressure contains an unknown condition`);
  }
  validateQuantity(node.allocatable, `${path}.allocatable`);
  validateQuantity(node.requested, `${path}.requested`);
  if (!isRecord(node.usage)) throw new Error(`${path}.usage missing`);
  validateSample(node.usage.cpu, `${path}.usage.cpu`);
  validateSample(node.usage.memory, `${path}.usage.memory`);
  validateSample(node.usage.memAvailableBytes, `${path}.usage.memAvailableBytes`);
  for (const field of ['memTotalBytes', 'cpuSampleCount', 'exporterUp']) {
    const value = node.usage[field];
    if (value !== undefined && value !== null && (typeof value !== 'number' || !Number.isFinite(value))) {
      throw new Error(`${path}.usage.${field} must be a finite number or null`);
    }
  }
  if (!Array.isArray(node.pods)) throw new Error(`${path}.pods missing`);
  validateOptionalArray(
    node.taints,
    `${path}.taints`,
    (t) => isRecord(t) && typeof t.key === 'string' && TAINT_EFFECTS.has(t.effect as string),
  );
}

const TAINT_EFFECTS = new Set(['NoSchedule', 'PreferNoSchedule', 'NoExecute']);

function validateOptionalArray(value: unknown, path: string, item: (x: unknown) => boolean): void {
  if (value === undefined) return;
  if (!Array.isArray(value) || !value.every(item)) throw new Error(`${path} must be an array of valid entries`);
}

function requireNumbers(record: unknown, keys: string[], path: string): void {
  if (!isRecord(record)) throw new Error(`${path} missing`);
  for (const key of keys) {
    if (!isFiniteNumber(record[key])) throw new Error(`${path}.${key} must be a number`);
  }
}

function validatePolicy(policy: Record<string, unknown>): void {
  const spec = policy.spec as Record<string, unknown>;
  requireNumbers(
    spec.filter,
    [
      'maxCPUUtilization',
      'maxMemoryUtilization',
      'minimumAvailableMemoryBytes',
      'startupMemoryBudgetBytes',
      'requestedCapacityReserve',
      'maxSampleAgeSeconds',
    ],
    'policy.spec.filter',
  );
  requireNumbers(spec.score, ['cpuWeight', 'memoryWeight', 'requestsWeight', 'recentFailurePenalty'], 'policy.spec.score');
  const score = spec.score as Record<string, unknown>;
  for (const optional of ['recentFailurePenaltyCap', 'failureHistoryWindowSeconds']) {
    if (score[optional] !== undefined && !isFiniteNumber(score[optional])) {
      throw new Error(`policy.spec.score.${optional} must be a number when present`);
    }
  }
  if (!Array.isArray(spec.requiredPlatforms)) throw new Error('policy.spec.requiredPlatforms missing');
  const status = policy.status;
  if (!isRecord(status) || !Array.isArray(status.cooldowns)) throw new Error('policy.status.cooldowns missing');
  status.cooldowns.forEach((c, i) => {
    if (!isRecord(c) || typeof c.workloadUid !== 'string' || typeof c.nodeName !== 'string' || !isFiniteNumber(c.until)) {
      throw new Error(`policy.status.cooldowns[${i}] must be {workloadUid, nodeName, until: epoch ms}`);
    }
  });
}
