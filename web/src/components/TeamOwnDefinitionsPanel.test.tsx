import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import TeamOwnDefinitionsPanel, { LocalAgentEditor } from "./TeamOwnDefinitionsPanel";

const render = (editorText: string) =>
  renderToStaticMarkup(createElement(TeamOwnDefinitionsPanel, { editorText, setEditorText: () => {}, team: "sdlc", tenant: "", disabled: false }));

describe("TeamOwnDefinitionsPanel", () => {
  it("lists the variables and each local kind that has entries", () => {
    const html = render(
      JSON.stringify({
        vars: { tone: "formal" },
        local: { agents: { reviewer: { tier: "middle", tools: ["Read"] } }, webhooks: { gh: { channel: "./events", auth: { kind: "none" } } } },
      }),
    );
    expect(html).toContain("This team&#x27;s own definitions (3)");
    expect(html).toContain("Variables (1)");
    expect(html).toContain("<code>tone</code> — default: formal");
    expect(html).toContain("Agents (1)");
    expect(html).toContain("<code>./reviewer</code>");
    expect(html).toContain("tier middle · tools: Read");
    expect(html).toContain("POST /v1/_teams/sdlc/webhooks/gh");
    expect(html).not.toContain("Skills (");
    expect(html).toContain("<code>sdlc/name</code>");
  });

  it("offers an editor for each of the team's own agents only, closed until asked for", () => {
    const html = render(
      JSON.stringify({
        local: {
          agents: { reviewer: { tier: "middle" }, writer: { tier: "small" } },
          channels: { events: { scope: "tenant" } },
        },
      }),
    );
    expect(html.match(/>edit<\/button>/g)).toHaveLength(2);
    expect(html).not.toContain("loomcycle-def-fields");
  });

  it("says how to declare them when the definition has none", () => {
    expect(render(JSON.stringify({ entry: "x" }))).toContain("(0)");
    expect(render(JSON.stringify({ entry: "x" }))).toContain("None. A team declares its variables under <code>vars</code>");
  });

  it("asks for valid JSON rather than guessing, and renders nothing for an empty editor", () => {
    expect(render("{ not json")).toContain("Fix the JSON above to see them here.");
    expect(render("  ")).toBe("");
  });
});

describe("LocalAgentEditor", () => {
  const def = { local: { agents: { reviewer: { tier: "middle", system_prompt: "You review diffs." } } } };
  const html = (name: string) =>
    renderToStaticMarkup(createElement(LocalAgentEditor, { def, name, disabled: false, onChange: () => {} }));

  it("shows the agent's body in the agent field list, its set groups open", () => {
    const out = html("reviewer");
    expect(out).toContain("loomcycle-def-fields");
    expect(out).toContain("You review diffs.");
    expect(out).toContain('value="middle"');
  });

  it("renders nothing for an agent the definition no longer declares", () => {
    expect(html("ghost")).toBe("");
  });
});
