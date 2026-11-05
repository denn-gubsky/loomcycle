import { describe, expect, it } from "vitest";
import type { Principal } from "../api";
import { canDecideOntology } from "./ontologyAuthority";

const principal = (scopes: string[], extra: Partial<Principal> = {}): Principal =>
  ({ tenant_id: "acme", subject: "s", scopes, is_admin: false, legacy: false, ...extra }) as Principal;

describe("canDecideOntology", () => {
  it("offers confirm, adopt and accept/reject to the operator plane only", () => {
    expect(canDecideOntology(principal(["substrate:tenant"]))).toBe(true);
    expect(canDecideOntology(principal([], { is_admin: true }))).toBe(true);
    expect(canDecideOntology(principal([], { legacy: true }))).toBe(true);
    expect(canDecideOntology(principal([], { open_mode: true }))).toBe(true);
    // A member: the server answers these routes 403.
    expect(canDecideOntology(principal(["runs:create", "runs:read", "channel:publish", "channel:read"]))).toBe(false);
    expect(canDecideOntology(principal(["substrate:user"]))).toBe(false);
    expect(canDecideOntology(null)).toBe(false);
  });
});
