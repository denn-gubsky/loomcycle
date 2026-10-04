import { describe, expect, it } from "vitest";
import { ALL_TENANTS, filterTenants, moveActive, normaliseTenants } from "./tenantOptions";

const ids = (opts: { id: string }[]) => opts.map((o) => o.id);

describe("normaliseTenants", () => {
  it("sorts, trims and de-duplicates the directory rows", () => {
    const got = normaliseTenants([
      { tenant: "zeta", users: 1, runs: 2 },
      { tenant: " acme ", users: 3, runs: 9 },
      { tenant: "zeta", users: 7, runs: 7 },
    ]);
    expect(got).toEqual([
      { id: "acme", users: 3, runs: 9 },
      { id: "zeta", users: 1, runs: 2 },
    ]);
  });

  it("drops the empty tenant, which would read as a second `all tenants` row", () => {
    expect(ids(normaliseTenants([{ tenant: "" }, { tenant: "  " }, { tenant: "acme" }]))).toEqual([
      "acme",
    ]);
  });

  it("treats a null or missing list as no known tenants", () => {
    // The Go handler serialises an empty slice as null.
    expect(normaliseTenants(null)).toEqual([]);
    expect(normaliseTenants(undefined)).toEqual([]);
  });
});

describe("filterTenants", () => {
  const known = normaliseTenants([
    { tenant: "acme" },
    { tenant: "Acme-eu" },
    { tenant: "beta" },
    { tenant: "meta-acme" },
  ]);

  it("lists every known tenant under `all tenants` when nothing is typed", () => {
    expect(ids(filterTenants(known, "  "))).toEqual(["", "acme", "Acme-eu", "beta", "meta-acme"]);
  });

  it("matches case-insensitively, prefix matches ahead of substring matches", () => {
    expect(ids(filterTenants(known, "ACME"))).toEqual(["", "acme", "Acme-eu", "meta-acme"]);
    expect(ids(filterTenants(known, "eta"))).toEqual(["", "beta", "meta-acme"]);
  });

  it("keeps `all tenants` even when no tenant matches", () => {
    expect(filterTenants(known, "nope")).toEqual([ALL_TENANTS]);
  });
});

describe("moveActive", () => {
  it("enters the list at the first row going down and the last going up", () => {
    expect(moveActive(-1, 3, "ArrowDown")).toBe(0);
    expect(moveActive(-1, 3, "ArrowUp")).toBe(2);
  });

  it("wraps at both ends", () => {
    expect(moveActive(2, 3, "ArrowDown")).toBe(0);
    expect(moveActive(0, 3, "ArrowUp")).toBe(2);
    expect(moveActive(1, 3, "ArrowDown")).toBe(2);
  });

  it("jumps with Home and End", () => {
    expect(moveActive(1, 3, "Home")).toBe(0);
    expect(moveActive(1, 3, "End")).toBe(2);
  });

  it("highlights nothing in an empty list", () => {
    expect(moveActive(0, 0, "ArrowDown")).toBe(-1);
  });
});
