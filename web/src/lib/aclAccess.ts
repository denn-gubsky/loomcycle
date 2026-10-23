import type { AclBinding, GroupMember, TranscriptEvent } from "../api";

// Group closure, binding labelling, the rollout-mode sentence and the /run
// notice's tool list for the Access console, the Users page and the terminal.

// The server encodes an unset time.Time field as Go's zero time (a struct
// `omitempty` is a no-op), so `expires_at` / `retired_at` arrive as
// "0001-01-01T00:00:00Z" when they mean "none".
export function isSetTime(t: string | undefined | null): t is string {
  return !!t && !t.startsWith("0001-01-01");
}

export type BindingState = "active" | "expired" | "retired";

export function bindingState(b: AclBinding, now: number = Date.now()): BindingState {
  if (isSetTime(b.retired_at)) return "retired";
  if (isSetTime(b.expires_at) && Date.parse(b.expires_at) <= now) return "expired";
  return "active";
}

// MembersIndex is group name → its direct members, as GET /v1/_groups/{name}
// returns them.
export type MembersIndex = Record<string, GroupMember[]>;

// Mirrors the engine's nesting cap (authz.MaxGroupNestingDepth). The store
// refuses deeper edges; this only bounds a walk over data the UI was handed.
const MAX_DEPTH = 8;

// effectiveMembers is every user the group holds, directly or through nested
// groups, each once with the shortest chain of member groups it comes through.
export function effectiveMembers(group: string, idx: MembersIndex): { subject: string; via: string[] }[] {
  const found = new Map<string, string[]>();
  const seen = new Set<string>([group]);
  let frontier: { name: string; via: string[] }[] = [{ name: group, via: [] }];
  for (let depth = 0; frontier.length > 0 && depth <= MAX_DEPTH; depth++) {
    const next: typeof frontier = [];
    for (const { name, via } of frontier) {
      for (const mem of idx[name] ?? []) {
        if (mem.member_kind === "user") {
          if (!found.has(mem.member_id)) found.set(mem.member_id, via);
        } else if (!seen.has(mem.member_id)) {
          seen.add(mem.member_id);
          next.push({ name: mem.member_id, via: [...via, mem.member_id] });
        }
      }
    }
    frontier = next;
  }
  return [...found.entries()].map(([subject, via]) => ({ subject, via })).sort((a, b) => a.subject.localeCompare(b.subject));
}

export interface TreeNode {
  name: string;
  users: string[];
  groups: TreeNode[];
  // A group already on the path from the root: drawn once more, not expanded.
  cycle?: boolean;
  // Deeper than the nesting cap: not expanded.
  truncated?: boolean;
}

export function membershipTree(group: string, idx: MembersIndex): TreeNode {
  const walk = (name: string, path: Set<string>, depth: number): TreeNode => {
    if (path.has(name)) return { name, users: [], groups: [], cycle: true };
    if (depth > MAX_DEPTH) return { name, users: [], groups: [], truncated: true };
    const members = idx[name] ?? [];
    const inner = new Set(path).add(name);
    return {
      name,
      users: members
        .filter((m) => m.member_kind === "user")
        .map((m) => m.member_id)
        .sort(),
      groups: members.filter((m) => m.member_kind === "group").map((m) => walk(m.member_id, inner, depth + 1)),
    };
  };
  return walk(group, new Set(), 0);
}

// groupsOfSubject is every group the subject is in: its direct groups (via
// null), then each group containing one of those, naming the member group it
// comes through.
export function groupsOfSubject(subject: string, idx: MembersIndex): { group: string; via: string | null }[] {
  const parents = new Map<string, string[]>(); // member group → groups holding it
  const out: { group: string; via: string | null }[] = [];
  const seen = new Set<string>();
  for (const [group, members] of Object.entries(idx)) {
    for (const m of members) {
      if (m.member_kind === "user" && m.member_id === subject && !seen.has(group)) {
        seen.add(group);
        out.push({ group, via: null });
      }
      if (m.member_kind === "group") parents.set(m.member_id, [...(parents.get(m.member_id) ?? []), group]);
    }
  }
  for (let i = 0; i < out.length && out.length <= 10_000; i++) {
    for (const parent of parents.get(out[i].group) ?? []) {
      if (seen.has(parent)) continue;
      seen.add(parent);
      out.push({ group: parent, via: out[i].group });
    }
  }
  return out;
}

export interface UserBinding {
  binding: AclBinding;
  // Why it applies to the user: bound to the user itself, to a group the user
  // is in (`group` names it), or to the whole tenant.
  source: "user" | "group" | "tenant";
  group?: string;
  state: BindingState;
}

// bindingsForUser is what binds a user: bindings to the subject, to any group
// it is in (transitively) and tenant-wide ones. Retired bindings are left out;
// expired ones stay, marked, so an operator sees what lapsed. Token bindings
// are not included: they follow a credential, not the user.
export function bindingsForUser(
  subject: string,
  groups: { group: string }[],
  bindings: AclBinding[],
  now: number = Date.now(),
): UserBinding[] {
  const inGroup = new Set(groups.map((g) => g.group));
  const rank = { user: 0, group: 1, tenant: 2 } as const;
  const out: UserBinding[] = [];
  for (const b of bindings) {
    const state = bindingState(b, now);
    if (state === "retired") continue;
    if (b.subject_kind === "user" && b.subject_id === subject) out.push({ binding: b, source: "user", state });
    else if (b.subject_kind === "group" && b.subject_id && inGroup.has(b.subject_id))
      out.push({ binding: b, source: "group", group: b.subject_id, state });
    else if (b.subject_kind === "tenant") out.push({ binding: b, source: "tenant", state });
  }
  return out.sort((a, b) => rank[a.source] - rank[b.source]);
}

export const ACL_MODES = ["compat", "shadow", "enforce"] as const;

const MODE_SENTENCES: Record<string, string> = {
  compat:
    "compat — the token scopes decide as before; an explicit deny in a rule list bound to the caller is honoured on top of them.",
  shadow:
    "shadow — the token scopes still decide; the rule lists are evaluated alongside, and every disagreement is logged and audited.",
  enforce:
    "enforce — the rule lists decide: anything no rule allows is refused, and a request whose rules cannot be read is refused rather than admitted.",
};

export function aclModeText(mode: string | undefined): { label: string; sentence: string } {
  if (mode && MODE_SENTENCES[mode]) return { label: mode, sentence: MODE_SENTENCES[mode] };
  return {
    label: mode || "unknown",
    sentence: "The server did not report its access-control mode (open mode has no principal and no rules of its own).",
  };
}

// aclRemovedTools lists the tools the caller's access-control rules withheld
// from a run: the run-start capability_inert events with gate "acl".
export function aclRemovedTools(events: TranscriptEvent[]): string[] {
  const out: string[] = [];
  for (const e of events) {
    const ci = e.event?.capability_inert;
    if (e.type === "capability_inert" && ci?.gate === "acl" && ci.tool && !out.includes(ci.tool)) out.push(ci.tool);
  }
  return out;
}
