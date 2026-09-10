import { describe, expect, it } from "vitest";
import type { FieldSpec } from "../types";
import {
  clearField, countSet, isSet, matchesQuery, setField, visibleFields,
} from "./value";

const f = (key: string, over: Partial<FieldSpec> = {}): FieldSpec => ({
  key, label: key, group: "G", type: "text", hint: `hint for ${key}`, ...over,
});

describe("isSet", () => {
  // The whole sparse-overlay contract in one test: a key present with a FALSY
  // value is a real setting, not an absent one. `value[key] ?? …` would promote
  // an explicit `temperature: 0` to "inherit" and silently un-determinise a
  // pinned agent.
  it("treats an explicit zero, false and empty string as SET", () => {
    expect(isSet({ max_tokens: 0 }, "max_tokens")).toBe(true);
    expect(isSet({ internal: false }, "internal")).toBe(true);
    expect(isSet({ model: "" }, "model")).toBe(true);
  });

  it("treats an absent key and an explicit undefined as UNSET", () => {
    expect(isSet({}, "model")).toBe(false);
    expect(isSet({ model: undefined }, "model")).toBe(false);
  });
});

describe("setField / clearField", () => {
  it("does not mutate the input overlay", () => {
    const before = { model: "a" };
    const after = setField(before, "tier", "middle");
    expect(before).toEqual({ model: "a" });
    expect(after).toEqual({ model: "a", tier: "middle" });
  });

  // Writing null would PERSIST a null into the def; only deleting the key lets
  // the parent / operator value flow through the merge again.
  it("clear DELETES the key rather than nulling it", () => {
    const after = clearField({ model: "a", tier: "middle" }, "tier");
    expect(Object.prototype.hasOwnProperty.call(after, "tier")).toBe(false);
    expect(after).toEqual({ model: "a" });
  });

  it("clearing an already-unset key returns the same object", () => {
    const before = { model: "a" };
    expect(clearField(before, "tier")).toBe(before);
  });

  it("can set a falsy value and read it back as set", () => {
    const after = setField({}, "unbounded_iterations", false);
    expect(isSet(after, "unbounded_iterations")).toBe(true);
  });
});

describe("countSet", () => {
  it("counts only keys present in the overlay", () => {
    const fields = [f("a"), f("b"), f("c")];
    expect(countSet({ a: 1, c: 0 }, fields)).toBe(2);
    expect(countSet({}, fields)).toBe(0);
  });

  it("ignores overlay keys the group does not own", () => {
    expect(countSet({ z: 1 }, [f("a")])).toBe(0);
  });
});

describe("matchesQuery", () => {
  const spec = f("max_context_tokens", { label: "Context window", hint: "The INPUT window this agent uses." });

  it("matches on key, label and hint, case-insensitively", () => {
    expect(matchesQuery(spec, "context_tok")).toBe(true);
    expect(matchesQuery(spec, "Window")).toBe(true);
    expect(matchesQuery(spec, "input window")).toBe(true);
  });

  it("an empty or whitespace query matches everything", () => {
    expect(matchesQuery(spec, "")).toBe(true);
    expect(matchesQuery(spec, "   ")).toBe(true);
  });

  it("does not match unrelated text", () => {
    expect(matchesQuery(spec, "temperature")).toBe(false);
  });
});

describe("visibleFields", () => {
  const fields = [f("model"), f("tier"), f("effort")];

  it("applies search and modified-only together", () => {
    expect(visibleFields(fields, { tier: "middle" }, "", true).map((x) => x.key)).toEqual(["tier"]);
    expect(visibleFields(fields, { tier: "middle" }, "e", false).map((x) => x.key)).toEqual(["model", "tier", "effort"]);
    expect(visibleFields(fields, { tier: "middle" }, "ti", true).map((x) => x.key)).toEqual(["tier"]);
    expect(visibleFields(fields, { model: "x" }, "ti", true)).toEqual([]);
  });

  it("modified-only keeps a field whose value is falsy-but-set", () => {
    expect(visibleFields(fields, { effort: "" }, "", true).map((x) => x.key)).toEqual(["effort"]);
  });

  it("preserves the given field order", () => {
    expect(visibleFields(fields, {}, "", false).map((x) => x.key)).toEqual(["model", "tier", "effort"]);
  });
});
