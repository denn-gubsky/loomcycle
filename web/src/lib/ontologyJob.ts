import type { OntologyJobStatus } from "../api";

// The ontology page's job panel (RFC GQ P5.6): how a class rename or concept re-tag
// job reads while it carries a change through the tenant's stored rows. Pure, so the
// wording an operator decides from is tested without mounting the page.

/** A job that has more work to do: the panel offers to resume it. */
export function jobUnfinished(job: OntologyJobStatus): boolean {
  return job.state === "pending" || job.state === "running" || job.state === "blocked";
}

/** What the job carries, e.g. "Class rename: Bulletin → Circular". A re-tag names each
 *  old tag with its new one. */
export function jobTitle(job: OntologyJobStatus): string {
  if (job.kind === "concept_retag") {
    const tags = job.to_tags ?? [];
    const moves = job.from.map((f, i) => `${f} → ${tags[i] ?? "?"}`);
    return `Concept re-tag: ${moves.join(", ")}`;
  }
  if (job.kind === "class_rename") {
    return `Class rename: ${job.from.join(" / ")} → ${job.to}`;
  }
  return `${job.kind}: ${job.from.join(", ")} → ${job.to}`;
}

/** The job's state, in the operator's words. */
export function jobStateLabel(state: string): string {
  switch (state) {
    case "pending":
      return "waiting to start";
    case "running":
      return "in progress";
    case "blocked":
      return "blocked";
    case "done":
      return "finished";
    case "abandoned":
      return "abandoned (its change never reached the ontology)";
    default:
      return state || "unknown";
  }
}

/** Where an unfinished job is, e.g. "step chunks_type in the tenant scope · 1 of 3
 *  scopes done · 12 rows left in this scope". A finished job says how many scopes it
 *  walked; any figure the server did not give is left out. */
export function jobProgress(job: OntologyJobStatus): string {
  const parts: string[] = [];
  const done = job.scopes_done ?? 0;
  if (!jobUnfinished(job)) {
    if (job.state === "done" && job.scopes_total) parts.push(`${job.scopes_total} scope${job.scopes_total === 1 ? "" : "s"} walked`);
  } else {
    if (job.step) parts.push(job.scope ? `step ${job.step} in the ${job.scope} scope` : `step ${job.step}`);
    if (job.scopes_total) parts.push(`${done} of ${job.scopes_total} scopes done`);
    if (job.remaining > 0) parts.push(`${job.remaining} row${job.remaining === 1 ? "" : "s"} left in this scope`);
  }
  const moved = Object.values(job.done ?? {}).reduce((a, b) => a + b, 0);
  if (moved > 0) parts.push(`${moved} row${moved === 1 ? "" : "s"} rewritten`);
  return parts.join(" · ");
}
