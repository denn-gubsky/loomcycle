import { describe, expect, it } from "vitest";
import {
  LOGOUT_HREF,
  SECTIONS,
  isMenuItemCurrent,
  resolveSection,
  settingsHref,
  settingsMenuItems,
  visibleSections,
} from "./settingsSections";

const labels = (items: { label: string }[]) => items.map((i) => i.label);

describe("settingsMenuItems", () => {
  it("gives an admin every section, then Log out last", () => {
    expect(labels(settingsMenuItems(true, false))).toEqual([
      "Credentials",
      "Limits",
      "Routing",
      "Ontology",
      "Retention",
      "Erasure",
      "Access",
      "Tokens",
      "Presets",
      "Runtime",
      "Lanes",
      "Maintenance",
      "Health",
      "Users",
      "Log out",
    ]);
  });

  it("gives a tenant operator the tenant-gated sections only, then Log out", () => {
    expect(labels(settingsMenuItems(false, true))).toEqual([
      "Credentials",
      "Limits",
      "Routing",
      "Ontology",
      "Retention",
      "Erasure",
      "Access",
      "Users",
      "Log out",
    ]);
  });

  it("links Users as a page of its own, after the sections and not as a hub tab", () => {
    for (const [isAdmin, hasTenant] of [
      [true, false],
      [false, true],
    ] as const) {
      const items = settingsMenuItems(isAdmin, hasTenant);
      expect(items.at(-2)).toEqual({ kind: "link", id: "users", label: "Users", href: "/users" });
    }
    // Not a tab: the hub must not grow a section the menu merely links to.
    expect(SECTIONS.map((s) => s.id as string)).not.toContain("users");
    expect(resolveSection("users", visibleSections(true, false))).toBe("credentials");
  });

  it("keeps Users from a delegated user, who cannot call the users API", () => {
    expect(settingsMenuItems(false, false).some((i) => i.kind === "link")).toBe(false);
  });

  it("still lets a delegated user log out, with no section to administer", () => {
    // The standalone sign-out button is gone; this row is the only way out.
    expect(settingsMenuItems(false, false)).toEqual([
      { kind: "logout", label: "Log out", href: LOGOUT_HREF },
    ]);
  });

  it("lists exactly the sections the hub shows the same viewer", () => {
    for (const [isAdmin, hasTenant] of [
      [true, false],
      [false, true],
      [false, false],
    ] as const) {
      const menu = settingsMenuItems(isAdmin, hasTenant).flatMap((i) =>
        i.kind === "section" ? [i.id] : [],
      );
      expect(menu).toEqual(visibleSections(isAdmin, hasTenant).map((s) => s.id));
    }
  });
});

describe("isMenuItemCurrent", () => {
  const items = settingsMenuItems(true, false);
  const current = (pathname: string) =>
    items.filter((i) => isMenuItemCurrent(i, pathname)).map((i) => i.label);

  it("marks the open section, the Users page, and never Log out", () => {
    expect(current("/settings/limits")).toEqual(["Limits"]);
    expect(current("/users")).toEqual(["Users"]);
    expect(current("/agents")).toEqual([]);
  });

  it("does not mistake a path that merely starts like a linked page", () => {
    expect(current("/users-archive")).toEqual([]);
  });
});

describe("settings deep links", () => {
  it("links each section at /settings/<id>", () => {
    expect(settingsHref("limits")).toBe("/settings/limits");
  });

  it("round-trips every section's link back to that section for an admin", () => {
    const all = visibleSections(true, false);
    expect(all).toHaveLength(SECTIONS.length);
    for (const s of SECTIONS) {
      const param = settingsHref(s.id).replace("/settings/", "");
      expect(resolveSection(param, all)).toBe(s.id);
    }
  });

  it("opens the viewer's first tab for a bare /settings or an unknown section", () => {
    const tenant = visibleSections(false, true);
    expect(resolveSection(undefined, tenant)).toBe("credentials");
    expect(resolveSection("no-such-tab", tenant)).toBe("credentials");
  });

  it("does not open an admin-only section for a tenant operator", () => {
    expect(resolveSection("tokens", visibleSections(false, true))).toBe("credentials");
    expect(resolveSection("tokens", visibleSections(true, false))).toBe("tokens");
  });

  it("resolves nothing for a viewer with no sections", () => {
    expect(resolveSection("limits", visibleSections(false, false))).toBeUndefined();
  });
});

describe("settings visibility from the server's capability summary", () => {
  const all = Object.fromEntries(
    [...SECTIONS.map((s) => s.id as string), "users"].map((id) => [id, true]),
  );

  it("hides Access from an operator denied read def:acl/*", () => {
    const op = { ...all, access: false, tokens: false, presets: false, runtime: false, lanes: false, maintenance: false, health: false };
    expect(visibleSections(false, true, op).map((s) => s.id)).not.toContain("access");
    expect(labels(settingsMenuItems(false, true, op))).not.toContain("Access");
    // The rest of the operator's tabs are unaffected.
    expect(visibleSections(false, true, op).map((s) => s.id)).toContain("limits");
  });

  it("gives an admin every section and Users", () => {
    expect(visibleSections(true, false, all)).toEqual(SECTIONS);
    expect(labels(settingsMenuItems(true, false, all))).toContain("Users");
  });

  it("governs the Users link by the server's answer too", () => {
    expect(labels(settingsMenuItems(false, true, { ...all, users: false }))).not.toContain("Users");
    expect(labels(settingsMenuItems(false, false, { users: true }))).toContain("Users");
  });

  it("falls back to the role rule without a summary", () => {
    expect(visibleSections(false, true, undefined)).toEqual(visibleSections(false, true));
  });
});
