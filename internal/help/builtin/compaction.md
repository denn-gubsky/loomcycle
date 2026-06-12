---
name: compaction
description: Context compaction — summarize older turns to free the model's context window and continue from the summary. Per-agent settings (enabled, target_percentage, keep_last_n, keep_first, autocompact_at_pct, model), three triggers (manual / auto / self via Context op=compact), and spawn inheritance. Read Context op=self (context + compaction) to decide when to self-compact.
---
A long run eventually fills the model's context window. **Compaction**
summarizes the older turns into a short recap and continues from it —
freeing context while preserving the thread. Each compaction replaces the
conversation with:

```
[ <pinned task, verbatim> + <summary of the middle> ]   ← one user turn
[ "Understood — continuing from the summary above." ]   ← assistant ack
<the last N turns, kept verbatim>
```

The kept tail is snapped to a clean turn boundary so a tool_use / tool_result
pair is never split. The system prompt is separate and untouched. The full
original transcript is retained (audit) — only what the model SEES collapses.

## Three ways it happens

1. **Manual** — an operator clicks **Compact** in the `/run` terminal (or
   `POST /v1/runs/{run_id}/compact`), at a safe boundary.
2. **Auto** — when `compaction.enabled` is true and the context footprint
   crosses `autocompact_at_pct`, the loop compacts on its own at the next clean
   boundary. Works for autonomous runs too; off by default.
3. **Self** — YOU can compact your own context: call **`Context op=compact`**.
   It schedules a compaction at your next step (honoring your keep_last_n /
   keep_first / target_percentage). Useful on a long autonomous run before you
   risk overflowing.

## Deciding to self-compact (the conscious path)

Call **`Context op=self`** — it reports both halves of the decision:

- `context`: `{ used_tokens, max_tokens, used_pct }` — how full your window is
  right now (as of the last completed turn). Absent before your first turn /
  when the window is unknown (e.g. Ollama).
- `compaction`: your resolved settings (below).

A typical rule an agent applies:

```
if context.used_pct >= compaction.autocompact_at_pct:
    Context op=compact     # free up room before the next big step
```

(If `compaction.enabled`, auto-compaction will also fire at that threshold —
self-compaction lets you do it earlier, e.g. right before fetching a large
document you know will bloat the next turn.)

## The settings (the per-agent `compaction` block)

| Field | Meaning | Range / default |
|---|---|---|
| `enabled` | turn AUTO-compaction on | default off |
| `target_percentage` | summarize the middle to ~N% of its length | 10–50 (default 10) |
| `keep_last_n` | keep the last N messages verbatim | default 4; 0 = keep none |
| `keep_first` | pin the first user message (the task) verbatim | default true |
| `autocompact_at_pct` | auto-trigger when used/window ≥ N% | 50–95 (default 80) |
| `model` | cheaper same-provider model for the summary call | default: the run's model |

Manual + self compaction work even when `enabled` is false; `enabled` only
gates AUTO. An unknown context window disables auto (manual/self still work).

## Where you set it (same shape as `sampling`)

1. **Static yaml** — on the agent:
   ```yaml
   agents:
     long-runner:
       model: claude-sonnet-4-6
       compaction:
         enabled: true
         autocompact_at_pct: 75
         keep_last_n: 6
         keep_first: true
         target_percentage: 15
         model: claude-haiku-4-5   # cheap summaries
   ```
2. **AgentDef create / fork** — the same `compaction` object in the overlay
   (merged per field; content-identifying — a fork that changes a compaction
   field mints a new version).
3. **Per run** — a `compaction` object on `POST /v1/runs` (wins per field over
   the agent's).

## Spawning: settings flow down the tree

Compaction is context-management, so it **inherits** from parent to child
(unlike memory/sampling, which are each agent's own):

- A child inherits the parent's effective compaction policy.
- The child's own def fills any field the parent left unset.
- The parent can override per-spawn via the **Agent tool's `compaction` field**:
  ```json
  {"op": "spawn", "name": "researcher", "prompt": "...",
   "compaction": {"enabled": true, "keep_last_n": 8}}
  ```

So a breeder/orchestrator can dial its solvers' context management up or down
per spawn without editing their defs.

## Related

- `Context op=self` — your live context footprint + resolved compaction.
- `Context op=compact` — self-compact now.
- `Context help(topic="sampling")` — the per-agent block this mirrors.
