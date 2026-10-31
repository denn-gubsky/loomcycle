// Ontology subject merge: POST /v1/_ontology/subjects/merge.
import { describe, expect, it } from "vitest";

import { LoomcycleError } from "../src/index.js";
import { jsonResponse, makeClient } from "./helpers.js";

const result = {
  tenant: "acme",
  duplicate: { document_id: "d1", root_chunk_id: "r1", natural_key: "person:ada-king" },
  survivor: { document_id: "d2", root_chunk_id: "r2", natural_key: "person:ada-lovelace" },
  facts_moved: 2,
  statements_repointed: 1,
  statements_retired: 1,
  statements_left: 0,
};

describe("mergeSubjects", () => {
  it("POSTs {duplicate, survivor} and returns the result typed", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(result)]);
    const res = await client.mergeSubjects("person:ada-king", "d2");
    expect(res.survivor.natural_key).toBe("person:ada-lovelace");
    expect(res.facts_moved).toBe(2);
    expect(res.not_announced).toBeUndefined();

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/subjects/merge");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ duplicate: "person:ada-king", survivor: "d2" });
  });

  it("passes an admin's tenant focus as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(result)]);
    await client.mergeSubjects("d1", "d2", { tenant: "globex" });
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/subjects/merge?tenant=globex");
  });

  it("throws with the server's code on a refusal", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "already_merged", error: "person:ada-king was already merged into another subject" }, 409),
    ]);
    const err = await client.mergeSubjects("d1", "d2").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("already_merged");
  });
});
