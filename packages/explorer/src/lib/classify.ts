import type {
  AcceptClassificationAnswer,
  ClassificationRecord,
  ClassifyAnswer,
  ClassifyCandidate,
  ClassifyStep,
} from "../types";

// Display helpers for RFC FO classification (the classify and
// accept_classification Document ops). Pure, so what the viewer shows is
// testable without rendering it.

// PendingClassification is what waits in a root chunk's classification record
// for an operator to accept.
export interface PendingClassification {
  proposedType?: string;
  proposedTags: string[];
  statementCount: number;
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}

// classificationRecord reads `fields.classification` off a root chunk's fields.
// The record lives in ordinary chunk fields, which any writer of the document may
// set, so it is read defensively: a field of the wrong shape is dropped rather
// than trusted, and anything that is not an object reads as no record.
export function classificationRecord(fields: unknown): ClassificationRecord | undefined {
  if (!fields || typeof fields !== "object") return undefined;
  const raw = (fields as { classification?: unknown }).classification;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const r = raw as Record<string, unknown>;
  const rec: ClassificationRecord = {};
  if (typeof r.by === "string") rec.by = r.by;
  if (typeof r.at === "number") rec.at = r.at;
  if (typeof r.applied_type === "string" && r.applied_type) rec.applied_type = r.applied_type;
  if (typeof r.proposed_type === "string" && r.proposed_type) rec.proposed_type = r.proposed_type;
  if (r.proposed_fields && typeof r.proposed_fields === "object" && !Array.isArray(r.proposed_fields)) {
    rec.proposed_fields = r.proposed_fields as Record<string, unknown>;
  }
  if (isStringArray(r.proposed_tags)) rec.proposed_tags = r.proposed_tags;
  if (isStringArray(r.statements)) rec.statements = r.statements;
  return rec;
}

// pendingClassification is the runtime's own "nothing_pending" test turned
// around: a record is pending when it proposes a type, names statements, or
// proposes tags. undefined when nothing waits (an applied-only record included).
export function pendingClassification(fields: unknown): PendingClassification | undefined {
  const rec = classificationRecord(fields);
  if (!rec) return undefined;
  const proposedTags = rec.proposed_tags ?? [];
  const statementCount = rec.statements?.length ?? 0;
  if (!rec.proposed_type && proposedTags.length === 0 && statementCount === 0) return undefined;
  return { proposedType: rec.proposed_type, proposedTags, statementCount };
}

// resultLabel says what a classify answer decided. A `report` answer wrote
// nothing, so it says what an Apply would do; a `propose` answer says what was
// done.
export function resultLabel(a: Pick<ClassifyAnswer, "result" | "write">): string {
  const report = a.write === "report";
  switch (a.result) {
    case "classified":
      return report ? "would be classified" : "classified";
    case "proposed":
      return report ? "would be proposed for review" : "proposed — waits for review";
    case "kept":
      return "kept — the type was set by hand";
    case "unclassified":
      return "unclassified — nothing decided";
    default:
      return String(a.result);
  }
}

// classDisposition says what became (or would become) of the class: applied to
// the document live, or only proposed. undefined when no class was chosen. A
// kept answer still names the class classify would choose, but never applies it.
export function classDisposition(
  a: Pick<ClassifyAnswer, "class" | "live" | "result" | "write">,
): string | undefined {
  if (!a.class) return undefined;
  if (a.result === "kept") return "not applied";
  const report = a.write === "report";
  if (a.live) return report ? "would be applied" : "applied";
  return report ? "would be proposed" : "proposed";
}

// canApply: a report answer that a propose run would act on — a class to apply
// or propose, or concepts / issuers to file. A propose answer has already been
// applied, and an empty unclassified or kept report has nothing to write.
export function canApply(a: ClassifyAnswer): boolean {
  if (a.write !== "report") return false;
  if (a.result === "classified" || a.result === "proposed") return true;
  return (a.concepts?.length ?? 0) > 0 || (a.issued_by?.length ?? 0) > 0;
}

// formatShare renders a 0..1 share as a whole percentage.
export function formatShare(share: number | undefined): string {
  if (share == null || !Number.isFinite(share)) return "";
  return `${Math.round(share * 100)}%`;
}

const STEP_LABELS: Record<string, string> = {
  rules: "Rules",
  vote: "Neighbourhood vote",
  model: "Model",
};

// stepLabel names a step; an unknown step name (a newer runtime) shows verbatim.
export function stepLabel(step: string): string {
  return STEP_LABELS[step] ?? step;
}

// candidateDetail is the one-line explanation of a candidate: the quote a rule
// matched and where, or a vote's count and share.
export function candidateDetail(c: ClassifyCandidate): string {
  const parts: string[] = [];
  if (c.quote) parts.push(c.from ? `“${c.quote}” (${c.from})` : `“${c.quote}”`);
  if (c.votes != null) parts.push(`${c.votes} vote${c.votes === 1 ? "" : "s"}`);
  const share = formatShare(c.share);
  if (share) parts.push(share);
  return parts.join(" · ");
}

// StepSummary is one step reduced to what the panel shows on its line; the
// candidates go behind a collapsible.
export interface StepSummary {
  label: string;
  chosen?: string;
  method?: string;
  neighbours?: number;
  reason?: string;
  candidates: { class: string; detail: string }[];
}

export function summarizeStep(s: ClassifyStep): StepSummary {
  return {
    label: stepLabel(s.step),
    ...(s.chosen ? { chosen: s.chosen } : {}),
    ...(s.method ? { method: s.method } : {}),
    ...(s.neighbours ? { neighbours: s.neighbours } : {}),
    ...(s.reason ? { reason: s.reason } : {}),
    candidates: (s.candidates ?? []).map((c) => ({ class: c.class, detail: candidateDetail(c) })),
  };
}

// acceptSummary reduces an accept_classification answer to a headline of what
// became live, plus a notice per thing that did not: a class left pending or not
// applied, skipped statements or tags, and the runtime's warnings.
export function acceptSummary(a: AcceptClassificationAnswer): { applied: string; notices: string[] } {
  const done: string[] = [];
  if (a.type) done.push(`type ${a.type}`);
  const n = a.accepted_statements?.length ?? 0;
  if (n > 0) done.push(`${n} statement${n === 1 ? "" : "s"}`);
  if (a.tags && a.tags.length > 0) done.push(`tags ${a.tags.join(", ")}`);
  const notices: string[] = [];
  if (a.type_pending) {
    notices.push("The proposed class stays pending: another document already holds its identity.");
  }
  if (a.skipped_type) {
    notices.push(`Class ${a.skipped_type} was not applied: it is not a declared document class.`);
  }
  const skipped = a.skipped_statements?.length ?? 0;
  if (skipped > 0) {
    notices.push(
      `${skipped} statement${skipped === 1 ? " was" : "s were"} skipped (already ruled on, or not this document's proposal).`,
    );
  }
  if (a.skipped_tags && a.skipped_tags.length > 0) {
    notices.push(`Tags not applied (not concepts of the ontology): ${a.skipped_tags.join(", ")}.`);
  }
  notices.push(...(a.warnings ?? []));
  return { applied: done.length > 0 ? `Accepted ${done.join(", ")}.` : "Nothing was accepted.", notices };
}

// classifyErrorText turns a refused op into the line the panel shows. The
// runtime's nothing_pending business error gets plain words; anything else
// (a permission refusal, an unconfirmed ontology) shows the runtime's own text.
export function classifyErrorText(e: unknown): string {
  const msg = e instanceof Error ? e.message : String(e);
  if (msg.includes("nothing_pending")) return "Nothing is pending for this document — it may already have been accepted.";
  return msg;
}
