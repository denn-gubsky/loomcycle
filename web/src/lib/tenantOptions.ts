// The pure half of the tenant combobox: what the drop-down lists. Kept out of the
// component so it can be tested without a DOM. The matching and highlight
// stepping are shared with the other comboboxes (./comboOptions).
import { filterOptions, moveActive } from "./comboOptions";

// TenantOption is one row of the drop-down. `id: ""` is the explicit "all tenants"
// entry — an empty focus — which is a real choice, not the absence of one.
export interface TenantOption {
  id: string;
  // Activity counts from the directory, shown beside the id. Absent on the
  // "all tenants" row.
  users?: number;
  runs?: number;
}

export const ALL_TENANTS: TenantOption = { id: "" };

// normaliseTenants turns the GET /v1/_tenants body into the list the drop-down
// keeps: trimmed, de-duplicated, sorted, and without the empty id.
//
// The empty id is dropped because the server derives the list from runs, and
// open-mode runs carry tenant "" — a row that would render as a second, blank
// "all tenants" entry.
export function normaliseTenants(
  rows: ReadonlyArray<{ tenant?: string; users?: number; runs?: number }> | null | undefined,
): TenantOption[] {
  const seen = new Map<string, TenantOption>();
  for (const r of rows ?? []) {
    const id = (r?.tenant ?? "").trim();
    if (!id || seen.has(id)) continue;
    seen.set(id, { id, users: r.users, runs: r.runs });
  }
  return [...seen.values()].sort((a, b) => a.id.localeCompare(b.id));
}

// filterTenants is the drop-down's contents for what the admin has typed: "all
// tenants" first, then the known tenants whose id contains the query
// (case-insensitive), ids that START with it ahead of ones that merely contain it.
//
// "all tenants" stays listed whatever is typed: it is how the admin leaves a
// focus, and a filter that hides the way out is a trap.
export function filterTenants(known: ReadonlyArray<TenantOption>, query: string): TenantOption[] {
  return [ALL_TENANTS, ...filterOptions(known, query)];
}

// Re-exported so the tenant field's callers and tests keep one import; the
// stepping itself is shared with every combobox.
export { moveActive };
