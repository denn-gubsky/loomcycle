/**
 * The access-control management surface (RFC EY / EZ): groups, rule lists,
 * bindings, the AuthZEN decision routes, and the AclDeniedError a rule's
 * refusal raises.
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse } from "./helpers.js";
import {
  AclDeniedError,
  InsufficientScopeError,
  InvalidArgumentError,
  LoomcycleError,
  NotFoundError,
  SessionBusyError,
} from "../src/errors.js";

const BASE = "http://test-loomcycle:8787";

function call(fetchMock: ReturnType<typeof makeClient>["fetchMock"], i = 0) {
  const c = fetchMock.mock.calls[i]!;
  const init = c[1] as RequestInit;
  return {
    url: c[0] as string,
    method: init.method ?? "GET",
    body: init.body === undefined ? undefined : JSON.parse(init.body as string),
  };
}

describe("groups", () => {
  it("listGroups reads GET /v1/_groups with the admin tenant focus", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        groups: [
          { tenant_id: "t1", name: "ops", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" },
          { tenant_id: "t1", name: "sre", members: ["alice"], source: "config" },
        ],
      }),
    ]);
    const res = await client.listGroups({ tenant: "t1" });
    expect(call(fetchMock).url).toBe(`${BASE}/v1/_groups?tenant=t1`);
    expect(call(fetchMock).method).toBe("GET");
    expect(res.groups[1]!.source).toBe("config");
    expect(res.groups[1]!.created_at).toBeUndefined();
  });

  it("getGroup / getGroupEffective url-encode the name", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ group: { name: "a b" }, members: [], contained_in: ["a b"] }),
      jsonResponse({ group: "a b", users: ["u1"], groups: [] }),
    ]);
    const detail = await client.getGroup("a b");
    const eff = await client.getGroupEffective("a b");
    expect(call(fetchMock, 0).url).toBe(`${BASE}/v1/_groups/a%20b`);
    expect(call(fetchMock, 1).url).toBe(`${BASE}/v1/_groups/a%20b/effective`);
    expect(detail.contained_in).toEqual(["a b"]);
    expect(eff.users).toEqual(["u1"]);
  });

  it("createGroup posts the body; deleteGroup deletes and returns the echo", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "ops", description: "d", created_at: "x", updated_at: "x" }, 201),
      jsonResponse({ deleted: "ops" }),
    ]);
    const row = await client.createGroup({ name: "ops", description: "d" }, { tenant: "t2" });
    const del = await client.deleteGroup("ops");
    expect(call(fetchMock, 0)).toEqual({
      url: `${BASE}/v1/_groups?tenant=t2`,
      method: "POST",
      body: { name: "ops", description: "d" },
    });
    expect(row.name).toBe("ops");
    expect(call(fetchMock, 1)).toMatchObject({ url: `${BASE}/v1/_groups/ops`, method: "DELETE" });
    expect(del.deleted).toBe("ops");
  });

  it("addGroupMember posts {kind,id}; removeGroupMember deletes the edge path", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ group: "ops", kind: "user", id: "u@x" }, 201),
      jsonResponse({ group: "ops", kind: "user", id: "u@x", removed: true }),
    ]);
    await client.addGroupMember("ops", { kind: "user", id: "u@x" });
    const rm = await client.removeGroupMember("ops", "user", "u@x", { tenant: "t" });
    expect(call(fetchMock, 0)).toEqual({
      url: `${BASE}/v1/_groups/ops/members`,
      method: "POST",
      body: { kind: "user", id: "u@x" },
    });
    expect(call(fetchMock, 1)).toMatchObject({
      url: `${BASE}/v1/_groups/ops/members/user/u%40x?tenant=t`,
      method: "DELETE",
    });
    expect(rm.removed).toBe(true);
  });
});

describe("rule lists", () => {
  const rules = [{ effect: "deny" as const, actions: ["run"], resources: ["agent:*"] }];

  it("each typed helper posts its op to /v1/_acldef", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ acls: [], builtin: [] }),
      jsonResponse({ name: "ops", rules, version: 1 }),
      jsonResponse({ name: "ops", rules, version: 2 }, 201),
      jsonResponse({ retired: "ops" }),
      jsonResponse({ valid: true, rules: 1, catalog_version: 1 }),
      jsonResponse({ catalog_version: 1, types: [{ type: "agent", verbs: ["run"] }], transports: ["http"] }),
    ]);
    await client.listAclDefs({ includeRetired: true, tenant: "t" });
    await client.getAclDef("ops");
    const put = await client.putAclDef({ name: "ops", rules, description: "d" });
    await client.retireAclDef("ops");
    const v = await client.validateAclRules(rules);
    const cat = await client.getAclCatalog();

    const bodies = fetchMock.mock.calls.map((_, i) => call(fetchMock, i));
    for (const b of bodies) expect(b.method).toBe("POST");
    expect(bodies[0]!.url).toBe(`${BASE}/v1/_acldef?tenant=t`);
    expect(bodies.slice(1).every((b) => b.url === `${BASE}/v1/_acldef`)).toBe(true);
    expect(bodies.map((b) => b.body)).toEqual([
      { op: "list", include_retired: true },
      { op: "get", name: "ops" },
      { op: "put", name: "ops", rules, description: "d" },
      { op: "retire", name: "ops" },
      { op: "validate", rules },
      { op: "catalog" },
    ]);
    expect(put.version).toBe(2);
    expect(v.valid).toBe(true);
    expect(cat.types[0]!.verbs).toEqual(["run"]);
  });

  it("listAclDefs leaves include_retired out unless asked", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ acls: [], builtin: [] })]);
    await client.listAclDefs();
    expect(call(fetchMock).body).toEqual({ op: "list" });
  });

  it("aclDef op names reads GET /v1/_acldef/names, where HTTP serves it", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ names: ["a", "b"] }),
      jsonResponse({ names: ["c"] }),
    ]);
    const viaOp = (await client.aclDef({ op: "names" }, { tenant: "t" })) as { names: string[] };
    const direct = await client.listAclDefNames();
    expect(call(fetchMock, 0)).toMatchObject({ url: `${BASE}/v1/_acldef/names?tenant=t`, method: "GET" });
    expect(call(fetchMock, 1)).toMatchObject({ url: `${BASE}/v1/_acldef/names`, method: "GET" });
    expect(viaOp.names).toEqual(["a", "b"]);
    expect(direct.names).toEqual(["c"]);
  });

  it("an invalid rule set raises InvalidArgumentError with code acl_invalid", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "acl_invalid", error: "rule 0: unknown verb" }, 400),
    ]);
    const err = await client.validateAclRules(rules).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InvalidArgumentError);
    expect((err as InvalidArgumentError).code).toBe("acl_invalid");
  });
});

describe("bindings", () => {
  it("listAclBindings sends include_retired=1 only when asked", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ bindings: [{ acl: "ops", subject_kind: "tenant", source: "config" }] }),
      jsonResponse({ bindings: [] }),
    ]);
    const res = await client.listAclBindings({ includeRetired: true, tenant: "t" });
    await client.listAclBindings();
    expect(call(fetchMock, 0).url).toBe(`${BASE}/v1/_aclbinding?include_retired=1&tenant=t`);
    expect(call(fetchMock, 1).url).toBe(`${BASE}/v1/_aclbinding`);
    expect(res.bindings[0]!.binding_id).toBeUndefined();
  });

  it("createAclBinding leaves an empty subject_id / expires_at out", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ binding_id: "b1", acl: "ops", subject_kind: "tenant", created_at: "x" }, 201),
    ]);
    const row = await client.createAclBinding({ acl: "ops", subject_kind: "tenant", subject_id: "", expires_at: "" });
    expect(call(fetchMock).body).toEqual({ acl: "ops", subject_kind: "tenant" });
    expect(row.binding_id).toBe("b1");
    expect(row.expires_at).toBeUndefined();
  });

  it("deleteAclBinding deletes /v1/_aclbinding/{id}", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ retired: "b/1" })]);
    const res = await client.deleteAclBinding("b/1");
    expect(call(fetchMock)).toMatchObject({ url: `${BASE}/v1/_aclbinding/b%2F1`, method: "DELETE" });
    expect(res.retired).toBe("b/1");
  });

  it("an unknown binding id is a NotFoundError", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "acl_binding_not_found", error: "no live binding of that id" }, 404),
    ]);
    await expect(client.deleteAclBinding("nope")).rejects.toBeInstanceOf(NotFoundError);
  });
});

describe("authz decisions", () => {
  const req = { action: { name: "run" }, resource: { type: "agent", id: "reviewer" } };
  const decision = { decision: false, outcome: "no_match", mode: "compat" };

  it("each question posts to its /v1/_authz route", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse(decision),
      jsonResponse({ evaluations: [decision] }),
      jsonResponse({ decision, traces: null, missing_acls: null, applied_bindings: null, groups: null }),
      jsonResponse({ results: [{ request: req, now: decision, with_draft: decision, changed: false }], missing_acls: null }),
    ]);
    const d = await client.authzEvaluation(req, { tenant: "t" });
    const batch = await client.authzEvaluations({
      action: { name: "run" },
      evaluations: [{ resource: { type: "agent", id: "a" } }],
      options: { evaluations_semantic: "deny_on_first_deny" },
    });
    const ex = await client.authzExplain(req);
    const sim = await client.authzSimulate({
      acls: [{ name: "draft", rules: [{ effect: "allow", actions: ["run"], resources: ["agent:*"] }] }],
      bindings: [{ acl: "draft", subject_kind: "user", subject_id: "me" }],
      requests: [req],
    });
    expect(fetchMock.mock.calls.map((_, i) => call(fetchMock, i).url)).toEqual([
      `${BASE}/v1/_authz/evaluation?tenant=t`,
      `${BASE}/v1/_authz/evaluations`,
      `${BASE}/v1/_authz/explain`,
      `${BASE}/v1/_authz/simulate`,
    ]);
    expect(call(fetchMock, 0).body).toEqual(req);
    expect(call(fetchMock, 1).body.options).toEqual({ evaluations_semantic: "deny_on_first_deny" });
    expect(d.outcome).toBe("no_match");
    expect(batch.evaluations).toHaveLength(1);
    expect(ex.traces).toBeNull();
    expect(sim.results[0]!.changed).toBe(false);
  });
});

describe("whoami access-control fields", () => {
  it("carries acl_mode, groups and acl_version when the server sends them", async () => {
    const { client } = makeClient([
      jsonResponse({
        tenant_id: "t", subject: "s", scopes: [], is_admin: false, legacy: false,
        acl_mode: "shadow", groups: ["ops"], acl_version: 7,
      }),
      jsonResponse({ tenant_id: "t", subject: "s", scopes: [], is_admin: false, legacy: false }),
    ]);
    const me = await client.whoami();
    const old = await client.whoami();
    expect(me.acl_mode).toBe("shadow");
    expect(me.groups).toEqual(["ops"]);
    expect(me.acl_version).toBe(7);
    expect(old.acl_mode).toBeUndefined();
  });
});

describe("access-control refusals", () => {
  it("a 403 acl_denied is an AclDeniedError carrying action, resource and reasons", async () => {
    const { client } = makeClient([
      jsonResponse(
        {
          code: "acl_denied",
          error: "author def:acl/ops is denied by lockdown rule 0",
          action: "author",
          resource: "def:acl/ops",
          reasons: [
            { acl: "lockdown", version: 3, rule: 0, effect: "deny" },
            { acl: 7, rule: "x", effect: "deny" }, // malformed: dropped
          ],
        },
        403,
      ),
    ]);
    const err = await client.putAclDef({ name: "ops", rules: [] }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AclDeniedError);
    expect(err).toBeInstanceOf(LoomcycleError);
    const e = err as AclDeniedError;
    expect(e.code).toBe("acl_denied");
    expect(e.status).toBe(403);
    expect(e.action).toBe("author");
    expect(e.resource).toBe("def:acl/ops");
    expect(e.reasons).toEqual([{ acl: "lockdown", version: 3, rule: 0, effect: "deny" }]);
    expect(e.retryable).toBe(false);
  });

  it("an acl_denied with no rule (enforce, no match) has empty reasons", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "acl_denied", error: "no rule allows read principal:group:*", action: "read", resource: "principal:group:*" }, 403),
    ]);
    const e = (await client.listGroups().catch((x: unknown) => x)) as AclDeniedError;
    expect(e).toBeInstanceOf(AclDeniedError);
    expect(e.reasons).toEqual([]);
  });

  it("the other 403s keep their classes", async () => {
    const { client } = makeClient([
      jsonResponse({ code: "insufficient_scope", error: "x", required_scope: "substrate:tenant" }, 403),
      jsonResponse({ code: "acl_self_elevation", error: "refused: a binding to your own subject" }, 403),
    ]);
    const scope = await client.listGroups().catch((x: unknown) => x);
    expect(scope).toBeInstanceOf(InsufficientScopeError);
    expect((scope as InsufficientScopeError).requiredScope).toBe("substrate:tenant");
    const self = await client.createAclBinding({ acl: "a", subject_kind: "user", subject_id: "me" }).catch((x: unknown) => x);
    expect(self).not.toBeInstanceOf(AclDeniedError);
    expect(self).toBeInstanceOf(LoomcycleError);
    expect((self as LoomcycleError).code).toBe("acl_self_elevation");
  });

  it("a group_external 409 naming a 'session' group is not a SessionBusyError", async () => {
    const { client } = makeClient([
      jsonResponse(
        { code: "group_external", error: 'group "session-ops" is synced from an identity provider (external_id "x"); manage its members there' },
        409,
      ),
    ]);
    const err = await client.addGroupMember("session-ops", { kind: "user", id: "u" }).catch((x: unknown) => x);
    expect(err).not.toBeInstanceOf(SessionBusyError);
    expect((err as LoomcycleError).code).toBe("group_external");
    expect((err as LoomcycleError).status).toBe(409);
  });
});
