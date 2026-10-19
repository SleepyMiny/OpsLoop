import { DemoDataSource } from '../data/demo/demoSource';
import { ReplaySource } from '../data/replay/replayCore';
import evidence from './evidence.public.json';
import type { DashboardDataSource } from '../data/source';

export class UnsupportedPublicMode extends Error {
  constructor() { super('공개 빌드는 Demo와 Evidence Replay만 제공합니다. Live 연결은 비공개 로컬 환경에서만 사용할 수 있습니다.'); }
}

/** No env argument, Live adapter import, API URL, fetch or fallback. */
export function createPublicDataSource(search: string): DashboardDataSource {
  const params = new URLSearchParams(search);
  const sources = params.getAll('source');
  if (sources.length > 1) throw new UnsupportedPublicMode();
  const requested = sources[0] ?? 'demo';
  if (requested === 'replay') return new ReplaySource(evidence);
  if (requested !== 'demo') throw new UnsupportedPublicMode();
  const source = new DemoDataSource({autoPlay: params.get('play') !== '0', autoLoop: params.get('loop') !== '0'});
  const scenario = params.get('scenario');
  if (scenario) {
    if (!source.listScenarios().some((s) => s.id === scenario)) throw new UnsupportedPublicMode();
    source.select(scenario);
  }
  const at = Number(params.get('at'));
  if (Number.isFinite(at) && at > 0) source.seek(at * 1000);
  return source;
}
