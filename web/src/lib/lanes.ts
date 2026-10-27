import type { ConcurrencyStatsResult, ConcurrencyStatsResponse, LaneStat, ProviderGateStat } from "../api";

// Pure derivations for the Lanes page (GET /v1/_concurrency/stats),
// kept out of the component so the empty-state choice and the formatting are
// testable without rendering.

export type LanesView =
  | { kind: "not_wired" }
  | { kind: "forbidden" }
  | { kind: "error"; message: string }
  | { kind: "uncapped"; stats: ConcurrencyStatsResponse }
  | { kind: "capped"; stats: ConcurrencyStatsResponse };

// lanesView picks what the tab shows for one fetch. The run queue is shown in
// both the capped and the uncapped case — it exists whether or not a provider
// caps model calls — so "uncapped" still carries the stats.
export function lanesView(r: ConcurrencyStatsResult): LanesView {
  if (!r.ok) {
    if (r.status === 503) return { kind: "not_wired" };
    if (r.status === 403) return { kind: "forbidden" };
    return { kind: "error", message: `HTTP ${r.status}` };
  }
  const providers = r.data.providers;
  if (!providers || Object.keys(providers).length === 0) return { kind: "uncapped", stats: r.data };
  return { kind: "capped", stats: r.data };
}

// The kinds in the order the docs list them; anything newer sorts after, by name.
const KIND_ORDER = ["chat", "decision", "embedder"];

function kindRank(kind: string): number {
  const i = KIND_ORDER.indexOf(kind);
  return i === -1 ? KIND_ORDER.length : i;
}

// sortedLanes is a provider's lanes in a stable display order. The server sends
// a map, whose key order is not a contract.
export function sortedLanes(p: ProviderGateStat): Array<[string, LaneStat]> {
  return Object.entries(p.lanes ?? {}).sort(
    ([a], [b]) => kindRank(a) - kindRank(b) || a.localeCompare(b),
  );
}

// sortedProviders orders providers by id, for the same reason.
export function sortedProviders(
  providers: Record<string, ProviderGateStat> | undefined,
): Array<[string, ProviderGateStat]> {
  return Object.entries(providers ?? {}).sort(([a], [b]) => a.localeCompare(b));
}

// modelsInFlight lists the admitted model identities, busiest first. Its length
// is the number of models computing — what max_models_in_flight caps — not the
// sum of the counts, which includes calls still queued in a lane.
export function modelsInFlight(p: ProviderGateStat): Array<[string, number]> {
  return Object.entries(p.models_in_flight ?? {}).sort(
    ([a, x], [b, y]) => y - x || a.localeCompare(b),
  );
}

// laneActive renders a lane's occupancy against its cap: "2 / 4".
export function laneActive(l: LaneStat): string {
  return `${l.active} / ${l.max_concurrent}`;
}

// laneQueued renders the waiters, naming the background share and the calls
// held back by their model's cap only when there are some: "3",
// "3 (1 background)" or "3 (1 background, 2 on model cap)". The last is what
// explains a queue beside a free slot.
export function laneQueued(l: LaneStat): string {
  const parts: string[] = [];
  if (l.queued_background > 0) parts.push(`${l.queued_background} background`);
  if ((l.blocked_on_model ?? 0) > 0) parts.push(`${l.blocked_on_model} on model cap`);
  return parts.length > 0 ? `${l.queued} (${parts.join(", ")})` : `${l.queued}`;
}

// laneSummary is one line for a lane, used as the row's hover text.
export function laneSummary(kind: string, l: LaneStat): string {
  return (
    `${kind}: ${laneActive(l)} active, ${l.reserved} reserved, ${laneQueued(l)} queued; ` +
    `${l.grants} grants, ${l.preemptions} preemptions`
  );
}
