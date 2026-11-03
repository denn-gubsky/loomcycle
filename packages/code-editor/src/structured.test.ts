import { describe, expect, it } from "vitest";
import { locate, parseJson, parseYaml, pathAt, positionAt, toJson, toYaml } from "./structured";

// A team shaped like the ones the Team page edits: own agents whose prompts
// are multi-line, a JSON schema, empty arrays, numbers and unicode.
const team = {
  entry: "intake",
  agents: {
    parser: {
      tier: "high",
      tools: [],
      system_prompt: "You are the parser.\nFind which lines belong to blocks B01..B12.\n\nRules:\n- keep order\n- never invent  \n",
      output_format: { type: "json_schema", schema: { type: "object", required: ["title", "blocks"], properties: { title: { type: "string" } } } },
    },
  },
  states: [
    { name: "intake", handler: { kind: "agent", agent: "parser" }, run_timeout_seconds: 300 },
    { name: "done", handler: { kind: "end" }, weight: -1.5, note: "Résumé — «ok» ✓", flag: true, nothing: null },
  ],
  "dotted.key": { "with space": "yes", on: "off", date: "2026-11-03", num_string: "123" },
};

describe("toYaml / parseYaml", () => {
  it("round-trips a team through YAML to the same value", () => {
    const back = parseYaml(toYaml(team));
    expect(back).toEqual({ ok: true, value: team });
  });

  it("writes a multi-line prompt as a block with real lines", () => {
    const y = toYaml(team);
    expect(y).toContain("system_prompt: |");
    expect(y).toContain("\n      Rules:\n");
    expect(y).not.toContain("\\n");
  });

  it("keeps the value's key order", () => {
    const y = toYaml({ zeta: 1, alpha: 2, mid: 3 });
    expect(y.indexOf("zeta")).toBeLessThan(y.indexOf("alpha"));
    expect(y.indexOf("alpha")).toBeLessThan(y.indexOf("mid"));
  });

  it("does not fold a long line (the editor wraps it)", () => {
    const long = "word ".repeat(80).trim();
    expect(toYaml({ d: long }).trim().split("\n")).toHaveLength(1);
  });

  it("keeps a date, yes/on and a numeric string as strings", () => {
    const r = parseYaml("d: 2026-11-03\ny: yes\no: on\n");
    expect(r).toEqual({ ok: true, value: { d: "2026-11-03", y: "yes", o: "on" } });
  });

  it.each([
    ["an alias", "a: &x [1]\nb: *x\n", /Anchors|Aliases/],
    ["an alias without its anchor", "a: 1\nb: *x\n", /./],
    ["a duplicate key", "a: 1\na: 2\n", /unique/i],
    ["a !!binary tag", "b: !!binary aGk=\n", /Tags \(!!binary\)/],
    ["a custom tag", "b: !foo x\n", /Tags \(!foo\)/],
    ["an explicit !!str tag", "b: !!str 5\n", /Tags/],
    ["infinity", "x: .inf\n", /Infinity/],
    ["a list at the root", "- a\n- b\n", /root must be a mapping/],
    ["a scalar at the root", "just text\n", /root must be a mapping/],
    ["an empty document", "", /root must be a mapping/],
  ])("refuses %s", (_, text, message) => {
    const r = parseYaml(text);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.message).toMatch(message);
  });

  it("places a YAML error on its line", () => {
    const r = parseYaml("a: 1\nb: [1, 2\nc: 3\n");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.line).toBeGreaterThanOrEqual(2);
  });

  it("places a refused tag on its line", () => {
    const r = parseYaml("a: 1\nb: !!binary aGk=\n");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.line).toBe(2);
  });

  it("does not expand an alias bomb", () => {
    const bomb = ["a: &a [x, x]", "b: &b [*a, *a]", "c: &c [*b, *b]", "d: &d [*c, *c]", "e: [*d, *d]"].join("\n");
    const r = parseYaml(bomb);
    expect(r.ok).toBe(false);
  });
});

describe("parseJson", () => {
  it("accepts an object and returns it", () => {
    expect(parseJson(toJson(team))).toEqual({ ok: true, value: team });
  });

  it("refuses a duplicate key, at the second one", () => {
    const text = '{\n  "a": 1,\n  "a": 2\n}';
    const r = parseJson(text);
    expect(r.ok).toBe(false);
    if (!r.ok) {
      expect(r.error.message).toMatch(/Duplicate key/);
      expect(r.error.line).toBe(3);
    }
  });

  it("refuses a non-object root", () => {
    for (const t of ["[1]", "1", '"s"', "null"]) expect(parseJson(t).ok).toBe(false);
  });

  it("places a syntax error on its line, whatever the engine says", () => {
    const r = parseJson('{\n  "a": 1,\n  "b": \n}');
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.line).toBe(4);
  });

  it("does not accept YAML-only syntax as JSON", () => {
    expect(parseJson("a: 1").ok).toBe(false);
    expect(parseJson("{'a': 1}").ok).toBe(false);
  });
});

describe("locate / pathAt", () => {
  const json = toJson(team);
  const yaml = toYaml(team);

  it.each([
    ["json", json],
    ["yaml", yaml],
  ] as const)("locates a nested member by its key (%s)", (format, text) => {
    const at = locate(text, format, ["states", 0, "handler", "agent"]);
    expect(at).toBeDefined();
    expect(text.slice(at!.offset)).toMatch(/^"?agent"?:/);
  });

  it.each([
    ["json", json],
    ["yaml", yaml],
  ] as const)("locates a quoted key with a dot in it (%s)", (format, text) => {
    const at = locate(text, format, ["dotted.key", "with space"]);
    expect(text.slice(at!.offset)).toMatch(/^"?with space"?:/);
  });

  it.each([
    ["json", json],
    ["yaml", yaml],
  ] as const)("falls back to the nearest enclosing place (%s)", (format, text) => {
    const parent = locate(text, format, ["states", 1]);
    const missing = locate(text, format, ["states", 1, "not_written"]);
    expect(missing?.offset).toBe(parent?.offset);
  });

  it("is undefined when the text does not parse", () => {
    expect(locate("{", "json", ["a"])).toBeUndefined();
    expect(locate("a: [", "yaml", ["a"])).toBeUndefined();
  });

  it.each([
    ["json", json],
    ["yaml", yaml],
  ] as const)("names the path under an offset (%s)", (format, text) => {
    const at = locate(text, format, ["states", 1, "handler", "kind"])!;
    expect(pathAt(text, format, at.offset)).toEqual(["states", 1, "handler", "kind"]);
  });

  it("pathAt is the root outside every member", () => {
    expect(pathAt("{\n\n  \"a\": 1\n}", "json", 2)).toEqual([]);
  });
});

describe("positionAt", () => {
  it("counts lines and columns from 1", () => {
    expect(positionAt("ab\ncd", 4)).toEqual({ offset: 4, line: 2, column: 2 });
    expect(positionAt("ab", 99)).toEqual({ offset: 2, line: 1, column: 3 });
  });
});
