---
name: Context/capabilities
description: "Context op=capabilities — which features this deployment actually has working (vector and full-text memory, SQL memory, documents, bash, sandbox, scheduler, webhooks, search, consolidation) plus size limits."
---
`capabilities` answers "does this deployment support X?" from what is
actually running, not from what was asked for. Call it **before** a call that
depends on an optional feature — semantic recall, SQL memory, documents, a
sandbox, web search — so you can pick another route instead of reading a
refusal.

It says whether the deployment has a feature. Whether YOU may use it is a
separate question: see your tools (`guide`) and your scopes (`self`).

## Arguments

None besides `op`.

## Returns

One object; most entries are `{available: true|false}`:

- `vector_memory` — semantic search over memory. May carry
  `embedder: {provider, model, dimension}`.
- `full_text_memory`, `memory_layer` (whether Memory add/recall work at all).
- `sql_memory`, `documents` — documents need SQL memory.
- `bash`, `bashbox`, `sandbox` (sandboxed code tools present on this run),
  `code_js`.
- `scheduler`, `webhooks`, `retention`.
- `search` — `{available, providers: [names]}`.
- `consolidation` — `{available, configured, merge_threshold,
  related_threshold, verify_writes, detect_conflicts}`.
- `limits` — `memory_inject_max_tokens` for you, and when set
  `max_request_bytes`, `max_consolidation_targets`,
  `max_consolidation_concurrency`.
- `storage` — `{backend}`; shown only to an administrator.

No secrets and no addresses are ever included.

## Errors

None in practice.

## Examples

Check what works before planning a memory-heavy task:

```json
{"op": "capabilities"}
```

```json result
{"vector_memory": {"available": true, "embedder": {"provider": "openai", "model": "text-embedding-3-small", "dimension": 1536}},
 "full_text_memory": {"available": true}, "memory_layer": {"available": true},
 "sql_memory": {"available": false}, "documents": {"available": false},
 "bash": {"available": false}, "bashbox": {"available": true}, "sandbox": {"available": false},
 "search": {"available": true, "providers": ["brave"]},
 "limits": {"memory_inject_max_tokens": 2000, "max_request_bytes": 16777216}}
```
