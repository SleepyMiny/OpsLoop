import { Instrument } from '../shared/primitives';
import type { RegionProps } from '../types';

/** Placeholder until the owning agent implements this region. */
export function PolicyPanel({ emphasized }: RegionProps) {
  return (
    <Instrument title="RecoveryPolicy" emphasized={emphasized ?? false}>
      <p>준비 중</p>
    </Instrument>
  );
}
