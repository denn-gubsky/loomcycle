import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import LiveRunPane from "./LiveRunPane";
import type { TranscriptEvent } from "../api";

const inert = (seq: number, tool: string, gate: string): TranscriptEvent => ({
  seq,
  run_id: "run-1",
  ts_ns: 0,
  type: "capability_inert",
  event: { type: "capability_inert", capability_inert: { tool, gate } },
});

describe("LiveRunPane", () => {
  it("states an ACL-withheld tool once, in the notice, not as a raw event line", () => {
    const html = renderToStaticMarkup(
      createElement(LiveRunPane, {
        events: [inert(0, "Bash", "acl"), inert(1, "AgentDef", "agent_def_scopes")],
        status: "running",
        agentId: "a",
        sessionId: "s",
        error: null,
        onCancel: () => {},
      }),
    );
    expect(html).toContain("Your access rules withheld a tool from this run");
    expect(html).not.toContain("&quot;gate&quot;:&quot;acl&quot;");
    expect(html).toContain("Bash withheld by your access rules");
    // Another gate's line is untouched.
    expect(html).toContain("&quot;gate&quot;:&quot;agent_def_scopes&quot;");
  });
});
