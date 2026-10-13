import { Instrument } from '../shared/primitives';
import type { RegionProps } from '../types';

/** Placeholder until the owning agent implements this region. */
export function CandidateBoard({ emphasized }: RegionProps) {
  return (
    <Instrument title="복구 후보 평가" emphasized={emphasized ?? false}>
      <p>준비 중</p>
    </Instrument>
  );
}
