<!--
  This file is an MD-EXPORT of the loomcycle document store — the store is the
  source of truth (/loomcycle/docs/external-api). Re-export it after editing the
  store; do not hand-edit here as the primary copy.
-->
# loomcycle data API — external HTTP access

loomcycle publishes a machine-readable **OpenAPI 3.1** contract for its memory
and document/path storage, so any application or language can generate a client
instead of hand-rolling requests. This guide is the entry point; the contract
itself is authoritative.

## Where the contract lives

Every loomcycle instance serves these **unauthenticated** (the contract is
non-secret; the data behind it is bearer-gated):

- `GET /v1/openapi.yaml` — the spec, verbatim.
- `GET /v1/openapi.json` — the same spec rendered as JSON (for tools that
  prefer it).
- `GET /v1/docs` — a self-contained **Swagger UI** console (no CDN, works
  air-gapped). Use its **Authorize** button to paste a bearer, then "Try it
  out" against the live server.

## Authentication

All data routes require a bearer token minted by an `OperatorTokenDef`, carrying
an authoritative `(tenant, subject, scopes)`. The memory + document + path
families require the `substrate:tenant` scope and are also reachable by a
**non-isolated tenant member** (a `runs:*` / `channel:*` user token); an isolated
`substrate:user` token is confined and refused. The tenant is always derived
server-side from the token — never from the request. The `?scope_id=` /
`?tenant=` query params only re-focus a browse WITHIN the caller's authorized
tenant (an admin may cross tenants).

## Generate a client

Point any OpenAPI generator at the served spec. For example, with
[`openapi-generator`](https://openapi-generator.tech):

```
openapi-generator generate \
  -i http://localhost:8787/v1/openapi.json \
  -g python \
  -o ./loomcycle-client
```

Swap `-g python` for `typescript-axios`, `go`, `java`, `rust`, etc. The
generated client covers the whole surface below.

## What's covered

- **Memory (REST):** list scopes / scope_ids / entries, get / put / delete an
  entry, semantic `search` (over KV entries **and** document-chunk bodies),
  `embed_stats`, and `reembed`.
- **Documents + Path (op-dispatch):** `POST /v1/_document` and `POST /v1/_path`
  each take a single request body discriminated on an `op` field — the request
  body mirrors the `Document` / `Path` tool input, and the response is the
  tool's own JSON result (shape varies by `op`). A tool refusal returns HTTP 422
  with `{code:"tool_refused", ...}`. This models the endpoints exactly as they
  behave (they forward the body to the tool verbatim).
- **Assets:** `GET /v1/_document/asset/{chunk_id}` returns a chunk's binary
  image.

## Notes for consumers

- The op-dispatch endpoints are intentionally not idiomatic REST — one path,
  many ops selected by `op`. A generated client exposes them as two operations
  (`documentOp`, `pathOp`) taking the discriminated body.
- `/v1/_memory/search` and the document/path endpoints require SQL Memory /
  vector support for some ops; unsupported requests return a `503` or a `400`
  with a diagnostic `code`.
- The spec is kept honest by a drift test that fails if a `Document` / `Path`
  tool op is added or removed without updating the contract.
