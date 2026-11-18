import type { ProposedChange } from "../api";

// proposedChangeText is what a change or retire proposal (RFC GQ P4.1) would do to the
// entry it names, one line per change, as plain text (rendered in a <pre>, so markup in
// a name or value stays text).
export function proposedChangeText(c: ProposedChange): string {
  if (c.op === "retire") {
    return `retire the ${c.kind} ${c.target_name}: nothing new may use it; what uses it stays`;
  }
  const lines: string[] = [];
  if (c.name) lines.push(`rename ${c.target_name} to ${c.name}`);
  if (c.parent) lines.push(`move under ${c.parent_name || "the section"}`);
  if (c.meaning) lines.push(`meaning: ${c.meaning}`);
  if (c.sets_directives) {
    const ds = c.directives ?? [];
    lines.push(
      ds.length === 0 ? "directives: none (clears them)" : "directives:",
    );
    for (const d of ds)
      lines.push(`    @${d.name}${d.value ? " " + d.value : ""}`);
  }
  return lines.join("\n");
}
