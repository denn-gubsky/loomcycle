---
name: fan-out-patterns
description: when (and when not) to use Agent.parallel_spawn vs sequential Agent.spawn vs Channel.publish for multi-agent workflows.
---
# Fan-out patterns — when (and when not) to use parallel_spawn

The `Agent` tool ships two ops for spawning sub-agents:

- `op: "spawn"` (default — also fires when `op` is omitted): synchronous, single child. The tool returns when that one child completes; you read its text and decide your next step.
- `op: "parallel_spawn"`: N children in flight at once. The tool returns when **every** child has completed (success or error). You get back a JSON envelope with one row per child.

There's also a third option that often beats both: **don't fan out at all**. Publish to a `Channel` and let downstream agents consume independently — the work doesn't have to rejoin your conversation.

This topic is decision-support, not a feature reference. See `Context.help(topic="loomcycle")` for the tool's actual schema.

## When `parallel_spawn` is the right answer

You want `parallel_spawn` when **all three** are true:

1. **The children are independent.** Child B's prompt doesn't depend on child A's output.
2. **You need every child's output before you can move on.** You're going to consolidate / compare / score / aggregate.
3. **The children are slow enough that parallelism actually pays.** LLM calls take seconds; if your "child" is a one-line lookup, the goroutine overhead + cap throttling makes parallel slower than sequential.

Canonical fits:
- 5 researchers cover 5 sub-topics; the parent merges them into a briefing.
- 10 scoring runs against 10 candidate JDs; the parent picks the top 3.
- 3 critic agents review the same draft; the parent reconciles disagreements.

## When `parallel_spawn` is the WRONG answer

### The children are sequential

If child B's prompt embeds child A's output, you have a pipeline, not a fan-out. Use sequential `spawn` calls. Trying to parallelize sequential work is the most common mistake: the model writes `parallel_spawn` because "concurrent is faster," then realizes child B's prompt needs child A's result, then writes a follow-up call that runs B sequentially anyway — paying both costs.

### You don't actually need the output back

If the children are independent AND independently consumable downstream, `Channel.publish` is structurally better than `parallel_spawn`:

- The parent doesn't block waiting for children.
- The children don't have to be sub-agents — they can be top-level agents subscribed to the channel.
- Failure of one child doesn't entangle with another's success.
- Backpressure is the channel's problem, not yours.

Use `parallel_spawn` when you need the join-back. Use `Channel.publish` when you don't.

### One child is enough

Single-child fan-out is a code smell: `parallel_spawn` with a 1-item array always costs more than `spawn` (extra envelope marshaling, an unnecessary goroutine, the JSON your model has to parse on return). If you might-want-N-but-N-is-currently-1, write `spawn` today and migrate when N actually grows.

### The work is cheap

A non-LLM sub-agent (e.g., one that just runs `Memory.get` and returns) finishes in microseconds. Parallelizing 8 such children is slower than serializing them — you pay goroutine setup, cap throttling, semaphore acquire, and envelope marshal for negligible per-child latency to amortize over.

Rule of thumb: if each child takes <100ms, sequential `spawn` is faster.

## Join semantics

`parallel_spawn` blocks the parent agent's tool call until **every** child returns — success or error. There's no partial-results streaming and no early-cancel-on-first-error. (To take results as they come, or to cancel one child, use `mode: "poll"` — see below.)

If a child fails, the error is captured in the envelope's `ok:false` row alongside `error: "<text>"`. The parent's model reads the envelope and decides what to do: retry the failed children, fall back, give up gracefully. The parent's run is **never torn down** because a child failed — exactly the same posture as a `spawn` op whose child errored.

If the parent is cancelled mid-call (an operator or caller cancelled the run, or its parent was cancelled), in-flight children inherit the cancellation; pending children that haven't been admitted to the goroutine pool yet are not started. The envelope still returns, with cancelled-or-not-started children marked `ok:false` + `error: "context canceled"`.

## Wait mode, poll mode, resident children, or channels?

Four ways to have other agents work for you, by what you need back and when:

- **Wait mode** (`spawn` / `parallel_spawn`, the default): you have nothing to
  do until the children are done. One call, their results in its answer.
- **Poll mode** (`mode: "poll"`): you have other work to do meanwhile — more
  tool calls, another batch, reading what the first children found while the
  rest still run. The call returns child_run_ids at once; `poll` reads results
  (`wait: "any"` to take them as they come, `"all"` to join), and a note on
  your next turn says when one finishes. You cannot forget them: ending your
  turn with children still running makes your run wait for them and gives you
  another turn. Use `on_parent_end: "cancel"` for speculative children you
  will not wait for.
- **Resident children** (`open` / `send`): the child must keep state between
  steps — a conversation, a warm sandbox, a REPL — and you steer it turn by
  turn. Not for one-shot tasks; a resident child is a conversation you own and
  must `close`.
- **Channels** (`publish` / `await`): the producers are not your children —
  scheduled runs, webhooks, other agents — or the results do not need to come
  back to you at all.

Poll mode is not cheaper than wait mode: the children cost the same. What it
buys is your own time while they run.

Poll-mode children also survive your run being paused and resumed, on this
instance or another: your run records what it started, read and was told of,
and comes back with the same children, results and notes — nothing handed
over or reported twice. See `help(topic="subagents")`.

**Teams fit here too.** When the work is a whole workflow — a team's walk —
rather than one agent, `TeamDef op=run` with `mode: "poll"` makes the walk a
background child of your run, exactly like a poll-mode sub-agent: you are told
when it ends, your run waits for it at the end of your turn, and you read its
answer with `TeamDef op=poll` (or `Agent op=poll`, which reads every background
child). Start several walks and sub-agents and collect them in any order. See
`help(topic="agent-teams")`.

## Concurrency cap

The runtime caps the number of children running concurrently per `parallel_spawn` call. Two layers:

1. **Per-agent override** — `max_concurrent_children: N` in the agent's `loomcycle.yaml` (or via AgentDef substrate overlay). When unset, falls back to:
2. **Runtime default** — 4. Bigger values fan out faster and spend provider capacity and tokens faster; this cap is the only bound on how many of one call's children run at once.

The cap throttles concurrency, not enrollment: if your `spawns` array has 10 entries and the cap is 4, all 10 are enrolled but only 4 run at a time — slots free up as each completes.

There's also a hard per-call ceiling (`MaxParallelSpawns = 32`) regardless of the per-agent cap. A `spawns` array bigger than that refuses up-front rather than truncating silently — split the work across multiple calls.

## Cost implications

Every child is a full agent run: its own loop, its own provider calls, its own iterations, its own tool dispatches. Three considerations:

- **API spend scales linearly with `spawns` length.** Parallel doesn't make it cheaper.
- **Children take no admission slot of their own.** A run and every sub-agent under it count as one run against the concurrency and per-user caps, so fanning out never makes a sibling run queue for admission. What children do contend for is provider capacity: a child on a provider whose concurrency the operator caps, and that no ancestor already holds, waits for that provider's slot.
- **Each child has its own `max_iterations`.** Children with `max_iterations: 64` × 8 spawns is 512 potential provider calls; that's a real cost.
- **A fan-out on your last iteration is not wasted.** When the last turn your `max_iterations` allows calls tools, you get one closing turn with their results and tools disabled; its answer is the run's. Plan the fan-out so that one turn can use what comes back.

A **code agent** orchestrating a fan-out is parked while its children run: the wait does not spend its run budget (`run_timeout_seconds`), so size that budget for the orchestrator's own work, not for the batch. The operator's lifetime limit on a code agent does count the wait. See `Context.help(topic="code-agents")`.

When in doubt: estimate `cost-per-child × N` before fanning out. If you can do the work with N=3 instead of N=10 by giving each child a wider topic, do that.

## Common shape mistakes

- **`op:"parallel_spawn"` with `name` / `prompt` at the top level.** Move them into the `spawns` array. The tool refuses this up-front so you see the mistake immediately rather than getting an empty envelope.
- **`op:"parallel_spawn"` with an empty `spawns` array.** Refused — write the entries before calling, even if you're about to populate them.
- **Mixing `op:"spawn"` with a `spawns` array.** Also refused — pick one shape per call.
- **Forgetting `op`.** `op` is optional ONLY for the single-spawn shape. `parallel_spawn` requires `op: "parallel_spawn"` explicitly.

## See also

- `Context.help(topic="scopes")` — how `agent` / `user` / `global` scopes shape Memory + Channel cursor namespacing across sub-runs.
- `Context.help(topic="subagents")` — recursion depth cap, sub-run lifecycle, transcript persistence.
- `Context.help(topic="fairness")` — the per-user admission cap, and why sub-agents don't count against it.
- `Channel.publish` — the alternative to fan-out when you don't need the join-back.
