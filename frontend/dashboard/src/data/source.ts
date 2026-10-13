import type { DashboardSnapshot, DataSourceKind } from '../domain/types';

/**
 * The only seam between the UI and where data comes from. Components never import
 * the demo engine or fetch() directly; they read a DashboardDataSource through
 * useDashboard(). Swapping Demo Mode for live data means providing another
 * implementation of this interface (see src/data/live/httpSource.ts).
 */
export type SourceStatus =
  | { state: 'connecting' }
  | { state: 'ok'; lastUpdate: number }
  | { state: 'error'; message: string; lastUpdate?: number }
  | { state: 'unconfigured'; message: string };

export interface DashboardDataSource {
  readonly kind: DataSourceKind;
  /** Human-readable origin, shown in the header next to the mode badge. */
  readonly label: string;
  /** Present only for Demo Mode; live sources have no playback controls. */
  readonly demo?: DemoController;
  start(): void;
  stop(): void;
  /** useSyncExternalStore-compatible: listener fires on snapshot, status or playback change. */
  subscribe(listener: () => void): () => void;
  /** Must return the same object until something changes. */
  getSnapshot(): DashboardSnapshot | null;
  getStatus(): SourceStatus;
}

export type ScenarioTag = 'demo-a' | 'demo-b' | 'negative';

export interface ScenarioInfo {
  id: string;
  title: string;
  /** One line a visitor can read in three seconds. */
  summary: string;
  tag: ScenarioTag;
  /** Architecture section the scenario illustrates, e.g. "§17 Demo A". */
  designRef: string;
}

export interface Chapter {
  atMs: number;
  label: string;
}

export interface PlaybackState {
  scenarioId: string;
  playing: boolean;
  /** Presenter speed multiplier (×1, ×2 …). */
  speed: number;
  /** Scenario-defined pacing for the current stretch, e.g. 8 while waiting on timeouts. */
  pace: number;
  autoLoop: boolean;
  simElapsedMs: number;
  durationMs: number;
  chapters: Chapter[];
  chapterIndex: number;
}

export interface DemoController {
  listScenarios(): ScenarioInfo[];
  getPlayback(): PlaybackState;
  select(scenarioId: string): void;
  play(): void;
  pause(): void;
  togglePlay(): void;
  restart(): void;
  nextChapter(): void;
  previousChapter(): void;
  seek(simMs: number): void;
  setSpeed(speed: number): void;
  setAutoLoop(enabled: boolean): void;
}
