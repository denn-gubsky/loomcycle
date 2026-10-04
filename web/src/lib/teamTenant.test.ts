import { describe, expect, it } from "vitest";
import type { Principal } from "../api";
import { ownTenantOf, sameTeam, teamReach, teamTenantOf } from "./teamTenant";

const principal = (p: Partial<Principal>): Principal => ({
  tenant_id: "", subject: "op", scopes: ["substrate:admin"], is_admin: true, legacy: false, ...p,
});

describe("ownTenantOf", () => {
  it("is the principal's tenant", () => {
    expect(ownTenantOf(principal({ tenant_id: "acme" }))).toBe("acme");
    expect(ownTenantOf(principal({ tenant_id: "" }))).toBe("");
  });

  it("is the shared tenant with no bearer configured, whatever the identity read reports", () => {
    expect(ownTenantOf(principal({ tenant_id: "default", open_mode: true }))).toBe("");
    expect(ownTenantOf(null)).toBe("");
  });
});

describe("sameTeam", () => {
  it("tells two tenants' teams of one name apart", () => {
    const shared: { name: string; tenant_id?: string } = { name: "pcparts" };
    const dev = { name: "pcparts", tenant_id: "loomcycle-dev" };
    expect(teamTenantOf(shared)).toBe("");
    expect(sameTeam(dev, "pcparts", "loomcycle-dev")).toBe(true);
    expect(sameTeam(shared, "pcparts", "loomcycle-dev")).toBe(false);
    expect(sameTeam(shared, "pcparts", "")).toBe(true);
  });
});

describe("teamReach", () => {
  it("leaves everything on for a team in the caller's own tenant", () => {
    expect(teamReach("", "")).toEqual({ byName: true, save: true, notice: "" });
    expect(teamReach("acme", "acme")).toEqual({ byName: true, save: true, notice: "" });
  });

  it("turns off run, save and delete for another tenant's team, naming the tenant", () => {
    const r = teamReach("loomcycle-dev", "");
    expect(r.byName).toBe(false);
    expect(r.save).toBe(false);
    expect(r.notice).toContain('tenant "loomcycle-dev"');
    expect(r.notice).toContain("sign in with a token");
  });

  it("keeps save for a shared team, which saves a copy in the caller's tenant", () => {
    const r = teamReach("", "acme");
    expect(r.byName).toBe(false);
    expect(r.save).toBe(true);
    expect(r.notice).toContain("shared tenant");
  });
});
