import { agentDefRegistry } from "@loomcycle/def-fields";
import { describe, expect, it } from "vitest";
import { FORM_OWNED_AGENT_KEYS, sourceOverlay } from "../components/LibraryEditModal";

// The Form and the List surfaces edit ONE overlay, and switching Form → List
// deletes the form-owned keys before merging the form's values back in (see
// formOverlay). These tests pin the two properties that makes that safe.

describe("FORM_OWNED_AGENT_KEYS", () => {
  it("has no duplicates", () => {
    expect(FORM_OWNED_AGENT_KEYS.length).toBe(new Set(FORM_OWNED_AGENT_KEYS).size);
  });

  // If the form owned a key the list did not render, switching to the list
  // would show an overlay with an invisible value — and switching back would
  // keep it, so the operator could neither see nor clear it.
  it("is a subset of what the list renders", () => {
    const listed = new Set(agentDefRegistry.fields.map((f) => f.key));
    const unlisted = FORM_OWNED_AGENT_KEYS.filter((k) => !listed.has(k));
    expect(unlisted).toEqual([]);
  });

  // The set this replaced was missing these two, which is why the old raw
  // overlay box warned about shadowing fields the form was already writing.
  it("covers the keys the previous covered-set had drifted past", () => {
    expect(FORM_OWNED_AGENT_KEYS).toContain("max_context_tokens");
    expect(FORM_OWNED_AGENT_KEYS).toContain("internal");
  });

  // description is form-owned AND rendered in the shared identity row, so the
  // list omits it. Anything else form-owned must be reachable in the list.
  it("keeps description, the one key the list deliberately omits", () => {
    expect(FORM_OWNED_AGENT_KEYS).toContain("description");
  });
});

describe("sourceOverlay", () => {
  it("carries every operator-settable key the source has", () => {
    const ov = sourceOverlay({
      tier: "middle",
      tools: ["Read"],
      max_context_tokens: 32000,
      context: { recall: true },
    });
    expect(ov).toEqual({
      tier: "middle",
      tools: ["Read"],
      max_context_tokens: 32000,
      context: { recall: true },
    });
  });

  // A static agent's served definition materialises `channels: {}` and
  // `interruption: {}` whether or not the operator wrote them. Keeping them
  // showed "2 set" on a Capabilities group nobody had configured, and a save
  // from the list would have written the empty blocks into the fork.
  it("drops nested blocks that carry no setting", () => {
    const ov = sourceOverlay({ tier: "middle", channels: {}, interruption: {} });
    expect(ov).toEqual({ tier: "middle" });
  });

  // The complement, and the sharper edge: an empty ARRAY is a real grant.
  // `tools: []` means zero tools, which is not the same as inheriting.
  it("keeps an empty array, which is a default-deny grant", () => {
    expect(sourceOverlay({ tools: [], memory_scopes: [] })).toEqual({
      tools: [],
      memory_scopes: [],
    });
  });

  it("keeps explicit zero, false and empty string", () => {
    const ov = sourceOverlay({ max_tokens: 0, internal: false, model: "" });
    expect(Object.prototype.hasOwnProperty.call(ov, "max_tokens")).toBe(true);
    expect(Object.prototype.hasOwnProperty.call(ov, "internal")).toBe(true);
    expect(Object.prototype.hasOwnProperty.call(ov, "model")).toBe(true);
  });

  it("drops the server-set and derived keys", () => {
    const ov = sourceOverlay({
      name: "a", def_id: "d", version: 2, content_sha256: "x",
      system_prompt_base: "b", retired: false, tier: "middle",
    });
    expect(ov).toEqual({ tier: "middle" });
  });

  it("is empty for a missing or non-object definition", () => {
    expect(sourceOverlay(undefined)).toEqual({});
    expect(sourceOverlay("nope")).toEqual({});
  });
});
