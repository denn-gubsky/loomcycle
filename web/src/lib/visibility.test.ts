import { describe, expect, it } from "vitest";
import { canSee, hasTenantScope, TENANT_SCOPE } from "./visibility";

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
