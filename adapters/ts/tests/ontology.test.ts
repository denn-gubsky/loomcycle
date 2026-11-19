// Ontology subject operations: POST /v1/_ontology/subjects/merge and
// POST /v1/_ontology/subjects/principal.
import { describe, expect, it } from "vitest";

import { LoomcycleError, SessionBusyError } from "../src/index.js";
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

describe("bindSubjectPrincipal", () => {
  const bound = {
    tenant: "acme", document_id: "d1", root_chunk_id: "r1", natural_key: "person:ada",
    path: "/facts/ada", principal: "u-ada", changed: true,
  };

  it("POSTs {subject, principal} and returns the result typed", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(bound)]);
    const res = await client.bindSubjectPrincipal("person:ada", "u-ada");
    expect(res.principal).toBe("u-ada");
    expect(res.changed).toBe(true);
    expect(res.previous).toBeUndefined();

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/subjects/principal");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ subject: "person:ada", principal: "u-ada" });
  });

  it("clears with an empty principal and passes an admin's tenant focus", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...bound, principal: "", previous: "u-ada" })]);
    const res = await client.bindSubjectPrincipal("d1", "", { tenant: "globex" });
    expect(res.previous).toBe("u-ada");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/subjects/principal?tenant=globex");
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ subject: "d1", principal: "" });
  });

  it("throws with the server's code on a refusal", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "binding_conflict", error: "principal \"u-ada\" is already bound to subject person:ada-king" }, 409),
    ]);
    const err = await client.bindSubjectPrincipal("person:ada-lovelace", "u-ada").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("binding_conflict");
  });
});

describe("erasure bound subjects", () => {
  it("are typed on the report", async () => {
    const report = {
      tenant: "acme", subject: "u-ada",
      tier1_covered: { counts: {}, total: 0 }, tier2_uncovered: { counts: {}, total: 0 },
      tier3_residue: { rows: 0, scopes: [], sessions_examined: 0, truncated: false },
      access: { groups: [], lists: [], tokens: [] }, notes: [],
      bound_subjects: [{ document_id: "d1", natural_key: "person:ada", path: "/facts/ada" }],
      bound_subjects_withheld: 1,
    };
    const { client } = makeClient([jsonResponse(report)]);
    const rep = await client.erasureReport("u-ada");
    expect(rep.bound_subjects?.[0]?.natural_key).toBe("person:ada");
    expect(rep.bound_subjects_withheld).toBe(1);
  });
});

describe("planOntology", () => {
  const plan = {
    tenant: "acme",
    fingerprint: "a".repeat(64),
    changes: [{
      op: "delete",
      entry: { chunk_id: "c1", name: "Provision", section: "classes", kind: "class" },
      risk: "destroys",
      counts: { typed_documents: { n: 3, effect: "orphaned", tenant_scope: 1, other_scopes: 2, sample: ["d1"] } },
      subtree: [{ chunk_id: "c2", name: "Article", section: "classes", kind: "class" }],
    }],
    issues: [],
    new_issues: [],
    scopes_counted: 2,
    scopes_skipped: 0,
    complete: true,
  };

  it("posts changes and returns the plan", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(plan)]);
    const res = await client.planOntology([
      { op: "delete", entry: "c1" },
      { op: "rename", entry: "c3", name: "refers_to" },
    ]);
    expect(res.changes[0]!.risk).toBe("destroys");
    expect(res.changes[0]!.counts["typed_documents"]!.other_scopes).toBe(2);
    expect(res.complete).toBe(true);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/plan");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({
      changes: [{ op: "delete", entry: "c1" }, { op: "rename", entry: "c3", name: "refers_to" }],
    });
  });

  it("posts a proposal item by its id and returns the proposal it plans", async () => {
    const proposed = { ...plan, changes: [{ ...plan.changes[0]!, op: "retire", proposal: "p1" }] };
    const { client, fetchMock } = makeClient([jsonResponse(proposed)]);
    const res = await client.planOntology([{ op: "proposal", id: "p1" }, { op: "retire", entry: "c1" }]);
    expect(res.changes[0]!.proposal).toBe("p1");
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({
      changes: [{ op: "proposal", id: "p1" }, { op: "retire", entry: "c1" }],
    });
  });

  it("passes an admin's tenant focus as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(plan)]);
    await client.planOntology([{ op: "retire", entry: "c1" }], { tenant: "globex" });
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/plan?tenant=globex");
  });

  it("throws with the server's code on a refusal", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "entry_not_found", error: "changes[0]: no in-force entry c9" }, 404),
    ]);
    const err = await client.planOntology([{ op: "retire", entry: "c9" }]).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(404);
    expect((err as LoomcycleError).code).toBe("entry_not_found");
  });
});

describe("a plan's refusal of a destructive ontology change", () => {
  it("a 409 plan_required naming a class Session is not a SessionBusyError", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "plan_required",
          error:
            'update_chunk: plan_required: something rests on what this changes: rename of class "Session" (typed_documents 2 stale); it needs the fingerprint of a plan of the change (plan_fingerprint). Nothing was changed.',
          fingerprint: "ab12",
          tool: "Document",
        },
        409,
      ),
    ]);
    const err = await client
      .document({ op: "update_chunk", scope: "tenant", id: "c1", revision: 2, title: "Sitting" })
      .catch((x: unknown) => x);
    expect(err).not.toBeInstanceOf(SessionBusyError);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).code).toBe("plan_required");
    expect((err as LoomcycleError).status).toBe(409);
  });
});
