import type { VerificationStats } from "../api";

// What the coverage panel should say about a scope, decided in one place.
//
// EXTRACTED FOR THE SAME REASON THE ONTOLOGY HREF WAS: the risky part is not the
// rendering, it is a number that is wrong in a way that looks fine. Two specific ones:
//
//   - an EMPTY store must not report "0% verified". The server omits `verified_share`
//     rather than dividing by zero, and a UI that computed its own would print 0%,
//     which reads as "nothing here is verified" — a different claim from "there is
//     nothing to verify", and one an operator would act on.
//   - "0 judged" must come with its reason. The overwhelmingly likely cause is that
//     verification was never enabled, and a bare zero reads as broken.
export interface CoverageSummary {
  /** No facts at all — show the "nothing stored yet" line and no numbers. */
  empty: boolean;
  /** Integer percent, or null when the server reported no share (an empty store). */
  sharePct: number | null;
  /** Facts exist but none carry a verdict — say why before someone reads it as a fault. */
  showUnjudgedNote: boolean;
  /** Something is withheld — say that withheld means hidden, not deleted. */
  showWithheldNote: boolean;
}

export function verificationSummary(stats: VerificationStats | null): CoverageSummary {
  if (!stats || stats.facts <= 0) {
    return { empty: true, sharePct: null, showUnjudgedNote: false, showWithheldNote: false };
  }
  return {
    empty: false,
    // Rendered from the SERVER's share, never recomputed here: two definitions of
    // "verified" would eventually disagree, and the server's is the one the gate uses.
    sharePct: stats.verified_share === undefined ? null : Math.round(stats.verified_share * 100),
    showUnjudgedNote: (stats.judged ?? 0) === 0,
    showWithheldNote: (stats.withheld ?? 0) > 0,
  };
}
