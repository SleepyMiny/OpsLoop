import { Instrument } from '../shared/primitives';
import type { RegionProps } from '../types';

/** Placeholder until the owning agent implements this region. */
export function PipelineLadder({ emphasized }: RegionProps) {
  return (
    <Instrument title="복구 단계" emphasized={emphasized ?? false}>
      <p>준비 중</p>
    </Instrument>
  );
}
