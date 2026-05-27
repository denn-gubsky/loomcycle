---
name: vector-memory
description: Semantic search on the Memory tool — when to use it, the wire shape, and the failure modes (v0.9.0).
---
The v0.9.0 substrate gives `Memory` a semantic-retrieval surface
alongside the existing k/v ops. You write rows with `embed: true`,
later find them with `op: search`, and let cosine similarity sort
out which past notes are relevant. Use this for "what have I
**learned** that's close to X?"; keep `get` / `list` for "what did
I **store** under this exact key?"

## When to reach for it

Vector search shines when:

- You're accumulating long-form observations (research notes, user
  history, prior decisions) and the right cue for retrieval is the
  **content**, not a known key.
- A query at retrieval time is unlikely to share exact tokens with
  the stored content (`"systems programmer"` should find a row
  whose `embed_text` was `"Alice writes Rust and Go"`).
- You're using Memory as the substrate for a knowledge-base /
  recommendation pattern — the agent surveys, summarises, then
  later asks "have I seen this before?"

K/V ops still beat vector search for:

- Counters (`incr` + `get`).
- Single-key state (preferences, voice, last-seen IDs).
- Bulk listing under a key prefix.

Mixing is normal. A typical agent writes a structured profile under
`profile/<user_id>` via `set` (no `embed`) AND a free-text journal
under `notes/<timestamp>` with `embed: true`. The same agent later
calls `get` for the profile and `search` for the journal.

## The two new fields on `set`

```jsonc
{
  "op": "set",
  "scope": "agent",
  "key": "rec1",
  "value": { "name": "Alice", "skills": ["Go", "Rust"] },

  "embed": true,                                  // opt in per-row
  "embed_text": "Alice is a Go and Rust developer" // text the embedder sees
}
```

- `embed` defaults to `false` — the k/v ops cost nothing extra; you
  pay only for rows you opt in.
- `embed_text` is the string fed to the embedder. **Omit it and the
  JSON-stringified `value` is used.** That works for primitive
  values (`"alice go rust"`) but is rarely what you want for objects
  — JSON syntax and field names pollute the token stream. Be
  explicit when the value is structured.

### What the response tells you

```jsonc
{ "ok": true, "embedded": true }
```

- `embedded: true` — the embedder succeeded; the row is searchable.
- `embedded: false` + `embed_warning: "..."` — the k/v row landed,
  but the embedder call failed transiently (network, 5xx, ctx
  deadline). The row is NOT searchable until re-embedded. Operators
  can run `POST /v1/_memory/reembed` to retry, OR you can call `set`
  again with the same key + `embed: true` on the next turn.

The partial-write semantics only apply to **transient** failures.
Permanent configuration errors — no embedder configured, no vector
support on the backend — refuse UPFRONT:

```jsonc
// no memory.embedder in operator yaml
{ "isError": true, "text": "memory: no embedder configured — set memory.embedder in operator yaml" }
```

The k/v row is NOT written in that case. Operators see the misconfig
loud-and-clear; agents don't accumulate unsearchable rows.

## The `search` op

```jsonc
{
  "op": "search",
  "scope": "agent",          // or "user"
  "query": "systems programmer",
  "top_k": 5,                // optional, default 10, max 50
  "prefix": "notes/"         // optional key_prefix filter
}
```

Response:

```jsonc
{
  "entries": [
    { "key": "rec1",
      "value": { "name": "Alice", ... },
      "score": 0.91,
      "embedded_with": { "provider": "openai", "model": "text-embedding-3-large" },
      "expires_at": null },
    ...
  ],
  "query_embedding_dim": 3072,
  "truncated": false
}
```

- `score` is cosine similarity in `[0, 1]` — higher is closer. The
  list is sorted by score DESC.
- `embedded_with` tells you which model produced THIS row's
  embedding. Mixed values signal the scope is mid-migration; an
  operator hasn't run `reembed` yet.
- `truncated: true` means there were strictly MORE rows than
  `top_k`. Raise `top_k` (max 50) or refine `prefix` to see more.

## Failure modes you need to know

| Error message keyword | Meaning | What to tell the operator |
|---|---|---|
| `vector index not configured` | Backend has no vector support (SQLite, or Postgres without `LOOMCYCLE_PGVECTOR_ENABLED=1`). | Enable pgvector on the Postgres backend, then restart loomcycle. |
| `no embedder configured` | No `memory.embedder:` block in yaml. | Add the block; pick `openai` or `gemini` provider. |
| `embedder not yet implemented` | Operator picked `provider: anthropic` — that's a stub in v0.9.0. | Use `openai` or `gemini`; Voyage proxy lands v0.9.1. |
| `dimension does not match` | Stored rows are at dim N; current embedder produces dim M (typical mid-migration). | Run `POST /v1/_memory/reembed?dry_run=false`. |

All four surface as tool-result errors (not transport errors) so
you can branch on them — `try` / `catch` style.

## Quota accounting

Embedding bytes do NOT count toward `memory_quota_bytes`. The cap
applies only to the k/v row's `key + value`. Operators tune the cap
for their per-agent state shape without worrying about the vector
inflating it.

## Operator visibility

Two admin endpoints surface the scope's embedding state — useful to
mention to operators when you suspect a model swap or quota issue:

- `GET /v1/_memory/embed_stats?scope=agent` — per-(provider, model,
  dim) row counts + total bytes.
- `POST /v1/_memory/reembed?scope=&scope_id=&dry_run=true` —
  list rows whose stored embedder differs from the configured one.
  `dry_run=false` re-embeds them.

The Web UI at `/ui/memory` exposes both as a per-scope badge +
"reembed plan → commit" flow. You can't call those endpoints
yourself (bearer-authed, operator-scoped) but agents should know to
reference them when surfacing "search is empty / stale" diagnostics
to the human.

## v0.11.5 — yaml-static entries + admin PUT/DELETE

Two new ways to seed and manage memory rows from outside an agent:

**yaml-static `memory.entries:`** — list of pre-seeded rows applied
on every boot. Idempotent (rows already in the substrate are
skipped, so the boot loader is safe to re-run and never clobbers
runtime updates).

```yaml
memory:
  embedder:
    provider: openai
    model: text-embedding-3-small
  entries:
    - scope: global
      scope_id: ""
      key: "company-policy"
      value: "All agents must respect rate limits."
    - scope: agent
      scope_id: "researcher"
      key: "default-format"
      value: { "format": "json" }
      embed: true
```

Optional `embed: true` synchronously embeds the value via the
configured embedder on boot. Many entries with `embed: true` will
slow boot proportional to the embedder's latency — the log line
`memory.entries: bootstrap complete — loaded=N skipped=N elapsed=...`
reports the cost so it's visible.

**HTTP admin CRUD** — `PUT` is idempotent upsert, `DELETE` is
idempotent removal:

```sh
# Set a row (overwrite if exists). Optional ?embed=true also
# computes the embedding.
curl -X PUT 'http://127.0.0.1:8787/v1/_memory/scopes/user/alice/keys/voice?embed=true' \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"value": "professional, concise"}'

# Delete a row.
curl -X DELETE http://127.0.0.1:8787/v1/_memory/scopes/user/alice/keys/voice \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN"
# → 204 (idempotent — missing rows also return 204)
```

The TS adapter ships `setMemoryEntry(scope, scopeID, key, opts)` +
`deleteMemoryEntry(...)` for the same surface. The Web UI's Memory
view gains "+ New entry" + per-row Edit / Delete buttons that drive
the same endpoints.

Admin scope set is `agent | user` (matches the existing GET surface).
Global-scope entries are yaml-only (no URL pattern for empty
scope_id).

## Cross-references

- `help(topic="memory-reducers")` — v0.12.x atomic ops on top of
  the same Memory tool (`merge` / `append_dedupe` / `bounded_list`)
  for the multi-caller-touches-same-key case. Orthogonal to
  semantic search; you can `embed: true` on a `set` and reduce
  with `merge` on later updates of the same key.
- `help(topic="scopes")` — agent vs user scope (same allowlist
  applies to `search`; you can't read another scope's vectors).
- `help(topic="loomcycle")` — the full Memory wire surface
  (`get`/`set`/`delete`/`list`/`incr`/`search`/`merge`/`append_dedupe`/`bounded_list`).
- `help(topic="pause-resume-snapshot")` — snapshot round-trip
  carries the embedding payload per memory row; restores to a
  vector-supporting backend land them, restores to a non-vector
  backend drop with a warning per row.
