import type { DashboardSnapshot, RecoveryIncident } from '../../domain/types';
import type { DashboardDataSource, DemoController, PlaybackState, ScenarioInfo, SourceStatus } from '../source';
import { DemoSimulation } from './engine';
import { DEMO_SCENARIOS, type DemoScenario } from './scenarios';

const FRAME_MS = 100;
/** Longest real-time gap applied in one frame, so a sleeping laptop does not jump ahead. */
const MAX_FRAME_DELTA_MS = 250;
/** Real-time pause on the final frame before auto-loop moves to the next scenario. */
const END_HOLD_MS = 6000;
const HISTORY_LIMIT = 6;
const FIRST_SEQUENCE = 6;

export interface DemoSourceOptions {
  scenarios?: DemoScenario[];
  initialScenarioId?: string;
  autoLoop?: boolean;
  autoPlay?: boolean;
  speed?: number;
  /** Injected for tests. */
  now?: () => number;
  setInterval?: (fn: () => void, ms: number) => unknown;
  clearInterval?: (handle: unknown) => void;
}

/**
 * Demo Mode data source: plays scripted scenarios through DemoSimulation in real
 * time and exposes presenter controls. Everything it emits is labeled `demo`.
 */
export class DemoDataSource implements DashboardDataSource, DemoController {
  readonly kind = 'demo' as const;
  readonly label = 'Demo Mode · 시뮬레이션 데이터';
  readonly demo: DemoController = this;

  private readonly scenarios: DemoScenario[];
  private readonly now: () => number;
  private readonly setTimer: (fn: () => void, ms: number) => unknown;
  private readonly clearTimer: (handle: unknown) => void;
  private readonly listeners = new Set<() => void>();

  private scenario: DemoScenario;
  private sim: DemoSimulation;
  private simMs = 0;
  private playing: boolean;
  private speed: number;
  private autoLoop: boolean;
  private timer: unknown = null;
  private lastFrameAt = 0;
  private endedAt: number | null = null;
  private sequence = FIRST_SEQUENCE;
  private history: RecoveryIncident[] = [];

  private cachedSnapshot: DashboardSnapshot | null = null;
  private cachedSimSnapshot: DashboardSnapshot | null = null;
  private cachedPlayback: PlaybackState | null = null;
  private cachedStatus: SourceStatus | null = null;

  constructor(options: DemoSourceOptions = {}) {
    this.scenarios = options.scenarios ?? DEMO_SCENARIOS;
    if (this.scenarios.length === 0) throw new Error('DemoDataSource needs at least one scenario');
    this.now = options.now ?? (() => performance.now());
    this.setTimer = options.setInterval ?? ((fn, ms) => window.setInterval(fn, ms));
    this.clearTimer = options.clearInterval ?? ((h) => window.clearInterval(h as number));
    this.playing = options.autoPlay ?? true;
    this.speed = options.speed ?? 1;
    this.autoLoop = options.autoLoop ?? true;
    this.scenario = this.findScenario(options.initialScenarioId) ?? this.scenarios[0]!;
    this.sim = this.createSim();
  }

  // --- DashboardDataSource -------------------------------------------------

  start(): void {
    if (this.timer !== null) return;
    this.lastFrameAt = this.now();
    this.timer = this.setTimer(() => this.frame(), FRAME_MS);
  }

  stop(): void {
    if (this.timer === null) return;
    this.clearTimer(this.timer);
    this.timer = null;
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  getSnapshot(): DashboardSnapshot {
    const simSnapshot = this.sim.snapshot();
    if (this.cachedSnapshot && this.cachedSimSnapshot === simSnapshot) return this.cachedSnapshot;
    this.cachedSimSnapshot = simSnapshot;
    this.cachedSnapshot = { ...simSnapshot, recentIncidents: this.history };
    return this.cachedSnapshot;
  }

  getStatus(): SourceStatus {
    const lastUpdate = this.getSnapshot().observedAt;
    if (this.cachedStatus?.state !== 'ok' || this.cachedStatus.lastUpdate !== lastUpdate) {
      this.cachedStatus = { state: 'ok', lastUpdate };
    }
    return this.cachedStatus;
  }

  // --- DemoController ------------------------------------------------------

  listScenarios(): ScenarioInfo[] {
    return this.scenarios.map(({ id, title, shortTitle, summary, tag, designRef }) => ({
      id,
      title,
      shortTitle,
      summary,
      tag,
      designRef,
    }));
  }

  getPlayback(): PlaybackState {
    if (this.cachedPlayback) return this.cachedPlayback;
    const chapters = this.sim.chapters();
    let chapterIndex = 0;
    chapters.forEach((c, i) => {
      if (c.atMs <= this.sim.elapsed) chapterIndex = i;
    });
    this.cachedPlayback = {
      scenarioId: this.scenario.id,
      playing: this.playing,
      speed: this.speed,
      pace: this.sim.pace,
      autoLoop: this.autoLoop,
      simElapsedMs: this.sim.elapsed,
      durationMs: this.sim.durationMs,
      chapters,
      chapterIndex,
    };
    return this.cachedPlayback;
  }

  select(scenarioId: string): void {
    const next = this.findScenario(scenarioId);
    if (!next) return;
    this.archiveIncident();
    this.scenario = next;
    this.sequence += 1;
    this.sim = this.createSim();
    this.simMs = 0;
    this.endedAt = null;
    this.emit();
  }

  play(): void {
    if (this.sim.finished) this.restart();
    this.playing = true;
    this.lastFrameAt = this.now();
    this.emit();
  }

  pause(): void {
    this.playing = false;
    this.emit();
  }

  togglePlay(): void {
    if (this.playing) this.pause();
    else this.play();
  }

  restart(): void {
    this.seek(0);
  }

  nextChapter(): void {
    const next = this.sim.chapters().find((c) => c.atMs > this.sim.elapsed + 1);
    this.seek(next ? next.atMs : this.sim.durationMs);
  }

  previousChapter(): void {
    const chapters = this.sim.chapters();
    const passed = chapters.filter((c) => c.atMs <= this.sim.elapsed);
    const current = passed.at(-1);
    // Within the first 2 s of a chapter, go to the one before it; otherwise restart this one.
    const target =
      current && this.sim.elapsed - current.atMs > 2000 ? current : (passed.at(-2) ?? chapters[0]);
    this.seek(target?.atMs ?? 0);
  }

  seek(simMs: number): void {
    this.simMs = Math.max(0, Math.min(simMs, this.sim.durationMs));
    this.sim.advanceTo(this.simMs);
    this.endedAt = this.sim.finished ? this.now() : null;
    this.emit();
  }

  setSpeed(speed: number): void {
    this.speed = Math.max(0.25, Math.min(speed, 8));
    this.emit();
  }

  setAutoLoop(enabled: boolean): void {
    this.autoLoop = enabled;
    this.emit();
  }

  // --- internals -----------------------------------------------------------

  private frame(): void {
    const t = this.now();
    const delta = Math.min(t - this.lastFrameAt, MAX_FRAME_DELTA_MS);
    this.lastFrameAt = t;
    if (!this.playing) return;

    if (this.sim.finished) {
      this.endedAt ??= t;
      if (!this.autoLoop) this.pause();
      else if (t - this.endedAt >= END_HOLD_MS) this.advanceScenario();
      return;
    }
    const before = this.sim.elapsed;
    this.simMs += delta * this.speed * this.sim.pace;
    this.sim.advanceTo(this.simMs);
    if (this.sim.elapsed !== before) this.emit();
  }

  private advanceScenario(): void {
    const idx = this.scenarios.findIndex((s) => s.id === this.scenario.id);
    const next = this.scenarios[(idx + 1) % this.scenarios.length]!;
    this.select(next.id);
  }

  private archiveIncident(): void {
    const incident = this.sim.snapshot().activeIncident;
    if (!incident) return;
    this.history = [incident, ...this.history.filter((i) => i.name !== incident.name)].slice(0, HISTORY_LIMIT);
  }

  private createSim(): DemoSimulation {
    return new DemoSimulation(this.scenario.script, { incidentSequence: this.sequence });
  }

  private findScenario(id: string | undefined): DemoScenario | undefined {
    return id ? this.scenarios.find((s) => s.id === id) : undefined;
  }

  private emit(): void {
    this.cachedPlayback = null;
    for (const listener of this.listeners) listener();
  }
}
