import { dump as yamlDump, load as yamlLoad } from "js-yaml";

// The Access console's rule model: the server's resource/action catalog
// (POST /v1/_acldef op=catalog), an ACL rule in its wire shape, and the
// structured editor's draft of one rule.
//
// THE CATALOG DRIVES EVERY PICKER, so an invalid (action, resource) pair cannot
// be authored: the resource-type picker lists the catalog's types, the scope
// picker lists that type's scopes, and the action checkboxes are the verbs
// EVERY chosen resource type has (a rule's actions must be verbs of all its
// types — the server refuses `read tool:*`). The server re-validates on save
// (op=validate, then op=create), so this is the UI's first line, not its last.

export interface CatalogType {
  type: string;
  verbs: string[];
  // Present on a scoped or enum type. A scoped type also has a separator
  // (`:` for document/memory, `/` for def); an enum type has none.
  scopes?: string[];
  separator?: string;
}

export interface AclCatalog {
  catalog_version: number;
  types: CatalogType[];
  transports: string[];
}

// Condition is the closed `when` set (RFC EY §3.5), as the server encodes it.
export interface Condition {
  transport?: string[];
  time?: { days?: string[]; between?: string[]; tz?: string };
  interactive?: boolean;
  tier?: string[];
  user_tier?: string[];
  source_cidr?: string[];
  owner_is_self?: boolean;
  agent?: string;
  depth?: { max: number };
}

export interface Rule {
  effect: "allow" | "deny";
  actions: string[];
  resources: string[];
  when?: Condition;
  description?: string;
}

export const WEEKDAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"] as const;

// ─── selectors ──────────────────────────────────────────────────────────────

export type SelectorShape = "glob" | "scoped" | "enum";

export function selectorShape(t: CatalogType): SelectorShape {
  if (!t.scopes || t.scopes.length === 0) return "glob";
  return t.separator ? "scoped" : "enum";
}

// SelectorDraft is one `type:selector` split into what the pickers edit:
// `scope` for a scoped or enum type, `name` for the glob part.
export interface SelectorDraft {
  type: string;
  scope: string;
  name: string;
}

export function catalogType(cat: AclCatalog, type: string): CatalogType | undefined {
  return cat.types.find((t) => t.type === type);
}

export function emptySelector(t: CatalogType): SelectorDraft {
  switch (selectorShape(t)) {
    case "glob":
      return { type: t.type, scope: "", name: "*" };
    default:
      return { type: t.type, scope: t.scopes![0], name: "" };
  }
}

export function buildSelector(cat: AclCatalog, d: SelectorDraft): string {
  const t = catalogType(cat, d.type);
  if (!t) return `${d.type}:${d.name}`;
  switch (selectorShape(t)) {
    case "glob":
      return `${d.type}:${d.name.trim()}`;
    case "enum":
      return `${d.type}:${d.scope}`;
    case "scoped": {
      const rest = d.name.trim();
      return rest ? `${d.type}:${d.scope}${t.separator}${rest}` : `${d.type}:${d.scope}`;
    }
  }
}

// parseSelector splits a wire selector; null when its type is not in the
// catalog or it has no `type:` prefix.
export function parseSelector(cat: AclCatalog, sel: string): SelectorDraft | null {
  const i = sel.indexOf(":");
  if (i <= 0) return null;
  const type = sel.slice(0, i);
  const rest = sel.slice(i + 1);
  const t = catalogType(cat, type);
  if (!t) return null;
  switch (selectorShape(t)) {
    case "glob":
      return { type, scope: "", name: rest };
    case "enum":
      return { type, scope: rest, name: "" };
    case "scoped": {
      const j = rest.indexOf(t.separator!);
      return j < 0 ? { type, scope: rest, name: "" } : { type, scope: rest.slice(0, j), name: rest.slice(j + 1) };
    }
  }
}

// verbsFor is the actions a rule over these resource types may name: the
// verbs every one of them has, in the first type's order.
export function verbsFor(cat: AclCatalog, types: string[]): string[] {
  if (types.length === 0) return [];
  const specs = types.map((t) => catalogType(cat, t));
  if (specs.some((s) => !s)) return [];
  return specs[0]!.verbs.filter((v) => specs.every((s) => s!.verbs.includes(v)));
}

// ─── rule drafts ────────────────────────────────────────────────────────────

export type TriState = "" | "true" | "false";

export interface ConditionDraft {
  transport: string[];
  days: string[];
  from: string;
  to: string;
  tz: string;
  interactive: TriState;
  tier: string; // comma-separated
  userTier: string;
  sourceCidr: string;
  ownerIsSelf: TriState;
  agent: string;
  depthMax: string;
}

export interface RuleDraft {
  effect: "allow" | "deny";
  actions: string[];
  resources: SelectorDraft[];
  when: ConditionDraft;
  description: string;
}

export function emptyCondition(): ConditionDraft {
  return {
    transport: [],
    days: [],
    from: "",
    to: "",
    tz: "",
    interactive: "",
    tier: "",
    userTier: "",
    sourceCidr: "",
    ownerIsSelf: "",
    agent: "",
    depthMax: "",
  };
}

export function emptyRuleDraft(cat: AclCatalog): RuleDraft {
  const t = catalogType(cat, "agent") ?? cat.types[0];
  return {
    effect: "allow",
    actions: [],
    resources: t ? [emptySelector(t)] : [],
    when: emptyCondition(),
    description: "",
  };
}

const KNOWN_WHEN = new Set(["transport", "time", "interactive", "tier", "user_tier", "source_cidr", "owner_is_self", "agent", "depth"]);

const tri = (b: boolean | undefined): TriState => (b === undefined ? "" : b ? "true" : "false");
const csv = (xs: string[] | undefined) => (xs ?? []).join(", ");
const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);

// draftFromRule opens a stored rule in the structured editor. A rule naming a
// type this catalog lacks, or a condition key it does not know, cannot be shown
// faithfully there; the JSON/YAML view still edits it.
export function draftFromRule(cat: AclCatalog, r: Rule): { ok: true; draft: RuleDraft } | { ok: false; error: string } {
  const resources: SelectorDraft[] = [];
  for (const sel of r.resources ?? []) {
    const d = parseSelector(cat, sel);
    if (!d) return { ok: false, error: `resource ${sel} is not a catalog type; edit it in the JSON/YAML view` };
    resources.push(d);
  }
  const w = r.when ?? {};
  for (const k of Object.keys(w)) {
    if (!KNOWN_WHEN.has(k)) return { ok: false, error: `condition ${k} is not one this console knows; edit it in the JSON/YAML view` };
  }
  return {
    ok: true,
    draft: {
      effect: r.effect,
      actions: [...(r.actions ?? [])],
      resources,
      description: r.description ?? "",
      when: {
        transport: [...(w.transport ?? [])],
        days: [...(w.time?.days ?? [])],
        from: w.time?.between?.[0] ?? "",
        to: w.time?.between?.[1] ?? "",
        tz: w.time?.tz ?? "",
        interactive: tri(w.interactive),
        tier: csv(w.tier),
        userTier: csv(w.user_tier),
        sourceCidr: csv(w.source_cidr),
        ownerIsSelf: tri(w.owner_is_self),
        agent: w.agent ?? "",
        depthMax: w.depth ? String(w.depth.max) : "",
      },
    },
  };
}

function conditionFromDraft(c: ConditionDraft): Condition | undefined {
  const out: Condition = {};
  if (c.transport.length) out.transport = [...c.transport];
  if (c.days.length || c.from || c.to || c.tz) {
    const time: NonNullable<Condition["time"]> = {};
    if (c.days.length) time.days = [...c.days];
    if (c.from || c.to) time.between = [c.from, c.to];
    if (c.tz.trim()) time.tz = c.tz.trim();
    out.time = time;
  }
  if (c.interactive) out.interactive = c.interactive === "true";
  if (splitList(c.tier).length) out.tier = splitList(c.tier);
  if (splitList(c.userTier).length) out.user_tier = splitList(c.userTier);
  if (splitList(c.sourceCidr).length) out.source_cidr = splitList(c.sourceCidr);
  if (c.ownerIsSelf) out.owner_is_self = c.ownerIsSelf === "true";
  if (c.agent.trim()) out.agent = c.agent.trim();
  if (c.depthMax.trim() !== "") out.depth = { max: Number(c.depthMax.trim()) };
  return Object.keys(out).length ? out : undefined;
}

export function ruleFromDraft(cat: AclCatalog, d: RuleDraft): Rule {
  const rule: Rule = {
    effect: d.effect,
    actions: [...d.actions],
    resources: d.resources.map((r) => buildSelector(cat, r)),
  };
  const when = conditionFromDraft(d.when);
  if (when) rule.when = when;
  if (d.description.trim()) rule.description = d.description.trim();
  return rule;
}

// setResourceType changes one resource's type, resetting its selector to that
// type's default and dropping the actions the new set of types lacks.
export function setResourceType(cat: AclCatalog, d: RuleDraft, index: number, type: string): RuleDraft {
  const t = catalogType(cat, type);
  if (!t) return d;
  const resources = d.resources.map((r, i) => (i === index ? emptySelector(t) : r));
  const verbs = verbsFor(
    cat,
    resources.map((r) => r.type),
  );
  return { ...d, resources, actions: d.actions.filter((a) => verbs.includes(a)) };
}

const HM = /^([01]\d|2[0-3]):[0-5]\d$/;

// validateDraft lists what keeps a draft from saving; empty = savable as far
// as the catalog can tell (the server still has the last word).
export function validateDraft(cat: AclCatalog, d: RuleDraft): string[] {
  const errs: string[] = [];
  if (d.actions.length === 0) errs.push("pick at least one action");
  if (d.resources.length === 0) errs.push("add at least one resource");
  d.resources.forEach((r, i) => {
    const t = catalogType(cat, r.type);
    const n = i + 1;
    if (!t) {
      errs.push(`resource ${n}: ${r.type} is not a catalog type`);
      return;
    }
    const shape = selectorShape(t);
    if (shape !== "glob" && r.scope !== "*" && !t.scopes!.includes(r.scope)) errs.push(`resource ${n}: pick a scope`);
    if (shape === "glob" && !r.name.trim()) errs.push(`resource ${n}: ${t.type} needs a name or glob (* for every ${t.type})`);
    if (shape !== "enum" && r.name.split("/").includes("..")) errs.push(`resource ${n}: a selector may not contain ..`);
  });
  const types = [...new Set(d.resources.map((r) => r.type))];
  for (const a of d.actions) {
    for (const ty of types) {
      const t = catalogType(cat, ty);
      if (t && !t.verbs.includes(a)) errs.push(`action ${a} is not a verb of ${ty}`);
    }
  }
  const c = d.when;
  if ((c.from || c.to) && !(HM.test(c.from) && HM.test(c.to))) errs.push("time window: use HH:MM for both ends");
  if (c.depthMax.trim() !== "" && !/^\d+$/.test(c.depthMax.trim())) errs.push("depth: a whole number, 0 or more");
  return errs;
}

// ─── the JSON / YAML view ───────────────────────────────────────────────────

export function rulesToText(rules: Rule[], format: "yaml" | "json"): string {
  return format === "json" ? JSON.stringify(rules, null, 2) : yamlDump(rules);
}

// rulesFromText reads the JSON/YAML view (YAML is a superset of JSON, so one
// loader takes both). It checks the SHAPE only; whether each rule is
// enforceable is the server's op=validate.
export function rulesFromText(text: string): { ok: true; rules: Rule[] } | { ok: false; error: string } {
  if (!text.trim()) return { ok: false, error: "the text is empty" };
  let parsed: unknown;
  try {
    parsed = yamlLoad(text);
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : String(e) };
  }
  if (parsed && typeof parsed === "object" && !Array.isArray(parsed) && Array.isArray((parsed as { rules?: unknown }).rules)) {
    parsed = (parsed as { rules: unknown[] }).rules;
  }
  if (!Array.isArray(parsed)) return { ok: false, error: "expected a list of rules (or an object with a rules list)" };
  for (let i = 0; i < parsed.length; i++) {
    const r = parsed[i] as Partial<Rule> | null;
    if (!r || typeof r !== "object" || typeof r.effect !== "string" || !Array.isArray(r.actions) || !Array.isArray(r.resources)) {
      return { ok: false, error: `rule ${i}: needs effect, actions and resources` };
    }
  }
  return { ok: true, rules: parsed as Rule[] };
}

// ─── the simulator's question ───────────────────────────────────────────────

export interface EvaluationInput {
  subject: string;
  action: string;
  type: string;
  id: string;
  owner: string;
  context: {
    transport?: string;
    interactive?: boolean;
    agent?: string;
    depth?: string;
    tier?: string;
    user_tier?: string;
  };
}

export interface EvaluationRequest {
  subject?: { type: string; id: string };
  action: { name: string };
  resource: { type: string; id: string; owner?: string };
  context?: { transport?: string; interactive?: boolean; agent?: string; depth?: number; tier?: string; user_tier?: string };
}

// evaluationRequest is the AuthZEN body for /v1/_authz/{evaluation,explain}
// and one entry of simulate's `requests`. An empty subject asks about the
// caller; an empty resource id asks about any instance of the type.
export function evaluationRequest(in_: EvaluationInput): EvaluationRequest {
  const out: EvaluationRequest = {
    action: { name: in_.action },
    resource: { type: in_.type, id: in_.id.trim() },
  };
  if (in_.subject.trim()) out.subject = { type: "user", id: in_.subject.trim() };
  if (in_.owner.trim()) out.resource.owner = in_.owner.trim();
  const c = in_.context;
  const ctx: NonNullable<EvaluationRequest["context"]> = {};
  if (c.transport) ctx.transport = c.transport;
  if (c.interactive) ctx.interactive = true;
  if (c.agent?.trim()) ctx.agent = c.agent.trim();
  if (c.depth?.trim() && /^\d+$/.test(c.depth.trim())) ctx.depth = Number(c.depth.trim());
  if (c.tier?.trim()) ctx.tier = c.tier.trim();
  if (c.user_tier?.trim()) ctx.user_tier = c.user_tier.trim();
  if (Object.keys(ctx).length) out.context = ctx;
  return out;
}

// ─── the simulator's draft ──────────────────────────────────────────────────

export interface DraftList {
  name: string;
  description?: string;
  rules: Rule[];
}

export interface DraftBinding {
  acl: string;
  subject_kind: string;
  subject_id?: string;
  expires_at?: string;
}

// draftDocFor writes an unsaved list (and any draft bindings) as the YAML the
// simulator's draft box edits: `{acls: [...], bindings: [...]}` — the shape
// POST /v1/_authz/simulate takes.
export function draftDocFor(list: DraftList | null, bindings: DraftBinding[] = []): string {
  return yamlDump({ acls: list ? [list] : [], bindings });
}

// parseDraftDoc reads the draft box. Blank = no draft. Each list and binding
// is checked for shape only; the server validates them against the catalog.
export function parseDraftDoc(
  text: string,
): { ok: true; acls: DraftList[]; bindings: DraftBinding[] } | { ok: false; error: string } {
  if (!text.trim()) return { ok: true, acls: [], bindings: [] };
  let parsed: unknown;
  try {
    parsed = yamlLoad(text);
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : String(e) };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, error: "the draft is an object with `acls` and/or `bindings` lists" };
  }
  const doc = parsed as { acls?: unknown; bindings?: unknown };
  const acls = doc.acls ?? [];
  const bindings = doc.bindings ?? [];
  if (!Array.isArray(acls)) return { ok: false, error: "acls must be a list" };
  if (!Array.isArray(bindings)) return { ok: false, error: "bindings must be a list" };
  for (let i = 0; i < acls.length; i++) {
    const a = acls[i] as Partial<DraftList> | null;
    if (!a || typeof a.name !== "string" || !Array.isArray(a.rules)) return { ok: false, error: `acls[${i}]: needs a name and a rules list` };
  }
  for (let i = 0; i < bindings.length; i++) {
    const b = bindings[i] as Partial<DraftBinding> | null;
    if (!b || typeof b.acl !== "string" || typeof b.subject_kind !== "string") {
      return { ok: false, error: `bindings[${i}]: needs acl and subject_kind` };
    }
  }
  return { ok: true, acls: acls as DraftList[], bindings: bindings as DraftBinding[] };
}

// ─── the list editor ────────────────────────────────────────────────────────

// A Slot is one rule in the list editor: a structured draft, or — for a rule
// the structured editor cannot show faithfully — the rule as stored, edited
// only in the JSON/YAML view. A raw rule saves back byte-for-byte.
export type Slot = { kind: "draft"; draft: RuleDraft } | { kind: "raw"; rule: Rule; why: string };

export function slotsFromRules(cat: AclCatalog, rules: Rule[]): Slot[] {
  return rules.map((rule) => {
    const res = draftFromRule(cat, rule);
    return res.ok ? { kind: "draft", draft: res.draft } : { kind: "raw", rule, why: res.error };
  });
}

export function rulesFromSlots(cat: AclCatalog, slots: Slot[]): Rule[] {
  return slots.map((s) => (s.kind === "draft" ? ruleFromDraft(cat, s.draft) : s.rule));
}

// The server's name shape (authz.ValidName) and its reserved prefix.
const LIST_NAME = /^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,63}$/;

export function listNameError(name: string): string | null {
  if (!name.trim()) return "a list needs a name";
  if (name.startsWith("builtin:")) return "builtin: names are reserved for the built-in lists";
  if (!LIST_NAME.test(name)) return "names start with a letter or digit and use letters, digits and _ . : / - (64 at most)";
  return null;
}
