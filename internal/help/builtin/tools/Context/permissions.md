---
name: Context/permissions
description: "Context op=permissions — the raw policy gates on this run: tool list, web host allowlist, memory, history, channel, agent-def, skill and evaluation grants."
---
`permissions` returns the policy settings that decide what your calls may do,
as they are configured on your agent. Compare an intended action against them
before you try it. For the question "which `scope` values may I pass to each
tool", `{"op":"self"}` gives a clearer answer: it has already worked out each
tool's grants, with the reason for every refusal.

## Arguments

None besides `op`.

## Returns

- `tools` — your tool names.
- `host_policy` — `{has_list, allowed_hosts, web_search_filter}`: the host
  list the caller of this run supplied. `has_list: false` means the caller
  supplied none and the operator's default list applies; `self` shows that
  default under `network`.
- `memory` — `{allowed_scopes, quota_bytes, sql_scopes}`. `allowed_scopes`
  gates Memory's ordinary operations; `sql_scopes` gates its SQL operations
  and a document's tenant scope.
- `history_scopes`, `agent_def_scopes`, `evaluation_scopes` — the scopes
  those tools may use.
- `channels` — `{publish, subscribe}` allowlist patterns.
- `skills` — the patterns of skills you may load (empty = all).

An empty or `null` list means nothing is granted there, except for `skills`,
where an empty list allows every skill.

## Errors

None in practice.

## Examples

See every gate on this run:

```json
{"op": "permissions"}
```

```json result
{"tools": ["Channel", "Context", "Memory"],
 "host_policy": {"has_list": true, "allowed_hosts": ["api.github.com"], "web_search_filter": ""},
 "memory": {"allowed_scopes": ["agent", "user"], "quota_bytes": 0, "sql_scopes": null},
 "history_scopes": null, "channels": {"publish": ["findings/*"], "subscribe": ["jobs/cv-requests"]},
 "agent_def_scopes": null, "skills": null, "evaluation_scopes": null}
```
