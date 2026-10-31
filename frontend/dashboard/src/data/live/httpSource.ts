import { parseSnapshot } from './parseSnapshot';
export { parseSnapshot } from './parseSnapshot';
import type { DashboardSnapshot } from '../../domain/types';
import type { DashboardDataSource, SourceStatus } from '../source';

/** Read-only BFF adapter. Missing configuration or failures remain explicit;
 * never substitutes Demo data. Credentials stay on the BFF server. */
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
    this.baseUrl = options.baseUrl === '/' ? '' : options.baseUrl?.replace(/\/+$/, '') || undefined;
    this.pollMs = options.pollMs ?? 5000;
    this.timeoutMs = options.timeoutMs ?? 4000;
    this.fetchImpl = options.fetchImpl ?? ((...args) => fetch(...args));
    this.staleAfterMs = options.staleAfterMs ?? this.pollMs * 3;
    this.now = options.now ?? (() => Date.now());
    this.label = this.baseUrl !== undefined ? `Live · ${this.baseUrl || 'same origin'}` : 'Live · 연결 안 됨';
    this.status = this.baseUrl !== undefined
      ? { state: 'connecting' }
      : { state: 'unconfigured', message: '실측 관측 연결이 설정되지 않았습니다.' };
  }

  start(): void {
    if (this.running || this.baseUrl === undefined) return;
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
      if (res.status === 410) {
        const closed: unknown = await res.json();
        if (generation !== this.generation) return;
        if (closed && typeof closed === 'object' && 'snapshot' in closed && closed.snapshot) this.snapshot = parseSnapshot(closed.snapshot);
        this.running = false;
        throw new Error('실측 연결을 종료했습니다. 마지막 관측 기록입니다.');
      }
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
