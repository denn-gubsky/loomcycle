import { useEffect, useState } from "react";
import type { MemoryScope, VerificationStats } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import { coverageSummary } from "../lib/coverage";

// CoverageBar — how much of this scope's fact store is actually verified (RFC CC).
//
// It sits above the list it summarises because the two are unreadable apart: a row's
// "refused" badge means little without knowing whether anything is judged at all, and
// the share means little without the rows behind it.
//
// THE TWO UNVERIFIED POPULATIONS STAY SEPARATE. A fact with no span can never be
// verified by anyone — the transcript it came from may be gone — while one merely
// awaiting a judge can. Rolling them together would make the backlog look like something
// a pass can fix.
export default function CoverageBar({
  scope,
  scopeId,
  reloadKey,
}: {
  scope: MemoryScope;
  scopeId?: string;
  reloadKey?: number;
}) {
  const data = useMemoryData();
  const [stats, setStats] = useState<VerificationStats | null>(null);
  const [unavailable, setUnavailable] = useState(false);

  useEffect(() => {
    let cancelled = false;
    data
      .verificationStats(scope, scopeId ? { scopeId } : undefined)
      .then((s) => {
        if (!cancelled) {
          setStats(s);
          setUnavailable(false);
        }
      })
      .catch(() => {
        // An older runtime has no such op. A missing coverage strip is not worth an
        // error banner over a list that works — it just does not render.
        if (!cancelled) setUnavailable(true);
      });
    return () => {
      cancelled = true;
    };
  }, [data, scope, scopeId, reloadKey]);

  // Every "what should this say" decision lives in one tested place — see the helper
  // for which numbers are misleading when rendered naively.
  const summary = coverageSummary(stats);
  if (unavailable || summary.empty || !stats) return null;
  return (
    <div className="fact-coverage">
      <span>
        <strong>{stats.facts}</strong> facts
      </span>
      <span>
        <strong>{stats.supported ?? 0}</strong> verified
        {/* Rendered from the SERVER's share, never recomputed: two definitions of
            "verified" would eventually disagree, and the server's is the one the
            gate uses. Absent on an empty store rather than shown as 0%. */}
        {summary.sharePct !== null && ` (${summary.sharePct}%)`}
      </span>
      {(stats.withheld ?? 0) > 0 && (
        <span title="withheld from recall, never deleted — tick “include refused” to read them">
          <strong>{stats.withheld}</strong> refused
        </span>
      )}
      {(stats.awaiting_judge ?? 0) > 0 && (
        <span title="has a span, no verdict yet — a consolidation pass will judge these">
          <strong>{stats.awaiting_judge}</strong> awaiting a judge
        </span>
      )}
      {(stats.unverifiable_no_span ?? 0) > 0 && (
        <span title="no source span — nothing can check these against a source; only a person can vouch for them">
          <strong>{stats.unverifiable_no_span}</strong> unverifiable
        </span>
      )}
      {summary.showUnjudgedNote && (
        <span className="fact-coverage-note">
          nothing judged yet — verdicts are only recorded when the deployment enables
          verified writes
        </span>
      )}
    </div>
  );
}
