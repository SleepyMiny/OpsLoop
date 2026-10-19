import { displayIncident } from '../../domain/displayIncident';
import { RUNTIME_PHASE_LABEL } from '../../domain/labels';
import type { DashboardSnapshot } from '../../domain/types';
import type { LampState } from '../shared/primitives';
import { isInFlight } from '../incident/ladderModel';

/**
 * What the Demo API itself is doing, shown next to the Engineering track. A pull
 * request never changes this: only Runtime recovery or a human deploy does.
 */
export interface RuntimeAlongside {
  tone: LampState;
  /** Visitor-facing state, e.g. "격리 중". */
  state: string;
  /** Raw desired-state fact, e.g. "replicas=0". */
  fact: string;
}

export function runtimeAlongside(snapshot: Pick<DashboardSnapshot, 'activeIncident' | 'workload'> & Partial<Pick<DashboardSnapshot, 'source' | 'recentIncidents'>>): RuntimeAlongside {
  const fact = `replicas=${snapshot.workload.replicas}`;
  const runtime = displayIncident(snapshot)?.status.runtime;
  if (!runtime) return { tone: 'ok', state: '감시 중, 열린 장애 없음', fact };

  switch (runtime.quarantine) {
    case 'QuarantineRequested':
      return { tone: 'caution', state: '격리 요청됨', fact };
    case 'QuarantineConfirmed':
      return { tone: 'fault', state: '격리 중', fact };
    case 'QuarantineUnconfirmed':
      return { tone: 'fault', state: '격리 확인 못 함', fact };
    default:
      break;
  }
  if (runtime.phase === 'RECOVERED') return { tone: 'ok', state: '복구됨', fact };
  if (runtime.phase === 'FAILED') return { tone: 'fault', state: '운영 실패', fact };
  if (isInFlight(runtime.phase)) {
    return { tone: 'act', state: `Runtime 복구 중, ${RUNTIME_PHASE_LABEL[runtime.phase]}`, fact };
  }
  return { tone: 'caution', state: RUNTIME_PHASE_LABEL[runtime.phase], fact };
}
