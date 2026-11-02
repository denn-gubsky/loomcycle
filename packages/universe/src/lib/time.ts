/** toMs reads a runtime timestamp in any of s / ms / µs / ns as milliseconds.
 *  The document ops stamp unix nanoseconds, older rows and fixtures use coarser
 *  units, and the magnitudes do not overlap for any date this side of 2200. */
export function toMs(t: number): number {
  return t > 1e17 ? t / 1e6 : t > 1e14 ? t / 1e3 : t > 1e11 ? t : t * 1e3;
}
