import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { mergeSubjects, type MergeSubjectsResult, type Principal } from "../api";
import SubjectMergePanel, { canMergeSubjects, mergeSummary } from "./SubjectMergePanel";

const principal = (scopes: string[], is_admin = false): Principal =>
  ({ tenant_id: "acme", subject: "s", scopes, is_admin, legacy: false }) as Principal;

afterEach(() => vi.unstubAllGlobals());

describe("canMergeSubjects", () => {
  it("offers the merge to an operator on a tenant document only", () => {
    expect(canMergeSubjects(principal(["substrate:tenant"]), "tenant")).toBe(true);
    expect(canMergeSubjects(principal([], true), "tenant")).toBe(true);
    expect(canMergeSubjects(principal(["runs:create", "runs:read"]), "tenant")).toBe(false);
    expect(canMergeSubjects(principal(["substrate:tenant"]), "user")).toBe(false);
    expect(canMergeSubjects(null, "tenant")).toBe(false);
  });
});

describe("SubjectMergePanel", () => {
  it("renders the action folded, with the survivor field", () => {
    const html = renderToStaticMarkup(createElement(SubjectMergePanel, { documentId: "d1" }));
    expect(html).toContain("Merge this subject into another");
    expect(html).toContain("document id or natural key");
  });

  it("summarizes what a merge did", () => {
    const r: MergeSubjectsResult = {
      tenant: "acme",
      duplicate: { document_id: "d1", root_chunk_id: "r1", natural_key: "person:ada-king" },
      survivor: { document_id: "d2", root_chunk_id: "r2", natural_key: "person:ada-lovelace" },
      facts_moved: 1, statements_repointed: 2, statements_retired: 0, statements_left: 1,
    };
    expect(mergeSummary(r)).toBe(
      "Merged person:ada-king into person:ada-lovelace, 1 fact moved, 2 statements re-pointed, 1 left for the links agent.",
    );
  });
});

describe("mergeSubjects", () => {
  it("POSTs the pair to the merge route with the tenant focus", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await mergeSubjects("d1", "person:ada-lovelace", "globex");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("/v1/_ontology/subjects/merge?tenant=globex");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ duplicate: "d1", survivor: "person:ada-lovelace" });
  });
});
