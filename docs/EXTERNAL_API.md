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

## Change feed — react to changes instead of polling

When the change-data-capture feed is enabled (`LOOMCYCLE_MEMORY_CHANGES_ENABLED=1`), loomcycle emits a **value-free** change event on every memory/document write — the coordinate of *what* changed (`{seq, type, tenant, scope, scope_id, key|chunk_id, at}`), not the value. A consumer reads the current value via the data API above. Off by default: when off there is zero overhead and no table growth.

**Subscribe (pull, SSE).** `GET /v1/_memory/changes?since=<seq>` and `GET /v1/_document/changes?since=<seq>` stream events as they happen; `seq` is a monotonic cursor to resume from. `substrate:tenant` scope; each stream is your own tenant's changes only (not member-accessible — the feed is operator observability).

**Push (webhook).** Declare a `change_subscriptions:` entry in operator yaml and loomcycle POSTs signed batches to your callback:

```yaml
change_subscriptions:
  my-consumer:
    callback_url: "https://consumer.example/loomcycle-changes"
    secret_env: LOOMCYCLE_CHANGE_SUB_SECRET   # env NAME of the HMAC key (allowlisted)
    tenant_id: ""                             # which tenant's feed ("" = shared/default)
    kinds: [memory, document.chunk.updated]   # optional (scope, kinds) filter
    # allow_private_host: true                # to deliver to a private-network callback
```

Each POST carries `X-Loomcycle-Signature: hex(hmac-sha256(secret, body))` — verify it with the secret to authenticate the sender. Delivery is **at-least-once** (a persisted cursor resumes across restarts); dedupe on the `seq`.

**Environment flags:**

- `LOOMCYCLE_MEMORY_CHANGES_ENABLED=1` — turn the feed on.
- `LOOMCYCLE_MEMORY_CHANGES_RETENTION_HOURS` — how long change rows are kept (default 24).
- `LOOMCYCLE_CHANGE_SUB_INTERVAL_MS` — push delivery poll interval (default 5000).

## loomcycle → loomcycle, and the gRPC/Python twin

Two more ways to consume the same memory surface:

- **A peer as a memory backend.** A `kind: remote` memory backend proxies an agent's memory to *another* loomcycle instance's `/v1/_memory/*` — one loomcycle uses another's memory as a backend (the peer embeds server-side). See [MEMORY-BACKENDS.md](MEMORY-BACKENDS.md).
- **A peer document as a federated source (RFC CE).** Declare a peer under `document_sources:` (see `loomcycle.example.yaml`), then bind a local document to a peer document with `Document op=set_remote` (`source` + `remote_ref`) and reconcile its keyed chunks with `op=sync`. `direction:pull` (default) copies the peer's chunks in; `direction:push` writes this document's chunks up to the peer. Either way sync reconciles by `natural_key`: a new key is created, a diverged body is updated in place (the overwritten body kept in the *losing side's* chunk history — the local document for pull, the peer for push), and a chunk with no `natural_key` is excluded and counted. Auth + SSRF mirror the remote memory backend (`api_key_env` is a credential-allowlisted env-var *name* resolved at use time; the peer host is dialed through the SSRF guard). This slice is content-only (full-tree reconciliation is a follow-on).
- **gRPC / Python.** The memory surface is also a gRPC RPC (`Memory`), so the Python client reaches it the same way it reaches documents and paths: `await client.memory({"op": "get", "scope": "user", "key": "tone"})`. The TypeScript client uses the HTTP surface above.
