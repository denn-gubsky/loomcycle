import type { ChunkDetail } from "../types";

// A subject is a document whose ROOT chunk is the entity node: it carries a natural
// key. A keyed chunk below a root is a fact filed under a subject, not a subject, and
// re-typing it is an ordinary chunk edit.
export function isSubjectRoot(chunk: ChunkDetail | null | undefined): boolean {
  return !!chunk && !chunk.parent_id && !!chunk.entity?.natural_key;
}

// entityTypeNames reads the entity types in force out of a describe_ontology answer:
// its chunk classes, sorted. A document class types a document, never the subject a
// document is about, so it is left out. Anything unexpected reads as no types.
export function entityTypeNames(described: unknown): string[] {
  const classes = (described as { classes?: unknown } | null)?.classes;
  if (!Array.isArray(classes)) return [];
  const names = new Set<string>();
  for (const c of classes) {
    const { name, kind } = (c ?? {}) as { name?: unknown; kind?: unknown };
    if (typeof name === "string" && name !== "" && kind !== "document") names.add(name);
  }
  return [...names].sort((a, b) => a.localeCompare(b));
}

// subjectTypeChoices is what the change-type picker offers: the types in force, and
// the subject's current type even when it is not one of them, so the picker can show
// where the subject stands rather than silently preselecting something else.
export function subjectTypeChoices(inForce: string[], current: string | undefined): string[] {
  const cur = current ?? "";
  if (cur === "" || inForce.some((t) => t.toLowerCase() === cur.toLowerCase())) return inForce;
  return [cur, ...inForce];
}
