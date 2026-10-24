// Subject erasure: GET/POST /v1/_erasure. The structured `access` field names
// the groups, lists and revoked member tokens — typed, so a caller reads
// res.access.tokens[i].def_id without a cast.
import { describe, expect, it } from "vitest";

import type { ErasureAccess } from "../src/index.js";
import { jsonResponse, makeClient } from "./helpers.js";

const access: ErasureAccess = {
  groups: ["ops"],
  lists: ["readers"],
  tokens: [{ def_id: "def_1", name: "u-alice" }],
};

const residue = { rows: 0, scopes: [], sessions_examined: 0, truncated: false };

describe("erasure", () => {
  it("erasureExecute returns the access field typed", async () => {
    const { client } = makeClient([jsonResponse({
      tenant: "acme", subject: "alice", dry_run: true,
      deleted: { operator_tokens: 1 }, retained: { usage_ledger: "kept" },
      residue, access, notes: [],
    })]);
    const res = await client.erasureExecute("alice");
    expect(res.access.tokens.map((t) => [t.def_id, t.name])).toEqual([["def_1", "u-alice"]]);
    expect(res.access.groups).toEqual(["ops"]);
    expect(res.access.lists).toEqual(["readers"]);
  });

  it("erasureReport returns the access field typed", async () => {
    const empty = { counts: {}, total: 0 };
    const { client } = makeClient([jsonResponse({
      tenant: "acme", subject: "alice",
      tier1_covered: empty, tier2_uncovered: empty, tier3_residue: residue,
      access, notes: [],
    })]);
    const rep = await client.erasureReport("alice", { tenant: "acme" });
    expect(rep.access.tokens[0]!.def_id).toBe("def_1");
  });
});
