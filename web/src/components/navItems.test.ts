import { describe, expect, it } from "vitest";
import { ACCOUNT_LINKS, accountMenuItems } from "../lib/accountMenu";
import {
  NAV_GROUPS,
  NAV_ITEMS,
  isNavItemCurrent,
  visibleGroupItems,
  visibleNavItems,
  visibleNavTree,
} from "./navItems";

const byId = (id: string) => NAV_ITEMS.find((i) => i.id === id)!;

describe("NAV_ITEMS and NAV_GROUPS", () => {
  it("puts every page in exactly one group, and names only real pages", () => {
    const grouped = NAV_GROUPS.flatMap((g) => g.items);
    expect([...grouped].sort()).toEqual(NAV_ITEMS.map((i) => i.id).sort());
    expect(new Set(grouped).size).toBe(grouped.length);
  });

  it("gives every page a distinct id and destination, and every group a distinct key", () => {
    const ids = NAV_ITEMS.map((i) => i.id);
    const tos = NAV_ITEMS.map((i) => i.to);
    const keys = NAV_GROUPS.map((g) => g.key);
    expect(new Set(ids).size).toBe(ids.length);
    expect(new Set(tos).size).toBe(tos.length);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("offers no page from both the left nav and the account menu", () => {
    const nav = new Set(NAV_ITEMS.map((i) => i.to));
    expect(ACCOUNT_LINKS.filter((l) => nav.has(l.href))).toEqual([]);
  });

  it("shows the Path tree as documents, under the id the server knows it by", () => {
    expect(byId("paths")).toMatchObject({ to: "/documents", label: "documents" });
  });

  it("gathers every access page under /access, in one group", () => {
    const access = NAV_GROUPS.find((g) => g.key === "g-access")!;
    expect(access.items).toEqual(["users", "groups", "policies", "bindings", "simulator", "tokens"]);
    for (const id of access.items) expect(byId(id).to).toBe("/access/" + id);
  });

  it("files each former Settings section under its group, at /<section>", () => {
    const groupOf = (id: string) => NAV_GROUPS.find((g) => g.items.includes(id))?.key;
    const want: Record<string, string> = {
      ontology: "g-knowledge",
      credentials: "g-connections",
      limits: "g-cost",
      routing: "g-cost",
      retention: "g-compliance",
      erasure: "g-compliance",
      presets: "g-system",
      runtime: "g-system",
      lanes: "g-system",
      maintenance: "g-system",
      health: "g-system",
    };
    for (const [id, group] of Object.entries(want)) {
      expect(groupOf(id)).toBe(group);
      expect(byId(id).to).toBe("/" + id);
    }
    // The tenant's credentials and the viewer's own are told apart by name.
    expect(byId("credentials").label).toBe("tenant credentials");
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
    expect(nav).toContain("new run");
  });

  it("shows an admin every page, in menu order", () => {
    const inMenuOrder = NAV_GROUPS.flatMap((g) => g.items).map(byId);
    expect(visibleNavItems(true, false, allTrue)).toEqual(inMenuOrder);
    // Open mode, where the server answers every page true, is the same.
    expect(visibleNavItems(false, false, allTrue)).toEqual(inMenuOrder);
  });

  it("follows the server over the role: an admin it denies a page loses the page", () => {
    expect(labelsOf(visibleNavItems(true, false, { ...allTrue, audit: false }))).not.toContain("audit");
  });

  it("falls back to the role rule without a summary (an older server)", () => {
    expect(labelsOf(visibleNavItems(false, false, undefined))).toEqual(["new run", "all runs"]);
    expect(labelsOf(visibleNavItems(false, true, undefined))).not.toContain("tokens");
    expect(visibleNavItems(true, false, undefined)).toHaveLength(NAV_ITEMS.length);
  });
});

describe("visibleNavTree", () => {
  it("drops a group, and a zone, the viewer can open nothing in", () => {
    const tree = visibleNavTree(false, false, undefined);
    expect(tree.map((z) => z.zone)).toEqual(["workspace"]);
    expect(tree[0].groups.map((g) => g.group.key)).toEqual(["g-runs"]);
    expect(labelsOf(tree[0].groups[0].items)).toEqual(["new run", "all runs"]);
  });

  it("gives an operator both zones, with the access group minus admin-only tokens", () => {
    const tree = visibleNavTree(false, true, undefined);
    expect(tree.map((z) => z.zone)).toEqual(["workspace", "admin"]);
    const access = tree[1].groups.find((g) => g.group.key === "g-access")!;
    expect(labelsOf(access.items)).toEqual(["users", "groups", "policies", "bindings", "simulator"]);
  });

  it("gates each access page on its own: an operator denied policies still manages users", () => {
    const op = { ...allTrue, policies: false, tokens: false };
    expect(labelsOf(visibleGroupItems("g-access", false, true, op))).toEqual([
      "users",
      "groups",
      "bindings",
      "simulator",
    ]);
  });
});

describe("isNavItemCurrent", () => {
  it("marks a page on any of its tabs, and a document opened from the tree", () => {
    expect(isNavItemCurrent(byId("library"), "/library/skills")).toBe(true);
    expect(isNavItemCurrent(byId("paths"), "/documents")).toBe(true);
    expect(isNavItemCurrent(byId("paths"), "/documents/abc123")).toBe(true);
    expect(isNavItemCurrent(byId("users"), "/access/users")).toBe(true);
  });

  it("does not mistake a path that merely starts like a page", () => {
    expect(isNavItemCurrent(byId("runs"), "/agents-archive")).toBe(false);
    expect(isNavItemCurrent(byId("users"), "/access/groups")).toBe(false);
  });
});

describe("the account menu", () => {
  it("is every viewer's way to their own pages and out", () => {
    expect(labelsOf(accountMenuItems(false, false, undefined, true))).toEqual([
      "Your account",
      "My credentials",
      "Log out",
    ]);
  });
});
