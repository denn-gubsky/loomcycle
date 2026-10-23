import { describe, expect, it } from "vitest";
import type { HookDecisionEventInfo, TranscriptEvent } from "../api";
import { formatLine } from "./TerminalTranscript";

const row = (hd: HookDecisionEventInfo): TranscriptEvent => ({
  seq: 7,
  run_id: "run-1",
  ts_ns: 0,
  type: "hook_decision",
  event: { type: "hook_decision", hook_decision: hd },
});

describe("hook_decision rows", () => {
  it("summarizes a deny with its reason as a refusal", () => {
    const l = formatLine(row({
      hook: "ops/url-gate", phase: "pre", tool_use_id: "toolu_1", tool_name: "WebFetch",
      decision: "deny", reason: "host not on the list",
    }));
    expect(l.payload).toBe("⛨ ops/url-gate pre WebFetch — deny: host not on the list");
    expect(l.cls).toBe("tl-error");
    expect(l.collapsible).toBe(true);
  });

  it("summarizes a rewrite_input and shows the rewritten input pretty-printed when expanded", () => {
    const l = formatLine(row({
      hook: "ops/redact", phase: "pre", tool_use_id: "toolu_2", tool_name: "HTTP",
      decision: "rewrite_input", updated_input: { url: "https://example.test", headers: {} },
    }));
    expect(l.payload).toBe("⛨ ops/redact pre HTTP — rewrote the input");
    expect(l.cls).toBe("tl-tool");
    expect(l.full).toContain("tool: HTTP (toolu_2)");
    expect(l.full).toContain('updated_input:\n{\n  "url": "https://example.test",');
  });

  it("summarizes a hold as held for review in the interrupt style", () => {
    const l = formatLine(row({
      hook: "ops/review", phase: "agent_stop", decision: "hold", reason: "needs a second look",
    }));
    expect(l.payload).toBe("⛨ ops/review agent_stop — held for review: needs a second look");
    expect(l.cls).toBe("tl-interrupt");
  });

  it("summarizes a channel drop by its channel as a refusal", () => {
    const l = formatLine(row({
      hook: "ops/screen", phase: "channel_publish", channel: "inbox", message_id: "m-9",
      decision: "drop", reason: "spam",
    }));
    expect(l.payload).toBe("⛨ ops/screen channel_publish inbox — drop: spam");
    expect(l.cls).toBe("tl-error");
    expect(l.full).toContain("channel: inbox (message m-9)");
  });

  it("marks only a fail-closed outage as a refusal", () => {
    const closed = formatLine(row({
      hook: "ops/down", phase: "pre", tool_name: "HTTP", decision: "unavailable",
      fail_mode: "closed", reason: "the hook timed out",
    }));
    expect(closed.payload).toBe("⛨ ops/down pre HTTP — unavailable (fail closed): the hook timed out");
    expect(closed.cls).toBe("tl-error");
    const open = formatLine(row({
      hook: "ops/down", phase: "pre", tool_name: "HTTP", decision: "unavailable",
      fail_mode: "open", reason: "the hook timed out",
    }));
    expect(open.cls).toBe("tl-tool");
  });
});

describe("team_decision rows", () => {
  it("says which way the decision sent the walk, with the answer when expanded", () => {
    const l = formatLine({
      seq: 3, run_id: "walk-1", ts_ns: 0, type: "team_decision",
      event: {
        type: "team_decision",
        team_decision: {
          state: "triage", visit: 2, edge: "conditional:billing", next: "billing-desk",
          answer: { model: "decide", answers: { route: { type: "choice", choice: "billing" } } },
        },
      },
    });
    expect(l.payload).toBe("◆ triage decided conditional:billing → billing-desk");
    expect(l.collapsible).toBe(true);
    expect(l.full).toContain('"choice": "billing"');
  });
});

describe("admission rows", () => {
  const adm = (type: string, admission: { reason?: string; position?: number; wait_ms?: number }) =>
    formatLine({ seq: 1, run_id: "run-1", ts_ns: 0, type, event: { type, admission } });

  it("says a run over its user's cap waits on that cap, with its place", () => {
    expect(adm("queued", { reason: "user_cap", position: 2 }).payload)
      .toBe("queued — waiting on your run cap, position 2");
  });

  it("says a run with every slot taken waits for a slot, with its place", () => {
    const l = adm("queued", { reason: "global_cap", position: 5 });
    expect(l.payload).toBe("queued — waiting for a run slot, position 5");
    expect(l.cls).toBe("tl-meta");
  });

  it("says how long an admitted run waited", () => {
    expect(adm("admitted", { wait_ms: 4200 }).payload).toBe("admitted after 4.2 s");
  });
});

describe("call admission rows", () => {
  const call = (type: string, admission: { reason?: string; position?: number; wait_ms?: number; provider?: string; kind?: string }) =>
    formatLine({ seq: 1, run_id: "run-1", ts_ns: 0, type, event: { type, admission } });

  it("names the provider a model call waits on, and why", () => {
    expect(call("call_queued", { reason: "lane", position: 3, provider: "ollama-local", kind: "chat" }).payload)
      .toBe("model call queued on ollama-local — waiting for a slot, position 3");
    expect(call("call_queued", { reason: "models_in_flight", position: 1, provider: "ollama-local", kind: "chat" }).payload)
      .toBe("model call queued on ollama-local — waiting for its model to load, position 1");
  });

  it("says how long a model call waited", () => {
    const l = call("call_admitted", { wait_ms: 1500, provider: "ollama-local", kind: "chat" });
    expect(l.payload).toBe("model call on ollama-local started after 1.5 s");
    expect(l.cls).toBe("tl-meta");
  });
});

describe("no-trace rows", () => {
  // A no-trace run stores each event as a marker: its user_input payload is
  // {type, retained:false, bytes}, not the prompt segments. Reading it as an
  // array threw and blanked the runs page.
  it("formats a user_input marker as an empty line instead of throwing", () => {
    const l = formatLine({
      seq: 8,
      run_id: "run-1",
      ts_ns: 0,
      type: "user_input",
      event: { type: "user_input" },
      payload: { type: "user_input", retained: false, bytes: 201 },
    });
    expect(l.kind.trim()).toBe("user_input");
    expect(l.payload).toBe("");
  });
});

describe("capability_inert rows", () => {
  const inert = (tool: string, gate: string, message?: string): TranscriptEvent => ({
    seq: 3,
    run_id: "run-1",
    ts_ns: 0,
    type: "capability_inert",
    event: { type: "capability_inert", capability_inert: { tool, gate, ...(message ? { message } : {}) } },
  });

  // The pane's notice already names the tool and why; the line is a short
  // marker in the stream rather than the raw event a second time.
  it("condenses a tool the caller's access rules withheld", () => {
    const l = formatLine(inert("Bash", "acl", "removed by the caller's access-control rules"));
    expect(l.payload).toBe("Bash withheld by your access rules");
    expect(l.cls).toBe("tl-meta");
    expect(l.payload).not.toContain("{");
  });

  it("keeps the raw line for every other gate", () => {
    const l = formatLine(inert("AgentDef", "agent_def_scopes"));
    expect(l.payload).toContain('"gate":"agent_def_scopes"');
    expect(l.payload).toContain('"tool":"AgentDef"');
    expect(l.cls).toBe("tl-other");
  });
});
