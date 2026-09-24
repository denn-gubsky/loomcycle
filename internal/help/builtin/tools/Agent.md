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

The `op` argument picks the operation; omit it and you get `spawn`. Fetch one
operation's article, with examples, as `Agent/<op>` — for example
`{"op":"help","topic":"Agent/parallel_spawn"}`.

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

## Resident sub-agents

`open`, `send`, `poll`, `cancel` and `close` drive a sub-agent that stays alive
between instructions and remembers the conversation, along with anything it
holds, such as a warm sandbox. Use one for a multi-step job where re-spawning
would lose that state. A resident child's `state` is `awaiting_input` (ready
for the next `send`), `running`, `completed`, `failed`, `interrupted` or
`closed`. **Close every child you open.** For the lifecycle in depth, read
`{"op":"help","topic":"resident-sub-agents"}`.

## Limits

- Sub-agents nest at most 3 levels below the top-level run; deeper calls are
  refused with `max sub-agent recursion depth (3) reached` — do the work
  yourself.
- `parallel_spawn` takes at most 32 entries per call. Split larger batches.
- A run may keep only a limited number of resident sub-agents open at once;
  close one to open another.
