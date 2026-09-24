---
name: Agent
description: "Agent tool — delegate to named sub-agents: spawn one and wait for its answer, fan out to several at once, or open a resident sub-agent you steer over many turns."
---
The `Agent` tool hands a task to **another agent** and gives you its answer.
The sub-agent runs as a child of your run, with its own definition, its own
tools and a fresh conversation that contains only the `prompt` you send.
You see its final reply, not its intermediate steps.

## When to use it — and when not

- **Use Agent** when you need another agent's result before you continue:
  a specialist task, a second opinion, several independent pieces of
  research at once.
- **Use `Channel`** instead when you do not need to wait: dropping work for a
  pool of workers, or handing off to an agent you do not spawn (a scheduled
  or webhook-triggered run).
- **Use `Skill`** instead when you only need instructions for doing the task
  yourself, with your own tools.

For the trade-offs, read `{"op":"help","topic":"subagents"}` and
`{"op":"help","topic":"fan-out-patterns"}`.

## Operations

The `op` argument picks the operation; omit it and you get `spawn`.

| op | What it does | Required besides `op` |
|---|---|---|
| `spawn` | run one sub-agent and return its final text | `name`, `prompt` |
| `parallel_spawn` | run several sub-agents at once, return all results | `spawns` |
| `open` | start a resident sub-agent that keeps its conversation | `name`, `prompt` |
| `send` | give a resident sub-agent its next instruction | `child_run_id`, `prompt` |
| `poll` | check a resident sub-agent without new input | `child_run_id` |
| `cancel` | stop a resident sub-agent's current turn; it stays open | `child_run_id` |
| `close` | shut a resident sub-agent down | `child_run_id` |

`name` must be a **registered agent name** — see `Context {"op":"agents"}`.
Only names; you cannot describe an ad-hoc agent here.

## What a sub-agent gets from you — and what it does not

It **inherits**: your end-user, tenant and user tier; the end-user's
credentials; your web host allowlist (it can reach no host you cannot); the
volumes it declares, but only where you have them too; your compaction
settings (unless you override them with `compaction`); and your
cancellation — if your run is cancelled, so is the child.

It does **not** inherit: your tools (it has its own list, and yours never
widens it), your memory, SQL, channel and skill grants (it has its own), your
agent-scoped memory (it is keyed by its own name), or your conversation. **The
`prompt` is all it knows about the task**, so put everything it needs in it.
Do not put tokens or keys in the prompt; it gets its own credentials.

## spawn

Arguments: `name`, `prompt`, optional `def_id` (run a specific version of that
agent, from `AgentDef`; its name must match `name`) and optional
`compaction` (`enabled`, `target_percentage` 10–50, `keep_last_n`,
`keep_first`, `autocompact_at_pct` 50–95, `model`).

Returns the child's final text as plain text, starting with a line
`[sub-agent agent_id=a_...]`. A child that keeps structured state adds
`Final state:` and its JSON.

## parallel_spawn

Arguments: `spawns`, a list of 1 to 32 entries, each `{name, prompt,
def_id?, compaction?}`. Do not also pass a top-level `name` or `prompt`.
The children run concurrently — by default at most 4 at a time; the rest
wait for a free slot — and the call returns when **all** of them have
finished.

Returns `{"results": [{index, agent, ok, output?, error?, state?}]}` in the
same order as `spawns`. One child failing does not fail the call: check `ok`
on each entry and retry or skip only the ones that failed.

## Resident sub-agents: open, send, poll, cancel, close

A resident sub-agent stays alive between instructions and remembers the
conversation, along with anything it holds, such as a warm sandbox. Use it
for a multi-step job where re-spawning would lose that state.

- `open` — `name`, `prompt`, optional `def_id` and `idle_ttl_seconds`
  (close the child after that long with no `send`; 0 = the operator's
  default). Runs the first turn and returns `{child_run_id, state, output}`.
- `send` — `child_run_id`, `prompt`, optional `timeout_ms`. Waits for the
  turn and returns `{child_run_id, state, output}`. With `timeout_ms` above 0,
  a turn still going after that long returns `state: "running"` and the
  output so far.
- `poll` — `child_run_id`, optional `timeout_ms` (0 = look without waiting).
- `cancel` — `child_run_id`. Stops the current turn; the child stays open.
- `close` — `child_run_id`. Shuts it down and frees what it holds. Closing
  twice is fine. **Close every child you open.**

`state` is `awaiting_input` (ready for the next `send`), `running`,
`completed`, `failed`, `interrupted` or `closed`. For the lifecycle in depth,
read `{"op":"help","topic":"resident-sub-agents"}`.

## Limits

- Sub-agents nest at most 3 levels below the top-level run; deeper calls are
  refused.
- `parallel_spawn` takes at most 32 entries per call. Split larger batches.
- A run may keep only a limited number of resident sub-agents open at once;
  close one to open another.

## Errors

- `missing required field: name` / `prompt` / `child_run_id`, or
  `spawns[N]: missing required field: ...` — add the field. A bad
  `parallel_spawn` entry refuses the whole call before any child starts.
- `unknown sub-agent "X" ...` — no agent is registered under that name for
  your tenant. Check `Context {"op":"agents"}`; retrying is pointless.
- `max sub-agent recursion depth (3) reached ...` — you are too deep to
  delegate. Do the work yourself.
- `Agent tool: def_id "..." is for agent "Y", not "X" ...` or `... is
  retired` — use a `def_id` that belongs to `name` and is live, or omit it.
- `op=spawn must not carry a 'spawns' array` / `op=parallel_spawn must not
  carry top-level name/prompt/def_id fields` — you mixed the two shapes.
- `sub-agent "X" failed (...)` — the child ran and failed. The message says
  why; decide whether to retry with a clearer prompt.
- `resident sub-agent "r_..." not found ...` — it was closed or timed out;
  `open` a new one.
- `resident sub-agent "r_..." is still running its previous turn ...` — `poll`
  it to wait, or `cancel` the turn, before you `send` again.
- `resident sub-agent cap reached ...` — `close` a child first.

## Examples

Delegate one task and wait for the answer:

```json
{"op": "spawn", "name": "cv-adapter", "prompt": "Write a one-page CV for candidate c_4471 targeting a senior backend role. Profile: 8 years Go, Postgres, Kubernetes; led a team of five."}
```

```json result
"[sub-agent agent_id=a_9f2c41d07be35a18]\n# Jane Doe — Senior Backend Engineer\n..."
```

Research three topics at once, then read each entry's `ok`:

```json
{"op": "parallel_spawn", "spawns": [
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Postgres on AWS, GCP and Azure."},
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Redis on AWS, GCP and Azure."},
  {"name": "summarizer", "prompt": "List the five most common reasons teams leave managed databases."}]}
```

```json result
{"results": [
  {"index": 0, "agent": "researcher", "ok": true, "output": "[sub-agent agent_id=a_...]\n..."},
  {"index": 1, "agent": "researcher", "ok": false, "error": "sub-agent \"researcher\" failed (...): ..."},
  {"index": 2, "agent": "summarizer", "ok": true, "output": "[sub-agent agent_id=a_...]\n..."}]}
```

Open a resident sub-agent, steer it, then close it:

```json
{"op": "open", "name": "data-analyst", "prompt": "Load sales_2026.csv from the data volume and describe its columns.", "idle_ttl_seconds": 600}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "awaiting_input", "output": "The file has 12 columns: ..."}
```

```json
{"op": "send", "child_run_id": "r_4e8b1c9a2f7d6035", "prompt": "Now chart monthly revenue by region.", "timeout_ms": 30000}
```

```json
{"op": "close", "child_run_id": "r_4e8b1c9a2f7d6035"}
```
