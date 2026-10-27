import { describe, expect, it } from "vitest";
import { NAV_ITEMS } from "../components/navItems";
import { SECTION_IDS, settingsRedirectTarget } from "./SectionPages";

describe("SECTION_IDS", () => {
  it("names a left-nav page at /<section> for every section", () => {
    // A section with no nav entry would render "not available" to everyone.
    for (const id of SECTION_IDS) {
      expect(NAV_ITEMS.find((it) => it.id === id)?.to).toBe("/" + id);
    }
  });
});

describe("settingsRedirectTarget", () => {
  it("sends /settings/<section> to /<section>", () => {
    expect(settingsRedirectTarget("limits")).toBe("/limits");
    expect(settingsRedirectTarget("health")).toBe("/health");
  });

  it("sends a bare or unknown /settings to the start page", () => {
    expect(settingsRedirectTarget(undefined)).toBe("/");
    expect(settingsRedirectTarget("no-such-section")).toBe("/");
    // Access and Tokens left the hub before the rest; their own routes catch them.
    expect(settingsRedirectTarget("tokens")).toBe("/");
  });
});
