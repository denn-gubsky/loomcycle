---
name: Agent/spawn
description: "Agent op=spawn — run one registered sub-agent on a task and wait for its final answer, or start it in the background with mode poll."
---
`spawn` hands one task to one sub-agent and returns its final reply when it
finishes. It is the default: a call with no `op` is a `spawn`. **The `prompt`
is everything the child knows about the task** — it does not see your
conversation — so put all it needs in it.

## Arguments

- `name` (required) — a registered agent name (`Context {"op":"agents"}`).
- `prompt` (required) — the task, complete. No tokens or keys: the child gets
  its own credentials.
- `def_id` — run a specific version of that agent (from `AgentDef`). The
  version's name must match `name`.
- `compaction` — override the child's context compaction (it inherits yours):
  `enabled`, `target_percentage` (10–50), `keep_last_n`, `keep_first`,
  `autocompact_at_pct` (50–95), `model`.
- `timeout_ms` — bound the child's run. When it runs out the child is
  cancelled (with everything it started) and the call fails naming the
  timeout. Time the child is held for review does not count. Absent or 0 =
  wait however long it runs. The operator may set a ceiling; a larger value
  is refused with the ceiling in the message.
- `mode` — `wait` (default): the call returns the child's answer. `poll`: the
  call returns at once and the child works in the background while you go on;
  read its result later with `poll`. A poll-mode child is still bounded by
  `timeout_ms`, still counts toward your live children until it ends, and is
  cancelled if your run is.
- `notify` — poll mode only. `true` (default): your next turn after the child
  finishes starts with a short note saying so. `false`: no note; you poll.
- `on_parent_end` — poll mode only. `wait` (default): if you end your turn
  while the child still runs, your run waits for it, then gives you one more
  turn. `cancel`: the child is cancelled when you end your turn.

Do not pass `spawns` here; that is `parallel_spawn`.

## Returns

The child's final text, as plain text, starting with a line
`[sub-agent agent_id=a_... run_id=r_...]` — `run_id` is the child's run, for
anything that needs to refer to it later. A reply longer than a quarter of
your context window is cut to that length and ends with
`[truncated at N characters; the full answer is in the transcript of run r_...]`
— the whole answer stays in that run's transcript. A child that keeps structured state
adds `Final state:` and its JSON. The state counts against the same quarter:
one that fits is kept whole and the text gets what is left; a larger one is
left out, with `[final state omitted: N characters, ...; it is in the
transcript of run r_...]` in its place. A failure names the run in its message
(`run=r_...`).

In poll mode the call returns `{child_run_id, agent, state}` with `state`
`"running"` — the child's run id, before it has done anything. Its answer
comes back from `poll`.

## Errors

- `missing required field: name` / `prompt` — add it.
- `unknown sub-agent "X" ...` — no agent is registered under that name for
  your tenant. Check `Context {"op":"agents"}`; retrying is pointless.
- `Agent tool: def_id "..." is for agent "Y", not "X" ...` or `... is
  retired` — use a `def_id` that belongs to `name` and is live, or omit it.
- `op=spawn must not carry a 'spawns' array` — you mixed in the
  `parallel_spawn` shape.
- `sub-agent "X" failed (...)` — the child ran and failed. The message says
  why; decide whether to retry with a clearer prompt.
- `max sub-agent recursion depth (3) reached ...` — you are too deep to
  delegate; do the work yourself.
- `sub-agent "X" timed out: timeout_ms=N elapsed ...; its run r_... was
  cancelled` — the child ran out of time. What it did before the bound is in
  its run's transcript; give it more time or a smaller task.
- `timeout_ms=N is above this runtime's ceiling of M ms` — pass at most M.
- `this run has N children alive and may have at most M at once ...` — wait
  for children you started to finish, or close resident ones, then retry.
- `mode "poll" is refused on your last iteration ...` — no turn would be left
  to collect the child; use `mode: "wait"`.
- `notify and on_parent_end apply to mode "poll" only` — add
  `"mode": "poll"` or drop them.

## Examples

Delegate one task and wait for the answer:

```json
{"op": "spawn", "name": "cv-adapter", "prompt": "Write a one-page CV for candidate c_4471 targeting a senior backend role. Profile: 8 years Go, Postgres, Kubernetes; led a team of five."}
```

```json result
"[sub-agent agent_id=a_9f2c41d07be35a18 run_id=r_2b7e90c41d6f3a58]\n# Jane Doe — Senior Backend Engineer\n..."
```

Bound a child that might hang to two minutes:

```json
{"op": "spawn", "name": "researcher", "prompt": "Find the release date of PostgreSQL 18 and cite the announcement.", "timeout_ms": 120000}
```

Start a child in the background and keep working; collect it later with
`poll`:

```json
{"op": "spawn", "name": "researcher", "prompt": "Find the release date of PostgreSQL 18 and cite the announcement.", "mode": "poll"}
```

```json result
{"child_run_id": "r_6a1f0c9e2b7d4835", "agent": "researcher", "state": "running"}
```

The same, with a child that should compact its context early on a long task:

```json
{"op": "spawn", "name": "researcher", "prompt": "Survey the 2026 literature on retrieval-augmented agents and list the ten most cited papers with one line each.", "compaction": {"enabled": true, "autocompact_at_pct": 70}}
```
