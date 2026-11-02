// Deterministic per-id randomness: the layout and the animation phases are
// seeded from ids, so a constellation looks the same in every session and on
// every client, and neighbouring stars never pulse in step.

/** FNV-1a 32-bit over the UTF-16 code units of s. */
export function fnv1a(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** mulberry32: a small seeded PRNG returning floats in [0, 1). */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** unit maps an id to a stable float in [0, 1). */
export function unit(id: string): number {
  return fnv1a(id) / 4294967296;
}
