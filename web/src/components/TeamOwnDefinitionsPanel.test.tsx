import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import TeamOwnDefinitionsPanel from "./TeamOwnDefinitionsPanel";

const render = (editorText: string) =>
  renderToStaticMarkup(createElement(TeamOwnDefinitionsPanel, { editorText, team: "sdlc", tenant: "" }));

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

  it("says how to declare them when the definition has none", () => {
    expect(render(JSON.stringify({ entry: "x" }))).toContain("(0)");
    expect(render(JSON.stringify({ entry: "x" }))).toContain("None. A team declares its variables under <code>vars</code>");
  });

  it("asks for valid JSON rather than guessing, and renders nothing for an empty editor", () => {
    expect(render("{ not json")).toContain("Fix the JSON above to see them here.");
    expect(render("  ")).toBe("");
  });
});
