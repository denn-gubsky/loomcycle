import type { ProposedSchemeConcept } from "../api";

// proposedOutlineText writes a proposed scheme's concepts as an indented outline, each
// with the directives accepting writes for it, so the reviewer sees an imported
// notation, alternative label or exact-match IRI before it is in force. Plain text: it
// is rendered as a text child, never as HTML.
export function proposedOutlineText(outline: ProposedSchemeConcept[]): string {
  const lines: string[] = [];
  for (const c of outline) {
    const pad = "  ".repeat(Math.max(0, c.level - 1));
    lines.push(pad + "- " + c.name);
    if (c.notation) lines.push(pad + "    @notation " + c.notation);
    for (const a of c.alt ?? []) lines.push(pad + "    @alt " + a);
    if (c.exact_match) lines.push(pad + "    @exact_match " + c.exact_match);
  }
  return lines.join("\n");
}
