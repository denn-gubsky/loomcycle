// ontologySections — formatting for the sectioned ontology (RFC FO) in the
// Settings → Ontology panel. Pure so it is tested without rendering.

import type { OntologyFieldSpec, OntologyResponse, OntologyTerm } from "../api";

// typedFieldLabel renders one typed field the way an operator wrote it:
// `rank: concept(normative-rank), required`. A plain text field with no flags
// is just its name, so an ontology that never typed anything reads as before.
export function typedFieldLabel(f: OntologyFieldSpec): string {
  let type = "";
  if (f.type === "enum") type = `enum(${(f.enum ?? []).join(", ")})`;
  else if (f.type === "concept" || f.type === "ref") type = `${f.type}(${f.range ?? ""})`;
  else if (f.type && f.type !== "text") type = f.type;
  const flags = [f.required && "required", f.identity && "identity", f.unique && "unique"].filter(
    Boolean,
  ) as string[];
  const tail = [type, ...flags].filter(Boolean).join(", ");
  return tail ? `${f.name}: ${tail}` : f.name;
}

// classDirectiveNotes lists the class directives a term carries (declared or
// inherited), for the one-line summary under the class name. Inherited ones are
// marked so the panel never claims a declaration the class did not write.
export function classDirectiveNotes(t: OntologyTerm): string[] {
  const out: string[] = [];
  const declared: Record<string, string | undefined> = {
    identifier_pattern: t.identifier_pattern,
    title_pattern: t.title_pattern,
    anchor_grammar: t.anchor_grammar,
    citation_grammar: t.citation_grammar,
    exclude: t.exclude?.join(", "),
  };
  const labels: Record<string, string> = {
    identifier_pattern: "identifier pattern",
    title_pattern: "title pattern",
    anchor_grammar: "anchors",
    citation_grammar: "citations",
    exclude: "excludes",
  };
  for (const key of Object.keys(labels)) {
    const own = declared[key];
    const inherited = t.inherited_directives?.[key];
    if (own) out.push(`${labels[key]}: ${own}`);
    else if (inherited) out.push(`${labels[key]}: ${inherited} (inherited)`);
  }
  if (t.title_identity) out.push("title is its identity");
  else if (t.inherited_title_identity) out.push("title is its identity (inherited)");
  if (t.renamed_from) out.push(`renamed from ${t.renamed_from}`);
  return out;
}

// factsUnderOlderVocabulary counts the tenant-scope facts written under an
// ontology version older than the current one. Facts with no recorded version
// (0) are counted separately — "unknown" is not "older".
export function factsUnderOlderVocabulary(r: OntologyResponse): { older: number; unknown: number } {
  const current = r.ontology_version ?? 0;
  let older = 0;
  let unknown = 0;
  for (const v of r.fact_versions ?? []) {
    if (v.version === 0) unknown += v.count;
    else if (v.version < current) older += v.count;
  }
  return { older, unknown };
}
