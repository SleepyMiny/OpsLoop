import type { DashboardSnapshot } from './types';

/** Replay focuses a recorded terminal Incident, without claiming it is active now. */
export function displayIncident(snapshot: Pick<DashboardSnapshot, 'activeIncident'> & Partial<Pick<DashboardSnapshot, 'source' | 'recentIncidents'>>) {
  return snapshot.activeIncident ?? (snapshot.source === 'replay' ? snapshot.recentIncidents?.[0] ?? null : null);
}
