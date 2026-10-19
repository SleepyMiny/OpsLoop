import { parseSnapshot } from '../live/parseSnapshot';
import type { DashboardSnapshot } from '../../domain/types';
import type { DashboardDataSource, ReplayController, SourceStatus } from '../source';

interface Experiment {
  id: string; title: string; limitations: string[];
  provenance: Array<{file: string; sha256: string}>;
  milestones: Array<{label: string; at?: number; elapsedSeconds?: number}>;
  frames: Array<{label: string; snapshot: DashboardSnapshot}>;
}

export function parseReplay(input: unknown): Experiment[] {
  if (!input || typeof input !== 'object' || !('schemaVersion' in input) || input.schemaVersion !== 'opsloop-evidence-replay/v1' || !('experiments' in input) || !Array.isArray(input.experiments) || input.experiments.length === 0) throw new Error('지원하지 않는 Replay 형식');
  const ids = new Set<string>();
  return input.experiments.map((raw): Experiment => {
    if (!raw || typeof raw.id !== 'string' || ids.has(raw.id) || typeof raw.title !== 'string' || !Array.isArray(raw.frames) || !raw.frames.length || !Array.isArray(raw.provenance) || !raw.provenance.length || raw.provenance.some((p: {file: string; sha256: string}) => !p || typeof p.file !== 'string' || p.file.includes('/') || !/^[a-f0-9]{64}$/.test(p.sha256))) throw new Error('Replay 증거 식별자·출처 오류');
    ids.add(raw.id);
    if (!Array.isArray(raw.limitations) || raw.limitations.some((x: unknown) => typeof x !== 'string') || !Array.isArray(raw.milestones)) throw new Error('Replay 설명 오류');
    if (raw.milestones.some((m: {label?: unknown; at?: unknown; elapsedSeconds?: unknown}) => !m || typeof m.label !== 'string' ||
      (m.at !== undefined && (typeof m.at !== 'number' || !Number.isFinite(m.at) || m.at < 0 || m.at >= 253402300800000)) ||
      (m.elapsedSeconds !== undefined && (typeof m.elapsedSeconds !== 'number' || !Number.isFinite(m.elapsedSeconds) || m.elapsedSeconds < 0)))) throw new Error('Replay milestone 시각 오류');
    return { id: raw.id, title: raw.title, limitations: raw.limitations, provenance: raw.provenance, milestones: raw.milestones,
      frames: raw.frames.map((f: {label: string; snapshot: unknown}) => {
        if (!f || typeof f.label !== 'string') throw new Error('Replay frame label 오류');
        const snapshot = parseSnapshot(f.snapshot);
        if (snapshot.policy.spec.mode !== 'ObserveOnly' || snapshot.activeIncident !== null || snapshot.recentIncidents.some((i) => i.status.runtime.attempts.length !== 0 || !['OBSERVED', 'MANUAL_REQUIRED'].includes(i.status.runtime.phase))) throw new Error('ObserveOnly 종료 기록만 재생합니다');
        return {label: f.label, snapshot: {...snapshot, source: 'replay'}};
      }) };
  });
}

/** Discrete preserved frames only: no invented phases, metrics, live polling or recovery. */
export class ReplaySource implements DashboardDataSource, ReplayController {
  readonly kind = 'replay' as const;
  readonly label = 'Evidence Replay · 과거 실환경 기록';
  readonly replay: ReplayController = this;
  private readonly experiments: Experiment[];
  private selected = 0;
  private index = 0;
  private snapshot!: DashboardSnapshot;
  private status!: SourceStatus;
  private listeners = new Set<() => void>();
  constructor(input: unknown) { this.experiments = parseReplay(input); this.update(); }
  start() {} stop() {}
  subscribe(listener: () => void) { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; }
  getSnapshot() { return this.snapshot; }
  getStatus() { return this.status; }
  listExperiments() { return this.experiments.map(({id, title}) => ({id, title})); }
  getState() { const e = this.experiments[this.selected]!; return {experimentId: e.id, frameIndex: this.index, frameLabels: e.frames.map((f) => f.label)}; }
  select(id: string) { const i = this.experiments.findIndex((e) => e.id === id); if (i < 0) throw new Error('알 수 없는 기록'); this.selected = i; this.index = 0; this.update(); }
  setFrame(index: number) { if (!Number.isInteger(index) || index < 0 || index >= this.experiments[this.selected]!.frames.length) throw new Error('알 수 없는 frame'); this.index = index; this.update(); }
  private update() {
    const e = this.experiments[this.selected]!; const f = e.frames[this.index]!;
    this.snapshot = {...f.snapshot, replay: {experimentId: e.id, title: e.title, frameLabel: f.label, frameIndex: this.index, frameCount: e.frames.length, limitations: e.limitations, milestones: e.milestones, provenance: e.provenance}};
    this.status = {state: 'ok', lastUpdate: f.snapshot.observedAt};
    for (const listener of this.listeners) listener();
  }
}
