// Helpers for a configured run's draft — the request the run will start with,
// in the server's snake_case wire keys (GET /v1/agents/{id} `draft`).

type Block = { type?: unknown; text?: unknown };
type Segment = { role?: unknown; content?: unknown };

// draftPromptText returns the draft's prompt when it is the simple shape the
// run form creates — one user segment holding one trusted-text block — and
// null for anything else (several segments, images, untrusted blocks). The
// editor offers a text box only for the simple shape; rewriting a richer one
// through a single box would silently flatten it.
export function draftPromptText(draft: Record<string, unknown> | undefined | null): string | null {
  const segs = draft?.segments;
  if (!Array.isArray(segs) || segs.length !== 1) return null;
  const seg = segs[0] as Segment;
  if (seg?.role !== "user" || !Array.isArray(seg.content) || seg.content.length !== 1) return null;
  const block = seg.content[0] as Block;
  if (block?.type !== "trusted-text" || typeof block.text !== "string") return null;
  return block.text;
}

// draftSettings is the draft minus its identity-free boilerplate (the agent
// and the prompt), for a read-only view of the overrides it will run with.
export function draftSettings(draft: Record<string, unknown> | undefined | null): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(draft ?? {})) {
    if (k === "agent" || k === "segments") continue;
    out[k] = v;
  }
  return out;
}
