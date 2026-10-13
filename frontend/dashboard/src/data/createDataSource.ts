import { DemoDataSource } from './demo/demoSource';
import { HttpDataSource } from './live/httpSource';
import type { DashboardDataSource } from './source';

/**
 * Chooses the data source. Demo Mode is the default and needs no network,
 * credentials or backend. Live is opt-in via `VITE_OPSLOOP_SOURCE=live` or
 * `?source=live`; the API base URL is only read from build-time env so a URL
 * parameter cannot point the kiosk at an arbitrary server.
 */
export interface DataSourceRequest {
  search: string;
  env: { VITE_OPSLOOP_SOURCE?: string; VITE_OPSLOOP_API_BASE?: string };
}

export function createDataSource({ search, env }: DataSourceRequest): DashboardDataSource {
  const params = new URLSearchParams(search);
  const requested = params.get('source') ?? env.VITE_OPSLOOP_SOURCE ?? 'demo';
  if (requested === 'live') return new HttpDataSource({ baseUrl: env.VITE_OPSLOOP_API_BASE });
  const scenario = params.get('scenario') ?? undefined;
  return new DemoDataSource({
    ...(scenario ? { initialScenarioId: scenario } : {}),
    autoLoop: params.get('loop') !== '0',
  });
}
