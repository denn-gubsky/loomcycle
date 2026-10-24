import { describe, expect, it } from "vitest";
import { MENU_LINKS, settingsMenuItems } from "../lib/settingsSections";
import { NAV_ITEMS, visibleNavItems } from "./navItems";

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

// What GET /v1/_me's effective.surfaces says for each class (abridged to the
// pages a test reads; a page the server does not list falls back to the role).
const allTrue = Object.fromEntries(NAV_ITEMS.map((i) => [i.id, true]));
const labelsOf = (items: { label: string }[]) => items.map((i) => i.label);

describe("visibleNavItems", () => {
  it("hides Channels from a member whose ACL denies channels, and shows what the member may use", () => {
    const member = { ...allTrue, channels: false, snapshots: false, activity: false };
    const nav = labelsOf(visibleNavItems(false, false, member));
    expect(nav).not.toContain("channels");
    expect(nav).not.toContain("snapshots");
    // A member without substrate:tenant used to see none of these.
    expect(nav).toContain("library");
    expect(nav).toContain("memory");
    expect(nav).toContain("run");
  });

  it("shows an admin every item", () => {
    expect(visibleNavItems(true, false, allTrue)).toEqual(NAV_ITEMS);
  });

  it("shows every item in open mode, where the server answers every page true", () => {
    expect(visibleNavItems(true, false, allTrue)).toEqual(NAV_ITEMS);
  });

  it("follows the server over the role: an admin it denies a page loses the page", () => {
    expect(labelsOf(visibleNavItems(true, false, { ...allTrue, audit: false }))).not.toContain("audit");
  });

  it("falls back to the role rule without a summary (an older server)", () => {
    expect(labelsOf(visibleNavItems(false, false, undefined))).toEqual(["run", "runs", "credentials"]);
    expect(visibleNavItems(true, false, undefined)).toEqual(NAV_ITEMS);
  });

  it("gives every entry a distinct id", () => {
    const ids = NAV_ITEMS.map((i) => i.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});
