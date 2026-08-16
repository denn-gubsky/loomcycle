import type { VerificationStats } from "../types";

// What the coverage strip should say, decided in one place.
//
// The risky part is not the rendering, it is a number that is wrong in a way that looks
// fine. Specifically: an EMPTY store must not report "0% verified". The server omits
// `verified_share` rather than dividing by zero, and a client computing its own would
// print 0% — which reads as "nothing here is verified", a different claim from "there is
// nothing to verify", and one an operator acts on differently.
export interface CoverageSummary {
  /** Nothing stored — render nothing at all rather than a row of zeroes. */
  empty: boolean;
  /** Integer percent, or null when the server reported no share. */
  sharePct: number | null;
  /** Facts exist but none carry a verdict — say why before someone reads it as a fault. */
  showUnjudgedNote: boolean;
}

export function coverageSummary(stats: VerificationStats | null | undefined): CoverageSummary {
  if (!stats || stats.facts <= 0) {
    return { empty: true, sharePct: null, showUnjudgedNote: false };
  }
  return {
    empty: false,
    // Rendered FROM the server's share, never recomputed here: two definitions of
    // "verified" would eventually disagree, and the server's is the one the gate uses.
    sharePct: stats.verified_share === undefined ? null : Math.round(stats.verified_share * 100),
    showUnjudgedNote: (stats.judged ?? 0) === 0,
  };
}
