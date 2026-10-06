---
name: resident-sub-agents
description: when (and how) to drive a persistent, steerable sub-agent with Agent open/send/close instead of re-spawning.
---
# Resident sub-agents — open / send / close

Most sub-agent work is one-shot: `Agent {op:"spawn", …}` runs a child to completion and returns its text. But some **interactive** helpers need to **keep state across many steps** — a REPL or debugger driven step by step, an interactive review, a long multi-turn analysis. Re-spawning throws that state away every call and forces you to re-thread it by hand. The resident ops solve that. (For deterministic batch work like running commands in a sandbox, prefer the `dev/exec` code-js agent with a `keep_open` + `session_id` envelope — it isn't a resident LLM agent.)

## The three ops

- **`open`** — start a PERSISTENT child and run its first turn:
  ```
  Agent {op:"open", name:"chat/medium", prompt:"<first instruction>"}
  → {"child_run_id":"run_…", "state":"awaiting_input", "output":"…"}
  ```
  Capture the `child_run_id` — it's the handle for everything after. The child then **parks**, resident, waiting for your next instruction. Its conversation and anything it holds (open files, a REPL session, accumulated analysis context) stays live. By default `open` waits for the first turn to finish; pass **`timeout_ms`** (as on `send`) to get the `child_run_id` back early with `"state":"running"` and the partial output, then `poll` for the rest.

- **`send`** — give the resident child its next instruction and get that turn's output:
  ```
  Agent {op:"send", child_run_id:"run_…", prompt:"<next instruction>"}
  → {"child_run_id":"run_…", "state":"awaiting_input", "output":"…"}
  ```
  By default it blocks until the child finishes the turn and re-parks. The child sees its full prior conversation — you don't restate context. Pass **`timeout_ms`** to bound the wait: if the turn is still going after that long, `send` returns early with `"state":"running"` and the partial output-so-far, so a long turn doesn't block you indefinitely — then `poll` to await it, or `cancel` to stop it.

- **`poll`** — check on a running child without giving it new input:
  ```
  Agent {op:"poll", child_run_id:"run_…", timeout_ms:30000}
  → {"state":"running"|"awaiting_input", "output":"<output so far>"}
  ```
  `timeout_ms:0` (or omitted) is an instant snapshot; a positive value waits up to that long for the child to park. Use it after an `open` or `send` returned `"running"`.

- **`cancel`** — stop the child's current turn (it stays alive):
  ```
  Agent {op:"cancel", child_run_id:"run_…"}
  → {"state":"awaiting_input", "output":"<partial>"}
  ```
  Turn-cancels the in-flight turn and re-parks the child — for a turn that's stuck or no longer worth finishing. Different from `close` (which terminates the child). A no-op if the child is already parked.

- **`close`** — shut the child down and free its resources:
  ```
  Agent {op:"close", child_run_id:"run_…"}
  ```
  Idempotent. **Always close a child you opened** once you're done with it.

## When to use resident vs spawn

- **Use `spawn` / `parallel_spawn`** when you can describe the whole job up front, or when N independent specialists run at once. The child is stateless and returns once.
- **Use `open` / `send` / `close`** when you'll inspect each result and decide the next step, and the child must stay stateful between steps. An interactive debugging or analysis session is the canonical case: `open` ("load and summarize this dataset"), `send` ("now drill into the outliers"), `send` ("compare against last week") — the child keeps its accumulated context the whole time; no re-spawn, nothing to re-thread. (Batch sandbox work is different — that's `dev/exec` with a `keep_open` + `session_id` envelope, not a resident LLM child.)

## Keeping a helper resident across a long task

A long-lived agent — an interactive session, or an orchestrator driving many steps or states — can keep ONE helper resident for the whole task: `open` it once, `send` it work as each step arrives (across state transitions, tool loops, whatever), and `close` it at the end. The child is parented to your run, so it stays alive as long as you do (and is reaped automatically if you finish without closing it). This is how you give a multi-step workflow a single warm REPL / analysis / review context instead of standing one up per step.

## Rules & limits

- **You own the lifecycle.** The child stays alive until you `close` it, or the run that opened it ends (completed, failed or cancelled), or the runtime reaps it. The runtime reaps a child for one of two reasons:
  - **idle** — no turn is running and nothing has used it (`open`, `send`, `poll`, `cancel`, or the end of its last turn) for the idle period (operator-configured, 30 minutes by default; override per child with `open`'s `idle_ttl_seconds`). A child running a turn is never idle, so a long turn you sent with `timeout_ms` is safe while you wait on it.
  - **turn ceiling** — its current turn has been running longer than the operator's ceiling (`LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS`, 2 hours by default). Polling does not extend it: the clock starts when the turn does. It ends a turn that would never finish on its own — `cancel` a turn you no longer need well before that.

  A reaped child's next `poll` or `send` fails with the reason (`idle timeout` or `turn ceiling`); `open` a new one.
- **Bounded.** A run may hold only so many resident children at once (operator cap, 8 by default); exceeding it fails `open` — close one first. An open resident child also counts toward the run's limit on live children of every kind (32 by default), so open residents leave less room for `spawn` and `parallel_spawn`.
- **`state`** tells you where the child is: `awaiting_input` (parked, ready for the next `send`), `completed`/`failed` (the child ended — a further `send` will fail), `closed`.
- **Longevity caveat:** a resident child is parented to your run — it stays live as long as you do, holds its state in memory between closely-spaced sends, and is reaped if you finish without closing it. Don't rely on it surviving a very long idle pause (e.g. waiting on a human across many minutes).
