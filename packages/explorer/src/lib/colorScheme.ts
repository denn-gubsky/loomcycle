import type { CSSProperties } from "react";

// RFC BN per-document color schemes. A document can tint its chunk tiles and its
// Path-tree row by (document type, state) and (chunk state), so a reader scanning
// the tree sees kind + progress at a glance. A scheme is a flat map of semantic
// keys → CSS hex colors, stored in the document's ROOT chunk fields (color_scheme)
// alongside a color_enabled flag; the backend stores/returns it verbatim and this
// module is the only place that interprets it.

// ColorScheme maps a semantic key to a CSS hex color. Keys:
//   doc.<type>.<status>  — a document tile/row by its ROOT chunk's type + status
//   doc.<type>           — fallback for any status of that document type
//   doc.*.<status>       — fallback for a type the scheme does not name, by status
//   doc.*                — fallback for a type the scheme does not name, any status
//   chunk.<status>       — a chunk tile by its own status
//   chunk.inherit        — "false" (or "none" / "off") keeps a chunk without a
//                          status colour neutral; by default it takes a lighter
//                          wash of its document's colour
// Unknown keys are ignored; a missing key → no tint (neutral). Keys match a type
// or status ignoring case, so an ontology class type (`Rfc`) finds `doc.rfc.*`.
export type ColorScheme = Record<string, string>;

// DocSummary is one row of the document `documents_summary` op (Path-tree color).
export interface DocSummary {
  document_id: string;
  title: string;
  root_chunk_id?: string;
  type?: string;
  status?: string;
  color_enabled: boolean;
  color_scheme?: ColorScheme;
  // brief — the document's annotation brief, present only when the document
  // has an annotation (tenant scope). The Path tree shows it under the name.
  brief?: string;
}

// DocumentMeta is the enriched get_document result (the viewer's own document).
export interface DocumentMeta {
  document_id: string;
  title: string;
  root_chunk_id?: string;
  type?: string;
  status?: string;
  color_enabled: boolean;
  color_scheme?: ColorScheme;
}

// DEFAULT_SCHEME seeds a new document's palette. The hexes are mid-tone so the
// low-alpha wash reads on BOTH the dark and light explorer themes; the scheme
// editor starts from this, and the resolver falls back to it when a document has
// color_enabled but no stored scheme yet.
export const DEFAULT_SCHEME: ColorScheme = {
  "doc.rfc.draft": "#c99a3a",
  "doc.rfc.review": "#6a7bd6",
  "doc.rfc.confirmed": "#3a9ad6",
  "doc.rfc.done": "#3aa06a",
  // plan documents share the RFC lifecycle vocabulary (draft→review→confirmed→done).
  "doc.plan.draft": "#c99a3a",
  "doc.plan.review": "#6a7bd6",
  "doc.plan.confirmed": "#3a9ad6",
  "doc.plan.done": "#3aa06a",
  "doc.document.done": "#3aa06a",
  "doc.research.done": "#8a6ad6",
  "doc.publication.draft": "#c99a3a",
  // Any other type — an ontology class (Guide, Benchmark, …) or one added later —
  // is coloured by its status alone, so retyping a document never blanks it.
  "doc.*.draft": "#c99a3a",
  "doc.*.review": "#6a7bd6",
  "doc.*.confirmed": "#3a9ad6",
  "doc.*.done": "#3aa06a",
  "chunk.draft": "#c99a3a",
  "chunk.review": "#6a7bd6",
  "chunk.confirmed": "#3a9ad6",
  "chunk.done": "#3aa06a",
  // PR-workflow chunk statuses (a plan document's PR chunks): queued → active → done.
  "chunk.backlog": "#8a8f98",
  "chunk.implementation": "#d1863a",
  "chunk.merged": "#3aa06a",
};

// TINT_ALPHA washes every scheme color into a tile background — a tint, not a
// fill — so the row text stays readable (the loomcycle tinted-tile pattern).
const TINT_ALPHA = 0.16;

// effectiveScheme is what the resolvers read: the stored scheme if present, else
// the defaults (so color_enabled alone gives a sensible palette before any edit).
export function effectiveScheme(scheme: ColorScheme | undefined): ColorScheme {
  return scheme && Object.keys(scheme).length > 0 ? scheme : DEFAULT_SCHEME;
}

// lookup reads one scheme key: exactly, else ignoring case. Types are written
// both ways — the built-in lowercase `rfc` and an ontology's class name `Rfc` —
// and a document retyped from one to the other must keep its colour.
function lookup(scheme: ColorScheme, key: string): string | undefined {
  if (Object.prototype.hasOwnProperty.call(scheme, key) && scheme[key]) return scheme[key];
  const want = key.toLowerCase();
  for (const k of Object.keys(scheme)) {
    if (k.toLowerCase() === want && scheme[k]) return scheme[k];
  }
  return undefined;
}

// docColor resolves a document's raw tile color from its (type,status): tries
// doc.<type>.<status>, then doc.<type>, then the any-type keys doc.*.<status> and
// doc.*. Returns undefined for no match.
export function docColor(
  type: string | undefined,
  status: string | undefined,
  scheme: ColorScheme,
): string | undefined {
  const t = type ?? "";
  const s = status ?? "";
  return (
    (t && s ? lookup(scheme, `doc.${t}.${s}`) : undefined) ??
    (t ? lookup(scheme, `doc.${t}`) : undefined) ??
    (s ? lookup(scheme, `doc.*.${s}`) : undefined) ??
    lookup(scheme, "doc.*")
  );
}

// chunkColor resolves a chunk tile's raw color from its status (chunk.<status>).
export function chunkColor(status: string | undefined, scheme: ColorScheme): string | undefined {
  const s = status ?? "";
  return s ? lookup(scheme, `chunk.${s}`) : undefined;
}

// chunkInheritsDocColor reports whether a chunk with no colour of its own takes
// its document's colour. Corpus chunks (sections of a law, of a regulation)
// carry no status, and should not get a placeholder one just to be coloured —
// status is a meaningful field that search filters on — so by default they wear
// their document's colour; `chunk.inherit: "false"` turns that off.
export function chunkInheritsDocColor(scheme: ColorScheme): boolean {
  const v = String(scheme["chunk.inherit"] ?? "")
    .trim()
    .toLowerCase();
  return !(v === "false" || v === "none" || v === "off" || v === "0");
}

// hexToTint converts a #rgb / #rrggbb color to an rgba() wash at alpha
// (TINT_ALPHA by default); undefined for a value it can't parse (→ no tint).
export function hexToTint(hex: string, alpha: number = TINT_ALPHA): string | undefined {
  const m = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return undefined;
  let h = m[1];
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

// tintStyle is the inline style for a tinted row/tile: a low-alpha wash
// background + a full-color left accent. undefined (→ no style) when the color is
// absent or unparseable, so a caller spreads it unconditionally.
export function tintStyle(color: string | undefined): CSSProperties | undefined {
  if (!color) return undefined;
  const bg = hexToTint(color);
  if (!bg) return undefined;
  return { background: bg, borderLeft: `3px solid ${color}` };
}

// washStyle is the lighter tint a chunk inherits from its document: half the
// wash and no left accent, so a chunk with a status colour of its own (a
// passport block's pass / fail) still stands out among its neighbours.
export function washStyle(color: string | undefined): CSSProperties | undefined {
  if (!color) return undefined;
  const bg = hexToTint(color, TINT_ALPHA / 2);
  return bg ? { background: bg } : undefined;
}

// chunkRowStyle is a chunk-tree row's tint. The document's own row (its root
// chunk) wears the document's colour, as its Path-tree row does; any other
// chunk its status colour (chunk.<status>), else — unless the scheme turns it
// off — a lighter wash of the document's colour. scheme null = colouring off.
export function chunkRowStyle(opts: ChunkRowColorOpts): CSSProperties | undefined {
  const c = chunkRowColor(opts);
  if (!c) return undefined;
  return c.wash ? washStyle(c.color) : tintStyle(c.color);
}

interface ChunkRowColorOpts {
  status: string | undefined;
  isRoot: boolean;
  documentColor: string | undefined;
  scheme: ColorScheme | null;
}

// parseable keeps a colour only when it can be turned into a tint, so a
// fallback chain skips an unparseable value as tintStyle's undefined did.
function parseable(color: string | undefined): string | undefined {
  return color && hexToTint(color) ? color : undefined;
}

// chunkRowColor decides WHICH colour a chunk-tree row wears and whether it is
// the full tint or the lighter inherited wash. chunkRowStyle (the flat tile)
// and chunkRowTint (the tree row's gradient) both render from it, so the two
// can never disagree about which rows are coloured.
function chunkRowColor(opts: ChunkRowColorOpts): { color: string; wash: boolean } | undefined {
  const { status, isRoot, documentColor, scheme } = opts;
  if (!scheme) return undefined;
  if (isRoot) {
    const c = parseable(documentColor) ?? parseable(chunkColor(status, scheme));
    return c ? { color: c, wash: false } : undefined;
  }
  const own = parseable(chunkColor(status, scheme));
  if (own) return { color: own, wash: false };
  const inherited = chunkInheritsDocColor(scheme) ? parseable(documentColor) : undefined;
  return inherited ? { color: inherited, wash: true } : undefined;
}

// A tree row's colour is a gradient that fades out to the right, so it starts
// stronger than the flat tile wash to read as clearly at the row's left edge.
const ROW_TINT_ALPHA = 0.3;
// The wash a chunk inherits from its document. Half the full tint faded out to
// nothing in the outline, so it sits closer to it.
const ROW_WASH_ALPHA = 0.22;

// rowTint is the inline style for a coloured explorer tree row (the shared
// .etree-row). It sets only the --etree-tint custom property: the stylesheet
// paints the gradient from it and layers hover and selection above it, so an
// inline colour can never hide the selected state. wash = the lighter tint a
// chunk inherits from its document.
export function rowTint(color: string | undefined, wash = false): CSSProperties | undefined {
  if (!color) return undefined;
  const tint = hexToTint(color, wash ? ROW_WASH_ALPHA : ROW_TINT_ALPHA);
  return tint ? ({ "--etree-tint": tint } as CSSProperties) : undefined;
}

// chunkRowTint is chunkRowStyle's decision rendered as a tree-row gradient.
export function chunkRowTint(opts: ChunkRowColorOpts): CSSProperties | undefined {
  const c = chunkRowColor(opts);
  return c ? rowTint(c.color, c.wash) : undefined;
}

// statusDotColor is the colour of a row's compact status mark: the status's
// chunk.<status> colour in the given palette, else in the default palette,
// else undefined (the stylesheet's neutral dot).
export function statusDotColor(status: string | undefined, scheme?: ColorScheme): string | undefined {
  if (!status) return undefined;
  return (scheme ? parseable(chunkColor(status, scheme)) : undefined) ?? parseable(chunkColor(status, DEFAULT_SCHEME));
}

