import { describe, expect, it } from "vitest";
import {
  LOGOUT_HREF,
  SECTIONS,
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
      "Tokens",
      "Presets",
      "Runtime",
      "Maintenance",
      "Health",
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
      "Log out",
    ]);
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
