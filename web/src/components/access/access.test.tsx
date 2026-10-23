import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AccessConsoleFrame } from "./AccessConsole";
import { RuleEditor } from "./RuleEditor";
import { PolicyList, PolicyEditor } from "./PoliciesTab";
import { BindingTable } from "./BindingsTab";
import { EffectiveMembers, GroupTable, MembershipTree } from "./GroupsTab";
import { DraftAnswer, SavedAnswer } from "./SimulatorTab";
import { emptyRuleDraft, slotsFromRules, type AclCatalog } from "../../lib/aclRules";
import { membershipTree } from "../../lib/aclAccess";
import type { AclBinding, GroupMember } from "../../api";

// Static renders: effects do not run, so each piece is rendered from the data
// it would be handed.

const catalog: AclCatalog = {
  catalog_version: 1,
  transports: ["http", "webui"],
  types: [
    { type: "agent", verbs: ["run", "read", "spawn"] },
    { type: "memory", verbs: ["read", "write"], scopes: ["user", "tenant"], separator: ":" },
    { type: "tool", verbs: ["invoke"] },
  ],
};
const noop = () => {};
const html = (el: Parameters<typeof renderToStaticMarkup>[0]) => renderToStaticMarkup(el);

describe("AccessConsoleFrame", () => {
  it("shows the mode with its sentence, the tenant it acts in, and the four tabs", () => {
    const out = html(createElement(AccessConsoleFrame, { mode: "shadow", tenantLabel: "acme", tab: "policies", onTab: noop }));
    expect(out).toContain("mode <code>shadow</code>");
    expect(out).toContain("every disagreement is logged and audited");
    expect(out).toContain("tenant: <code>acme</code>");
    for (const t of ["Groups", "Policies", "Bindings", "Simulator"]) expect(out).toContain(`>${t}</button>`);
    expect(out).toContain('class="settings-tab active">Policies');
  });
});

describe("RuleEditor", () => {
  it("offers only the chosen type's verbs as actions", () => {
    const d = { ...emptyRuleDraft(catalog), actions: ["run"] };
    const out = html(createElement(RuleEditor, { catalog, draft: d, index: 0, count: 1, onChange: noop }));
    for (const v of ["run", "read", "spawn"]) expect(out).toContain(`/> ${v}</label>`);
    expect(out).not.toContain("/> invoke</label>");
    expect(out).not.toContain("/> write</label>");
  });

  it("offers a scope picker for a scoped type and no action shared by mixed types", () => {
    const d = {
      ...emptyRuleDraft(catalog),
      resources: [
        { type: "memory", scope: "tenant", name: "" },
        { type: "tool", scope: "", name: "Bash" },
      ],
    };
    const out = html(createElement(RuleEditor, { catalog, draft: d, index: 2, count: 3, onChange: noop }));
    expect(out).toContain('<option value="tenant" selected="">tenant</option>');
    expect(out).toContain("share no action");
    expect(out).toContain("rule 2");
    expect(out).toContain("pick at least one action");
  });

  it("is a disabled fieldset with no edit tools when read-only", () => {
    const out = html(createElement(RuleEditor, { catalog, draft: emptyRuleDraft(catalog), index: 0, count: 1, readOnly: true, onChange: noop, onRemove: noop }));
    expect(out).toMatch(/<fieldset class="acl-rule acl-rule-allow" disabled="">/);
    expect(out).not.toContain("Remove rule");
    expect(out).not.toContain("pick at least one action");
  });
});

describe("PolicyList and PolicyEditor", () => {
  it("lists stored lists, marks the built-in ones read-only, and says what an empty tenant has", () => {
    const out = html(
      createElement(PolicyList, {
        rows: [],
        builtin: [{ name: "builtin:member", rules: [{ effect: "allow", actions: ["run"], resources: ["agent:*"] }] }],
        onOpen: noop,
        onOpenBuiltin: noop,
      }),
    );
    expect(out).toContain("no rule lists in this tenant yet");
    expect(out).toContain("<code>builtin:member</code>");
    expect(out).toContain("built-in · read-only");
  });

  it("opens a built-in list without save or retire, and a stored one with them", () => {
    const rules = [{ effect: "deny" as const, actions: ["invoke"], resources: ["tool:Bash"] }];
    const base = { name: "x", description: "", isNew: false, retired: false, version: 3, slots: slotsFromRules(catalog, rules) };
    const props = { catalog, onClose: noop, onSaved: noop, onRetired: noop, onSimulate: noop };
    const builtin = html(createElement(PolicyEditor, { ...props, initial: { ...base, builtin: true } }));
    expect(builtin).not.toContain(">save</button>");
    expect(builtin).not.toContain(">retire</button>");
    expect(builtin).toContain("simulate unsaved");
    const stored = html(createElement(PolicyEditor, { ...props, initial: { ...base, builtin: false } }));
    expect(stored).toContain(">save</button>");
    expect(stored).toContain(">retire</button>");
    // The text view mirrors the rules in YAML.
    expect(stored).toContain("- effect: deny");
  });
});

describe("BindingTable", () => {
  const b = (over: Partial<AclBinding>): AclBinding => ({ binding_id: "1", acl: "l", subject_kind: "user", subject_id: "alice", created_at: "", ...over });

  it("shows never for no expiry, marks an expired binding, and names a tenant-wide one", () => {
    const now = Date.parse("2026-10-23T12:00:00Z");
    const out = html(
      createElement(BindingTable, {
        now,
        onRetire: noop,
        bindings: [b({}), b({ binding_id: "2", expires_at: "2026-10-01T00:00:00Z" }), b({ binding_id: "3", subject_kind: "tenant", subject_id: undefined })],
      }),
    );
    expect(out).toContain(">never<");
    expect(out).toContain(">expired<");
    expect(out).toContain("whole tenant");
  });

  it("has an empty state instead of an empty table", () => {
    expect(html(createElement(BindingTable, { bindings: [], onRetire: noop }))).toContain("no bindings yet");
  });
});

describe("groups", () => {
  const m = (group: string, kind: "user" | "group", id: string): GroupMember => ({ group, member_kind: kind, member_id: id, added_at: "" });
  const idx = { support: [m("support", "user", "alice"), m("support", "group", "temps")], temps: [m("temps", "user", "carol")] };

  it("counts direct and effective members per group and marks an external group", () => {
    const out = html(
      createElement(GroupTable, {
        groups: [
          { name: "support", created_at: "" },
          { name: "okta-eng", created_at: "", external_id: "okta:1" },
        ],
        idx,
        open: null,
        onToggle: noop,
        onDelete: noop,
      }),
    );
    expect(out).toContain("1 users, 1 groups");
    expect(out).toContain(">external</span>");
  });

  it("draws the nesting and the effective users with how each is reached", () => {
    expect(html(createElement(MembershipTree, { node: membershipTree("support", idx) }))).toContain("<code>temps</code>");
    const eff = html(createElement(EffectiveMembers, { group: "support", idx }));
    expect(eff).toContain("<code>alice</code><span class=\"settings-muted\"> — direct</span>");
    expect(eff).toContain("<code>carol</code><span class=\"settings-muted\"> — via temps</span>");
  });
});

describe("simulator answers", () => {
  it("shows the decision, what no-match means in the mode, and the rule trail", () => {
    const out = html(
      createElement(SavedAnswer, {
        decision: { decision: false, outcome: "no_match", mode: "compat" },
        explain: {
          decision: { decision: false, outcome: "no_match", mode: "compat" },
          traces: [{ acl: "interns", rule: 0, effect: "deny", matched: false, why: "action not listed" }],
          applied_bindings: [{ acl: "interns", subject_kind: "group", subject_id: "temps" }],
          groups: ["temps"],
          missing_acls: ["ghost"],
        },
      }),
    );
    expect(out).toContain("no rule matched");
    expect(out).toContain("in compat mode the token’s scopes decide");
    expect(out).toContain("action not listed");
    expect(out).toContain("<code>interns</code> → group temps");
    expect(out).toContain("bound but not found");
  });

  it("compares now against the draft and says when the draft changes the answer", () => {
    const out = html(
      createElement(DraftAnswer, {
        missing: [],
        result: {
          request: {},
          now: { decision: false, outcome: "no_match", mode: "compat" },
          with_draft: {
            decision: false,
            outcome: "deny",
            mode: "compat",
            reasons: [
              { acl: "interns", rule: 0, effect: "deny" },
              { acl: "builtin:member", rule: 2, effect: "allow" },
            ],
          },
          changed: true,
        },
      }),
    );
    expect(out).toContain("the draft changes the answer");
    expect(out).toContain("<code>interns</code> rule 0 (deny)");
    // The allow the deny overrode is not presented as deciding it.
    expect(out).not.toContain("builtin:member");
  });
});
