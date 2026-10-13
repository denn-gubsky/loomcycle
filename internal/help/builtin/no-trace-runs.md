---
name: no-trace-runs
description: runs whose content is delivered to the caller and stored nowhere — the retention mode on a run, an agent or a token, what is kept, and what such a run cannot do.
---
By default loomcycle keeps everything a run says: transcripts are what
make resume, continuation, the console and debugging work. A **no-trace
run** is for content that has one purpose and one reader. What it says is
delivered to whoever is waiting for it and stored nowhere.

## Turning it on

The mode is called `retention`. Its values are `none` and the default.

- **A run:** `"retention": "none"` on `POST /v1/runs`, a `runs:batch`
  child, gRPC `Run` / `SpawnRunBatch`, MCP `spawn_run` / `spawn_runs`.
- **An agent:** `retention: none` in the yaml, a markdown agent's
  frontmatter, or the `AgentDef` create/fork overlay.
- **A token:** `--retention none` on `operator-token create`, or
  `retention` wherever a token is minted.
- **A parent run:** every sub-agent of a no-trace run is one.

The strictest wins and nothing loosens it. A request for `default` under
a `none` token or agent runs as `none`. An unknown value is refused.

## What is kept

- The run and session rows, their links, agent, user and tenant.
- Status, a stop reason from the runtime's own list, the error's class.
- Model, provider, token counts, cost, timings, and the usage ledger.
- A marker for every event: its type, time, size, tool name and ids.

## What is not stored

- Prompts, the system prompt, model text, reasoning.
- Tool arguments and results, a sub-agent's answer.
- The final answer, the error's text, a typed cancel reason.
- The run's settings record, a title or summary, trace-index rows,
  banked turns.

## What is still delivered

A caller holding the connection gets the full stream and the full error
text. A parent gets its child's answer. A detached run returns its id and
nothing readable afterwards: its agent must deliver its own result.

## What it cannot do

Each of these needs the content stored, so each is refused with
`retention_none_unsupported`:

- be interactive or held for review;
- join an existing session, be continued, replayed, compacted or recapped;
- be configured as a draft, sent a message, or retuned;
- ask a person through the Interruption tool, or be evaluated;
- start a team walk.

## Worth knowing

- Tools that write on purpose (Memory, Channel, Document, Write, MCP
  tools) store what the agent tells them to. Give a no-trace agent only
  the tools it should be able to write with.
- Hooks you configured are sent what they were always sent.
- A fork can add the mode to an agent and cannot remove it.
- A pause does not park a no-trace run; the pause result counts it in
  `unpausable_runs_count`, and a snapshot leaves it out.
- A restart loses it: at boot it is failed with stop reason
  `not_resumable` and its idempotency key is released.
- Its chat is deleted at its age and never exported.

## Debugging one

An operator can store the runs of one no-trace agent, in one tenant, for
a limited time:

- `PUT /v1/_retention/overrides` with `{"agent": "judge", "until":
  "<RFC 3339 time>"}`, at most 24 hours ahead. `DELETE
  /v1/_retention/overrides?agent=judge` ends it early; `GET` lists what
  is in force.
- It lifts a mode that came from the agent's definition or the run
  request. It never lifts one a caller's token requires, and it does not
  lift the mode a sub-agent inherits from a no-trace parent.
- An admin sets one for any tenant, a tenant operator for its own. A
  member may not.
- Setting and clearing are audited, and the console shows a banner while
  one is in force.
- The same three operations are the gRPC `RetentionOverride` RPC, the MCP
  tool `retention_override`, and methods on the TypeScript and Python
  clients.
- Runs made in the window are ordinary stored runs; delete their run
  trees when the debugging is done.
