import type { DocSummary } from "./colorScheme";
import { documentSummaryOf, isAnnotationEntry, type PathNode } from "../components/PathTree";

// Search and filter over the Path tree (RFC GZ). Both work on the folders
// already listed: the tree is lazy, so a folder nobody opened holds nothing
// to match yet, and the search line says so. Once the runtime filters a
// listing itself (RFC HA echoes the filters it applied), the same controls
// cover the whole subtree; `findCoverage` is where that is decided.

export interface TreeFilter {
  name: string;
  type: string; // "" = any
  status: string; // "" = any
}

export const NO_FILTER: TreeFilter = { name: "", type: "", status: "" };

export function filterActive(f: TreeFilter): boolean {
  return f.name.trim() !== "" || f.type !== "" || f.status !== "";
}

// findCoverage says what a search or a filter reaches: "opened" — the folders
// already listed — until the runtime answers listings with the filters it
// applied (RFC HA), then "subtree". No runtime does yet, so a listing never
// carries the echo and this stays "opened".
export type FindCoverage = "opened" | "subtree";
export function findCoverage(rootListing: unknown): FindCoverage {
  const echo = (rootListing as { appliedFilters?: unknown } | undefined)?.appliedFilters;
  return echo !== undefined && echo !== null ? "subtree" : "opened";
}

export function coverageLabel(mode: "search" | "filter", c: FindCoverage): string {
  const verb = mode === "search" ? "searching" : "filtering";
  return c === "subtree" ? `${verb} the whole tree` : `${verb} opened folders`;
}

const fold = (s: string) => s.toLocaleLowerCase();

// searchListed lists, in tree order (depth first, as drawn), the listed
// entries under `base` ("" = the root) whose names contain `query`. A
// folder's `_annotation` entry is never a match: it has no row.
export function searchListed(tree: PathNode[], base: string, query: string): PathNode[] {
  const q = fold(query.trim());
  if (!q) return [];
  const out: PathNode[] = [];
  const walk = (nodes: PathNode[]) => {
    for (const n of nodes) {
      if (isAnnotationEntry(n)) continue;
      const inside = base === "" || n.fullPath.startsWith(`${base}/`);
      if (inside && fold(n.name).includes(q)) out.push(n);
      // Only a branch that leads to (or lies under) the base is walked.
      if (inside || base.startsWith(`${n.fullPath}/`)) walk(n.children);
    }
  };
  walk(tree);
  return out;
}

function docMatches(n: PathNode, f: TreeFilter, summaries?: Map<string, DocSummary>): boolean {
  const s = documentSummaryOf(n, summaries);
  const q = fold(f.name.trim());
  if (q && !fold(n.name).includes(q) && !fold(s?.title ?? "").includes(q)) return false;
  if (f.type && fold(s?.type ?? "") !== fold(f.type)) return false;
  if (f.status && fold(s?.status ?? "") !== fold(f.status)) return false;
  return true;
}

// filterTree narrows the listed tree to what matches. A document (or any
// non-folder entry) stays when it matches; a folder stays when something
// under it does — it is then shown open, as `open` lists — or, with only a
// name to match, when its own name does (shown as it is, with what it holds).
// Folders with no match are gone.
export function filterTree(
  tree: PathNode[],
  f: TreeFilter,
  summaries?: Map<string, DocSummary>,
): { tree: PathNode[]; open: Set<string> } {
  const open = new Set<string>();
  const nameOnly = f.type === "" && f.status === "";
  const q = fold(f.name.trim());
  const walk = (nodes: PathNode[]): PathNode[] => {
    const kept: PathNode[] = [];
    for (const n of nodes) {
      if (isAnnotationEntry(n)) {
        kept.push(n); // never drawn; kept so the folder's card keeps its brief
        continue;
      }
      const isFolder = n.kind === "directory";
      const under = walk(n.children);
      const hasMatchUnder = under.some((c) => !isAnnotationEntry(c));
      if (hasMatchUnder) {
        open.add(n.fullPath);
        kept.push({ ...n, children: under });
      } else if (isFolder ? nameOnly && q !== "" && fold(n.name).includes(q) : docMatches(n, f, summaries)) {
        kept.push(n);
      }
    }
    return kept;
  };
  return { tree: walk(tree), open };
}

// filterChoices lists the types and statuses among the listed documents, for
// the filter's pickers.
export function filterChoices(
  tree: PathNode[],
  summaries?: Map<string, DocSummary>,
): { types: string[]; statuses: string[] } {
  const types = new Set<string>();
  const statuses = new Set<string>();
  const walk = (nodes: PathNode[]) => {
    for (const n of nodes) {
      const s = documentSummaryOf(n, summaries);
      if (s?.type) types.add(s.type);
      if (s?.status) statuses.add(s.status);
      walk(n.children);
    }
  };
  walk(tree);
  const sort = (a: string, b: string) => a.localeCompare(b);
  return { types: [...types].sort(sort), statuses: [...statuses].sort(sort) };
}
