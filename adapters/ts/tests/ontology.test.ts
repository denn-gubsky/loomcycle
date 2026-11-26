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
    content_version: "0123456789abcdef",
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
    expect(res.content_version).toBe("0123456789abcdef");

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

describe("ontology releases", () => {
  const release = {
    seq: 2,
    content_version: "0123456789abcdef",
    fingerprint: "f".repeat(64),
    reason: "confirm",
    actor: "u-op",
    created_at: 1764000000000000000,
    entries: 12,
  };

  it("listOntologyReleases GETs the list and returns it typed", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", releases: [release, { ...release, seq: 1, reason: "catch_up" }] }),
    ]);
    const res = await client.listOntologyReleases();
    expect(res.releases.map((r) => [r.seq, r.reason])).toEqual([[2, "confirm"], [1, "catch_up"]]);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/releases");
    expect(call[1]!.method).toBe("GET");
  });

  it("listOntologyReleases passes an admin's tenant focus as ?tenant=", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ tenant: "globex", releases: [] })]);
    await client.listOntologyReleases({ tenant: "globex" });
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/releases?tenant=globex");
  });

  it("listOntologyReleases throws forbidden with its status", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "forbidden", error: "reading ontology releases is for an admin or a tenant operator" }, 403),
    ]);
    const err = await client.listOntologyReleases().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(403);
    expect((err as LoomcycleError).code).toBe("forbidden");
  });

  it("getOntologyRelease GETs one release with its export_md", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ tenant: "acme", release, export_md: "# Ontology\n" }),
    ]);
    const res = await client.getOntologyRelease(2, { tenant: "globex" });
    expect(res.release.seq).toBe(2);
    expect(res.export_md).toBe("# Ontology\n");
    expect(res.plan_summary).toBeUndefined();
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/releases/2?tenant=globex");
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("getOntologyRelease throws release_not_found with its status", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "release_not_found", error: "no complete release 9" }, 404),
    ]);
    const err = await client.getOntologyRelease(9).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(404);
    expect((err as LoomcycleError).code).toBe("release_not_found");
  });

  const diff = {
    tenant: "acme",
    from: { seq: 2, content_version: "0123456789abcdef" },
    to: { current: true, content_version: "fedcba9876543210" },
    changes: [{
      chunk_id: "c1",
      kind: "class",
      change: "changed",
      changed: ["renamed", "meaning"],
      before: { name: "event", meaning: "a thing that happened", directives: {} },
      after: { name: "incident", meaning: "an unplanned event", directives: {} },
    }],
    issues: [],
  };

  it("diffOntologyReleases defaults to=current and returns the changes typed", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(diff)]);
    const res = await client.diffOntologyReleases(2);
    expect(res.to.current).toBe(true);
    expect(res.changes[0]!.changed).toEqual(["renamed", "meaning"]);
    expect(res.changes[0]!.after?.name).toBe("incident");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/releases/diff?from=2&to=current");
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("diffOntologyReleases sends a release number as to and an admin's tenant focus", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(diff)]);
    await client.diffOntologyReleases(1, 3, { tenant: "globex" });
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_ontology/releases/diff?from=1&to=3&tenant=globex",
    );
  });

  it("diffOntologyReleases throws invalid_request with its status", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "invalid_request", error: "from must be a release's sequence number, a positive integer" }, 400),
    ]);
    const err = await client.diffOntologyReleases(0).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(400);
    expect((err as LoomcycleError).code).toBe("invalid_request");
  });
});

describe("ontology job", () => {
  const job = {
    id: "j1",
    kind: "concept_retag",
    entry: "c1",
    from: ["topics/risk"],
    to: "topics/reporting/risk",
    to_tags: ["topics/reporting/risk"],
    state: "running",
    step: "document_tags",
    done: { chunk_tags: 4 },
    remaining: 7,
    scope: "user",
    scopes_done: 1,
    scopes_total: 3,
    created_at: 1764000000000000000,
    updated_at: 1764000000000000001,
  };

  it("getOntologyJob GETs the job and returns it typed", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ tenant: "acme", job })]);
    const res = await client.getOntologyJob();
    expect(res.job?.state).toBe("running");
    expect(res.job?.to_tags).toEqual(["topics/reporting/risk"]);
    expect(res.job?.scopes_total).toBe(3);
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/job");
    expect(fetchMock.mock.calls[0]![1]!.method).toBe("GET");
  });

  it("getOntologyJob passes an admin's tenant focus and reads no job as null", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ tenant: "globex", job: null })]);
    const res = await client.getOntologyJob({ tenant: "globex" });
    expect(res.job).toBeNull();
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/_ontology/job?tenant=globex");
  });

  it("resumeOntologyJob POSTs the job's id and returns the job as the slice left it", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ tenant: "acme", job: { ...job, state: "done" } })]);
    const res = await client.resumeOntologyJob({ id: "j1", tenant: "globex" });
    expect(res.job?.state).toBe("done");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/job/resume?tenant=globex");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ id: "j1" });
  });

  it("resumeOntologyJob requires the job's id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ code: "invalid_body", error: "a resume needs the job's id" }, 400),
    ]);
    // The type requires the id; the server refuses an empty one.
    const typeCheckOnly = (c: typeof client) => {
      // @ts-expect-error id is required
      void c.resumeOntologyJob();
      // @ts-expect-error id is required
      void c.resumeOntologyJob({ tenant: "acme" });
    };
    void typeCheckOnly;
    const err = await client.resumeOntologyJob({ id: "" }).catch((e: unknown) => e);
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ id: "" });
    expect((err as LoomcycleError).status).toBe(400);
    expect((err as LoomcycleError).code).toBe("invalid_body");
  });

  it("resumeOntologyJob throws job_busy with its status", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "job_busy", error: "a class rename or concept move is being checked or carried out right now" }, 409),
    ]);
    const err = await client.resumeOntologyJob({ id: "j1" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("job_busy");
  });

  it("planOntology carries a planned class rename's job block", async () => {
    const plan = {
      tenant: "acme",
      fingerprint: "f".repeat(64),
      changes: [{
        op: "rename",
        entry: { chunk_id: "c1", name: "Bulletin", section: "classes", kind: "class" },
        risk: "stales",
        counts: {},
        job: {
          steps: [{ name: "documents_type", tenant_scope: 2, other_scopes: 5 }],
          runnable: true,
          collisions_total: 0,
          scopes_not_counted: 0,
          running: job,
        },
      }],
      issues: [],
      new_issues: [],
      scopes_counted: 2,
      scopes_skipped: 0,
      complete: true,
    };
    const { client } = makeClient([jsonResponse(plan)]);
    const res = await client.planOntology([{ op: "rename", entry: "c1", name: "Circular" }]);
    expect(res.changes[0]!.job?.steps[0]!.other_scopes).toBe(5);
    expect(res.changes[0]!.job?.running?.kind).toBe("concept_retag");
  });
});

describe("ontology rollback", () => {
  const report = {
    tenant: "acme",
    rollback: {
      release: 2,
      release_content_version: "aaaaaaaaaaaaaaaa",
      content_version: "aaaaaaaaaaaaaaaa",
      matches: true,
      risk: "none",
      applied: [{ op: "add", entry: "c2", name: "Ledger", new_id: "c9" }],
      rollback_release: 3,
    },
  };

  it("planOntology sends a release change and reads the release each change carries", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({
      tenant: "acme",
      fingerprint: "f".repeat(64),
      changes: [{ op: "unretire", entry: { chunk_id: "c1", name: "Memo", section: "classes", kind: "class" }, risk: "none", counts: {}, release: 2 }],
      issues: [],
      new_issues: [],
      scopes_counted: 0,
      scopes_skipped: 0,
      complete: true,
    })]);
    const res = await client.planOntology([{ op: "release", seq: 2 }]);
    expect(res.changes[0]!.release).toBe(2);
    expect(JSON.parse(fetchMock.mock.calls[0]![1]!.body as string)).toEqual({ changes: [{ op: "release", seq: 2 }] });
  });

  it("rollbackOntology POSTs seq and the plan's fingerprint and returns the report typed", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(report)]);
    const res = await client.rollbackOntology(2, "f".repeat(64), { tenant: "globex" });
    expect(res.rollback.matches).toBe(true);
    expect(res.rollback.applied[0]!.new_id).toBe("c9");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_ontology/rollback?tenant=globex");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ seq: 2, plan_fingerprint: "f".repeat(64) });
  });

  it("rollbackOntology throws plan_stale with the current fingerprint in its body", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "plan_stale", error: "rollback: plan_stale: …", fingerprint: "e".repeat(64) }, 409),
    ]);
    const err = await client.rollbackOntology(2, "f".repeat(64)).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoomcycleError);
    expect((err as LoomcycleError).status).toBe(409);
    expect((err as LoomcycleError).code).toBe("plan_stale");
    expect(JSON.parse((err as LoomcycleError).bodyText!).fingerprint).toBe("e".repeat(64));
  });
});
