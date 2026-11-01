# loomcycle — async Python client

`loomcycle` is the async Python client for [loomcycle][1]'s gRPC API
(introduced in v0.5.5). As of **v1.1.1 it covers all 42 gRPC RPCs** —
run streaming + continuation, the RFC AI **interactive session**
(`run_input` + `stream_run` + an `interactive=True` flag), batch
fan-out, run compaction, agent metadata + transcript, pause / resume /
state, the snapshot lifecycle, the resolver probe, the full
substrate-def family (incl. RFC AH volume_def), channel publish /
subscribe / peek / ack / await / broadcast, and run-state streaming —
through an ergonomic `LoomcycleClient` class with no need to import
generated protobuf types in your application code.

[1]: https://github.com/denn-gubsky/loomcycle

## Status

- Wraps loomcycle's gRPC server (`LOOMCYCLE_GRPC_ADDR`).
- Async-only (`grpc.aio`). Python 3.9+.
- **Full parity with the gRPC service surface** (42 RPCs).
- The TypeScript adapter (`adapters/ts/`) additionally exposes
  **HTTP-only** operations that have no gRPC RPC — memory-entry admin,
  interruptions, library enumeration, the LLM gateway, and
  whoami / list-users. Those are not reachable over gRPC and so are not
  in this client; use the HTTP+SSE surface for them.
- Production tag: `1.1.1` (42-RPC parity, version-aligned with the
  loomcycle v1.1.x line; ships on the `python-v1.1.1` tag).

## Install

```bash
pip install loomcycle
```

## Quick start

```python
import asyncio
from loomcycle import LoomcycleClient

async def main():
    async with LoomcycleClient(
        target="127.0.0.1:8788",
        auth_token="<LOOMCYCLE_AUTH_TOKEN>",
    ) as client:
        async for ev in client.run_streaming(
            agent="default",
            segments=[
                {
                    "role": "user",
                    "content": [
                        {"type": "trusted-text", "text": "Summarize loomcycle in one sentence."}
                    ],
                }
            ],
        ):
            if ev.type == "text":
                print(ev.text, end="", flush=True)
            elif ev.type == "tool_use":
                print(f"\n[tool_use: {ev.tool_use.name}]", flush=True)
            elif ev.type == "done":
                print(f"\n[done: {ev.stop_reason}]")

asyncio.run(main())
```

## Capturing the run handle

The gRPC server's first two stream frames are synthetic
registration frames — `LoomcycleClient` swallows them and exposes
the captured IDs via an `on_handle` callback:

```python
from loomcycle import LoomcycleClient, RunHandle

async def main():
    handle: RunHandle | None = None

    def capture(h: RunHandle):
        nonlocal handle
        handle = h
        print(f"agent_id={h.agent_id} session_id={h.session_id} run_id={h.run_id}")

    async with LoomcycleClient(target="127.0.0.1:8788") as client:
        async for ev in client.run_streaming(
            agent="default",
            segments=[...],
            on_handle=capture,
        ):
            ...

    # Use handle.session_id later to continue or read transcript.
```

## API

All methods are coroutine methods on `LoomcycleClient`.

| Method | Returns | Notes |
|---|---|---|
| `run_streaming(agent, segments, ...)` | `AsyncIterator[AgentEvent]` | Server-streams provider events for a fresh run. |
| `continue_session(session_id, segments, ...)` | `AsyncIterator[AgentEvent]` | Continues an existing session. |
| `get_agent(agent_id)` | `dict` | One agent's status + usage. |
| `get_run(run_id)` | `dict` | One run by its run id — `get_agent`'s shape; use it when an agent id names several runs (every walk of a team is `team:<name>`). |
| `cancel_agent(agent_id, reason="")` | `int` | Returns count of agents cancelled (cascades to children). |
| `list_user_agents(user_id, status="", awaited_state=())` | `AgentList` (a `list[dict]`) | `status`: one string (`running`, `completed`, `failed`, `cancelled`, …; `""` = all) or a list of them (a run in any; `["all"]` = all). `awaited_state`: one or more of `channel`, `interrupted`, `review`, `input`, `children` — only running runs blocked on one of those waits; the server reads at most 1,000 running runs and `.truncated` is True when it left some unread. An unknown value raises `InvalidArgumentError`. |
| `list_walk_runs(walk_id, limit=None, cursor=None)` | `dict` | One page of a team walk's runs — its own run and every member, oldest first: `{"agents": [...], "next_cursor": str}`. Pass `next_cursor` back as `cursor`; `""` on the last page. `limit` defaults to 100, at most 1000. |
| `list_child_runs(parent_run_id, limit=None, cursor=None)` | `dict` | One page of a run's direct children (the runs it started itself), of every status, oldest first, paged like `list_walk_runs`. Not the run itself, and not its grandchildren. |
| `get_transcript(session_id)` | `list[dict]` | Persisted event log; `payload` is raw JSON bytes. |
| `health()` | `dict` | Liveness + build info. Unauthenticated. |
| `get_config()` | `dict` | v1.38.0 — instance configuration: build identity, the feature matrix, and the live provider/model/search cascade with `active`/`selected`. `view` names the disclosure level (`authenticated` or `admin`; the HTTP surface's narrower `public` level does not exist over gRPC, which authenticates before dispatch). |
| `hook_def(input)` | `dict` | Reusable hook definitions (create / fork / get / list / promote / retire / verify / delete). An agent attaches hooks in its own definition. |
| `close()` | `None` | Idempotent. Use `async with` to do this automatically. |
| `pause_runtime(timeout_ms=0)` | `dict` | v0.8.18 — quiesce the runtime. Returns `{status, duration_ms, force_cancelled_count, paused_runs_count, warnings}`. |
| `resume_runtime()` | `dict` | v0.8.18 — release the quiesce. Returns `{status, resumed_run_count, warnings}`. |
| `get_runtime_state()` | `dict` | v0.8.18 — current state. Returns `{status, paused_at, paused_run_count, snapshots_count}`. |
| `create_snapshot(description="", include_history=False, since_ts=None, max_bytes=0)` | `dict` | v0.8.18 — capture running-state JSON envelope. |
| `list_snapshots()` | `list[dict]` | v0.8.18 — metadata only; up to 200 most-recent. |
| `get_snapshot(snapshot_id)` | `dict` | v0.8.18 — full envelope including `json_content` bytes. |
| `export_snapshot(snapshot_id)` | `dict` | v0.8.18 — canonical bytes via `raw_json` for streaming consumers. |
| `restore_snapshot(snapshot_id=..., raw_json=..., include_history=False)` | `dict` | v0.8.18 — exactly one of `snapshot_id` / `raw_json`. Per-section counters returned. |
| `delete_snapshot(snapshot_id)` | `bool` | v0.8.18 — idempotent; returns True. |
| `spawn_run_batch(spawns, mode="join", timeout_ms=0)` | `dict` | v0.8.0 — spawn up to 32 runs concurrently (RFC Y); index-aligned `{spawned, results}`, per-child failures in-envelope. |
| `compact_run(run_id, reason="")` | `dict` | v0.8.0 — summarize a parked run's context. `{run_id, compacted, before_tokens, after_tokens, applied}`. |
| `delete_run_tree(run_id="", session_id="")` | `dict` | Permanently delete a run and every run it spawned (or, with `session_id`, every run of a session), with what is stored about them. The usage ledger is kept. Needs the `runs:delete` scope. `{run_id, session_id, deleted_runs, counts, kept, not_reached}`; an unknown, already deleted or unreadable root returns `deleted_runs: 0`. A tree with a live or paused run deletes nothing and raises `FailedPrecondition`. |
| `resolve_probe()` | `dict` | v0.8.0 — resolver provider/model availability matrix. |
| `decide(state, questions, model="")` | `dict` | Ask a decision model typed questions (`choice` / `noul` / `score`) about `state`. `{model, provider, served_model, answers, usage}`; each answer is a dict under its question's name. See [Decision models](#decision-models). |
| `list_decision_models()` | `dict` | The decision models a `decide` call may name, and the default: `{"default": str, "models": [{name, provider, model, limits}]}`. |
| `agent_def(input)` / `skill_def(input)` | `dict` | Substrate AgentDef / SkillDef tool; op-discriminated body. |
| `mcp_server_def` / `schedule_def` / `a2a_server_card_def` / `a2a_agent_def` / `webhook_def` / `memory_backend_def` / `operator_token_def` `(input)` | `dict` | v0.8.0 — the rest of the substrate-def family; same shape + `SubstrateToolRefusedError` contract. |
| `client_def(input)` | `dict` | RFC FL — the apps that sign people in through this server's gates, over the `ClientDef` RPC; op-discriminated (create / update / get / list / retire / verify), `client` holds the same fields as a config `auth.clients` entry. A tenant operator registers its own tenant's apps, an admin any tenant's; config clients are read only. Same `SubstrateToolRefusedError` contract. The sign-in flow itself is the TypeScript adapter's. |
| `volume_def(input)` | `dict` | v0.9.0 — RFC AH dynamic filesystem-volume substrate; op-discriminated (create / get / list / delete / purge), tenant-confined, same `SubstrateToolRefusedError` contract. |
| `group_def(input)` / `acl_def(input)` / `acl_binding(input)` / `authz(input)` | `dict` | Access control (RFC EY / EZ) over the `GroupDef` / `AclDef` / `AclBinding` / `Authz` RPCs. `input` is the HTTP route's JSON body plus `op` (and an admin's `tenant` focus); the answer is the route's body. Ops: groups `list` / `get` / `effective` / `create` / `delete` / `add_member` / `remove_member`; lists `create` / `update` / `put` (one upsert) / `get` / `list` / `retire` / `validate` / `catalog` / `names`; bindings `list` / `create` / `delete`; decisions `evaluation` / `evaluations` / `explain` / `simulate`. Groups, lists and bindings need a tenant operator token; the decisions are open to any principal about itself. Config-declared rows carry `"source": "config"` and are read-only. A refusal raises `AclRefusedError` (`AclDeniedError` when a rule decided) — see [Errors](#errors). |
| `evaluation(input)` | `dict` | The Evaluation tool outside a run — score runs and read the scores back; op-discriminated (submit / get / list_for_run / list_for_def / aggregate). `substrate:tenant` may use every op in its own tenant, admin in every tenant; any other token only `submit_self`, which a call from outside a run cannot use, so each op raises `SubstrateToolRefusedError`. Another tenant's id reads like a missing one. |
| `submit_evaluation(run_id, score, dimensions=None, judgement=None, rationale=None)` / `get_evaluation(eval_id)` / `list_evaluations_for_run(run_id, limit=None)` / `list_evaluations_for_def(def_id, limit=None)` / `aggregate_evaluations(def_id, include_lineage=False)` | `dict` | `evaluation` with keyword arguments; each sends only the options given. |
| `merge_subjects(duplicate, survivor, tenant="")` | `dict` | Fold one ontology subject into another (each a document id or natural key) over the `MergeSubjects` RPC, mirroring `POST /v1/_ontology/subjects/merge`: facts move, statements are copied onto the survivor (originals expired as merged), the duplicate stays retired with `fields.merged_into`, and a user the duplicate is bound to is bound to the survivor (`principal_conflict` when the survivor is bound to another). `{tenant, duplicate, survivor, facts_moved, statements_repointed, statements_retired, statements_left, not_announced}`. Needs `substrate:tenant`; `tenant` is an admin's focus. |
| `bind_subject_principal(subject, principal, tenant="")` | `dict` | Bind an ontology subject (document id, root chunk id or natural key) to the user it stands for, or clear it with `principal=""`, over the `BindSubjectPrincipal` RPC, mirroring `POST /v1/_ontology/subjects/principal`. The user must be registered in the tenant (any status). Erasing that user then reports the subject, unbinds it and announces `erased`; `erasure_report` / `erasure_execute` list such subjects in `bound_subjects`. `{tenant, document_id, root_chunk_id, natural_key, path, principal, previous, changed}`. Needs `substrate:tenant`; a refusal's `.reason` is `subject_not_found`, `not_a_subject`, `binding_conflict`, `principal_not_found`, `forbidden`, `invalid_request` or `unavailable`. |
| `list_review_queue(filter=None, summarize=False, limit=0, after="", order="", scope="", scope_id="", tenant="")` | `dict` | The statement review queue over the `ListReviewQueue` RPC, mirroring `GET /v1/_statements/review`: proposed relations narrowed by `filter` (`method` prefix, `kind(s)`, `actor`, `from_/to_document_id`, `from_/to_under_path`, `min_confidence`, `created_before/after` as unix ns or an aware `datetime`, `has_judge_verdict`, `judge_verdict`, and the exact `method_exact`, `kind_exact`, `from_path_prefix`, `from_unfiled`; an unknown key raises `ValueError`), or with `summarize=True` counts by kind × method × from-path prefix × judge verdict, each group with the `filter` that selects exactly it. `{tenant, scope, statements, truncated, next_cursor[, summary, total]}`. |
| `rule_statements(action, ids=None, filter=None, reason="", dry_run=True, expect_count=None, max=0, scope="", scope_id="", tenant="")` | `dict` | Accept or reject up to 1000 statements at once over the `RuleStatements` RPC, mirroring `POST /v1/_statements/rule`. A dry run by default (`matched`, `sample`, `breakdown`); rule with `dry_run=False` and `expect_count` = the dry run's `matched` (`.reason == "queue_changed"` otherwise). `reason` is required to reject; each statement records `ruled_by: bulk:<ruling_id>`. For an admin, a tenant operator, or a member holding the access-control action `review` on the from-documents; others' statements are skipped and counted. |
| `revert_ruling(ruling_id, scope="", scope_id="", tenant="")` | `dict` | Undo a bulk ruling, or a review policy's, over the `RevertRuling` RPC, mirroring `POST /v1/_statements/revert`: statements nothing changed since go back to their status before; changed ones are listed in `items` (error `changed`) and left. `{tenant, scope, ruling_id, action, reverted, changed, skipped, items, revert_id}`. |
| `list_rulings(limit=0, after="", scope="", scope_id="", tenant="")` | `dict` | The scope's bulk rulings and reverts, newest first, over the `ListRulings` RPC, mirroring `GET /v1/_statements/rulings` (limit 20 by default, at most 100). Each: action, `ruled_by`, `actor`, reason, counts, `selector` (`filter`, or `ids_count` — never the ids; `reverts` on a revert), `reverted` / `reverted_by`. An operator sees all, a member its own. `scope_id` (with `scope="user"`, also on the three methods above) names another subject's user scope: an admin's or a tenant operator's, within its tenant. |
| `apply_review_policy(filter=None, dry_run=True, expect_count=None, max=0, scope="", scope_id="", tenant="")` | `dict` | Run the tenant's review policy (the ontology's Review section) over the proposals already in the queue, over the `ApplyReviewPolicy` RPC, mirroring `POST /v1/_statements/apply_policy`. A dry run by default (`matched`, `accept` / `reject` / `judge` / `human`, per-rule `rules`, `sample`); apply with `dry_run=False` and `expect_count` = the dry run's `matched`: at most `max` (1000) accepts and rejects as one ruling (`ruling_id`, undone by `revert_ruling`), each recording `ruled_by: policy:<rule>@<ontology version>`. An admin or a tenant operator only (`.reason == "forbidden"` for a member). |
| `crypto(input)` | `dict` | The Crypto tool outside a run — encrypt / decrypt / rewrap / mac / verify_mac / sign / verify / public_key / totp / hash / random with keys the runtime holds; op-discriminated, and key material is never returned. Needs `substrate:tenant`; uses your tenant's keys and your own user keys, never an agent's. An admin may add `tenant` to use another tenant's keys. A refusal (including a failed decrypt, which never says why) raises `SubstrateToolRefusedError`; a spent token rate raises `BackpressureError` (`reason == "rate_limited"`, retryable). |
| `run_input(run_id, text)` | `dict` | v1.1.1 — RFC AI; steer a live interactive run. `{run_id, delivered}`. NotFound (`AgentNotFoundError`) / ResourceExhausted (`BackpressureError`) on a gone run / full queue. |
| `stream_run(run_id, from_seq=0)` | `AsyncIterator[AgentEvent]` | v1.1.1 — RFC AI; re-attach to a run's events (replay-then-tail). Operator turns replay as `steer` events (`user_input.source=="replay"`). Pair with `interactive=True` on `run_streaming` / `continue_session`. |
| `list_channels()` | `list[dict]` | v0.8.0 — declared + runtime channels with aggregate stats. |
| `publish_channel(channel, payload, scope="global", scope_id="", deliver_at="")` | `dict` | v0.8.0 — publish raw-JSON `payload` (bytes); `deliver_at` defers. |
| `subscribe_channel(channel, ...)` / `peek_channel(channel, ...)` | `dict` | v0.8.0 — long-poll / non-destructive read; `{messages, next_cursor?}`. |
| `ack_channel(channel, cursor, ...)` | `bool` | v0.8.0 — commit a channel cursor. |
| `await_channels(channels, mode="any", n=0, ...)` | `dict` | v0.8.0 — fan-in across channels (any / all / at_least). |
| `broadcast_channels(channels, payload, ...)` | `dict` | v0.8.0 — fan-out one payload to N channels. |
| `stream_user_run_states(user_id, statuses=None, agent="", walk_id="", parent_run_id="")` | `AsyncIterator[dict]` | v0.8.0 — stream a user's run-state transitions. `walk_id` (1.78.0) narrows it server-side to the runs one team walk spawned; each event carries `parent_context` (`None` outside a walk) with `walk_id` / `state` / `state_visit` (every member) and `wave_id` / `wave_index` (a starter's wave). |

`run_streaming` / `continue_session` / each `spawn_run_batch` child also accept
per-run `sampling` and `compaction` dict overrides (v0.8.0); an explicit
`temperature: 0.0` is preserved as deterministic.

## Decision models

A **decision model** answers typed questions about a piece of text and returns
each answer with its probabilities. It does not write: it reads the `state` you
give it and answers a choice, a yes/no or a score, so there is no reply to parse
and a call costs a few output tokens. Use it to route, gate, rank or grade text
you already have. No run is involved; the tokens are charged to the caller and
count against its token budget.

```python
out = await client.decide(
    {"ticket": "My invoice for March was charged twice and I want my money back."},
    {
        "route": {
            "type": "choice",
            "instructions": "Which team should handle this ticket?",
            "criteria": {"billing": "invoices, refunds, charges", "support": "bugs, outages", "sales": None},
        },
        "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"},
        "detail": {
            "type": "score",
            "instructions": "How complete is the problem report?",
            "criteria": ["no detail", "some detail", "everything needed"],
        },
    },
)
out["answers"]["route"]   # {"type": "choice", "choice": "billing", "probabilities": {...}, "confidence": 0.91}
out["answers"]["urgent"]  # {"type": "noul", "noul": 0.316}
out["answers"]["detail"]  # {"type": "score", "score": 1.87, "legend": {...}, "probabilities": {...}, "confidence": 0.68}
out["usage"]              # {"input_tokens": 1059, "output_tokens": 4}
```

`state` and each question's `criteria` are plain dicts and lists, and each
answer comes back as a dict: the JSON the wire carries is encoded and decoded
for you.

| `type` | `criteria` | Answer |
|---|---|---|
| `choice` | required — a dict: each key is an option, its value describes it (`None` when the key explains itself) | `{type, choice, probabilities, confidence}` |
| `noul` | optional — a dict describing `"true"` and/or `"false"` | `{type, noul}` — the probability of YES, 0 to 1 |
| `score` | required — a list of level descriptions, **lowest first** | `{type, score, legend, probabilities, confidence}` — `score` is the expected position, counted from 0 |

- **`noul: 0` is an answer** (a definite no), not a missing one — test it with
  `"noul" in answer`, never truthiness.
- **The probabilities are not calibrated.** Compare options within one answer;
  do not treat a fixed threshold as a guarantee.
- **An answer is the model's own JSON**, decoded and otherwise untouched, so a
  number is the number the model wrote and a field a later model adds is there
  to read.
- Leave `model` out for the deployment's default; name one only from
  `list_decision_models()`.

A refusal raises with the decision's code on `e.reason`:

| `e.reason` | gRPC code → exception | What to do |
|---|---|---|
| `invalid_input` / `bad_question` / `bad_options` / `too_many_questions` | `INVALID_ARGUMENT` → `InvalidArgumentError` | Fix the request; the message names the fault. |
| `model_not_allowed` | `INVALID_ARGUMENT` → `InvalidArgumentError` | Leave `model` out, or use a name from `list_decision_models()`. |
| `prompt_too_large` | `INVALID_ARGUMENT` → `InvalidArgumentError` | The request does not fit the model's context and is never shortened for you: shorten `state` or ask fewer questions. |
| `operator_key_restricted` | `PERMISSION_DENIED` → `LoomcycleError` | The caller may not use the operator's provider key and has none of its own. Do not retry. |
| `token_limit_exceeded` | `RESOURCE_EXHAUSTED` → `BackpressureError` (`retryable` False) | The caller is at a hard token budget. Retrying does not help until it is raised or the month rolls over. |
| `model_not_found` | `FAILED_PRECONDITION` → `LoomcycleError` | The provider does not serve that model; the same call fails again. |
| `decision_not_configured` | `FAILED_PRECONDITION` → `LoomcycleError` | This deployment declares no decision models. Do not retry. |
| `timeout` | `DEADLINE_EXCEEDED` → `LoomcycleError` | Send the same call again (`retryable` is True); if it repeats, make it smaller. |
| `call_failed` | `UNAVAILABLE` → `UnavailableError` | Try once more (`retryable` is True). |

```python
from loomcycle import LoomcycleError

try:
    out = await client.decide(state, questions)
except LoomcycleError as e:
    if e.reason == "prompt_too_large":
        ...  # shorten state and retry
    else:
        raise
```

## Errors

Every method translates gRPC error codes to typed Python exceptions:

| gRPC code | Exception |
|---|---|
| `NOT_FOUND` (with session in msg) | `SessionNotFoundError` |
| `NOT_FOUND` (otherwise — agent ctx) | `AgentNotFoundError` |
| `FAILED_PRECONDITION` (session busy) | `SessionBusyError` |
| `FAILED_PRECONDITION` (other) | `LoomcycleError` |
| `ALREADY_EXISTS` | `AgentIDInUseError` |
| `RESOURCE_EXHAUSTED` (snapshot) | `SnapshotTooLargeError` (v0.8.18) |
| `RESOURCE_EXHAUSTED` (other) | `BackpressureError` (`retryable` unless `.reason` is `token_limit_exceeded` or it is the configured-run cap) |
| `UNAUTHENTICATED` | `AuthError` |
| `UNAVAILABLE` (pause not configured) | `PauseNotConfiguredError` (v0.8.18, subclass of UnavailableError) |
| `UNAVAILABLE` (other) | `UnavailableError` |
| `NOT_FOUND` (with snapshot ctx) | `SnapshotNotFoundError` (v0.8.18) |
| `FAILED_PRECONDITION` (already pausing) | `AlreadyPausingError` (v0.8.18) |
| `FAILED_PRECONDITION` (not paused) | `NotPausedError` (v0.8.18) |
| `FAILED_PRECONDITION` (snapshot version) | `SnapshotVersionError` (v0.8.18) |
| `INVALID_ARGUMENT` / `INTERNAL` / other | `LoomcycleError` |

All exceptions inherit from `LoomcycleError` and preserve the
original `grpc.StatusCode` on `.code` for log correlation.

**Access-control refusals.** `group_def` / `acl_def` / `acl_binding` /
`authz` answer a refusal as a tool result, not a gRPC status, and the client
raises `AclRefusedError` (a `SubstrateToolRefusedError`): `.message` is the
server's sentence, `.reason` its code (`insufficient_scope` with
`.required_scope`, `acl_self_elevation`, `authz_subject`,
`acl_binding_invalid`, `acl_invalid`, `group_external`, `*_not_found`,
`*_exists`, `unknown_op`, …) and `.body` the whole error body; `.retryable`
follows the server's `error_info`. A rule's refusal (`acl_denied`) is the
subclass `AclDeniedError`, with `.action`, `.resource` and `.reasons` (the
rules that decided, denies first; empty when none matched) — the same class
name the TypeScript client raises. A bearer the RPC's own gate refuses is a
gRPC status as for any RPC (`AuthError`, or a `LoomcycleError` with
`PERMISSION_DENIED`).

```python
from loomcycle import AclDeniedError, AclRefusedError

try:
    await client.acl_def({"op": "put", "name": "ops", "rules": rules})
except AclDeniedError as e:
    print(f"{e.action} {e.resource} denied by", [(r["acl"], r["rule"]) for r in e.reasons])
except AclRefusedError as e:
    print(e.reason, e.message)
```

One gRPC code covers several conditions (`RESOURCE_EXHAUSTED` is a full queue,
a per-user quota or a token budget), so when the server says which one it is,
that name is on `.reason` — `token_limit_exceeded`, `backpressure`,
`model_not_allowed`, … — the same string the HTTP surface puts in an error
body's `code`. Branch on `.reason` rather than on the message. It is `None`
when the server attached none.

**`e.retryable`: can the same call succeed later?** Every exception has a
read-only `retryable`, judged from its class and `.reason`. It is `True` only
for a refusal that clears on its own:

- a `BackpressureError` whose `.reason` is `backpressure`,
  `per_user_quota_exhausted`, `rate_limited` or absent (a full queue, the
  caller's own run cap, starting runs too fast);
- an `UnavailableError` whose `.reason` is `runtime_paused`,
  `tier_unavailable` or `pin_unavailable` (the operator paused the runtime;
  no provider in the tier is up right now);
- a `decide()` refusal whose `.reason` is `call_failed` (`UnavailableError`:
  the decision-model call did not complete) or `timeout` (`LoomcycleError`
  from `DEADLINE_EXCEEDED`: it did not answer in time).

It is `False` for everything else — in particular a `BackpressureError` with
`.reason == "token_limit_exceeded"`, and the configured-run cap (a
`BackpressureError` whose message ends "start or discard one first": the user
already holds as many configured runs as allowed, and only starting or
discarding one makes room; the server sends no reason for it over gRPC, so it
is recognised by that message). Every `RESOURCE_EXHAUSTED` raises
`BackpressureError`, so `except BackpressureError: retry()` alone retries a
hard token budget for the rest of the month; test `e.retryable` first. A
`UnavailableError` with no reason may be a dropped connection (worth a retry)
or a feature the server does not have (not), which `retryable` cannot tell
apart, so it is `False`: retry it on your own judgement. The default is
`False` because a wrong `True` turns a refusal that never clears into an
endless loop. The client never retries on its own, so bound your loop.

```python
import asyncio

from loomcycle import LoomcycleError

for attempt in range(5):
    try:
        async for ev in client.run_streaming(...): ...
        break
    except LoomcycleError as e:
        if not e.retryable or attempt == 4:
            raise  # e.g. e.reason == "token_limit_exceeded": do not retry
        log.warning("loomcycle refused (reason=%s), retrying: %s", e.reason, e.message)
        await asyncio.sleep(2 ** attempt)
```

## Allowed-hosts semantics

`allowed_hosts` mirrors the HTTP API's narrowing semantics:

| Value | Effect |
|---|---|
| `None` (default) | No narrowing; the operator's static allowlist is the floor. |
| `[]` | Deny-all; the agent gets no network access. |
| `["foo.com"]` | Intersection with the operator's static list. |

This is enforced server-side in `internal/tools/builtin/narrowing.go`;
`allowed_hosts` is a trust boundary — it must come from your
application code, never from a model.

## Verifying a signed hook delivery

A HookDef whose webhook body names a `signing_secret` signs every call with
`X-Loomcycle-Signature: t=<unix>,v1=<hex HMAC-SHA256 of t + "." + raw body>`.
**Only the body is signed.** It carries `delivery_id` (a retry keeps it),
`hook_name` and `owner`; the `X-Loomcycle-Delivery` and `X-Loomcycle-Hook`
headers are unsigned copies a replayer can change, so dedupe and identify the
hook from the verified body, never from them. Verify the **raw** body, before
parsing it:

```python
from loomcycle import verify_loomcycle_delivery

raw = request.get_data()  # the exact bytes received
body = verify_loomcycle_delivery(raw, request.headers.get("X-Loomcycle-Signature", ""), SECRET)
if body is None:
    abort(401)
if already_seen(body["delivery_id"]):  # keep ids at least 300 s
    return previous_answer(body["delivery_id"])
```

`verify_loomcycle_delivery` returns the parsed body, or `None` when the
signature fails or the body has no `delivery_id`/`hook_name`.
`verify_loomcycle_signature` is the bare check: `True` only for a signature
made within `tolerance_sec` (default 300) of now; a malformed header, a stale
or future `t`, or a wrong MAC is `False`. Standard library only; the compare
is `hmac.compare_digest`. Give each receiver its own signing secret, so a
delivery captured at one cannot be replayed to another.

## Development

```bash
# One-time setup:
python3 -m venv adapters/python/.venv
adapters/python/.venv/bin/pip install -e adapters/python[dev]

# Run tests (offline):
make python-test

# Regenerate stubs after editing proto/loomcycle.proto:
make python-proto
```

The package commits its generated `loomcycle_pb2.py` /
`loomcycle_pb2_grpc.py` so end users don't need a working `protoc`
to install. Re-run `make python-proto` whenever the proto changes.

## Live integration test

To run the example end-to-end against a local loomcycle:

```bash
# In one shell — start loomcycle with gRPC enabled:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
./bin/loomcycle --config loomcycle.yaml

# In another shell — run the example:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
adapters/python/.venv/bin/python examples/python-cli/main.py
```

## License

Apache-2.0. Same as loomcycle.
