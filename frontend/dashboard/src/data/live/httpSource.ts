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
  fetchImpl?: typeof fetch;
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

  constructor(options: HttpSourceOptions) {
    this.baseUrl = options.baseUrl?.replace(/\/+$/, '') || undefined;
    this.pollMs = options.pollMs ?? 5000;
    this.timeoutMs = options.timeoutMs ?? 4000;
    this.fetchImpl = options.fetchImpl ?? ((...args) => fetch(...args));
    this.label = this.baseUrl ? `Live · ${this.baseUrl}` : 'Live · 연결 안 됨';
    this.status = this.baseUrl
      ? { state: 'connecting' }
      : { state: 'unconfigured', message: 'VITE_OPSLOOP_API_BASE가 설정되지 않았습니다.' };
  }

  start(): void {
    if (this.running || !this.baseUrl) return;
    this.running = true;
    void this.poll();
  }

  stop(): void {
    this.running = false;
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

  private async poll(): Promise<void> {
    if (!this.running) return;
    this.controller = new AbortController();
    const timeout = setTimeout(() => this.controller?.abort(), this.timeoutMs);
    try {
      const res = await this.fetchImpl(`${this.baseUrl}${SNAPSHOT_PATH}`, {
        signal: this.controller.signal,
        headers: { Accept: 'application/json' },
        credentials: 'omit',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body: unknown = await res.json();
      this.snapshot = parseSnapshot(body);
      this.status = { state: 'ok', lastUpdate: this.snapshot.observedAt };
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      // Keep the last good snapshot visible but mark it as not current.
      this.status = {
        state: 'error',
        message,
        ...(this.snapshot ? { lastUpdate: this.snapshot.observedAt } : {}),
      };
    } finally {
      clearTimeout(timeout);
    }
    this.emit();
    if (this.running) this.timer = setTimeout(() => void this.poll(), this.pollMs);
  }

  private emit(): void {
    for (const listener of this.listeners) listener();
  }
}

/**
 * Minimal structural validation. It rejects payloads that would break rendering;
 * it is not a full schema validator, and the BFF remains responsible for content.
 */
export function parseSnapshot(body: unknown): DashboardSnapshot {
  if (!isRecord(body)) throw new Error('snapshot is not an object');
  const { observedAt, cluster, workload, policy, http } = body;
  if (typeof observedAt !== 'number') throw new Error('snapshot.observedAt must be epoch ms');
  if (!isRecord(cluster) || !Array.isArray(cluster.nodes)) throw new Error('snapshot.cluster.nodes missing');
  if (!isRecord(workload) || typeof workload.name !== 'string') throw new Error('snapshot.workload missing');
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
