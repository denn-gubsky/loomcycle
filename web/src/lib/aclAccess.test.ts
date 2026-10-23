import { describe, expect, it } from "vitest";
import {
  ACL_MODES,
  aclModeText,
  aclRemovedTools,
  bindingState,
  bindingsForUser,
  effectiveMembers,
  groupsOfSubject,
  isSetTime,
  membershipTree,
  type MembersIndex,
} from "./aclAccess";
import type { AclBinding, GroupMember, TranscriptEvent } from "../api";

const m = (group: string, kind: "user" | "group", id: string): GroupMember => ({
  group,
  member_kind: kind,
  member_id: id,
  added_at: "2026-10-23T00:00:00Z",
});

// support ⊃ {alice, contractors}; contractors ⊃ {bob, temps}; temps ⊃ {carol}
const idx: MembersIndex = {
  support: [m("support", "user", "alice"), m("support", "group", "contractors")],
  contractors: [m("contractors", "user", "bob"), m("contractors", "group", "temps")],
  temps: [m("temps", "user", "carol"), m("temps", "user", "alice")],
  admins: [m("admins", "user", "dave")],
};

const b = (over: Partial<AclBinding>): AclBinding => ({
  binding_id: "b",
  acl: "x",
  subject_kind: "user",
  created_at: "2026-10-23T00:00:00Z",
  expires_at: "0001-01-01T00:00:00Z",
  retired_at: "0001-01-01T00:00:00Z",
  ...over,
});

describe("time fields", () => {
  it("reads Go's zero time as unset", () => {
    expect(isSetTime("0001-01-01T00:00:00Z")).toBe(false);
    expect(isSetTime(undefined)).toBe(false);
    expect(isSetTime("")).toBe(false);
    expect(isSetTime("2027-01-01T00:00:00Z")).toBe(true);
  });

  it("says a binding is active, expired or retired", () => {
    const now = Date.parse("2026-10-23T12:00:00Z");
    expect(bindingState(b({}), now)).toBe("active");
    expect(bindingState(b({ expires_at: "2026-10-23T11:00:00Z" }), now)).toBe("expired");
    expect(bindingState(b({ expires_at: "2026-10-24T11:00:00Z" }), now)).toBe("active");
    expect(bindingState(b({ retired_at: "2026-10-23T10:00:00Z" }), now)).toBe("retired");
  });
});

describe("group membership", () => {
  it("lists every transitive member once, with the shortest chain of groups it comes through", () => {
    expect(effectiveMembers("support", idx)).toEqual([
      { subject: "alice", via: [] },
      { subject: "bob", via: ["contractors"] },
      { subject: "carol", via: ["contractors", "temps"] },
    ]);
    expect(effectiveMembers("nosuch", idx)).toEqual([]);
  });

  it("draws the nested tree and marks a cycle instead of looping", () => {
    const cyclic: MembersIndex = {
      a: [m("a", "group", "b"), m("a", "user", "u1")],
      b: [m("b", "group", "a")],
    };
    expect(membershipTree("a", cyclic)).toEqual({
      name: "a",
      users: ["u1"],
      groups: [{ name: "b", users: [], groups: [{ name: "a", users: [], groups: [], cycle: true }] }],
    });
    expect(membershipTree("support", idx).groups[0].groups[0]).toEqual({ name: "temps", users: ["alice", "carol"], groups: [] });
  });

  it("finds a subject's groups, direct and through nesting", () => {
    expect(groupsOfSubject("alice", idx)).toEqual([
      { group: "support", via: null },
      { group: "temps", via: null },
      { group: "contractors", via: "temps" },
    ]);
    expect(groupsOfSubject("carol", idx)).toEqual([
      { group: "temps", via: null },
      { group: "contractors", via: "temps" },
      { group: "support", via: "contractors" },
    ]);
    expect(groupsOfSubject("nobody", idx)).toEqual([]);
  });
});

describe("a user's bindings", () => {
  const now = Date.parse("2026-10-23T12:00:00Z");
  const bindings = [
    b({ binding_id: "1", acl: "own", subject_kind: "user", subject_id: "carol" }),
    b({ binding_id: "2", acl: "for-temps", subject_kind: "group", subject_id: "temps" }),
    b({ binding_id: "3", acl: "for-support", subject_kind: "group", subject_id: "support", expires_at: "2026-10-22T00:00:00Z" }),
    b({ binding_id: "4", acl: "everyone", subject_kind: "tenant" }),
    b({ binding_id: "5", acl: "other", subject_kind: "user", subject_id: "dave" }),
    b({ binding_id: "6", acl: "gone", subject_kind: "user", subject_id: "carol", retired_at: "2026-10-01T00:00:00Z" }),
    b({ binding_id: "7", acl: "admins-only", subject_kind: "group", subject_id: "admins" }),
    b({ binding_id: "8", acl: "tok", subject_kind: "token", subject_id: "def-1" }),
  ];

  it("labels its own, its groups' and tenant-wide bindings, and leaves out other subjects' and retired ones", () => {
    const got = bindingsForUser("carol", groupsOfSubject("carol", idx), bindings, now);
    expect(got.map((x) => [x.binding.binding_id, x.source, x.group ?? "", x.state])).toEqual([
      ["1", "user", "", "active"],
      ["2", "group", "temps", "active"],
      ["3", "group", "support", "expired"],
      ["4", "tenant", "", "active"],
    ]);
  });
});

describe("rollout mode", () => {
  it("has one sentence for each mode and falls back for an unknown one", () => {
    for (const mode of ACL_MODES) expect(aclModeText(mode).sentence.length).toBeGreaterThan(20);
    expect(aclModeText("compat").label).toBe("compat");
    expect(aclModeText(undefined).label).toBe("unknown");
  });
});

describe("aclRemovedTools", () => {
  const ev = (seq: number, type: string, event: Record<string, unknown>): TranscriptEvent => ({
    seq,
    run_id: "",
    ts_ns: 0,
    type,
    event: { type, ...event },
  });

  it("lists the tools an ACL withheld, once each, ignoring other inert gates", () => {
    const events = [
      ev(0, "capability_inert", { capability_inert: { tool: "Bash", gate: "acl" } }),
      ev(1, "capability_inert", { capability_inert: { tool: "AgentDef", gate: "agent_def_scopes" } }),
      ev(2, "text", { text: "hi" }),
      ev(3, "capability_inert", { capability_inert: { tool: "mcp__jobs__patch", gate: "acl" } }),
      ev(4, "capability_inert", { capability_inert: { tool: "Bash", gate: "acl" } }),
    ];
    expect(aclRemovedTools(events)).toEqual(["Bash", "mcp__jobs__patch"]);
    expect(aclRemovedTools([])).toEqual([]);
  });
});
