// Deleting one run tree: DELETE /v1/runs/{run_id}/tree and
// DELETE /v1/sessions/{session_id}?cascade=children.
import { describe, expect, it } from "vitest";

import { InsufficientScopeError, LoomcycleError } from "../src/errors.js";
import { jsonResponse, makeClient } from "./helpers.js";

const deletion = {
  run_id: "r_1",
  deleted_runs: 3,
  counts: {
    runs: 3, events: 12, sessions: 3, session_embeddings: 0,
    interrupts: 1, evaluations: 0, memory_pending: 0, trace_index: 2,
  },
  kept: { usage_ledger: "kept by design" },
  not_reached: [
    { name: "snapshots", count: 1, reason: "a stored snapshot may hold a copy" },
    { name: "retention_exports", reason: "files on disk" },
  ],
};

describe("run tree delete", () => {
  it("deleteRunTree sends DELETE /v1/runs/{id}/tree and returns the counts", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(deletion)]);
    const res = await client.deleteRunTree("r_1");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/runs/r_1/tree");
    expect((init as RequestInit).method).toBe("DELETE");
    expect((init as RequestInit).body).toBeUndefined();
    expect(res.deleted_runs).toBe(3);
    expect(res.counts.events).toBe(12);
    expect(res.kept.usage_ledger).toBeTruthy();
    expect(res.not_reached.map((n) => [n.name, n.count])).toEqual([["snapshots", 1], ["retention_exports", undefined]]);
  });

  it("deleteRunTree escapes the run id, so it cannot address another route", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...deletion, deleted_runs: 0 })]);
    await client.deleteRunTree("r_1/../../_erasure");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/r_1%2F..%2F..%2F_erasure/tree");
  });

  it("an unknown or already deleted run resolves with zero counts, it does not raise", async () => {
    const zero = { run_id: "r_gone", deleted_runs: 0, counts: { runs: 0, events: 0 }, kept: {}, not_reached: [] };
    const { client } = makeClient([jsonResponse(zero)]);
    await expect(client.deleteRunTree("r_gone")).resolves.toEqual(zero);
  });

  it("a live run in the tree raises with code run_tree_live", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "run_tree_live", error: "run r_2 is still live; cancel it first", run_id: "r_2" }, 409),
    ]);
    const err = await client.deleteRunTree("r_1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("run_tree_live");
    expect((err as LoomcycleError).bodyText).toContain("r_2");
  });

  it("a token without runs:delete raises InsufficientScopeError naming the scope", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "insufficient_scope", error: "insufficient scope", required_scope: "runs:delete" }, 403),
    ]);
    const err = await client.deleteRunTree("r_1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InsufficientScopeError);
    expect((err as InsufficientScopeError).status).toBe(403);
    expect((err as InsufficientScopeError).requiredScope).toBe("runs:delete");
  });

  it("deleteSession sends DELETE /v1/sessions/{id}?cascade=children", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...deletion, run_id: undefined, session_id: "s 1" })]);
    const res = await client.deleteSession("s 1", { cascade: "children" });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/sessions/s%201?cascade=children");
    expect((init as RequestInit).method).toBe("DELETE");
    expect(res.session_id).toBe("s 1");
    expect(res.deleted_runs).toBe(3);
  });
});
