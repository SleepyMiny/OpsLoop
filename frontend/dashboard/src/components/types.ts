import type { DashboardSnapshot } from '../domain/types';

/** Every dashboard region receives the whole snapshot and picks what it needs. */
export interface RegionProps {
  snapshot: DashboardSnapshot;
  /** True when the current narration step points at this region. */
  emphasized?: boolean;
}
