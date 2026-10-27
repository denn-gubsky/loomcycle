import { describe, expect, it } from "vitest";
import {
  LOGOUT_HREF,
  SECTIONS,
  accountMenuItems,
  isMenuItemCurrent,
  resolveSection,
  settingsHref,
  settingsMenuItems,
  visibleSections,
} from "./settingsSections";

const labels = (items: { label: string }[]) => items.map((i) => i.label);

describe("settingsMenuItems", () => {
  it("gives an admin every section, and nothing but sections", () => {
    expect(labels(settingsMenuItems(true, false))).toEqual([
      "Credentials",
      "Limits",
      "Routing",
      "Ontology",
      "Retention",
      "Erasure",
      "Presets",
      "Runtime",
      "Lanes",
      "Maintenance",
      "Health",
    ]);
  });

  it("gives a tenant operator the tenant-gated sections only", () => {
    expect(labels(settingsMenuItems(false, true))).toEqual([
      "Credentials",
      "Limits",
      "Routing",
      "Ontology",
      "Retention",
      "Erasure",
    ]);
  });

  it("is empty for a delegated user, who then gets no gear at all", () => {
    expect(settingsMenuItems(false, false)).toEqual([]);
  });

  it("lists exactly the sections the hub shows the same viewer", () => {
    for (const [isAdmin, hasTenant] of [
      [true, false],
      [false, true],
      [false, false],
    ] as const) {
      const menu = settingsMenuItems(isAdmin, hasTenant).map((i) => (i.kind === "section" ? i.id : ""));
      expect(menu).toEqual(visibleSections(isAdmin, hasTenant).map((s) => s.id));
    }
  });
});

describe("accountMenuItems", () => {
  it("gives a signed-in viewer Your account, their own pages, then Log out", () => {
    expect(accountMenuItems(true, false, undefined, true)).toEqual([
      { kind: "link", id: "account", label: "Your account", href: "/account" },
      { kind: "link", id: "my-credentials", label: "My credentials", href: "/my-credentials" },
      { kind: "logout", label: "Log out", href: LOGOUT_HREF },
    ]);
  });

  it("offers no Your account where nobody is signed in (open mode)", () => {
    expect(labels(accountMenuItems(true, false, undefined, false))).toEqual(["My credentials", "Log out"]);
  });

  it("still lets a viewer the server denies their credentials log out", () => {
    // This menu is the only way out of a session.
    expect(accountMenuItems(false, false, { "my-credentials": false }, false)).toEqual([
      { kind: "logout", label: "Log out", href: LOGOUT_HREF },
    ]);
  });
});

describe("isMenuItemCurrent", () => {
  const items = [...settingsMenuItems(true, false), ...accountMenuItems(true, false, undefined, true)];
  const current = (pathname: string) =>
    items.filter((i) => isMenuItemCurrent(i, pathname)).map((i) => i.label);

  it("marks the open section, the viewer's own page, and never Log out", () => {
    expect(current("/settings/limits")).toEqual(["Limits"]);
    expect(current("/account")).toEqual(["Your account"]);
    expect(current("/my-credentials")).toEqual(["My credentials"]);
    expect(current("/agents")).toEqual([]);
  });

  it("does not mistake a path that merely starts like a linked page", () => {
    expect(current("/account-archive")).toEqual([]);
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
    expect(resolveSection("presets", visibleSections(false, true))).toBe("credentials");
    expect(resolveSection("presets", visibleSections(true, false))).toBe("presets");
  });

  it("resolves nothing for a viewer with no sections", () => {
    expect(resolveSection("limits", visibleSections(false, false))).toBeUndefined();
  });
});

describe("settings visibility from the server's capability summary", () => {
  const all = Object.fromEntries(SECTIONS.map((s) => [s.id as string, true]));

  it("hides a section the server denies, and leaves the rest", () => {
    const op = { ...all, ontology: false, presets: false, runtime: false, lanes: false, maintenance: false, health: false };
    expect(visibleSections(false, true, op).map((s) => s.id)).not.toContain("ontology");
    expect(labels(settingsMenuItems(false, true, op))).not.toContain("Ontology");
    expect(visibleSections(false, true, op).map((s) => s.id)).toContain("limits");
  });

  it("gives an admin every section", () => {
    expect(visibleSections(true, false, all)).toEqual(SECTIONS);
  });

  it("governs My credentials by the server's answer too", () => {
    expect(labels(accountMenuItems(false, false, { "my-credentials": false }, true))).not.toContain("My credentials");
  });

  it("falls back to the role rule without a summary", () => {
    expect(visibleSections(false, true, undefined)).toEqual(visibleSections(false, true));
  });
});
