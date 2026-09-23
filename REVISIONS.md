# LoomCycle release history

Per-version release notes for **v1.43.2 onward**, newest first. Earlier releases (v0.4.0 – v1.43.1) were never written up here; their notes live in the annotated git tags — `git tag -l --format='%(contents)' v1.20.0` prints one.

The current and immediately previous releases are also summarised in the main [`README.md`](README.md).

Each entry is the release's tag annotation, so the tag and this file cannot disagree about what shipped.

For the **public roadmap**, see [`docs/PLAN.md`](docs/PLAN.md).

## What's in v1.92.0

*A run keeps its answer and its prompt, an operator can tell an agent which tool to call first, and a local model's stateful chat stops mid-task no more.*

Eight PRs. Two start RFC DI (the Run as the unit a caller configures and reads), two close security gaps, and the rest are fixes found by live use and by review.

### A run keeps its result and its prompt (#1342, RFC DI-P1)

Nothing durable answered "what did this run say?" or "what was it asked?".

- **`runs.result`** holds the run's final text (and a stateful run's final Σ). It is written by `FinishRun` in the same update that makes the run terminal, and returned on single-run reads: HTTP `GET /v1/agents/{id}`, MCP `get_run`, gRPC `Agent.result`, TS `Agent.result`, Python `result`. Listings omit it to stay small. It has the transcript's retention and erasure tier. **Migration 0079.**
- **`GET /v1/runs/{run_id}/prompt`** returns the request the run's first model call was actually sent — the node role, injected metadata, `{{…}}` expansion and the stateful loop's instructions included. It is recorded once per loop entry as a store-only `prompt_snapshot` event. The recorded text is redacted, it is never streamed, and the Web UI hides it. TS `getRunPrompt()`.
- **Team members are addressable:** starter sink messages and parallel result envelopes carry each member's `run_id`, a failed member's included.
- **⚠️ Security, found on the way:** MCP `get_run` / `list_runs` could read **other tenants'** runs over a per-tenant MCP session. They are tenant-gated now, as HTTP and gRPC already were.

### `tool_choice` per agent and per run (#1344, RFC DI-P2a)

```
"tool_choice": {"mode": "auto|none|required|tool", "name": "WebSearch", "until": "first_call|until_called|always"}
```

- **What it is for:** "this agent must search before it answers", "this classifier must answer through its schema tool".
- **Where it is set:** agent yaml, the AgentDef overlay (with a Web UI editor), and per run on `POST /v1/runs`, continuation, gRPC, MCP `spawn_run(s)`, TS `toolChoice` and Python `tool_choice`.
- **`until` exists because a choice forced on every call never lets a run finish.** `first_call` (the default) forces only the first call; `until_called` forces until the model makes the call; `always` is refused for `required` and `tool`.
- **A provider that cannot enforce the choice** runs on and emits `capability_inert` (re-checked after a fallback).
- **Resume** does not force a choice the run already spent. A stateful run reports that it ignores the setting.

### Stateful mode: a plan is not an answer (#1343, #1345)

Observed live on `ornith-1.5:35b`: a stateful step carried a patch and a plan — "I will run a web search for the exact figures" — with **no action and no answer**. The loop read "no action" as "turn over" and showed the plan as the reply, so the chat stopped mid-task two turns running. Replaying that step against the model reproduced the shape in 4 of 9 replies.

- **A turn ends only when the model says so** — `done`, or an answer in `final`. A step that would end one without an answer is sent back, told what it missed, on the `max_patch_retries` budget. The same rule stops the model's inner monologue ("Operator said continue…") being shown as its reply.
- **The stateful prompt now shows the two accepted reply shapes as examples**, and names the wrong ones. Measured on the same model, first reply only: 5/9 → **10/10** valid at a working step, 5/8 → **7/8** at an answer step.
- **(#1345)** The stdio MCP client could report a delivered result as `server exited` when a server answered and exited straight after — a Go `select` picking at random between two ready cases. That was the flaky `TestServerCrashFailsInFlightCalls`, and a real defect for one-shot MCP servers.

### Fixes

- **⚠️ Tenant hook callbacks can no longer reach private addresses (#1339).** A hook receives every matching tool input, and a `substrate:tenant` operator could point one at the cloud metadata endpoint or an internal service — SSRF plus exfiltration. Tenant hooks now dial through the network guard (checked after DNS and on every redirect). Operator-global hooks are unchanged.
- **Anthropic: stop sending parameters current models reject (#1340).** Opus 4.7 and later, Sonnet 5, Fable and Mythos 400'd on the old request shape:
  - reasoning depth now goes out as `output_config.effort` with adaptive thinking, not a thinking budget;
  - non-default sampling is dropped on those models;
  - a forced `tool_choice` is dropped on Opus 5.5, Fable 5.1 and Mythos 5.1, which refuse it under any configuration. That forced choice is exactly what broke **every stateful run** on those models.

  Older ids are byte-identical.
- **A Post hook no longer strips a tool's error classification (#1338).** The server always wires a hook dispatcher, so the category, retry hint and backoff (RFC DA) were being dropped from every tool call in production.
- **A running team walk can be cancelled (#1341)** through `POST /v1/runs/{run_id}/cancel` and gRPC `CancelTurn`. Every member run it spawned stops with it, and the walk's row finishes `cancelled` with the reason. Single replica for now.

### Upgrade notes

- **Migration 0079** adds `runs.result`. It runs at startup, like every migration.
- **⚠️ A tenant hook whose callback is on a private network now needs the operator to allowlist that host** — `hooks.private_host_allowlist`, or `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST`. Until then the call follows the hook's `fail_mode`.
- **Anthropic on current models:** a configured `temperature` / `top_p` / `top_k` is now dropped (with a log line) instead of failing the request.
- **Stateful agents on weaker models** may spend a retry where they used to end a turn early. That is the intended trade: a correction call instead of a wrong answer.
- **The adapters are bumped to 1.92.0 WITH new surface:** `getRunPrompt`, `toolChoice`, `Agent.result` (TS) and `tool_choice`, `result` (Python).

## What's in v1.91.0

*A stateful answer the model wrote into its state reaches the operator.*

One runtime fix (#1336), found the morning after v1.90.0 shipped, on the same local model that produced v1.90.0's report.

### ⚠️ The answer was in the state, and the operator saw an empty turn

Observed live on `ornith-1.5:35b` (ollama-local, `mode: stateful`). The model ended its turns with its reply fields **inside** the patch:

```
{"patch": {"done": true, "final": "<the whole answer>"},
 "action": {"tool": "emit_state", "input": {}}}
```

The loop reads only the top-level `final`. What followed:

- The answer was merged into Σ, and naming `emit_state` as the action drew a "not an action" error.
- v1.90.0's empty-turn re-prompt could not help. Asked for its answer "in `final`", the model believed it had given one and repeated the same shape, so the operator saw *"(the model ended its turn without an answer; it updated its state: done, final)"*.
- The stale answer then stayed in Σ into the next turn, where the model read it back as something it knew.

What changed:

- emit_state's own fields — `final`, `done`, `action`, `reasoning` — found inside the patch are moved to where they belong and **deleted from the state**. That also clears a stale copy left there by an earlier turn.
- A field the **state schema declares** is left alone.
- An `emit_state` action that carries an answer, or says it is done, ends the turn instead of being refused. One with neither is still refused as before.
- The tool description, the system prompt and the re-prompt now say plainly that these fields sit **next to** `patch`, never inside it.

### Also on main

- **Memory benchmarks (#1335):** the cascade gate's routing measured on LoCoMo, with results under `bench/` and a section in `docs/MEMORY-ARCHITECTURE.md`. Docs and data only; no runtime change.

### Upgrade notes

- **No wire, schema or config change.**
- **An existing stateful session may already carry a stale `final` / `done` in Σ** from before this release. It is removed the next time the model writes that field into its patch again. A model that now answers correctly, at the top level, leaves the stale copy in place — start a new chat if one is confusing the model.
- **The adapters are bumped to 1.91.0 with no surface change**, to keep the client version matching the runtime.

## What's in v1.90.0

*A stateful chat keeps its state and stays live — v1.88.0 made it park, and this makes the park worth something.*

One PR (#1333), 23 commits, one per finding of a code review of the structured-state loop. The review and the per-commit status are in the doc store at `/loomcycle/docs/stateful-context-review-digest` (§10, §11).

### ⚠️ An interactive `mode: stateful` chat threw its state away every turn

v1.88.0 gave the stateful loop a park, and the runtime did park — but the embedded `/run` terminal saw something else.

1. The loop emitted `done` at **every** turn boundary before parking. `done` is terminal to every consumer, so the terminal marked the chat **completed** after its first answer.
2. The terminal then sent the operator's next message as a **session continuation**: a new, non-interactive run.
3. That run started from an **empty state**, and its first observation was the whole replayed transcript, relabelled `Task:` — the history stateful mode exists not to feed back.
4. The original run stayed parked forever, heartbeating and holding its concurrency slot.

So a chat looked like it worked while discarding its state each turn and leaking a run per chat.

Now:

- The run emits **one** `done`, when it actually ends. The turn boundary is `awaiting_input`, exactly as in the append loop.
- The terminal steers a parked run whatever its status says.
- Resume and session continuation start a stateful run from its **last recorded state** and the observation it was waiting on — the result of the action it chose, or the operator's message — never from the replayed transcript.
- An action that was mid-flight when the process stopped is reported to the model as interrupted and is **not** re-run.

### The stateful loop skipped what every other run gets

`runStateful` branched out of the run before most of what a run sets up. Each of these is fixed:

| missing | consequence |
|---|---|
| **heartbeat** | a working stateful run sent none, so the stale-run sweeper failed it as `heartbeat_timeout` 10 minutes in, while it kept running and spending tokens |
| **tool-use hooks** | an operator's Pre-hook **deny did not apply** to a `mode: stateful` agent; Post-hooks, host-widening audit and the parallel_spawn ledger were skipped too |
| **context-transform plugins** | the `redact` secret scrubber never saw a stateful request — and Σ and the observation are where tool output lands |
| **secret masking on persist** | tool results were masked in the events BLOB, but the copy of them in Σ, stored on every step, was not |
| **retry and fallback** | one 429 or "overloaded" killed a stateful run that an append run would ride out |
| **runtime pause** | a stateful run kept calling its provider while the runtime reported itself paused, and never reached the one state resume re-dispatches |
| **`context_size` / `op=self`** | a per-agent context size never reached the provider (Ollama used its own), and `Context op=self` reported no provider, model or footprint |

### Smaller fixes

- **`POST /v1/runs/{id}/compact` on a stateful run returns 409 `stateful_run`.** It used to summarise the replayed transcript (a model call), bank the span, answer `compacted: true`, and then the loop dropped the summary.
- **A turn whose answer went into the state instead of `final` is asked again**, and is never shown as an empty message.
- **An action missing a required input field is named back to the model** before dispatch, e.g. `{"tool":"Interruption","input":{}}`.
- **The Web UI matched tool results by a field the server never sends**, so an `Interruption` chip read `interrupted: ?` and the busy hint named a tool as running for the rest of the run. This affected every mode.
- Stateful action ids are unique across a session's runs.
- A large patch is weighed before the next request goes out, not one step late.
- A correction request replays the model's own thinking block (Anthropic and DeepSeek reject a replayed turn without it).
- An object patched where there was none drops its nulls (RFC 7386).

### Upgrade notes

- **Consumers of the event stream:** an interactive stateful run no longer emits a per-turn `done` — only one, when it ends. That is what the append loop has always done. A client that rendered each stateful answer on `done` should use `text` / `awaiting_input`, as it already must for append runs.
- **`mode: auto` still sends an interactive run to recap.** This is kept on purpose until interactive stateful chats have been proven in the field. An explicit `mode: stateful` is honoured.
- **Still not wired for stateful runs:** turn-cancel (`POST /v1/runs/{id}/cancel` still 409s) and a per-turn step budget (RFC DH P3).
- **No wire, schema or config change.** The adapters are bumped to 1.90.0 with **no surface change**, to keep the client version matching the runtime.

## What's in v1.89.0

*A stateful run reports the tokens it spends — and until now it spent them against no budget at all.*

One PR. Small, and worth deploying for the second paragraph rather than the first.

### ⚠️ `mode: stateful` was exempt from token budgets (#1331)

Reported as `tokens: 0 in / 0 out` on a RUNNING interactive chat, which is
the visible corner of it and the least of it.

`runStateful` accumulated its totals into the final `EventDone` and emitted **no
per-call `EventUsage` at all**. That event is what three separate subsystems key
off:

| consumer | consequence |
|---|---|
| the UI gauge | a stateful run read `0 in / 0 out` for its whole life |
| the RFC AV ledger | `recordCallUsage` never fired → `token_usage` had no rows → `GET /v1/_usage` was blind to stateful runs → `runs.cost` summed an empty set to NULL |
| **the RFC AW budgets** | `limits.Add` never fired → a `mode: stateful` agent spent tokens that counted against **no per-scope budget** |

The last row is why this is not a display bug. **An operator who set a hard
token limit was not protected from this mode**, and nothing said so — the run
looked like it cost nothing.

The event is now stamped like the append loop's: the effective window, so the
gauge has a denominator, and the SERVING provider, so the ledger records which
key paid across a mid-run fallback.

### A test that leaked a parked goroutine

Found by CI going red on a DATA RACE that named the wrong test.
`parkHeartbeatInterval` is a package-level var one test lowers to keep itself
fast — safe only while no other goroutine is inside `parkForInput`, which reads
it. v1.88.0's resume test started an interactive run and never awaited it, so
the leaked park read the var while the next test wrote it, and the failure
surfaced a long way from the cause. Awaited now, with a note at the var saying
what mutating it costs.

### Upgrading

- **Check your usage figures for stateful agents.** Every `mode: stateful` run
  before v1.89.0 wrote NO ledger rows, so its tokens are absent from
  `GET /v1/_usage`, its `runs.cost` is NULL, and it never incremented a budget
  counter. The history cannot be reconstructed — the per-call figures were never
  recorded. Counters are correct from this release forward.
- **⚠️ If you rely on RFC AW token budgets, re-check them against a tenant that
  runs stateful agents.** Its month-to-date total was under-counted by exactly
  those runs.
- **The adapters are bumped to 1.89.0 with NO surface change**, as in v1.88.0 —
  the bump keeps "the client version matches the runtime version" true rather
  than announcing anything new.

## What's in v1.88.0

*A stateful chat waits for you instead of ending, and remembers what it knew when it wakes up.*

Three PRs, all from one operator report against v1.87.0, and all in the
stateful loop. RFC DH P1 + P2 — released together on purpose, because P1 alone
is honest in-process and a lie the first time a replica restarts.

### The chat stopped after every turn (#1328, RFC DH P1)

> The chat/local model with stateful context still stops the agent after each
> turn regardless of interactive flag state.

It did, and `grep SteerQueue internal/loop/stateful.go` returned **zero hits**.
The loop reached the model's `done` and RETURNED, where the append/recap loop
calls `parkForOperatorTurn` instead. `interactive: true` was not ignored so much
as unimplemented — the flag reached a loop with nowhere to put it.

⚠️ **AND THE RUNTIME ALREADY BELIEVED THESE WERE INCOMPATIBLE.**
`resolveAutoContextMode` refuses to route an interactive run to stateful and
says why in its own comment — "that loop has no steer/park". But it governs
`mode: auto` only, so an **explicit** `mode: stateful` walked past the guard in
silence. The protection you got depended on how little you specified.

A stateful run now parks at `done`, and your next message becomes its next
**observation**, prefixed `operator: `. The park belongs at `done` and not at
every step: the step loop is internal machinery you never see, and the thing
you wait for is the answer.

⚠️ **THE RUNTIME STILL DOES NOT WRITE Σ.** A reserved key filled by the runtime
would make Σ half model-authored and half not, with `state_schema` validating
only one of the halves. The prompt instead tells the model that this observation
is a person speaking and that nothing survives the turn except the state, then
leaves it to decide what is durable — which is its job in this mode. That
paragraph is added for interactive runs only; an autonomous one cannot receive
such an observation.

The iteration cap is also now computed once, in `Run`, and handed down. This
loop derived its own and missed every lift `Run` applies — including the one
that matters, since each PARK consumes an iteration. A parked chat on the
default 16 would have died after a handful of exchanges reporting
`max_iterations`, which is an answer about the wrong thing.
`unbounded_iterations` stops being inert in this mode.

### And it forgot everything when it woke up (#1329, RFC DH P2)

Σ survives a live park because the goroutine holding it does. Across a pause, a
snapshot restore or a replica move it was lost.

⚠️ **A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES.** `replayTranscript` rebuilds
a conversation, which is precisely what stateful mode exists to *not* have — the
model is fed only (Σ, observation). A resumed run handed only `PriorMessages`
started from an **empty Σ** and cheerfully continued a conversation whose every
established fact it had forgotten. That is worse than refusing to resume: it
looks like it worked.

No new column: each `context_state` marker already carries the whole post-merge
Σ, so the last one is the answer. ⚠️ Which makes that marker **load-bearing**,
where its own comment used to say "still persisted for audit" — both comments
now point at each other, so anyone trimming it to save transcript bytes meets
the reason not to.

### `tool not found: emit_state` (#1327)

From the same chat. The model named `emit_state` as its ACTION, and the action
name went straight to the dispatcher unchecked — so the answer was a lookup that
could only miss.

⚠️ `emit_state` is not a tool. It is the channel the model is already speaking
through, offered as the only entry in `tools` on every step, and its `action`
field names a DIFFERENT tool for the runtime to run. Both that and any unoffered
name are now refused in the loop, which is the only place that knows which tools
THIS agent was offered — the dispatcher knows every tool in the process, so "not
found" is the most it can ever say. The observation names the mistake AND the
alternatives.

### Upgrading

- **Nothing to change.** An interactive `mode: stateful` agent — `chat/local` in
  the bundled `chat` preset — starts behaving as a terminal chat on upgrade:
  it parks for your next message instead of ending the run.
- **Autonomous stateful runs are unaffected.** They still end at `done`; the
  park requires both `interactive` and a steer queue.
- **⚠️ Do not trim `context_state` events from a transcript.** They are how a
  resumed stateful run recovers its state.
- **The adapters are bumped to 1.88.0 with NO surface change.** Nothing in
  `@loomcycle/client` or the Python client differs from 1.87.0 — the bump keeps
  "the client version matches the runtime version" true rather than announcing
  anything new.
- Still open from RFC DH: **P3** — RFC BH turn-cancel needs a defined resting
  place in the stateful loop (cancelling mid-dispatch leaves an action whose
  result nothing will read), and a per-turn step budget.

## What's in v1.87.0

*A mistyped key can no longer mint an admin token, a summarizer gets the budget to answer, and the emit_state contract is finally on the wire.*

Five PRs, four of them opened by one operator report and one by a security
incident. ⚠️ **One breaking change — see Upgrading.**

### ⚠️ A mistyped key minted admin tokens (#1324)

Hit live while minting bench tenants. The caller sent `allowed_scopes` — which
is what the *response* echoes and what the stored column is named, so it is not
an unreasonable guess — `json.Unmarshal` dropped the unknown key, `scopes` came
through empty, and the documented default fired:

```go
if len(scopes) == 0 {
    scopes = []string{auth.ScopeAdmin}   // "single token, full power"
}
```

Three tenant tokens were minted as **admin**. HTTP 200, a plausible-looking
response, no error anywhere.

**And the mistake is self-locking.** One non-retired admin-scoped def turns OFF
the `LOOMCYCLE_AUTH_TOKEN` fallback, so the accidental token revokes the very
bearer that minted it — every later admin call returns an opaque 401 and there
is no way back through the API. Two of the three deployments needed SQL to
recover.

Two layers now, and neither alone is the fix. An omitted scope list is
**refused** rather than escalated — admin is stated, not inherited, so a typo is
an inert 400 instead of maximum privilege. And the mint tool refuses **unknown
keys**, deliberately unlike the rest of the substrate: everywhere else a stray
field just means you get a def that differs from what you typed, but here the
dropped field decided *privilege*. `import_token` keeps the old default, since
binding the legacy token already says which scope it carries.

### The stateful loop stopped killing runs over a conversational reply (#1320)

`chat/local` FAILED on its first turn with *"model did not call emit_state"* —
4287 in / 557 out, 45.4s, before the operator's "Hello. What can you do?" got
any answer. The model was not broken: it was asked a conversational question and
answered conversationally, and the loop threw 557 tokens of perfectly good prose
away and gave up.

`on_invalid_patch` / `max_patch_retries` covered a patch that ARRIVED and failed
validation; a model that never called the tool got no retry at all. Both are the
same condition from the runtime's side — the output was not usable and the model
can fix it — so a missing call is now retried under the same budget, re-prompted
with what the model actually said. What it produced is no longer discarded
either, so the terminal error can say what the run *got* rather than only what
it wanted. Thinking is kept separately and never replayed: a reasoning-only
reply looks identical to silence from outside, and those need opposite fixes.

### The emit_state contract is on the wire (#1321, RFC DG P1)

`grep -rn "tool_choice" internal/` returned **one** hit before this release, in
a comment about OpenAI-compat fields the gateway ignores. loomcycle had never
sent `tool_choice` on any driver — so a model that ignored "call `emit_state`
exactly once" was not disobeying anything the wire ever said.

`providers.ToolChoice{Mode,Name}` now maps onto all three dialects (Anthropic
`{type:any|tool|none}`, OpenAI `required`/the nested named form, Gemini
`functionCallingConfig`), with deepseek/vllm/llamacpp inheriting through the
OpenAI driver. The zero value is auto, so every unopted request is
byte-identical.

⚠️ **Ollama is non-mandatory, by decision.** `/api/chat` has no `tool_choice`
and neither does its OpenAI shim, so a forced request there DEGRADES rather than
being refused — refusing would make every local-model agent unrunnable to buy a
guarantee it never had. The run says so when it finally fails, because "the model
ignored the constraint" and "there was no constraint" call for opposite next
moves.

⚠️ **Anthropic drops a forced choice under extended thinking**, which the driver
attaches whenever an agent sets `effort` on a reasoning-capable model. That pair
400s, so thinking wins and the choice is dropped with a log line — the same
precedence already applied to temperature.

### A summarizer gets the budget to answer, not just to think (#1322)

Reported from the terminal on `chat/local-small`:

> Context recap declined at 87% of the window: the summarizer returned no text …
> **raise recap_max_chars**, or choose an effort that stops the model thinking

That advice is the tell — it asks you to lengthen the summary to fix a *budget*.
`recap_max_chars` asks the PROMPT for a length, and the runtime derived the
model's OUTPUT CAP from the same number: 192 tokens at the default. A reasoning
model spends that thinking before it writes a word.

The cap now has a floor independent of the requested length, and the call asks
for the least reasoning the provider will give it — it previously sent **no**
effort at all, and on Anthropic and Ollama an unset hint means "the model's own
default", which for qwen3/deepseek-r1 is to think. If that hint breaks a
provider (OpenAI passes it through as `reasoning_effort`, which a non-reasoning
model rejects) the call drops it and retries once, which can only turn a decline
into a summary.

### Recap can run on its own model (#1323)

`compaction:` has had a `model:` override since it shipped; `context:` never got
one, so a recap always ran on the agent's own model. ⚠️ The point is the model's
BEHAVIOUR, not its context window: a recap reads a span and writes a short note,
so it is never the call that runs out of window — what breaks it is a reasoning
model. Point `context.model` at a cheap non-thinking summarizer beside a
thinking chat model.

Carried across all six surfaces, with a guard that reflects over
`config.Context`'s json tags rather than holding a list. It found a real
pre-existing gap on its first run: `recall` and `harvest_to_memory` were missing
from the MCP per-run context schema, so an MCP caller could not set either.

### Upgrading

- ⚠️ **BREAKING: `loomcycle operator-token create` now requires `--scopes`.** It
  refuses in the CLI and prints both spellings plus the lockout warning.
  `--copy-from-env` is exempt. The Web UI is unaffected — it already sent
  `scopes` explicitly and refused an empty set. If you script token minting,
  add the flag.
- ⚠️ **Check your existing tokens.** If any were minted without an explicit
  scope list, they are admin. `select name, allowed_scopes from
  operator_token_defs where retired_at is null;` — and note that an unintended
  admin row also disables the legacy `LOOMCYCLE_AUTH_TOKEN` login.
- **`effort` on an Ollama stateful agent now reaches the wire as a forced tool
  call** where the provider supports one. Nothing to change; a local model that
  ignored the prompt contract is now re-prompted rather than fatal.
- **The `History op=recap` token cap rose from 160 to 1024.** A deliberate
  trade: 160 was a tight guard against a verbose model and a guaranteed EMPTY
  result against a reasoning one.
- **Adapters at 1.87.0** (`@loomcycle/client` on npm; Python rides its own
  `python-v1.87.0` tag). `context.model` is new public surface on both.
- Cut as a minor, not a patch: a patch tag builds only
  `denngubsky/loomcycle-browser`, and these fixes have to reach the images a
  deployment pulls.

## What's in v1.86.0

*The distillation reports v1.85.0 added now wait for a number the provider returned — and every wire event is finally nameable from a typed client.*

Three PRs, all fixes, cut as a minor because the TS adapter gains real public
API and a deployment needs the full image build.

⚠️ **THE REGRESSION v1.85.0 SHIPPED WITH, REPORTED FROM PRODUCTION WITHIN A
DAY.** An operator's interactive terminal stopped rendering after `started` on
every run — three agents, two context modes — and reported the runs as hung.
They were not hung. v1.85.0 had begun emitting three NEW frames between
`started` and the first turn, then answering and parking exactly as before:

```
v1.84.0  session agent steer started                                              text usage awaiting_input
v1.85.0  session agent steer started  ctx_distill_declined ×2  context_exhausted  text usage awaiting_input
```

The `409` on cancel was the same fact seen from another angle: turn-cancel is
disarmed while a run waits at `awaiting_input`, so it correctly refuses a run
that is parked rather than stuck.

And the frames were **false**. The run had sent nothing:

> context recap declined: context.keep_last_n 6 pins all **1 message(s)**, leaving nothing to distil — `severity: warning`
>
> context not reclaimed: **88%** of the window (17600/20000 tokens) is in use and distillation did not shrink it

One message, because the conversation had not started. 88%, because before the
first turn the footprint is a chars/4 estimate of a request no tokenizer has
seen, measured against the static capability a driver may still revise — Ollama
reads the model's actually-loaded window from `/api/ps` only after a call.

**Three compounding causes, each correct on its own.** #1305 seeded the
footprint so a one-iteration continuation could distil before sending an
oversized prompt; its own commit message names the failure it was introducing —
*"a short history must NOT distil at start"*. #1308 then added the preamble to
that seed, which is right: on a small window the system prompt and tool
catalogue **are** the request. But a preamble is not a short history. It is
fixed and irreducible, so on an agent whose preamble is most of its window it
sits above the threshold on iteration ZERO and on every iteration after, and the
gate opens on a quantity distillation cannot move. #1312 then hung a second tier
and an exhaustion report on that same gate, so one silent decline became three
alarms.

**The fix is one rule: report what you measured.** The gate still OPENS on the
seed and distillation still runs — that protection is the point of seeding it,
and a resumed transcript already over the window is still reduced before the
first call. Only the operator-facing reports — the declines, the exhaustion
report, and the value `Context op=self` carries — wait for a footprint a
provider actually returned. `runStateful` gets the same rule, where a driver
that reports no usage would otherwise claim an empty Σ cannot be reduced.

### Every wire event value is nameable from TypeScript (#1317)

`context_exhausted` reached that terminal as a type **no typed consumer could
name**: it was never added to `@loomcycle/client`'s `EventType`. It was not
alone — **sixteen** wire values were missing from that union, some for a dozen
releases:

`context_recap` · `context_state` · `context_distill_declined` ·
`context_exhausted` · `thinking` · `turn_cancelled` · `provider_fallback` ·
`fallback_suppressed` · `model_downgraded` · `cache_invalidated` ·
`reasoning_invalidated` · `channel_publish` · `channel_delivery` ·
`interruption_pending` · `spawn_child_started` · `spawn_child_result`

The **payloads** were missing too, so even a consumer casting out of the union
had nothing to reach into: eleven fields on `AgentEvent` — the five context
payloads, `fallback`, `channel`, `interruption`, `turn_cancelled`,
`spawn_child`, and the `reasoning` trace on `done`.

Two hand-written per-event parity guards already existed and neither caught it,
because each only guards its own event. So the new guard **does not hold a
list**: it enumerates the runtime's own `EventType` constants out of
`provider.go` and requires each value on the union's member lines. A list in the
test would be a third copy to forget.

### The effective-config report answers from the run (#1318)

`GET /v1/runs/{id}/effective-config` merged the run's `Routing`, `Resources` and
`Tuning` onto the stored definition and computed its `inert` advisory array from
the result — but never passed `Context`. So the array answered from the stored
definition while `fields.context` **in the same response** answered from the
run: one payload, two verdicts about one setting. Seeing a per-run override
introduce or clear a trap is the entire reason that array exists over the
boot-time warning, and it structurally could not.

Two more values that existed and never arrived: `used_tokens` had been declared
on every decline since the type existed and was set by nothing — so a decline
reported the window size and left the reader to guess how full it was, and a
consumer saw `0` and read *"0 tokens in use"* rather than *"not reported"*. And
`EffectiveConfigResponse` declared `fields` and stopped, so `inert` — the half
`fields` structurally cannot express, because a setting can be in force **and**
inert — was unreachable from a typed client.

### Upgrading

- **A genuine `context_exhausted` still fires.** This release removes the FALSE
  pre-turn report, not the feature: a run whose window really is full, measured,
  will still say so. A consumer that renders the event stream needs a case for
  it — `@loomcycle/client` 1.86.0 finally lets you write one.
- **Adapters at 1.86.0** (`@loomcycle/client` on npm; the Python adapter rides
  its own `python-v1.86.0` tag). Not ceremonial: #1317 and #1318 add public
  surface, and adapter surface that ships without its version bump is stranded.
- **Cut as a minor, not a patch.** A patch tag builds only
  `denngubsky/loomcycle-browser`, and this fix has to reach the images a
  deployment actually pulls.
- Nothing to change in configuration. If you deleted `compaction.autocompact_at_pct`
  on v1.84.0's advice, v1.85.0's upgrade note still applies — put it back.

## What's in v1.85.0

*Context distillation now reclaims the window in every mode — or says loudly that it cannot.*

Seven PRs. v1.84.0 made a failed distillation **visible**; this release makes it
**recoverable**, and closes the gap that visibility immediately exposed.

⚠️ **THE DEFECT v1.84.0 SHIPPED WITH, FOUND IN PRODUCTION.** The footprint that
drives the distillation gate counted the CONVERSATION only, while the value it
was compared against — and the value the provider bills — counts the system
prompt and tool catalogue too. On a `chat/local` agent with a 2048-token window
the estimate read **2 tokens (0.1%)** against a real request of **3340 (163%)**:
the run sent 163% of its window with the gate never opening. A small window with
a large preamble is the worst possible ratio, and no unit fixture had that shape
— every one was built large-in-messages, which is exactly where the estimate and
reality agree. Reported by a consumer running a real configuration, not by the
suite.

That fix also corrects `Context op=self`'s gauge, which under-reported by the
whole preamble on every turn after a distillation — so an agent deciding whether
to self-compact was reading the same wrong number it was being judged by.

A DECLINED DISTILLATION NOW HAS A SECOND TIER. The gate branched once — recap
mode took recap, everything else took compaction — so `compaction.autocompact_at_pct`
was inert in recap mode **by construction**, and a recap that declined left
nothing else to try while the run climbed to the provider's limit.

Compaction is now reachable from every mode, and it is the right last resort
precisely because it fails DIFFERENTLY: `empty_summary` is a property of the
recap budget and the recap prompt, and a compaction summary runs on neither. A
second tier that failed for the same reasons would be theatre.

THE WINDOW NOW BEATS A PINNING `keep_last_n`. The declined-split path never
reached the tail cap, so a run whose kept tail alone exceeded the window had no
escape — `keep_last_n` could veto every distillation path. The precedence is now
explicit: `keep_last_n` is a PREFERENCE about how much to keep verbatim, the
window is a HARD LIMIT, and a preference does not override a limit. It fires only
when the tail genuinely does not fit.

STATEFUL MODE IS BOUNDED AT LAST. Its transcript is rebuilt from (Σ, observation)
each step so it cannot accumulate — which is why the gate was never wired there —
but **Σ itself accumulates**, and the whole of it is serialised into the prompt
every step. Nothing measured it and nothing bounded it.

Summarising is the wrong operation on a state object: Σ is validated against
`state_schema`, and prose is not a Σ. The structural equivalent is EVICTION, by a
retention class declared per property — `core` never dropped, `derived`
recomputable, `scratch` dropped first, least-recently-written as the tiebreaker
within a class. **The default is `core`**, so no agent starts losing state on
upgrade; the cost is that an undeclared schema gets no relief and reports
exhaustion instead. Evicted entries are banked before they go, because Σ is the
run's working memory and losing it silently would trade a context problem for a
data-loss one.

⚠️ **AND THE ESCAPE HATCH THAT ALREADY EXISTED WAS NOT ONE.** A model can prune Σ
by emitting `null`. Relying on that is relying on the model to ELECT a behaviour,
and the measurement for exactly that pattern is that it does not: a tool
parameter was passed on 51 of 128 calls, and making it imperative took compliance
to 100% while accuracy collapsed to 0.003. A bound the model must choose is not a
bound.

EXHAUSTION IS NOW LOUD. When nothing reclaims the window and the footprint is at
the point where something should have, the run emits `context_exhausted` carrying
every tier's verdict — because "the mechanism ran and refused, here is the fix it
named" and "nothing ran at all" call for opposite next moves. Banded by ten
points rather than reported once, since a run stuck at 82% and the same run at
95% is news twice.

Declines also carry a SEVERITY now. Most are routine — `reasoning_keep` is the
operator's own instruction, `not_smaller` is a correct refusal — but
`split_declined` is a warning: that path will decline identically every time and
the window keeps filling. If every decline warned, none of them would.

⚠️ **THE TWO `keep_last_n` KEYS ARE NAMED APART EVERYWHERE.** Recap reads
`context.keep_last_n` (default 6); compaction reads `compaction.keep_last_n`
(default 4). A message naming the bare key sends half its readers to edit the
setting that was not the problem, and they then watch the window keep filling and
conclude the fix does not work.

## Upgrade notes

⚠️ **IF YOU FOLLOWED THE v1.84.0 ADVISORY AND DELETED `compaction.autocompact_at_pct`,
PUT IT BACK.** That release warned it "does nothing" outside `append` mode. It was
true then. It is false now: the threshold is the BACKSTOP for every mode, and in
`stateful` it is the only bound Σ has. The advisory is retired in this release,
but configuration already edited on its advice will not repair itself.

**Distillation now fires where it previously could not**, so agents near their
thresholds will distil EARLIER than before. Three changes compound here: the
footprint now includes the system prompt and tool catalogue (so the same
conversation reads as a larger fraction of the window), compaction is reachable
from every mode, and a run that answers in a single iteration can distil at all.
This is the intended behaviour, and it is a visible change for every `recap` and
`stateful` agent.

⚠️ **`chat/local` IS NO LONGER LOCAL.** It moves from a pinned `ollama-local` model
to `tier: high`, which is a cloud tier — every prompt and tool result now leaves
the machine. Its system prompt no longer claims otherwise. It also moves to
`mode: stateful`, which means **the `/run` terminal cannot steer or park it**: the
stateful loop has no steering and no `end_turn` parking. Pin the previous
behaviour in your own overlay if you were relying on either.

**A `stateful` agent wanting Σ eviction must declare retention classes** on its
`state_schema` properties (`x-retention: scratch | derived | core`). Without them
every key defaults to `core`, nothing is evictable, and the run reports exhaustion
rather than shrinking.

Adapters: `@loomcycle/client` **1.85.0**, `loomcycle` (PyPI) **1.85.0** — carrying
a corrected `reasoning: "keep"` doc. The old text promised a decline
unconditionally; it is only reported once the threshold is reached, and that
sentence is what led a consumer to expect a frame that was never owed.

## What's in v1.84.0

*Context distillation could fail in five different ways and say nothing. It cannot any more.*

Twelve PRs. One line dominates — a whole subsystem made observable before it was
changed — plus three grpc vulnerabilities closed on the serving path.

A LIVE CHAT CLIMBED TO THE TOP OF ITS WINDOW WHILE AUTO-DISTILLATION NEVER FIRED
ONCE, and every mechanism that could have reported why was silent. The threshold
was crossed at 72%; the next call was at 99%. Zero recap markers, zero errors.

The leading cause, on the evidence: the recap summariser's token budget is
derived from `recap_max_chars` (`/4 + 64`), so the default allows about **192
tokens** — and `summarizeWith` accumulates only text, while Ollama routes a
thinking model's reasoning to a separate channel. That session logged **1293**
thinking events. A thinking model given 192 tokens spends them thinking and
returns nothing, which took a branch that emitted no event, no error and no
marker.

⚠️ THE SILENCE WAS THE DEFECT, NOT THE DECLINE. Telling "the threshold was never
crossed" from "it was crossed and the summariser returned empty" required reading
the summariser's source and counting event types in a raw transcript. That is not
a diagnosis an operator can make, which is why visibility came first and the
behavioural fixes second.

**`context_distill_declined`** is a new event carrying one of five reasons,
because they call for five different actions: `split_declined` (carrying the
message count and `keep_last_n` — those two numbers ARE the diagnosis),
`empty_summary`, `summarize_failed`, `not_smaller`, `reasoning_keep`. It is
emitted at most once per (mode, reason) per run — a condition that is a property
of the configuration must not repeat until it buries itself — and surfaces on
`Context op=self` beside the footprint, because an agent told it is at 99% will
otherwise call `op=compact` into the same decline and learn nothing.

⚠️ A DISTILLATION THAT MADE THINGS WORSE WAS BEING APPLIED. In the observed
session an operator's manual compact went `14230 -> 14334` tokens and was applied
anyway: both numbers were already measured at every site and never compared. All
three sites now refuse when the result is not smaller — `after >= before` exactly,
not a margin, because a 10% rule would have refused the first GOOD compaction of
that same session.

The harvests moved with it. `Recall.Harvest` and the memory bank ran ABOVE the
measurement, so a declined distillation had already handed away a span it then
kept — and with the gate re-firing every iteration, wrote the same content
repeatedly.

A RUN THAT ANSWERS IN ONE ITERATION CAN NOW DISTIL. The footprint was zero until
the first provider call RETURNED, while the gate runs at the top of an iteration —
so a continuation answering at `end_turn`, which is a single iteration, could
never distil however full its replayed prompt. The observed session's last run
sent 30100 tokens of a 32768 window this way and reclaimed nothing. It is now
seeded from the messages the first request will actually carry.

A MANUAL COMPACT OPERATES ON THE SESSION, which is what the loop holds. It used
to fetch the session transcript and then filter it to one `run_id`, so a
continuation chat presented 2-3 messages and answered "nothing to compact" at 92%
of the window — and when the split DID succeed, the kept-tail count was computed
from that run-scoped slice and applied against the session-scoped history,
producing a summary of a span the model never held.

"Nothing to compact" is now three answers with their numbers: `noop` (too short),
`noop_keep_spans_all` (`keep_last_n` pins everything — lower it), and
`noop_not_smaller`. The existing `noop` value is unchanged, so a consumer matching
it keeps working.

DEAD CONFIGURATION SAYS SO, at boot and in the effective-config report. The agent
in that session declared `compaction.autocompact_at_pct: 70` and
`compaction.memory_flush: true`, and both were inert: **only `mode: append`
consults the compaction path at all**. Every other mode distils by its own route —
`recap` takes the other branch, `stateful` returns before the gate exists, and
`auto` resolves to one of those two, so it is inert either way. Reported with the
live knob to set instead.

**`context` reaches the typed client.** The server has accepted a per-run
`context` block since it shipped and no typed consumer could send one — and there
was no workaround, because the request body is assembled from an allow-list, so
the key was dropped even by a caller casting past the type checker. That is what
made A/B-ing distillation modes impossible, which is how the rest of this work
gets verified.

THREE GRPC VULNERABILITIES CLOSED, all reaching loomcycle's own serving path
rather than merely present in the module graph: a server panic via a missing
`:authority`/`Host` header, heap exhaustion via HTTP/2 DATA frame fragmentation,
and the xDS RBAC + HTTP/2 transport issue. Two are unauthenticated remote DoS.
grpc 1.80.0 -> 1.83.2; `govulncheck` goes from three affecting findings to zero.

ON THE MEMORY SIDE, a retrieval grant and an honest negative result. Recall can
now search the trace index with the caller's own query and return those turns as a
separate block — worth **+31pp** on a local answerer where fact-anchored
attachment reaches 0.5473 and direct search reaches 0.7877.

⚠️ BUT THE GATE WAS NOT MET, and the notes record that rather than the headline.
On a second conversation the effect is +12.3pp against the +29.6pp of the first, so
the original figure was a property of that conversation rather than of the
architecture. The cause is a gap in the grant: it enriched `op=recall` only, and
the model elects the op — on the second conversation it chose `search` 49 times of
85, and the inert half moved by **exactly zero**, which rules out the alternative
explanation that `search` simply marked harder questions. The grant now covers
`search` too.

**Upgrade notes.** No configuration changes are required.

`POST /v1/runs/{run_id}/compact` now operates on the SESSION rather than the run.
Byte-identical for a single-run session, which is most autonomous runs; a
continuation chat will now find something to compact where it previously reported
a noop.

A compaction or recap whose result is not smaller is now REFUSED rather than
applied. If you have a fixture or a monitor asserting that a tiny conversation
compacts successfully, it will now report `noop_not_smaller` — which is the
correct answer: the compaction preamble alone is ~190 characters, so a very short
history genuinely grows.

Adapters: `@loomcycle/client` **1.84.0**, `loomcycle` (PyPI) **1.84.0**. The TS
bump carries the new `context` block, which loomboard's distillation panel needs.

## What's in v1.83.0

*The run controls that shipped write-only can now be read back — and a granted tool that could never work says so at run start.*

Eighteen PRs. Two lines finishing at once — the per-run override surface gets
its reads, its last two transports and its interactive fields, and the
capability-gate work gets its defaults, its warning and its correction — plus
three phases of recall provenance, which turned into a dating fix once the
measurements came back.

A RUN'S CONFIGURATION CAN BE READ, NOT JUST WRITTEN. The previous release let a
run carry its own model, budgets and tuning; every bit of it was write-only. A
caller could set an override and had no way to read it back, and a retune that
moved no model left no trace at all. Three reads close that:

  - `GET /v1/runs/{run_id}/config` reports what the RUN overrides. There was
    nothing to extend — there is no `GET /v1/runs/{run_id}` at all. Absent means
    "not overridden", which at this layer beats a resolved value, and a run that
    was never retuned is a 200 with an empty config: "this run overrides
    nothing" is an answer, not a 404.
  - `GET /v1/runs/{run_id}/effective-config` reports what it will ACTUALLY use,
    field by field, with the layer that decided each — `run` / `definition` /
    `user_tier` / `operator` / `resolved` / `default`. The SOURCE is the point.
    `max_iterations: 16` cannot distinguish a deliberate setting from a default
    nobody chose, and those call for opposite actions.
  - `POST /v1/runs/{run_id}/retune` now answers with the merged record it
    already computed and threw away. A caller cannot recompute it: naming a
    `model` clears the `provider` so a previous choice cannot linger and
    contradict the new pin, and naming a `tier` clears the `model`. A panel
    echoing its own request back would display something the run does not hold.

AN OVERRIDE EVENT THAT COULD NEVER FIRE, NOW TWO THAT DO. `OverrideInfo.Fields`
documented itself as carrying the keys a request actually set — "so a reader can
see a budget or tuning change that moved no model at all" — and was hardcoded to
`{"model"}`. Worse than a wrong value: the loop returns early unless ROUTING
changed, so a retune of only `max_tokens` emitted nothing whatsoever. A consumer
had built a branch against a documented capability that could not run.

Making the loop report more would have been the wrong fix. The loop cannot see a
request; it sees a re-resolved routing outcome. So there are now two events
answering different questions — the server says WHAT WAS ASKED FOR at retune
time, the loop says WHAT THE RUN IS NOW USING once it knows, with the from/to
pair the server could not yet have. A `from`/`to` pair present is the
discriminator, and the doc comment that misled the consumer now says so.

A RUNNING AGENT CAN BE PROMOTED TO INTERACTIVE. An override could change how a
run was routed and budgeted but not whether it could be TALKED TO, so a long
autonomous run that needed a correction could not be given one — it had to be
cancelled and restarted. `interactive` and `interruption` are now override
fields, evaluated at the turn boundary rather than only at start, so a run
already in flight parks for input from the next boundary on.

RETUNE REACHES ALL FIVE SURFACES. gRPC gained `RetuneRun`, and a steer over gRPC
can carry overrides with its text. MCP gained `retune_run` — it has no steer
tool at all and never got one. The TS adapter gained `getRunConfig`,
`getEffectiveConfig` and a `retuneRun` whose return type is no longer stale,
plus the source union as a type so a consumer switching on it cannot misspell a
case. And `metadata`, the per-run layered-`context` block and cost-attribution
lineage reached the gRPC wire, all three reachable from HTTP since they shipped
and all three silently absent rather than refused.

A TOOL THE AGENT CANNOT USE NOW SAYS SO, AT RUN START. `tools` and the
capability gates are two independent grants, and the second silently voids the
first: an agent granted `AgentDef` with no `agent_def_scopes` was refused
mid-task, and the operator saw "the agent didn't do it" rather than "the agent
could not". A server-generated `capability_inert` event now rides the run's own
event channel at start — the path the budget warnings take, for the same reason
— reaching a live SSE or gRPC consumer and persisted. Once per run, not per
call: the condition belongs to the definition, not to any invocation. The
payload carries the FIX as well as the fact, because a reader told "AgentDef is
inert" still has to work out which yaml key governs it.

ONLY GENUINELY INERT GRANTS ARE REPORTED, which is the part that needed thought
rather than typing. `sql_scopes` and `evaluation_scopes` now join
`memory_scopes` and `history_scope` in resolving to what the caller already owns
— `sql_scopes` to `["user"]` and `evaluation_scopes` to `["submit_self"]` — so
an unset one is no longer inert, and an event for it would fire on nearly every
run of most agents. That is how a signal becomes noise and then gets ignored,
taking the real warnings with it. What remains reportable: the def-authoring
gates, the two A2A gates, and Channel with neither side of its ACL.

⚠️ THE SCOPE DEFAULTS MADE EXISTING WARNINGS FALSE. `agentGateWarnings` has
warned about empty capability gates since F21, and two of its lines stopped
being true the moment those gates gained defaults: "every Memory op will
default-deny" and "every Evaluation op will default-deny". A warning that states
the wrong consequence sends an operator to fix what is not broken and teaches
them the channel is noise — which costs the warnings that ARE right. The text
now distinguishes what RESOLVES from what still grants nothing. The core-blocks
and consolidation advisories needed more than a reword: their default is the
WRONG SCOPE for them, so they name the scope to set instead.

Those advisories also gained REACH. They were logged once at boot, where an
operator debugging a week later never sees them. `doctor` inspected no agents at
all — so the most common "why is my agent not doing that" went unmentioned by
the command people run precisely to be told what is wrong — and `validate` did
not print them either. Both now do.

TWO SILENT-FAILURE PATHS FIXED. A fan-out child with no prompt reached the model
as a null user turn: it got the system prompt, answered whatever that implied,
and COMPLETED, so the caller read a green envelope and the emptiness was visible
only in the thinking trace. The same caller mistake was already a 422 on the
single-spawn path. And the Python adapter could not send `user_credentials` at
all — absent from all three enumerations — so a caller whose MCP headers carry
`${run.credentials.<name>}` got an empty map and a downstream 401, with nothing
client-side saying the credential had been dropped.

RECALL CAN ATTACH THE TURN A FACT CAME FROM — ON THE OPERATOR'S SAY-SO. A
distilled fact is tenseless; the turn it came from opens with a timestamp. On
LoCoMo conv-26 reaching the turns moved accuracy 0.396 → 0.788 (McNemar +61/-3,
p=4.7e-15), almost all of it in temporal questions (0.176 → 0.797).

⚠️ BUT THE SECOND RETRIEVAL ONLY HAPPENS IF THE ANSWERER ELECTS IT, AND MOST DO
NOT. Same tool, same prompt, same store: deepseek issued 214 trace retrievals
across 150 questions; qwen3.6 issued 7; an agentic-tuned ornith-1.5:35b issued
17, for +2.4pp (p=0.508). And it cannot be fixed by insisting — replacing the
advisory prompt with an imperative procedure took qwen3.6 to 100% compliance and
accuracy to 0.0034 of 1.0, emitting tool-call JSON into the answer field. On a
small model a mandatory protocol competes with the task for attention.

So the retrieval happens in the RUNTIME, where nothing has to elect it. A
`recall_include_turns` grant on the agent def — operator-resolved, never
model-supplied, the same trust posture as `memory_scopes` — makes every recall
that agent issues carry provenance. The `include_turns` tool parameter stays;
either alone is enough.

Turns are resolved through `source_session_id` rather than the trace index (the
index is forward-only and empty unless an operator backfilled it;
`source_session_id` is populated on 87% of facts in the reference store and needs
no flag), and nothing is re-ranked — the fact list is chosen exactly as today and
each chosen fact is EXPANDED, so turns never compete for a slot. Default off,
bounded per turn and per response, with `turns_attached` / `turns_dropped_for_budget`
so a caller can tell "nothing resolvable" from "budget spent".

⚠️ AND IT ENFORCES `history_scope`, NOT ONLY THE GRANT. Recall-attached turns are
history reach wearing a memory hat: without the second gate an agent denied
History could read the same words by asking for facts. `recall_include_turns`
decides whether turns are OFFERED; `history_scope` decides whether they may be
READ; both must say yes. The field is `notOverridable` per-run for the same
reason — a run that could set it would obtain transcript through the memory path,
which is exactly what an operator declines when they narrow `history_scope`.

RECALL NOW RETURNS WHEN A FACT WAS OBSERVED. `Memory op=recall` returned no
date on a hit, so "when did X happen" had nothing to answer from — the distilled
sentence is tenseless. The date was believed to arrive with the source span, and
measured on a live store that is true of a third of them: of 150 hits, 111
carried a span and only 36 of those (32%) carried a date. The cause is in span
derivation — a turn reads `[1:56 pm on 8 May, 2023] Caroline: …` and spans are
cut on sentence punctuation as well as line breaks, so the FIRST sentence keeps
the stamp and every later one loses it.

Meanwhile `observed_at` is populated on 87% of facts and was being discarded at
this boundary: the date the question needs was already on the row, one column
away from the projection that read it. It is returned as RFC3339 rather than the
stored unix nanos, because this field is read by a model answering "when" and a
nanosecond integer is not an answer it can give. A row carrying ONLY a date is
now kept — the old rule required a span or a pointer, so a fact whose span was
never derived but whose observation time WAS recorded was dropped entirely, the
exact row this change exists to surface.

⚠️ WHAT IT DELIBERATELY DOES NOT DO is prepend the turn's stamp to the span.
That was the first attempt and it breaks a load-bearing invariant: the span must
be a verbatim substring of the transcript, which is what makes fabrication
impossible rather than merely unlikely, and what a verification judge checks
against. Synthesising a span that never appeared in the source would trade a
retrieval problem for an evidence one.

AND AN `observed_at` LATER THAN ITS OWN TURN IS THE INGESTION DATE, NOT THE
EVENT. Measured on the same store, 5 of 263 facts carried a 2026 `observed_at`
on a 2023 corpus — two of them while their own span still read
`[7:55 pm on 9 June, 2023]`. Not a parse failure: the stamping is gap-filling by
design and a model-supplied value always wins, so when the extractor emits the
date it is RUNNING on, nothing corrects it. `observed_at` is when it was SAID,
and a turn cannot have been said after its own timestamp, so that one direction
is now corrected to the span. The model-wins rule is otherwise intact — a value
EARLIER than the turn is exactly the relative date ("last month") the parser
cannot resolve, which is why the rule exists.

⚠️ 2% WOULD HAVE BEEN COSMETIC WHILE THE FIELD WAS WRITE-ONLY. Recall returns
`observed_at` as of this release, so a wrong date now reaches the reader AS AN
ANSWER — and a confidently wrong date is worse than a missing one.

THE TS AGENT-DEF MIRROR STOPS DRIFTING. The substrate carries an agent def as an
overlay whose shape the in-process tool owns; gRPC declares it as opaque bytes
and the Python adapter takes a plain mapping, so both already passed any key
through. TypeScript is the exception — `AgentDefOverlay` is a hand-written
mirror with no index signature, so excess-property checking REFUSES any key it
does not declare, and a field absent there is not a documentation gap but a
field a typed TS caller cannot set at all. `recall_include_turns` is added.

⚠️ THE MIRROR IS ALREADY 30 FIELDS BEHIND the Go overlay's 48 —
`memory_consolidation`, `sampling`, `compaction`, `history_scope`, `volumes` and
the entire `*_def_scopes` family are unreachable from a typed TS caller. That is
pre-existing and is NOT fixed here: which of them the adapter should expose is a
decision about its surface, not about this feature. What IS fixed is that it
stops growing — the 29 remaining gaps are frozen in a register that may only
SHRINK, so a 31st unmirrored field fails, and an entry that later gets mirrored
must be removed or it fails the other way. The drift reached 30 precisely
because nothing objected as it grew.

**Upgrade notes.** `sql_scopes` and `evaluation_scopes` change posture on
upgrade, the same way `memory_scopes` and `history_scope` did in v1.82.0: an
agent that holds `Memory`/`Evaluation` with the scope list unset previously had
every such call refused and now resolves to the caller's own data (`sql_scopes`
only when the run carries a user id). Set the list explicitly to widen or
narrow, or `["-*"]` to grant none — the same deny-all sentinel `skills` uses.
Existing agent rows stay byte-stable: the defaults resolve at policy time, not
by materialising into the definition, so `content_sha256` does not fork.

`Memory op=recall` hits now carry an `observed_at` they did not before — additive,
but a consumer that pins the response shape should expect the field. Turn
attachment stays off unless an operator grants `recall_include_turns`.

Adapters: `@loomcycle/client` **1.83.0**, `loomcycle` (PyPI) **1.83.0**.

## What's in v1.82.0

*A run can now choose how it runs — and a tool an agent was granted but could never use now works.*

Eighteen PRs. Two features worth the version, plus the memory-graph work
continuing underneath them.

A RUN CHOOSES ITS OWN MODEL, BUDGET AND TUNING — WITHIN WHAT THE DEFINITION
ALLOWS. Everything about how an agent ran used to be fixed at the definition: to
try one question on a bigger model, or give one job a longer leash, you forked
the agent. A run now carries its own `model` / `provider` / `tier` / `effort`,
its own `max_tokens` / `max_iterations` / `unbounded_iterations`, and the tuning
row (`retry_attempts`, the memory budgets, `inject_tool_guide`).

Three rules make it safe rather than a hole in the definition:

  - naming a model PINS it, clearing the tier — a cascade that may route
    elsewhere does not answer "use this model";
  - naming a provider NARROWS and keeps the tier, because collapsing it would
    trade away fallback the caller never gave up;
  - an unknown `effort` is REFUSED, not dropped. Ignored and applied look
    identical from outside, and that is the whole problem with silent defaults.

The budget knobs are raisable except `max_concurrent_children`, which may only
be LOWERED — it is the only bound on sub-agent fan-out that exists, since a
child takes no admission slot and is not budget-checked at spawn. Raising it is
refused rather than clamped.

The overrides survive the places a per-run value usually dies: a provider
fallback re-resolves WITH them rather than from the definition, a paused run
comes back carrying them, a parked run can be RETUNED mid-conversation, and a
child inherits them only when it is the same definition (re-validated at spawn,
and dropped rather than refused when it does not fit, because inheritance is
offered rather than requested). They reach gRPC, the MCP spawn tools, and both
adapters.

Untrusted triggers cannot route: webhook, A2A and scheduled runs build their
input from the definition, and an AST test over the input literals keeps it that
way.

A GRANTED TOOL THAT COULD NOT WORK NOW WORKS. An agent could hold `Memory` in
its tools and be unable to use it, because `memory_scopes` was default-deny when
empty and nothing said so — the refusal arrived when the model called the tool,
mid-task. An unset `memory_scopes` now resolves to what the caller already OWNS:
`user`, plus `tenant` for a non-isolated member. An unset `history_scope`
resolves to `user`, since a user always has access to their own chats. A
declared list stays authoritative and is never widened.

⚠️ UPGRADE: an agent holding `Memory` or `History` with NO scope list gains
user-scope access it did not have before. That is the intent — the grant was
meant to work — but it is a posture change, so it is stated here rather than
left to be discovered. To grant nothing, say so: `memory_scopes: ["-*"]`, the
same spelling `skills: [-*]` already uses. An empty list cannot carry that
meaning, because the stored agent shape omits empty lists and one reads back
indistinguishable from never having been set.

The default is applied at policy-resolution time, never written into the
definition: these fields are content-identifying, so materialising a default
would change every affected agent's `content_sha256` and fork it on upgrade.

A MISSING PER-RUN CREDENTIAL NOW REFUSES THE CALL instead of dropping the header
and sending it anyway. An unresolved `${run.user_bearer}` / `${run.credentials.
<name>}` used to go out as an anonymous request — and a peer that does not
authenticate served it. It is a business failure, not a retryable one, and the
distinction matters: the retry path was previously spending 30 seconds of
backoff on a request that could never succeed.

GRAPH RECALL SEEDS SEMANTICALLY AND WALKS FAR ENOUGH. Traversal now starts from
a semantic match rather than a title hit, bounds its hops, batches its id
queries, and honours `limit`. Provisioning a scope's schema moved to once per
process rather than once per call.

Also: the MCP batch spawn tool advertises the per-run knobs it already accepted
(it was accepting them and documenting none of them, which for a model-facing
tool is the same as not having them), the Python adapter's override inputs reach
the generated stubs and both request builders, and the transport-parity guard
now checks each SURFACE rather than each file — it had been satisfied by one
spawn tool on behalf of a sibling that carried nothing.

## What's in v1.81.0

*A fact naming two things was stored joined to neither, and the benchmark built to catch that also answered whether graph traversal is worth building.*

Five PRs, two themes: the memory graph got its edges back, and a resumed run
comes back as the run that paused.

THE EXTRACTOR'S OUTPUT SCHEMA OMITTED A FIELD, AND THE GRAPH HAD NO CHAINS. A
fact naming two entities — "Dave works at the shop" — was stored with one
`about` edge, to its own subject, and nothing joining it to the other. The store
filled with spokes and no chains, so a question needing A→B and B→C had no path
to walk.

Nothing failed while this was true. The pass succeeds, the facts are written,
recall returns them. Only the structure was missing, which is why it survived.

The cause was the JSON template at the end of the extractor prompt — the shape
the model copies, and deliberately the last thing it reads before the
transcript. `also_about` was named in the prose above it and absent from the
template. Measured paired, pinned, on a corpus where every fact names two
entities:

  pre-fix prompt                      0/72
  pre-fix template + the new prose    0/72
  this prompt                        72/72    two-sided exact p = 0.00049

The middle arm is the one that settles it: rewording the prose changes nothing.
The template is the whole effect. End to end on a full corpus, facts carrying
both entities went 36% -> 93%, and consolidation recall 352/376 -> 376/376.

THE MINIMAL FORM LEADS THE TEMPLATE FOR A REASON. Adding the field with the full
shape first cost the invented-entity guarantee: on a transcript naming no
person, service or organisation, the extractor invented one. The subject rate
rose 0.60 -> 0.80 and the eval gate refused the baseline. Leading with the
no-entity shape restores it — 0 violations at three seeds — and a test now holds
the invariant: every field the prose names in backticks must appear in the
template.

AND THE BENCHMARK THAT FOUND IT ANSWERED ITS OWN QUESTION. RFC DB asked whether
following typed relations beats retrieval at all, or whether the idea is
unmeasurable. On 120 synthesis questions over 63 sessions, every arm capped at
the same content budget:

  oracle              99.6%   the ceiling; the answerer is not the bottleneck
  traversal           58.6%   +130/-4 vs single-hop, two-sided exact p < 1e-6
  single-hop           5.8%   today's retrieval
  shuffled-relation    0.8%   the same walk over randomly rewired edges
  no-memory            0.0%   nothing leaks from training

The shuffled arm is what makes the win mean something. Same code, same depth,
same budget, the same 28 facts in the prompt; only the edge targets permuted. It
does not merely fail to beat single-hop — it loses to it. The gain is the
relations, not the volume.

Bounded honestly: that is a corpus BUILT to contain relational structure, and it
measures the mechanism with retrieval done by the harness. Whether real corpora
carry chains, and whether an agent handed the tool would invoke it, are both
still open.

A RESUMED RUN COMES BACK AS THE RUN THAT PAUSED. Every call-time value a run
started with — temperature, compaction policy, layered-context mode, context
window, run timeout — was rebuilt from the AGENT DEFINITION on resume, so a run
that started with a per-run override came back without it and never said so. It
now restores from itself, or refuses and says why.

A CHAT THAT WAS WAITING COMES BACK WAITING, NOT FAILED. A run parked awaiting
its operator ends on an assistant turn, and re-entering the loop would hand the
provider a trailing assistant message. Resume refused it and marked the run
FAILED — an operator's idle chat, destroyed by a restart. It now comes back
parked, which is what it was.

- the chat agents inject what is true of the PERSON, not only of the deployment

@loomcycle/client 1.81.0 — no wire change; version parity for the release.
loomcycle (PyPI) 1.81.0 — no wire change; version parity for the release.

## What's in v1.80.0

*A failed tool call now says what kind of failure it was.*

Fifteen PRs. One theme dominates: a tool failure stopped being a boolean and a
sentence, and became something an agent can act on.

An agent receiving a failed tool call has exactly one decision to make —
resend, change the call, or stop — and every failure in this runtime arrived as
one boolean and one English string. "No rows matched" and "the database was
unreachable" are both short sentences. Guessing wrong means retrying a call
that can never succeed, or abandoning one that would have worked.

The runtime ALREADY knew the answer and discarded it. The HTTP surface maps
~135 typed codes and emits Retry-After; gRPC maps the same conditions onto
ResourceExhausted and PermissionDenied; the provider layer classifies failures
for fallback. All of it was thrown away at the boundary an agent stands on.

  errorCategory   transient | validation | business | permission
  isRetryable     will resending THIS call fail?
  description     what went wrong AND what to do next
  retryAfterSeconds  optional, and only where waiting actually helps

The distinction that earns its keep is validation vs business: validation is
recoverable by the agent alone, business is not. An agent that treats a policy
refusal as a formatting mistake will loop until something stops it.

ONE CLASSIFICATION, FOUR RENDERINGS. Derived from the typed errors the runtime
already had, so the surfaces cannot drift apart:

  MCP      structuredContent on the tool result
  gRPC     google.rpc.ErrorInfo + RetryInfo on the status, AND error_info on
           streamed error frames
  HTTP     error_info on the SSE error event
  in-band  a [category · retryable · retry in Ns] prefix in the tool_result
           text, on EVERY provider

That last one is not redundancy. is_error reaches the model on ANTHROPIC ONLY —
the OpenAI dialect, Gemini and Ollama have no slot for it in their wire
formats — so a classification living only in a field would have been an
Anthropic-only feature, silently absent everywhere else.

A TOKEN BUDGET IS NOT BACKPRESSURE. Both render 429, and both arrived as
codes.ResourceExhausted on gRPC, indistinguishable except by string-matching
the message. But a concurrency cap clears in seconds and a budget clears when
an operator raises it or the month rolls over. Telling an agent to retry the
second on the schedule of the first burns its attempts against a wall.

A CROSS-TENANT MISS IS NEVER A PERMISSION ERROR. It stays an opaque not-found,
because a permission category on such a read confirms the row exists and turns
the error into an existence oracle. This is guarded by a test, since it is
exactly the invariant a later "improve the error message" change reintroduces.

A successful query that matched nothing now looks nothing like a failed one.
Grep and Glob returned the bare fragment "no matches" — a fragment an agent can
read either way — and both now state that the search completed. 27
collection-returning ops report resultCount, and ZERO IS EMITTED, never
omitted: omitting it is precisely what makes a successful empty query
indistinguishable from a failed one.

A tool that does not count emits no key at all. "Did not count" and "counted
zero" are different statements.

A spawn_run whose run FAILED now returns isError: true. It previously came back
success-shaped — isError unset, the failure buried in a string inside the JSON
payload — so a caller had to parse the payload to learn the call had failed at
all. A cancelled run is still not an error: the caller asked for it.

Event.error_info is a new proto field. Taken as a protocol change rather than
bolted alongside, since there are no gRPC consumers yet.

- a chat is searchable by what was SAID, not only by what it was called —
  user turns first, then assistant turns, with a bounded backfill
- one subject reads across every scope the caller can reach
- the empty-dossier sweep could silently un-adopt a subject; fixed

- release notes stopped reaching the release page. goreleaser's mode: replace
  discards the body GitHub populates from the annotated tag, and the changelog
  is disabled, so v1.79.0 shipped a 2-byte body. The tag annotation is now
  rendered explicitly.

@loomcycle/client 1.80.0 — ErrorInfo on RunEvent.
loomcycle (PyPI) 1.80.0 — ErrorInfo on AgentEvent, absent-vs-zero preserved
through HasField.

## What's in v1.79.0

*Every tool says what NOT to use it for, and a memory subject is proposed rather than minted.*

Nine PRs. Two themes: the tool surface stopped being a list of names, and the
memory layer learned to hand an unknown subject to an operator instead of
guessing.

A tool description is the only text that reaches a model on EVERY request. The
material was mostly already written, in Go doc comments the model never sees:
`Read` shipped as "Read a UTF-8 text file from disk." while its comment
documented the 256 KiB cap, the volume sandbox and the refusal behaviour.

Every tool now states its purpose, its inputs and their constraints, what it
refuses or caps, and — the element none of them had — WHICH NEIGHBOUR OWNS THE
JOB IT DOES NOT.

  Read        not for finding files; Glob by name, Grep by content
  Write       not for partial edits; it drops what you did not resend
  Bash        not for work Read/Write/Edit/Glob/Grep already cover
  WebFetch    not for discovery, and not for a non-GET method
  subscribe_channel   at-most-once; peek+ack is the at-least-once pair
  register_agent      a TTL scratch agent; agentdef is the durable one
  get_snapshot        read it; export_snapshot moves it
  a2aservercarddef    advertises US; a2aagentdef registers a peer WE call
  path rm             removes the NAME, not the thing
  volumedef delete    keeps the files; purge deletes the tree

The MCP surface matters most here: it is read by EXTERNAL agents with no
loomcycle system prompt doing the disambiguating, so the description is the
entire briefing. Seven of those descriptions also claimed "Operator-admin-only"
when the authorization map says otherwise — stale prose that told tenant
operators they were locked out of tools they can use.

Both rubrics are enforced by tests rather than convention, and the MCP one
ranges over the tool surface itself, so a new tool is held to it the day it is
added.

INTROSPECTION LEAKED THE CATALOGUE. The Context tool is deliberately pointed at
the runtime-wide tool set, so every introspection op is a disclosure decision.
Its filter was a plain map lookup that failed in BOTH directions: an unstamped
or empty allowlist disclosed EVERYTHING, while the "*" that every operator
surface passes matched nothing and disclosed NOTHING. A grant of
`mcp__slack__*` disclosed none of that server's tools either. All three ops now
use the same matcher the exposure layer and the substrate tool-ceiling checks
already agree on, and an absent list discloses nothing.

ONE AGENT COULD CHOOSE WHAT ANOTHER READS. A team node's threaded input is the
previous state's OUTPUT, and it was being handed to the placeholder expander.
An agent ending a turn with `{{document:/secrets/…}}` had that document inlined
into the NEXT agent's prompt, under the runtime's authority, with reach neither
needs the Document tool to use. Threaded output now travels in a data slot —
substituted after expansion, never scanned — the mechanism already built for a
Starter's source message.

- a fact records every subject it names, not just the first
- an unknown subject is PROPOSED for an operator to adopt, never minted from a
  transcript; adoption shares it from that point on and never backfills
- retiring a fact now closes the k/v plane and the fact graph in one op
- a reclaimed scope no longer leaves its chunk bodies behind
- empty subject dossiers are swept, opt-in and off by default

- the TS adapter's TESTS are typechecked. vitest strips types without checking
  them, so no adapter type was guarded by anything — deleting a field from
  types.ts broke no build and failed no test. Turning the check on surfaced two
  live bugs where a mocked error body was never the JSON the test was named for.
- generated stubs are gated against the .proto, ignoring generator version
  stamps so the gate answers "do the stubs match the schema" rather than "was
  the same binary used". It caught a `make python-proto` target that had never
  worked on Linux.
- goreleaser is `mode: replace`, so a release re-run no longer dies on
  already_exists before reaching the Homebrew step.

@loomcycle/client 1.79.0 — gains `propose_subject` on DocumentToolInput.

loomcycle (PyPI) 1.79.0 — first publish since 1.67.0, and it carries the gRPC
`walk_id` filter plus `parent_context` lineage: the team-walk view that reached
HTTP, MCP and TypeScript earlier is finally reachable from Python. Released on
its own python-v1.79.0 tag.

## What's in v1.78.0

*Prompt bindings that reach, behind a guard that holds.*

RFC CY Track A completes the prompt-expansion families, and the walk filter
reaches every transport. Nine PRs.

An operator writing an agent's prompt could bind a document or the tool
inventory into it, but not a memory key, not a search, and not a page. A second
colon now turns both families into an argument form:

    {{memory:key:launch}}                 one stored entry, the run's own scope
    {{memory:search:deploy checklist}}    the top matches
    {{tool:WebFetch:https://docs/x}}      that page, fetched once at run start
    {{tool:WebSearch:release notes}}      that search

All four fold into the SINGLE combined regex the families already share, so a
placeholder sitting inside injected content — a fetched page, an agent-written
memory — is still text rather than an instruction the runtime executes.

GATED TWICE, and the two gates fail in different directions.

  Authorship. Only a definition an OPERATOR wrote may use the argument forms.
  The guard covers ONLY what was added: every family that worked before keeps
  working for every definition, legacy rows included. Gating those would strip
  expansion from working defs the moment the migration ran — a guard that
  breaks working defs on upgrade is an outage, not a guard.

  Trust rule 5d — a placeholder guard is not a NETWORK guard. The argument
  admits ${…}, and variables bind from untrusted sources. The operator authors
  the template; an attacker must not pick the target, and a charset check
  cannot help because a URL charset spells any host. So a resolved value that
  becomes a network target must be on the operator's STATIC
  http_host_allowlist. Parameterised fetches work; choosing an unlisted host
  does not, and the refusal names the host.

Prompt assembly can now block and fail, so the network work is bounded by one
5s budget for the WHOLE assembly and fails soft: a refused, unreachable, slow
or empty page renders nothing and the run proceeds.

Who wrote a definition is now recorded on both def planes — agent_defs and
teamdefs — stamped from the runtime's own view of the caller, never from
anything the caller supplies, and excluded from content_sha256 so a fork across
deployments still verifies.

IN A TEAM NODE'S PROMPT THE AUTHOR IS THE TEAM, and the two segments have
different authors: `system` is always the team's text, `input` is the team's
template when the node declares one and the PREVIOUS STATE'S OUTPUT when it
does not. Threaded output is never treated as authored — otherwise an agent
could emit {{tool:WebFetch:…}} and have the next node's assembly fetch it,
which is 5d arriving through the back door.

`?walk_id=` landed on the HTTP run-state stream in 1.77.0 but reached no typed
client. A walk's own run id IS its walk id, so a caller that started a team
detached filters by the handle it already holds:

  TS     streamUserRunStates(userId, { walkId })  + filter_walk_id on the open
         frame, + walk_id / wave_id / wave_index on ParentContext
  gRPC   StreamUserRunStatesRequest.walk_id, and RunStateEvent.parent_context —
         a new message, carrying lineage gRPC has never had
  Python stream_user_run_states(..., walk_id=) and a parent_context key on
         every event dict

wave_id groups a fan-out and wave_index is the position inside it, so a live
view can place an agent WITHIN a walk rather than merely inside it.

- a fact references every subject it names, not just the first
- the extractor is windowed by default — 8 turns, guarded on short sources
- a run knows its chat, so a queued fact can be followed home
- the evaluation gate stops being a coin flip: sampling is pinned, so the
  suite measures the prompt instead of an unpinned sampler

- operator_authored survives a snapshot. Capture/restore silently DEMOTED
  every operator-authored definition, so an operator restoring onto a new
  deployment found their own defs no longer resolving bindings that worked
  before the capture.

@loomcycle/client 1.78.0 · loomcycle (PyPI) 1.78.0 — both publish on this tag.

## What's in v1.77.0

*The TeamDef workflow substrate (RFC CY), and facts get one home (RFC CV).*

Two lines land in this release.

A team is no longer just a state machine you invoke. It reads its own work,
fans it out, publishes results onward, and can be watched and stepped while it
runs.

**The `starter` primitive.** The dispatcher the design was missing: it reads
ONE channel, fans out a wave (one run per message, dynamic N), and the RUNTIME
publishes each result to a declared sink. One subscriber means one cursor, so
a fan-out is correct by construction, and agents in a wave need no channel
grant in either direction — the team is the ACL subject. Every spawned run
emits exactly one sink message on a guaranteed path, so a downstream fan-in is
unblocked by failure rather than hanging on it.

**A walk is a run.** `TeamDef op=run` now opens a session and a `runs` row
filed under `team:<name>`, and `mode: "detach"` returns `{run_id, status}`
immediately with the walk continuing behind it. That run_id is also the walk's
correlation id, stamped on the `parent_context` of every agent the walk
spawns — one handle for the response, the run row, the event stream and the
debugger.

**Debug a workflow while it runs.** Breakpoints on a Starter pause before
dispatch (every prompt composed, nothing spawned) or after collection (the
wave done, nothing published). Answer `continue` / `release:<n>` / `abort`
through the existing Interruption machinery. The armed set is read at every
pause, not captured at dispatch, so a state can be armed AFTER a run
started — the case you are actually in when you watch a wave go wrong.

    PUT /v1/runs/{run_id}/breakpoints   {"breakpoints": ["review"]}
    GET /v1/users/{user_id}/agents/stream?walk_id=<run_id>

**Authoring catches what used to fail at run time.** create/fork preflight a
definition's channel references and refuse an unrunnable one with the exact
block to paste; `op=verify` reports `runnable` plus the issues a stored def has
accumulated (a channel deleted, an ACL gap, a member retired).

**Armed subscriptions** (`LOOMCYCLE_TEAM_SUBSCRIPTIONS=1`, default off) drive a
promoted team when its source has work, one replica at a time via a per-team
Postgres advisory lock. This is the only part of the runtime that starts agent
runs unprompted — turning it on is a spend commitment, and it is off until you
say otherwise.

**`system-channels` bundle** (`LOOMCYCLE_PRESETS=base,system-channels`)
declares the runtime's own channels a workflow can read — three heartbeats and
the interruption pair. A bundle rather than a default because a ticker writes a
row every period on every deployment, forever.

**TypeScript adapter 1.77.0** carries the whole surface: `runTeam({mode:
"detach", breakpoints})`, `getRunBreakpoints`, `setRunBreakpoints`.

The memory line moves facts onto the chunk plane: a subject is its own
document and its facts are its children, a fact is reachable from every
subject it is about, and it can be traced back to the turn it came from. The
tenant entity registry is curator-gated.

- **The wave correlation was being dropped on write.** `ParentContext.IsZero()`
  did not list the wave fields, so every run a Starter spawned stored a NULL
  `parent_context`. The guard is now derived from the struct.
- **An "open" channel ACL granted nothing.** Three def-authoring planes
  declared `Publish: ["*"]`, which the matcher never matches — so no Starter
  team was authorable-and-runnable over MCP, gRPC or the admin HTTP surface.
- **Channel `hold` is honoured by every writer**, not only the two that
  resolved the definition.
- **A resolved `{{document:}}` ref is re-checked against its charset**, closing
  a path where an untrusted variable could escape the frame into a system
  prompt.

Nothing is required. Every new behaviour is opt-in: a team without a `starter`
runs exactly as before, `breakpoints` and `mode` are absent unless passed,
armed subscriptions and the system-channels bundle are off until selected.

## What's in v1.76.0

*Temporal memory that answers the question, and an agent editor with no textarea.*

A minor release: full build (all binary variants, multi-arch Docker, sandbox
image). Two lines land here — the memory subsystem's temporal fields becoming
real end to end, and the Library agent editor exposing every parameter an agent
has. No schema migration; no wire-breaking change.

The benchmark number this line is chasing: the raw-turns arm answers temporal
questions at 0.873 while the distilled-facts arm answers them at 0.18. The
fields meant to close that gap existed but were not being filled, not being
selected, and not being followed.

- `observed_at` is now parsed FROM THE TURN rather than asked of the model
  (#1143). Measured across three full runs, asking for it filled it on 0 of
  ~240 facts — the model kept dates in the fact prose instead of the field.
- An unresolved time reference is now kept and `valid_at` resolved from it
  (#1146). `observed_at` ("when it was said") was already saturated at 100%;
  `valid_at` ("when it was true in the world") sat at ~19% and is what the
  questions actually ask — 60 of 63 temporal gold answers name the EVENT's
  date, not the utterance's.
- `recall` now reaches through to the span a fact was distilled from (#1147).
  The store already held that provenance and nothing at retrieval followed it.
- The extractor is asked for the time on the path that has one (#1139), and a
  question is no longer taken as evidence for a fact (#1153).
- An extraction-granularity knob plus rolling fact context (#1155), with the
  window reaching the queued path (#1156), a chat scanned WHOLE rather than
  trimmed (#1157), and the queued batch windowed by MESSAGE rather than by
  queued item (#1158).

Three read-path fixes make those columns survive the round trip: `MemoryList`
returns the temporal columns it was silently dropping (#1144), the remaining
Postgres memory reads carry them too (#1148) — a read path that does not SELECT
them hands back the zero instant, and zero is MEANINGFUL on these columns — and
a memory snapshot round trip keeps them (#1149).

`path op=ls` takes a `limit` and an opaque `cursor` (#1140). It previously
accepted neither, so a caller could not ask for less than the whole directory —
fine today, and not fine for a listing whose size is the tenant's entity count.
`@loomcycle/client`'s `PathToolInput` carries both fields.

The agent editor gained a second, switchable edit surface, and the raw
JSON/YAML overlay box is deleted (#1159, #1160).

An agent has 47 persisted overlay parameters. The editor gave structured
controls to 19 and put the rest behind a free-text textarea with no validation
until submit, no bounds, and no explanation of what any key meant. It was the
third attempt at that idea; each failed the same way, because a textarea cannot
explain a parameter.

- **Form** — the curated layout, unchanged and still the default.
- **All parameters** — every parameter, one row each, under collapsible groups:
  a typed control, its bounds, its overlay key and an always-visible hint per
  row; per-group "n set" badges so a CLOSED group still says what the def
  overrides; groups open by default at exactly the ones holding values.

Both surfaces are driven by one declarative registry, so a parameter is a
single entry rather than a state hook plus JSX plus an overlay builder plus a
hand-maintained covered-keys set. A drift test pins the registry against the
persisted overlay's Go shape, so a new backend parameter cannot ship
unreachable.

The registry and the editor ship as **@loomcycle/def-fields 0.1.0**, a
standalone package with react/react-dom as its only peers, so the loomboard
canvas can reuse the same controls without pulling the Library UI.
**@loomcycle/library 0.5.0** consumes it as a required peer dependency.

LongMemEval added as a second corpus with abstention scored correctly (#1142),
its non-string answers handled (#1154), and a `-answer-only` mode that grades
the store as it stands (#1152). The retention export is pinned to carry
`doc.chunk` body rows (#1141), coordination test replica ids now pass
`ValidateReplicaID` (#1150), and the coordination tier is gated on the Postgres
job (#1151). `make build-ui` installs the new package's deps (#1161) — the
release build runs that target rather than CI's own install list, and the two
had diverged.

`@loomcycle/client` remains at 1.72.0, so the npm publish is skipped for this
tag and the `PathToolInput` additions above are in-tree but not yet on npm.

## What's in v1.75.1

*The memory embedder resolves its endpoint from `providers:` yaml.*

A patch on the v1.75 line. One runtime fix plus the memory-architecture guide.

Repointing `ollama-local` at a new Ollama host the documented way —
`providers: ollama-local: base_url:` in loomcycle.yaml — moved CHAT but not the
EMBEDDER. providerbuild prefers the map entry, while the embedder read
cfg.Env.OllamaBaseURL only, so the two halves of one provider account resolved
independently.

Observed live: chat reached a newly-installed Ollama box and billed 50 calls
while every embed failed `dial tcp: lookup <host> on 127.0.0.11:53: no such
host` — the env var named a Tailscale MagicDNS host, which does not resolve
against Docker's embedded DNS inside the container.

An embed failure warns but does not fail the write (by design: the k/v row is
kept, the response carries `embedded:false` + `embed_warning`), so a caller that
ignores that field just accumulates unembedded rows. A live scope reached 108
fact rows with ~0 embeddings and `/v1/_memory/search` 500'd, while every write
still reported 200.

The two copies of the per-provider base-URL switch are now one shared function.
base_url precedence, highest first:

    memory.embedder.base_url > providers.<id>.base_url > the per-id env default

YAML is the config home; the env var is only the floor. Chat resolution is
unchanged — byte-identical inputs to every driver factory.

Two carve-outs, both covered by tests: `anthropic` inherits NEITHER half of its
chat entry (that embedder slot is a Voyage AI proxy, so an Anthropic proxy
base_url would receive Voyage requests and ANTHROPIC_API_KEY would be sent to
Voyage), and the per-id env KEY stays the last resort for a deployment with no
`providers:` block (the openai embedder accepts an empty key at construction and
only fails later at 401).

Operators: if the embedder endpoint is set via `memory.embedder.base_url` it
still wins by design — remove it to inherit the `providers:` entry. Either way
the address must be one the CONTAINER can resolve; prefer a tailnet IP or a
compose service name over a MagicDNS name. `PUT /v1/_memory/scopes/<scope>/<id>/keys/<key>?embed=true`
returns `embed_warning` naming the endpoint and the error — the whole diagnosis
in one call.

The memory architecture guide, with structure and state diagrams, exported from
the document store to docs/MEMORY-ARCHITECTURE.md.

Tagged as a patch but released with force_full, because the fix is in the Go
runtime: the patch tier alone builds only loomcycle-browser. Adapters are
unchanged and their publish jobs skip clean on version mismatch (no wire change).

## What's in v1.75.0

*Recall the agent can actually reach; one tenant mapping; a benchmark.*

that refuses a rigged partition

THE RECALL TOOL IS AUTO-GRANTED WHEN RECALL IS ON (#1135)

v1.73.0 shipped recall-augmented distillation: an opt-in context.recall harvests
every evicted span into a run-scoped index. The agent could not query it. An
empty `tools:` allowlist is default-DENY rather than default-all, and a populated
one easily omits Recall — so a recall-enabled agent had the index and no way to
reach it, and the docs implied a default-all toolset that does not exist.

An end-to-end run found it the way these things get found: the agent, asked for a
value distillation had evicted, CONFABULATED rather than recalled, because Recall
was never in its toolset at all. Enabling recall now auto-grants the builtin at
every toolset-resolution site — RunOnce, the HTTP run path, session-continue,
sub-agent spawn and resume. Read-only over the run's own evicted spans and the
agent's own memory scope, so it widens the allowlist and not the trust boundary;
a no-op when recall is off or no embedder is configured.

ONE TENANT MAPPING (#1136)

The `"" -> "default"` mapping that turns a runtime tenant into a SQL Memory scope
key had FOUR implementations across three packages, each with a comment naming a
DIFFERENT one as the source of truth and nothing asserting they agreed. They did
agree, byte for byte, which is the only reason this was latent rather than live.

Divergence is not cosmetic, and the erasure call site already spelled out why: a
DropScope built from a raw "" tenant matches nothing, so a single-tenant
deployment's subject erasure would leave the subject's ENTIRE SQL Memory database
in place while REPORTING SUCCESS. The rule now lives once, in internal/sqlmem —
the package whose own constraint creates it, since pgScopeNames must reject an
empty tenant or every single-tenant deployment would share one schema and one
LOGIN role. Pinned by a test covering the empty case, a tenant literally NAMED
"default", and that distinct tenants still derive distinct schemas.

A BENCHMARK THAT REFUSES A RIGGED PARTITION (#1136)

The answer axis already refused an EMPTY store, on the stated grounds that
"accuracy 0.0000" is a number about the plumbing wearing the costume of a result
about memory. That was not enough. With the corpus tenant's ontology declaring a
tenant memory scope for its own entity types, the consolidator PLACED most facts
into the tenant scope while the answerer recalls from the user scope only. Enough
rows remained for the empty check to pass, the run scored 0.0216 with 95%
abstention, and three plausible mechanisms were reasoned on top of that number
before anyone compared the counter to the store.

The guard now compares them: how many facts the pass reported writing against how
many are reachable where the answerer looks, refusing on a shortfall of more than
half and naming placement as the usual cause. Chunk-body rows are deliberately
not counted — counting them is exactly what made a diverted partition look
populated.

WHAT THE MEASUREMENT SAID, since three releases served it

Consolidated facts versus raw turns, same corpus, same 199 paired questions:
0.7383 for turns against 0.1574 for facts, McNemar exact p = 2.4e-29. The
mechanism is abstention (76% versus 15%) rather than retrieval, and not yield —
7.2:1 and 9.0:1 compression with exact counters. Temporal is the sharpest slice:
0.873 against 0.036, because a raw turn carries its timestamp in the text and
distillation strips it. Consolidation's demonstrated gain on that corpus is about
one question in 199.

The operational consequence is why #1135 matters more than it looks: `Memory add`
with the default infer=true stores no retrievable row, so a deployment's recall
path IS the losing arm. Recall-augmented distillation is the answer to that, and
until this release the tool was unreachable.

THE SELF-GUARD LIMITATION, STATED (#1134)

Placement's most reassuring promise was overstated. "It never places a fact about
you" holds only for facts learned from that owner's OWN conversations: another
user recording the same thing is recording a fact about a third party, so their
copy is placed. Measured on a two-user corpus, each user published the other
speaker's facts tenant-wide, leaving each owner's facts MORE exposed than before
placement was enabled. Not a bug — what per-scope decisions with no global view
produce. The shipped ontology template now says so in the place an operator reads
while deciding, and docs/TOOLS.md documents the mechanism for the first time.

VERSIONS

@loomcycle/library is 0.4.0 for the capability-gates renderer (#1133) and
publishes on its own library-v tag. The TS adapter stays at 1.72.0 and the Python
adapter at 1.67.0, so both skip clean here. @loomcycle/memory-view 0.5.0 is
already on npm.

## What's in v1.74.0

*What distillation drops can now outlive the run.*

PERSISTENT-MEMORY HARVEST ACROSS DISTILLATION MODES (#1131, RFC CT P2)

v1.73.0 made an evicted span recoverable WITHIN its run, through a
run-scoped index and the Recall tool. This is the cross-run sibling: an
opt-in per-agent `context.harvest_to_memory` banks each evicted span for the
memory consolidator, so what a distillation drops can become a durable fact
instead of being lost at the end of the run.

It generalizes what already existed for exactly one mode. `compaction.memory_flush`
banked the span behind a compaction cut and nothing else; banking now happens at
the recap and stateful boundaries too — the same three places the P1 recall index
harvests. The banking callback is installed when either flag is set, so an
operator relying on memory_flush is unaffected.

BANKING RATHER THAN INLINE EXTRACTION, and the choice was measured. Banking
hands raw spans to the existing consolidator instead of extracting per span on
the hot path. RFC CU Probe 2 found that per-span isolated extraction LOSES
coreference-dependent facts — a subject named two spans earlier — that the
consolidator's whole/batched extraction keeps: 0.75 versus 0.00 on user-project
coreference, McNemar p=0.0010. Broader context and no model call on the
distillation path is the right shape.

A misconfiguration is now loud rather than silent: no store, no user scope, or no
user_id surfaces as an EventError instead of a run that simply never harvests. A
banking failure never fails the run. The banked span's metadata source
generalizes from "compaction" to "distillation" since it now covers all three (no
consumer branches on it). Deliberately absent on resume, which replays past
distillations. Off by default, and content-identifying like the rest of the
context block, so a fork that flips it mints a distinct content_sha256 while every
pre-feature agent row stays byte-stable.

The server now resolves the merged context once per run, removing three redundant
recomputes in the run builders.

RELEASE HISTORY AND THE RECALL TOOL DOCUMENTED (#1132)

REVISIONS.md had stopped at v1.71.0 while three tags shipped; v1.72.0 and v1.73.0
now have sections. The v1.72.0 entry also records what the memory-placement work
MEASURED, including the part that does not flatter it: duplicate copies fell 30 to
23, which is -23% and not elimination, because cross-user duplication was replaced
by tenant-to-owner duplication. The payoff that does hold is sharing — 94 facts
readable by both users where none were before — and the self-guard's limitation is
stated as a limitation: it cannot protect what the counterparty also recorded.

The `Recall` tool shipped in v1.73.0 appearing in no tool list at all is fixed:
README, docs/TOOLS.md and the project guide now carry it. The TOOLS.md entry as
first written was wrong, taken from a commit message, and reading the registration
site corrected it — the tool is registered UNCONDITIONALLY like Memory, because
only the run-scoped index is conditional and the durable-memory search is useful
to any agent that grants the tool.

VERSIONS

The TS adapter is unchanged at 1.72.0 and the Python adapter at 1.67.0, so both
publishes skip clean on this tag. @loomcycle/memory-view sits at 0.5.0 and
@loomcycle/library at 0.3.0; both publish only on their own `memory-view-v` /
`library-v` tags, and the embedded Web UI consumes both from SOURCE, so this
binary ships their current state regardless.

KNOWN DOC GAP

README's capability table does not yet mention `context.harvest_to_memory`
alongside `context.recall`; the operator reference in docs/CONFIGURATION.md
covers it.

## What's in v1.73.0

**Recall over what distillation threw away (#1129, RFC CT P1).** Every context
retention mode in this runtime discards work by design: a compaction drops the
turns behind its cut, a recap keeps the reasoning and drops the rest, and a
stateful step feeds forward `(Σ, O)` and discards how it got there. That is the
point of them — but the dropped span is often exactly where the one value the
model now needs was stated.

An opt-in per-agent `context.recall` harvests each evicted span, at the last
moment it exists, into a run-scoped embedded index, and gives the agent one
`Recall(query)` tool to read it back. **Free text rather than identifiers**,
because a model queries fluently in plain language and barely reproduces ids or
its own exact prior wording — which also makes the tool far likelier to be
invoked at all. It searches the run index, silently falls back to the agent's
durable memory across its permitted scopes, merges by score, and returns the
originals verbatim.

Run-scoped and in-memory deliberately: the persistent vector store is per-scope
and durable, so indexing every evicted turn there would pollute the agent's
memory and add store writes to the distillation hot path. A harvest is never
fatal, a nil embedder makes both halves a clean no-op, and the index is
FIFO-capped. The resume path is deliberately **not** wired — a resumed run
replays its past distillations without harvesting, so nothing double-indexes.

Off by default: unopted runs and no-embedder deployments are byte-identical, and
`recall` threads through the same content-identifying plumbing as the rest of the
context block, so a fork that flips it mints a distinct `content_sha256` while
every pre-feature agent row stays byte-stable.

**The Memory console honours the tenant focus (#1130).** The last hop of the
admin-focus fix below. The server had accepted `?tenant=` since v1.72.0 and
`@loomcycle/client` 1.72.0 could send it, but the console was not asking, so a
super-admin's Memory page could only read its own tenant — usually the empty one
— while that tenant's operator saw its rows. Every other browse surface
(Documents, Paths, Agents, Users) already consumed the topbar focus; Memory was
the one that never did.

The focus binds at data-layer **construction**, not per call. Which tenant's
workspace an operator is looking at is a property of the console session, so
threading it through `MemoryDataLayer` would have put the same never-varying
value into twenty call sites and changed a 21-method interface to carry it. A
`browse` option captured by `dataLayerFromClient` reaches every method without
any of them knowing it exists. `listScopes` deliberately does not carry it: that
route answers "what *kinds* of scope exist", a constant set, and sending a tenant
would imply the answer varies by tenant.

Verified on a live deployment before the tag, with two tenants holding 0 and 136
rows: an admin naming none still gets 0 (the default is unmoved), an admin naming
the tenant gets 136 (the capability is real), and a tenant operator naming
*another* tenant gets its own 136 — the wire value ignored, not honoured and then
checked.

`@loomcycle/memory-view` is 0.5.0, with its `@loomcycle/client` peer raised to
`^1.72.0` so a consumer resolving an older client gets a dependency error rather
than a console that silently sends nothing. The TS adapter is unchanged at
1.72.0 and the Python adapter at 1.67.0.

## What's in v1.72.0

Three fixes found by *using* the runtime rather than reading it. Each was an
operator question the console answered wrongly, or not at all.

**A super-admin may focus a tenant on the memory browse routes (#1127).** An
admin was strictly **less** capable than a tenant operator here, which is
backwards: `substrate:admin` satisfies every scope gate and then could not read
the data the gate admits. The five memory browse handlers resolved the tenant
from the bearer and took nothing from the URL, so a `substrate:tenant` token read
its own tenant's memory while an admin saw only its own — usually empty — and had
no parameter available to look elsewhere. From the console that reads as a
permissions failure.

The inconsistency was inside one file: the sibling maintenance routes on the same
store — embed stats, reembed, backfill, purge — had accepted `?tenant=` via
`principalTenantScope` all along. Only the reads and writes an operator actually
browses with were pinned.

Only an **explicit** focus widens. With no `?tenant=` the resolution is
byte-identical to before, so this adds a capability rather than moving a default,
and a non-admin's wire value is ignored rather than honoured-then-checked — no
tenant can widen its own scope.

Three helpers resolve a tenant and they disagree on the admin default. That
disagreement is irreducible: a **list** read can mean "every tenant" while a
single-tuple read must name exactly one. It is now written down once, naming all
three and the invariant they share, with every cell of the table pinned by a
test. What made it a defect was not the difference — it was that nothing
asserted it, so the difference read as an accident, and on these routes it was.

**The Library shows what an agent is allowed to reach (#1126).** An operator
asking "what can this agent do?" got half an answer, and it cost real diagnosis
time: while checking why memory placement was not working, the Library showed no
`sql_scopes` at all, which reads as the grant being absent. The grant was
present. Only the field was missing.

Two independent gaps produced one symptom. The wire shape was a hand-maintained
third mirror of the substrate agent shape — its own comment said mirroring was
its purpose — and it had drifted to **19 of 46 fields**: `sql_scopes`,
`history_scope`, `memory_consolidation`, `internal`, `sampling`, `compaction`,
`context`, the five `*_def_scopes` authoring gates and eighteen more never
reached the client. Rather than add the missing 27, the mirror is deleted: a
converter makes the authoritative struct the wire shape, so there is no third
thing left to fall behind. And the renderer showed no capability gate at all, not
even `memory_scopes`, so even served none of it would have appeared — an agent
granted the tenant plane looked identical to one confined to its own scope.

The guard is two assertions because they catch different faults. **Coverage**
populates every field and requires none left at zero, which catches the forgotten
assignment. The **round trip** requires equality, which catches a field wired to
the *wrong* source — `SqlScopes: def.MemoryScopes` leaves nothing zero and is
still wrong. A round trip alone would also miss a symmetric omission, since a
field dropped by both directions survives it untouched. Both faults were verified
by injection rather than argued.

**Tenant focus on the client's memory admin methods (#1128).**
`@loomcycle/client` 1.72.0: the five memory browse methods take an optional
`tenant`. `listMemoryScopes` deliberately does not. A blank focus is dropped
rather than sent, because the Web UI's switcher is empty until an admin types one
and empty there means "my own tenant", not a tenant named `""`.

### Known state, stated plainly

**This release closes a chain that began with ontology-declared memory
placement.** v1.71.0 made placement reachable at all — the shipped consolidator
had held no `sql_scopes`, so neither half of a placed fact could reach the tenant
plane — and v1.71.1 fixed placed facts arriving **orphaned** from their subjects,
because the `about` edge joining them was written in the caller's scope where
neither endpoint existed. Neither has its own section here: patch releases carry
their notes on the annotated tag.

**Placement has now been measured.** On a two-user corpus, duplication fell from
30 duplicate copies to 23 — cross-user duplication was eliminated and *replaced*
by tenant↔owner duplication, so the honest figure is −23%, not elimination. The
real payoff is sharing: **94 facts are readable by both users where 0 were
before**. A second-order effect was unpredicted — cross-user type stability, with
`subject types held stable` at 4 for the user who wrote first and **91** for the
second, whose extractor kept proposing new types for subjects already on file in
the shared plane.

**The self-guard cannot protect what the counterparty also recorded.** Each user
played one speaker in the corpus. The guard correctly kept a speaker's own facts
in their scope, while the *other* user, for whom that speaker is a third party,
published the same facts to the tenant plane. So placement left the owner's facts
more exposed than baseline, which is precisely what the guard exists to prevent.
Not a bug — the consequence of per-scope decisions with no global view. In any
multi-party corpus the guard's promise is defeated by the other party, and the
narrow declarations (`organization`, `project`, `service`) are the safer default.

## What's in v1.71.1

*A placed fact stays reachable from its subject.*

A PATCH, BUILT FULL. The fix is in the embedded memory bundle, so it ships in
the runtime binary and image rather than the browser image a patch tag
normally builds alone. Released with force_full.

WHAT WAS WRONG (#1125)

v1.71.0 made ontology-declared placement reachable for the first time. Running
it end to end showed that a placed fact was stored in the shared plane and
orphaned there.

`mirrorEntity` threads the placed scope into every write it makes — the
entities document, the canonical-type lookup, the subject node, the fact node
— except the `about` edge that joins the two nodes, which used the pass's
configured scope. So both nodes landed in the tenant plane and the edge was
written in the caller's scope, where neither endpoint exists. The link failed,
a counter incremented, and the pass carried on.

Measured on the live store: 3 facts placed, 3 edges lost, 0 inbound edges on
the subject node. The facts sit in the plane every user reads and a graph walk
from the person's name reaches none of them — which is the single property the
entity tier exists to provide. One word: the placed scope, not the configured
one.

The judge had the same defect one function over. A candidate carried its id but
not the scope it was written in, so judge_fact looked for a placed fact in the
configured scope and could only fail. The scope now travels with the candidate.

Two silent failure sites now record the reason, matching the two that already
did. This is why the diagnosis needed a code read: the deployed build reported
"3 graph write(s) failed" and nothing more, while the mechanism for saying why
was already there and simply not wired at these sites.

WHY NO TEST CAUGHT IT

The test double was scope-blind. Its chunk store was natural_key -> id with no
record of where a chunk was written, so every scope answered identically, and
the sibling test — which asserts the row, the chunks AND the entities document
all reach the declared scope — passed throughout without ever looking at the
link. A double has to model the dimension the feature varies. It now records
each chunk's write scope and refuses a link naming a scope neither endpoint
lives in, as the real store does, and reproduces the production report
verbatim.

The adapters and protos are unchanged and remain at 1.67.0.

## What's in v1.71.0

Two lines land together: the layered-context work reaches the multi-agent case, and
ontology-declared memory placement becomes reachable at all.

**A stateful sub-agent hands its state up, not its transcript (#1122).** When a fan-out
parent spawns a stateful child, the parent now receives the child's final structured
state Σ as the result rather than only its prose. A single spawn folds Σ into the
child's `tool_result` as JSON; `parallel_spawn` carries it as a structured `state` field
on each envelope entry; the spawn ledger captures it so a parent restored from a
snapshot keeps a completed child's Σ. That turns N growing transcripts into N compact
structured results — `O(T²)`-per-agent × N becomes N independent `O(T)`. Non-stateful
children are unchanged, and the Team orchestrator is string-only end to end, so
carrying Σ there is still a follow-on.

**`context.mode: auto` routes by the model that actually resolved (#1120).** An
operator sets `auto` once and the agent runs schema-free **recap** on a local backend
and structured **stateful** on a frontier API, instead of hand-picking per deployment —
because structured state needs reliable structured output and a weaker local model
cannot be relied on for it. Providers gained a `Local` capability to make that a routing
fact rather than a guess (`ollama-local`, vllm and llamacpp are local; the hosted
`ollama` is not). The mode resolves once at run start on a clone of the context, so the
shared agent def is never mutated; an interactive run never resolves to stateful, since
that loop has no steer/park. An explicit mode still wins, and an agent with no context
block stays `append` — byte-identical, so `auto` is opt-in. The same PR closed a
transport gap: the per-run `context` override now flows through the connector and MCP
`spawn_run`, matching `sampling` and `compaction`, which were already there.

**A model may propose a state schema; only an operator adopts it (#1121).** A stateful
agent can suggest the shape its task's state should hold via `emit_state`'s
`propose_schema`. The proposal is inert — recorded on the transcript, returned on
`RunResult.ProposedSchema`, surfaced only when it differs from the active schema — and
changes validation for no run. Adoption reuses the versioned agent-def substrate: an
operator forks the def with the schema in `context.state_schema` and promotes it. Same
fail-safe as the ontology's propose→adopt, and no bespoke store for it.

**Ontology-declared placement was unreachable, in every deployment (#1123).** v1.68.0
shipped the mechanism and v1.70.0 fixed the extractor that was starving it, but nothing
could place a fact: the shipped consolidator held `memory_scopes: [agent, user]` and no
`sql_scopes` at all. A placed fact is stored **twice** — a k/v row, which `recall`
searches, and a typed chunk, which a graph walk reaches — so `tenant` has to be on both
grants or the two halves cannot land in the same scope. The bundle now grants them.

The absence had been deliberate, on the argument that an unused grant is the capability
an injected instruction reaches for. That argument is written for a *prompt-driven*
agent, where model output steers control flow. The consolidator is `provider: code-js`:
a deterministic body in which model output is data that gets written and never code that
gets run. What bounds the tenant write is the placement resolver, which fails closed on
every uncertainty — an unknown or inconsistently-typed subject, or a fact about the
profile owner, stays with the user. The grant is also inert on its own: no shipped
ontology type declares a scope, so nothing is placed until an operator declares one for
a type, per tenant. Placement remains **off** by default.

**Both ways to change a bundled agent's grants were broken, and finding out cost an
instance (#1123).** A runtime `AgentDef fork` to flip one grant returned
`fork: definition (139169 bytes) exceeds max 131072` — for a code body the overlay never
touched. `MaxCodeBytes` (256 KiB) exists precisely so executable source is not judged by
the whole-definition cap (128 KiB), but the definition check measured the JSON with the
body inside it, so any body between the two passed the cap written for it and was
refused by the smaller one. The consolidator's ~133 KB sits in that dead zone; the
dedicated cap could never bind. It now measures the definition without the body, on
create and fork alike, and the stored definition is unchanged.

The config route merges correctly — a re-declared bundle agent keeps its body, provider,
tools and gates — but layered onto a stack that does *not* contain the bundle, the same
overlay is indistinguishable from a new agent, so validation refuses it with
`no model, no tier, and no defaults.model`. Accurate, and it sends the reader to inspect
the overlay they just wrote rather than their `LOOMCYCLE_PRESETS`; boot is fatal, so the
wrong first guess costs a down instance. A declaration carrying nothing but capability
grants now says so and names the layer stack to check. Two statements of the `sql_scopes`
enum that listed `agent, user, run` long after `tenant` was added — one of them the
validation error itself, so a *correct* config read as rejected — are fixed, and the
message is now rendered from the set.

### Known state, stated plainly

**Whether declared placement is worth having is still unmeasured.** It is now reachable
for the first time, which is the precondition, not the result. The baseline it has to
beat was measured on a two-user corpus before placement could fire: **30 duplicated
facts, 22%** of the store.

**The `sql_scopes` grant is wider than this needs.** The gate consults a single list, so
opening the Document tenant-chunk path opens the raw `Memory sql_exec` surface on the
tenant keyspace with it; there is no narrower grant today. The consolidator body issues
no SQL op, and a test now asserts it does not, so adding one means re-arguing the grant.

**A failed fork can leave an orphan def row.** `AgentDefCreate` bootstraps a v1 row from
static config before the cap check runs, and does not set the active pointer — so
nothing serves it and resolution still falls through to static config. Do not `promote`
such a row: it captured the pre-change definition.

The adapters and protos are unchanged and remain at 1.67.0, so no `python-v` tag
accompanies this release.

## What's in v1.70.0

*(This entry was reconstructed from the annotated tag, which is authoritative — the
release was cut without a `REVISIONS.md` section.)*

**The extractor is told who the owner is, or told nothing (#1118).** v1.68.1 gave the
extraction prompt a rule worse than no rule, and a live two-speaker run proved it: it
said *"a fact about THEM takes the subject `user`"* and never said who THEM was, because
the declared Identity names live in the user-root document, which reaches
`{{memory:user_info}}` and not the extractor. So the model guessed the more prominent
speaker. In a scope whose profile declared Dave, it made `user` = Calvin — **inverting
the self-guard**, protecting Calvin's facts as if they were the owner's while leaving
Dave's, the actual owner's, placeable. Protecting the wrong person while exposing the
right one is worse than doing neither. `Context op=self` now reports `self_names`,
parsed server-side by the one function that owns the column-0 rule that stops the
template's own indented example from naming every unedited profile after it. The names
are **omitted**, not empty, when nobody declared any, so a caller can tell "nobody said"
from "said nothing"; the prompt then asks only for consistent spelling rather than
inviting the model to identify an owner it cannot know.

**Layered execution context (#1117, #1119).** L1 reasoning-recap context retention and
L2 structured execution state — the first two phases, with an RFC 7386 merge plus a
minimal schema validator for the structured-state layer.

**What the measurement found.** On the only real corpus available, the local extractor
produced **zero** facts from 568 turns while a cloud model produced **74** from the same
input — so the earlier "typing is inconsistent" finding had been measuring a model that
was barely functioning, not a design defect. With a capable extractor the typing bars
pass: 62/62 linkage, 0% of claims on a multiply-typed subject, repeat-subject
consistency 1.00 against 0.00 before.

## What's in v1.69.0

v1.68.0 shipped ontology-declared placement. A two-user run on a real corpus then
showed it could not work: **every typed fact was being lost before it reached the
graph.** This release is what that measurement found, and it is worth reading in
that order — none of it was predicted from the code.

**A subject keeps the type it is already filed under.** A subject node's natural key
is `type + ":" + slug`, so the type is part of the identity. Each extraction call
picked a type fresh, with no knowledge of how that subject was typed before, and
nothing reconciled them. On a 142-claim benchmark store one person existed **five
times** — `event`, `location`, `object`, `organization`, `person` — carrying 89 claims
across five nodes, and **94% of all claims hung off a multiply-typed subject**. So
*"what else do we know about her"*, the question the entity tier exists to answer,
could reach at most a fifth of what was stored.

The type a subject already has now wins over the current call's guess, for both the
key and the field. **First write wins**, and that is the design rather than a
tie-break: it is the only *stable* choice, because any rule that can change a
subject's type later re-partitions every fact already filed under the old one. A
wrong-but-stable type keeps one subject's facts together, which is what matters.
Existing multiply-typed nodes are **not** migrated — this prevents new splits.

**An undeclared type becomes a candidate instead of a lost write.** The extractor
invents kinds the tenant has not declared — `experience`, on the corpus measured — and
`upsert_chunk` refused those writes, so the fact landed in key/value with no graph
presence at all. The gate's own message is that an undeclared-type node *"becomes a
node nobody can find"*; refusing produces **no** node, which loses the subject
entirely.

Such a type is now filed as an **inert ontology candidate** and the subject written
under `object`. The candidate changes nothing for any run until an operator accepts
it, so an invented kind becomes something a person can adopt rather than a silent
loss. A statement class misused as a kind — `preference:user` reads as *"the entity of
type preference named user"* — also falls back rather than being dropped, but is not
proposed: those names are already declared and are simply being misused. The key moves
with the type, because the type is identity; and the retry is matched against the
gate's own wording, so a store fault is never mistaken for an undeclared type.

**A failing consolidation pass says why.** The consolidator counted its graph failures
and discarded the reason — and there is no second place to look, because it is an
`internal: true` agent whose runs are kept out of the run and history surfaces and
whose transcript cannot be read back. The pass report is the only thing it ever gets to
say. It now carries the first error text, and counts a failed subject-type **lookup**
separately from a failed chunk **write**: one counter for both is what made a live
signal uninterpretable.

**Identity documents on every user-creation path.** v1.68.0 provisioned the user-root
and tenant-root documents when a principal was established — for one of the three paths
that establish one. The Web UI drives the other two, so users created there got no
profile: no Identity section, no way to declare their own names, and placement cannot
tell a fact about that person from a fact about a colleague without them.

### Known state, stated plainly

**RFC CQ's gate is still not satisfied.** On the only real corpus available, 1,136
turns of input across two users produced **four facts**, with 35–39 malformed extractor
replies dropped per pass. Placement cannot matter at that rate, and a
typing-consistency number computed over a handful of subjects is not a result — that
mistake has already been made once in this line and corrected. The yield question is
what gates a real answer, and it is open.

The adapters are unchanged and remain at **1.67.0**.

## What's in v1.68.2

*A failing consolidation pass says why.*

One fix. The memory consolidator counted its entity-graph failures and threw
away the reason:

    entities 0 fact(s) across 0 subject(s), 2 graph write(s) failed

That is unactionable, and there is no second place to look. The consolidator
is an `internal: true` agent, so its runs are deliberately kept out of the run
and history surfaces and its transcript cannot be read back — confirmed with a
404 from both /v1/runs/{id} and its events. The pass report is the only thing
it ever gets to say, so a swallowed message is a message lost for good. A live
two-user run on a real corpus stalled on exactly this: every entity write
failed and the cause was unrecoverable afterwards.

Three changes, none of which alter what gets stored:

  - The FIRST error text is kept and reported, truncated. First only, because a
    per-failure list would put unbounded model-adjacent text in the report, and
    the first one is what a person acts on.

  - A failed subject-type LOOKUP now has its own counter and message. It is a
    different failure from a failed chunk WRITE — a bad lookup means the type
    may drift, a bad write means the fact is missing from the graph entirely.
    Sharing one counter is what made the live signal uninterpretable as soon as
    v1.68.1's canonicalType began contributing to it.

  - A reply that PARSES but carries no id is now counted. It previously
    returned empty and incremented nothing, so a pass could lose every entity
    write and still report a clean sheet.

BUILD NOTE

Tagged as a patch but built with force_full, because the fix is in the runtime
and a patch tag otherwise builds only the loomcycle-browser image.

The adapters are unchanged and remain at 1.67.0, so no python-v tag
accompanies this.

## What's in v1.68.1

*The entity graph keeps one subject as one subject.*

Two runtime fixes on the v1.68.0 memory line, both found by measuring a real
store rather than by reading the code.

A SUBJECT KEEPS THE TYPE IT IS ALREADY FILED UNDER

A subject node's natural key is `type + ":" + slug`, so the type is part of
the identity. Each extraction call saw one transcript and picked a type
fresh, with no knowledge of how that subject was typed before, and nothing
reconciled them. Measured on a benchmark store of 142 claims:

    caroline -> event, location, object, organization, person  (5 nodes, 89 claims)
    melanie  -> event, object, person                          (3 nodes, 43 claims)
    nia      -> event, object                                  (2 nodes,  4 claims)

Every subject mentioned more than once was typed inconsistently, and 94% of
all claims hung off a multiply-typed subject. Facts about one person were
scattered across five graph nodes, so "what else do we know about her" — the
question the entity tier exists to answer — could reach at most a fifth of
what was stored. It also meant ontology-declared placement would refuse 94%
of that corpus, since it correctly declines an inconsistently typed subject.

The type a subject is already filed under now wins over the current call's
guess, for both the natural key and the type field. FIRST WRITE WINS, and
that is the design rather than a tie-break: the first type is the only STABLE
choice, because any rule that can change a subject's type later
re-partitions every fact already filed under the old one.

Existing multiply-typed nodes are NOT migrated — this prevents new splits.

The extraction prompt also now states whose memory it is filling, as a rule
("a fact about THEM takes the subject `user`") rather than by handing the
model a subject id. A transcript between two other people has no fact about
the owner, so a benchmark corpus is unaffected.

IDENTITY DOCUMENTS ARE PROVISIONED ON EVERY USER-CREATION PATH

v1.68.0 provisioned the user-root and tenant-root documents when a principal
was established — for one of the three paths that establish one. The hook sat
on the OperatorTokenDef substrate tool; `POST /v1/_users` and
`POST /v1/_users/{subject}/tokens` write their rows directly and share no
code with it. The Web UI drives those two, so a user created there got no
profile, hence no Identity section, hence no way to declare their own names —
and placement cannot tell a fact about that person from a fact about a
colleague without them. The feature was inert for the people it was built
for.

Both paths are now hooked. A user created before this ships stays without a
profile until a token is minted for it, or the document is created directly.

ALSO

A long-horizon context-retention benchmark harness (bench only, no runtime
surface).

BUILD NOTE

Tagged as a patch but built with force_full, because the fixes are in the
runtime and a patch tag otherwise builds only the loomcycle-browser image.

The adapters are unchanged and remain at 1.67.0, so no python-v tag
accompanies this.

## What's in v1.68.0

**Organisation knowledge has somewhere to live (RFC CQ).** Some of what a
consolidator learns is not about the user — *"the checkout-api service requires two
approvals"* is true for everyone in the tenant. Until now every fact went into one
user's scope, so a colleague either re-learned it from their own conversations or
never learned it at all, and there was no surface anywhere that would tell an
operator this was happening.

**Placement is operator config, not per-fact inference.** An entity type in the
tenant ontology may declare which memory scope facts about that kind of thing belong
in:

```
## service
- `@memory_scope` tenant
- `name` — what people call it
```

A fact reaches its scope through the subject entity it is already linked to. The
alternative — asking a model per fact — would put a judgement in front of every
write, thousands of independent chances to put a private sentence in front of the
whole tenant, and it would land on the extractor, whose own prompt is documented as
*"a mitigation, not a guarantee"*. A handful of declarations, authored once and
versioned in a document, replaces all of that. It also costs nothing at write time:
the type is already assigned.

**Every uncertainty declines to move the fact**, and that asymmetry is the design
rather than caution. Declining costs exactly what the system already costs — the fact
stays in one user's scope. Moving one wrongly is not recoverable. So a placement is
refused for an undeclared or unknown type, a draft ontology, no ontology at all, a
subject that names the run's own user, a subject the store types inconsistently, an
isolated member, and a scope the writing agent has not been granted — each with a
reason an operator can act on.

**Both halves of a fact move together, or neither does.** A fact is stored twice: the
key/value row semantic recall searches, and a chunk mirror the graph walks. Split
those across scopes and `recall` finds the fact in one place while `graph_recall`
finds it in another, which is worse than never moving it. So the decision is made
once per batch, before either write, by the writer that owns both — a new read-only
`Memory op=placement`. A tenant placement consequently needs the tenant grant on
*both* `memory_scopes` and `sql_scopes`, because the mirror is a Document write.

**A user can say who they are.** The per-user profile document gained an Identity
section — `@name` and `@alias` bullets — because nothing else can tell a fact about
the user under their own name from a fact about a colleague: *"Ada prefers Go"* and
*"Maria owns the release process"* are the same shape. With names declared, the
user's own facts stay theirs whatever their type says. Undeclared, that gap remains
exactly as it was, which the test suite asserts out loud rather than assuming away.

**The identity documents now exist when a principal does**, at token mint and at boot
for config-declared principals, instead of appearing on the first run that happened
to reference them. A template that arrives after the moment it was needed is not a
template. `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS=0` restores the old lazy-only
behaviour.

**Reads are unchanged, and now say so.** A memory read touches exactly one scope —
it always did, but the `scope` description described *who* could reach the tenant
keyspace and never that a separate call is required to read it, so an agent asked
once, got nothing, and concluded the organisation knew nothing. Both `Memory` and
`Document` now state the invariant and the remedy: one scope per call, two calls to
consult two, merge by score.

**Inert until an operator turns it on.** Nothing is declared out of the box, the
consolidator's grants are untouched, and a deployment that edits nothing behaves
exactly as it did in v1.67.0.

### Also in this release

**A sqlite upgrade blocker.** Any deployment whose `memory` table predated RFC CL
(v1.65.0) could not start on v1.66+: `migrate` created three partial indexes ahead of
the `ALTER`s adding the columns they name, and on an existing table
`CREATE TABLE IF NOT EXISTS` is a no-op, so the index failed on a missing column and
the store never opened. A fresh database gets those columns from `CREATE TABLE`, which
is why every test passed — they all start from an empty file. Both halves are now
tested: a legacy-shaped fixture, and a guard that reads the two statement lists and
fails for any table.

**Wikidata benchmark harnesses** for knowledge updates, ontology typing and
cross-lingual recall, plus a bulk fact-corpus builder and importer.

### Known state, stated plainly

RFC CQ's own gate — whether a real store's entity typing is consistent enough to
carry a scope decision — **has not been run on a real multi-user store**, and its
preliminary reading on a small development scope *fails*: linkage was complete, but
two of five subjects carried two types, and one of them would place a personal fact
in the shared plane. That is a data-quality problem rather than a design one, and it
is why placement declines an inconsistently typed subject instead of guessing. Until
that measurement exists, treat a `@memory_scope` declaration as something to try on a
store whose typing you have looked at.

The adapters are unchanged in this release and remain at **1.67.0**.

## What's in v1.67.0

**Per-user credential self-service (RFC CN).** A logged-in user can now store its
OWN API tokens — a personal Slack/Telegram bot token, a per-user webhook secret —
without handing the secret to a tenant operator. The *consumption* side already
bound per-user credentials (`$cred:<name>` resolves **agent > user > tenant**, per
run); the missing half was letting a user *put a token in*. Now every transport does.

Before this, `POST /v1/_credentialdef` was `substrate:tenant`-gated: an isolated
`substrate:user` user was refused at the tenant-member isolation floor, and the Web
UI's only credential control lived in the operator Settings hub. The rule now: a user
may manage **only `scope=user`** credentials keyed on its own subject; an omitted
scope defaults to `user`; `scope=tenant`/`agent` still requires `substrate:tenant`.
One shared check (`credential.ConstrainToUserScope`) enforces it identically on every
surface, so the transports cannot drift.

### Across every transport

- **HTTP** (#1093) — `POST /v1/_credentialdef` admits an isolated user (additively —
  tenant/admin authoring and the RFC CB member path are unchanged), confined to
  `scope=user` by the handler.
- **MCP** (#1095) — an isolated `substrate:user` session may open `/v1/_mcp` and call
  **only** the `credentialdef` meta-tool (the `loomcycle mcp --upstream` thin client).
- **Web UI** (#1096) — a standalone **"My Credentials"** page, visible to every login
  (including a delegated user with no Settings gear); the operator Settings →
  Credentials tab (tenant authoring) is unchanged.
- **gRPC + adapters** (#1097) — a new `CredentialDef` RPC, plus
  `createCredential`/`listCredentials`/`deleteCredential` on `@loomcycle/client` and
  `credential_def()` on the Python client. Both adapters ship at **1.67.0**.

The worked example — each user receives its own GitHub webhook and publishes to *its*
Slack/Telegram channel with *its* bot token — needs no per-user def authoring: an
operator authors the flow once referencing `$cred:telegram`, and each user self-serves
only its token, which wins per run over the tenant fallback.

### Also

- **fix(history):** `op=recap` now writes a **short** chat-list summary (at most two
  sentences, under 256 chars) instead of reusing the compaction prompt — which on a
  long chat returned several paragraphs that no list surface could rely on (#1092).
- **bench:** the LoCoMo harness gains an **answer axis** (`-mode answer` — whether an
  agent can *answer* from what it retrieved, not just whether the right rows come
  back) plus the RFC CL/CM measurement instrumentation (#1094).
- Refreshed the default local-provider model aliases in the `local` preset.

## What's in v1.66.0

Two feature lines land together: **RFC CK** makes local inference a first-class
bundle and lets an operator reload a running config without dropping in-flight runs,
and **RFC CL** gives a memory row a sense of *time* — when a thing was said and when
it was true — so a question can ask about a moment, not just a topic. Minor rather
than a patch: both add runtime primitives the binary must carry, and the Memory tool
plus the adapters gain surface.

### RFC CK — local providers in bundle YAML, and on-the-fly config reload

**Local inference is now configured in YAML, not scattered across ENV.** Dedicated
`vllm` and `llamacpp` drivers (OpenAI-compatible, on the DeepSeek delegate pattern)
join `ollama-local`, and the whole local-provider matrix — `base_url`, the advertised
context window, header/idle timeouts, enablement — is settable in a bundle's
`providers:` block, with `loomcycle.yaml` and ENV still overriding per the existing
layer order (#1079).

**`POST /v1/_config/reload` reloads a changed config in place** — the retune that used
to mean a restart (a bigger `num_ctx`, a new endpoint, a longer timeout) now takes
effect without dropping in-flight runs. It re-assembles the same layered stack the
server booted from, **validates the candidate before applying** (a typo is rejected
`422` and the running config keeps serving), applies the sections it can apply live,
and reports the rest under `restart_required`; `?dry_run=1` returns the section diff
without applying.

- The endpoint + in-place resolver/provider rebuild (#1080), a config `Holder` that
  makes `user_tiers` / `agents` / `defaults` reload live (#1081), and subsystem
  reloaders for concurrency caps, `scheduled_runs`, and channels (#1086).
- **Section-per-file config**: a `loomcycle.yaml` base auto-layers its
  `loomcycle.*.yaml` siblings (deep-merged, lexical), so a large config splits by
  section — `loomcycle.providers.yaml`, `loomcycle.memory.yaml`, … (#1083, #1084).
- **Re-glob on reload**: adding or removing a config or section file after boot is
  picked up on the next reload — no restart (#1088).
- Deliberately restart-required, with reasons reported: the memory embedder (a
  model/dimension change invalidates every stored vector), skills, the listen
  address, and the store DSN.

### RFC CL — a memory row learns *when*

A key/value memory row used to carry one time — `created_at`, when loomcycle stored
it, which on a bulk import is one clustered instant for the whole corpus and answers
none of the questions people actually ask. It now carries three:

- **`observed_at`** — when the thing was *said or written* (#1085). A new **`when`**
  predicate on `search` / `recall` narrows by it, and `set` takes it to date a row.
  Caller-supplied and never inferred — a guessed date is worse than none, because it
  silently filters the right row out of a window it belongs in. Soft by default:
  undated rows survive the window for the ranker to demote (`missing: prefer`).
- **`valid_at` / `invalid_at`** — when the thing was *true* (#1087). An **`as_of`**
  predicate answers "what was true on the 3rd" exactly, where the observed window
  could only approximate it. `as_of` is always a hard filter — a row valid over a
  different interval is not a weaker answer, it is a wrong one. Half-open
  `[valid_at, invalid_at)` with NULL meaning still true.
- The consolidator now **dates the durable facts it already keeps** (#1089), so
  `as_of` can answer "what medication was I on in April". Scoped honestly to semantic
  (durable-state) memory — it does not attempt episodic "what did I do that day"
  coverage, which belongs in its own RFC.

The adapters gain the memory-search time surface: **`@loomcycle/client` 1.66.0** and
the **Python `loomcycle` 1.66.0**.

### Also

- **fix(store):** a Postgres def-INSERT placeholder-count regression (landed with the
  phase-2a memory change) failed every `agent_def` / `skill_def` / …`_def` create on
  the Postgres store; the def INSERTs are restored to their correct column count
  (#1090). The sqlite path was unaffected.

## What's in v1.65.0

**`recall` told the model every row was a fact, and carried no kind.** Minor rather
than a patch for the same two reasons as v1.64.0: the fix is in the runtime binary,
which a `vX.Y.Z` patch tag does not build, and the shape of `recall`'s result changes
— the array is renamed and each row gains a field.

### It promised a `kind` and never sent one (#1077)

The Memory tool's input schema has always said, of `sources`, that *"each result
carries a matching kind"*. For `search` that is true. For `recall` it was not: the
projection rendered `{id, memory, score}` and nothing else, so a caller could not
tell a consolidator-distilled fact from a remark an agent had jotted down from
document prose. The class was already being computed — the source selector filters on
it — so nothing was missing but carrying it out to the caller.

This is the same defect class as v1.64.0's, which added `kind` to `search` and left
`recall` alone.

An **empty kind stays empty**. A remote memory layer handing back opaque server-side
ids cannot classify its own rows, and defaulting those to `"fact"` would recreate the
very thing being fixed, so the field is omitted instead.

### And the array called them all facts anyway

Recall's default admits notes as well as facts, and on a corpus of raw ingested turns
EVERY row is a note — so `facts[]` asserted, of every result, a status most of them
did not have.

Not cosmetic. The LoCoMo answer axis run twice over one corpus (1,535 questions, same
judge, same answerer model, 1,524 graded by both), changing only which op the answerer
called:

| slice | `op=search` | `op=recall` | delta |
|---|---|---|---|
| **overall** | **0.6906** | **0.6692** | **-0.0214** |
| single-hop | 0.7873 | 0.7873 | 0.0000 |
| multi-hop | 0.5344 | 0.5283 | -0.0060 |
| temporal | 0.6589 | 0.5727 | **-0.0862** |
| open-domain | 0.3571 | 0.3258 | -0.0313 |

**Retrieval was identical between the two.** Probed in-band with the same query, both
ops returned the same ten keys, in the same order, with the same scores — expected
here, because with no consolidation pass every row is a note, so recall's facts+notes
default and search's unfiltered scan select the same set (`facts_written=0` in both
runs). The entire gap is the projection the model reads.

It concentrates in temporal questions, and the failure has one shape: of 43 temporal
answers that went correct to wrong, 29 reported the timestamp of the UTTERANCE as the
date of the EVENT.

```
"When did Caroline go to the LGBTQ support group?"    gold: 7 May 2023
  through search:  "7 May 2023 (she went 'yesterday' on 8 May)"   correct
  through recall:  "8 May 2023"                                   wrong
```

Reading a dated remark as a standing fact is what "facts" invites. The other 14 were
abstentions (NOT_FOUND 0.134 → 0.167).

So the array is now `memories`, each row carries its `kind`, and the op's description
says what a remembered remark IS: something recorded, not something established, often
stamped with the time it was SAID rather than the time it happened.

### Upgrade note

`recall` returns `{"memories": [{id, memory, score, kind}]}` where it previously
returned `{"facts": [{id, memory, score}]}`. Anything reading `.facts` off a recall
result needs `.memories`.

There is deliberately **no compatibility alias on the wire**: this is model-visible
tool output, and emitting both names would leave the misleading one in front of the
model, which is the whole point of the change. The one in-tree consumer — the code-js
consolidator's `recall()` — reads `memories || facts`, because a code-js body replays
against tool results recorded earlier in the SAME run, so a pass straddling the
upgrade would otherwise read undefined, recall nothing, and write a duplicate instead
of merging.

`Document op=list_facts` is untouched and still returns `facts` — those are facts.

### Measured after the fact — it held

The figures above are from the OLD projection; this section originally said the
recovery was a hypothesis, not a result. It has since been run. Same corpus, same
answerer def, same judge, same model — only the projection differs:

| slice | `op=search` | recall (old) | **recall (v1.65.0)** | vs old | vs search |
|---|---|---|---|---|---|
| **overall** | 0.6906 | 0.6692 | **0.6915** | **+0.0224** | +0.0009 |
| single-hop | 0.7873 | 0.7873 | 0.7860 | -0.0013 | -0.0013 |
| multi-hop | 0.5344 | 0.5283 | 0.5356 | +0.0073 | +0.0012 |
| **temporal** | 0.6589 | 0.5727 | **0.6701** | **+0.0974** | +0.0112 |
| open-domain | 0.3571 | 0.3258 | 0.3678 | +0.0420 | +0.0107 |

1,535 questions, 1,454 graded. `recall` went from 2.2pp behind `search` to level with
it, and every category is now within about one question of `search` — the gap is
closed, not narrowed.

The recovery landed where the diagnosis said it would. Temporal gained 9.7pp;
single-hop, which does not depend on resolving a date against an utterance time,
stayed flat at -0.13pp. A uniform lift across categories would have been WEAKER
evidence — it would have suggested some general effect rather than the specific
mechanism claimed. Of the 43 temporal questions that `search` answered correctly and
the old projection got wrong, 30 are now correct, 12 still wrong, 1 unparsed.
Abstentions fell 0.167 to 0.158.

### What the remaining 12 are, and they are not this bug

Investigated, because "mostly fixed" is not a finding. They split in two, neither of
which the projection can reach:

**Five are the deictic step failing at the last inch.** The model now demonstrably
UNDERSTANDS the offset and still answers the wrong date — one reply reads *"8 May 2023
(she went 'yesterday,' stated 8 May)"*, which states the correct reasoning and then
emits the speaking date anyway. Nothing about the row's framing is misleading it any
more; it is an instruction-following failure at the final-answer step, and it belongs
to whatever prompt is asking the question.

**Seven need a time PREDICATE, which this memory plane does not have.** They ask
things like *"which city was Calvin at on October 3, 2023"* or *"what was Dave doing
in the first weekend of October 2023"*. Cosine similarity over prose cannot answer
that: the embedding of a date-constrained question does not retrieve the turn that
happens to carry that timestamp, because dates are not semantically encoded. Five
abstained and two answered confidently wrong, which is the worse failure.

That splits cleanly in the aggregate, across all 294 graded temporal questions and not
just the 12:

| temporal question shape | n | accuracy | abstention |
|---|---|---|---|
| carries an absolute date / window constraint | 31 | 0.5484 | 0.323 |
| topic-shaped | 263 | 0.6844 | 0.171 |

So a date-constrained question is roughly 14pp harder and abstains about twice as
often. The machinery for it already exists elsewhere in the runtime — the bi-temporal
entity sidecar's `valid_at` / `invalid_at` and `graph_recall`'s `as_of` predicate — but
the k/v memory plane's `recall` and `search` expose no date filter at all. Closing
that is a feature, not a fix, and wants its own RFC.

## What's in v1.64.0

**`recall` could not see an agent's own notes.** Minor rather than a patch for two
reasons: the fix is in the runtime binary, which a `vX.Y.Z` patch tag does not
build, and a bare `recall` now returns notes alongside facts — a visible behaviour
change for anything reading it.

### Two defects, either of which alone empties the result (#1075)

`Memory op=recall` returned NOTHING on a scope holding 419 embedded rows, while
`op=search` returned three hits for the same query in the same partition:

```
search sources=[notes]        → 3 results (cosine 0.546, 0.539…)
recall sources=[notes]        → 0
recall sources=[facts,notes]  → 0
recall (no sources)           → 0
```

**The in-band parser silently dropped `notes`.** `parseSources` had cases for
facts and documents and none for notes — while the op's OWN input-schema enum
advertises all three. Unknown values are dropped rather than rejected by design
(rejecting would break an older runtime against a newer value name, which is the
right call), so an explicit `sources:["notes"]` became NO selector. One cause, two
opposite symptoms: `search` widened to everything and looked correct, `recall`
fell through to its default and returned nothing. The HTTP parser
(`parseMemorySources`) had always handled notes, so the same selector meant
different things depending on which surface a caller used.

**Recall's default excluded notes, contradicting its own schema.**
`inprocess.Recall` defaulted to facts alone; the schema promises "facts+notes". A
row written with `set` — or with an off-run PUT — carries no provenance, so
`ClassifyMemoryRow` calls it a NOTE, and the default hid every one of them. The
facts/notes split landed after that default was written and the line was never
revisited: before the split, "facts" WAS the whole of an agent's own memory.
Documents stay excluded, which is the separate and still-correct reason the
default exists at all (a horizontal rule outranking the fact holding an answer).

The `SourceFacts` doc comment still described facts as including notes; reading it
that way is what made the default look intentional. Corrected.

### How it surfaced, and what it was costing

The LoCoMo answer axis. An answerer whose prompt says `op=recall` scored **0.0000
with a 94% abstention rate** over 419 embedded conversation turns — it was finding
nothing, not answering wrongly. Pointed at `op=search`, which by accident of the
first defect applied no filter at all, the same store, same embedder and same
questions scored **0.7353 with zero abstentions**.

So the practical cost was: any agent following the documented advice to read its
own memory with `recall` saw only what a consolidator had distilled, and nothing
it had written itself.

### The fixture could not have caught it, which is also fixed

The inprocess test double honoured only `filter.KeyPrefix`. It ignored
`filter.Provenance` and `filter.ExcludeKeyPrefix` and never populated
`MemorySearchEntry.Origin`, so NO test in that package could distinguish a fact
from a note or exclude a document — source filtering was structurally untestable
there. The double now records each row's origin on the way in (the real stores
keep it in a column `MemoryEntry` does not expose, so a double reading rows back
cannot recover it), applies both filter dimensions, and carries `Origin` through.

That mattered immediately: the fail-before for the second defect initially PASSED,
because the assertion could not observe the thing it named. Making the double
faithful is what turned it into a regression test.

### Adapters

None. The fix is runtime-internal, so `@loomcycle/client` and the Python package
stay at 1.61.0 and `@loomcycle/library` at 0.3.0.

## What's in v1.63.0

**A curator that never ran, and the two agent fields you could not set without
curl.** Minor: the changes are in the runtime binary and the embedded Web UI,
neither of which a `vX.Y.Z` patch tag builds.

### memory/ontologist named its own tool in lowercase and never ran (#1072)

It failed its FIRST tool call on every run, then spent the rest of its budget
reasoning about which tools it had. A live pass: `tool not found: document`,
followed by 1,937 output tokens concluding — wrongly — that the Document tool was
missing from its environment, and inventing two tools
(`generate_tool_suggestion`, `search`) that exist nowhere in loomcycle.

The def was fine. It grants `tools: [Document]`. The PROMPT told the model to call
`document` — lowercase, three times — and tool dispatch is an exact-match map
lookup (`tools.Dispatcher.Execute` does `d.tools[name]`), so the call never
reached the tool and never could.

Nothing caught it because the two halves are checked by different things and
neither checks the pair: the ACL is validated at config load, the prose is not
validated at all. The agent looked correctly configured in every listing, and the
only symptom was a curator producing confident nonsense.

`TestBundlePrompts_NameToolsExactly` now scans every bundle agent's system prompt
for the backticked `` `name` op=… `` form the bundles use to teach a tool call and
asserts the name is one that agent is granted. Scoped to that form rather than to
every mention of a word, because "ordinary document chunks" is legitimate prose in
the same paragraph — and it fails when the pattern matches nothing, so it cannot
pass by examining zero instructions.

### The agent modal can set max_context_tokens and internal (#1073)

Both round-trip the AgentDef create/fork overlay and neither was reachable from
the Library modal, so setting either meant hand-rolling an API call.

`max_context_tokens` sits one row from `max_tokens` and they are trivially
confusable with different consequences — one truncates the reply, the other
truncates the prompt — so both inputs carry a hint. The hint leads with what
decides whether you want it: on a local model this becomes THAT agent's own
`num_ctx`, so a smaller window is a cheaper and faster call; on a cloud model it
can only lower the effective window, never raise it.

`internal` is a checkbox that says ONE-WAY, because `applyOverlay` does
`if ov.Internal { d.Internal = true }`. A plain checkbox would let an operator
untick it, fork, and get an agent that is still internal with nothing saying so.
The overlay emits the key only when true, mirroring that merge instead of sending
a `false` the server ignores.

Also fixed the text that hid both: the overlay's own schema `description` listed
neither, so an MCP caller reading the tool schema could not discover them either.
That description still documents only 24 of the overlay's 46 fields — the 22 it
omits include `sampling`, `compaction`, `volumes`, `sql_scopes`,
`memory_consolidation` and the four `*_def_scopes` gates. Left for its own change,
because a coverage test would fail on twenty fields unrelated to this one.

### Packages

`@loomcycle/library` 0.3.0 (the modal change), published from its own
`library-v0.3.0` tag. The Web UI embedded in the binary compiles that source
directly, so the runtime carries the change either way. `@loomcycle/client` and
the Python adapter are unchanged at 1.61.0.

## What's in v1.62.0

**Three ways a consolidation pass wasted a deployment's time, all found by running
one.** Minor: the changes are in the runtime binary and the embedded bundle, neither
of which a `vX.Y.Z` patch tag builds. No adapter or wire change.

The occasion was a LoCoMo memory benchmark on a local-model deployment. Every item
below is measured from that run's event log rather than reasoned about.

### A killed pass no longer strands its lease (#1069)

The consolidation pass releases its lease in a `finally`, which covers every path
its own code can take — and none of the paths where the **run** is killed out from
under it. The owner of a lease is the run id and `cursor_release` is
ownership-scoped, so a dead run can never hand its lease back and nothing else is
permitted to. The target stayed leased until the TTL elapsed.

What that looked like: a pass took the lease at 14:21:46 and spawned an extractor
child that never returned — neither run emitted a `done` event in the following two
hours. The parent hung on that one call, exhausted its 25-minute
`run_timeout_seconds`, and was killed before reaching the `finally`; its 30-minute
lease then sat until 14:51:46. The harness polls every three minutes, so **ten
consecutive passes** bailed with "target busy, nothing read, nothing written" across
a window where nothing was running at all.

Note the shape: `run_timeout_seconds` is deliberately *below* `lease_ttl_ms` so a
live pass can never be stolen from — which means a killed pass was *guaranteed* to
leave a stranded lease, every time.

`MemoryCursorReleaseByOwner(owner)` now frees a run's lease from
`finishRunWithCancel`, the terminal path for every run, as a deferred best-effort
hook. Owner-only with no tenant argument, because a run id is globally unique and a
dead run cannot say which target it had leased. An empty owner is a no-op, since
`''` is what an *unleased* row stores in `leased_by`. The TTL keeps its job as the
backstop for a process that dies without reaching any cleanup; what is gone is the
case where the runtime *knew* the run was over and let the lease sit anyway.

### The queue no longer takes a whole pass per batch, and one bad item stops blocking the rest (#1070)

`consolidatePending` took exactly one extractor call's worth of queued items and
deferred the remainder, so throughput was **one batch per pass** however long the
queue was — a 419-turn conversation measured ten-to-sixteen passes and about an
hour. The items are independent; nothing about correctness required stopping after
one call, only the absence of a loop. Now bounded by `max_pending_calls` (4),
because a pass still has to finish inside `run_timeout_seconds`.

The second half was a **livelock**, not slowness. An empty extraction left the whole
batch queued — right in itself, because an ack is the one irreversible step here and
an empty reply cannot distinguish "these turns hold nothing durable" from "the model
glitched". But it also meant a batch the extractor keeps answering empty for was
re-drained every pass, spent a call, acked nothing, and blocked everything behind it
forever. Observed live as ten consecutive replies of **two output tokens** with the
queue head never moving, and it is why a pre-ingest drain spent ~70 minutes without
reaching the run's own data.

The fix does **not** ack on empty. An empty multi-item batch narrows to its head (a
multi-item batch does not say which item the model could not read), and a single
item that still yields nothing is **stepped over**: left queued for a later pass
while this pass continues with what is behind it. The blockage is gone, no input is
traded for throughput, and a permanently unreadable item settles at one call per
pass instead of every call forever. It is reported, because a silent step-over looks
exactly like a pass that spent its budget and moved nothing.

Worth recording: the first version of this *did* ack a single item that came back
empty, reasoning that the model had examined it alone — the same evidence the chat
path accepts when it lets an empty chat move the watermark.
`TestConsolidator_EmptyBatchReplyLeavesTheQueuedItemsQueued` refused it, correctly:
a transient extractor glitch would have dropped those turns permanently. Step-over
solves the blocking without that trade, and the test passes unmodified.

### The consolidator's children ask for the window they actually need (#1070)

They ran at whatever window the `ollama-local` registration was pinned to, because
until v1.61.0 that was the only knob. RFC CJ made it per-agent and the Ollama driver
prefers `Request.MaxContextTokens` over the construction-time `num_ctx`, so each
child can now size its own — and on a local deployment the KV cache is allocated per
request, so a smaller window is a cheaper and faster call.

Sized from observed input tokens: **`memory/extractor` 16384** (a real extraction
measured 3,826 input tokens; its prompt budget is `max_part_chars`, 12,000 chars ≈
3–4k tokens, plus the injected ontology), **`memory/judge` and
`memory/conflict-judge` 8192** (715–731 input tokens across a live batch of eight),
**`memory/ontologist` 32768** (it surveys the store rather than reading one supplied
text; a live pass ran 86k across seventeen calls).

The extractor's *window* and its *prompt budget* bound different failures, so raising
`max_part_chars` to fill the window would be wrong — 12,000 is measured against the
model, where a 21,635-char prompt came back empty while 15,684 and 15,599 extracted
cleanly.

### Not changed, deliberately

The `lease_ttl_ms` / `run_timeout_seconds` ratio (30 and 25 minutes against a
four-minute pass) still looks generous. #1069 removes the wedge that made it hurt,
and #1070 makes a pass *longer* — up to four extractor calls — so tightening the
budget now would cause more kills rather than fewer. Worth revisiting once the new
pass duration has been measured.

### Adapters

None. Both changes are runtime-internal, so `@loomcycle/client` and the Python
package stay at 1.61.0.

## What's in v1.61.0

**Size each agent's context window — per agent and per run.** Minor rather than a
patch: the changes land in the runtime binary and the adapters, neither of which a
`vX.Y.Z` patch tag builds. It completes RFC CJ across both phases, adds a
memory-retrieval benchmark harness, and fixes a load-dependent test flake.

### A context window per agent (#1065)

One Ollama host served every local agent the same context window — the global
`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` (or a per-provider `options.num_ctx`), or, unset,
Ollama's silent 4096-token floor. A fetch-heavy `researcher` that needed 128K and a
`chat` agent that wanted 16K could not coexist on one registration without standing
up a second provider.

A per-agent `max_context_tokens` now sizes the window for **that agent only**. On a
local (Ollama) model it is sent as `options.num_ctx`, winning over both the env knob
and any per-provider value; the driver reports the source as `(request)`. On a cloud
model — where the window is fixed by the model — it instead caps the agent's
*effective* window (the compaction budget the context gauge and `autocompact_at_pct`
read), clamped to the model's maximum, so it can only lower, never enlarge. It is
distinct from `max_tokens` (the output cap), content-identifying (a fork that changes
it mints a new `content_sha256`), and flows down the spawn tree. `0`/unset is
byte-identical to before.

### Per-run override, op=self visibility, adapter parity (#1067)

The same knob became a per-**run** override, mirroring the existing per-run
`sampling`/`compaction` overrides exactly: a value on `POST /v1/runs` (or a
continuation), on the gRPC `RunRequest`/`ContinueRequest`, on the MCP `spawn_run` /
`spawn_runs` and the HTTP `/v1/runs:batch` fan-out, and on both adapters. A per-run
value `> 0` wins over the agent's own; `0`/absent inherits it (which itself falls
through to the provider/driver default). Because a window is never meaningfully `0`,
absence and zero coincide — so it is a plain scalar (a new `config.MergeMaxContextTokens`
helper beside `MergeSampling`/`MergeCompaction`) and a plain `int32` on the wire, not
a proto3 `optional`. Sub-agent spawns and resumed runs deliberately use the agent-def
value only — a per-run override is neither inherited by children nor snapshotted, the
same posture per-run sampling takes at those two sites.

An agent can now read its resolved cap for its own run via `Context op=self`
(`max_context_tokens`), reported even before the first turn; the *effective* window
after the first turn continues to appear there as `context.max_tokens`.

One pre-existing gap fell out of this: the MCP streaming-spawn path was silently
dropping per-run `sampling` **and** `compaction` on the way to the run input. It now
carries all three overrides, like the blocking path.

### A memory-retrieval benchmark harness (#1064)

`bench/cmd/locomo` adds a convert / ingest / search / all / purge harness over the
LoCoMo long-conversation QA set, so memory-retrieval quality (recall@k) can be
measured against a fixed corpus rather than by feel — the `qa.evidence` dialogue ids
give free retrieval ground truth. It needs pgvector (the SQLite tier has no vectors),
and the corpus is CC BY-NC, so it is never vendored.

### De-flaked the deferred-channel store contract (#1066)

Two deferred-channel store-contract subtests asserted a message was hidden before its
`visible_at` using a 150 ms window; on a loaded `-race` CI runner the window elapsed
before the assertion ran, producing a spurious "visible too early" failure. The
windows are widened to 2 s. Test-only.

### Adapters

Both bump to **1.61.0**. `@loomcycle/client` gains `maxContextTokens` on the run and
continue options (serialized as `max_context_tokens`); the Python package gains a
`max_context_tokens` argument mapped onto the gRPC request. Additive — existing
callers are unchanged.

## What's in v1.60.0

**A fact that contradicts a stored one is now noticed.** Minor rather than a patch
because the change lives in the runtime binary and the embedded bundle, which a
`vX.Y.Z` patch tag does not build. One feature, and it deliberately writes nothing.

### Conflict detection, report-only (#1062)

A fact that contradicted one already in the store did not retire it. Both persisted,
both came back from recall, and nothing marked either as suspect — the failure a
neutral survey of 2026 memory systems calls *hallucinations of the past* and
attributes specifically to append-only fact stores.

Retirement did exist, but it was reached from exactly **one** place. `applyOne` fills
`supersede_queue` only from near-duplicate collapse, where a neighbour qualifies on
cosine similarity plus subject overlap. So the decision to retire a fact was made
entirely by **similarity**, and nothing anywhere asked whether two claims can both be
true. That cuts both ways: two facts that genuinely conflict but score below the merge
band both survive and both recall, while two near-duplicates that do *not* conflict
risk being collapsed into one.

Now, for each fact written, the neighbours in the "related but distinct" band — at or
above `related_threshold`, below `merge_threshold` — go to a new tool-less judge as
**pairs**, which answers `contradicts` / `independent` / `unclear`.

**It writes nothing.** Every verdict is counted, reported and discarded:

```
conflict candidates 3, judged 3 (1 would be retired as contradicted,
2 independent, 0 unclear) — reporting only, nothing was changed
```

That is the design rather than a limitation. The band has never been measured against
real conflicts, and a detector that retires facts on an unmeasured threshold is the
failure the merge band already taught this pipeline about once. The report says what
enforcement *would* do so an operator can calibrate first, and acting on a verdict
will require **its own key** — a deployment that turns detection on today cannot start
retiring facts on a later upgrade without a second, separate decision.

Turn it on with `memory.consolidation.detect_conflicts: true`. Off by default, read
through the capabilities report like `verify_writes` and the similarity bands, and
bounded at four extra judge calls per pass.

### Three things this needed less of than the design expected

- **No new threshold.** `related_threshold` has been in the operator config *and* the
  capabilities report all along, documented as exactly this band's lower edge — it
  outlived the code that used to read it. That code appended `" (related: …)"` to the
  fact TEXT and was removed because the tails nested into the stored value and
  therefore into the embedding, defeating the very merge band they were meant to
  inform. Its epitaph — *"there is nowhere structured to put the linkage"* — is what
  stopped being true: the entity sidecar carries `invalid_at` / `expired_at` /
  `judged_by`, and supersede records which chunk replaced which.
- **No new wire op.** The write path enforcement will need already exists.
- **One Go field.** `detect_conflicts`, beside `verify_writes`.

### One deliberate asymmetry

The subject-overlap gate is **not** applied to conflict candidates, though the merge
path uses it two lines above. There it stops a destructive merge resting on a single
similarity number, and it earns its place. Here it would defeat the purpose: the
reason to ask a model at all is that lexical overlap misses real conflicts — it scores
**0.000** for a non-Latin paraphrase pair, because the tokenizer only sees `[a-z0-9]`,
and around 0.5 for two different-language facts sharing nothing but a brand name.
Screening candidates with the signal whose blind spots this exists to cover would
inherit every one of them. A test with a Cyrillic neighbour fails if the gate returns.

The **class** refusal does carry over, including the half that refuses a key the pass
cannot parse — an opaque row from a remote backend, or one a user wrote themselves.
Detection writes nothing today, so that half is not load-bearing yet; it is enforced
from the start so enforcement needs no widening later.

### A separate judge agent

`memory/conflict-judge` is a new tool-less agent on the same tier as the entailment
judge, not a second question added to it. That judge's system prompt is deliberately
single-purpose and its tier choice rests on a measured eval; a differently-shaped
question in the same prompt would put that result in doubt every time either question
changed. Two single-purpose agents cost one extra def.

### Adapters

None. The flag is yaml, and the capabilities payload is untyped in both adapters, so
`@loomcycle/client` and the Python package are unchanged at 1.58.0 — as at v1.59.0.

## What's in v1.59.0

**The document viewer surfaces the document id and every chunk id, each with a
copy button.** Minor rather than a patch: the Web UI is embedded in the runtime
binary, and a `vX.Y.Z` patch tag builds only the browser sidecar — so a patch
could not ship a UI change.

### Copy document + chunk ids from the viewer (#1060)

Wiring a document or a specific chunk into an external workflow — n8n over the
MCP/HTTP API — needs the exact `document_id` / chunk id the API expects. The
viewer used those ids only internally (React keys, data-layer calls) and never
displayed them, so there was no way to grab one from the UI.

The `@loomcycle/explorer` document viewer now surfaces them, each with a
click-to-copy button:

- the **document id** in the toolbar,
- the **selected chunk id** in the content head,
- a compact copy on **every chunk-tree row** (faint until the row is hovered or
  selected, so the tree keeps its clean scan-line).

The on-screen id is monospace and ellipsized, but the clipboard always receives
the FULL id — a long UUID displays compactly yet copies completely. The copy
click never selects the chunk or toggles the tree, and a failed copy (an
insecure context with no clipboard API) degrades to a `✗` rather than throwing.

## What's in v1.58.0

**A search over the document store now says where its results came from, and an
embedder migration runs in batches.** Minor rather than a patch: the search response
gains two wire fields, and a `vX.Y.Z` patch tag builds only the browser sidecar —
both changes here live in the runtime binary, so a patch tag could not ship either.

### A document search hit names its document and heading (#1058)

A semantic search that spans the doc store returned prose addressed only by an opaque
`doc.chunk:<32 hex>` key. The id is the right ADDRESS — stable, unique, what
`get_chunk` takes — but it is not an identity. A live query for *"the judge withholds
a fact whose span does not support it"* returned six hits, all six of them the right
document, and nothing in the response said so:

```
doc.chunk:2f51f960973ec940301a4d4aebcf35ff
doc.chunk:f5e7492bc3f2065dad0272c969d8beb0
…
```

Anything wanting to cite a result had to fetch each row just to learn its heading.

**Keying by title instead was the tempting fix and is wrong**: titles are not unique,
not stable under a rename, and not addressable. So the key stays and the hit carries
its readable identity beside it — `document` (the document's title) and `title` (the
chunk's own heading), both additive and `omitempty`, on the off-run
`POST /v1/_memory/search` and the in-band `Memory op=search` alike.

The in-band surface also gains **`chunk_id`**, which it never had. The two projections
are hand-maintained copies, so an agent could be shown a document body it had no way
to fetch, while an operator on the HTTP endpoint got the id — a five-release drift
that only became visible once both were touched at once.

Labels resolve in ONE batched query per search, and are **best-effort by
construction**: they live in SQL Memory, a different plane from the bodies, so a
deployment without it, an unkeyable scope, a store fault, or a chunk deleted
mid-search each cost the label and never the result. The resolver owns the
Memory→SQL Memory scope mapping so neither caller restates it — the two planes key the
*tenant* scope differently (`""` against the tenant itself), and a restated rule is
how that axis drifts. A drift test compares the mapping against
`Document.resolveScope` directly, because a divergence there resolves against an empty
schema and the labels simply stop appearing, which is indistinguishable from "no
labels available".

The Memory console names a hit `document › heading` instead of printing a 32-hex id,
keeping the id on the tooltip. A document's root chunk carries the document's own
title, so the two are collapsed rather than rendered as `Verified writes › Verified
writes`.

One assertion in this change was **found vacuous and replaced**, which is worth
recording because the failure mode is not the usual one. To prove the resolver
deduplicates chunk ids, the test asked for each id twice and checked the label count
— and it passed with the dedup pass deleted, because `WHERE id IN (a,a,b,b)` returns
one row per DISTINCT id regardless: SQL `IN` has set semantics, so the assertion could
not see the difference. Nothing about the representation was misread; the layer below
already provided the property. The replacement pins a consequence only this code
controls — ids are deduplicated BEFORE the lookup is capped, so a page of hits from
one document cannot crowd out a different chunk's label.

### reembed embeds in batches, not one call per row (#1057)

A real embedder migration measured **~12 rows/minute**: 3,633 document-chunk rows
moving from a 768d model to a 1024d one, about five hours, because the loop called
`Embed` once per row and every row paid a fresh HTTP round trip and prefill setup
against a local Ollama. The `Embedder` interface has always been batch-shaped (N texts
in, N vectors out, chunked again by the driver's own batch size), so the per-row call
was leaving that on the floor.

Rows now reach the embedder in batches of 64 — two orders of magnitude fewer round
trips — while the STORE WRITE stays per row. That is what keeps the operation
resumable: a client timeout or a cancelled context mid-sweep costs only the current
batch, and the next call picks up what is left. It is also how an operator paginates a
scope too large for one request, so it is preserved deliberately. Not "all of them" in
one call, either: a single `Embed` over a whole 1000-row page would make one failure
cost the page, and document bodies can be large.

**A batch error falls back to per row.** One unembeddable row must not cost its
batch-mates: before batching, a bad row was counted and skipped while the rest
migrated, and that accounting is what an operator reads to decide whether a sweep is
done. Retrying singly restores it exactly — and as a side effect a transient one-shot
fault now recovers instead of stranding a row. The store write moved into one shared
helper so the batch path and its fallback cannot drift in what they record, and the
recorded dimension stays the OBSERVED width of the returned vector rather than the
embedder's advertised one (a driver that cannot know its own dimension answers 0, and
a row written with 0 makes every later search in that scope report a spurious
mismatch).

The existing partial-failure test had to be **reworked, not extended**: it asserted
the handler's CALL PATTERN rather than a guarantee. Its stub failed "the next call",
which under batching describes exactly the transient fault the fallback now retries,
so it reported 2 reembedded where it wanted 1 and 1. The stub now models a
permanently bad ROW and the test asserts the contract — an unembeddable row is
reported in `failed` and `failed_keys`, never silently dropped.

**Measured on the deployment that prompted it**: the migration that had been running
at ~12 rows/minute completed a 3,650-row scope, which now reports a single embedder at
a single width.

### Adapters

`@loomcycle/client` **1.58.0** carries the two new optional fields on
`MemorySearchEntry`. `@loomcycle/memory-view` **0.4.0** renders them. The Python
adapter is republished at **1.58.0** to keep the install-name version aligned with the
runtime; it has no changes this release.

## What's in v1.57.1

**An embedder migration could not reach the tenant holding the data.** Patch — one
server-side fix; no wire, schema or adapter change.

### reembed ignored ?tenant=, and reported a truthful-looking zero

`POST /v1/_memory/reembed` took the tenant from the caller's principal ALONE. An
operator migrating another tenant's scope after an embedder change therefore swept their
OWN partition, found nothing, and got `rows_total: 0` — indistinguishable from "already
migrated".

Observed on a live deployment mid-migration from `embeddinggemma` (768d) to `bge-m3`
(1024d): a legacy admin token carries no tenant, so every call resolved to the default
partition and reported zero, while `embed_stats` for the real tenant showed **3,633 rows
still on the old embedder against 17 on the new**. The console's own reembed plan showed
the same zero for the same reason.

What makes that worse than a wrong number: once ANY row of the new dimension exists, the
search-time dimension pre-check (which samples one row) stops firing. Semantic search
then returns only the post-switch slice, with plausible scores, instead of failing
loudly — so the operator has no signal that 99.5% of the index has gone unreachable.

`principalTenantScope` now resolves the tenant, so `?tenant=` reaches both the read and
the write-back, and an authenticated admin who names none is REFUSED rather than
defaulted — a reembed writes, and spends one embedder call per row, so sweeping an
unnamed partition is not merely a misleading count.

**The refusal is narrower than its siblings, deliberately.** `principalTenantScope`
reports all=true for a request with no principal, which is every request on an open-mode
deployment — and an open-mode install has no tenants, so demanding one there would break
a route that has worked since v0.9.0 to buy no safety. It fires only for an authenticated
admin; all five pre-existing reembed tests pass unchanged.

This was the third variant of one defect. `embed_stats` was already fixed to honour
`?tenant=`, and `backfill_embeddings`, `purge_stale_embeddings`, erasure, directory and
orphan-repair all guard the unnamed-tenant case. reembed was the one sibling with
neither.

**Why it survived:** the test double was blind to the tenant in three places —
`vectorAdminStore` dropped the tenant argument on three embed methods and keyed rows on
`scope|scope_id` alone, its list method joined the k/v row with a hardcoded empty tenant,
and its stats method prefixed without one. A double that ignores the dimension under test
cannot fail a bug in it. All three now carry the tenant through a shared `embedKey`.

### Release mechanics

A `vX.Y.Z` patch tag builds only the browser sidecar, and this fix is in the runtime
binary — so this release was published with `force_full=true` on the release workflow to
produce binaries and images. goreleaser's `mode: keep-existing` preserves the
annotated-tag notes the lean job pre-created.

## What's in v1.57.0

**Two defects that reached an operator, and a signal for the failure neither of them
could report.** Minor rather than a patch for two reasons: the change feed gains a new
wire field, and a `vX.Y.Z` patch tag builds only the browser sidecar — the fix that
matters here is embedded in the runtime binary, so it needs a full build to ship at all.

### The console's Backfill and Purge buttons threw (#1052)

Pressing either in the Memory console produced `e.backfillEmbeddings is not a function`.
Both methods landed in `@loomcycle/client` at 1.55.0; `web/package.json` depended on
`^1.49.0`.

**Why nothing caught it** is the part worth knowing. `@loomcycle/memory-view` compiles
from SOURCE through a Vite alias, and `web/vite.config.ts` lists `@loomcycle/client` in
`resolve.dedupe` — added, per its own comment, "for good measure — a duplicate is
wasteful even if not fatal". Dedupe makes web's copy the one that lands in the BUNDLE,
which turned a wasteful duplicate into a version DOWNGRADE. Both plausible guards are
structurally blind to it: `tsc --noEmit` (which web's build does run) resolves the
client from the memory-view source file — the package's own, correct copy — and
typechecks clean against a version the bundle will not contain; `vite build` strips
types and checks nothing. Green in CI and in `make build-ui`, broken only in a browser
at the moment a button was pressed.

`reembed` kept working throughout, because `reembedMemory` has existed since 1.49.0 —
which is why three buttons failed in two ways and looked like three unrelated bugs.

Fixed by bumping web to `^1.56.0`, verified by asserting the built bundle contains the
methods rather than by inference. A new **`TestWebDedupedDeps_SatisfySourcePackagePeers`**
makes it non-recurring: for every package web consumes from source, a peer dependency
that is also in vite's dedupe list must be satisfied by web's own range. It is a Go test
because `go test ./...` is the gate everyone runs and it needs no `node_modules`, and it
parses the dedupe list from the real `vite.config.ts` so removing a name there relaxes
the test honestly.

Also fixed alongside: `purge_stale_embeddings` advertised `agent, user, tenant` in its
validator and then demanded `scope_id` unconditionally, refusing the tenant scope its
siblings accept (the tenant keyspace is one partition with an empty store scope_id).
**Latent** — the client never had the method, so that request never reached the server.

### Non-Latin facts collided on one memory key (#1047, #1053)

`rawWords` tokenises on `/[^a-z0-9]+/`, so a fact in Cyrillic, Greek, Hebrew or any CJK
script reduced to no words and `slug()` fell back to the constant `"unnamed"`. `factKey`
is the consolidation pass's idempotency mechanism and the write path upserts on it, so
the SECOND such fact silently overwrote the first: a scope's entire non-Latin population
collapsed onto ONE row. Confirmed against the real engine — two different Ukrainian facts
both keyed `memory/fact/unnamed`. The entity-tier mirror used the same string as its
`natural_key` and collided identically.

An empty slug now appends a fingerprint of the text: a pure function of it (or a re-run
of the pass would duplicate rather than converge), two 32-bit hashes rather than one (a
single space collides at roughly a percent by ten thousand facts, and a collision is the
failure being fixed), shift-and-add only (the engine is ES5.1-era: no `Math.imul`, and a
32-bit multiply loses precision past 2^53), and base36 so the key stays inside
`[a-z0-9-]` — it is interpolated into SQL by the lookup path.

**It does not widen the tokeniser.** A non-Latin fact still derives no source span, so it
stores unverified-and-visible rather than verified. That change redefines what a "word"
is and needs the `merge_min_subject_overlap` floor re-measured against its labelled
pairs; it is deliberately separate. This stops the data loss.

⚠️ **It cannot recover what was already overwritten.** On a store that held several
non-Latin facts, only the last survives, and this release prevents further loss rather
than restoring the earlier ones.

### The change feed says what the embedder is doing (#1051)

An embedder outage is invisible at every call site, by design: a failed embedding on a
content write is not fatal — the body is stored and the embedding skipped with a log line
— because losing an author's text to an unreachable embedder is worse than losing its
searchability. The consequence is that writes succeed, change frames keep arriving, and
search quietly stops finding anything written during the outage. A reader watching the
Activity tab to answer "is the memory pipeline healthy" saw a busy feed and concluded yes.

The opening `feed` frame now carries an additive `embedder` block: `state`
(`absent` | `untried` | `ok` | `failing`), provider, model, calls, failures,
`last_failure_kind`, and the last ok/failure stamps. Not a probe — a probe would cost a
model call per reader and describe one instant; a new `providers.ObservedEmbedder`
decorator records what the traffic that already went through actually did, so the answer
cannot disagree with reality (the same argument as `cdc.Store.CapturesChanges()`).

Four decisions with a plausible wrong answer, each pinned by a test: `untried` is its own
state, because a configured-but-never-called embedder must not report `ok` — that is the
state a freshly booted deployment is in; the failure COUNT is reported, not a boolean,
because it is roughly how many rows need `backfill_embeddings`; no error TEXT escapes,
since a transport failure reads `dial tcp 192.168.0.77:11434: …`, a map of the operator's
network, so failures are a classified kind matched by `errors.Is`; and it is separate from
`enabled`, because capture being on and the embedder working are independent failures.

`@loomcycle/memory-view` **0.3.1** surfaces it as one sentence for a failing or absent
embedder, and stays silent for `untried`, `ok`, and a runtime too old to send the field —
warning about the normal boot state would train an operator to ignore the only line that
matters when it fires.

### Adapters

`@loomcycle/client` and `loomcycle` (PyPI) go to **1.57.0** as version-aligned lockstep
releases; no client-surface change (the `embedder` block rides an SSE frame the TS client
already forwards, and the fixes are runtime-side). `@loomcycle/memory-view` **0.3.1**
publishes on its own tag.

### Upgrading

Nothing to migrate, and the two operator-visible notes are above: the console fix is
embedded in the binary so it arrives only with this build, and the keyspace fix prevents
further collisions without recovering earlier ones.

## What's in v1.56.1

**A tool-call regression on qwen-via-Ollama, and the v1.56.0 bundle change that caused
it.** Patch — one runtime fix and one config revert; no wire, schema, or adapter change.

### qwen on Ollama stopped calling tools

A `chat/medium` run on a local qwen3.6 model wanted to search but emitted its call as
TEXT, copying the framing of loomcycle's own injected `{{tool:Context.*}}` reference
blocks — `<tool_result> {"type":"function","function":{"name":"WebSearch", …}} </tool_result>`.
Ollama's extractor expects `<tool_call>`, so it recovered nothing, and the existing
text-recovery parser only understood a flat `{name, arguments}` object. The wrapped,
OpenAI-nested shape fell through, and the run ended with the un-executed block as its
"answer" — no search ran.

- The Ollama driver's `tryParseToolCallsFromText` now peels one wrapper tag
  (`<tool_call>` / `<tool_result>` / `<function_call>`, hyphen or underscore, with or
  without attributes) and accepts the OpenAI-nested `{"function":{name,arguments}}`
  envelope, with arguments as an object or a JSON-encoded string. Same strict,
  tools-gated contract — prose and non-tool JSON still recover nothing.

### inject_tool_guide is no longer a bundle default

Making `inject_tool_guide` a bundle default in v1.56.0 was premature: its target is
small local models, and that is exactly where the three injected `<tool-result>`-framed
blocks both bloat the prompt and get copied by the model as its tool-call format. The
flag is removed from every bundled agent; the mechanism — `Context op=guide`, the
`HintedTool` hints, the injectable refs, and the per-agent flag — stays intact for
explicit opt-in on a strong-model agent.

## What's in v1.56.0

**Agents that know how to call their tools, a live view of the memory pipeline, and
three defects the first live verified-writes run turned up.** Minor — two feature
lines and three fixes, two of which change numbers an operator may be watching.

### Agents start with runtime knowledge instead of blind

The loop already sends every tool's full JSON schema on each request, but small and
local models under-attend to that array: they guess which op to call and which fields
are required, then learn from refusals. The system-prompt injector could restate tool
NAMES and nothing about how to use them.

- **`Context op=guide`** returns, for this run's resolved tools, `{name,
  side_effect_class, ops[], required[], hint}`. The ops and required fields are parsed
  from each tool's OWN input schema, so the digest cannot drift from the schema the
  model is being sent; the hint comes from a new optional `tools.HintedTool`,
  implemented on the builtins that produce the most call errors — Memory, Document,
  Path, Channel, Agent, Skill.
- **`{{tool:Context.guide}}` and `{{tool:Context.capabilities}}`** are now injectable
  prompt references (both pure reads, resolvable at assembly time), sorted for
  byte-stable prompt caching.
- **A per-agent `inject_tool_guide`** appends both refs automatically, so an operator
  need not hand-place placeholders. **Default OFF** — every existing custom agent stays
  byte-identical until it opts in, and a flag-OFF agent takes the unchanged fast path.
  Round-tripped through the AgentDef create/fork overlay, the read adapter, the
  MD-frontmatter loader and `content_sha256`, mirroring `memory_protocol`.
- **The bundled LLM agents opt in**: `chat/*`, `doc/manager`, the agent-teams and
  team-examples agents, and `memory/ontologist`. Deliberately not flagged: the doc/team
  sub-agents are SKILLS injected into the flagged agents (the field has no meaning on a
  skill), the `code-js` agents run no prompt, and `memory/extractor` / `memory/judge`
  hold no tools so the guide would render nothing.

### The memory console can watch the pipeline work

RFC CF's last phase. A consolidation pass takes minutes and reports only at the end, so
there was nothing to look at in between.

- **Both change feeds now open with a `feed` frame** stating whether writes are actually
  being captured, plus the cursor in force. This closes a real trap: with
  `LOOMCYCLE_MEMORY_CHANGES_ENABLED` unset nothing writes the change table, so the
  stream connects, keepalives flow, and no frame ever arrives — indistinguishable from a
  healthy feed over a quiet store. The answer comes from the STORE (a new
  `cdc.Store.CapturesChanges()`), because the CDC decorator is in the write path exactly
  when the feed is on and therefore cannot disagree with reality the way a second reading
  of an env var can. Additive: a subscriber that switches on the change type ignores an
  event name it does not know.
- **An Activity tab** in `@loomcycle/memory-view` 0.3.0 tails both families, showing
  coordinates and never values (the feed is value-free by design). It keeps three
  non-live states apart — this build cannot tail / the runtime says the feed is off /
  connected-and-quiet — because collapsing them into "no rows yet" makes two
  misconfigurations look like "the pass is doing nothing".

### Three fixes, from running verified writes on real data for the first time

The verified-writes line shipped in v1.54.0 and had never been enabled anywhere. Turning
it on against real chats, with a local extractor and judge, worked — and exposed three
defects that only appear on real data.

- **`verification_stats` counted entity IDENTITY nodes as facts.** The pass mirrors each
  fact as two chunks — the claim, and an identity node for the subject it is about — and
  both carry entity metadata. An identity node can never carry a span (a subject is a
  name), so every new subject added a permanently unverifiable row and the reported share
  FELL as the store got richer. Measured on a live store: `0.579` reported where `0.846`
  was true, and 7 facts called impossible to verify where the answer was 1.
  **⚠️ Your numbers will move on upgrade** — the verified share rises and
  `unverifiable_no_span` falls, with no data change behind it. `list_facts` gains an
  opt-in **`claims_only`**; the default listing stays wide because document federation
  reconciles identity nodes too. The obvious fix was wrong and measurement caught it:
  filtering `type = 'fact'` drops operator-`remember`ed facts, one of which landed as
  type `object` while carrying a span.
- **Span derivation attached SEARCH PATTERNS as evidence.** Two consecutive passes each
  withheld a TRUE fact whose recorded span was a tool-call line or a regex — a local chat
  model had written its tool calls as prose, and Dice overlap rewards a short candidate
  whose every token hits, so a 6-token pattern beat a 30-token sentence that stated the
  fact outright. The judge was right to refuse both; the evidence was mis-attached
  upstream. `splitSentences` now drops a candidate with no assertion in it — one that is
  only tags, or dense enough in punctuation to be code. Such a fact gets NO span
  (unverified and visible) rather than a false one. Does not re-derive spans already
  stored.
- **`loomcycle validate` exited 2 on every bundled agent preset.** It applied the pin-path
  rule to agents that name a `tier:`, so it named agents the running server resolves
  fine — and because it returns at the first agent, MCP servers and everything after went
  unchecked. `agents list --json` bailed mid-array, leaving unparseable output. The tier
  is now checked first, mirroring the runtime; an agent with neither a pin nor a tier
  still fails, which the resolver also refuses.

### Adapters

- **`@loomcycle/client` 1.56.0** — carries `claims_only` on the Document tool input.
- **`loomcycle` (PyPI) 1.56.0** — no functional change; realigned so the python tag
  publishes (PyPI was left at 1.54.0 when v1.55.x shipped without a python tag).
- **`@loomcycle/memory-view` 0.2.0 → 0.3.0**, published on its own tags: the facts and
  verdict surface, the coverage strip, `remember`, the two embedding-maintenance ops, the
  claims-only fact list, and the Activity tab.

### Upgrading

Nothing to migrate. The change feed stays opt-in (`LOOMCYCLE_MEMORY_CHANGES_ENABLED=1`),
`inject_tool_guide` is off unless an agent sets it, and `verify_writes` is still off by
default. The one visible difference on an existing store is the corrected coverage
arithmetic described above.

**Known issue:** facts whose text is in a non-Latin script all slug to one memory key and
overwrite each other ([#1047](https://github.com/denn-gubsky/loomcycle/issues/1047)) —
pre-existing since the deterministic-key consolidator, unaffected for Latin-script
deployments.

## What's in v1.55.1

**An operator may vouch for a fact that has no span.** Patch — one reported bug, its root
cause, and two pieces of copy that were saying something untrue.

- **The bug.** Opening a fact stored before spans existed, writing "User confirmed", and
  being refused. Both controls failed there — only `unclear` was allowed without a span —
  so the console offered two buttons the server would never accept.
- **The rule's own justification had expired.** `judge_fact` refused because a verdict
  without evidence "would be indistinguishable from one that was checked". That stopped
  being true when `judged_by` landed in v1.54.0: the store now records whether a machine
  or a human reached a verdict, so an operator putting their name to a fact is no longer
  mistakable for an entailment check. **An operator** may now record `supported` or
  `unsupported` on a span-less fact — no span is invented, it still counts as span-less in
  coverage, and the reason plus `judged_by` carry whose word it is. **An agent still
  cannot**: a run affirming what it cannot check is exactly the claim a span exists to
  substantiate. **`mistyped` is excluded for anyone** — it says the span carries the claim
  but the filing is wrong, so with no span there is nothing for it to be about.
- **Copy.** The console now distinguishes CHECKING from VOUCHING at the moment your name
  goes on a verdict; and the coverage strip no longer describes span-less facts as ones
  that "can never be verified by anyone" — they cannot be checked against a source, but a
  person can still vouch.

Built FULL via a `force_full` dispatch despite the patch tag: the fix is in the Go runtime,
and the lean patch tier builds only the `loomcycle-browser` image, which would have left
the change unable to reach a deployment. The Web-UI half lives in
`@loomcycle/memory-view`, which publishes on its own `memory-view-v*` tag.

## What's in v1.55.0

**One memory console, and an erasure you cannot perform unrecorded.** Minor — the memory
surfaces built over the previous release are consolidated into a single console, an
operator can write a fact down themselves, retention is finally readable, and subject
erasure grows a durable record of who asked for it. Plus Web-UI authoring for the two
peer-federation substrates that shipped headless in v1.54.0.

### RFC CF — the memory console

- **One console, not two.** v1.54.0 added a facts panel and a search panel to the Web UI
  beside `/memory` — which is a thin wrapper around `@loomcycle/memory-view`, a published
  package that already shipped `entries` | `facts` | `search` tabs. The duplicates are
  removed and the genuinely new capability folded into the package: **spans, verdicts,
  `include_refuted`, and the two controls that let a person overrule the judge**, plus a
  coverage strip over the fact list. The line drawn: the package owns views over one
  scope's data, the app owns the operator plane — so starting a consolidation pass is a
  host-supplied callback rather than a URL the package knows.
- **The safety valve exists now.** Verified writes shipped with "a wrong verdict is always
  recoverable by re-judging" as its argument for withholding rather than deleting, and
  that recovery was reachable only by API. An operator can now see the claim, the span it
  was drawn from, and the verdict — and overrule it in either direction.
- **`judged_by`** records WHO reached a verdict, server-stamped from the call's own
  context exactly as `origin` is. There is no wire field to set it: an agent able to
  record "an operator decided this" would be one injected instruction away from laundering
  a machine's verdict into a human's. An agent's verdict carries the agent's NAME; a
  verdict predating the column reads as unknown rather than as either party.
- **`Document op=remember`** stores a statement a person supplied as a fact that **cites
  itself** — the text becomes both the claim and its source span, filed `evidential`. An
  operator's instruction is a source, not a claim; storing it self-citing makes
  operator-authored memory the best-evidenced kind rather than the worst. Additive only:
  there is no "forget", because an instruction that deletes on a fuzzy match is how data
  disappears quietly.
- **Retention is readable.** `GET /v1/_retention` gets a Settings surface: what the sweeper
  is configured to remove, per family, with the sweeper's own on/off state stated first.
  A `purgeable` count is never rendered alone — it is computed regardless of mode, so
  beside an `off` family a bare number reads as a countdown to a deletion that never
  happens.
- **Settings tabs get the three-tier role class** the left nav already had. The binary
  `admin` boolean could not express a tenant operator, so a delegated user reaching
  `/settings` directly was shown five tabs whose every call 403s.

### Subject erasure — audited, or refused

- **An erasure now writes its own durable record, and refuses without one.** The audit sink
  says recording "must never block the caller's primary operation"; erasure is the
  deliberate exception, because it is the one operation nothing can undo. A deployment
  that cannot say who erased which subject does not get to perform the deletion — refusing
  is recoverable, an unrecorded erasure is not. With no `LOOMCYCLE_AUDIT_LOG_PATH`,
  erasure is disabled and boot says so.
- **Two records, ordered.** `erase_intent` is written BEFORE any deletion and its failure
  refuses the operation; `erase_result` follows with the planes deleted and retained. A
  crash between them still leaves evidence that an erasure was attempted, by whom, against
  which subject. Dry runs write nothing.
- **The console: report before execute, always.** The erase control does not lift until a
  report has been shown for the SAME subject — otherwise an operator could read what would
  go for one person and confirm the deletion of another, which no confirmation dialog
  catches. Confirmation is the subject id typed back and compared exactly: a modal with a
  red button is dismissed by reflex, and the failure mode here is erasing the WRONG
  subject rather than erasing accidentally. The three tiers are never summed — they are
  degrees of REACH, so each is labelled by what it does ("will be deleted" / "held, but
  NOT deleted" / "cannot be reached") — and a residue of zero across zero sessions reads
  as UNKNOWN rather than none, since with no sessions examined there was nothing to trace
  derived data from.

### Web UI — the peer-federation substrates

- **Remote memory backends** (RFC CD Part B) are authorable from Integrations: a kind
  selector reveals the peer connection — `base_url`, `api_key_env` (an env-var NAME, never
  a secret) and an optional `api_version`.
- **Document Sources** (RFC CE) join them as a fifth Integrations family, so a peer
  `DocumentSourceDef` can be authored, forked and retired from the Web UI rather than only
  from yaml, the substrate API or MCP.

### Adapters

- `@loomcycle/client` gains **`backfillEmbeddings()`** and **`purgeStaleEmbeddings()`** —
  the two embedding-maintenance ops that had no client method. `@loomcycle/memory-view`
  will surface them once it can depend on this release.

## What's in v1.54.0

**External data access, document federation, and verified writes.** Minor — three feature lines land together: any app reaches loomcycle's memory + documents over a documented HTTP contract or a peer loomcycle (RFC CD), a document is replicated to and reconciled with a peer instance in both directions (RFC CE), and a fact records the source it came from and is checked against it before it is trusted (RFC CC). Plus the loomboard saved-views scaffold (RFC BT) and a memory/facts Web-UI surface.

### RFC CD — external data-access channels

- **OpenAPI 3.1 contract + Swagger UI.** A hand-authored `api/openapi.yaml` documents the whole data surface — the memory REST family + `POST /v1/_memory/search`, the `/v1/_document` and `/v1/_path` op-dispatch (modeled as a `oneOf` on `op`), and the asset GET — served at `GET /v1/openapi.{yaml,json}` with a self-contained Swagger UI console at `/v1/docs` (vendored, no CDN). Any language gets a generated SDK; the contract is public, the data still bearer-gated. A drift test pins the spec's op enums to the tool op sets.
- **A peer as a memory backend (`kind: remote`).** A memory backend can now proxy an agent's memory to *another* loomcycle instance's `/v1/_memory/*` — the peer embeds server-side; get/set/list/delete/search/stats all round-trip. Auth is a credential-allowlisted `api_key_env` (never an infra secret, never `${...}`-interpolated), resolved at use time; the peer host is dialed through the SSRF guard; `fallback_on_error` degrades to local.
- **Change feed (CDC), pull + push.** An opt-in, value-free change feed (`LOOMCYCLE_MEMORY_CHANGES_ENABLED`) emits at the store write choke point so both in-run and external CRUD land in one stream. Consumers subscribe over SSE (`GET /v1/_memory/changes`, `/v1/_document/changes`) or register a config-declared `change_subscriptions:` HMAC-signed webhook with a persisted at-least-once cursor. Tenant-scoped, SSRF-allowlisted; the feed is operator observability (`substrate:tenant`, not member-accessible).
- **Python gRPC memory parity.** A generic `Memory` RPC + `client.memory()` gives the Python adapter the memory surface it lacked, riding the same op-dispatch shape as documents and paths.
- **Ops:** every published image is mirrored to `ghcr.io` so a Docker-Hub-rate-limited operator has a fallback.

### RFC CE — remote document backend + federation

- **Bind + reconcile a document with a peer.** Declare a peer under `document_sources:` (or author one at runtime — see the substrate below), bind a local document to a peer document with `Document op=set_remote`, and reconcile with `op=sync`. `direction:pull` (default) copies the peer's chunks in; `direction:push` writes this document's chunks up to the peer. Reconciliation keys on `natural_key` and carries each keyed chunk's **body, tags, hierarchy** (it lands under its keyed parent at its sibling position) and its **manual cross-reference links**; a diverged body is updated in place with the overwritten body kept in the *losing side's* chunk history (retire-not-delete); a chunk without a `natural_key` is excluded and counted.
- **`op=diff_remote`** — a read-only dry-run that classifies keyed chunks into `only_local` / `only_remote` / `diverged` / `retagged` / `reparented` / `same` (plus unkeyed + edge-drift counts) without touching either side, so you see exactly what a sync would change first.
- **`DocumentSourceDef` substrate** — a source can be authored, forked, and retired at runtime as a versioned, tenant-scoped substrate Def over every transport (HTTP / gRPC / MCP / TS / Python), a faithful mirror of `MemoryBackendDef`; `set_remote`/`sync` resolve a name tenant-substrate → static yaml → shared substrate.
- **Reviewed + hardened.** A whole-line adversarial code review surfaced eight findings, all fixed with fail-before regression tests: sync now converges on a refuted chunk (no create-churn), the `DocumentSourceDef` HTTP route is `substrate:tenant` in parity with gRPC/MCP, title/type/status edits propagate, `diff_remote`'s reparent prediction matches what a sync does, a retired source stops resolving, a tags-only sync no longer writes a duplicate-body revision, and the runtime + static source validators accept the same values.

### RFC CC — verified writes

- **A fact records where it came from and is checked against it.** A fact now stores the exact `source_quote` span it was derived from (RFC CC P1); the subject becomes structured data the ontology gates entity writes against (P2); a write-time judge checks each fact against its own quote and **withholds** what fails (a refuted fact is retained but hidden from `list_facts`/`graph_recall`, readable with `include_refuted`), rather than deleting it. `verbatim_answer` answers a lookup question with a stored fact quoted verbatim plus its span and no generated text; a backfill sweep judges facts that predate the feature; `judged_at` records *who* judged a fact in a column the caller cannot set.
- **Web UI.** A Memory tab surfaces verification coverage and lets an operator start a pass; a facts surface shows evidence, verdicts, and a way to overrule the judge; semantic search over memory is the front door to the facts panel.

### Adapters + packages

- **`@loomcycle/client` 1.54.0** (npm) — adds `documentSourceDef()` and the accumulated substrate/memory surface.
- **`loomcycle` 1.54.0** (PyPI) — adds `memory()` (gRPC) and `document_source_def()`.
- **RFC BT** — the `@loomcycle/loomboard` saved-views scaffold (P1) + the board-bound `TeamDef op=run` task-key tagging (P4); loomboard versions on its own `loomboard-v*` tags. The React `explorer`/`library`/`memory-view` packages version independently.

## What's in v1.53.2

**Tenant-member access: a non-isolated user reads and writes the tenant plane (RFC CB).** Patch (Go, auth only). A delegated per-user token that is not isolated is now a full tenant *member* over HTTP.

**The gap.** A non-isolated user token (a "member" — `runs:*`/`channel:*`, `access_mode "tenant"`) already had whole-tenant *data* access inside a run — loomcycle's rule is "whole-tenant data access is conferred by NOT being isolated." But the same token was 403'd at every HTTP `/v1/_*` tenant surface by the route scope-gate alone, so a member could not browse or author its tenant's shared Library, Documents, or Memory over HTTP, and consumers (loomboard) hid those views rather than render a wall of 403s.

**The change.** `authMiddleware` now admits a non-isolated principal on a *member-accessible* tenant route: `tenantMemberAccessible(method, path) && !auth.IsIsolated(p)`. The predicate opens any route that requires `substrate:tenant`, **minus** the operator-control carve-outs — user create / roster mutation / user-token minting, per-subject erasure, budget *writes* (`PUT`/`DELETE /v1/_limits`), and tool-use hooks — which stay operator-only. `substrate:admin` routes (cross-tenant enumeration, runtime admin, token minting) are excluded automatically, since they are not `substrate:tenant`.

**No new scope, no re-mint.** It keys on the existing isolated/non-isolated boundary — generalizing RFC BY's discovery tiering to the route gate — so no token changes. The isolation floor is untouched: an isolated `substrate:user` token still fails every tenant gate, `auth.IsIsolated` stays true for it, and `ConfineIsolatedScope` plus the run-start isolation stamps are unchanged. A member is still confined to its own tenant by each handler's `principalTenantScope`/`tenantFromCtx`.

**Scope.** The HTTP gate. gRPC/MCP substrate-plane parity is deferred (a member's runs already reach tenant data on those planes since they are not isolated). loomboard's `canTenant` relax — so a member renders the Library/Documents/Memory nav — ships in loomboard separately. No adapter or Web UI changes here.

## What's in v1.53.1

**The routing view stops blanking on an empty tier cascade.** Patch (Go + Web UI). The Settings → Routing page went blank on a real deployment, and the cause was a `null` where the UI expected a list.

**The bug.** `GET /v1/_routing` builds each tier's candidate cascade by appending to a nil slice, so a `user_tier × tier` that resolves to **zero candidates** — a perfectly valid config state, e.g. a `high` user-tier whose `high` tier has no available model — came back as JSON `"cascade": null`. The routing view's `TierCard` does `tier.cascade.length` on that; `null.length` throws, React unmounts, and the whole page is blank. Same class as the `/v1/_usage` nil→null crash in v1.11.1.

**The fix, at both ends.** `handleRouting` now initializes the `Tiers` and `Cascade` slices to non-nil empties, so an empty resolve serializes as `[]`; and `RoutingView` adds `?? []` guards on `user_tiers` / `tiers` / `cascade` so a single bad tier can never blank the page again. A regression test asserts an empty cascade decodes to a non-nil slice (verified failing on the old code).

**Built with `force_full` on purpose.** Like v1.52.1, this Web-UI-touching patch was cut with the full pipeline so it publishes the deployable `denngubsky/loomcycle` image with the fixed UI embedded — a lean browser-patch would ship only `loomcycle-browser`, which the standard deployment does not consume.

Adapter versions are unchanged — no adapter surface was touched.

## What's in v1.53.0

**Ontology curation: an agent can suggest entity types, and only an operator can accept one.** Minor — a new inert entity status, two operator actions, one narrow authoring op, a bundled curator agent, and a refreshed MCP tool surface. No schema change; one behaviour change for agents that were writing the ontology document directly (below).

**The problem.** The ontology had a single gate — the root chunk's `draft`/`confirmed` status — so anything appended to a confirmed document was live on the next run. There was nowhere to put a suggestion that is real but not in force, which meant an agent could not *suggest* a type, only add one. Separately, overriding a standard type meant reading its field names off the panel and retyping them by hand.

**A proposal is the entity in its final place, switched off.** `chunks.status` was already a column the ontology reader ignored, so `proposed` and `rejected` are now reserved as inert: such a chunk is not an entity, not rendered into any prompt, and not expanded in retrieval — but it *is* reported to the operator, in its position in the tree, with its fields and its body. Accepting clears the status in place, so an accepted subclass lands under the parent it was filed under and inherits from it. Nothing is copied out of a staging area.

**Only those two words are inert, and that matters more than the feature.** Every other status — including one the build has never heard of — leaves the entity in force. The shorter inverse rule ("anything not blank or confirmed is inert") would silently drop types from any document where an operator had used the status field for their own purposes, which is exactly the failure the chunk-tree reader was written to remove. Children of an inert chunk re-parent to the nearest in-force ancestor for the same reason: rejecting a parent must never switch off a live type beneath it. A **rejection is kept** as a tombstone, so an automated curator can see what was already turned down instead of re-proposing it.

**Adopt a standard type.** One action copies a standard type into the document with its fields, so it can be extended or subclassed without transcription. The fields come from the seed **server-side** — a client-supplied list could disagree with the type it claims to copy, and the document would look like an override of `event` while declaring something else. It records provenance (what was copied, from where, at which build) in the chunk's structured fields, never its body: adoption **freezes** your copy, since an override replaces the standard term wholesale, and without provenance that is undiscoverable later.

**An agent may propose, never decide.** On the tenant ontology document alone, a tool call from a run may only add a chunk whose status is `proposed`. Update, delete, move, reorder, supersede, `import_md`, `import_canvas`, `delete_document` and `set_path` are all refused there — the last two reach the whole ontology, `set_path` by re-homing `/memory/ontology` so the reader resolves somewhere else. The operator/agent distinction is a server-stamped context marker with **no wire form**: the off-run path also carries a synthetic agent id, and `POST /v1/runs` accepts a caller-supplied `agent_id`, so keying on that would have let a run claim to be the operator.

**`propose_entity`** is the front door: it resolves the ontology itself, takes the parent **by name** (the names the agent was given in its prompt — it cannot know a chunk id), stamps the inert status so the contract cannot be spelled wrong, bounds the evidence body, and refuses a name already in force, already proposed, or already rejected. It needs **no tenant grant**, deliberately: a suggestion cannot change what any run is told, so requiring the authority live authoring needs would mean only an agent that could already author live types could offer one.

**The bundled `memory/ontologist`** does a pass over one user's stored facts and files suggestions with evidence — counts and example titles — reachable from Settings → Ontology by a link that *stages* the run in the terminal rather than starting one from a settings click. On demand, not scheduled: a curator filing proposals nobody reads is clutter with a cron job. It is one agent definition; no new runtime primitives. Measured on a local model it is useful but not reliable, and its value depends on facts carrying types at all — measure your extraction model's type-emission rate before scheduling a pass.

**The MCP tool surface was refreshed, and had two problems.** The Document description was 13 ops behind the tool (`query_documents`, `list_facts`, the tag ops, the history ops, `backlinks`, `related`, `unlinked_mentions`, the canvas ops) — for an MCP client the description *is* the documentation, so an unlisted op is one no model will call. And 18 internal design-document citations had accumulated, 13 inside `Description` strings that go in front of every client's model. Both are now enforced by tests. The Claude Code plugin's reference is updated to match (plugin PR #26); it ships no tool schemas of its own, since the thin client proxies this `tools/list`.

**⚠️ Behaviour change.** An agent that was writing the ontology document directly will now be refused everything except filing a proposal. An MCP session is an agent surface, so an operator driving the thin client can no longer flip the ontology's confirm status either — the Web UI and `POST /v1/_ontology` remain the operator path.

Adapter versions are unchanged: `@loomcycle/client` 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/memory-view` 0.1.0. `@loomcycle/explorer` 0.7.0 publishes on its own `explorer-v0.7.0` tag, carrying the `+ child` nesting fix from v1.52.1 to external consumers.

## What's in v1.52.1

**Patch: the ontology was not editable through the UI, for two independent reasons.** v1.52.0 shipped RFC BZ's reader, inheritance, retrieval expansion and panel tree — and no working path for the operator who owns the taxonomy to author one. Both bugs were reported from a real deployment.

**The document viewer could not nest anything, anywhere.** `+ text` inserts a SIBLING (`after_id`), which is right for prose flow and wrong for structure — so a hierarchy could only ever be created by `import_md` or by an agent. Selecting a type and adding a sub-item put it BESIDE that type at the top level. A new **`+ child`** action passes `parent_id`, appending the new chunk under the selection and opening the editor on it so it can be named immediately (a type's name is its heading). `+ text` keeps its sibling behaviour; the tooltips now say which is which.

**And the Settings panel's "Edit ontology →" link never worked.** It pointed at `/documents/<id>` with no scope. The ontology lives at *tenant* scope and the viewer folds an absent scope to `user`, so the link opened the right document id in the wrong store, the read 422'd, and the operator got "No chunks" and no create buttons at all — indistinguishable from "editing is not possible here", and most of why the UI read as unusable. The URL is now built by a tested helper rather than an inline template literal, because a dropped query param is exactly what nobody notices. This was the only `/documents/` deep link in the codebase, so no other surface carried the same defect.

The panel also now names the steps — select a type, then `+ child` — which is not something anyone can guess.

**Release note for operators:** this is a Web-UI fix, and the Web UI is embedded in the binary and the `denngubsky/loomcycle` image. The patch build tier publishes only `loomcycle-browser`, so this tag was released with the `force_full` dispatch to produce the full artifact set (binaries, Homebrew, and every image variant) — the escape hatch the release workflow documents for exactly this case. The RFC BR sandbox sidecar images are unchanged and were not rebuilt.

Nothing else changed: no Go code, no schema, no wire surface. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0 and `@loomcycle/memory-view` 0.1.0. The `+ child` fix reaches the Web UI through the package source (Vite alias); publishing it to external `@loomcycle/explorer` consumers needs an `explorer-v*` bump and tag.

## What's in v1.52.0

**RFC BZ: the tenant ontology gets classes and subclasses — one chunk is one entity, and a child chunk is a subclass.** Minor — no schema change and no wire-breaking change, but retrieval returns strictly more for a hierarchical ontology, and a bug fix makes types live that were silently inert. Read the upgrade note below before confirming an ontology you had already nested.

**The bug it fixes, and why it needed a new reader.** An operator who organised their ontology into a hierarchy — the natural move in a document UI — lost every nested type. `ParseOntologyMarkdown` matched only `"## "`, so a `### incident` was recognised as neither a term nor a title and was dropped: no error, no warning, nothing in the Settings panel. Their document read correctly and their ontology did not match it. "Also match `###`" cannot fix it, because the ontology was read from `export_md`, which renders chunk depth as `strings.Repeat("#", level)` — after flattening, a subclass's title and a heading inside a body are the same bytes, so the information needed to tell a nested TYPE from a nested COMMENT is destroyed before the parser sees it. The reader now walks the chunk tree, where `parent_id` *is* the hierarchy: the chunk's title names the entity, backticked bullets are its fields, and a child chunk is a subclass. Depth is capped at four levels and a deeper type is flattened onto the cap rather than dropped — silently discarding an entity is the bug being fixed, and a cap that did it one level down would be the same failure.

**A subclass inherits its parent's fields.** `incident` under `event` carries `occurred_at` without restating it, transitively; a child redeclaring a field wins. An inherited field cannot be removed, which is correct rather than a limitation — a subclass lacking its parent's field is not a subclass, and someone who wants a type without those fields wants a sibling. Removal lives on the other axis, where a tenant term replaces a same-named seed term wholesale, so resolution runs *after* layering: a subclass of a type the tenant redefined inherits the tenant's fields, not the seed's. To subclass a *standard* type, declare it as your own root (which overrides the seed by name) and nest beneath your copy.

**The prompt renders a tree, and a flat ontology renders byte-identically.** Extraction now sees indentation plus one instruction — use the most specific type that fits — because a model handed a ladder with nothing telling it to climb sits at the top and the subclasses go unused. The tree form engages **only** when a hierarchy is actually present, and a dangling parent does not count: this string is in the system prompt of every extracting agent, so a whitespace or ordering change would invalidate provider prompt caches and shift extraction results for every deployment that never nests. That guarantee is asserted by test, not assumed.

**Retrieval expands subtypes — the payoff.** `list_facts` and `query_chunks` now match a type *and its subclasses*, transitively and downward only: `event` finds `incident` and `outage`, while `outage` still finds only outages. Expansion happens at query time and storage keeps the concrete type only, so re-parenting a type takes effect immediately and retroactively; materialising ancestors into each row would mean a correction silently invalidates every fact written before it. It applies **only when the ontology is confirmed** — a draft is inert everywhere else, and retrieval must not be the one surface where an unconfirmed edit changes answers — and the response reports `type_expanded_to` when the filter widened, because a wider answer with no explanation reads as a bug. `graph_recall` is not included: its seeds come from title text or explicit ids, so it has no type filter to expand.

**The Settings panel shows the taxonomy it now parses.** Both columns render as trees with field counts, declared fields plainly and inherited ones dimmed, plus three signals that were previously invisible: the depth cap says when it flattened a document, a leaf declaring no fields of its own is flagged (almost always a section chunk that silently became a subclass), and an awkward type name gets an advisory. Two columns are kept rather than merged into one tree — the gap between "this deployment defines" and "in force now" is the only visible evidence that a draft is inert.

**`preference` and `fact` are pinned as roots.** They may be *subclassed* — `dietary-preference` under `preference` is fine — but they may not be given a parent: that inverts the memory tier, and with subtype expansion a query for that parent would sweep in every preference the user ever expressed. Enforced by clearing the parent rather than rejecting the document, and reported in the panel. **Type names are warned about, never rewritten:** a name is part of a stored fact's key, so normalising `Notes on naming` after facts exist under that spelling would split the type in half.

**The extraction eval can now measure hierarchy use, and the measurement overturned the assumption behind it.** A new `specificity` ability, a hard type assertion, and a separate `--corpus hierarchy` fixture (kept separate so the shipped corpus digest — and its recorded baselines — stay valid). The RFC expected *over-specification*: given a ladder, a model reaching for `incident` where `event` was right. Across four runs of `ollama-local qwen3.6` it never once climbed past the evidence; what it does is decline to type at all on the subtype cases and fall back to the standard roots. Under-engagement, not over-reach. No baseline entry was recorded on purpose — four cases swinging 0.25–1.00 cannot support a 0.15 tolerance, and a flapping gate teaches everyone to pass `--no-gate`. Separately verified: the seed-only extractor prompt still digests to a recorded baseline's value, so nothing in this release invalidated the committed numbers.

**⚠️ Upgrade note.** The fix is a migration event. Types you nested and saw no effect from were inert; after this release they are live subclasses and they steer extraction, and a type filter returns strictly more than it did. Re-read your ontology in Settings → Ontology before confirming it — the panel now shows the tree it actually parses, so the change is visible in one screen rather than discovered through extraction results.

`/v1/_ontology` gains `parent`, `inherited` and `name_issue` per term and reports `notes` (a list) in place of the single-string `note` introduced earlier in this same unreleased line, so no released consumer saw the old shape. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0 and `@loomcycle/explorer` 0.6.0 — none was touched.

## What's in v1.51.0

**RFC BY: a user token can now read its own chats and discover the agents it may run.** Minor — one new endpoint, one additive `@loomcycle/client` method, and a widened-then-reconfined gate. No schema change, no wire-breaking change. Completes RFC BX's "user access = own + bundled + tenant-if-enabled" model on the read side.

**The gap it closes.** RFC BX gave a `substrate:user` token the run plane but no read surfaces. A delegated user could start and read its own runs and nothing else — it could not see its own past chats, and it had no way to find a runnable agent without being told the name out of band. Both surfaces were gated at `substrate:tenant`, which no delegated user token holds; and a **tenant-mode** user token holds neither `substrate:tenant` nor `substrate:user` (only *isolated* users carry `substrate:user`), so keying the gate on either scope would lock one user type out.

**A member-read gate, not a scope hole.** The user-read surfaces are gated on `runs:read` — the honest floor every such principal already holds (isolated users imply it, tenant-mode users hold it directly, tenant operators and admin sit above), so opening them grants no capability a user did not have. The security-bearing decision moves into the handler, keyed on the authenticated principal, never the wire.

**Own history is capped, not just admitted.** `/v1/_history` now admits a member token, and a delegated user's history scope is capped to `[self, user]` — its own chats, in both access modes — while a tenant operator keeps `[self, user, tenant]` and admin adds cross-tenant `global`. The owner subject/tenant is stamped from the principal, so `[self, user]` is exactly the caller's own history and nothing else. The cap is applied on both the HTTP and gRPC paths, so a member is confined identically on either transport. The TS `history()` method is unchanged — it already carried the caller's bearer.

**Discovery is tiered server-side by access mode.** A new `GET /v1/_runnable-agents` returns the agents the caller may run: bundled/system agents always (the shared floor), the tenant's shared agents only when the caller may use them, and own user-scoped agents (reserved — none today). An **isolated** token never sees the tenant's agents — a hard floor read from the token scopes, so a stale `access_mode` column can never widen it; a tenant-mode user is governed by its authoritative `users.access_mode` row. Entries are lean (`name` + `source` tier) — no operator metadata (version counts, retired badges, content hashes) and no system prompts; that stays in the `substrate:tenant` Library. A different tenant's agent is never leaked.

**`@loomcycle/client` 1.51.0** adds `runnableAgents()` (+ `RunnableAgent` / `RunnableAgentsResponse`). Additive; existing callers unchanged.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched. The gRPC/MCP discovery twins and a longer-term scope-hierarchy cleanup are noted as follow-ons in the RFC.

## What's in v1.50.0

**RFC BX Phase 2 follow-ups: an isolation invariant made structural, and the delegated-users API reaches `@loomcycle/client`.** Minor — additive adapter surface, one defense-in-depth runtime stamp, one CI deflake. No schema change, no wire change.

**The isolation bit is now stamped where it was merely unreachable.** RFC BX Phase 2 confines an isolated member (a `substrate:user`-topped principal) to its own data scope through `RunIdentity.Isolated`, which `ConfineIsolatedScope` reads to refuse tenant/global scopes. Every HTTP run-start site stamped it; the MCP-direct path (`mcpPrincipalCtx`) and the two gRPC substrate paths (`substrateGRPCCtx`, `substrateGRPCUserCtx`) did not. That was safe — but only because all three gate on `substrate:tenant`, a scope an isolated token cannot hold, so no isolated principal ever reached them. Safe-by-unreachability is a coupling between a route gate and a confinement in a different file: widen the gate and the confinement disappears silently. It is now stamped from `auth.IsIsolated(principal)` at all three sites, so the property holds by construction rather than by the current gate. Behaviour is unchanged today; the regression tests fail on the unstamped code.

**`@loomcycle/client` 1.50.0 gains the user + token management surface.** The RFC BX Phase 2 routes — `/v1/_users` CRUD and `/v1/_users/{subject}/tokens` mint/list/revoke — shipped with a Web UI console but no adapter method, so a programmatic operator had to hand-roll `fetch`. Added `createUser` / `updateUser` / `deleteUser` / `mintUserToken` / `listUserTokens` / `revokeUserToken` and their types, mirroring the Web UI client. The tenant is server-derived from the bearer, so none of them send one; the mint result carries the plaintext bearer exactly once. These are HTTP-only admin routes — the Python adapter is gRPC-only and there are no gRPC user RPCs, so it is unchanged.

**A CI flake removed.** `TestProviderGate_ZeroOverheadWhenUnconfigured` asserts five runs overlap in the provider (`peak > 1`) using a bare 20 ms delay, which a loaded runner could stagger into a false `peak=1` — it failed exactly this way on a recent `Go 1.26.x` job. It now uses the harness's existing `holdUntil` rendezvous so the observed peak is deterministic; a genuine under-admit still fails (the 2 s safety valve fires, peak stays < 2).

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.49.1

*Memory search: the source filter is a visible dropdown.*

Patch. UI-only — no wire, adapter, or runtime API change. The TS adapter stays at
1.49.0, so its publish is correctly skipped (nothing to republish).

The RFC BW `source` field in the @loomcycle/memory-view search panel (shipped in
v1.49.0) rendered as a bare <input list> + <datalist>: a plain empty text box
whose facts / notes / documents choices only surface on focus, with a
subtle/browser-dependent dropdown affordance — so operators read it as an empty,
non-functional field. #961 replaces it with an explicit combobox: a text input
with a visible ▾ chevron that opens a real option list (all sources / facts /
notes / documents) on click, closing on outside-click or Escape, and still
free-text editable (an unknown value is dropped server-side). Same `sources`
selector on POST /v1/_memory/search — only the control's presentation changed.

@loomcycle/memory-view is versioned on its own memory-view-v* tag (0.1.0,
unpublished); this runtime tag does not publish it.

## What's in v1.49.0

**🎯 Targeted memory search: ask for facts, notes, or documents by name.** RFC BW, all three phases, plus the `@loomcycle/memory-view` console package. One wire field changes value set — see the note below.

**The bug it fixes, measured.** A user asked their chat agent *"remind me which medicine do I use"*. Seven of ten `recall` hits were document chunks, and the fact naming their medication ranked **fifth** — below a horizontal rule (`"---"`, 0.477), a shell fence (`` "```sh" ``, 0.452) and a bare `#` (0.451).

Two independent causes, both ours. Markdown scaffolding was being embedded: a heading-split import turns a fence line into its own chunk, and a short syntax token embeds near the centroid of everything, so it ranks mid-high for *every* query — it does not waste a row, it buries answers. Those are now rejected, by one predicate covering both a chunk's body and its title fallback (a first attempt rejected the body and then fell through to an equally scaffold-ish title, moving the noise rather than removing it).

The structural cause was worse. **RFC BU §6 promised that `recall` does not reach documents** — and that held only because chunk bodies had no embeddings. RFC BU phases 1–2 embedded ~2,900 of them into the same per-scope vector plane `recall` searches, so the guarantee went false without a line of `recall` changing. The RFC that declared the invariant is the one that broke it, which is the argument for putting the boundary in the API rather than leaving it emergent from whatever happens to be indexed.

**A named selector, not another reserved string.** The reported failure was an agent not knowing that document bodies live under `doc.chunk:`, so answering it with a second magic string would have answered the symptom:

```
Memory op=recall scope=user query="which medicine"          → facts + notes (new default)
Memory op=search scope=user query="…" sources=[documents]    → document text
Memory op=search scope=user query="…" sources=[facts]        → distilled facts only
```

`prefix` survives as the escape hatch and composes as an AND. **`recall` now defaults to memory rather than everything** — that is the fix, not a side effect; an operator wanting the old behaviour passes all three sources explicitly. `search` still spans every plane by default, because `/v1/_memory/search` exists to answer "where did I record this" and narrowing it would break what v1.47.0 shipped.

**Facts vs notes is decided by `origin`, not `class`.** Both are provenance columns, but `origin` is stamped by the server from the writer's identity while `class` is model-supplied — so keying "is this a consolidated fact" off `class` would let an agent promote its own note to a fact by labelling it. Legacy rows predating the column count as **notes** rather than vanishing from both halves of the split.

**Two source combinations are refused rather than approximated.** `documents` together with only one of facts/notes needs a disjunction across two independent dimensions. It could be built and deliberately is not, because the alternative that matters is what happens instead: silently widening to "everything" hands back rows the caller excluded *while it believes the filter applied*. The error names the supported sets.

**A backend that ignores the selector must say so.** `sources_applied: false` is surfaced with a note, and false is the zero value on purpose — an external memory-layer service that drops the selector must not be able to do so silently, because the caller would then trust a label the result does not deserve. Recall is where it matters most: its default excludes documents, so a backend that ignored it returns exactly what the default exists to keep out.

**The filter runs in SQL, and that was forced.** The candidate pool is capped at 51 rows, so on a scope with ~2,900 chunks against ~42 facts a post-filter would leave a caller asking for ten facts with two.

**Measured before shipping**, because the RFC made it a gate: excluding documents on a 2,942-row scope is **~31× faster**, not slower — 30 ms against 928 ms. The reason matters more than the number: migration 0017 deliberately creates no ANN index, so there is nothing to degrade and a predicate cutting 2,942 candidates to 42 cuts the work proportionally. The risk returns if an operator opts into HNSW, which 0017 invites, so the regression assertion stays.

**Also: the Memory tool now teaches itself.** All three `chat/*` agents pointed at `Document graph_recall` for "what do we know about X" — which walks out from facts carrying entity metadata and returns `seeds: 0` on a store without them, reading as "nothing is remembered". Nothing named `Memory op=recall` or its required `scope`+`query`, so an agent guessed its way there through three missing-field errors. The prompts and the `memory-layer` help topic now name the invocation first and position `graph_recall` as the second step.

**New package: `@loomcycle/memory-view`** — the operator Memory console as a reusable React component (scope/scope_id/key browser, entry editor, fact viewer, unified search panel, reembed flow), scoped under `.loomcycle-memory-view`. The Web UI consumes it from source. Published on its own `memory-view-v*` tag; **0.1.0 is not published yet.**

### ⚠️ Wire change

`kind` on `POST /v1/_memory/search` was `"memory" | "document"` and is now **`"fact" | "note" | "document"`**. A reader switching on `"document"` is unaffected; one asserting `kind == "memory"` must move to `fact`/`note`. The refinement is the point — "the user told me this" and "an agent jotted this down" are different claims, and collapsing them is what let document prose read back as remembered fact. `@loomcycle/client` **1.49.0** moves in lockstep.

### Upgrade note

Existing deployments benefit immediately for new writes. Scaffolding rows already embedded stay in the index until re-swept; `POST /v1/_memory/backfill_embeddings` does not remove an existing embedding, so clearing them means deleting those chunks or re-embedding the scope.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.48.0

**🔖 Heading chunks became searchable, and `@loomcycle/client` ships the memory-view SDK.** Minor: one embedding-policy change and three additive adapter methods. No schema change, no wire/proto change.

**A bodyless chunk now embeds its TITLE.** Every embedding policy derived text from the *body*, so a chunk whose body yielded none was excluded from semantic search altogether — which silently removed the most navigable part of a document from retrieval. On the reference deployment **186 of ~3,000 chunks are heading-only**, with titles like `RFC BE — History Tool (browse / search / rename / annotate past chats)` and `Phase 2 — name-links + transclusion`.

The rule applies uniformly after the per-type switch: when the derived text is empty, fall back to the title. It is a **fallback, never an addition** — appending it would double-weight whatever the author put in the heading on every chunk in the corpus, and for prose the body usually restates the heading anyway. Being uniform, it also covers a diagram whose labels were all syntax and an image with neither caption nor description, which would have made the deployment's two images findable before any vision call.

The only quality test is **"contains a letter"**, not a length. Measured before building: of 20 sampled bodyless chunks 18 had meaningful titles, and the shortest were `Active RFCs` (11 characters) and `Configuration` (13) — a length filter would discard exactly what someone searching a document would type. `---`, `42`, `1.2.3` carry no language and are dropped, so no vector is stored for them.

The admin backfill applies the same fallback, or existing documents would stay permanently less searchable than ones authored after this landed — and a bodyless chunk is unreachable any other way, since the sweep sees memory *rows* while a title lives in SQL Memory. Both surfaces route through one judgement so they cannot disagree.

**`@loomcycle/client` 1.48.0** adds three HTTP-only methods backing the memory view: `memorySearch()` (the off-run unified search over k/v entries and document-chunk bodies from v1.47.0, each hit tagged `kind: memory|document`), `memoryEmbedStats()`, and `reembedMemory()` — whose `dryRun` stays a safe dry run when omitted, since the server defaults it true. Fact reads (`list_facts`, `get_chunk`'s `entity` block) ride the existing `document()` passthrough rather than gaining bespoke methods. Additive: existing callers are unchanged.

**Why npm skips 1.47.0.** The adapter's version was bumped to 1.47.0 in the same window that tag was cut, and a publish only fires when the package version equals the tag — so `@loomcycle/client@1.47.0` never existed on npm. It is realigned to 1.48.0 here, and npm goes 1.46.0 → 1.48.0. Nothing is missing; there was never a 1.47.0 package.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched; the SDK methods are TS-only because the endpoints they wrap have no gRPC twin.

## What's in v1.47.0

**🔎 A human-facing memory view, the memory surfaces opened to tenant operators, and the last of the RFC BU sweep fixes.** Minor rather than a patch because RFC BV Phase 1 adds new surface — one HTTP endpoint and two Document ops — and the `/v1/_memory/*` family changes who can reach it. No schema change, no wire/proto change, no adapter change.

**RFC BV Phase 1 — reading the memory plane like a human would.** The entity tier stores a fact as a chunk plus a `chunk_memory_meta` sidecar (bi-temporal timelines + provenance), but nothing could *read* that sidecar in a typed way: `get_chunk` returned a chunk with no way to tell a fact from a plain section, and nothing enumerated facts for a browse surface.

- `get_chunk` now attaches an **`entity`** block when the chunk has a sidecar (omitted otherwise): raw unix-nanos timestamps, so the viewer formats them rather than the server guessing, and an always-present `retired` bool keyed on *system* time (`expired_at`) so a future world-time `invalid_at` is not misreported as retired.
- **`list_facts`** browses the scope's facts newest-first, metadata only — the viewer fetches a body on click — filterable by type/class/document_id and using the same temporal filter `graph_recall` does. Its per-fact `entity` block reuses the same renderer, so the two surfaces cannot drift.
- **`POST /v1/_memory/search`** is an off-run semantic search with an empty key prefix, so one query spans both plain k/v entries and document-chunk bodies — what answers "where did I record this" across the whole stack. Each hit is tagged `kind=memory` or `kind=document` (+`chunk_id`) so a document hit can be followed to `get_chunk`.

  Security-critical detail: the in-process backend resolves the tenant from the *run* identity, and off-run there is none — so the handler stamps the authenticated principal's tenant before searching. Without it the search would run at the shared `""` tenant and could read another tenant's rows. `TestMemorySearch_TenantIsolation` fails if the stamp is removed.

**The `/v1/_memory/*` family is reachable by tenant operators.** It was pinned to `substrate:admin` by the `/v1/_*` catch-all, so a `substrate:tenant` operator 403'd at the gate — even though memory rows carry a `tenant_id` and every handler already sources the tenant from the authenticated principal rather than the wire. The same gap the Library had in v1.6.3: the handler confines, but the route never lets a tenant token reach it. The routes now grant `ScopeTenant` and the handlers still confine a non-admin to its own tenant, with `TestHandleMemory_TenantOperatorConfined` proving a `?tenant=` naming another tenant is ignored — the invariant the re-gate rests on.

**`repair-tenant` deliberately stays operator-admin**: it rewrites rows across every scope in one statement to re-stamp the legacy `""` partition, and a cross-tenant bulk rewrite is not a tenant operator's authority. The Web UI's memory nav item moves from admin to tenant with it; **channels stays admin**, because it still has no tenant column — the reason memory used to be admin too.

**An uncaptioned image could never be embedded.** Found by verifying the v1.46.1 deploy rather than trusting its output: the describe pass reported `described=2 failed=0` with two accurate descriptions persisted, and the images stayed unsearchable while the backfill's candidate count never moved.

`embedBody` guarded on the **raw body** before the per-type switch — and an image's body *is* its caption, so an image with no caption returned early and its generated description was never consulted, no matter how many times a describe pass wrote one. The body is only one of the sources for an image, and may not be a source at all.

This is the failure mode `SetAssetDescription`'s own comment warns about, reached by a different route: `get_asset` shows a description, the sweep reports success, the row holds 472 characters — and nothing is indexed. Correct-looking from every surface an operator would check. Both existing image tests used a *captioned* chunk, which is why it went uncovered, and an uncaptioned image is the common case for an uploaded asset. The fix derives the text first and checks it after; prose and diagram behaviour is unchanged.

**Upgrade note.** If a describe pass already ran on v1.46.0/v1.46.1, its descriptions are persisted and need no second vision call — rewriting each image chunk's body re-enters the embed path and indexes it. The embedding backfill *cannot* do this: it reads the chunk-body envelope from the memory plane and has no view of `chunk_assets`, so an uncaptioned image is invisible to it.

Adapters unchanged: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.1

**🩹 Both v1.46.0 upgrade sweeps were broken in practice.** Patch release — no new surface, no schema change. Found by running the sweeps against a real deployment (154 documents, 3,143 chunks, 2 images); neither fault was visible from the unit tests, because each needs scale or a real model to appear.

**The embedding backfill starved instead of finishing.** Throughput decayed 189 / 179 / 169 / 160 / 147 embedded per 200-row window, heading for zero with ~2,300 rows still unembedded. `limit` bounded how many rows were *looked at*, not how many were embedded — and a row with no body text (a document root, a section heading) can never gain an embedding, so it stays a candidate forever. Because the query is `ORDER BY key`, those rows accumulate at the *front* of the window: the residue obeys `R' = R + p·(limit − R)`, whose fixed point is `R = limit`. Eventually the window is entirely rows that cannot be embedded and the sweep does nothing, while its own notes promise "re-invoke until candidates reaches 0" — a state it can never reach. `MemoryEmbedListMissing` now takes a keyset cursor and the handler pages with it until it has *embedded* `limit` rows, reporting `skipped_empty` and `more`.

Widening the window did not help either: the store reset a limit over 1000 back to the 200 default, so asking for 5000 returned a **smaller** page than asking for 1000. That is now a clamp.

**The describe pass truncated before it said anything.** The image sweep reported 0 failures and both images as answered-empty. The model was fine — asked directly, `qwen3.6` describes the image in 198 characters. The token ceiling was 300, and a thinking model emits its reasoning trace *first*: 1281 characters of it, `done_reason=length`, and no description at all. 300 looked ample because the same prompt answers in 84 tokens with thinking off.

Worse than a failed call: the pass recorded it as answered-empty and stamped `described_at`, whose whole purpose is to separate "a model looked and found nothing" from "nothing has looked yet" — so a code bug became a permanent fact about the data that no re-run would revisit. A turn that stopped at the ceiling is now a **failure**, left retryable, with the reason named. The ceiling is 1500, and it is a ceiling rather than a target, so a generous value costs nothing on a normal call. Deliberately *not* fixed by forcing `effort: low` (which the Ollama driver maps to `think:false`): that flag errors on a model which cannot reason, so it would break a non-thinking vision model such as llava to accommodate a thinking one.

**Also:** an admin token that names no tenant on the backfill is refused with `400 tenant_required`, mirroring the erasure, directory and orphan-repair surfaces. Memory rows are keyed on the tenant, so omitting it resolved to the *default* tenant and reported a truthful-looking `candidates: 0` against a tenant the operator never meant — demonstrated on a three-tenant deployment, where the same request with and without `?tenant=` returned 200 candidates and 0.

**Upgrade note.** If you already ran the v1.46.0 sweeps, re-run the backfill — it will now finish rather than stall. Images stamped by the buggy describe pass need clearing to become candidates again:

```sql
UPDATE chunk_assets SET described_at = NULL
 WHERE described_at IS NOT NULL AND coalesce(description,'') = '';
```

Adapters unchanged from v1.46.0: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.0

**📄 Documents became a searchable, linked knowledge store.** Two RFCs completed end to end — **RFC BS** (structure primitives: tags, links, transclusion, history, discovery, canvas) and **RFC BU** (searchable bodies: embed-on-write per chunk type, backfill, diagram and image handling) — plus a **directory** surface on every transport. No schema migration on the main store; the document scopes self-provision their own tables.

**Documents are now findable by their content.** They were not, and the reason was structural rather than a bug: the searchable half of a document (the SQL `chunks` table) has no body column, while the half holding the text (`doc.chunk:<id>` rows in Memory) had no index — `writeBody` called plain `MemorySet`. The keyword leg did not rescue it either, since full-text ranks a column on the *embeddings* table, so an unembedded row was invisible to both search paths. Neither component was faulty; the gap was in the seam, which is why neither one's tests could surface it. Chunk bodies are now embedded on write, and `memory op=search` with prefix `doc.chunk:` is the entry point.

What gets embedded is **per chunk type**, and the rule that fell out of it is worth stating: *use a model only when the content is not already text.*

| chunk type | embedded text | model? |
|---|---|---|
| prose | the body verbatim | no |
| `mermaid` | extracted node/edge labels + the diagram kind | no |
| `image` | the author's caption + a persisted vision description | for the description only |

Mermaid is text pretending to be a picture; an image is a picture. A diagram's labels are extracted deterministically across ten dialects — and the extractor is two passes, because a first version using label-shape regexes alone silently lost the content of half of them (`erDiagram` entity names are bare identifiers, `gantt` task names sit *before* the colon while the metadata follows it, `mindmap` nodes are bare indented words). An unrecognised diagram type degrades rather than vanishing.

For images, the **caption is embedded on write with no model at all**, so an image is searchable the moment it is written. A vision description is generated by an explicit operator pass (`POST /v1/_document/describe_images`, tier-resolved, dry-run by default, resumable) and **persisted** rather than regenerated: a description is model output, so regenerating yields different text, and an index that silently re-ranks on every re-embed is worse than one that is merely stale. Persisting it also makes it auditable and survives an embedder swap without a second vision call per image.

**Prose became a graph.** A chunk body's `[[name]]` links are materialised as `references` edges, re-derived on every body write and resolved through the Path dirent tree; `![[target]]` **embeds** are expanded inline at export time (transclusion), degrading to the literal text on a cycle, a depth cap or an unresolved target — a rendering nicety must never abort an export. Plus the discovery half: **`backlinks`** (what links here, manual and parser edges alike), **`related`** (semantic neighbours, a straight reuse of the new body embeddings), **`unlinked_mentions`** (chunks that name a target without linking to it), and a per-chunk **body-change log** with `history` / `get_version` / `diff`.

**Tags and document-level type/status** are first-class query axes now, in their own SQL join tables. A chunk's `fields` live in a Memory k/v blob, unreachable from SQL, so a tag stored there could never be queried — and "every draft RFC" previously meant querying root chunks. **JSON Canvas** import/export round-trips a document to the open spatial-graph format Obsidian Canvas uses.

**A directory surface**, on HTTP, MCP, gRPC, TS and Python: `users`, `inspect`, `tenants`. Read-only and derived — there is deliberately no create or update, because a "user" here is not stored (`ListUsers` is a `GROUP BY` over `runs.user_id`), so "user CRUD" has no create/update half and its delete half is already the subject-erasure surface. What was missing was the *read*: answering "what does alice actually have here" meant five calls against five surfaces, each with its own tenant-scoping rule, where getting one wrong yields not an error but a plausible number from the wrong tenant. Listing tenants is admin-only and **refuses** rather than filtering, because a filtered list still confirms the caller's own tenant in a shape indistinguishable from "you are the only tenant here".

**Two bugs worth calling out**, both found by building on the code rather than by review:

- A mermaid chunk was embedded as **raw diagram source** when created natively, while the identical diagram arriving through `import_md` was skipped. The classifier recognised only the fenced form, but a mermaid chunk *stores its bare source* — so the common path was the broken one. Behaviour now branches on the authoritative chunk type, since no content predicate can separate "a diagram" from "prose that opens with the word pie".
- The describe route calls `provider.Call` **directly** without the credential-override path, exactly like the LLM gateway — so without the same refusal it would have been a cost-isolation bypass, letting an operator-key-restricted tenant spend the operator's key. It now returns 403 `operator_key_restricted`.

**Also:** the memory extractor runs at `effort=low` **in the copy that actually ships** (the fix was in a bundle file that was not the embedded one), and an `effort=medium` evaluation baseline is recorded under the current prompt.

**Upgrade notes.** Two one-time sweeps make *existing* content searchable, and neither runs automatically by design — thousands of model calls at boot is a bill nobody approved:

```
POST /v1/_memory/backfill_embeddings?scope=user&scope_id=<subject>&prefix=doc.chunk:&dry_run=false
POST /v1/_document/describe_images?scope=user&scope_id=<subject>&dry_run=false
```

Both are resumable with no cursor: an embedded row or a described image drops out of its candidate set, so re-invoke until `candidates` reaches 0. Every chunk write now makes an embedder call — best-effort, so a cold or absent embedder logs and moves on rather than failing an author's write. Expect per-scope vector growth roughly equal to the chunk count, which brings the existing per-scope quota closer.

**The shared document viewer surfaces all of it.** `@loomcycle/explorer` gains read-only tag chips, a **Connections** panel with three lazily-fetched lists (backlinks — marking which edges came from `[[name]]` parsing — plus related and unlinked mentions, the three reads under `Promise.allSettled` so one failure never blanks the others, and a missing embedder showing a muted "not configured" note rather than an error), a **History** modal with the revision list, one revision's exact body and a unified diff, and a **Download .canvas** button beside Download .md. Board, kanban and spatial views stay a loomboard concern.

Adapters: `@loomcycle/client` **1.46.0**, `loomcycle` (PyPI) **1.46.0** — the Python adapter had been pinned at 1.38.0, so this release publishes its erasure and directory methods — and `@loomcycle/explorer` **0.6.0**.

## What's in v1.45.0

**🧹 Subject erasure on every transport, and a nine-pass audit of the memory subsystem.** One new wire surface; the rest is fixes. Additive index only (0065, from v1.44.0); no schema or wire break.

**Erasure is no longer HTTP-only.** v1.44.0 shipped it as the one substrate surface without transport parity; it now runs over MCP (an `erasure` tool, `op=report|execute`), gRPC (`ErasureReport` / `ErasureExecute`), the TS client (`erasureReport()` / `erasureExecute()`), and the Python client. The logic moved into a shared service so all four run the SAME code — an erasure that removed a different set of planes depending on which client asked would make "we erased them" a claim about a library rather than about the data. **The safety guards moved with it**: requiring `confirm` to equal the subject is a property of the erasure, not of HTTP, and leaving it to each caller would mean the newest transport is the one missing it. On MCP and gRPC the tenant comes from the principal with no wire field at all, so a session cannot reach another tenant's subject.

**Then nine review passes over the memory subsystem, which found fourteen defects.** Every one sat in state reconstruction, tenant keying, or reachability — none in transport, dispatch or gating. The ones an operator should know about:

- **A single-tenant deployment's erasure silently spared the subject's entire SQL Memory database** — every document they authored and their whole entity graph. SQL Memory rejects an empty tenant and stores the default one as `"default"`, while the k/v plane keeps the raw `""`; the drop key was built from the raw value and matched nothing. The live deployment test could not have caught it: its tenant was non-empty, where the two spellings coincide.
- **The correction chain could fork.** Nothing stopped two chunks from each superseding the same fact — both stayed live and `graph_recall` returned both as current, which is exactly what supersede-not-delete exists to prevent.
- **A known future end date deleted the fact.** "The contract runs until 2027" vanished from every default recall the moment the end date was written, because the filter required `invalid_at IS NULL`. The decisive argument is internal consistency: the default *is* "as_of now", so it must reduce to the `as_of` predicate with the current time substituted — and it did not.
- **`import_md` read fenced code as document structure.** Any document containing a Markdown sample re-imported with more chunks than it had, its body truncated at the fence. That is most technical documentation, including this project's own RFCs.
- **`create_chunk` / `move_chunk` accepted a parent that does not exist**, returning success for a chunk nothing could ever reach — absent from `get_document` and `export_md`, and invisible to the dead-link sweeper, which looks for a missing *document* rather than a missing *parent*.
- **Vector search died instead of degrading in a mixed-dimension scope** — the state migration 0017 calls the typical steady state mid-migration. One row at another dimension aborted the whole query with a raw driver error, so recall was dead for that scope until every row was re-embedded.

**Three hardening fixes where a guarantee held only by accident.** The postgres schema and LOGIN role for a SQL Memory scope are derived by hashing a separator-joined key, and the separator-free requirement was asserted in a comment rather than enforced — three distinct scopes could derive one schema and one role. It was unreachable only because the tenant component happens to come first. The SQL validator's comment-stripper was dialect-blind, so a legitimate `VALUES ($$x; y$$)` was refused while `$$--$$` hid a real statement separator — safe only because the driver's extended protocol rejects multi-statement strings, a dependency nobody had written down. And a `MemoryBackendDef` could name any env var as its credential source: `api_key_env: LOOMCYCLE_AUTH_TOKEN` beside a def-supplied `base_url` is a one-request exfiltration of the operator bearer, and the def persisted holding it.

**⚠️ Two CI gaps, and the second explains several of the above.** The pgvector round-trip contract **never ran** — it is gated behind an opt-in flag the workflow never set, even though the store job's service image is already pgvector. A skipped suite is worse than a missing one: it reports green and reads as covered. It could not have run even with the flag, because the per-test schema excluded `public` and migration 0017's `vector` type is unqualified. Both are fixed, and the postgres-tier `-run` filter — which the workflow *asks in writing* to be maintained — is now enforced by a test rather than a comment.

**🔬 One eval finding.** A change to the extractor's system prompt **un-gates every measured configuration**, invisibly: the baseline keys on the prompt hash, and no match is deliberately not a regression. `effort=medium` has been ungated since the prompt changed. The gate now says so instead of reporting a clean pass with nothing behind it.

**Adapters:** `@loomcycle/client` **1.45.0** publishes on this tag with the erasure methods. The Python client's erasure methods ship on its own `python-v*` tag.

## What's in v1.44.1

**🩹 Patch — the erasure report omitted planes it had examined.** Reporting accuracy only; nothing about what gets deleted changes.

Found by driving v1.44.0 against a live deployment rather than a fixture. A dry run for a real subject returned `{"chats":2,"interrupts":0,"memory_rows":2}` — but `credentials`, `token_limits`, `path_entries` and `sql_memory_scopes` are all examined on that path, and all four were missing. An operator reading that cannot tell *"this subject had no credentials"* from *"credentials were never looked at"*, which in a compliance report is the entire question being asked. It is the same ambiguity between FAILED and EMPTY that the `errors` field exists to prevent, reintroduced one field lower down.

Two causes. The executor accumulated with `deleted[k]++` from an absent key, so a plane that removed nothing left no trace; every considered plane is now registered at zero up front, which establishes the invariant that **a key's presence means the plane was examined and its value means how many rows went**. And the report set `sql_memory_scopes` only when it found one, so absence meant either no scope or no look.

SQL Memory is the one genuinely conditional plane: with the subsystem unconfigured it really is unexamined, so rather than omit it the report now SAYS so in `notes`. Exactly one of the two must hold — counted, or declared unexamined — and the regression test asserts that as an exclusive-or rather than checking either alone.

⚠️ **No schema change, no wire change, no adapter change.** Existing `/v1/_erasure` responses gain keys; none are removed or renamed.

## What's in v1.44.0

**🧹 A subject erasure you can run, and — more to the point — one that tells you what it did not reach.** Two new endpoints, one new store method, one additive index. The memory bundle stays opt-in.

**`GET /v1/_erasure?tenant=&subject=` reports one person's footprint in three tiers**, and the tiers are separated by what they *guarantee* rather than by what they contain:

| tier | what | deletable |
|---|---|---|
| 1 | chats, user-scope memory, the user's SQL Memory scope, path entries | ✅ existing primitives |
| 2 | credentials, token limits, interrupts, usage ledger | ❌ nothing, before this release |
| 3 | facts about the subject in a shared agent's or another user's scope | ❌ not addressable at all |

Tier 2 is listed apart because the distinction is not row count but whether anything can remove them. A subject's **encrypted credentials** surviving an "erasure" is the worst entry on that list, and it is invisible unless something counts it.

**`POST /v1/_erasure` executes tiers 1 and 2, and defaults to doing nothing.** `dry_run` is a nullable boolean on purpose: an omitted field — or `POST {}` from a client with a buggy serialiser — must mean *do nothing*, and a plain boolean would make the zero value destructive. A live run additionally requires `confirm` to equal `subject`; a subject id that matches nothing is harmless, one that matches the **wrong person** is not. An admin token must name the tenant explicitly, because a subject id is only unique within one and an unnamed tenant would not merely report the wrong people — it would delete them.

⚠️ **The residue report is one-shot, and this is the property to understand before using it.** Tier 3 is reachable only by tracing provenance from the subject's chats, and the erasure deletes those chats. Afterwards the trace handle is gone: a report run later shows `residue: 0` **while the facts are still stored**. That is not a defect to be fixed at this layer — it is what "the subject's chats are deleted" means when chats are the only index into derived facts. So the residue is measured before anything is removed, the response states that **it is the only durable record of what was not reached**, and the report closes the matching lie from its own side by rendering tier 3 **UNDETERMINABLE** rather than `0` when there are no sessions to trace from. That is exactly the state a subject is left in after an erasure, and exactly when someone would read a zero as confirmation the job is done. **Retain the erasure response.**

**The usage/cost ledger is retained by design, and says so.** Cost rows are accounting records an operator may be legally required to keep; the correct treatment is to break the personal linkage rather than destroy the totals. That is a row merge rather than a delete — `usage_archive` carries `user_id` inside its primary key, so anonymising means folding rows into the empty-string bucket and summing — and it is left for its own change rather than swept in. It appears under `retained` with the reason, because an erasure that lists only its successes reads as complete.

**Deleting an interrupt is not filtered by status.** An interrupt row holds the model's question *and* the user's free-text answer, so removing only the pending ones would report success having left the conversation behind. The new `InterruptDeleteAllByUser` is also uncapped, unlike the lister it mirrors: a partial delete that reports success is the one outcome an erasure must never produce.

**🧹 Dead chunk references are now collected.** A chunk is referenced from five places, and neither `delete_chunk` nor the read-time guard reaches all of them — the body delete runs after the transaction commits (a different store, so it cannot join the txn), an out-of-band delete bypasses the tool entirely, and the read-time guard makes an orphan *invisible*, which is precisely why nothing ever notices it. This is always-on integrity rather than opt-in policy: everything it removes is unreachable by definition. Two guards refuse rather than proceed — a fault reading the live chunk set aborts the scope, and a scope holding bodies but zero chunk rows is treated as mis-resolved rather than fully deleted, because being wrong there costs every body in the scope.

**🔬 The memory eval never sent the ontology it was scoring.** `{{memory:ontology}}` reached the model as nineteen literal characters, which explains the v1.43.0 finding about entity types outside the ontology: the model was never shown one. The A/B rerun settles it — **a confirmed ontology eliminates statement-class-as-entity-type entirely (5 → 0)**, retiring the type-validation follow-up that release proposed.

⚠️ **One additive migration (0065)** — a partial index on `memory(tenant_id, source_session_id)`. No wire change, no adapter change. The erasure surface is **HTTP-only in this release**: there is no MCP tool, no gRPC RPC, no adapter method and no Web UI page, unlike every other substrate surface. An operator drives it with `curl`.

## What's in v1.43.2

**🔎 The entity graph was correct and unfindable.** The first live consolidation pass built a graph, and inspecting it showed why the retrieval half was unusable. Runtime + bundle; no migration, no wire change.

**`graph_recall` searched every chunk in the scope, and the scope is where the document store lives.** Measured on the reference deployment: **2 entity chunks among 3,071 chunks across 150 documents**. So "what do we know about X" answered with documentation prose. Discovery now requires a `chunk_memory_meta` row — a chunk without one is prose, not part of the graph — and it is restricted for DISCOVERY only, never for explicit `seed_ids`, which exist precisely to hand in results found some other way. That is the same division the temporal filter already draws.

This is the failure mode a fixture corpus cannot show. Every test corpus is a clean room; signal-to-noise is a property of the corpus rather than the code, and 2-of-3071 is only visible where the other 3,069 exist.

**The title match was a bare substring, so `statin` matched `re-statin-g`.** Verbatim from the deployment: a query for "statin" returned a configuration paragraph reading "an overlay that puts OAuth on top WITHOUT restating the base matrix". Whole-word matching now happens in Go, because neither storage tier has a portable word boundary and the padded-LIKE trick misfires on punctuation — a title ending in a colon, or a hyphenated term, would stop matching. The LIKE remains a coarse prefilter and the fetch over-fetches, so filtering can never silently return fewer seeds than were asked for, which would read as "the graph holds nothing else" rather than "the prefilter was noisy". A query with no word character at its edges falls back to substring instead of matching nothing.

**A statement class is not an entity type.** The pass typed "the user prefers statin alternatives" as `preference:user` — the entity of type *preference* named *user*. That is not a harmless mislabel: **every preference about the user collapses onto that one node**, producing a hub that means nothing and burying the facts it should organise. An invented type such as `process` makes one strangely-labelled node; a statement class makes a magnet. `preference` and `fact` are now refused as entity types (they belong to the extractor's `class` vocabulary, and are in the ontology because the memory tier needs them as chunk types), and the refusals are COUNTED — a pass that mirrored nothing because every type was rejected must not look like a pass with nothing to mirror.

⚠️ **A claim in the v1.43.0 notes was wrong, and is corrected there.** Those notes said the extractor's inconsistent subject spelling forked one person into two graph nodes. It does not: `slug()` lowercases and its stopword list already drops `a`/`an`/`the`, so `the user`, `user` and `The user` all reduce to one key. The claim had been read out of the code rather than run — and it surfaced because the fix written for it changed nothing when removed, which is only possible if it was doing nothing. The regression test is kept regardless, because the guarantee is load-bearing and currently emerges from a stopword list nobody would think to protect: deleting the articles from it forks every subject whose name carries one.

**Still accepted, and stated rather than hidden:** entity types OUTSIDE the ontology (`profession`, `setting`, `process`, `task` all appeared in the eval). Refusing them needs the effective ontology reachable from the consolidator, which by design cannot read a tenant-scope document — a follow-up, and far lower stakes than the magnet above.

**🧰 Deploy fixes for the serve-and-test posture.** The sandbox sidecar pin is bumped so `expose` actually works: #891 set `SANDBOX_EXPOSE_NETWORK` but left the sidecar on an image predating the expose seam it added in the same change, so the old sidecar silently dropped the unknown field, opened a plain session and returned no exposed host — a serve-and-test run "succeeded" while the browser could not resolve the alias. The rest of the family had drifted too. And the TrueNAS deploy gains a **browser + sandbox variant**: build a feature in an isolated sandbox, serve it, and test it in a headless browser in one run, which was previously reachable only in the cloud deployment.

⚠️ **No migration, no schema change, no wire change.** Adapters unchanged. The memory bundle remains opt-in with its schedule at `enabled: false`. If you have already run a pass, entity nodes typed `preference:` or `fact:` from before this release are still in the store — they are inert rather than harmful (nothing reads them as entities now) but worth deleting if you want a clean graph.

## Earlier releases

Notes for **v1.43.1 back to v0.4.0** are not repeated here.

They were dropped from this file deliberately rather than lost: every tag carries
its own annotated message, and every release has a GitHub page with the same
notes. This file had grown to 185 sections and ~700 KB, which made it useless for
its actual job — telling you what changed *recently*.

- **Per-release notes:** <https://github.com/denn-gubsky/loomcycle/releases>
- **From a checkout:** `git tag --sort=-v:refname` to list, `git show <tag>` for one
- **What a version contains:** `git log --oneline <older>..<newer>`

For the shape of the runtime as it stands rather than how it got there, see
[`README.md`](README.md); for where it is going, [`docs/PLAN.md`](docs/PLAN.md).
