import { useEffect, useMemo, useRef } from "react";
import type {
  EventPayload,
  SystemPromptPayload,
  TranscriptEvent,
  UserInputPayload,
} from "../api";

// TerminalTranscript renders the run's events as a chronological
// flat stream of "[hh:mm:ss.SSS] event_type | payload" lines,
// styled like a terminal log tail. Differentiator from the panels
// view: shows EVERY event (no visibility filter) and aligns the
// stream vertically so the operator can scan timing relations
// across tool calls.
//
// Coalescing: adjacent text + thinking events from the same turn
// merge into one wrapped block (user-confirmed). Without this the
// per-token deltas from streaming providers fill the screen with
// near-meaningless single-word lines.
//
// Auto-tail: scroll-to-bottom on new events ONLY when the operator
// is already near the bottom — don't yank them away from a line
// they're actively reading.

const TS_PAD = "             "; // 13 spaces; width of "[hh:mm:ss.SSS]"
const KIND_PAD_WIDTH = 11;       // longest kind label: "tool_result"

export interface TerminalTranscriptProps {
  events: TranscriptEvent[];
}

export default function TerminalTranscript({ events }: TerminalTranscriptProps) {
  const ref = useRef<HTMLDivElement | null>(null);
  const lines = useMemo(() => coalesceTextTerminal(events).map(formatLine), [events]);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }, [lines.length]);

  return (
    <div ref={ref} className="terminal-transcript">
      {lines.map((l, i) => (
        <div key={l.key ?? i} className={`tl-row ${l.cls}`}>
          {/* Preserve newlines within a single coalesced text block;
              wrap long lines via CSS white-space: pre-wrap. */}
          <span className="tl-ts">{l.ts}</span>
          <span className="tl-kind">{l.kind}</span>
          <span className="tl-sep">|</span>
          <span className="tl-payload">{l.payload}</span>
        </div>
      ))}
    </div>
  );
}

interface FormattedLine {
  key: number | string;
  ts: string;     // "[hh:mm:ss.SSS]" or blank
  kind: string;   // padded to KIND_PAD_WIDTH
  cls: string;
  payload: string;
}

// coalesceTextTerminal merges adjacent text + thinking events into
// one event whose `text` is the concatenated stream. Mirrors
// AgentDetailPane.tsx's coalesceText so the terminal view stays
// consistent with the panels view's text grouping. Other event
// types (tool_call, tool_result, usage, etc.) are NOT merged.
function coalesceTextTerminal(events: TranscriptEvent[]): TranscriptEvent[] {
  const out: TranscriptEvent[] = [];
  for (const ev of events) {
    const last = out[out.length - 1];
    const kind = ev.event?.type ?? ev.type;
    const lastKind = last ? (last.event?.type ?? last.type) : "";
    if (last && (kind === "text" || kind === "thinking") && kind === lastKind) {
      out[out.length - 1] = {
        ...last,
        event: { ...last.event, text: (last.event?.text ?? "") + (ev.event?.text ?? "") },
      };
    } else {
      out.push(ev);
    }
  }
  return out;
}

function formatLine(row: TranscriptEvent): FormattedLine {
  const ts = row.ts_ns > 0 ? `[${formatHMSms(row.ts_ns)}]` : TS_PAD;
  const ev = row.event ?? ({ type: row.type } as EventPayload);
  const kindRaw = ev.type ?? row.type ?? "?";
  const kind = kindRaw.padEnd(KIND_PAD_WIDTH).slice(0, KIND_PAD_WIDTH);
  const key = row.seq;
  switch (kindRaw) {
    case "text":
    case "thinking":
      return { key, ts, kind, cls: "tl-text", payload: ev.text ?? "" };
    case "tool_call": {
      const t = ev.tool_use;
      const inputStr = t?.input
        ? typeof t.input === "string"
          ? oneLine(t.input)
          : oneLine(JSON.stringify(t.input))
        : "";
      const idTail = t?.id ? ` ${t.id.slice(0, 8)}` : "";
      return {
        key, ts, kind, cls: "tl-tool",
        payload: `${t?.name ?? "?"}${idTail} ${inputStr}`.trimEnd(),
      };
    }
    case "tool_result": {
      const id = (ev as { tool_use_id?: string }).tool_use_id ?? "";
      const idTail = id ? `${id.slice(0, 8)} ` : "";
      return {
        key, ts, kind,
        cls: ev.is_error ? "tl-error" : "tl-result",
        payload: `${idTail}${oneLine(ev.text ?? "")}`,
      };
    }
    case "usage":
      return {
        key, ts, kind, cls: "tl-usage",
        payload: `in=${ev.usage?.input_tokens ?? 0} out=${ev.usage?.output_tokens ?? 0}${ev.usage?.model ? ` model=${ev.usage.model}` : ""}`,
      };
    case "done":
      return {
        key, ts, kind, cls: "tl-done",
        payload: `stop_reason=${ev.stop_reason ?? "?"}${ev.usage?.model ? ` model=${ev.usage.model}` : ""}`,
      };
    case "retry": {
      const r = ev.retry ?? {};
      return {
        key, ts, kind, cls: "tl-retry",
        payload: `${r.provider ?? "?"} #${r.attempt ?? "?"} wait=${r.wait_ms ?? 0}ms ${r.reason ?? ""}`.trimEnd(),
      };
    }
    case "error":
      return { key, ts, kind, cls: "tl-error", payload: oneLine(ev.error ?? "") };
    case "interruption_pending": {
      const i = ev.interruption;
      const opts = i?.options && i.options.length > 0 ? ` [${i.options.join(" | ")}]` : "";
      return {
        key, ts, kind, cls: "tl-interrupt",
        payload: `❓ ${oneLine(i?.question ?? "")}${opts}`.trimEnd(),
      };
    }
    case "steer":
      // operator-injected mid-run instruction; "»" marks it as operator input.
      return { key, ts, kind, cls: "tl-steer", payload: `» ${oneLine(ev.user_input?.text ?? "")}` };
    case "awaiting_input":
      return { key, ts, kind, cls: "tl-meta", payload: "idle — waiting for operator input" };
    case "started":
      return { key, ts, kind, cls: "tl-meta", payload: "" };
    case "session":
    case "agent":
      return { key, ts, kind, cls: "tl-meta", payload: oneLine(JSON.stringify(ev)) };
    case "user_input": {
      // v0.9.x: user_input's payload lives on the sidecar (the raw
      // segments JSON). Show the first user-role segment's text as a
      // one-liner; system-role segments are skipped because the
      // dedicated system_prompt event already surfaces them.
      const segs = (row.payload as UserInputPayload[] | undefined) ?? [];
      const userSeg = segs.find((s) => s.role === "user");
      const text = userSeg?.content?.[0]?.text ?? "";
      return { key, ts, kind, cls: "tl-text", payload: oneLine(text) };
    }
    case "system_prompt": {
      // v0.9.x: same sidecar pattern as user_input. Surface the
      // first line of the resolved prompt + an agent_def_id chip
      // when present so operators eyeballing the terminal stream
      // can correlate two runs' prompt versions at a glance.
      const p = row.payload as SystemPromptPayload | undefined;
      const head = p?.system_prompt ?? "";
      const tail = p?.agent_def_id ? ` [${p.agent_def_id}]` : "";
      return { key, ts, kind, cls: "tl-text", payload: oneLine(head) + tail };
    }
    default:
      return { key, ts, kind, cls: "tl-other", payload: oneLine(JSON.stringify(ev)) };
  }
}

function oneLine(s: string): string {
  return s.replace(/\s+/g, " ").trim();
}

function formatHMSms(ns: number): string {
  const ms = Math.floor(ns / 1_000_000);
  const d = new Date(ms);
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}.${pad3(d.getMilliseconds())}`;
}

function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

function pad3(n: number): string {
  return String(n).padStart(3, "0");
}
