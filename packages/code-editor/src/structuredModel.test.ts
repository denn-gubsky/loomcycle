import { describe, expect, it } from "vitest";
import { toJson, toYaml } from "./structured";
import { applyEdit, applyExternal, initState, switchFormat } from "./structuredModel";

const value = toJson({ name: "t", agents: { a: { system_prompt: "line 1\nline 2" } } });

describe("StructuredEditor state", () => {
  it("opens YAML as the YAML of the host's JSON", () => {
    const s = initState(value, "yaml");
    expect(s.format).toBe("yaml");
    expect(s.text).toBe(toYaml(JSON.parse(value)));
    expect(s.error).toBeNull();
  });

  it("opens JSON when the host's value is not valid JSON, so nothing is hidden", () => {
    const s = initState("{ broken", "yaml");
    expect(s.format).toBe("json");
    expect(s.text).toBe("{ broken");
    expect(s.error).not.toBeNull();
  });

  it("an empty value is not an error and keeps the chosen format (still loading)", () => {
    expect(initState("", "json").error).toBeNull();
    const s = initState("", "yaml");
    expect(s.format).toBe("yaml");
    expect(s.error).toBeNull();
  });

  it("a value arriving after an empty one opens in the chosen format", () => {
    expect(applyExternal(initState("", "yaml"), value).format).toBe("yaml");
  });

  it("returns to the chosen format once an invalid value is replaced by a valid one", () => {
    const shownAsJson = initState("{ broken", "yaml");
    expect(shownAsJson.format).toBe("json");
    expect(applyExternal(shownAsJson, value).format).toBe("yaml");
  });

  it("clearing the YAML text hands the host an empty value", () => {
    const r = applyEdit(initState(value, "yaml"), "  \n");
    expect(r.emit).toBe("");
    expect(r.state.error).toBeNull();
  });

  it("a valid YAML edit hands the host the same value as JSON", () => {
    const s = initState(value, "yaml");
    const edited = s.text.replace("name: t", "name: renamed");
    const r = applyEdit(s, edited);
    expect(r.emit).toBeDefined();
    expect(JSON.parse(r.emit!)).toEqual({ ...JSON.parse(value), name: "renamed" });
    expect(r.state.text).toBe(edited);
  });

  it("an invalid YAML edit keeps the host's value and reports the error", () => {
    const s = initState(value, "yaml");
    const r = applyEdit(s, "name: [unclosed\n");
    expect(r.emit).toBeUndefined();
    expect(r.state.value).toBe(value);
    expect(r.state.error).not.toBeNull();
  });

  it("a JSON edit passes through as typed, invalid or not", () => {
    const s = initState(value, "json");
    const r = applyEdit(s, "{ not json");
    expect(r.emit).toBe("{ not json");
    expect(r.state.error).not.toBeNull();
  });

  it("the host's echo of our own edit keeps the text as typed", () => {
    const s = initState(value, "yaml");
    const typed = s.text.replace("name: t", "name:    spaced") + "\n\n";
    const r = applyEdit(s, typed);
    expect(applyExternal(r.state, r.emit!)).toBe(r.state);
  });

  it("a new value from the host replaces the text", () => {
    const s = initState(value, "yaml");
    const other = toJson({ name: "other" });
    const next = applyExternal(s, other);
    expect(next.text).toBe("name: other\n");
    expect(next.value).toBe(other);
  });

  it("switching format converts the text", () => {
    const s = initState(value, "json");
    const y = switchFormat(s, "yaml");
    expect(y.refused).toBeUndefined();
    expect(y.state.text).toBe(toYaml(JSON.parse(value)));
    const back = switchFormat(y.state, "json");
    expect(back.state.text).toBe(value);
  });

  it("switching back to JSON after YAML edits shows the edited value", () => {
    const s = initState(value, "yaml");
    const r = applyEdit(s, s.text.replace("name: t", "name: v2"));
    const back = switchFormat(r.state, "json");
    expect(JSON.parse(back.state.text).name).toBe("v2");
  });

  it("switching from text that does not parse is refused, keeping the text", () => {
    const s = applyEdit(initState(value, "yaml"), "name: [unclosed\n").state;
    const r = switchFormat(s, "json");
    expect(r.refused).toBeDefined();
    expect(r.state).toBe(s);
  });
});
