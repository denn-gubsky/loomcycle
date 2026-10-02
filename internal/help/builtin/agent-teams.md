---
name: agent-teams
description: Agent teams — a TeamDef is a state-machine workflow (states + transitions + a per-state handler agent) that a team walks task-by-task, driven either by the deterministic op=run autopilot or by an LLM team/orchestrator.
aliases: [teamdef, teams, team]
---
An **agent team** is a workflow plus the agents that carry it out, captured as a
`TeamDef`. The definition is a **state-machine graph** — the graph *is* the
workflow, and it is domain-agnostic (software delivery, marketing, accounting,
research, …).

## The model

- **States** are the steps a unit of work passes through (e.g. `architecture` →
  `implementation` → `review` → `pr`). Each state binds a **handler**:
  - `agent` — one agent runs the step;
  - `parallel` — several agents fan out, then a `consolidator` agent reads their
    outputs and picks the outgoing edge;
  - `consolidator` — a standalone judging step;
  - `terminal` — an end state (no agent, no outgoing edges);
  - `starter` — dispatches a wave of runs from a channel or a document (see
    Starters below).
- **`timeout_ms`** on a handler bounds how long its runs may take (`0` or
  unset = no limit):
  - on `agent`, `parallel` and `consolidator` it bounds ONE execution of the
    handler — every run it starts (the agent, each fan-out member, the
    consolidator) — measured from when the state starts. When it runs out, the
    runs still going are cancelled and the state fails with
    `state "<id>" timed out: timeout_ms=<N> elapsed`, which ends the walk like
    any other handler failure;
  - on `starter` it bounds EACH spawned run, from its dispatch. A run that runs
    out is cancelled and publishes `status: "timeout"` to the sink (still one
    message per run); it does not count toward the wave's `wait`.
  - Time a run spends held for a review verdict does not count. A starter run's
    clock stops while it is held; a handler-wide clock stops while every run in
    flight is held.
- **Transitions** are the edges between states, gated by an `on` label:
  `success` (advance), `pushback:<reason>` (loop back for rework), or
  `conditional:<expr>`. A state's outbound labels are unique, and every cycle is
  bounded by a per-state `max_iterations` cap so a workflow always terminates.

## Starters — a wave of runs from a channel or a document

A `starter` state dispatches a **wave**: one agent run per work item, each
result published to its `sink` channel (one message per run). Its `source`
says where the items come from:

- **A channel** — `source: {channel: "pr-events"}`. Each message is an item;
  `fanout.per: "message"` (one run each, `max` required) or `"once"` (one run
  holding the batch). `ack`, `wait`, `n`, `wait_ms`, `batch` shape the read.
- **A document** — `source: {kind: "document", path: "/specs/acme"}`. Each
  **top-level section** (the root's direct children, in order) is an item:
  - `path` is a fixed absolute path; `scope` is `user` (default — the tree of
    the person the walk runs as) or `tenant` (needs the tenant memory and SQL
    Memory grants a Document read needs);
  - `fanout.per: "chunk"` runs one agent per section and requires `max`;
    `"once"` runs one agent holding every section;
  - the document is read once, when the wave dispatches — an edit during the
    wave does not change the items;
  - more sections than `max` fails the walk (both numbers are named), and so
    does a document with no sections;
  - `ack`, `wait`, `n`, `wait_ms`, `batch` and `per: "message"` are refused —
    a document has no cursor and nothing to wait for;
  - a team whose entry state reads a document is never started automatically;
    run it with `op=run`.
- **The walk's input** — `source: {kind: "input"}`. The value the walk is
  run with is read once, when the wave dispatches, so a team starts from its
  own input in one `op=run` and processes exactly that input:
  - a JSON array is one item per element, in order; any other JSON value
    (an object, a string, a number) is one item, the value itself; text that
    is not JSON, or no input at all, is the item `{"text": "<input>"}`;
  - `fanout.per: "message"` runs one agent per item and requires `max`;
    `"once"` runs one agent holding every item. More items than `max` fails
    the walk (both numbers are named), and so does an empty array;
  - it must be the definition's `entry` state, and nothing may lead back into
    it — no transition (a pushback or conditional retry, a self-loop) and no
    cap `reroute`: its items are the input the walk was started with, so
    route a retry to a later state;
  - it may carry `schema`, the run form a client renders, as an `input`
    state does (no `input` state can come before the entry) — see The input
    form;
  - `source.channel`, `path`, `scope`, `select`, `wait`, `n`, `wait_ms`,
    `batch`, `ack` and `per: "chunk"` are refused — it reads no store and
    has no cursor;
  - it is never started automatically: there is nothing to watch.

Each run receives its item in `{{starter.message}}` (`{{starter.messages}}`, a
JSON array, for `per: "once"`); put the placeholder in `prompt.input`. A
document item is `{document_id, chunk_id, index, title, markdown}`, where
`markdown` is the section with everything under it. The item is data: a
`{{…}}` written inside it is never expanded. `binds` project fields of the item
into variables, e.g. `binds: {title: "$.title"}` → `${var.title}`.

## When to use a Starter — and when an agent state is enough

A Starter is not the default first state. Use one when at least one of these
holds:

1. **Each run needs data from the item it works on.** A Starter puts the
   message or section into the run's prompt (`{{starter.message}}`), and
   `binds` lift its fields into variables — a document id, a chunk id, a ticket
   number. Data the whole walk shares needs no Starter: an `agent` state's
   `system_prompt` and `input_template` expand `${var.*}`, `${now.*}`,
   `{{document:…}}` and `{{tool:…}}` too, and `{{memory:key|search:…}}` when the
   TeamDef is operator-authored. (`binds` read the FIRST message of a wave;
   what differs per run reaches it only through `{{starter.message}}`.)
2. **The number of runs depends on the input.** `per: "message"` runs one
   agent per message read; a document source with `per: "chunk"` runs one per
   top-level section, so a spec with 3 sections gets 3 runs and one with 12
   gets 12 — up to `max` (at most 32), 4 at a time. `parallel` cannot do this:
   its agent list is fixed when the team is written.
3. **Nothing should run until there is work.** A Starter waits for its
   channel inside the walk: no agent is started, no model is called and no
   provider slot is taken until a message arrives. An agent told to wait on a
   channel is a live run while it waits. It holds its provider slot and a
   place in its wave, and every empty `subscribe` (each capped at the long-poll
   limit, 30 s by default) ends a model turn, spending tokens and one of its
   `max_iterations`. Only a team whose entry state is a Starter can be armed to
   start itself when a message arrives (team subscriptions).

Otherwise an `agent` state can read the channel itself. List the `Channel`
tool and grant the channel in the AgentDef's `channels.subscribe` (the
TeamDef's `channels` ACL covers Starters and `channel` nodes only), and say in
its prompt what to read. This fits an agent that is already running and
consumes a stream as part of its work.

Without a Starter you also give up:

- **the sink's guarantee** — a Starter publishes exactly one message per run
  (`ok`, `error`, `timeout` or `rejected`), even for a run that crashed or was
  cancelled; an agent told to publish may not;
- **redelivery** — with `ack: "after_results"` (the default) a failed wave
  leaves its input on the channel for the next walk; an agent's own
  `subscribe` commits the previous batch when it reads the next;
- **the `before_dispatch` breakpoint**, which only a Starter has (a
  `<state>:review` hold works on `agent` and `parallel` states too).

## Publishing to a channel

- A `channel` state publishes its input to `channel` and passes it on
  unchanged. The message is `{"state": "<id>", "output": "<input as text>"}`
  unless it sets `payload: "raw"`, which publishes the input itself as a JSON
  value: JSON input as it is (an object stays an object), any other text as
  `{"text": "<input>"}`. A Starter's `binds` can read the fields of a raw
  message; inside the envelope they are only text.
- An `input` state may set `publish: {channel: "<name>"}`. It publishes the
  walk's input by the same rule, then applies its `capture`, and passes the
  input on unchanged.
- Both need the channel in the TeamDef's `channels.publish`. A publish that
  fails fails the state.
- A Starter that reads a channel takes its **oldest unacknowledged** message,
  which on a channel with a backlog may not be the one this walk just
  published. When a walk must process its own input, start the team with a
  Starter whose source is the walk's input rather than publishing to a channel
  and reading it back.

## The input form

A team's front door is its **input form**: `schema`, a JSON Schema a client
renders as the run form. It goes on an `input` state, or on a Starter whose
source is the walk's input (which must be the entry); only the **entry's**
form describes what a caller sends.

The runtime does not interpret the form except for one check: when `op=run`
starts a walk at its entry, the input is checked before any run starts or any
model is called. The input is read as a JSON value by the rule above (text
that is not JSON is `{"text": "<input>"}`), then:

1. a top-level `type` — one name or a list — must match: `object`, `array`,
   `string`, `number`, `integer` (no fractional part), `boolean`, `null`;
2. if the value is an object, every field in `required` must be present;
3. if the value is an object, a present field whose `properties.<field>.type`
   is declared must match it. One level only.

Nothing else is checked: not nested schemas, `enum`, `format`, `$ref` or
`x-` keywords. A bad input is refused naming the field and the rule, e.g.
`input field "chunk_id" is required` or `input field "document_id" must be a
string, got a number`; nothing has run, so fix the input and run again. A walk
resumed from a board at a later state is not checked.

**Pickers.** A field may carry `x-loomcycle-picker` to tell a client how a
person chooses its value:

- `kind` — `document` or `chunk` (`memory`, `channel` and `agent` are
  reserved);
- `scope` — the store to browse: `user` (default) or `tenant`;
- `under_path` — optional: narrows a document list to a Path subtree;
- `document` — chunk pickers only: the sibling field holding the document id,
  so the chunk list follows the chosen document;
- `depth` — optional: `1` lists top-level sections only.

The value stays the plain id string, so a headless caller just sends ids. The
runtime never validates picker values (an unknown document or chunk is not
refused at start); clients such as the canvas render them.

```json
{"type": "object", "required": ["document_id", "chunk_id"],
 "properties": {
   "document_id": {"type": "string", "title": "Document",
                   "x-loomcycle-picker": {"kind": "document", "scope": "user", "under_path": "/parts"}},
   "chunk_id":    {"type": "string", "title": "Part",
                   "x-loomcycle-picker": {"kind": "chunk", "document": "document_id", "depth": 1}}}}
```

## The task board

Live work rides on a **Document** used as a task board: one chunk per work-item,
and the chunk's `status` field holds its current state. The team advances a chunk
by moving `status` from one state to the next per the transitions. (See the
`document` help topic.)

## Two ways to run a team

- **`TeamDef op=run`** — the deterministic **autopilot**: it walks a team's graph
  end to end and returns the per-state trace. Headless, no human in the loop. It
  executes every handler kind — a single `agent`, a `parallel` fan-out (its agents
  run concurrently; `wait` = `all` | `any` | `at_least:<N>`), and a `consolidator`
  that reads the results and picks the outgoing edge, so `pushback` rework loops
  route just as they render. A consolidator selects its edge by emitting a line
  `signal: <edge>` (e.g. `signal: success` or `signal: pushback:redo`): the
  `<edge>` must match one of the state's outbound transition labels, the last such
  line wins, and no signal defaults to `success`. Every cycle is bounded by the
  per-state `max_iterations` cap.
- **The `team/orchestrator` agent** — an LLM **team lead** and the human's
  contact point. Run it interactively: it reads the TeamDef as its map, drives
  the Document board (moving `status`, spawning each state's handler), decides
  routing (including pushback + parallel fan-out via the Agent tool), and — for
  software teams — sets up an ephemeral repo volume and opens a PR. It keeps the
  human in the loop and is steerable mid-run.

## Authoring + running

- **Build** a team with the `team/assistant` agent (it assembles a TeamDef from
  agents that already exist) or the `TeamDef` tool directly
  (`op=create` / `fork` / `promote` / `render_diagram`). The graph is validated
  before any write — a dangling transition, unreachable state, or
  parallel-without-consolidator is refused.
- **Inspect** a team's shape with `TeamDef op=render_diagram` (a Mermaid
  `stateDiagram-v2` with the colour scheme applied).
- **Handlers** are ordinary agents; a team just names them per state. Missing a
  role? Build it with the `agent/assistant` agent first.
- **Hooks.** A state may carry `hooks` / `tool_hooks`, added to every run it
  starts; the TeamDef's own `hooks` take `run_end` for the walk. See
  `help(topic="hooks")`.

## Cross-references

- `help(topic="document")` — the chunked-graph Document used as the task board.
- `help(topic="hooks")` — webhooks and code hooks a state or a walk can add.
- `help(topic="subagents")` — how handler agents are spawned (spawn vs parallel).
- `help(topic="volumes")` / `help(topic="volumedef")` — the workspace a software team clones a repo into.
- `Context op=permissions` — your effective tools + scopes.
