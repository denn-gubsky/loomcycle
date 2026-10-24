import { describe, expect, it } from "vitest";
import { canSee, grantOf, hasTenantScope, surfacesOf, TENANT_SCOPE } from "./visibility";

describe("canSee", () => {
  it("shows an `all` surface to every role, including a delegated user", () => {
    expect(canSee("all", false, false)).toBe(true);
  });

  it("shows a `tenant` surface to an operator and to admin, not to a delegated user", () => {
    // The delegated-user case is the one that matters: substrate:user holds none of
    // the tenant scopes, so every call behind a `tenant` surface 403s for them.
    expect(canSee("tenant", false, false)).toBe(false);
    expect(canSee("tenant", false, true)).toBe(true);
    expect(canSee("tenant", true, false)).toBe(true);
  });

  it("admin satisfies tenant, because the server's HasScope says so", () => {
    // If this ever stopped being true in the UI, an admin would lose surfaces the
    // API would happily serve them.
    expect(canSee("tenant", true, false)).toBe(true);
  });

  it("keeps an `admin` surface away from a tenant operator", () => {
    expect(canSee("admin", false, true)).toBe(false);
    expect(canSee("admin", true, true)).toBe(true);
  });
});

describe("hasTenantScope", () => {
  it("reads the operator role off the scope list", () => {
    expect(hasTenantScope([TENANT_SCOPE])).toBe(true);
    expect(hasTenantScope(["runs:read", TENANT_SCOPE])).toBe(true);
  });

  it("is false for a delegated user and for a missing list", () => {
    expect(hasTenantScope(["substrate:user", "runs:read"])).toBe(false);
    expect(hasTenantScope([])).toBe(false);
    expect(hasTenantScope(undefined)).toBe(false);
  });

  it("does not match a scope that merely contains the name", () => {
    // An exact-membership check, not a substring one — "substrate:tenant-readonly"
    // is not the operator scope, and a startsWith/includes bug would grant it.
    expect(hasTenantScope(["substrate:tenant-readonly"])).toBe(false);
  });
});

describe("canSee with the server's capability summary", () => {
  it("lets the server's answer decide over the role, both ways", () => {
    // A member (no tenant scope) the server says may use a tenant surface sees it…
    expect(canSee("tenant", false, false, true)).toBe(true);
    // …and an operator the server says may not (an ACL deny) does not.
    expect(canSee("tenant", false, true, false)).toBe(false);
    expect(canSee("admin", true, false, false)).toBe(false);
  });

  it("falls back to the role rule when the server did not answer for the page", () => {
    expect(canSee("tenant", false, true, undefined)).toBe(true);
    expect(canSee("tenant", false, false, undefined)).toBe(false);
  });
});

describe("grantOf", () => {
  it("reads a page's answer and nothing else", () => {
    expect(grantOf({ channels: false }, "channels")).toBe(false);
    expect(grantOf({ channels: true }, "channels")).toBe(true);
    expect(grantOf({ channels: true }, "memory")).toBeUndefined();
    expect(grantOf(undefined, "channels")).toBeUndefined();
    // An inherited property is not an answer.
    expect(grantOf({}, "toString")).toBeUndefined();
  });
});

describe("surfacesOf", () => {
  it("reads effective.surfaces off the principal, absent on an older server", () => {
    expect(surfacesOf({ effective: { surfaces: { run: true } } })).toEqual({ run: true });
    expect(surfacesOf({})).toBeUndefined();
    expect(surfacesOf(null)).toBeUndefined();
    expect(surfacesOf(undefined)).toBeUndefined();
  });
});
