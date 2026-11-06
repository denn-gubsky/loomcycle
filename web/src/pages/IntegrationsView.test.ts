import { describe, expect, it } from "vitest";
import { canDeclareTrust } from "./IntegrationsView";
import type { Principal } from "../api";

const principal = (over: Partial<Principal> = {}): Principal => ({
  tenant_id: "acme",
  subject: "bob",
  scopes: [],
  is_admin: false,
  legacy: false,
  ...over,
});

describe("canDeclareTrust", () => {
  it("is the operator plane: admin, legacy, open mode, a tenant operator", () => {
    expect(canDeclareTrust(principal({ is_admin: true }))).toBe(true);
    expect(canDeclareTrust(principal({ legacy: true }))).toBe(true);
    expect(canDeclareTrust(principal({ open_mode: true }))).toBe(true);
    expect(canDeclareTrust(principal({ scopes: ["substrate:tenant"] }))).toBe(true);
  });
  it("is nobody else", () => {
    expect(canDeclareTrust(principal({ scopes: ["runs:create"] }))).toBe(false);
    expect(canDeclareTrust(null)).toBe(false);
  });
});
