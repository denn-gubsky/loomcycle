import type { ChannelDescriptor } from "../api";

// v0.11.12 — extend the filter axis to include `runtime` and `orphan`
// source-tag filters. v0.11.5 added the channels CRUD substrate with
// every row carrying a `source: "yaml" | "runtime" | "orphan"` tag,
// but the filter row only had scope-based chips. Operators wanting to
// see "just the runtime-created channels" had to scan visually for
// the source chip on each row. The new chips filter by c.source
// directly.
//
// `documents` shows the document change feeds (source "document",
// documents/<id>/chunks). There is one per document ever edited, and they
// are nothing an operator publishes to or awaits, so every other chip,
// "all" included, leaves them out.
export type FilterKind =
  | "all"
  | "system"
  | "global"
  | "user"
  | "agent"
  | "yaml"
  | "runtime"
  | "orphan"
  | "documents";

export const DOCUMENT_FEED_SOURCE = "document";

// sourceFilters is the closed set of source-tag filter values; the
// rest fall through to scope-based filtering. Keep in sync with the
// FilterKind type + the chip set.
const sourceFilters: Record<string, boolean> = {
  yaml: true,
  runtime: true,
  orphan: true,
};

export function filterChannels(channels: ChannelDescriptor[], f: FilterKind): ChannelDescriptor[] {
  if (f === "documents") return channels.filter((c) => c.source === DOCUMENT_FEED_SOURCE);
  const usable = channels.filter((c) => c.source !== DOCUMENT_FEED_SOURCE);
  if (f === "all") return usable;
  if (f === "system") return usable.filter((c) => c.name.startsWith("_system/"));
  if (sourceFilters[f]) return usable.filter((c) => c.source === f);
  return usable.filter((c) => c.scope === f);
}
