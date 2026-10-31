// ontologySections — formatting for the sectioned ontology (RFC FO) in the
// Settings → Ontology panel. Pure so it is tested without rendering.

import type {
  OntologyConcept,
  OntologyInstanceState,
  OntologyFieldSpec,
  OntologyRelation,
  OntologyResponse,
  OntologyTerm,
} from "../api";

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
    definitions: t.definitions,
    exclude: t.exclude?.join(", "),
  };
  const labels: Record<string, string> = {
    identifier_pattern: "identifier pattern",
    title_pattern: "title pattern",
    anchor_grammar: "anchors",
    citation_grammar: "citations",
    definitions: "definitions",
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

// relationSummary is the one-line description of a relation kind under its name in the
// panel: direction, the classes at either end, characteristics, qualifiers and flags.
export function relationSummary(r: OntologyRelation): string {
  const parts: string[] = [];
  if (r.symmetric) parts.push("either direction");
  else if (r.inverse) parts.push(`inverse ${r.inverse}`);
  if ((r.domain ?? []).length || (r.range ?? []).length) {
    parts.push(`${(r.domain ?? []).join("|") || "anything"} → ${(r.range ?? []).join("|") || "anything"}`);
  }
  const traits = [
    r.asymmetric && "asymmetric",
    r.irreflexive && "irreflexive",
    r.functional && "one target per source",
    r.transitive && "transitive",
    r.transitive_within && `transitive within ${r.transitive_within}`,
    r.dated && "dated",
  ].filter(Boolean) as string[];
  parts.push(...traits);
  if ((r.qualifiers ?? []).length) {
    const qs = (r.qualifiers ?? []).map((q) =>
      q.values?.length ? `${q.name}(${q.values.join("|")})` : q.name,
    );
    parts.push(`qualifiers ${qs.join(", ")}`);
  }
  if (r.discriminator) parts.push(`distinct per ${r.discriminator}`);
  if (r.walk) parts.push("walked");
  if (r.auto_assert) parts.push("auto-asserted");
  if (r.renamed_from) parts.push(`renamed from ${r.renamed_from}`);
  return parts.join(" · ");
}

// buildRelationTree nests relation kinds under their parent kind. A kind whose parent is
// not in the list (a standard kind's parent shown elsewhere, say) is a root rather than
// dropped — a kind missing from the panel is one the operator cannot see is in force.
export interface RelationNode extends OntologyRelation {
  children: RelationNode[];
}

export function buildRelationTree(relations: OntologyRelation[]): RelationNode[] {
  const byName = new Map<string, RelationNode>();
  relations.forEach((r) => byName.set(r.name, { ...r, children: [] }));
  const roots: RelationNode[] = [];
  byName.forEach((n) => {
    const parent = n.parent ? byName.get(n.parent) : undefined;
    if (parent && parent !== n) parent.children.push(n);
    else roots.push(n);
  });
  const sort = (ns: RelationNode[]) => {
    ns.sort((a, b) => a.name.localeCompare(b.name));
    ns.forEach((n) => sort(n.children));
  };
  sort(roots);
  return roots;
}

// conceptSummary is the one-line description of a concept in the panel: its tag (its
// identity across scopes), notation, rank, aliases, and — on a file plan — the policy it
// carries, labelled as not yet enforced so no operator relies on it.
export function conceptSummary(c: OntologyConcept): string {
  const parts: string[] = [c.tag];
  if (c.notation) parts.push(c.notation);
  if (c.rank) parts.push(`rank ${c.rank}`);
  if ((c.alt ?? []).length) parts.push(`also ${(c.alt ?? []).join(", ")}`);
  const policy = [c.retention && `retention ${c.retention}`, c.acl && `access ${c.acl}`].filter(
    Boolean,
  ) as string[];
  if (policy.length) parts.push(`${policy.join(", ")} (not enforced yet)`);
  return parts.join(" · ");
}

// instanceStateLabel says what a confirm does (or did) for one instance, in operator
// words rather than the wire state.
export function instanceStateLabel(s: OntologyInstanceState): string {
  switch (s.state) {
    case "pending":
      return "adopted on the next confirm";
    case "adopted":
      return "adopted";
    case "linked":
      return "matched to a subject memory already knew";
    case "renamed":
      return "renamed — keeps its key";
    case "unchanged":
      return "in the registry";
    default:
      return s.note ? `not adopted: ${s.note}` : "not adopted";
  }
}

// ─── The page's section tabs ─────────────────────────────────────────────────

export type OntologyTabId =
  | "classes"
  | "document-classes"
  | "relations"
  | "schemes"
  | "instances"
  | "grammars"
  | "health";

export interface OntologyTab {
  id: OntologyTabId;
  label: string;
  /** What the tab holds, shown beside its label. */
  count: number;
  /** Something in it needs the operator: a disabled grammar, a stale concept tag… */
  warn: boolean;
}

// ontologyTabs lists the page's tabs for one ontology: Classes always (a section-less
// document is all classes), every other section only when the document declares it or
// the server reported it, and Health only when it has a finding. A tab for a section
// the document lacks would be an empty page that reads as data lost.
export function ontologyTabs(r: OntologyResponse): OntologyTab[] {
  const tabs: OntologyTab[] = [];
  const termWarn = (ts: OntologyTerm[] | undefined) =>
    (ts ?? []).some((t) => (t.issues ?? []).length > 0 || !!t.name_issue);
  tabs.push({
    id: "classes",
    label: "classes",
    count: (r.effective ?? []).length,
    warn: termWarn(r.terms),
  });
  if ((r.document_classes ?? []).length > 0) {
    tabs.push({
      id: "document-classes",
      label: "document classes",
      count: (r.document_classes ?? []).length,
      warn: termWarn(r.document_classes),
    });
  }
  if (r.relations !== undefined) {
    tabs.push({
      id: "relations",
      label: "relations",
      count: (r.relations ?? []).length,
      warn: (r.relations ?? []).some((x) => (x.issues ?? []).length > 0 || !!x.name_issue),
    });
  }
  if (r.schemes !== undefined) {
    tabs.push({
      id: "schemes",
      label: "schemes",
      count: (r.schemes ?? []).length,
      warn:
        (r.scheme_mirror_stale ?? []).length > 0 ||
        (r.schemes ?? []).some(
          (s) => (s.issues ?? []).length > 0 || (s.concepts ?? []).some((c) => (c.issues ?? []).length > 0),
        ),
    });
  }
  if (r.instances !== undefined) {
    const list = r.instances?.instances ?? [];
    tabs.push({
      id: "instances",
      label: "instances",
      count: list.length,
      warn: list.some((i) => i.state === "error"),
    });
  }
  if ((r.grammars ?? []).length > 0) {
    tabs.push({
      id: "grammars",
      label: "grammars",
      count: (r.grammars ?? []).length,
      warn: (r.grammars ?? []).some((g) => !!g.issue),
    });
  }
  const findings = ontologyHealthFindings(r);
  if (findings > 0) tabs.push({ id: "health", label: "health", count: findings, warn: true });
  return tabs;
}

// ontologyHealthFindings counts what the Health tab reports: types in use that no class
// declares, facts written under an older or unknown vocabulary, and undated statements
// of a dated relation kind. Each kind counts once, however many rows it covers.
export function ontologyHealthFindings(r: OntologyResponse): number {
  const v = factsUnderOlderVocabulary(r);
  return (
    ((r.undeclared_types ?? []).length > 0 ? 1 : 0) +
    (v.older > 0 || v.unknown > 0 ? 1 : 0) +
    ((r.undated_statements ?? 0) > 0 ? 1 : 0)
  );
}

// pickOntologyTab resolves the tab a URL asks for against the tabs this ontology has:
// an unknown or absent one (a link from before a section was removed) opens Classes.
export function pickOntologyTab(requested: string | null, tabs: OntologyTab[]): OntologyTabId {
  return tabs.find((t) => t.id === requested)?.id ?? "classes";
}
