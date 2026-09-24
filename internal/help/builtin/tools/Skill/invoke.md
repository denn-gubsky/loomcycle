---
name: Skill/invoke
description: "Skill op=invoke — load one skill's instructions by name; the tool result is the skill's text, which you then follow."
---
`invoke` loads a skill and returns its text. Read it and act on it: the text
is guidance for the task in front of you, not something to repeat back. If
you omit `op`, `invoke` is assumed, so `{"name": "doc/redactor"}` works too.

The one thing to get right: **use the exact name**, including its group
(`doc/redactor`, not `redactor`). If you are not sure of it, call
`{"op":"list"}` first.

## Arguments

- `name` (required) — the skill's full name.

## Returns

The skill's text as plain text (usually markdown), not JSON. When a skill has
been revised at runtime, you get the active revision.

## Errors

- `missing required field: name`.
- ``skill "X" is not permitted by this agent's `skills:` allowlist`` — your
  agent may not load it. Retrying is pointless; `list` shows what you may
  load.
- `unknown skill "X" (available: ...)` — no skill has that name. Pick one
  from the names the error lists, or call `list`.
- `skill "X" (...) requires tools [...] not granted by this agent's tools` —
  the skill needs tools you do not hold, so it cannot be used by you. Use a
  different skill, or hand the task to an agent that holds those tools.
- `Skill tool: no skills configured ...` — this deployment has no skills.

## Examples

Load a skill by its full name:

```json
{"op": "invoke", "name": "doc/redactor"}
```

```json result
"# Redacting documents\n\nBefore sharing a document outside the tenant, ..."
```
