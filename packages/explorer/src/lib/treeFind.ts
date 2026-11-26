import type { DocSummary } from "./colorScheme";
import type { PathLsFilters } from "./dataLayer";
import { documentSummaryOf, isAnnotationEntry, type PathNode } from "../components/PathTree";

// Search and filter over the Path tree (RFC GZ). Search works on the folders
// already listed: the tree is lazy, so a folder nobody opened holds nothing
// to match yet, and the search line says so. The filter is the runtime's
// (RFC HA): each one-level listing is asked with the filters, so it covers
// the whole subtree; a runtime that does not filter (no echo of the filters
// on its answer) gets the filter applied here, to the folders already listed.
// `findCoverage` is where that is decided.

export interface TreeFilter {
  name: string;
  type: string; // "" = any
  status: string; // "" = any
}

export const NO_FILTER: TreeFilter = { name: "", type: "", status: "" };

export function filterActive(f: TreeFilter): boolean {
  return f.name.trim() !== "" || f.type.trim() !== "" || f.status.trim() !== "";
}

// STATUS_SUGGESTIONS are the statuses the knowledge base works with; a status
// is free text in the store, so they are offered, not imposed.
export const STATUS_SUGGESTIONS = ["draft", "review", "confirmed", "done"];

// findCoverage says what a search or a filter reaches, from the root listing
// it is drawn from: "subtree" when the runtime answered it with the filters it
// applied (RFC HA), else "opened" — the folders already listed. A search's
// listing is never filtered, so a search always reads "opened".
export type FindCoverage = "opened" | "subtree";
export function findCoverage(rootListing: unknown): FindCoverage {
  const echo = (rootListing as { appliedFilters?: unknown } | undefined)?.appliedFilters;
  return echo !== undefined && echo !== null ? "subtree" : "opened";
}

export function coverageLabel(mode: "search" | "filter", c: FindCoverage): string {
  const verb = mode === "search" ? "searching" : "filtering";
  return c === "subtree" ? `${verb} the whole tree` : `${verb} opened folders`;
}

// lsFilters is a filter as a one-level ls takes it; undefined when nothing is
// set. The name is trimmed, as the local filter reads it.
export function lsFilters(f: TreeFilter): PathLsFilters | undefined {
  if (!filterActive(f)) return undefined;
  const out: PathLsFilters = {};
  const name = f.name.trim();
  const type = f.type.trim();
  const status = f.status.trim();
  if (name) out.name_contains = name;
  if (type) out.type = type;
  if (status) out.status = status;
  return out;
}

// AUTO_OPEN_FOLDERS bounds how many folders a filtered tree opens by itself.
// Every folder a filtered listing keeps leads to a match, so opening it is
// what the reader would do next; but each opening is one more filtered ls
// (which can read up to 50,000 documents' metadata), so past the bound the
// folders wait, closed, for a click.
export const AUTO_OPEN_FOLDERS = 8;

// autoOpen picks the folders of a filtered tree to open next, breadth first:
// drawn (every folder above is open), not open yet, never opened before
// (`tried` — a folder the reader closes stays closed), and leading to a
// match: a directory always does, a document with names below it only when
// its count says something below matches. At most `budget - tried.size`.
export function autoOpen(
  tree: PathNode[],
  open: ReadonlySet<string>,
  tried: ReadonlySet<string>,
  budget: number = AUTO_OPEN_FOLDERS,
): string[] {
  const room = budget - tried.size;
  if (room <= 0) return [];
  const out: string[] = [];
  let level = tree;
  while (level.length > 0 && out.length < room) {
    const next: PathNode[] = [];
    for (const n of level) {
      if (isAnnotationEntry(n)) continue;
      const opens =
        n.kind === "directory" || (n.hasChildren === true && (!n.matched || (n.documents ?? 0) > 0));
      if (!opens) continue;
      if (open.has(n.fullPath)) next.push(...n.children);
      else if (!tried.has(n.fullPath) && out.length < room) out.push(n.fullPath);
    }
    level = next;
  }
  return out;
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
      if (inside || n.fullPath === base || base.startsWith(`${n.fullPath}/`)) walk(n.children);
    }
  };
  walk(tree);
  return out;
}

function docMatches(n: PathNode, f: TreeFilter, summaries?: Map<string, DocSummary>): boolean {
  const s = documentSummaryOf(n, summaries);
  const q = fold(f.name.trim());
  if (q && !fold(n.name).includes(q) && !fold(s?.title ?? "").includes(q)) return false;
  const type = f.type.trim();
  const status = f.status.trim();
  if (type && fold(s?.type ?? "") !== fold(type)) return false;
  if (status && fold(s?.status ?? "") !== fold(status)) return false;
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
  const nameOnly = f.type.trim() === "" && f.status.trim() === "";
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

// filterChoices lists the types and statuses among the documents of the given
// trees (the plain tree and the filtered one), as the filter fields'
// suggestions; the statuses always include STATUS_SUGGESTIONS. A folder's
// `_annotation` document is no document of the folder — the runtime's filter
// never matches it — so neither it nor its `Annotation` type is offered.
export function filterChoices(
  trees: PathNode[][],
  summaries?: Map<string, DocSummary>,
): { types: string[]; statuses: string[] } {
  const types = new Set<string>();
  const statuses = new Set<string>(STATUS_SUGGESTIONS);
  const walk = (nodes: PathNode[]) => {
    for (const n of nodes) {
      if (!isAnnotationEntry(n)) {
        const s = documentSummaryOf(n, summaries);
        if (s?.type && fold(s.type) !== "annotation") types.add(s.type);
        if (s?.status) statuses.add(s.status);
      }
      walk(n.children);
    }
  };
  trees.forEach(walk);
  const sort = (a: string, b: string) => a.localeCompare(b);
  return { types: [...types].sort(sort), statuses: [...statuses].sort(sort) };
}
