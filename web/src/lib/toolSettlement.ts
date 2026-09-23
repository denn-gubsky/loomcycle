import type { EventPayload } from "../api";

// toolResultId returns the id of the tool call a tool_result frame answers.
//
// ⚠️ THE SERVER CARRIES IT AS `tool_use.id`, not as a top-level `tool_use_id`.
// providers.Event has no top-level id field — `tool_use_id` exists only on a
// provider ContentBlock, which never reaches this wire. Every settlement check
// here used to read the top-level field, found nothing, and so treated every
// tool call as still in flight: an Interruption that had already failed showed
// "interrupted" for the rest of the run, and the busy hint named the last tool
// as running forever. The top-level read is kept only as a fallback.
export function toolResultId(ev: EventPayload | undefined): string {
  if (!ev || ev.type !== "tool_result") return "";
  return ev.tool_use?.id || ev.tool_use_id || "";
}

// settledToolIds is the set of tool-call ids that already have a result.
export function settledToolIds(
  events: ReadonlyArray<{ event?: EventPayload; type?: string }>,
): Set<string> {
  const out = new Set<string>();
  for (const row of events) {
    const id = toolResultId(row.event);
    if (id) out.add(id);
  }
  return out;
}
