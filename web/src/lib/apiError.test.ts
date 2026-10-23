import { describe, expect, it } from "vitest";
import { ApiError, apiError, denyingReasons, describeAclDenial, forbiddenKind } from "./apiError";

const aclBody = JSON.stringify({
  code: "acl_denied",
  error: "run agent:secret is denied by interns rule 2",
  action: "run",
  resource: "agent:secret",
  reasons: [{ acl: "interns", version: 3, rule: 2, effect: "deny" }],
});

describe("apiError", () => {
  it("names the action, the resource and the denying list and rule of an acl_denied 403", () => {
    const e = apiError(403, "Forbidden", aclBody);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.message).toBe('403 Forbidden: access denied — may not run agent:secret: denied by list "interns" rule 2');
    expect(e.acl).toEqual({
      action: "run",
      resource: "agent:secret",
      reasons: [{ acl: "interns", version: 3, rule: 2, effect: "deny" }],
    });
    expect(e.code).toBe("acl_denied");
    expect(forbiddenKind(e)).toBe("acl_denied");
  });

  it("says no rule allowed it when an acl_denied carries no reasons (enforce, no match)", () => {
    const body = JSON.stringify({ code: "acl_denied", error: "no rule allows read def:acl/*", action: "read", resource: "def:acl/*" });
    expect(apiError(403, "Forbidden", body).message).toBe(
      "403 Forbidden: access denied — may not read def:acl/*: no rule allows it",
    );
  });

  it("lists every denying rule when several matched", () => {
    expect(
      describeAclDenial({
        action: "invoke",
        resource: "tool:Bash",
        reasons: [
          { acl: "a", rule: 0, effect: "deny" },
          { acl: "b", rule: 4, effect: "deny" },
        ],
      }),
    ).toBe('may not invoke tool:Bash: denied by list "a" rule 0; list "b" rule 4');
  });

  it("keeps the status-line prefix so callers that read the status still can", () => {
    expect(apiError(403, "Forbidden", aclBody).message).toMatch(/^403 /);
  });

  it("renders insufficient_scope as a permission refusal naming the scope", () => {
    const body = JSON.stringify({ code: "insufficient_scope", error: "this route requires substrate:tenant", required_scope: "substrate:tenant" });
    const e = apiError(403, "Forbidden", body);
    expect(e.message).toBe("403 Forbidden: not permitted — this route requires substrate:tenant (needs the substrate:tenant scope)");
    expect(forbiddenKind(e)).toBe("insufficient_scope");
  });

  it("renders acl_self_elevation as a refusal with the server's reason", () => {
    const body = JSON.stringify({ code: "acl_self_elevation", error: "refused: a binding to your own subject would change your own authority" });
    const e = apiError(403, "Forbidden", body);
    expect(e.message).toBe("403 Forbidden: refused — a binding to your own subject would change your own authority");
    expect(forbiddenKind(e)).toBe("acl_self_elevation");
  });

  it("leaves every other error's message byte-identical to the old status line", () => {
    const raw = '{"code":"group_not_found","error":"group not found"}';
    const e = apiError(404, "Not Found", raw);
    expect(e.message).toBe(`404 Not Found: ${raw}`);
    expect(e.code).toBe("group_not_found");
    expect(forbiddenKind(e)).toBeNull();
    const long = "x".repeat(500);
    expect(apiError(500, "Internal Server Error", long).message).toBe(`500 Internal Server Error: ${"x".repeat(200)}`);
  });

  it("uses the server's own message when asked, and still formats an ACL refusal", () => {
    expect(apiError(422, "Unprocessable", '{"code":"x","error":"Documents require SQL Memory"}', { serverMessage: true }).message).toBe(
      "Documents require SQL Memory",
    );
    expect(apiError(403, "Forbidden", aclBody, { serverMessage: true }).message).toBe(
      'access denied — may not run agent:secret: denied by list "interns" rule 2',
    );
  });

  it("survives a body that is not JSON, or JSON of the wrong shape", () => {
    expect(apiError(502, "Bad Gateway", "<html>").message).toBe("502 Bad Gateway: <html>");
    expect(apiError(403, "Forbidden", "null").message).toBe("403 Forbidden: null");
    const odd = JSON.stringify({ code: "acl_denied", reasons: "nope" });
    expect(apiError(403, "Forbidden", odd).message).toBe("403 Forbidden: access denied — may not ? ?: no rule allows it");
  });

  it("classifies a plain Error by nothing", () => {
    expect(forbiddenKind(new Error("403 Forbidden: x"))).toBeNull();
    expect(forbiddenKind("boom")).toBeNull();
  });
});

describe("apiError — what the server actually sends", () => {
  it("names only the denying rules: the reasons also carry the allows that matched", () => {
    // Verbatim shape from a compat-mode refusal of a tenant operator.
    const body = JSON.stringify({
      code: "acl_denied",
      error: "author def:acl/locked-x is denied by no-locked rule 0",
      action: "author",
      resource: "def:acl/locked-x",
      reasons: [
        { acl: "no-locked", version: 1, rule: 0, effect: "deny" },
        { acl: "builtin:tenant-operator", rule: 15, effect: "allow" },
      ],
    });
    const e = apiError(403, "Forbidden", body);
    expect(e.message).toBe('403 Forbidden: access denied — may not author def:acl/locked-x: denied by list "no-locked" rule 0');
    expect(denyingReasons(e.acl!)).toEqual([{ acl: "no-locked", version: 1, rule: 0, effect: "deny" }]);
  });

  it("treats a simulator refusal to name another subject as a refusal", () => {
    const body = JSON.stringify({ code: "authz_subject", error: "naming another subject needs a tenant operator" });
    const e = apiError(403, "Forbidden", body);
    expect(e.message).toBe("403 Forbidden: not permitted — naming another subject needs a tenant operator");
    expect(forbiddenKind(e)).toBe("authz_subject");
    // The same code on a 503 is an outage, not a refusal.
    expect(forbiddenKind(apiError(503, "Service Unavailable", JSON.stringify({ code: "authz_subject", error: "x" })))).toBeNull();
  });
});
