import { describe, expect, it } from "vitest";
import { MENU_LINKS, settingsMenuItems } from "../lib/settingsSections";
import { NAV_ITEMS } from "./navItems";

describe("NAV_ITEMS", () => {
  it("has no users entry — Users is reached from the settings menu", () => {
    expect(NAV_ITEMS.map((i) => i.to)).not.toContain("/users");
    expect(NAV_ITEMS.map((i) => i.label)).not.toContain("users");
  });

  it("offers no page from both the left nav and the settings menu", () => {
    const nav = new Set(NAV_ITEMS.map((i) => i.to));
    expect(MENU_LINKS.filter((l) => nav.has(l.href))).toEqual([]);
  });

  it("still leaves Users reachable for the roles the nav entry served", () => {
    const hrefs = (isAdmin: boolean, tenant: boolean) =>
      settingsMenuItems(isAdmin, tenant).map((i) => i.href);
    expect(hrefs(true, false)).toContain("/users");
    expect(hrefs(false, true)).toContain("/users");
    expect(hrefs(false, false)).not.toContain("/users");
  });

  it("gives every entry a distinct destination", () => {
    const tos = NAV_ITEMS.map((i) => i.to);
    expect(new Set(tos).size).toBe(tos.length);
  });
});
