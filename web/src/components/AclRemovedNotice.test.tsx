import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import AclRemovedNotice from "./AclRemovedNotice";
import type { TranscriptEvent } from "../api";

const inert = (seq: number, tool: string, gate: string): TranscriptEvent => ({
  seq,
  run_id: "",
  ts_ns: 0,
  type: "capability_inert",
  event: { type: "capability_inert", capability_inert: { tool, gate } },
});

describe("AclRemovedNotice", () => {
  it("names the tools an ACL withheld as a note, not an error", () => {
    const html = renderToStaticMarkup(
      createElement(AclRemovedNotice, { events: [inert(0, "Bash", "acl"), inert(1, "HTTP", "acl"), inert(2, "AgentDef", "agent_def_scopes")] }),
    );
    expect(html).toContain('role="note"');
    expect(html).toContain("withheld 2 tools from this run");
    expect(html).toMatch(/<code>Bash<\/code><\/span><span>, <code>HTTP<\/code>/);
    expect(html).not.toContain("AgentDef");
    expect(html).not.toContain("error");
  });

  it("renders nothing when no ACL removed a tool", () => {
    expect(renderToStaticMarkup(createElement(AclRemovedNotice, { events: [inert(0, "AgentDef", "agent_def_scopes")] }))).toBe("");
  });
});
