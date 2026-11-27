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

  const withQuestion = (allowOther: boolean) =>
    renderToStaticMarkup(
      createElement(LiveRunPane, {
        events: [],
        status: "running",
        agentId: "a",
        sessionId: "s",
        error: null,
        onCancel: () => {},
        pendingInterrupt: { interruptId: "intr_1", question: "Proceed?", options: ["Yes", "No"], allowOther },
        onAnswerInterrupt: () => {},
      }),
    );

  it("offers an Other answer box beside the option buttons when the question allows one (RFC HG)", () => {
    const html = withQuestion(true);
    expect(html).toContain(">Yes</button>");
    expect(html).toContain('aria-label="Other answer"');
  });

  it("offers only the option buttons when the question does not allow another answer", () => {
    const html = withQuestion(false);
    expect(html).toContain(">Yes</button>");
    expect(html).not.toContain("Other answer");
  });
});
