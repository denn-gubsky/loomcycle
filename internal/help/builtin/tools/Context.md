---
name: Context
description: "Context tool — read-only introspection: who you are, which tools and scopes you hold, what this deployment supports, the help library, the clock, and your own context window."
---
The `Context` tool tells you about **your own run and the runtime around it**.
Every operation only reads: calling it never changes anything, so it is always
safe to call, as often as you need.

## Start here

Four calls answer most questions before you make a mistake:

1. `{"op":"help"}` — the help library's index. Then `{"op":"help","topic":"<name>"}`
   reads a topic, and `{"op":"help","topic":"<Tool>/<op>"}` shows exactly how to
   call one operation, with examples.
2. `{"op":"self"}` — who you are (agent, user, tenant, run), your model, and
   **which `scope` values each of your tools lets you use**.
3. `{"op":"guide"}` — for every tool you hold, its operations and required
   arguments, in one short list.
4. `{"op":"capabilities"}` — what this deployment actually supports (vector
   memory, SQL memory, documents, bash, scheduler, search…), so you do not call
   something that can only refuse.

## When to use it — and when not

- **Use Context** to look before you act: check a scope, a tool's arguments,
  whether a feature exists, what time it is, how full your context window is.
- **Do not use Context** to read past conversations. That is the `History`
  tool.
- **Do not use Context** to create or change agents, skills or channels. That
  is `AgentDef`, `SkillDef` and `Channel`.

## Operations

Every operation takes `op`. Fetch one operation's article, with examples, as
`Context/<op>` — for example `{"op":"help","topic":"Context/self"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `help` | the help index, one topic, or a search across topics | — |
| `self` | your identity, model, scopes, volumes, network, context usage | — |
| `guide` | per tool: operations, required arguments, a usage hint | — |
| `capabilities` | which features this deployment has | — |
| `tools` | your tools with their descriptions | — |
| `doc` | one tool's full schema plus its help article | `name` |
| `permissions` | the policy gates on your run (tools, hosts, memory, channels, skills…) | — |
| `agents` | the agents declared in the operator's config | — |
| `lineage` | an agent definition's ancestors and descendants | `def_id` |
| `evaluations` | score statistics for an agent definition | `def_id` |
| `channels` | declared channels, their scope, and whether you may use each | — |
| `time` | the current time and how long this run has been going | — |
| `compact` | ask for your own context to be compacted at the next step | — |
| `state` | your recorded structured state (stateful runs only) | — |

## What Context never shows

Context reports only what applies to you. It lists only tools you hold, and
it never returns a secret: no API keys, no bearer tokens, no server addresses
beyond the URL the operator chose to publish. For how scopes work across every
tool, read `{"op":"help","topic":"scopes"}`.
