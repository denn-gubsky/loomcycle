---
name: resident-sub-agents
description: when (and how) to drive a persistent, steerable sub-agent with Agent open/send/close instead of re-spawning.
---
# Resident sub-agents — open / send / close

Most sub-agent work is one-shot: `Agent {op:"spawn", …}` runs a child to completion and returns its text. But some helpers need to **keep state across many steps** — a warm sandbox container through a compile→test→fix loop, a REPL or debugger, a long multi-turn analysis. Re-spawning throws that state away every call and forces you to re-thread it by hand. The resident ops solve that.

## The three ops

- **`open`** — start a PERSISTENT child and run its first turn:
  ```
  Agent {op:"open", name:"dev/sandbox", prompt:"<first instruction>"}
  → {"child_run_id":"run_…", "state":"awaiting_input", "output":"…"}
  ```
  Capture the `child_run_id` — it's the handle for everything after. The child then **parks**, resident, waiting for your next instruction. Its conversation and anything it holds (a sandbox container, installed deps, a build cache) stay live.

- **`send`** — give the resident child its next instruction and get that turn's output:
  ```
  Agent {op:"send", child_run_id:"run_…", prompt:"<next instruction>"}
  → {"child_run_id":"run_…", "state":"awaiting_input", "output":"…"}
  ```
  It blocks until the child finishes the turn and re-parks. The child sees its full prior conversation — you don't restate context.

- **`close`** — shut the child down and free its resources:
  ```
  Agent {op:"close", child_run_id:"run_…"}
  ```
  Idempotent. **Always close a child you opened** once you're done with it.

## When to use resident vs spawn

- **Use `spawn` / `parallel_spawn`** when you can describe the whole job up front, or when N independent specialists run at once. The child is stateless and returns once.
- **Use `open` / `send` / `close`** when you'll inspect each result and decide the next step, and the child must stay stateful between steps. A compile→test→fix loop against one warm sandbox container is the canonical case: `open` (write + build), `send` ("now run the tests"), `send` ("fix line 42 and re-run") — the container stays hot the whole time; no re-spawn, no session_id to carry.

## Rules & limits

- **You own the lifecycle.** The child stays alive until you `close` it, or it is idle-reaped after a period with no `send` (operator-configured; override per child with `open`'s `idle_ttl_seconds`), or the run that opened it ends.
- **Bounded.** A run may hold only so many resident children at once (operator cap); exceeding it fails `open` — close one first.
- **`state`** tells you where the child is: `awaiting_input` (parked, ready for the next `send`), `completed`/`failed` (the child ended — a further `send` will fail), `closed`.
- **Container caveat:** a resident child keeps its sandbox container warm between closely-spaced sends, but the container has its own idle timeout on the sandbox side — a very long pause (e.g. waiting on a human across many minutes) can still reap it. For a genuinely long-lived workspace, ask the child to use a durable workspace.
