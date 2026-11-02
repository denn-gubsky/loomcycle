import { DEFAULT_SCHEME, chunkColor, docColor, effectiveScheme } from "@loomcycle/explorer";
import { fnv1a } from "./hash";
import type { UniverseModel } from "./model";

// Star colours come from the document colour scheme (RFC BN, the explorer's
// resolvers): a document star by its type + status, a chunk by its status. A
// document without colouring enabled still draws with the default scheme, so
// the sky is never monochrome; a key the scheme lacks falls back to a pale
// neutral by depth (roots brighter than leaves).

export type RGB = [number, number, number];

export function hexToRgb(hex: string | undefined): RGB | undefined {
  if (!hex) return undefined;
  const m = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return undefined;
  let h = m[1];
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  return [parseInt(h.slice(0, 2), 16) / 255, parseInt(h.slice(2, 4), 16) / 255, parseInt(h.slice(4, 6), 16) / 255];
}

const NEUTRAL: RGB[] = [
  [0.86, 0.91, 1.0], // root: blue-white
  [0.8, 0.84, 0.92], // section
  [0.68, 0.72, 0.8], // leaf
];

/** chunkRgb is the colour of chunk c: its document's star colour for a root. */
export function chunkRgb(m: UniverseModel, c: number): RGB {
  const di = m.chunk.doc[c];
  const scheme = m.doc.colorEnabled[di] ? effectiveScheme(m.doc.scheme[di] ?? undefined) : DEFAULT_SCHEME;
  const isRoot = m.doc.root[di] === c;
  const hex = isRoot
    ? (docColor(m.doc.type[di], m.doc.status[di], scheme) ?? chunkColor(m.chunk.status[c], scheme))
    : chunkColor(m.chunk.status[c], scheme);
  return hexToRgb(hex) ?? NEUTRAL[Math.min(2, m.chunk.depth[c])];
}

// Link colours by kind: the common kinds get fixed hues; any other kind (an
// ontology-declared one) a stable hue from its name.
const KIND_HEX: Record<string, string> = {
  references: "#6aa9ff",
  cites: "#6aa9ff",
  supersedes: "#ff9f43",
  about: "#b48cff",
  derived_from: "#4fd1c5",
  mentions: "#9aa0ad",
  related: "#f6c453",
  defines: "#7ee08a",
  implements: "#ff7aa8",
  based_on: "#58c4dd",
};

export function kindRgb(kind: string): RGB {
  const fixed = hexToRgb(KIND_HEX[kind]);
  if (fixed) return fixed;
  const h = (fnv1a(kind) % 360) / 360;
  return hslToRgb(h, 0.65, 0.65);
}

export function kindHex(kind: string): string {
  const [r, g, b] = kindRgb(kind);
  const to = (x: number) => Math.round(x * 255).toString(16).padStart(2, "0");
  return `#${to(r)}${to(g)}${to(b)}`;
}

function hslToRgb(h: number, s: number, l: number): RGB {
  const f = (n: number) => {
    const k = (n + h * 12) % 12;
    const a = s * Math.min(l, 1 - l);
    return l - a * Math.max(-1, Math.min(k - 3, Math.min(9 - k, 1)));
  };
  return [f(0), f(8), f(4)];
}

/** rgbHex renders an RGB triple as #rrggbb (the search list's colour chips). */
export function rgbHex([r, g, b]: RGB): string {
  const to = (x: number) => Math.round(Math.max(0, Math.min(1, x)) * 255).toString(16).padStart(2, "0");
  return `#${to(r)}${to(g)}${to(b)}`;
}

/** recency maps an updated_at stamp (any of s / ms / µs / ns) to (0, 1]:
 *  1 = changed just now, decaying with a one-day time constant. 0 = unknown. */
export function recency(updatedAt: number, nowMs: number): number {
  if (!updatedAt || updatedAt <= 0) return 0;
  const ms =
    updatedAt > 1e17 ? updatedAt / 1e6 : updatedAt > 1e14 ? updatedAt / 1e3 : updatedAt > 1e11 ? updatedAt : updatedAt * 1e3;
  const age = Math.max(0, nowMs - ms);
  return Math.exp(-age / 86_400_000);
}
