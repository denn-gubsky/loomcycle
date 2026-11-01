// Statement review: GET /v1/_statements/review, POST /v1/_statements/rule and
// POST /v1/_statements/revert.
import { describe, expect, it } from "vitest";

import { LoomcycleError } from "../src/index.js";
import { jsonResponse, makeClient } from "./helpers.js";

const base = "http://test-loomcycle:8787";

describe("listReviewQueue", () => {
  it("sends the filter as query parameters and returns the page typed", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", scope: "tenant", statements: [{ id: "s1", kind: "cites" }], truncated: true, next_cursor: "c1" }),
    ]);
    const res = await client.listReviewQueue({
      filter: { method: "rule:", kinds: ["cites", "repeals"], from_under_path: "/board", min_confidence: 0.5, has_judge_verdict: false, created_after: "1793481550963216000" },
      limit: 50,
    });
    expect(res.statements[0]!.id).toBe("s1");
    expect(res.next_cursor).toBe("c1");
    const url = new URL(fetchMock.mock.calls[0]![0] as string);
    expect(url.pathname).toBe("/v1/_statements/review");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      method: "rule:", kinds: "cites,repeals", from_under_path: "/board", min_confidence: "0.5",
      has_judge_verdict: "false", created_after: "1793481550963216000", limit: "50",
    });
  });

  it("asks for the summary and an admin's tenant", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "globex", scope: "tenant", statements: [], summary: [{ kind: "cites", method: "rule:citation", from_path_prefix: "/board/reg", judge_verdict: "", count: 3 }], total: 3 }),
    ]);
    const res = await client.listReviewQueue({ summarize: true, tenant: "globex" });
    expect(res.summary![0]!.count).toBe(3);
    expect(fetchMock.mock.calls[0]![0]).toBe(`${base}/v1/_statements/review?summarize=true&tenant=globex`);
  });
});

describe("ruleStatements", () => {
  const dry = { tenant: "acme", scope: "tenant", action: "accept", dry_run: true, matched: 2, selected: 2, skipped: 0, to_rule: 2, ruled: 0, failed: 0, breakdown: [], sample: [{ id: "s1" }] };

  it("POSTs a dry run by default, then the ruling with its count", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse(dry),
      jsonResponse({ ...dry, dry_run: false, ruling_id: "r1", ruled: 2, items: [{ id: "s1", status: "asserted" }] }),
    ]);
    const filter = { method: "rule:citation" };
    const d = await client.ruleStatements("accept", { filter });
    const r = await client.ruleStatements("accept", { filter }, { dryRun: false, expectCount: d.matched, max: 500 });
    expect(r.ruling_id).toBe("r1");
    expect(r.items![0]!.status).toBe("asserted");
    expect(fetchMock.mock.calls[0]![0]).toBe(`${base}/v1/_statements/rule`);
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ action: "accept", filter });
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string)).toEqual({ action: "accept", filter, dry_run: false, expect_count: 2, max: 500 });
  });

  it("names statements by ids, with a reason to reject", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...dry, action: "reject" })]);
    await client.ruleStatements("reject", { ids: ["s1", "s2"] }, { reason: "wrong act", tenant: "globex" });
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ action: "reject", ids: ["s1", "s2"], reason: "wrong act", tenant: "globex" });
  });

  it("throws with the server's code when the queue changed", async () => {
    const { client } = makeClient([jsonResponse({ code: "queue_changed", error: "the selection now matches 3 statements, not the 2 expected" }, 409)]);
    const err = await client.ruleStatements("accept", { filter: {} }, { dryRun: false, expectCount: 2 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("queue_changed");
  });
});

describe("revertRuling", () => {
  it("POSTs the ruling id and returns what went back", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", scope: "tenant", ruling_id: "r1", action: "accept", reverted: 1, changed: 1, skipped: 0,
        items: [{ id: "s1", status: "proposed" }, { id: "s2", error: "changed", message: "changed since the ruling" }] }),
    ]);
    const res = await client.revertRuling("r1");
    expect(res.reverted).toBe(1);
    expect(res.items[1]!.error).toBe("changed");
    expect(fetchMock.mock.calls[0]![0]).toBe(`${base}/v1/_statements/revert`);
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ ruling_id: "r1" });
  });

  it("throws ruling_not_found for an unknown ruling", async () => {
    const { client } = makeClient([jsonResponse({ code: "ruling_not_found", error: "no bulk ruling r9 in this scope" }, 404)]);
    const err = await client.revertRuling("r9").catch((e: unknown) => e);
    expect((err as LoomcycleError).code).toBe("ruling_not_found");
  });
});

describe("listRulings", () => {
  it("GETs a page of rulings with its cursor, scope and subject", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", scope: "user", next_cursor: "n1", rulings: [
        { id: "r1", action: "accept", ruled_by: "operator:ops", actor: "ops", selector: { ids_count: 3 }, matched: 3, ruled: 3, failed: 0, skipped: 0,
          created_at: 1, reverted: true, reverted_at: 2, reverted_by: "v1" },
      ] }),
    ]);
    const res = await client.listRulings({ limit: 5, after: "c", scope: "user", scopeId: "bob" });
    expect(res.rulings[0]!.selector.ids_count).toBe(3);
    expect(res.rulings[0]!.reverted_by).toBe("v1");
    expect(res.next_cursor).toBe("n1");
    expect(fetchMock.mock.calls[0]![0]).toBe(`${base}/v1/_statements/rulings?limit=5&after=c&scope=user&scope_id=bob`);
  });
});

describe("scope_id and the exact selectors", () => {
  it("names another subject's user scope on the queue, a ruling and a revert", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", scope: "user", statements: [] }),
      jsonResponse({ tenant: "acme", scope: "user", action: "accept", dry_run: true, matched: 0, selected: 0, skipped: 0, to_rule: 0, ruled: 0, failed: 0, breakdown: [] }),
      jsonResponse({ tenant: "acme", scope: "user", ruling_id: "r1", action: "accept", reverted: 0, changed: 0, skipped: 0, items: [], revert_id: "v1" }),
    ]);
    await client.listReviewQueue({ scope: "user", scopeId: "bob", filter: { method_exact: "rule:citation", from_unfiled: true } });
    const url = new URL(fetchMock.mock.calls[0]![0] as string);
    expect(Object.fromEntries(url.searchParams)).toEqual({ method_exact: "rule:citation", from_unfiled: "true", scope: "user", scope_id: "bob" });
    await client.ruleStatements("accept", { filter: { kind_exact: "cites", from_path_prefix: "/board/reg" } }, { scope: "user", scopeId: "bob" });
    expect(JSON.parse(fetchMock.mock.calls[1]![1]!.body as string)).toEqual({
      action: "accept", filter: { kind_exact: "cites", from_path_prefix: "/board/reg" }, scope: "user", scope_id: "bob",
    });
    const rev = await client.revertRuling("r1", { scope: "user", scopeId: "bob" });
    expect(rev.revert_id).toBe("v1");
    expect(JSON.parse(fetchMock.mock.calls[2]![1]!.body as string)).toEqual({ ruling_id: "r1", scope: "user", scope_id: "bob" });
  });
});
