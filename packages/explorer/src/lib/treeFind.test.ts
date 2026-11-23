import { describe, expect, it } from "vitest";
import type { PathEntry } from "../types";
import type { DocSummary } from "./colorScheme";
import type { DirListing } from "./lazyPathTree";
import { buildLazyTree } from "../components/PathTree";
import { coverageLabel, filterTree, findCoverage, searchListed } from "./treeFind";

const dir = (full_path: string): PathEntry => ({
  name: full_path.slice(full_path.lastIndexOf("/") + 1),
  kind: "directory",
  full_path,
});
const doc = (full_path: string, id: string): PathEntry => ({ ...dir(full_path), kind: "document", resource_ref: { document_id: id } });
const listing = (entries: PathEntry[]): DirListing => ({ entries, loaded: true, loading: false });

const tree = buildLazyTree(
  new Map([
    ["", listing([dir("/kb"), dir("/plans"), dir("/unopened")])],
    ["/kb", listing([doc("/kb/_annotation", "ann"), dir("/kb/docs")])],
    ["/kb/docs", listing([doc("/kb/docs/plan-a", "a"), doc("/kb/docs/guide", "g")])],
    ["/plans", listing([doc("/plans/plan-b", "b")])],
  ]),
);
const summaries = new Map<string, DocSummary>([
  ["a", { document_id: "a", title: "Plan A", type: "Plan", status: "draft", color_enabled: false }],
  ["g", { document_id: "g", title: "Guide", type: "Guide", status: "done", color_enabled: false }],
  ["b", { document_id: "b", title: "Plan B", type: "Plan", status: "done", color_enabled: false }],
]);

describe("treeFind", () => {
  it("TestSearchListed_MatchesNamesInTreeOrderUnderTheBase", () => {
    expect(searchListed(tree, "", "plan").map((n) => n.fullPath)).toEqual(["/kb/docs/plan-a", "/plans", "/plans/plan-b"]);
    // Under a selected folder only, its own children included.
    expect(searchListed(tree, "/kb", "plan").map((n) => n.fullPath)).toEqual(["/kb/docs/plan-a"]);
    expect(searchListed(tree, "/kb/docs", "PLAN").map((n) => n.fullPath)).toEqual(["/kb/docs/plan-a"]);
    // A folder's _annotation is never a match; an empty query matches nothing.
    expect(searchListed(tree, "", "annotation")).toEqual([]);
    expect(searchListed(tree, "", "  ")).toEqual([]);
  });

  it("TestFilterTree_HidesFoldersWithNoMatchAndOpensTheOnesThatHaveOne", () => {
    const { tree: kept, open } = filterTree(tree, { name: "", type: "Plan", status: "draft" }, summaries);
    expect(kept.map((n) => n.fullPath)).toEqual(["/kb"]);
    expect(kept[0].children.filter((n) => n.name !== "_annotation").map((n) => n.fullPath)).toEqual(["/kb/docs"]);
    expect(kept[0].children.find((n) => n.name === "docs")!.children.map((n) => n.fullPath)).toEqual(["/kb/docs/plan-a"]);
    expect([...open].sort()).toEqual(["/kb", "/kb/docs"]);
    // A name alone keeps a folder whose own name matches.
    expect(filterTree(tree, { name: "unop", type: "", status: "" }, summaries).tree.map((n) => n.fullPath)).toEqual([
      "/unopened",
    ]);
  });

  it("TestFindCoverage_SaysOpenedFoldersUntilTheRuntimeEchoesItsFilters", () => {
    expect(findCoverage(listing([]))).toBe("opened");
    expect(coverageLabel("search", "opened")).toBe("searching opened folders");
    expect(coverageLabel("filter", "opened")).toBe("filtering opened folders");
    expect(findCoverage({ ...listing([]), appliedFilters: { name: "x" } })).toBe("subtree");
  });
});
