---
name: scopes
description: "Scopes — agent, user, tenant (and run): which store a call reads or writes, what each resolves to, which tools use which, and what this run is granted."
---
A **scope** says WHOSE store a call reads or writes. You choose the scope; the
runtime fills in the identity from the run itself. You never pass an id for
it — which is what stops one user's agent from reading another user's data.

**First, find out what you are granted.** Every scoped tool's description ends
with the scopes this run may use, and `{"op":"self"}` on the Context tool
returns a `scopes` block: for each scoped tool you hold, the granted values
and, for each refused one, the exact reason a call would be given.

## The scopes

| Scope | Resolves to | Shared with | Use it for |
|---|---|---|---|
| `agent` | this agent's name | every run of THIS agent, for every user | the agent's own counters, learned conventions, working notes |
| `user` | the run's user id | every agent that works for THIS user | the user's preferences, their facts, their documents |
| `tenant` | the run's tenant | every user and every agent in the tenant | curated reference material the whole team should read |
| `run` | the top-level run | this run and the sub-agents it spawns; dropped when the run ends | scratch tables for one task (SQL only) |

Widest to narrowest: tenant ⊃ user ⊃ agent. A wider scope is not a fallback
for a narrower one: **each call reads exactly one scope.** A `user` search does
not see `tenant` knowledge. To consult both, make two calls.

`user` needs a user id on the run. On a run started without one, every `user`
call is refused whatever the grant says.

## Which tool uses which

| Tool | `scope` values | Default when omitted | What grants each value |
|---|---|---|---|
| Memory (get/set/search/recall/…) | `agent`, `user`, `tenant` | none — `scope` is required | the agent's `memory_scopes`; unset means `user` (+ `tenant` for a team member) |
| Memory (`sql_*` ops) | `agent`, `user`, `run`, `tenant` | none — required | the agent's `sql_scopes`; unset means `user` |
| Document | `agent`, `user`, `tenant` | `user` | `agent` and `user` are open; `tenant` needs BOTH `memory_scopes` and `sql_scopes` to include `tenant` |
| Path | `agent`, `user`, `tenant` | `agent` | open; the tree you name must match where the resource lives |
| History | `self`, `user`, `tenant`, `global` | `self` | the agent's `history_scope`; unset means `user` |
| CredentialDef | `tenant`, `user`, `agent` | `tenant` | open |

Two traps in that table:

- **History's `self` is not you.** It means this AGENT's chats across every
  user — wider than `user`, which is the caller's own chats. The default grant
  is `user` but the default scope is `self`, so **always pass `scope` to
  History**.
- **Defaults differ per tool.** Path defaults to `agent`, Document to `user`.
  A document written with no scope and then looked up in Path with no scope
  is looked up in the wrong tree. Pass `scope` on both calls.

Channels are scoped differently: each channel declares its own scope when it is
defined, and your agent's `channels` publish/subscribe patterns decide which
you may use. There is no `scope` argument to choose.

## Choosing a scope

- A preference or fact the **user** told you → `user`. Every agent working for
  them will see it, and no other user will.
- Something about **how you work** — a convention, a counter, a lesson — →
  `agent`.
- Reference material **the whole team** should rely on → `tenant`. It is read
  by every user and agent in the tenant as ground truth, so never put anything
  there derived from untrusted text.
- Intermediate results for **this task only** → SQL `run`.

When unsure between `user` and `tenant`, Memory's `placement` op answers which
scope a batch of facts belongs in, without writing anything.

## Examples

A user's preference, visible to every agent that works for them:

```json tool=Memory
{"op": "set", "scope": "user", "key": "preferred_language", "value": "en"}
```

This agent's own counter, shared across all its runs:

```json tool=Memory
{"op": "incr", "scope": "agent", "key": "reports_written", "delta": 1}
```

Searching the tenant's shared knowledge — a separate call from searching the
user's own:

```json tool=Memory
{"op": "search", "scope": "tenant", "query": "release checklist"}
```

A scratch table for this task, gone when the run ends:

```json tool=Memory
{"op": "sql_exec", "scope": "run", "statement": "CREATE TABLE findings (id INTEGER PRIMARY KEY, note TEXT)"}
```

A document the whole tenant should read:

```json tool=Document
{"op": "create_document", "scope": "tenant", "title": "Onboarding guide", "path": "/docs/onboarding"}
```

The same document, found by path — in the SAME scope it was written to:

```json tool=Path
{"op": "resolve", "path": "/docs/onboarding", "scope": "tenant"}
```

The user's own past chats (not `self`, which is every user's chats with this
agent):

```json tool=History
{"op": "search", "scope": "user", "query": "invoice"}
```

## What a refusal means

- **Not granted** — "scope "tenant" not in this agent's memory_scopes": the
  operator has not granted it. Use a scope you have; do not retry the same call.
- **Cannot resolve** — "scope=user requires a user_id on the run": the grant
  exists but this run has no user. Use `agent`, or tell the user the run needs
  to be started for them.
- **Team member confinement** — a user who is an isolated member of the tenant
  may use only their own `user` and `agent` scopes; `tenant` is refused for
  them whatever the agent is granted.

None of these is fixed by retrying. `{"op":"self"}` on the Context tool shows
the full picture before you try.
