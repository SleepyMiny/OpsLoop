import { DemoDataSource } from './demo/demoSource';
import { HttpDataSource } from './live/httpSource';
import { ReplayDataSource } from './replay/replaySource';
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
  if (requested === 'replay') return new ReplayDataSource();
  if (requested !== 'demo') throw new Error('알 수 없는 데이터 모드. demo, live, replay 중 선택하세요.');
  const scenario = params.get('scenario') ?? undefined;
  const source = new DemoDataSource({
    ...(scenario ? { initialScenarioId: scenario } : {}),
    autoLoop: params.get('loop') !== '0',
    autoPlay: params.get('play') !== '0',
  });
  // ?at=<seconds> opens a scenario at a given moment (presenter bookmarks, visual QA).
  const at = Number(params.get('at'));
  if (Number.isFinite(at) && at > 0) source.seek(at * 1000);
  return source;
}
