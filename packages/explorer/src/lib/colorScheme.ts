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
//   chunk.<status>       — a chunk tile by its own status
// Unknown keys are ignored; a missing key → no tint (neutral).
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

// docColor resolves a document's raw tile color from its (type,status): tries
// doc.<type>.<status>, then doc.<type>. Returns undefined for no match.
export function docColor(
  type: string | undefined,
  status: string | undefined,
  scheme: ColorScheme,
): string | undefined {
  const t = type ?? "";
  const s = status ?? "";
  if (t && s && scheme[`doc.${t}.${s}`]) return scheme[`doc.${t}.${s}`];
  if (t && scheme[`doc.${t}`]) return scheme[`doc.${t}`];
  return undefined;
}

// chunkColor resolves a chunk tile's raw color from its status (chunk.<status>).
export function chunkColor(status: string | undefined, scheme: ColorScheme): string | undefined {
  const s = status ?? "";
  return s ? scheme[`chunk.${s}`] : undefined;
}

// hexToTint converts a #rgb / #rrggbb color to an rgba() wash at TINT_ALPHA;
// undefined for a value it can't parse (→ no tint applied).
export function hexToTint(hex: string): string | undefined {
  const m = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return undefined;
  let h = m[1];
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  const r = parseInt(h.slice(0, 2), 16);
  const g = parseInt(h.slice(2, 4), 16);
  const b = parseInt(h.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${TINT_ALPHA})`;
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
