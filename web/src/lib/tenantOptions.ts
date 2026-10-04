// The pure half of the tenant combobox: what the drop-down lists, and where the
// highlight goes. Kept out of the component so it can be tested without a DOM.

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
  const q = query.trim().toLowerCase();
  if (!q) return [ALL_TENANTS, ...known];
  const starts: TenantOption[] = [];
  const contains: TenantOption[] = [];
  for (const t of known) {
    const at = t.id.toLowerCase().indexOf(q);
    if (at === 0) starts.push(t);
    else if (at > 0) contains.push(t);
  }
  return [ALL_TENANTS, ...starts, ...contains];
}

// moveActive steps the highlighted row for an arrow / Home / End key, wrapping
// at both ends. -1 means nothing is highlighted, which is the state after
// opening: Enter then submits what was typed instead of picking a row the admin
// never looked at.
export function moveActive(
  current: number,
  count: number,
  key: "ArrowDown" | "ArrowUp" | "Home" | "End",
): number {
  if (count <= 0) return -1;
  switch (key) {
    case "Home":
      return 0;
    case "End":
      return count - 1;
    case "ArrowDown":
      return current < 0 || current >= count - 1 ? 0 : current + 1;
    case "ArrowUp":
      return current <= 0 ? count - 1 : current - 1;
  }
}
