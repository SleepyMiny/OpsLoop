import { Instrument } from '../shared/primitives';
import type { RegionProps } from '../types';

/** Placeholder until the owning agent implements this region. */
export function ClusterMimic({ emphasized }: RegionProps) {
  return (
    <Instrument title="Hybrid k3s 클러스터" emphasized={emphasized ?? false}>
      <p>준비 중</p>
    </Instrument>
  );
}
