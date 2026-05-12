---
name: scopes
description: agent vs user vs global scope — the same model across Memory, Channel, and (read-only) Evaluation.
---
Scopes are loomcycle's primary isolation axis. The model picks a
SCOPE (a category); the runtime resolves SCOPE_ID server-side from
your ctx. A model-supplied scope_id would let one user's agent
read another user's keys; the split is the security invariant.

## The three scopes

| Scope | scope_id is resolved to | Use when |
|---|---|---|
| `agent` | yaml agent name (from `Context.self`) | per-agent state shared across every user and every run of that agent (counters, learned facts, per-agent voice) |
| `user` | run's `user_id` (from `Context.self`) | per-end-user state shared across every agent that has user-scope access (preferences, conversation history facets) |
| `global` | empty string (single key) | cross-tenant fan-out (alarm channels, runtime broadcasts). **Use sparingly** — operator yaml ACL is the only isolation here. |

## How Memory uses scopes

`Memory.set(scope=agent, key=foo, value=42)`:

- resolves scope_id to your agent's yaml name (e.g. "qa-bot")
- writes to (scope=agent, scope_id="qa-bot", key="foo")
- another qa-bot run reads it back with `Memory.get(scope=agent, key=foo)`
- a *different* agent ("review-bot") with `memory_scopes: [agent]` reads
  its own keyspace under (scope=agent, scope_id="review-bot") — there
  is NO cross-agent leak.

Cross-agent shared state lives under `scope=user`:

```
qa-bot:    Memory.set(scope=user, key=preferences, value={...})
review-bot:Memory.get(scope=user, key=preferences)   ← reads the same row
```

This works because review-bot's `memory_scopes: [user]` grants it
read access, and the run's `user_id` is the shared scope_id.

## How Channel uses scopes

Same model — but cursor isolation matters more. Two agents
subscribing to `findings` with scope=`agent` get DIFFERENT cursors
(each tracks its own drain position). With scope=`user`, they share
a cursor (work-distribution queue keyed by end-user).

## Quick lookup table

| You want… | Scope |
|---|---|
| State that survives across runs of THIS agent | `agent` |
| State that follows THE USER across all agents | `user` |
| A broadcast channel every subscriber sees | `global` |
| A queue split between identical agent runs | `agent` |
| A queue keyed by user (one user at a time consumes) | `user` |

## What you can't do

- You can't pass `scope_id` directly. The runtime resolves it.
- You can't write to a scope your agent yaml doesn't grant. If
  `memory_scopes` is empty, every Memory call refuses.
- You can't read another user's `scope=user` data — the resolved
  scope_id locks it to your own user_id.

Check what scopes you have with `Context.permissions`.
