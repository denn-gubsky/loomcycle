# doc-colorizer bundle

A deterministic, zero-token **code-js** agent (RFC J) — `doc/colorizer` — that
stamps per-document color metadata onto the **root chunk** of every Document
under a Path subtree:

```json
{ "color_enabled": true, "color_scheme": { "doc.rfc": "#3a9ad6", ... } }
```

It exists because coloring is pure mechanical iteration (`Path ls` →
`Document get_chunk` → merge fields → `update_chunk`) with no reasoning to do.
An LLM agent would add token cost, latency, and a hallucination class of bug (a
wrong hex value, a skipped doc). The code-agent runs the exact loop
deterministically and idempotently, offloaded from the caller's context.

## Requirements

- `LOOMCYCLE_CODE_AGENTS_ENABLED=1` — code agents are **opt-in**. A
  `provider: code-js` agent selected **without** this flag **fails boot by
  design** (it is not a silent idle def). Layer this bundle only with the flag
  set, and never add it to a default preset stack.
- `LOOMCYCLE_SQLMEM_ENABLED=1` and a `user_id` on the run — Documents require
  SQL Memory and the agent operates at `scope=user`.

The bundle carries **no** provider matrix (a code-agent never calls a model).

## Use

Selected as an embedded preset (ships in the binary) or layered as a file.

```bash
# embedded preset (ships in the binary):
LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_SQLMEM_ENABLED=1 \
  LOOMCYCLE_PRESETS=base,doc-colorizer ./bin/loomcycle

# or layered as a file over your own config:
LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_SQLMEM_ENABLED=1 \
  ./bin/loomcycle --config <your-config>.yaml \
                  --config bundles/doc-colorizer/loomcycle.yaml
```

Then spawn it. With no parameters it colors `/loomcycle/rfcs` (scope `user`)
with the built-in scheme:

```bash
# operator / HTTP
curl -sX POST localhost:8080/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"agent":"doc/colorizer","user_id":"me","prompt":""}'
```

An LLM parent can drive it for deterministic results without paying tokens:

```javascript
Agent.spawn({ agent: "doc/colorizer", prompt: "" });
```

The final result is a summary, e.g. `colored 55 / skipped 0 / failed 0 under
/loomcycle/rfcs`, plus structured `{colored, skipped, failed, subtree, dry_run}`.

## Parameters

Pass a JSON object as the run **prompt** (preferred). Scalar overrides
(`subtree`, `scope`, `dry_run`) may also arrive on the non-secret run
**metadata** channel. All are optional; omitted keys take the defaults.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `subtree` | string | `/loomcycle/rfcs` | Path subtree to walk (recursive; documents at any depth are colored) |
| `scope` | string | `user` | Path + Document scope |
| `color_enabled` | bool | `true` | value written to `fields.color_enabled` |
| `color_scheme` | object | built-in scheme | value written to `fields.color_scheme` |
| `dry_run` | bool | `false` | report what *would* change; write nothing |

Examples:

```jsonc
{}                                        // color /loomcycle/rfcs, default scheme
{"subtree":"/loomcycle/docs"}             // color another subtree
{"dry_run":true}                          // preview counts, no writes
{"color_scheme":{"doc.rfc":"#123456", ...}}  // apply a different palette
```

## Reusing on newly created documents

Re-spawn any time — the agent is idempotent (it merges the two color keys onto
the root chunk, preserving any other fields, and honors optimistic concurrency
with one revision retry). Point `subtree` at wherever new documents live.

## Notes / sharp edges

- **Scope of a run.** Each document costs ~3 store round-trips (`get_document` +
  `get_chunk` + `update_chunk`), all inside one run's wall-clock deadline
  (`LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS`, default 120s). For a large
  subtree, raise that knob; it bounds how many documents one spawn can cover.
- **Root chunk only.** It sets document-level fields on the root chunk (the
  `doc.*` keys). Per-chunk status coloring (`chunk.*`) is carried in the same
  `color_scheme` for the viewer to use; this agent does not recolor child chunks.
- **Determinism.** No `Math.random()` / `Date.now()` feeds a tool input, so the
  run resumes cleanly across restart/replica without
  `LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1`.

## Where it lives

The shipped artifact is the embedded copy at
`cmd/loomcycle/embedded/bundles/doc-colorizer.yaml` (auto-embedded, selectable
via `LOOMCYCLE_PRESETS`). This directory is the human-facing source of record;
the two YAMLs carry identical agent + `code` content (only the leading comment
header differs, for the preset description).
