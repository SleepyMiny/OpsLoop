import type { DashboardSnapshot } from '../../domain/types';
import type { DashboardDataSource, SourceStatus } from '../source';

/**
 * Live adapter for a future read-only BFF (see docs/live-data-contract.md).
 *
 * Nothing in the repository serves this endpoint yet: the Controller exposes only
 * /metrics, /healthz and /readyz, and its state lives in RecoveryPolicy /
 * RecoveryIncident CRD status. The browser must not hold a kubeconfig, so a small
 * BFF that reads those CRDs plus Prometheus is the expected producer. Until it
 * exists this source reports `unconfigured` or `error` honestly instead of falling
 * back to demo data.
 */
export interface HttpSourceOptions {
  baseUrl: string | undefined;
  pollMs?: number;
  timeoutMs?: number;
  /** A snapshot older than this (by its observedAt) is reported as stale. Default 3 polls. */
  staleAfterMs?: number;
  fetchImpl?: typeof fetch;
  now?: () => number;
}

export const SNAPSHOT_PATH = '/api/v1/snapshot';

export class HttpDataSource implements DashboardDataSource {
  readonly kind = 'live' as const;
  readonly label: string;

  private readonly baseUrl: string | undefined;
  private readonly pollMs: number;
  private readonly timeoutMs: number;
  private readonly fetchImpl: typeof fetch;
  private readonly listeners = new Set<() => void>();
  private snapshot: DashboardSnapshot | null = null;
  private status: SourceStatus;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private controller: AbortController | null = null;
  private running = false;
  /** Bumped on every start/stop so a poll from an earlier run cannot reschedule itself. */
  private generation = 0;
  private readonly staleAfterMs: number;
  private readonly now: () => number;

  constructor(options: HttpSourceOptions) {
    this.baseUrl = options.baseUrl?.replace(/\/+$/, '') || undefined;
    this.pollMs = options.pollMs ?? 5000;
    this.timeoutMs = options.timeoutMs ?? 4000;
    this.fetchImpl = options.fetchImpl ?? ((...args) => fetch(...args));
    this.staleAfterMs = options.staleAfterMs ?? this.pollMs * 3;
    this.now = options.now ?? (() => Date.now());
    this.label = this.baseUrl ? `Live · ${this.baseUrl}` : 'Live · 연결 안 됨';
    this.status = this.baseUrl
      ? { state: 'connecting' }
      : { state: 'unconfigured', message: 'VITE_OPSLOOP_API_BASE가 설정되지 않았습니다.' };
  }

  start(): void {
    if (this.running || !this.baseUrl) return;
    this.running = true;
    this.generation += 1;
    void this.poll(this.generation);
  }

  stop(): void {
    this.running = false;
    this.generation += 1;
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    this.controller?.abort();
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  getSnapshot(): DashboardSnapshot | null {
    return this.snapshot;
  }

  getStatus(): SourceStatus {
    return this.status;
  }

  private async poll(generation: number): Promise<void> {
    if (!this.running || generation !== this.generation) return;
    const controller = new AbortController();
    this.controller = controller;
    let timedOut = false;
    const timeout = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, this.timeoutMs);
    try {
      const res = await this.fetchImpl(`${this.baseUrl}${SNAPSHOT_PATH}`, {
        signal: controller.signal,
        headers: { Accept: 'application/json' },
        credentials: 'omit',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body: unknown = await res.json();
      if (generation !== this.generation) return;
      this.snapshot = parseSnapshot(body);
      const age = this.now() - this.snapshot.observedAt;
      this.status =
        age > this.staleAfterMs
          ? {
              state: 'stale',
              lastUpdate: this.snapshot.observedAt,
              message: `BFF가 ${Math.round(age / 1000)}초 전 snapshot을 돌려주고 있습니다.`,
            }
          : { state: 'ok', lastUpdate: this.snapshot.observedAt };
    } catch (err) {
      // An abort caused by stop() (or a restart) is not a connection error.
      if (generation !== this.generation) return;
      const message = timedOut ? `응답 없음 (${this.timeoutMs} ms 초과)` : err instanceof Error ? err.message : String(err);
      // Keep the last good snapshot visible but mark it as not current.
      this.status = {
        state: 'error',
        message,
        ...(this.snapshot ? { lastUpdate: this.snapshot.observedAt } : {}),
      };
    } finally {
      clearTimeout(timeout);
    }
    if (generation !== this.generation) return;
    this.emit();
    if (this.running) this.timer = setTimeout(() => void this.poll(generation), this.pollMs);
  }

  private emit(): void {
    for (const listener of this.listeners) listener();
  }
}

/**
 * Structural validation of the fields the dashboard computes with: timestamps
 * and quantities must already be numbers (epoch ms, bytes, millicores), enums
 * must be known values. A payload that fails is rejected with a path-specific
 * message instead of being rendered with NaN or an unknown label. It is not a
 * full schema validator; the BFF remains responsible for content.
 */
export function parseSnapshot(body: unknown): DashboardSnapshot {
  if (!isRecord(body)) throw new Error('snapshot is not an object');
  const { observedAt, cluster, workload, policy, http } = body;
  if (!isFiniteNumber(observedAt)) throw new Error('snapshot.observedAt must be epoch ms');
  if (!isRecord(cluster) || !Array.isArray(cluster.nodes)) throw new Error('snapshot.cluster.nodes missing');
  cluster.nodes.forEach((node, i) => validateNode(node, `cluster.nodes[${i}]`));
  if (!isRecord(workload) || typeof workload.name !== 'string') throw new Error('snapshot.workload missing');
  validateQuantity(workload.requests, 'workload.requests');
  if (!isRecord(policy) || !isRecord(policy.spec)) throw new Error('snapshot.policy.spec missing');
  if (!isRecord(http) || !Array.isArray(http.probes)) throw new Error('snapshot.http.probes missing');
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
  if (!Array.isArray(node.pods)) throw new Error(`${path}.pods missing`);
}
