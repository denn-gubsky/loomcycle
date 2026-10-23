import { describe, expect, it } from "vitest";
import {
  buildSelector,
  draftDocFor,
  listNameError,
  rulesFromSlots,
  slotsFromRules,
  parseDraftDoc,
  draftFromRule,
  emptyRuleDraft,
  evaluationRequest,
  parseSelector,
  ruleFromDraft,
  rulesFromText,
  rulesToText,
  selectorShape,
  setResourceType,
  validateDraft,
  verbsFor,
  type AclCatalog,
  type Rule,
} from "./aclRules";

// A slice of the server's `op=catalog` answer, in its wire shape: one type of
// each selector shape (glob, scoped with ':' and '/', enum).
const catalog: AclCatalog = {
  catalog_version: 1,
  transports: ["http", "grpc", "mcp", "webui", "a2a", "webhook", "schedule", "bridge"],
  types: [
    { type: "agent", verbs: ["run", "read", "spawn", "steer", "cancel", "review"] },
    { type: "def", verbs: ["author", "retire", "read", "manage"], scopes: ["agent", "skill", "acl"], separator: "/" },
    { type: "document", verbs: ["read", "write", "delete", "link", "browse"], scopes: ["agent", "user", "tenant"], separator: ":" },
    { type: "memory", verbs: ["read", "write", "recall", "browse"], scopes: ["user", "agent", "tenant", "global"], separator: ":" },
    { type: "run", verbs: ["read", "delete", "steer", "cancel", "compact", "retune", "review"], scopes: ["own", "tenant"] },
    { type: "tool", verbs: ["invoke"] },
    { type: "volume", verbs: ["read", "write", "provision", "mount"] },
  ],
};

describe("selectors", () => {
  it("knows each type's selector shape from the catalog entry", () => {
    expect(selectorShape(catalog.types[0])).toBe("glob");
    expect(selectorShape(catalog.types[1])).toBe("scoped");
    expect(selectorShape(catalog.types[4])).toBe("enum");
  });

  it("builds the wire selector for every shape, omitting an empty scoped glob", () => {
    expect(buildSelector(catalog, { type: "agent", scope: "", name: "chat/*" })).toBe("agent:chat/*");
    expect(buildSelector(catalog, { type: "def", scope: "agent", name: "chat/*" })).toBe("def:agent/chat/*");
    expect(buildSelector(catalog, { type: "document", scope: "tenant", name: "/kb/**" })).toBe("document:tenant:/kb/**");
    expect(buildSelector(catalog, { type: "memory", scope: "tenant", name: "" })).toBe("memory:tenant");
    expect(buildSelector(catalog, { type: "run", scope: "own", name: "" })).toBe("run:own");
  });

  it("parses a wire selector back into the same parts", () => {
    for (const sel of ["agent:chat/*", "def:agent/chat/*", "document:tenant:/kb/**", "memory:tenant", "run:own", "tool:*"]) {
      const d = parseSelector(catalog, sel);
      expect(d).not.toBeNull();
      expect(buildSelector(catalog, d!)).toBe(sel);
    }
    expect(parseSelector(catalog, "nosuch:x")).toBeNull();
    expect(parseSelector(catalog, "agent")).toBeNull();
  });

  it("offers only the verbs every chosen type has", () => {
    expect(verbsFor(catalog, ["tool"])).toEqual(["invoke"]);
    expect(verbsFor(catalog, ["agent", "volume"])).toEqual(["read"]);
    expect(verbsFor(catalog, ["agent", "tool"])).toEqual([]);
    expect(verbsFor(catalog, [])).toEqual([]);
  });
});

describe("rule drafts", () => {
  const rfcRules: Rule[] = [
    { effect: "allow", actions: ["run", "read"], resources: ["agent:chat/*", "agent:support/**"] },
    { effect: "allow", actions: ["read", "write"], resources: ["memory:user", "document:user:/**"] },
    { effect: "deny", actions: ["invoke"], resources: ["tool:Bash"], description: "no shell" },
    {
      effect: "allow",
      actions: ["invoke"],
      resources: ["tool:*"],
      when: {
        transport: ["webui", "http"],
        time: { days: ["mon", "fri"], between: ["07:00", "20:00"], tz: "Europe/Kyiv" },
        interactive: false,
        owner_is_self: true,
        tier: ["middle"],
        user_tier: ["pro", "free"],
        source_cidr: ["10.0.0.0/8"],
        agent: "ci/*",
        depth: { max: 0 },
      },
    },
  ];

  it("round-trips every rule shape through the structured editor losslessly", () => {
    for (const r of rfcRules) {
      const res = draftFromRule(catalog, r);
      expect(res.ok).toBe(true);
      if (res.ok) expect(ruleFromDraft(catalog, res.draft)).toEqual(r);
    }
  });

  it("refuses to open a rule naming a type the catalog lacks in the structured editor", () => {
    const res = draftFromRule(catalog, { effect: "allow", actions: ["use"], resources: ["gateway:chat"] });
    expect(res.ok).toBe(false);
  });

  it("drops actions the new type lacks when a resource's type changes", () => {
    let d = emptyRuleDraft(catalog);
    d = { ...d, actions: ["run", "read"] };
    d = setResourceType(catalog, d, 0, "volume");
    expect(d.resources[0]).toEqual({ type: "volume", scope: "", name: "*" });
    expect(d.actions).toEqual(["read"]);
    d = setResourceType(catalog, d, 0, "memory");
    expect(d.resources[0]).toEqual({ type: "memory", scope: "user", name: "" });
  });

  it("reports what makes a draft unsavable, and nothing for a good one", () => {
    const good = { ...emptyRuleDraft(catalog), actions: ["run"] };
    expect(validateDraft(catalog, good)).toEqual([]);

    expect(validateDraft(catalog, emptyRuleDraft(catalog))).toContain("pick at least one action");
    expect(validateDraft(catalog, { ...good, resources: [] })).toContain("add at least one resource");
    expect(validateDraft(catalog, { ...good, resources: [{ type: "agent", scope: "", name: "" }] })).toContain(
      "resource 1: agent needs a name or glob (* for every agent)",
    );
    expect(validateDraft(catalog, { ...good, resources: [{ type: "agent", scope: "", name: "a/../b" }] })).toContain(
      "resource 1: a selector may not contain ..",
    );
    expect(
      validateDraft(catalog, { ...good, resources: [...good.resources, { type: "tool", scope: "", name: "Bash" }] }),
    ).toContain("action run is not a verb of tool");
    const badTime = { ...good, when: { ...good.when, from: "7am", to: "20:00" } };
    expect(validateDraft(catalog, badTime)).toContain("time window: use HH:MM for both ends");
    const halfTime = { ...good, when: { ...good.when, from: "07:00", to: "" } };
    expect(validateDraft(catalog, halfTime)).toContain("time window: use HH:MM for both ends");
    const badDepth = { ...good, when: { ...good.when, depthMax: "-1" } };
    expect(validateDraft(catalog, badDepth)).toContain("depth: a whole number, 0 or more");
  });

  it("omits an empty condition and an empty description from the saved rule", () => {
    const d = { ...emptyRuleDraft(catalog), actions: ["run"] };
    expect(ruleFromDraft(catalog, d)).toEqual({ effect: "allow", actions: ["run"], resources: ["agent:*"] });
  });
});

describe("JSON / YAML view", () => {
  const rules: Rule[] = [
    { effect: "deny", actions: ["invoke"], resources: ["tool:Bash"] },
    { effect: "allow", actions: ["run"], resources: ["agent:chat/*"], when: { transport: ["webui"] } },
    // "07:00" is a number in YAML 1.1 (sexagesimal); it must come back a string.
    { effect: "allow", actions: ["run"], resources: ["agent:*"], when: { time: { between: ["07:00", "20:00"] }, owner_is_self: true } },
  ];

  it("writes and reads back the same rules in YAML and in JSON", () => {
    for (const fmt of ["yaml", "json"] as const) {
      const text = rulesToText(rules, fmt);
      const back = rulesFromText(text);
      expect(back).toEqual({ ok: true, rules });
    }
    expect(rulesToText(rules, "yaml")).toContain("- effect: deny");
  });

  it("accepts a whole list document, taking its rules", () => {
    expect(rulesFromText('{"name":"x","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}')).toEqual({
      ok: true,
      rules: [rules[0]],
    });
  });

  it("explains text that is not a rule list instead of throwing", () => {
    expect(rulesFromText("")).toEqual({ ok: false, error: "the text is empty" });
    expect(rulesFromText("a: [")).toMatchObject({ ok: false });
    expect(rulesFromText("42")).toEqual({ ok: false, error: "expected a list of rules (or an object with a rules list)" });
    expect(rulesFromText("- effect: allow")).toEqual({ ok: false, error: "rule 0: needs effect, actions and resources" });
  });
});

describe("evaluationRequest", () => {
  it("sends only what was filled in, with the resource id defaulting to every instance", () => {
    expect(
      evaluationRequest({ subject: "", action: "run", type: "agent", id: "", owner: "", context: {} }),
    ).toEqual({ action: { name: "run" }, resource: { type: "agent", id: "" } });
  });

  it("names the subject, the owner and the context when given", () => {
    expect(
      evaluationRequest({
        subject: " alice ",
        action: "invoke",
        type: "tool",
        id: "Bash",
        owner: "alice",
        context: { transport: "webui", interactive: true, agent: "ci/build", depth: "2", tier: "", user_tier: "pro" },
      }),
    ).toEqual({
      subject: { type: "user", id: "alice" },
      action: { name: "invoke" },
      resource: { type: "tool", id: "Bash", owner: "alice" },
      context: { transport: "webui", interactive: true, agent: "ci/build", depth: 2, user_tier: "pro" },
    });
  });
});

describe("simulator draft document", () => {
  it("reads draft lists and bindings, and writes a list back as a draft", () => {
    const list = { name: "interns", description: "no shell", rules: [{ effect: "deny" as const, actions: ["invoke"], resources: ["tool:Bash"] }] };
    const text = draftDocFor(list, [{ acl: "interns", subject_kind: "user", subject_id: "alice" }]);
    expect(parseDraftDoc(text)).toEqual({
      ok: true,
      acls: [list],
      bindings: [{ acl: "interns", subject_kind: "user", subject_id: "alice" }],
    });
  });

  it("takes either half alone, and nothing as nothing", () => {
    expect(parseDraftDoc("bindings:\n  - acl: x\n    subject_kind: tenant\n")).toEqual({
      ok: true,
      acls: [],
      bindings: [{ acl: "x", subject_kind: "tenant" }],
    });
    expect(parseDraftDoc("  ")).toEqual({ ok: true, acls: [], bindings: [] });
  });

  it("explains a malformed draft", () => {
    expect(parseDraftDoc("- 1")).toEqual({ ok: false, error: "the draft is an object with `acls` and/or `bindings` lists" });
    expect(parseDraftDoc("acls: [{rules: []}]")).toEqual({ ok: false, error: "acls[0]: needs a name and a rules list" });
    expect(parseDraftDoc("bindings: [{acl: x}]")).toEqual({ ok: false, error: "bindings[0]: needs acl and subject_kind" });
    expect(parseDraftDoc("acls: {}")).toEqual({ ok: false, error: "acls must be a list" });
  });
});

describe("editor slots", () => {
  it("opens representable rules as drafts and keeps the rest raw, giving the same rules back", () => {
    const rules: Rule[] = [
      { effect: "deny", actions: ["invoke"], resources: ["tool:Bash"] },
      { effect: "allow", actions: ["use"], resources: ["gateway:chat"] },
    ];
    const slots = slotsFromRules(catalog, rules);
    expect(slots.map((s) => s.kind)).toEqual(["draft", "raw"]);
    expect(rulesFromSlots(catalog, slots)).toEqual(rules);
  });

  it("checks a list name the way the server does", () => {
    expect(listNameError("support/tier-1")).toBeNull();
    expect(listNameError("")).toBe("a list needs a name");
    expect(listNameError("-x")).toBe("names start with a letter or digit and use letters, digits and _ . : / - (64 at most)");
    expect(listNameError("builtin:member")).toBe("builtin: names are reserved for the built-in lists");
  });
});
