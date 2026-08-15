// Reading a memory search result.
//
// The endpoint classifies each hit itself — `kind` is "fact" (a consolidator distilled
// it), "note" (an agent wrote it directly with `set`) or "document" (a Document chunk
// body), and a document hit carries the `chunk_id` its entity block hangs off. So this
// module does not parse keys; it decides what the console can DO with each kind.
// The KIND a hit comes back as, and the SOURCE name used to ask for it, are different
// vocabularies — singular on the result, plural on the request. Both are the server's;
// keeping them as separate types is what stops one being sent where the other is meant.
export type HitKind = "fact" | "note" | "document";
export type SearchSource = "facts" | "notes" | "documents";

export const SEARCH_SOURCES: { id: SearchSource; label: string; hint: string }[] = [
  { id: "facts", label: "facts", hint: "distilled by a consolidation pass" },
  { id: "notes", label: "notes", hint: "written directly by an agent" },
  { id: "documents", label: "documents", hint: "prose in a document" },
];

export interface SearchHit {
  key: string;
  value?: unknown;
  score: number;
  rank_score: number;
  kind: string;
  chunk_id?: string;
}

// judgeable reports whether a hit can carry a verdict.
//
// ONLY A DOCUMENT-CHUNK HIT CAN. Verification state lives on the entity tier, so a k/v
// row — the "fact" and "note" kinds — has nowhere to hold a verdict and no span to check
// one against. Offering the controls on those rows would produce a button that 404s,
// and hiding the distinction would suggest the k/v plane is verified when it is not.
export function judgeable(hit: SearchHit): boolean {
  return hit.kind === "document" && !!hit.chunk_id;
}

// hitText renders a hit's stored value for display.
//
// A k/v value arrives as raw JSON, which is usually a bare string but is not required to
// be one. Rendering `[object Object]` at an operator is worse than rendering the JSON,
// so anything that is not a string is shown as its JSON form rather than coerced.
export function hitText(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null || value === undefined) return "";
  return JSON.stringify(value);
}

// sourcesParam turns the panel's checkbox state into the wire's `sources`.
//
// ALL-SELECTED SENDS NOTHING, deliberately. The endpoint treats an empty list as "every
// source", and sending all three explicitly would mean the same thing by a longer route —
// but it would also freeze today's vocabulary into every request, so a fourth source
// added server-side would be silently excluded by a client that thinks it asked for
// everything.
export function sourcesParam(selected: Set<string>, all: string[]): string[] | undefined {
  if (selected.size === 0 || selected.size === all.length) return undefined;
  return all.filter((s) => selected.has(s));
}
