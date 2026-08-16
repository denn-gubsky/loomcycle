// Reading the retention report.
//
// THE MISLEADING NUMBER THIS EXISTS TO PREVENT. `purgeable` is a preview computed
// REGARDLESS OF MODE — it answers "how much would the current age settings match", not
// "how much is about to be deleted". A family set to `off` deletes nothing, and showing
// "38 purgeable" beside it reads as a countdown. The inverse is worse: a family set to
// `prune` with rows matching IS deleting data on the next sweep, and that deserves to
// look different from a preview.
//
// So a count is only ever rendered together with what its family's mode will actually do
// with it.
export type RetentionMode = "off" | "prune" | "export+prune" | string;

export interface RetentionFamily {
  key: string;
  label: string;
  mode: RetentionMode;
  maxAgeMs: number;
  /** Preview count, when the caller is allowed to see one. */
  purgeable?: number;
}

export type FamilyEffect = "inert" | "deletes" | "exports-then-deletes";

// familyEffect says what this family's mode DOES, in the terms an operator cares about.
export function familyEffect(mode: RetentionMode): FamilyEffect {
  switch (mode) {
    case "prune":
      return "deletes";
    case "export+prune":
      return "exports-then-deletes";
    default:
      // Anything unrecognised is treated as inert. A future mode this UI does not know
      // must not be announced as deleting data — the safe reading of an unknown is that
      // we cannot say it deletes, and the raw mode string is shown alongside anyway.
      return "inert";
  }
}

// countMeaning is what a purgeable count means for THIS family. The count alone is
// ambiguous; paired with the mode it is not.
export function countMeaning(mode: RetentionMode, purgeable?: number): string {
  if (purgeable === undefined) return "";
  if (purgeable === 0) return "nothing matches the current age";
  return familyEffect(mode) === "inert"
    ? `${purgeable} would match — but this family is off, so nothing is deleted`
    : `${purgeable} will be removed on the next sweep`;
}

// exportMisconfigured reports the one combination that silently does nothing: a family
// set to export-then-delete with no export directory configured. The sweeper refuses to
// run it, so the operator's data is neither exported nor pruned — and the report is the
// only place that is visible.
export function exportMisconfigured(mode: RetentionMode, exportDir: string | undefined): boolean {
  return familyEffect(mode) === "exports-then-deletes" && !exportDir;
}
