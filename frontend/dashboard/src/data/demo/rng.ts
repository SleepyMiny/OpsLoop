/** mulberry32: tiny seeded PRNG so every replay of a scenario is identical. */
export function createRng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Deterministic short hex id, e.g. for Pod UIDs and digests in Demo Mode. */
export function hexId(rng: () => number, length: number): string {
  let out = '';
  while (out.length < length) out += Math.floor(rng() * 16).toString(16);
  return out;
}
