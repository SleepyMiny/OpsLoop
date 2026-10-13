import { useCallback, useEffect, useSyncExternalStore } from 'react';
import type { DashboardSnapshot } from '../domain/types';
import type { DashboardDataSource, PlaybackState, SourceStatus } from './source';

export interface DashboardState {
  snapshot: DashboardSnapshot | null;
  status: SourceStatus;
  playback: PlaybackState | null;
}

/** Subscribes a component tree to a data source; starts it on mount, stops on unmount. */
export function useDashboard(source: DashboardDataSource): DashboardState {
  useEffect(() => {
    source.start();
    return () => source.stop();
  }, [source]);

  const subscribe = useCallback((listener: () => void) => source.subscribe(listener), [source]);
  const snapshot = useSyncExternalStore(subscribe, () => source.getSnapshot());
  const status = useSyncExternalStore(subscribe, () => source.getStatus());
  const playback = useSyncExternalStore(subscribe, () => source.demo?.getPlayback() ?? null);
  return { snapshot, status, playback };
}
