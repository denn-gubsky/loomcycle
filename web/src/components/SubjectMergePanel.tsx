import { useState } from "react";
import { mergeSubjects, type MergeSubjectsResult, type Principal } from "../api";
import ErrorNotice from "./ErrorNotice";

// SubjectMergePanel is the operator's "merge into another subject" action on a
// tenant document's page. The server decides whether the open document is a
// subject at all (and refuses `not_a_subject` otherwise), so the panel does not
// re-derive it; it is offered only where a merge can be asked for — a tenant
// document, to an operator.

/** Whether p may be offered a merge of a document in scope: an admin or a
 *  tenant operator, on a tenant-scope document. The server checks again. */
export function canMergeSubjects(p: Principal | null | undefined, scope: string): boolean {
  if (scope !== "tenant" || !p) return false;
  return p.is_admin || p.scopes.includes("substrate:tenant");
}

/** The sentence a finished merge reports. */
export function mergeSummary(r: MergeSubjectsResult): string {
  const parts = [
    `Merged ${r.duplicate.natural_key} into ${r.survivor.natural_key}`,
    `${r.facts_moved} fact${r.facts_moved === 1 ? "" : "s"} moved`,
    `${r.statements_repointed} statement${r.statements_repointed === 1 ? "" : "s"} re-pointed`,
  ];
  if (r.statements_retired > 0) parts.push(`${r.statements_retired} retired`);
  if (r.statements_left > 0) parts.push(`${r.statements_left} left for the links agent`);
  return parts.join(", ") + ".";
}

export default function SubjectMergePanel({
  documentId,
  tenant,
  onMerged,
}: {
  documentId: string;
  tenant?: string;
  onMerged?: (r: MergeSubjectsResult) => void;
}) {
  const [survivor, setSurvivor] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<MergeSubjectsResult | null>(null);
  const [error, setError] = useState<unknown>(null);

  async function merge() {
    const target = survivor.trim();
    if (!target) return;
    // A merge re-points every statement about this subject, tenant-wide, and
    // no other call undoes it: ask once more.
    if (!window.confirm(`Merge this subject into ${target}? Its facts and statements move to that subject; this one stays as a retired pointer.`)) {
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const r = await mergeSubjects(documentId, target, tenant);
      setResult(r);
      onMerged?.(r);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <details className="subject-merge">
      <summary>Merge this subject into another…</summary>
      <p className="settings-muted">
        When this subject and another name the same entity, merge this one into the one that stays. Its
        facts move there, its statements are re-pointed there (the originals kept, expired), and this
        document stays as a retired pointer to the survivor.
      </p>
      <div className="subject-merge-row">
        <input
          type="text"
          value={survivor}
          placeholder="Survivor: document id or natural key (person:ada-lovelace)"
          onChange={(e) => setSurvivor(e.target.value)}
          disabled={busy || result !== null}
        />
        <button type="button" onClick={merge} disabled={busy || result !== null || survivor.trim() === ""}>
          {busy ? "Merging…" : "Merge"}
        </button>
      </div>
      {result && (
        <div className="subject-merge-result" role="status">
          {mergeSummary(result)}
          {result.not_announced && <div className="settings-muted">Not announced: {result.not_announced}</div>}
        </div>
      )}
      <ErrorNotice error={error} />
    </details>
  );
}
