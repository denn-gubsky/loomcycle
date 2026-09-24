---
name: Memory
description: "Memory tool — durable storage that survives across runs: key/value entries, remembered facts and notes (add/recall/search), a per-scope SQL database, and the machinery background consolidation uses."
---
The `Memory` tool stores things that must **outlive this run**: a user's
preferences, a counter, a lesson for your future self, facts distilled from
past conversations, or tables you query with SQL. What you write here is still
there in the next run and the next session.

It has several facets behind one tool, chosen by `op`:

- **Key/value** — you name a key and read or write its JSON value.
- **Facts and notes** — hand over conversation with `add`, find remembered
  things by meaning with `recall` or `search`.
- **SQL Memory** — a real SQL database per scope, for structured data.
- **Cursors** and the **pending queue** — bookkeeping that a background
  consolidation agent uses. An ordinary agent rarely needs them.

## When to use it — and when not

- **Use Memory** to keep something beyond this run, or to find something an
  earlier run remembered.
- **Recall** (a separate tool) fetches details from earlier in THIS
  conversation that were summarised away. It is read-only. To store a durable
  fact, use Memory.
- **History** browses past chats themselves: list them, read a transcript,
  resume one. Use it when you need what was actually said in a specific
  conversation — for example the chat a recalled fact names in
  `source_session_id`.
- **Document** holds chunked, structured documents that people and agents
  co-author, with headings, links and a fact graph. Use it for long-form,
  organised content. Memory is for individual values and remembered facts.

**`add` is asynchronous — a later `recall` may not see it yet.** It queues the
conversation for a background consolidator, which distils durable facts later.
When you need to read something back straight away, use `set` and `get`.

## Operations

Every call takes `op` and `scope`. Fetch one operation's article, with
examples, as `Memory/<op>` — for example `{"op":"help","topic":"Memory/set"}`.

**Key/value**

| op | What it does | Required besides `op`, `scope` |
|---|---|---|
| `get` | read one entry | `key` (or `path`) |
| `set` | write or overwrite one entry; optional TTL, embedding, Path name | `key`, `value` |
| `delete` | remove one entry | `key` |
| `list` | list entries, optionally by key prefix | — |
| `incr` | atomically add to a number | `key` |
| `merge` | atomically overlay fields onto a JSON object | `key`, `value` |
| `append_dedupe` | atomically add an item to a JSON array unless present | `key`, `value` |
| `bounded_list` | atomically append to a JSON array, keeping the newest N | `key`, `value`, `limit` |

**Facts and notes**

| op | What it does | Required besides `op`, `scope` |
|---|---|---|
| `add` | hand over conversation turns to be distilled into facts (async) | `messages` |
| `recall` | find remembered facts and notes by meaning | `query` |
| `search` | semantic search over stored rows, with ranking and filters | `query` |
| `placement` | ask which scope a batch of facts belongs in; writes nothing | `items` |
| `supersede` | retire a fact without deleting it (consolidation grant) | `key` |

**SQL Memory** — a separate grant, `sql_scopes`

| op | What it does | Required besides `op`, `scope` |
|---|---|---|
| `sql_query` | run one read-only `SELECT` | `statement` |
| `sql_exec` | run one DDL/DML statement | `statement` |
| `sql_begin` | open a transaction (or nest a savepoint) | — |
| `sql_commit` | commit the innermost open level | — |
| `sql_rollback` | roll back the innermost open level | — |

**Consolidation machinery** — needs the consolidation grant

| op | What it does | Required besides `op`, `scope` |
|---|---|---|
| `cursor_get` | read the target's watermark and lease | — |
| `cursor_scan` | list finished chats not consolidated yet, oldest first | — |
| `cursor_lease` | take the target's lease so no other pass works it | — |
| `cursor_advance` | move the watermark past a chat you consolidated | `completed_at`, `session_id` |
| `cursor_release` | give the lease back | — |
| `pending_drain` | read queued items waiting to be consolidated | — |
| `pending_ack` | mark drained items done | `ids` |

## Scopes

**`scope` is required on every call** — there is no default. One call reads
and writes only the scope it names.

- Key/value, facts and consolidation ops accept `agent`, `user` or `tenant`,
  gated by the agent's `memory_scopes`.
- SQL ops accept `agent`, `user`, `tenant` or `run` (a scratch database
  dropped when the run ends), gated by the agent's `sql_scopes`. That is a
  **separate grant**: having Memory does not give you SQL. `run` is refused on
  every non-SQL op.

A `user`-scope recall does not see `tenant` knowledge, and the other way
round; to consult both, make two calls. `Context op=self` reports which scopes
this run may use. For the full model, read `{"op":"help","topic":"scopes"}`.

## Values, sizes and times

- `value` is any JSON: an object, array, string, number or boolean.
- `ttl` is in seconds; omit it for no expiry.
- An operator can cap one value's size and a scope's total size. A write that
  would go over either is refused, with the numbers in the message.
- Timestamps (`observed_at`, `valid_at`, `invalid_at`, `when.from`, …) are
  RFC3339, e.g. `2026-03-14T09:00:00Z`.
