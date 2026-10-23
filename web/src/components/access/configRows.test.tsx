import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { PolicyEditor, PolicyList } from "./PoliciesTab";
import { BindingTable, bindingKey } from "./BindingsTab";
import { GroupPanel, GroupTable, loadMembersIndex } from "./GroupsTab";
import { slotsFromRules, type AclCatalog } from "../../lib/aclRules";
import type { AclBinding, AclDefRow, GroupRow } from "../../api";

// Entries the config declares (`acls:` / `groups:` / `acl_bindings:`) arrive
// in the lists marked "source":"config". The server has no row to write for
// them, so the console offers no write: like a built-in, they are shown, can
// be opened and simulated, and say why they are read-only.

const catalog: AclCatalog = {
  catalog_version: 1,
  transports: ["http"],
  types: [{ type: "tool", verbs: ["invoke"] }],
};
const noop = () => {};
const html = (el: Parameters<typeof renderToStaticMarkup>[0]) => renderToStaticMarkup(el);
const rules = [{ effect: "deny" as const, actions: ["invoke"], resources: ["tool:Bash"] }];

describe("config-declared rule lists", () => {
  const stored: AclDefRow = {
    name: "no-shell",
    version: 2,
    content_sha256: "x",
    catalog_version: 1,
    rules,
    created_at: "2026-10-23T00:00:00Z",
    updated_at: "2026-10-23T00:00:00Z",
  };
  const fromConfig: AclDefRow = { name: "yaml-list", rules, source: "config" };

  it("lists a config list beside a stored one, marked read-only, with no version", () => {
    const out = html(createElement(PolicyList, { rows: [stored, fromConfig], builtin: [], onOpen: noop, onOpenBuiltin: noop }));
    expect(out).toContain("<code>yaml-list</code>");
    expect(out).toContain("config · read-only");
    expect(out).toContain(">v2<");
    // The stored list is live, the config one is not given a stored status.
    expect(out.match(/config · read-only/g)?.length).toBe(1);
    expect(out).toContain(">live<");
  });

  it("opens a config list read-only: no save, no retire, but simulate unsaved still works", () => {
    const out = html(
      createElement(PolicyEditor, {
        catalog,
        onClose: noop,
        onSaved: noop,
        onRetired: noop,
        onSimulate: noop,
        initial: { name: "yaml-list", description: "", isNew: false, builtin: false, config: true, retired: false, slots: slotsFromRules(catalog, rules) },
      }),
    );
    expect(out).toContain("config · read-only");
    expect(out).not.toContain(">save</button>");
    expect(out).not.toContain(">retire</button>");
    expect(out).toContain("simulate unsaved");
    expect(out).toMatch(/<fieldset class="acl-rule acl-rule-deny" disabled="">/);
  });
});

describe("config-declared groups", () => {
  const groups: GroupRow[] = [
    { name: "support", created_at: "2026-10-23T00:00:00Z" },
    { name: "yaml-group", source: "config", members: ["alice"], groups: ["support"] },
  ];

  it("has no delete for a config group, and a pill saying why", () => {
    const out = html(createElement(GroupTable, { groups, idx: {}, open: null, onToggle: noop, onDelete: noop }));
    expect(out).toContain("config · read-only");
    expect(out.match(/title="Delete group"/g)?.length).toBe(1);
  });

  it("offers no member add or remove on a config group", () => {
    const idx = { "yaml-group": [{ group: "yaml-group", member_kind: "user" as const, member_id: "alice", added_at: "" }] };
    const out = html(createElement(GroupPanel, { group: groups[1], idx, allGroups: ["support", "yaml-group"], onChanged: async () => {} }));
    expect(out).toContain("declared in the config");
    expect(out).not.toContain('title="Remove member"');
    expect(out).not.toContain('aria-label="member id"');
  });

  it("indexes a config group's members from the list itself, not a per-group fetch the server would 404", async () => {
    const idx = await loadMembersIndex([groups[1]]);
    expect(idx["yaml-group"].map((m) => `${m.member_kind}:${m.member_id}`)).toEqual(["user:alice", "group:support"]);
  });
});

describe("config-declared bindings", () => {
  const stored: AclBinding = { binding_id: "aclb_1", acl: "no-shell", subject_kind: "user", subject_id: "alice", created_at: "", created_by: "ops" };
  const a: AclBinding = { acl: "yaml-list", subject_kind: "group", subject_id: "support", source: "config" };
  const b: AclBinding = { acl: "yaml-list", subject_kind: "tenant", source: "config" };

  it("keys a binding with no id on what it binds, so two config bindings do not collide", () => {
    expect(bindingKey(stored)).toBe("aclb_1");
    expect(bindingKey(a)).not.toBe(bindingKey(b));
    expect(bindingKey(a)).toContain("yaml-list");
  });

  it("offers no remove on a config binding, and marks it", () => {
    const out = html(createElement(BindingTable, { bindings: [stored, a, b], onRetire: noop }));
    expect(out.match(/config · read-only/g)?.length).toBe(2);
    expect(out.match(/>remove<\/button>/g)?.length).toBe(1);
    expect(out).toContain(">never<");
  });
});
