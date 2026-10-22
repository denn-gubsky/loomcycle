# LoomCycle release history

Per-version release notes for **v1.43.2 onward**, newest first. Earlier releases (v0.4.0 – v1.43.1) were never written up here; their notes live in the annotated git tags — `git tag -l --format='%(contents)' v1.20.0` prints one.

The current and immediately previous releases are also summarised in the main [`README.md`](README.md).

Each entry is the release's tag annotation, so the tag and this file cannot disagree about what shipped.

For the **public roadmap**, see [`docs/PLAN.md`](docs/PLAN.md).

## What's in v1.121.0

*Embeddings on Amazon Bedrock, and the residuals of an independent re-audit of the security review. **`memory.embedder` can now use Cohere Embed on Bedrock**, signed with a Bedrock provider's AWS credentials, so a deployment that runs on an instance role needs no key to get memory search, Document search and consolidation merging. Search queries are now embedded in a query mode that embedders such as Cohere distinguish from stored text, and an embedder can sit on any provider whose driver has one, under any name. Also: a run its caller ends mid-turn is recorded `cancelled`, not `completed`; a single `spawn_run` on a held key stops at the join bound; MCP stdio servers are stopped with their process group; a volume purge can no longer be redirected by a symlink swap; refused webhook deliveries cost the same time; and gbash moves to v0.0.41. **Read the upgrade notes: a new error category, a per-file limit in Bashbox, and an embedder config that is now refused.***

A minor release: #2020 to #2034.
- #2027 and #2032 (RFC EV) add the embedder query mode, the driver-based embedder lookup and the Bedrock embedder;
- the re-audit residuals (RFC EU): the MCP stdio stop (#2024), the volume purge (#2025), webhook timing (#2026), gbash v0.0.41 (#2023), and the security wording (#2022, #2029);
- fixes: the single `spawn_run` join bound (#2020) and runs cancelled mid-turn (#2021);
- CI: swap-race proofs fail instead of skipping (#2028), and `go mod verify` plus a gosec high-severity gate (#2030);
- #2023 updates gbash and its `awk`/`jq` contrib modules to v0.0.41; no other dependency changes. #2034 is these notes.
- `@loomcycle/client` and the Python adapter are 1.121.0. #2021 documents the new `cancelled` value of `error_info.category` in both (a plain string field, so no type change). No method changes in either.

### Embeddings on Amazon Bedrock (#2032)

A deployment that runs all inference through Bedrock with an IAM role had no embedder. Memory recall, vector search, Document search, Recall and trace search refused with `embedder_not_configured`, and consolidation stored duplicates because the similarity merge never ran. Bedrock serves embeddings only through `InvokeModel`, which none of the existing embedders speak.

- **Configure it** on a Bedrock provider that already authenticates with AWS credentials (an `auth:` block, such as `bedrock-claude` from the `bedrock` preset):

  ```yaml
  models:
    cohere-embed: { provider: bedrock-claude, model: eu.cohere.embed-v4:0, kind: embedder }
  memory:
    embedder:
      model: cohere-embed
      api: bedrock
      dimensions: 1024
  ```

  The new `memory.embedder.api: bedrock` names the protocol. The embedder uses the provider's host (its `base_url` path is dropped) and the provider's credentials: the same chain, nothing fetched at boot, every secret registered with the redactor. `aws_bedrock_token` mode works too.
- **Models:** Cohere Embed v4 (`cohere.embed-v4:0`) and v3 (`cohere.embed-multilingual-v3`, `cohere.embed-english-v3`), as a base id or an inference-profile id (`eu.`, `us.`, `global.` …). An application-profile ARN is refused.
  - v4 `dimensions`: 256, 512, 1024 or 1536 (default 1536). v3 is fixed at 1024. Any other value is refused at config load with the allowed ones.
  - **Residency:** v4 is not served in-region in eu-central-1. From Frankfurt it is reached through `eu.cohere.embed-v4:0`, which processes in six EU regions, or `global.`, which may leave the EU. `cohere.embed-multilingual-v3` runs in eu-central-1 itself.
- **Config load refuses** `api: bedrock` on a provider with no `auth:`, on a `bedrock-mantle` provider (it serves no embeddings), on a `base_url` other than `https://bedrock-runtime.<auth.region>.amazonaws.com`, with an `auth.service` other than `bedrock`, and together with the embedder block's own `base_url` or `api_key_env`. The credentials can reach only the provider's own runtime host.
- **Calls:** batches of at most 96 texts (fewer with a smaller `memory.embedder.batch_size` or `LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE`), order kept, each vector's width checked. Over-long texts are truncated rather than failing the batch. Up to 3 attempts for 408, 429, 500 and 503, with short backoff and `Retry-After` honoured (capped at 10 s), all inside the embedder's timeout; expired credentials get one refresh and retry. No credentials fails each call with the permanent `provider authentication unavailable` error and never stops boot.
- **Errors** name the model id, region, status and AWS error type, plus the first line of AWS's message, capped. Never the input texts.
- **`loomcycle doctor`** has a new Embedder line for any configured embedder: provider, embedder, model and width. For `api: bedrock` it says the credentials are the provider's (whose identity the AWS line already prints), and warns when the model is not in the region's model listing.
- **IAM:** `bedrock:InvokeModel` on the model. For a cross-region profile such as `eu.cohere.embed-v4:0`, on the profile ARN in the calling region **and** on `arn:aws:bedrock:*::foundation-model/cohere.embed-v4:0` in every region the profile routes to. Allowing the model names is not enough.
- **Not included:** a tenant's own Bedrock key for embeddings, embedder token usage in the usage ledger (Bedrock reports no counts for Cohere), and Titan embeddings.
- The `bedrock` preset gains a commented embedder example; it still declares no new provider and changes no routing. See "Embeddings" under "Amazon Bedrock with AWS credentials" in [`docs/PROVIDERS.md`](docs/PROVIDERS.md) and the embedder section of [`docs/TOOLS.md`](docs/TOOLS.md).

### Search queries are embedded in a query mode (#2027)

Retrieval embedders such as Cohere place a search query differently from the stored text it should find (`search_query` against `search_document`). loomcycle embedded both the same way.

- **An embedder may now have a query mode**, and every search embeds its query through it: Memory search and recall (and so Document search, trace recall, prompt injection, `POST /v1/_memory/search` and the consolidation merge pass), Recall, help search, Document verbatim answers and graph seeds, History content search, and History `related` by free text. Stored text, and comparisons of two stored texts (Document `related`, History `related` by chat, SQL Memory `$embed`, `POST /v1/embeddings`), keep the document mode.
- **Only the Bedrock embedder has a query mode.** For OpenAI, Gemini, Ollama, Voyage and the rest both modes are the same call, so their vectors, results and calibration are unchanged.
- **`loomcycle memory-calibrate`** measures new facts in query mode against stored facts in document mode, the comparison the merge pass makes, so a merge band calibrated for Cohere fits what merging does.

### An embedder on any provider whose driver has one (#2027)

`memory.embedder` looked its embedder up by provider id, so only the built-in ids could embed. A provider declared under its own name, `my-gateway: { driver: openai }` or any OpenAI-compatible gateway, failed with `unknown provider`.

- **The lookup is now:** `memory.embedder.api` when set; else the provider id, as before (`ollama-local` stays the local flavour, `anthropic` stays Voyage); else the provider's driver (never `anthropic`, which has no embeddings API); else an error naming the provider, its driver and the embedders available.
- Config validation and boot use the same lookup, so a config that loads builds the embedder it was checked against.
- **A provider with an `auth:` block is refused as the embedder unless `api: bedrock` is set.** Before, such a config loaded and would have sent the vendor key named by the id (an `OPENAI_API_KEY`, say) to the Bedrock host.

### A run its caller ends mid-turn is recorded cancelled (#2021)

When a run's caller ended it mid-turn (an HTTP client disconnecting from a non-detached stream, an MCP `spawn_run` transport timeout, a gRPC deadline or cancel, a parent cancelling a sub-agent), the run was stored `completed` with its partial text, or `failed` with "context canceled".

- **Now it is `cancelled`**, with a reason: "client disconnected", "parent run cancelled" or "caller deadline exceeded". A caller deadline at a turn boundary, previously `failed`, is `cancelled` "caller deadline exceeded".
- **An API cancel mid-stream** ends the SSE stream and gRPC `Run` with one `error` frame carrying `error_info.category: "cancelled"`, where it ended with `done` and an empty reason (gRPC: with no terminal frame). The Web UI terminal shows "cancelled".
- **A new error category value, `cancelled`**, not retryable. A consumer that switches exhaustively on `error_info.category` will see it.
- A wall-limit cut mid-turn records stop reason `wall_limit` (it read `cancelled` on a `failed` row). An Agent-tool child cut by its `timeout_ms` mid-stream is reported timed out instead of returning its partial text as the answer. Stateful runs had the same bug and are fixed too.
- **Compaction:** a summary cut by a cancel is no longer applied, so a truncated summary is never persisted (the manual compact answers `502`). A summary stream that ends without finishing on a live connection is retried once, never applied truncated.
- Unchanged: the shutdown pause, operator pause, retune, model swap, review hold, idle timeout, provider fallback and turn cancel, none of which cancel the run. A scheduler `on_complete` hook still fires only on `completed`.

### A single spawn_run on a held key stops at the join bound (#2020)

v1.120.0 bounded the batch join (#2012). A single blocking `spawn_run` (MCP, connector `SpawnRun`) whose `idempotency_key` joined a run another request started still waited until that run ended. With no `timeout_ms` and `LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS` at its default `0`, that could be forever.

- **When the caller sets no deadline**, the wait now stops at `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (default `1h`). The run is reported as it is: `running`, its ids, `deduplicated: true`, and the batch's "join timed out after …; the run goes on" error. It is never cancelled, since this caller did not start it.
- A caller deadline still wins, `0` leaves the wait to the caller, and the streaming surfaces (`POST /v1/runs`, gRPC `Run`) are unchanged.

### MCP stdio servers stop with their process group (#2024)

Most stdio MCP servers start through a wrapper (`npx -y …`, `uvx …`, a script) whose child is the real server. Closing a server killed only the wrapper and then waited for the real server to exit on its own, usually never. A tool call whose server failed its handshake hung instead of returning the error, and one wrapped server stalled shutdown.

- **A stdio server now starts in its own process group.** Closing it closes stdin, kills the whole group at once, and waits at most 2 s for its output to end before letting go. A descendant that left the group (`setsid()`) survives, as it does for Bash.
- **On Linux** a server is also killed when loomcycle dies, even by `SIGKILL`. macOS has no equivalent; see [`docs/TOOLS.md`](docs/TOOLS.md) for what survives a crash there.

### A volume purge cannot be redirected by a symlink swap (#2025)

Every destructive volume delete (the VolumeDef purge, the per-run ephemeral purge and its sweeper, the git-source cleanup) checked a resolved path and then deleted by that path string. An ancestor swapped for a symlink in between carried the delete wherever the link pointed: outside the dynamic root, or into another tenant's directory, which passed the root check. A run needed Bash or a Bashbox host command on a static volume containing the dynamic root to swap it.

- **The delete now goes through a handle on the dynamic root**, opened one directory at a time with an identity check per step, as the file tools do since v1.119.0. A swapped component is refused.
- **Fix:** under a dynamic root configured through a symlink, the git-source cleanup refused its own directory (logged `REFUSED`) and left it behind. It now cleans up.

### Refused webhook deliveries cost the same (#2026)

v1.119.0 made every refused delivery answer the same `404` bytes, but not in the same time: a few probes with a large body told a live webhook name from an unknown one.

- **Every refusal now costs two HMAC passes over the body**, the most a live check makes: an unknown or disabled name, an oversize body, a missing or malformed signature, a stale timestamp, and a wrong bearer token. Verdicts are unchanged.
- **Left:** a live webhook copies the body into memory before checking it, about 0.2 ms per MiB, which a made-up name deliberately does not. The first delivery to a webhook whose secret is a stored credential reads it, then caches it for 5 s. See [`docs/FORGE_WEBHOOKS.md`](docs/FORGE_WEBHOOKS.md).
- **Cost:** a refused delivery takes two SHA-256 passes over a body the server already reads in full.

### gbash v0.0.41 in Bashbox (#2023)

Bashbox runs gbash inside the loomcycle process, so a gbash panic or hang reaches every run on the replica. v0.0.41 fixes several of each: expansion and parser panics and blow-ups, heredoc hangs, budget checks that `eval`, `source` and `trap` skipped, background jobs outliving the run, and unbounded `cp`, `tar` and `gzip` output. In the filesystem layer, `..` after a symlink now applies to the link's target, a hard link needs write access to its source, and reading a host directory no longer hangs on a FIFO.

- **Behaviour change: one file written through a redirect or `cp` stops at 8 MiB**, on read-only and read-write volumes; a named pipe buffers at most 64 KiB. Reads through a read-write volume were already capped at 10 MiB. loomcycle does not change gbash's limit.

### Security wording and CI (#2022, #2028, #2029, #2030)

- **README and SECURITY.md say what two protections cover** (#2022). A host vouched for by name in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST` still reaches loopback and private ranges wherever its DNS points, never link-local or cloud metadata. Operator-token hashes are peppered only when `LOOMCYCLE_SECRET_KEY` (or an explicit pepper) is set.
- **A WebSocket bearer can land in proxy logs** (#2029). A browser sends it in `Sec-WebSocket-Protocol`, which many proxies log. [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) and the `client-tools` help topic say what to do: a short-lived per-user token limited to the agents it needs, never an operator bearer.
- **CI** fails, rather than skips, when the symlink swap-race proofs cannot run (#2028), runs `go mod verify`, fails on high-severity gosec findings, and logs a failed advisory unlock through the logger (#2030).

### Upgrade notes

- **No migration.**
- **Behaviour changes:**
  - **Runs ended by their caller mid-turn** are `cancelled` with a reason, not `completed` with partial text or `failed`. An API cancel mid-stream ends the stream with an `error` frame, `error_info.category: "cancelled"`. **A new category value**: check any consumer that switches exhaustively on it.
  - **Bashbox:** one file written through a redirect or `cp` stops at 8 MiB.
  - **Embedder config:** an embedder on a provider with an `auth:` block is refused at load unless it sets `memory.embedder.api: bedrock`.
  - **Search vectors:** unchanged for every embedder but Bedrock. Only an embedder with a query mode embeds queries differently.
  - **A single `spawn_run`** joining a held key with no deadline returns after `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (default 1h), the run still going.
  - **MCP stdio servers** are killed with their whole process group when closed, with no grace period.
- **Switching to a Bedrock embedder:** set `memory.embedder` as above, restart, run `loomcycle memory-calibrate` and set `memory.consolidation.merge_threshold` and `related_threshold` from it. On a store that already has vectors, `POST /v1/_memory/reembed`; for facts stored with no vector, `POST /v1/_memory/backfill_embeddings`. Similarity bands belong to the embedding model and do not carry over. The probe `PUT …/keys/probe?embed=true` needs a vector-capable store; `POST /v1/embeddings` exercises the embedder directly.
- **Images:** a minor tag rebuilds every image. All are at 1.121.0, for amd64 and arm64: `loomcycle`, `loomcycle-toolbox`, `loomcycle-browser` and the sandbox images.

## What's in v1.120.0

*The security review's closing decisions, and a bound on batch joins. **Boot no longer rewraps operator tokens minted before the pepper**: a new `loomcycle tokens rewrap` command lists them and rewraps only the rows you name after reviewing them. **A deployment upgrading straight from a release before v1.119.0 must run it once: read the upgrade notes first.** Also: a config reload keeps an admin credential in config, a resume gives back exactly the per-user slot a run took, a batch spawn in join mode can no longer wait forever, shared-tier definitions are marked in get and list, and the chat bridges move to 0.2.0.*

A minor release: #2010 to #2019.
- the security review's closing decisions (RFC ES): the token rewrap command (C2: #2015, #2018), the reload's admin credential (C1: #2013, #2016), the shared-tier mark (C3: #2011), and the per-user slot and consolidation pass across a resume (C4: #2014);
- #2012 bounds a batch join, and #2017 documents it for MCP;
- #2010 moves the four chat bridges to 0.2.0, on `@loomcycle/bridge-core` 0.2.1;
- no dependency updates, and #2019 is these notes.
- `@loomcycle/client` and the Python adapter are 1.120.0. The TS client's `AgentDefRowResponse`, `TeamDefDetail` and `TeamVersion` types gain an optional `shared` (#2011). The Python adapter changes only its version. No method changes in either.

### Pre-pepper tokens are rewrapped by a command, not at boot (#2015, #2018)

v1.119.0 rewrapped every unpeppered operator-token row at the first boot under a derived pepper, and stored a marker row to record that this boot had happened. Both are database state. Someone who can write the database could delete the marker (and any rows already on the pepper), insert an unpeppered row for a token they chose, and the next restart wrapped it into a working token. On a deployment with no token rows, that took deleting one row, and nobody was logged out.

- **Boot only reports.** Under a derived pepper (`LOOMCYCLE_SECRET_KEY` set, no `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`), boot changes nothing. It logs a WARNING that counts the unpeppered rows, lists the live ones by quoted `def_id` (at most 50) and names `loomcycle tokens rewrap`. Login refuses those rows until they are rewrapped. Boot also logs the pepper's fingerprint: `auth: operator tokens use a pepper derived from LOOMCYCLE_SECRET_KEY, fingerprint …`.
- **Step 1, review.** `loomcycle tokens rewrap` with no flags writes nothing. It prints the pepper's fingerprint, then each live unpeppered row: `def_id`, name, tenant, subject, scopes (an admin row is flagged), created, retired and expiry times, and a per-row fingerprint. Then the row count and a digest of the whole set. Every field is printed quoted, with control characters escaped, so a row cannot redraw the terminal to hide itself.
- **Step 2, name the rows.** `--def-id <def_id>@<fingerprint>` (repeatable), copied from the review, or `--all-listed <digest>`.
  - A bare `def_id` is refused. Either form is refused if a named row was added, retired or changed since the review, and the refusal never prints the new fingerprint or digest.
  - Only the named rows are rewrapped, in one transaction, each a compare-and-set against the hash it was reviewed with. A row nobody named stays refused.
  - `--dry-run` checks a selection and writes nothing.
  - The result re-prints each rewrapped row, and lists a named row that changed in between as NOT moved (exit 1).
- **Only live rows.** A retired or expired unpeppered row is listed apart as not rewrappable and can never be named. Wrapped, a forged row inserted already retired would have become a working token once someone cleared its `retired_at`. A row in its rotation grace window still counts as live.
- **SUSPICIOUS rows are refused.** A `def_id` other than `def_` + 16 lowercase hex (the shape every mint writes), or a control, format or non-printable character or invalid UTF-8 in any field, marks the row SUSPICIOUS, and it cannot be rewrapped. Non-ASCII in a name, tenant or subject draws a lookalike warning. For a row you did not mint, retire it (`loomcycle operator-token retire --def-id <id>`): a retired row leaves the set.
- **Run it with the server's key.**
  - The command loads config as the server does (presets, `LOOMCYCLE_CONFIG_DIR`, `LOOMCYCLE_CONFIG_FILES`, `--config`, `--preset`) and opens the server's store. In a container, `docker exec <container> /usr/local/bin/loomcycle tokens rewrap` already has all of it.
  - The fingerprint it prints must match the one in the boot log. It refuses (exit 2) when it finds live rows under another key and none under its own, the usual sign of a wrong `LOOMCYCLE_SECRET_KEY`.
  - It also refuses under an explicit pepper or with no pepper, and when the SQLite database it would open does not exist.
  - If a rewrap did run under the wrong key, set that key as `LOOMCYCLE_SECRET_KEY_PREVIOUS` on the server. Those rows then log in and move onto the current key.
- **Safe while servers run.** Two runs at once wrap a row once, and a running server logs a rewrapped token in at once, with no restart.
- **The v1.119.0 marker row is no longer read.** A deployment that has one keeps it; nothing needs removing.
- **What this does not stop:** the review shows what a row grants, not whose token it holds. Someone who can write the database and replaces a genuine pre-pepper row's hash before you review it gets that row rewrapped for a token of their choosing. A change after the review is caught, by both the per-row fingerprint and the digest.
- See "Token pepper" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md#token-pepper).

### A config reload keeps an admin credential in config (#2013, #2016)

v1.119.0 refused a reload that dropped the last config admin credential only when the store held no admin token. That left a lockout across replicas:

1. Replica A reloads a config that drops its `substrate:admin` principal while an admin token exists. The token made it acceptable.
2. Replica B, whose config still has its admin principal, retires that last admin token. Its retire guard sees its own principal and allows it.
3. Authentication stays on everywhere, but replica A has no admin credential at all. Nobody can mint tokens or use its admin routes until it restarts.

- **Now:** when the running config has an admin credential (`LOOMCYCLE_AUTH_TOKEN`, or a `substrate:admin` principal that can log in), the new config must have one too. Admin tokens in the store no longer count. Otherwise the reload is refused with `422 config_invalid` (a dry run reports it), naming the ways out: keep the admin principal, set `LOOMCYCLE_AUTH_TOKEN`, or restart.
- **The last-admin-token retire guard reads the current config** for both credentials. It read `LOOMCYCLE_AUTH_TOKEN` as it was at boot, so after a reload that set it, retiring the last admin token was still refused. A reload that sets `LOOMCYCLE_AUTH_TOKEN` or adds an admin principal now lifts the refusal at once.

### A batch join can no longer wait forever (#2012, #2017)

A batch spawn in join mode (`POST /v1/runs:batch`, MCP `spawn_runs`, gRPC `SpawnRunBatch`) had a deadline only when the request sent `timeout_ms`. A child whose `idempotency_key` matched a run that never finishes, or a fresh child that never finished (held for review with no TTL, or a provider that never answers), blocked the call for as long as the caller stayed: forever for a gRPC call with no deadline, a connected HTTP client or an MCP session.

- **The bound** is the request's `timeout_ms` when set, and otherwise the new `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (a Go duration, default `1h`; `0` = no server bound, so only the caller's deadline or disconnect ends the join). A request's `timeout_ms` replaces it, larger or smaller. A value that does not parse is refused at startup.
- **At the server bound the join only stops waiting.** A child still going comes back with its current status (`running` or `queued`), its ids and an `error`: `join timed out after 1h0m0s; the run goes on: read it by run_id`. It keeps running, and keeps its admission slot, like a detached child, and completes as usual. A child not yet admitted is withdrawn, so no run starts that the caller has no id for.
- **A request's `timeout_ms` cancels** the batch's own unfinished children. **Fix:** such a child was stored `completed` with its partial text. It is now recorded `cancelled`, "batch join timed out".
- **A deduplicated child is never cancelled**, since another request started it. It comes back with that run's current status, its ids and `deduplicated: true`.
- Unchanged: a caller's deadline or disconnect still ends the batch's fresh children, a batch that finishes inside the bound, detach mode, and the envelope on every transport (the reason uses the existing `error` field).
- Not covered: a single MCP `spawn_run` on a held `idempotency_key` with no `timeout_ms` still waits until the MCP call is cancelled.
- See [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), [`docs/GRPC.md`](docs/GRPC.md) and "Bounding a batch join" in [`docs/MCP_SERVER.md`](docs/MCP_SERVER.md).

### Shared-tier definitions are marked in get and list (#2011)

Since v1.119.0 a tenant caller, and an admin inside a run, reads two tiers of AgentDef, SkillDef and TeamDef definitions: its own tenant's and the shared `""` tier. It may retire or promote only its own. `get` and `list` returned both mixed together, and each tier numbers its own versions, so a list could show two "version 1" rows and nothing to say which one the caller owns.

- **`get` and `list` rows of the shared tier carry `"shared": true`.** Every other row leaves the field out, so the change is additive. Create, fork, retire and promote responses are unchanged.
- **Every transport:** `POST /v1/_agentdef`, `_skilldef` and `_teamdef`, the gRPC def RPCs and the MCP `agentdef`, `skilldef` and `teamdef` tools all return the same JSON. No proto or OpenAPI change: the gRPC def RPCs return the tool's JSON as bytes.
- **A caller whose own tenant is `""` sees no mark**: it owns that tier and may retire and promote its rows. That is open mode, the stdio `loomcycle mcp`, and runs fired by a yaml schedule or webhook.
- The in-run tool descriptions, the MCP tool descriptions and the TeamDef help topics now say that shared rows are marked, may be read, run and forked, and may not be retired or promoted (over MCP, only an admin token may).

### Per-user slots and consolidation passes across a resume (#2014)

Two leftovers from the v1.119.0 shutdown pause.

- **A resume gives back exactly the per-user slot a run took.** When a resume settled a run that a stopped replica had parked, it gave that replica's `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER` slot back for every top-level run. A run admitted while the per-user cap was off took no slot, so the resume took back another run's, and the counter's floor at 0 hid it.
  - Migration 0108 adds a nullable `runs.user_slot`. Every start path (a fresh run, a queued run's start, a configured run's start) records whether its admission counted the run in the cluster's shared counter.
  - The resume takes that record by compare-and-set and gives back only a slot the run held. A row with no record (parked by an older binary) keeps the earlier rule: top-level with a replica id.
  - A crash between taking a slot and recording it leaks one slot rather than releasing another run's.
  - The column is not carried in snapshots: the slot it names lives in the source cluster's counter.
- **The dead-replica reap gives back only the slots its runs took.** It decremented the per-user counter once per failed run with a user, sub-runs and runs admitted with the cap off included: a dead replica with one top-level run and five sub-agents gave back six slots where one was counted. It now counts a run that records a slot, or a top-level run from before migration 0108.
- **A resumed consolidation pass is not doubled.** A pass can still come back through a resume: the process was killed before its shutdown cancel was stored, or a snapshot taken during an operator pause was restored. It then runs outside the fan-out that started it, and the next tick started a second pass on the same target.
  - The fan-out now skips a target with a live top-level run of the consolidator agent in the same tenant and user, a parked one included, and logs the skip. The run row is what every replica sees, so a pass another replica resumed counts too.
  - A parked pass that nobody resumes holds its target for at most 4 hours from its last heartbeat. The tick then logs a warning naming the run and consolidates the target again.
  - The hold is by consolidator agent and user: two schedules running the same consolidator agent for one user hold each other off, as they target the same memory.
- See item 10 of [`docs/MULTI-REPLICA.md`](docs/MULTI-REPLICA.md) and §9c of [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Chat bridges 0.2.0 (#2010)

The four bridges depended on `@loomcycle/bridge-core` `^0.1.0`, which on a 0.x line never reaches 0.2, so the published bridges ran core 0.1.0.

- `@loomcycle/bridge-telegram`, `-slack`, `-discord` and `-teams` are 0.2.0, on `@loomcycle/bridge-core` `^0.2.1`.
- **Every bridge:** an agent's text before and after a tool call no longer runs together into one paragraph (core 0.2.1).
- **Discord:** `/ask prompt:/help` lists the Discord commands, as `/help` does. It listed the core's typed commands, which have no `/ask`.
- They are on npm, and the `loomcycle-bridge-<name>` images are on Docker Hub and GHCR as `:0.2.0` and `:latest`, for amd64 and arm64. They publish on their own tags, not with this release.

### Upgrade notes

- **Migration 0108** adds a nullable `runs.user_slot` on SQLite and Postgres. It is additive, has no backfill and applies at boot. Rows from before it keep the earlier slot rule.
- **Act on this before you upgrade:**
  - **Operator tokens, when you upgrade straight from a release before v1.119.0** with `LOOMCYCLE_SECRET_KEY` set and no `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`. Boot no longer rewraps the tokens minted before the pepper, and login refuses them until you run `loomcycle tokens rewrap` once:
    1. run it with no flags, with the server's `LOOMCYCLE_SECRET_KEY` and config, and check that the pepper fingerprint it prints matches the server's boot log;
    2. review the rows, then rewrap the ones you minted with `--def-id <def_id>@<fingerprint>` for each, or all of them with `--all-listed <digest>`. Retire any row you did not mint.

    Under Docker or TrueNAS: `docker exec <container> /usr/local/bin/loomcycle tokens rewrap`. It is safe while the server runs and needs no restart. `LOOMCYCLE_AUTH_TOKEN` keeps working meanwhile. The v1.119.0 upgrade notes on token hashes apply too: upgrade every replica together, and a rollback means re-minting.
  - **A deployment that already booted v1.119.0 with `LOOMCYCLE_SECRET_KEY` set has nothing to do.** That boot rewrapped its rows.
  - **A digest or bare `--def-id` printed by an earlier build of the command no longer matches.** Re-run the review.
- **Behaviour changes:**
  - **Config reload:** when the running config has an admin credential, a reload that leaves the new config without one is refused, even when the store holds a minted admin token. To drop the last `substrate:admin` principal, restart or set `LOOMCYCLE_AUTH_TOKEN`.
  - **Token retire:** after a reload that sets `LOOMCYCLE_AUTH_TOKEN` or adds an admin principal, retiring the last minted admin token is allowed straight away.
  - **Batch join:** a join that used to hang now returns after `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (default 1h), its unfinished children still running and reported as such. A child cut off by a request's `timeout_ms` is now recorded `cancelled`, where it was `completed` with partial text.
  - **Def rows:** AgentDef, SkillDef and TeamDef `get` and `list` rows of the shared `""` tier carry an additive `"shared": true` for a caller whose own tenant is not `""`.
  - **In a cluster:** a resume and the dead-replica reap give back a per-user slot only for a run that took one. A consolidation target whose pass is still live, or parked for under 4 hours, is skipped by the next tick.
- **New env var:** `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (default `1h`; `0` = no server bound).
- **Chat bridges:** 0.2.0 takes effect only when you redeploy a bridge: pull `loomcycle-bridge-<name>:0.2.0` (or `:latest`), or install the 0.2.0 package.
- **Images:** a minor tag rebuilds every image. All are at 1.120.0, for amd64 and arm64: `loomcycle`, `loomcycle-toolbox`, `loomcycle-browser` and the sandbox images.

## What's in v1.119.0

*The fixes from a security review of the whole runtime. A server authenticated only by a minted admin token could serve a request unauthenticated when it failed to read its token store: that now fails closed. **Open mode is refused on a public listener**, declaring config `principals:` turns authentication on, and operator-token hashes are peppered by default. **Some upgrades need an action: read the upgrade notes first.** Also: a shutdown pauses the runs in flight and a restart resumes them, `POST /v1/runs` starts runs the way every other transport does, logs can be JSON, and Amazon Bedrock can be reached with AWS IAM credentials.*

A minor release: #1890 to #2009.
- the security review's fixes (RFC ES): authentication (#1949, #1952, #1957, #1999, #2000, #2001, #2003), egress and webhooks (#1951, #1961), Bash, Bashbox and the file tools (#1950, #1953, #1963), authority inside a run (#1997, #2002), the shutdown pause (#1998, #2005), and supply chain and CI (#1894, #1895, #1897, #1948, #1966, #1988);
- the code restructuring (RFC ET): `POST /v1/runs` and session messages start through the shared run start (#1960, #1967) with follow-ups (#2004), reads confined for agent-restricted tokens (#2007), run-policy inheritance (#1958, #1959, #2006, #2008), structured logging (#1955), and internal splits with no behaviour change (#1896, #1921, #1954, #1956, #1962);
- #1891 and #1892 (RFC EM) add AWS IAM authentication for Amazon Bedrock;
- #1890 fixes paragraph breaks in the chat bridges' core;
- #1893, #1964 and #1996 fix flaky tests, and #1987 regroups Dependabot;
- 72 dependency updates came from Dependabot, and #2009 is these notes.
- `@loomcycle/client` and the Python adapter are 1.119.0. The TS client's `RunSpec` type gains `spawn.offered_overrides` (#2008), and the `sessionId` doc now says it continues the session. The Python adapter accepts protobuf 7. No method changes in either.

### A store error no longer reads as open mode (#1957)

loomcycle decides whether authentication is on by checking for `LOOMCYCLE_AUTH_TOKEN` and, failing that, by counting the admin operator tokens in its store. Any error reading that count meant open mode, so the request was served with no principal, token minting included.

- **Affected:** deployments authenticated only by a minted admin operator token, with no `LOOMCYCLE_AUTH_TOKEN`. A deployment with `LOOMCYCLE_AUTH_TOKEN` set was safe.
- **Trigger:** a database fault, or a client that half-closes its connection so its own request is cancelled before the count runs. On an unpatched build, 39 of 50 such no-bearer requests to `/v1/_me` got `200` with `open_mode:true`. The gRPC interceptor and the A2A frontier used the same check.
- **Second case:** a `LOOMCYCLE_AUTH_TOKEN` retired by an admin token authenticated again as `substrate:admin` while the count could not be read, and the answer was cached for the token-cache lifetime.

Both now fail closed: the request needs a valid bearer and gets `401` (gRPC `Unauthenticated`) without one. During a store outage, no-bearer requests get `401`, where before they were served.

### Open mode is refused on a public listener (#1949, #2000)

Open mode is a server with no `LOOMCYCLE_AUTH_TOKEN`, no config `principals:` and no admin operator token. Every route is unauthenticated. Nothing stopped it on `0.0.0.0`.

- **Boot refusal.** A server in open mode now exits at boot when `LOOMCYCLE_LISTEN_ADDR` or `LOOMCYCLE_GRPC_ADDR` binds anything but `127.0.0.0/8`, `::1` or `localhost`. `:8787`, `0.0.0.0`, `::`, a LAN address and any other host name count as public:

  ```
  auth: refusing to start in open mode on LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787: …
  ```

  Set `LOOMCYCLE_AUTH_TOKEN` (`openssl rand -hex 32`) or declare `principals:`, bind `127.0.0.1`, or, for a throwaway test harness only, set `LOOMCYCLE_ALLOW_OPEN_MODE=1`.
- A boot that cannot read the store to decide is refused too.
- **Warnings.** The open-mode warning used to check only `LOOMCYCLE_AUTH_TOKEN`, so it fired on a deployment authenticated by an admin token, and the gRPC banner said `auth=false` there. It now follows the real decision and repeats every 10 minutes. The Web UI shows an "Unauthenticated" banner in open mode. `loomcycle doctor` warns about a public listener in open mode.
- **WebSocket bearer.** A `bearer.<token>` entry in `Sec-WebSocket-Protocol` was accepted on every request. It now authenticates only a WebSocket upgrade to `/v1/client-tools`. No loomcycle client sent it anywhere else.

### Declaring config principals turns authentication on (#2000, #2001)

A deployment whose only credentials were config `principals:` ran in open mode. Every caller was the default admin in tenant `""`, the declared principals never authenticated as themselves, and minting a user token answered `500`.

- **Now:** declaring any principal turns authentication on. Requests without a valid bearer get `401` (gRPC `Unauthenticated`), each principal authenticates as itself, and `/v1/_me` no longer reports `open_mode`.
- **Fail-closed:** a principal whose `token_env` is empty still turns authentication on. If no credential can log in, every request gets `401`, and boot says so.
- **Boot warnings:** "no credential can authenticate", and "no admin credential", which names the fix: add a `substrate:admin` principal or set `LOOMCYCLE_AUTH_TOKEN`.
- **Config reload** (`POST /v1/_config/reload`):
  - A reload with principals answered `422 config_invalid` from its second load on (#2001). It works now.
  - A reload is refused when the running config turns authentication on by itself (`LOOMCYCLE_AUTH_TOKEN` or principals) and the new one does not. An admin token in the store does not make it acceptable. To move from principals to minted tokens only, restart or keep `LOOMCYCLE_AUTH_TOKEN`.
  - A reload is refused when it drops the last config admin credential and the store holds no admin token.
  - A successful reload flushes the token cache, so a removed principal stops authenticating at once.
- **First load (#2001):** a `${NAME}` reference to a principal's `token_env` elsewhere in the config, such as an MCP server header, was replaced by the bearer on the first load in a process. It is now left literal, as later loads did.
- **User-token mint:** a tenant-less caller gets `400 tenant_required` instead of `500 token_create_failed`.
- The last-admin-token retire guard stays unless an admin principal that can log in exists, read at retire time.

### Operator-token hashes are peppered by default (#1952, #1999, #2003)

With no `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, a token's stored hash was a plain SHA-256 of the token. Anyone who can write the `operator_token_defs` table could insert a row for a token of their choosing and log in with it.

- **Derived pepper.** With no explicit pepper, the pepper is now derived from `LOOMCYCLE_SECRET_KEY` (HKDF-SHA256). New hashes are stored as `p1:<key fingerprint>:<HMAC>`.
- **Existing rows.** At the first boot with a derived pepper, every stored hash is rewrapped in one transaction as `w1:<key fingerprint>:<HMAC of the old hash>`, and a marker row records that this boot happened. A `w1:` row moves to `p1:` at its token's next login. No existing token stops working.
- **The unpeppered fallback is gone.** A row written later with a plain SHA-256 hash never authenticates. Boot logs any such row by `def_id`.
- Rows hashed under `LOOMCYCLE_SECRET_KEY_PREVIOUS` still authenticate and move to the current key at login.
- **Unchanged:** an explicit `LOOMCYCLE_OPERATOR_TOKEN_PEPPER` keeps its hashes byte-identical. A deployment with neither a pepper nor `LOOMCYCLE_SECRET_KEY` is unchanged and gets a boot warning.
- An admin token row that cannot authenticate (an unpeppered row the rewrap refused, or one under a key no longer configured) no longer disables `LOOMCYCLE_AUTH_TOKEN`, and no longer counts for the last-admin retire guard.
- `loomcycle doctor` reports where the pepper comes from: explicit, derived or none.

### A shutdown pauses runs; a restart resumes them (#1998, #2005)

On SIGTERM or SIGINT the server stopped its workers and returned. Nothing paused or waited for the runs in flight. Their rows were left `running` for the stale-run sweeper to fail, and a Bash command's process outlived the server.

- **The pause.** New runs are refused (`503`) and each run in flight parks at its next iteration boundary, bounded by `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS` (default 7000; `0` turns the pause off; capped at 5 minutes). The HTTP and gRPC listeners drain alongside it for at most 5 s each. gRPC `GracefulStop` now has that 5 s deadline.
- **The interrupt.** At three quarters of the bound, the tool calls of runs not yet parked are interrupted. A Bash or Bashbox-fallback command's process group is killed, and the call fails with `interrupted: the runtime shut down while this call was running, so it may not have finished.` An interactive or resident run has its turn stopped and parks waiting for its next message.
- **The resume.** The next boot resumes every paused run. The resumed run reads the interrupted call's failure and decides whether to run it again.
- **In a cluster:**
  - every replica also runs the resume pass periodically, one replica at a time, so a rolling update that starts the new replica first still resumes the old one's runs;
  - the dead-replica sweep no longer fails paused runs;
  - each resume claims its row with a compare-and-set, and the user's run slot moves with the run.
- **Not resumed:** a non-interactive run still in a model call when the bound runs out, and a no-trace run. They end as before.
- A fan-out parent keeps its children only with `LOOMCYCLE_RESUME_FANOUT=1`. Without it the interrupt cancels its spawn and the children end cancelled.
- A blocking call (an `Interruption`, a `Channel` await, an `Agent` wait or poll) is interrupted too, so a person may see the same interruption twice.
- A memory-consolidation pass and a team subscription's walk are cancelled at shutdown. The next tick repeats them.

### Bash and Bashbox host commands end with their process group (#1950)

Only the direct child was killed on a timeout. A background process holding the output pipe blocked the call for its whole life: `sleep 600 & echo hi` took 600 s whatever the timeout.

- Bash and the Bashbox host-command fallback now run each command in its own process group. A timeout or cancel kills the whole group.
- A command that exits normally kills nothing: a `server &` keeps running. The output pipe is closed 2 s after the exit, and the call succeeds with `[output closed 2s after exit: a background process still holds it]`.
- A descendant that calls `setsid()` leaves the group and survives. Bash is still not a sandbox.
- Ctrl-C in the terminal no longer reaches in-flight Bash children directly. Stopping loomcycle interrupts them through the shutdown pause instead.

### The file tools open volume files through `os.Root` (#1950)

Read, Write, Edit, NotebookEdit, Grep, Glob and the Crypto tool's volume references checked a path, then opened the path string. Anything that could create symlinks in the volume, including gbash's `ln -s` on a read-write Bashbox mount, could swap a directory for a symlink in between and reach outside the volume.

- Every open now walks the path one directory handle at a time, checks each step is the directory the path named, and refuses a swapped one: `the path changed while it was being opened; try again`.
- Grep and Glob walk by directory handle and never follow a symlinked directory.
- **Behaviour change:** a directory inside a volume that the server may traverse but not list (mode `0100`, or a `0300` drop box) is now refused with `permission denied`. Before, Read and Write worked through it.

### Bashbox: per-command credentials and a memory cap (#1953, #1963)

- **The model can read any credential given to a shell.** The docs said a credential passed to a Bashbox host command was out of the model's reach. It is not: with `git` allowed, `git -c alias.x='!env' x` prints it. The docs and the help topic now say so plainly.
- **Per-command binding.** `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` accepts `cmd:NAME`, for example `git:GITHUB_TOKEN,gh:GH_TOKEN`. A bound credential reaches only that command. A bare `NAME` reaches every allowed command, as before.
- **Boot validation.** The server refuses to boot, even with Bashbox off, on:
  - a malformed `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` entry, or a name outside `[A-Za-z0-9_-]{1,128}`;
  - a binding to a command missing from `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`;
  - a `cmd:NAME` entry in `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV`, which has no per-command form.
- **Memory cap.** A write to a read-only volume goes to an in-memory overlay in loomcycle's own heap, which had no limit: `yes > f` passed 3 GiB in about a second. `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` (default 64 MiB, at least 1 MiB) now caps the total one call writes to in-memory files: writes, new files and directories, copies of volume files into memory (`chmod`, `touch`, `mv`, `ln`, an append) and named pipes. Deleting does not give bytes back. A write past the cap fails with `no space left on device`, and the call fails. Writes to a read-write volume go to disk and are not counted.
- **Escape suite.** `make bashbox-escape` runs every Bashbox containment test, and CI runs it on every pull request.
- See "Into a shell" in [`docs/CREDENTIALS.md`](docs/CREDENTIALS.md), the Bashbox part of [`docs/TOOLS.md`](docs/TOOLS.md), and §9d.4 of [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Egress: link-local fence, header redaction, tenant MCP servers (#1951, #1961)

- **Vouched names never reach link-local or metadata addresses.** A host name on `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST` was exempt from the private-address check wherever it resolved, so whoever controls its DNS could point it at `169.254.169.254`. A vouched name still reaches loopback, RFC 1918, CGNAT and ULA addresses, but never:
  - link-local (`169.254.0.0/16`, `fe80::/10`);
  - the metadata addresses `fd00:ec2::254`, `fd00:ec2::23` and `100.100.100.200`;
  - local-use NAT64 (`64:ff9b:1::/48`) and Teredo (`2001::/32`), whose embedded IPv4 cannot be read.

  Only a CIDR entry covering them allows them. An IPv6 address with a zone (`fe80::1%en0`) is refused.
- **An IP-literal host entry in that space refuses boot**, naming the entry that works: list `100.100.100.200/32`, not `100.100.100.200`.
- **HTTP tool header redaction.** The HTTP tool wrote every response header into the tool result, so session cookies and minted tokens reached the model. A header whose name contains `token`, `secret`, `session`, `cookie`, `credential`, `password`, `auth`, `jwt` or `bearer`, or ends in `key`, now reads `[redacted]`: `Set-Cookie`, `Authorization`, `X-Auth-Token`, `Ocp-Apim-Subscription-Key`. `WWW-Authenticate`, `Proxy-Authenticate`, `X-OAuth-Scopes`, `Access-Control-Allow-Credentials` and the pagination headers `Page-Token`, `Next-Page-Token` and `Continuation-Token` stay readable. The header lines are now sorted by name.
- **Tenant MCP servers dial through the egress guard.** `LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS` defaults to on, so no MCP-HTTP connection was guarded, including one to a server a tenant registered at runtime. A tenant's MCP server now always dials through the guard, whatever that setting says. One on a private address is refused, with a tool error that names the server, its tenant and the fix: list its host in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, or have an admin re-save the definition outside a run. Boot logs a `mcp_server_defs: WARNING:` line for each one whose host is `localhost` or a private IP literal. Operator yaml servers and admin-saved definitions are unaffected.
- An MCPServerDef saved from inside a run is never operator-authored, even in a run started with an admin token.
- The private-address rules are in [`docs/TOOLS.md`](docs/TOOLS.md).

### Webhooks answer a bad signature like an unknown name (#1951)

A webhook answered `404` for an unknown name but `401` for a bad signature, so a caller could find which names exist. A definition whose secret did not resolve answered `503` naming the env var or `$cred:` reference.

| Delivery | Before | Now |
|---|---|---|
| wrong signature, stale timestamp, missing signature header, wrong bearer | `401 {"error":"unauthorized"}` | `404 {"error":"unknown_webhook"}`, byte-identical to an unknown or disabled name |
| body over the definition's `body_size_limit_bytes` | `400 {"error":"bad_body"}` | `404 {"error":"unknown_webhook"}` |
| secret unresolvable, or `auth.kind: none` without `LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED=1` | `503` naming the secret, or `unauthenticated_mode_disabled` | `503 {"error":"unavailable"}` |

- Every name reads the body the same way, under a 30 s read deadline, so neither an `Expect: 100-continue` probe nor a stalled sender tells names apart.
- The delivery log, the delivery counts and the server log keep the real verdict and the secret's name. **A 404 from a webhook you created means a wrong secret or signature: check its recent deliveries.**
- This covers `/v1/_webhooks/{name}`, the tenant form, the forge presets and team-local webhooks. The admin dry-run `/v1/_webhooks/{name}/test` still reports the verdict.

### Operator authority never comes from inside a run (#1997, #2002)

A run started with an admin token carries the admin principal into every tool call its model makes, and the definition tools treated such a call as the operator. A prompt-injected model is not the operator.

- **Reachable before this release:** an agent in such a run could list, retire, promote or fork another tenant's AgentDefs, SkillDefs and TeamDefs, and read its evaluations and lineage. That included runs started with `LOOMCYCLE_AUTH_TOKEN`.
- **Now:** inside a run, an admin principal acts like a tenant caller. It reads and writes its own tenant, reads and forks the shared `""` tier, cannot retire or promote a shared definition, and gets the same opaque not-found any tenant's run gets for another tenant's. Off-run callers (the HTTP, gRPC and MCP operator planes, the Web UI) keep cross-tenant reach.
- The operator-only checks of the other definition tools (a free `api_key_env`, a `$cred:` not yet stored, a trigger in another tenant, another user's credential scope) also require an off-run caller. None of those tools is given to agents today.
- Every run can now read the shared `""` tier through get and list, which it could already fork and run.
- `Context op=self` reports `principal.admin_in_run: false` for an admin principal inside a run.
- Listing ScheduleDef, A2AServerCardDef or A2AAgentDef in an agent's tools now warns that the tool is not available to agents. It never was.

### `POST /v1/runs` starts runs through the shared run start (#1960, #1967, #2004)

`POST /v1/runs` and `POST /v1/sessions/{id}/messages` had their own copies of the run start that gRPC, MCP, A2A, the scheduler, webhooks and batch share, so every run feature landed twice. Both are now translators over the shared start. Golden SSE transcripts pinned the old behaviour first, and every difference was listed and decided. Most answers are unchanged. The changes:

- **`session_id` on `POST /v1/runs` is a real continuation:** the session's transcript is replayed, and the run uses the session's agent and identity. It used to append a fresh run with no replay, under the request's agent. A request naming a different agent gets `400`. MCP `spawn_run`, gRPC `Run` and `runs:batch` now refuse it the same way, where before they ran the session's agent.
- **A detached run streams live.** It used to tail the store. Its steer echo arrives with `source:"api"`, and no replayed input frame is sent. The Web UI de-duplicates the operator's echo. Re-attaching with `GET /v1/runs/{id}/stream` is unchanged.
- **An interactive continuation survives the client leaving** and can be re-attached, like a fresh interactive run.
- gRPC `Continue` takes the cluster-wide session lock, as HTTP does.
- The system prompt is no longer stored inside a run's `user_input`.
- A cancelled run records its last events, and a run cancelled mid-turn records one error event, not two.
- Continuations now get the fan-out cap, the run overrides, recall and the memory harvest.
- A continuation sent right after a cancel or a run's end waits up to 5 s for that run to finish, instead of `409 session_busy`.
- A run's stream no longer drops a `done` written just before its terminal status.

### Interactive runs end after an hour idle (#2004)

A parked interactive run waited for input forever.

- A run parked at `awaiting_input` with no message for `LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT` (default `1h`; `0` waits forever) now ends `completed`, with stop reason `idle_timeout`, on the answer it already gave.
- The session is kept. A normal continuation resumes it with the whole history.
- A steer after the end gets `404`, with a hint to continue the session. The chat bridges already treat a `404` as "the run ended".
- It covers every top-level run. It does not cover review holds, sub-runs or resident children, which have their own bounds.
- A value that does not parse as a Go duration is refused at startup.

### Agent-restricted tokens read only what their list reaches (#2004, #2007, #1958, #1959)

A token limited to some agents (`restrictions.agents`) was confined on the run plane, but its reads were checked by tenant only.

- **Reads (#2007):** a run is readable when the root of its spawn tree is a listed agent, and a session when every run in it is. Elsewhere the answer is the route's own not-found, byte-identical to a nonexistent id. This covers 16 read paths on HTTP, MCP and gRPC: transcripts, runs, streams, prompts, walk and child listings, user agents, interrupts and the run-state stream. Unrestricted, admin, legacy and open-mode callers are unchanged.
- **Two laundering paths are closed:** `POST /v1/sessions/{id}/replay` of a session the token cannot read, and an `idempotency_key` another caller used, which returned that caller's run.
- **Continuation refusals (#2004)** no longer name the session's agent, and give one answer whatever agent the request names. An unreachable session answers `404`, where it was `403`.
- **Team walks (#1958):** a walk started from an operator plane passed the operator's authority to every member, so a tenant member's walk could record AgentDefs as operator-authored and create schedules under `any`. Members now start without it.
- **Hook host grants (#1959):** a hook's `allow_hosts` for one `Agent` call reached the child's whole run. It now stops at the spawn.

### Amazon Bedrock with AWS credentials (#1891, #1892)

loomcycle reached Bedrock only with a static Bedrock API key. A provider can now authenticate with AWS IAM:

```yaml
providers:
  bedrock-claude:
    driver: anthropic
    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic
    auth:
      type: aws_sigv4          # or aws_bedrock_token
      region: eu-central-1     # must match base_url
      # role_arn / external_id_env / session_name / service: optional
```

- `aws_sigv4` signs every call. `aws_bedrock_token` mints Bedrock's short-term key and re-mints it after 80% of its life.
- Credentials come from the AWS SDK's chain: environment, shared files, ECS, EKS and EC2 instance roles, with an optional `AssumeRole`. They refresh before they expire. The EC2 metadata service is used through IMDSv2 only.
- It works in the existing Anthropic and OpenAI drivers, on the `bedrock-runtime` and `bedrock-mantle` hosts. The routing probe lists Bedrock's models.
- A tenant's own key overrides the operator's under the name `AWS_BEARER_TOKEN_BEDROCK`. Every secret obtained is registered with the redactor.
- **`bedrock` preset:** `LOOMCYCLE_PRESETS=base,bedrock` adds `bedrock-claude` and `bedrock-openai` for `LOOMCYCLE_BEDROCK_REGION`. It changes no routing.
- `loomcycle doctor` checks each `auth` provider's AWS identity, and warns when host Bash or the Bashbox host fallback is on beside one, since a host command can read the instance role's credentials. The Web UI routing page shows an admin how each provider authenticates.
- See "Amazon Bedrock with AWS credentials" in [`docs/PROVIDERS.md`](docs/PROVIDERS.md), with the IAM policy.

### Structured logging (#1955)

- `LOOMCYCLE_LOG_FORMAT=text|json` (default `text`) and `LOOMCYCLE_LOG_LEVEL=debug|info|warn|error` (default `info`).
- Text output is byte-identical to before. JSON is one object per line, with `run_id`, `tenant`, `agent`, `user`, `trace_id` and `span_id` where a line belongs to a run.
- 159 log calls in 29 packages are converted; the rest follow. An unconverted line is never hidden by the level, so a fatal reason always prints.
- The values of secret-named env variables are masked in log lines.

### Run-policy inheritance (#2006, #2008)

A run's policy was set value by value through 26 context setters and copied into child runs field by field. It is now one typed run policy with one inheritance rule per field, and a test fails on a field without a rule. Fixed on the way:

- a child's fan-out cap is now the smaller of its parent's and its own;
- a spawn's compaction override, parallel-spawn index and timeout no longer reach descendants;
- a locked child no longer inherits its parent's refused compaction or context settings, and its host list can only narrow;
- a resumed sub-run offers its children the overrides it offered live (#2008). In an A→B→A′ chain, an `interruption` narrowing from A was lost after a restart. The offer is recorded as `spawn.offered_overrides` in the child's run record.

### Smaller fixes

- **Chat bridges (#1890):** an agent's text before and after a tool call ran together into one paragraph. `@loomcycle/bridge-core` 0.2.1 starts a new paragraph. It publishes on its own tag, and the bridges move to it in a later release.
- **Env catalogue (#1988):** a duplicated `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` entry is removed, so the TrueNAS form matches its generator.

### Supply chain and CI (#1894, #1895, #1897, #1948, #1966, #1987)

- **Vulnerable dependencies (#1897):** `golang.org/x/net` moves to v0.60.0 and OpenTelemetry to v1.45.0, for five vulnerabilities `govulncheck` found reachable.
- **Go toolchain (#1966):** CI and release builds now take the newest Go 1.26 patch, not the runner's cached one. That puts the fix for GO-2026-6809 (`crypto/x509`, Go 1.26.10) into the release binaries.
- **Scanning (#1895):** `govulncheck` fails a pull request on a reachable vulnerability; gosec reports, and CodeQL scans Go and TypeScript.
- **Reporting (#1894, #1948):** `SECURITY.md` describes private reporting through GitHub Security Advisories, with response targets.
- **Dependabot (#1894, #1948, #1987):** weekly updates for Go modules, npm, pip, GitHub Actions, Dockerfiles and compose files. npm is grouped by dependency across every package, and majors of the dev toolchain are taken by hand.

### Internal restructuring (no behaviour change)

- `main()` is split from 3,240 lines into ordered boot phases (#1954), and the 1,617-line loop into per-phase methods (#1962).
- The 404-method `Store` interface is split into 38 domain interfaces (#1921), and a shared SQL dialect layer serves both backends, used first by dirents and volumes (#1956).
- `scripts/js-packages.sh` and `make js-install`, `js-build`, `js-test`, `js-typecheck` and `js-audit` run npm tasks across every JavaScript package (#1896).

### Dependency updates

72 Dependabot updates:
- **Go modules:** a group of 18, among them gRPC 1.84, pgx 5.11, modernc sqlite 1.60, a2a-go 2.6 and OpenTelemetry 1.47.
- **npm:** across the Web UI, the TS adapter and the packages, including TypeScript 7, Vitest 5, Vite 8 and `@types/node` 26 in most packages. Further majors of the dev toolchain are taken by hand (#1987).
- **GitHub Actions** to new majors: `actions/checkout`, `setup-go`, `setup-python` and `setup-node`; `docker/setup-buildx-action`, `setup-qemu-action`, `login-action` and `build-push-action`; `goreleaser/goreleaser-action`.
- **Docker:** the Web UI build stage to `node:26-alpine`, pinchtab v0.15.2 in the browser image and compose files, nginx 1.31 in the cluster compose file. The bridge images, and the cloud stack's loomboard and landing images, move to Node 26 when they are next built.
- **Python adapter:** protobuf below 8.

### Upgrade notes

- **No migration and no schema change.** The token rewrap stores one marker row, and a finished run's pause state is now cleared; neither needs an action.
- **Act on these before you upgrade:**
  - **Open mode on a public listener is refused at boot.** A server with no `LOOMCYCLE_AUTH_TOKEN`, no `principals:` and no admin token now exits when `LOOMCYCLE_LISTEN_ADDR` or `LOOMCYCLE_GRPC_ADDR` binds beyond loopback. Set `LOOMCYCLE_AUTH_TOKEN`, bind `127.0.0.1`, or, for a test harness only, set `LOOMCYCLE_ALLOW_OPEN_MODE=1`. Docker: `-e LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787` now needs `-e LOOMCYCLE_AUTH_TOKEN=…`.
  - **Config `principals:` turn authentication on.** A deployment that declared principals and relied on open mode now answers `401` to a request without a valid bearer. A principal with an empty `token_env` turns it on too.
  - **Token hashes, when `LOOMCYCLE_SECRET_KEY` is set and `LOOMCYCLE_OPERATOR_TOKEN_PEPPER` is not:**
    - **upgrade every replica together.** An older release cannot read the `p1:` and `w1:` forms, so a token is refused by an old replica once a new one has rewrapped or rehashed it;
    - **rolling back means re-minting every operator token**, since the first boot rewraps every stored hash;
    - from now on, removing `LOOMCYCLE_SECRET_KEY`, or changing it without `LOOMCYCLE_SECRET_KEY_PREVIOUS`, logs out every operator token. Changing an explicit pepper does too, as before.
  - **Bashbox credentials:** a malformed `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` entry, a binding to a command not in `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`, or a `cmd:NAME` entry in `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV` now refuses boot, even with Bashbox off.
  - **Private-host allowlist:** a link-local or metadata IP literal listed as a host entry (such as `100.100.100.200`) now refuses boot. List it as a CIDR (`100.100.100.200/32`).
  - **Tenant MCP servers on private addresses** are refused at dial, whatever `LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS` says. List the host in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, or have an admin re-save the definition outside a run. Boot lists the affected definitions.
  - **Shutdown:** a replica's in-flight runs pause on SIGTERM for up to `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS` (default 7000 ms), which leaves room within Docker's default 10 s stop grace. To give runs longer, raise it together with `stop_grace_period`, `terminationGracePeriodSeconds` or `TimeoutStopSec`. A fan-out parent keeps its children across the restart only with `LOOMCYCLE_RESUME_FANOUT=1`. `0` restores the old shutdown.
- **Behaviour changes:**
  - **Webhooks:** a bad signature, a stale timestamp, a missing signature header, a wrong bearer and an over-size body all answer `404 {"error":"unknown_webhook"}`. An unresolvable secret answers `503 {"error":"unavailable"}`. A 404 from your own webhook means a wrong secret: see its delivery log.
  - **HTTP tool:** credential-bearing response header values read `[redacted]`, and the header lines are sorted.
  - **Egress:** a vouched host name never reaches link-local, metadata, local-use NAT64 or Teredo addresses. Only a CIDR entry covering them does.
  - **File tools:** a traverse-only directory inside a volume (mode `0100`, or a `0300` drop box) is refused.
  - **Bashbox:** on a read-only volume, a call that writes more than `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` (default 64 MiB) to in-memory files fails with `no space left on device`.
  - **Bash:** a command that leaves a background process holding its output returns 2 s after it exits. A timeout kills the whole process group.
  - **An admin token's run stays in its own tenant.** It reads, but cannot retire or promote, shared `""` definitions. Those are the ones made in open mode or before the first token, by yaml schedules or webhooks, or through the stdio `loomcycle mcp`. Runs started with `LOOMCYCLE_AUTH_TOKEN` execute in tenant `default`, so this applies to them, and Evaluation no longer shows them the evaluations of runs fired by yaml schedules or webhooks. Do cross-tenant work from an operator surface.
  - **Interactive runs:** a parked run with no message for `LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT` (default 1h) ends `completed` with `idle_timeout`. Its session is kept. A steer after that gets `404`. Set `0` to wait forever.
  - **`POST /v1/runs` with `session_id`** continues the session: its transcript is replayed and its own agent runs. A request naming a different agent gets `400`.
  - **A detached `POST /v1/runs`** streams live. The steer echo arrives with `source:"api"`, and no replayed input frame is sent.
  - **Agent-restricted tokens** read only runs and sessions whose spawn-tree root is on their list. Anything else answers the route's not-found, and listings drop it. A continuation of an unreachable session answers `404`, where it was `403`.
  - **WebSocket bearer:** `Sec-WebSocket-Protocol: bearer.<token>` authenticates only the WebSocket upgrade to `/v1/client-tools`.
  - **User-token mint** from a tenant-less caller: `400 tenant_required`, was `500`.
- **New env vars:**
  - `LOOMCYCLE_ALLOW_OPEN_MODE` (default off);
  - `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS` (default 7000);
  - `LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT` (default `1h`);
  - `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` (default 64 MiB);
  - `LOOMCYCLE_LOG_FORMAT` (default `text`) and `LOOMCYCLE_LOG_LEVEL` (default `info`);
  - `LOOMCYCLE_BEDROCK_REGION`, for the `bedrock` preset.
- **Release binaries** are built with the newest Go 1.26 patch.
- **Images:** a minor tag rebuilds every image. All are at 1.119.0, for amd64 and arm64: `loomcycle`, `loomcycle-toolbox`, `loomcycle-browser` and the sandbox images.

## What's in v1.118.1

*A token limited to an agent named in a group, such as `chat/medium`, could not be minted: the request was refused with 400 `invalid_restrictions`. Every bridge mints each of its users such a token for its one agent, so no bridge could answer anyone.*

A patch on the v1.118 line: #1888.
- `@loomcycle/client` and the Python adapter stay at 1.118.0, with no API change.

### A token's agent list accepts a grouped name (#1888)

A token can be limited to a list of agents (`agents` on `POST /v1/_users/{subject}/tokens`, and on every transport's mint). Each name was checked against a pattern without `/`, though agents are named in groups (`chat/medium`, `doc/manager`). A mint for a grouped agent was refused:

```
400 {"code":"invalid_restrictions","error":"agents: \"chat/medium\" is not a valid agent name"}
```

The bridges mint each platform user a token for the bridge's agent, `chat/medium` by default. The Telegram bridge on loomcycle.cloud answered every message with "Something went wrong. Please try again."

The pattern now accepts `/`. It stays as wide as an agent name can be and no wider. A listed name that cannot exist admits nothing, as before.

### Upgrade notes

- **No migration and no config change.**
- **Images:** a patch tag rebuilds only `denngubsky/loomcycle-browser` (linux/amd64). The plain `loomcycle`, toolbox and sandbox images stay at 1.118.0.

## What's in v1.118.0

*A queued run that never starts now ends cleanly: its idempotency key is released, so a retry runs the work, and its end is published, so a client waiting on it is not left hanging. A restart during a pause no longer holds the boot or a snapshot restore until the resume. Also: an agent told it hit the configured-run cap no longer retries forever, and only an operator may change a shared HookDef.*

A minor release of fixes: #1881 to #1887.
- #1885 is the queued-run fix;
- #1883 classifies configured-run refusals by their code;
- #1884 makes a shared HookDef the operator's;
- #1882 lets the bridge image workflow build a tag older than its Dockerfile;
- #1881 and #1886 are documentation, and #1887 is these notes.
- `@loomcycle/client` and the Python adapter are 1.118.0, with no API change. The Python adapter's comments now say the server names `configured_run_cap`.

### A queued run that never starts ends cleanly (#1885)

A run that waited in the run queue and ended without starting (its wait was cancelled, or it was refused at its start) was closed like a run that had finished. Three things were wrong:

- **Its idempotency keys stayed bound.** A caller's retry with the same key was answered with the dead run instead of running the work.
- **Its stored request was kept.**
- **Its end was never published** on the run-state stream. A client holding the run id from its queued answer waited for a terminal frame that never came.

The store gains `EndQueuedRun`: it ends a run that is still queued with the given status, releases its idempotency keys and clears the stored request, as already happened for a run a restart cannot start. Every path that ends a queued run now publishes the end, the restart path included.

**A restart during a pause.** A run that a restart queues again, and that finds its slot free, can still have to wait out a pause. It was not announced until the pause ended, so it could not be cancelled, and the restarter waits for each run to be placed. That held the boot, or the snapshot restore that restarted it, until the resume. Such a run is now announced before it waits, as a queued run is.

### An agent told it hit the configured-run cap stops retrying (#1883)

The `configured_run` MCP tool classified a refusal by its HTTP status:

| Refusal | Before | Now |
|---|---|---|
| `configured_run_cap` (429) | transient, retryable: the agent retried forever, though only starting or discarding a draft clears it | business, not retryable, telling the agent to start or discard a draft |
| `rate_limited` (429) | retryable, no wait | retryable, with `retryAfterSeconds` |
| `no_store` (503) | transient | business, not retryable: the deployment has no store, so start the run at once |
| `agent_not_allowed`, `override_not_allowed` (403) | unclassified | permission |

The classification now lives in one table keyed by the code each refusal names, read by both MCP and gRPC, so the two cannot disagree. gRPC configured-run refusals gain the same `category` and `is_retryable` metadata. A test fails when a new refusal code is neither classified nor recorded as deliberately unclassified.

### Only an operator may change a shared HookDef (#1884)

A shared HookDef (written with no tenant) is what every tenant's agents fall back to when they name a hook their own tenant does not define. v1.115.0 kept another tenant's agents out of the no-trace gate's answer. That left a tenant's no-trace agent falling back to a shared hook open to a change by a tenantless non-operator.

Now every create, fork, promote, retire or delete of a shared HookDef by a non-operator is refused, with one message. The decision is made before anything is read, so the answer depends on the caller alone and says nothing about any tenant's agents. An operator's change is audited, as before, when a no-trace agent names the hook. Changes to a tenant's own HookDefs keep the per-tenant gate.

### Upgrade notes

- **No migration and no config change.**
- **Behaviour changes:**
  - A tenantless caller that is not an operator can no longer write a shared HookDef.
  - An MCP agent receives `configured_run_cap` and `no_store` as not retryable.
- **Images:** every image is rebuilt at 1.118.0, for amd64 and arm64: `loomcycle`, `loomcycle-toolbox`, `loomcycle-browser` and the sandbox images.

## What's in v1.117.0

*The **Bashbox sandbox can no longer be crashed by the script it runs**: gbash moves to v0.0.40, which turns two one-line scripts that killed the whole loomcycle process into ordinary failed commands, and hardens its symlink, size and time limits. Also: **chat bridges for Slack, Telegram and Discord**, beside Teams, on one shared core, each published to npm on its own version line.*

A minor release: #1871 to #1880.
- #1877 upgrades gbash, the Bashbox sandbox, from v0.0.38 to v0.0.40;
- #1871 adds `@loomcycle/bridge-core`, the shared core of the chat bridges, and #1876 gives it per-platform `/help` text;
- #1872 moves the Teams bridge onto it as `@loomcycle/bridge-teams`;
- #1873, #1874 and #1875 add `@loomcycle/bridge-telegram`, `@loomcycle/bridge-slack` and `@loomcycle/bridge-discord`;
- #1878 publishes a container image for each platform bridge on its tag, and #1879 adds an optional Telegram bridge to the loomcycle.cloud stack;
- #1880 is these notes.
- `@loomcycle/client` and the Python adapter are 1.117.0, with no change in either.

### A script can no longer crash loomcycle through Bashbox (#1877)

Bashbox runs gbash inside the loomcycle process. On gbash v0.0.38 two scripts hit a fatal Go runtime error, which no recovery can catch, so either one ended the process and every run on the replica, for every tenant:

| Script | Before | Now |
|---|---|---|
| `jq -c -n 'reduce range(3000000) as $i (0;[.])'` | `fatal error: stack overflow` | exit 5, "value nesting exceeds depth limit of 10000" |
| `echo a \| dd bs=200000000000000` | `fatal error: out of memory` | exit 1, "bs=N cannot fit into memory" |

gbash v0.0.39 and v0.0.40 are almost entirely security hardening:
- **Crashes:** jq nesting depth is bounded, and a panic in a pipeline, background job or process substitution fails that subshell instead of the process.
- **Size and time:** `dd`, `truncate`, `gunzip` and `tar -x` respect the file-size limit; text builtins, awk, brace expansion, shell nesting and variable size are bounded; copy loops and walks stop at the timeout.
- **Symlinks:** the write, remove and rename policy resolves symlinked parents, and the escapes through `tar`, `cp` and `find` are closed.
- **Host probes:** builtins no longer check whether paths exist on the host disk.
- **Hangs:** named pipes and deep or malformed shell input no longer hang or crash the parser.

Behaviour changes, all toward GNU bash: `find` no longer follows symlinks without `-L`, creating a file under a missing directory fails, and `GBASH_COMPAT_ROOT` in the script environment is ignored. No loomcycle setting changes.

### Chat bridges for Slack, Telegram and Discord (#1871 to #1879)

A bridge connects a chat platform to a loomcycle agent: each person who writes to the bot gets their own conversation with the agent, under their own token, with the agent's questions, review holds and results drawn as the platform's cards or buttons. Until now only Teams had one. The platform-free part of it is now **`@loomcycle/bridge-core`**, and four bridges are built on it:

| Package | Platform |
|---|---|
| `@loomcycle/bridge-teams` | Microsoft Teams (formerly the unpublished `@loomcycle/teams-bridge`) |
| `@loomcycle/bridge-slack` | Slack: DMs and channel threads, buttons, answers streamed in DMs |
| `@loomcycle/bridge-telegram` | Telegram: a webhook, inline keyboards, optional streaming in private chats |
| `@loomcycle/bridge-discord` | Discord: slash commands (`/ask`, `/new`, `/stop`, `/help`) over HTTP interactions, buttons; server installs only, free-text messages later |

- **Trust is per speaker.** Text is trusted only in a fenced space: a 1:1 chat with an allowed user, or an allow-listed workspace or tenant. Everyone else's text reaches the agent as an untrusted block, so the agent reads it as data, it never reaches memory, and it never steers a live run. An untrusted speaker may press the buttons the agent offered but cannot type an answer.
- **A guest never owns a conversation.** A Slack Connect guest is admitted, as untrusted, in a channel and refused in a DM; a conversation a guest opens has no owner until a member speaks.
- **Each platform's safety rules:** no broadcast mentions, no link previews or unfurls, no link label that hides its target, the bot's own and other bots' messages dropped, platform retries de-duplicated.
- **Configuration** is `BRIDGE_*` for the shared settings plus each platform's own keys. The Teams bridge's `TEAMS_BRIDGE_*` settings become `BRIDGE_*`, except `TEAMS_BRIDGE_ALLOWED_TENANTS`; `BRIDGE_DEV` must now be `1` or `0`.
- **Releases:** each package has its own `bridge-<name>-v*` tag and 0.x version, published by `publish-bridges.yml`, which refuses a package that depends on a sibling by path. Published: `bridge-core` 0.2.0 and the four bridges at 0.1.0. The bridges are on core 0.1.0 and adopt the per-platform `/help` text of 0.2.0 in a later minor.
- **Container images (#1878):** each platform bridge tag also publishes `denngubsky/loomcycle-bridge-<name>` (and the `ghcr.io/denn-gubsky/` mirror) for amd64 and arm64, built from the package source at the tag: `node:22-alpine`, running as `node`, with a writable `/data` for `BRIDGE_STORE=file:`.
- **loomcycle.cloud (#1879):** the cloud stack's compose file has an optional `telegram-bridge` service, off unless `COMPOSE_PROFILES=telegram`, reached by Telegram through the stack's Cloudflare tunnel; its `INSTALL.md` has the steps.

## What's in v1.116.0

*A **`Crypto` tool whose keys the model never sees.** An agent names a key; the runtime encrypts, decrypts, signs, verifies, MACs or makes a one-time code, and only the result comes back. Keys live in loomcycle (sealed under `LOOMCYCLE_SECRET_KEY`) or in **Vault Transit, AWS KMS or Google Cloud KMS**, where the material never leaves the KMS. The same operations are on the operator API (`POST /v1/_crypto`, gRPC, MCP, both clients), with JWE public-key encryption, RSA-PSS, Argon2id passwords, and data read from and written to a volume, memory or a document by reference, so the model never reads it. Also: **untrusted text never reaches memory**, and **another user who continues a session never gets the session user's credentials**.*

A minor release: #1861 to #1868.
- #1865, #1866, #1867 and #1868 add the `Crypto` tool, its operator API, JWE / X25519 / RSA / passwords / references, and keys held in an external KMS;
- #1862 and #1863 keep untrusted text out of memory;
- #1864 withholds a session user's credentials from another user's work in that session;
- #1861 fixes a flaky webhook test.
- `@loomcycle/client` and the Python adapter are 1.116.0.

### The Crypto tool: keys the model never sees (#1865)

An agent that had to protect a value or sign a request was handed the key in its prompt, where it landed in the transcript, provider logs and snapshots, or shelled out through Bash with the key in its environment. Now:

```json
{"op": "encrypt", "key": "orders", "plaintext": "4111…", "context": "order:7"}
→ {"ciphertext": "lcx1:kt:orders:3:…", "key_version": 3}
```

- **Ops:** `encrypt`, `decrypt`, `rewrap`, `mac`, `verify_mac`, `sign`, `verify`, `public_key`, `totp`, `hash`, `random`.
- **Algorithms:** AES-256-GCM, XChaCha20-Poly1305, HMAC-SHA256/512, Ed25519, ECDSA P-256 (PEM / JWK public keys, JWS), RFC 6238 TOTP, SHA-2/3. The algorithm is fixed by the key's type, never by the call. Nonces come only from the runtime.
- **The envelope** is `lcx1:<source>:<name>:<version>:<body>`. The source names the key's kind and scope (`kt`/`ku`/`ka` a crypto key in tenant/user/agent scope, `ct`/`cu`/`ca` an ordinary credential, `r` a run credential). The associated data binds tenant, source, name, version and the caller's `context`, so a value moved to another key, scope or context does not open.
- **Crypto keys** are CredentialDef rows with `usage: crypto`, a fixed `key_type`, numbered versions and `min_version`. New CredentialDef ops: `generate`, `rotate`, `set_min_version`, `delete_version`, `export_key`, `import_key` (export/import under a passphrase, admin-only, outside a run). Inside a run, an agent manages keys only in its own agent scope. At most 32 usable versions per key.
- **One way only:** no `$cred:` path resolves a crypto key. An ordinary credential may serve as key material (`cred`), unless it carries `allowed_urls`.
- **A key name has no scope precedence:** it resolves only when exactly one allowed scope holds it; otherwise the call names `key_scope`. An envelope records its scope, so a same-named key that appears later can neither open old data nor block it. (Precedence would let a user plant a key under the tenant key's name and verify their own forgeries.)
- **`crypto_scopes`** on the AgentDef (`agent` / `user` / `tenant` / `run`) is default-deny: with none, only `hash` and `random` work. Sub-agents narrow to their parent's scopes; it is not per-run overridable and not part of `content_sha256`.
- **Failures are opaque:** a wrong key, context or tampered body is "did not decrypt" / `valid: false`.
- **`sensitive: true`** on a decrypt stores a marker instead of the result. `plaintext` (and CredentialDef's `passphrase` / `blob`) are masked in the stored tool call.
- **Limits:** 1 MiB per argument and 100 ops per run (`LOOMCYCLE_CRYPTO_MAX_BYTES`, `LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN`).
- **Audit and metrics:** every keyed op and key change is audited, with no data or key material; `/metrics` gains `loomcycle_crypto_ops_total{op,outcome}`.
- **Web UI:** the credentials panel lists crypto keys with their versions, and generates, imports, rotates, retires and deletes them.

### The operator API on every transport (#1866)

A service outside any run (a backend sealing a field before writing it, or checking a partner's webhook signature) can now use the tenant's keys without holding them.
- **HTTP** `POST /v1/_crypto`, **gRPC** `rpc Crypto`, the **MCP** `crypto` meta-tool, **TS** `crypto(input)` plus typed key helpers (`generateCryptoKey`, `rotateCryptoKey`, `exportCryptoKey`, …), **Python** `crypto(input)`; OpenAPI `CryptoToolInput`, drift-tested against the tool's schema.
- **Keys come from the principal:** its tenant's keys and its own subject's user keys. Agent keys and run credentials are not reachable. An admin may name another tenant, and then gets only that tenant's keys.
- **Requires `substrate:tenant`**, and is carved out of member access: a member who could decrypt with the tenant's keys could read whatever the tenant sealed.
- **Limits:** each call takes one from the token's run rate (`429` with `Retry-After`, gRPC `ResourceExhausted`, an MCP tool error with `retryAfterSeconds`). The per-run budget does not apply.
- **Interop:** the input, envelope and errors are the tool's own, so a value the API seals opens in the tenant's runs, and the reverse.
- The audit actor is the caller, not the tenant whose keys were used.

### JWE, X25519 / RSA, passwords and data by reference (#1867)

- **New key types:** `x25519`, `rsa-oaep-2048|3072|4096` (encrypt to the public half, decrypt) and `rsa-pss-2048|3072|4096` (sign / verify, JWS `PS256`). Purpose is part of the type, so one RSA key never both signs and decrypts.
- **JWE** (compact): `dir` for `aes-256-gcm` (`format: jwe`), `ECDH-ES` for X25519, `RSA-OAEP-256`; `enc` is always `A256GCM`. A token naming another alg or enc, or carrying `zip` / `crit`, is refused, and an RSA unwrap failure continues with a random key, so failures stay opaque. The `kid` picks the version; rewrap keeps a JWE a JWE.
- **Encrypt to someone else:** `encrypt` with `public_key` seals to an X25519 / RSA key the runtime does not hold; like `verify` with `public_key`, it needs no crypto scope.
- **Passwords:** `hash_password` / `verify_password`, Argon2id PHC strings (m=19 MiB, t=2, p=1). Verify bounds the parameters a stored hash may name, so a supplied hash cannot exhaust the host. `password` is masked in the stored call.
- **`from` / `to` by reference:** read an op's input from, and write encrypt / decrypt / rewrap output to, `volume:[<name>:]<path>`, `memory:<scope>/<key>` or `document:<scope>/<chunk_id>`. With `to`, the result is `{to, bytes, …}`, never the content.
  - Each store is reached through its own checks (the volume fence and atomic writes, Memory's scope and write checks, the Document tool's scope, ontology and revision).
  - The run must hold the matching tool (`Read`, `Write`, `Memory`, `Document`) in its **effective** tool list, and that tool's **pre-hooks** judge the call.
  - Memory and chunks hold text, so binary output is refused there. References are refused on the operator API.

### Keys held in Vault, AWS KMS or Google Cloud KMS (#1868)

Many teams must keep keys in a KMS they already run and audit. A crypto key can now be a **reference** to such a key: the agent, the scopes and the API stay the same, and every op on the key is a KMS call.

```json
{"op": "create", "usage": "crypto", "scope": "tenant", "name": "orders",
 "key_type": "aes-256-gcm", "backend": "vault",
 "external": {"address": "https://vault.internal:8200", "key": "orders", "token": "$cred:vault-token"}}
```

- **Backends:** `vault` (Transit; `address`, `key`, `mount`, `namespace`, `token`), `aws_kms` (`region`, `key_id`, `access_key_id`, `secret_access_key`, `session_token`; SigV4), `gcp_kms` (`key`, `service_account`).
- **Created outside a run only**, in tenant or user scope. Before storing, loomcycle asks the KMS that the key exists, is enabled and is of `key_type`.
- **KMS credentials** (`$cred:`) resolve only in the key's own scope: a user's key runs on that user's KMS credentials, a tenant's on the tenant's. A credential carrying `allowed_urls` cannot back one.
- **The reference is sealed** with its usage, key type and backend, so a database edit leaves the key unopenable, never pointed at another KMS. Lists show `key_backend`, not the address.
- **Versions belong to the KMS:** `rotate`, `set_min_version`, `delete_version` and `export_key` are refused; `delete` removes only the reference. The envelope carries the KMS's version (0 for AWS), and the associated data still binds the key's name and `context`, so a value moved under another name does not open even when one KMS key backs both.
- **rsa-oaep** keys encrypt as a JWE to the KMS's public key, and the KMS unwraps it. JWS signing goes through the KMS. An `aes-256-gcm` KMS key has no JWE form.
- **Vault's host** must be on `LOOMCYCLE_HTTP_HOST_ALLOWLIST` or `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, checked at create and at every use, and is dialled through the SSRF guard with redirects off. `LOOMCYCLE_KMS_VAULT_ALLOW_HTTP=1` admits a plain-http dev Vault.
- **Failures:** a KMS that is down is `transient`, refused credentials `permission`, a missing key not found, a decrypt failure the usual opaque answer. An error never carries a KMS response body or token.
- **TS** `createKmsCryptoKey`; the **Web UI** names the KMS and offers only delete for such a key.

### Untrusted text never reaches memory (#1862, #1863)

v1.115.0 kept untrusted blocks fenced on the memory paths. They still reached the extractor, the archive-time evidence quote and the trace index. Now nothing from them does.
- **An allowlist of dialogue:** only the conversation's own text (the caller's trusted text, the model's replies, operator steers) is marked `Dialogue`, and every memory path banks only that: consolidation, compaction, recap, stateful harvest, the archive-time evidence span, the trace index (user and assistant turns only) and recalled source turns. Tool output (WebFetch, HTTP, MCP results) counts as untrusted.
- **History `get` gains `omit_untrusted`** (with `format: "conversation"`), which drops untrusted blocks by their stored type. The consolidator always asks for it.
- **The extractor's prompt** says text inside a `<kind>…</kind>` fence is outside material, never the user's own words (#1863). Its extraction baseline is re-measured and scores the same as the previous prompts, with no violations.
- An agent that deliberately writes what it read with `Memory add` / `set` is out of scope, and the docs say so.

### Another user's work in a session (#1864)

Any non-isolated principal could continue another member's session, and the run ran as that member, so bob continuing alice's chat used alice's stored credentials. Work sent by another user (a different tenant or subject, admins included) now runs as the session's user **without that user's stored credentials**:
- continuations on every transport, `POST /v1/sessions/{id}/messages`, a configured run started or edited by another user, steers, retunes, review feedback, turns sent to resident children, and the compaction and recap made for them;
- every credential reader asks one `NoUserScope()`; the bit is written to the run's row first, so it holds across replicas and on resume;
- the caller's own per-run `user_credentials` / `user_bearer` still reach the run.

Open mode and the legacy `LOOMCYCLE_AUTH_TOKEN` (an integration speaking for its own users) are unchanged. Not yet covered: an interruption answer.

### Upgrade notes

- No schema migrations.
- **Behaviour changes:**
  - Continuing another user's session, or steering, retuning or reviewing their run, withholds that user's stored credentials.
  - A stateful run's `harvest_to_memory` banks nothing (its spans were tool observations and evicted state), and recap or summary text is no longer re-banked.
  - Stored blocks typed other than `""` / `trusted-text` (such as a legacy `"text"`) no longer count as the user's words on memory paths. Trace rows indexed under a kind age out with the index TTL.
- **Crypto keys need `LOOMCYCLE_SECRET_KEY`**, also for a KMS key, whose reference is sealed. A Vault address must be on an HTTP host allowlist.
- New env knobs: `LOOMCYCLE_CRYPTO_MAX_BYTES`, `LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN`, `LOOMCYCLE_KMS_VAULT_ALLOW_HTTP`.

## What's in v1.115.0

*An untrusted block can carry **code kept exactly as written**, in a fence the code cannot close. Otherwise this release is **fixes from a code review** of the last two lines of work: the run queue and provider lanes, and the content-safety features (no-trace runs, restricted tokens, trigger credentials, forge webhooks, git sources, read-only volumes). A no-trace child's answer is no longer stored in its parent, a token limited to some agents can no longer control other agents' runs, and an A2A token reaches only its own tenant. A YAML-declared MCP server that **starts after loomcycle** is now picked up by the next run under any tenant.*

A minor release: #1832 to #1859.
- #1833 adds the `code` kind of untrusted block;
- #1832, #1834 to #1837, #1839 to #1842, #1844 and #1846 fix and finish the run queue, call lanes and queue-aware routing;
- #1848, #1853 and #1854 fix no-trace runs;
- #1847, #1851 and #1855 fix restricted tokens and A2A;
- #1849 and #1858 fix whose credential a trigger's run uses;
- #1850 and #1852 fix forge webhooks;
- #1838 and #1856 keep untrusted text fenced after the loop;
- #1843, #1845 and #1859 fix git sources, volumes and `Read`;
- #1857 recovers an MCP server that starts after loomcycle.
- `@loomcycle/client` and the Python adapter are 1.115.0.

### Code kept exactly as written (#1833)

Every untrusted block is escaped: every spelling of `<` becomes `‹`, and invisible characters are dropped. Code under review then reaches the model changed: `Vec<T>` loses its `<`, a bug that turns on `&lt;` versus `<` cannot be seen, and a bidi control (Trojan Source) is removed before anyone can flag it.

```json
{"type": "untrusted-block", "kind": "code", "text": "if a<b && s == \"&lt;\" { … }"}
```

- **The fence** is `<untrusted-code b="H">…</untrusted-code b="H">`, where `H` is the first 128 bits of the text's SHA-256. It depends only on the text, so a replay is byte-identical and the prompt cache holds. If the text contains `H`, a salted boundary is tried, and after eight tries the block is escaped as before.
- **One rewrite inside the text:** the fence's own tag name gets `‹` for its `<`, so the only `</untrusted-code` the model sees is the real one.
- **Invisible characters are shown, not dropped,** as `⟨U+202E⟩`: bidi and zero-width characters, variation selectors, controls other than tab/LF/CR, a lone CR, U+2028/2029.
- **A size bound:** a block that would grow by more than a quarter plus 4 KiB is escaped instead.
- **Where:** run segments on HTTP, MCP and gRPC (`PromptContentBlock.kind`, field 6, new), the `Agent` tool's spawn / parallel_spawn / open `untrusted` entries, code-js input forwarding, and schedule prompts, which dropped `kind` before. Both clients send it.
- **Fixed on the way:** a caller could label its own text `run_metadata`, which marks what an external trigger sent, on `POST /v1/runs`, continuations, MCP and gRPC. It is now relabelled at every intake.

### The run queue, lanes and queue-aware routing: fixes (#1832, #1834–#1837, #1839–#1842, #1844, #1846)

- **Reloaded run-queue caps apply at once** (#1832):
  - raising `max_concurrent_runs` left queued runs waiting, and lowering it did nothing while runs were queued;
  - turning the per-user cap off stranded the runs queued on it;
  - a reloaded `max_queued_runs_per_user` was never applied.

  Also, a slot handed to a waiter just as it gave up sat idle, and a run that had waited on its user's cap could then be refused "queue full".
- **Lane reservations are freed** (#1834). A slot a lending run gave back, or one held while its run waited at `max_models_in_flight`, stayed reserved until `affinity_hold_ms` ran out. A shrunk lane kept reservations past its new cap, and reloading a provider's caps away never released its waiters.
- **Queue-aware choices survive operator turns and retunes** (#1835). An operator turn moved a run back onto the busy candidate it had skipped, and a move after preemption ignored a retune. A sub-agent now inherits its parent's current run settings, not the ones it started with.
- **llama.cpp slot pinning holds** (#1836). LLM gateway requests are sent with their held slot. A run's side calls (a tool's rerank, a summary) no longer use the run's reserved slot, so they don't overwrite its prompt cache.
- **Streaming `spawn_run` carries `admission` and `interruption`** (#1837); before, a spawn with `admission: "reject"` queued instead of being refused. A2A reports a queued or configured run as `SUBMITTED` (it answered "task not found"). Retune refusals have the same codes on gRPC as on HTTP, and `/retune` answers 409 for a no-trace run, as `/input` does.
- **An interactive run whose turn is cancelled while its model call waits in a lane parks for steering** (#1840), instead of ending with "context canceled". It takes its lent run slot back before computing again.
- **Lane wait counts as queue time, not service time** (#1841). On providers whose driver reports no phase timings (llama.cpp, OpenAI-compatible, cloud), a busy lane made the model look slower, and queue-aware routing moved runs off it more than it should. The ledger's `queue_ms` now shows lane waits for every provider.
- **Cancels reach queued runs on another replica** (#1842), and a dead replica's queued runs are failed by the sweeper instead of waiting forever. A restored snapshot keeps each run's replica.
- **Queue-aware starts honour vision and the queue wait** (#1844). A run carrying an image, or continuing a session that had one, is never moved to a text-only provider. A run that waited in the run queue chooses its provider again when it starts. `switch_after_ms` is capped at 24 h.
- **The cluster session lock is let go while a turn is queued** (#1846). Each queued continuation held a Postgres connection for its whole wait, so enough of them drained the pool and the running runs could never finish. If another replica takes the session meanwhile, the queued turn ends failed.
- **Lent runs are visible** (#1839). Lent runs count against `max_queue_depth` but showed only in `/v1/_concurrency/stats`, so `/metrics` and the Lanes tab read "queued 0" while runs were refused "queue full". There is now a `loomcycle_concurrency_lent` gauge, and `lent` appears in the Lanes view.
- **Config:** the example config gains a `providers:` lanes block. `LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER` and `LOOMCYCLE_PROVIDER_QUEUE_DEPTH` are in both env templates and the TrueNAS install wizard.

### No-trace runs: what was still stored (#1848, #1854, #1853)

- **A no-trace child's answer was stored three times in an ordinary parent's events:** in the tool result, in the spawn ledger and in the poll-mode result. The parent's model and stream still get it; storage keeps a marker. A parent's record of a child it cannot show to be ordinary (a row that cannot be found) is reduced the same way.
- **A queued no-trace run stored its run config and its request.** Its record is now only its mode.
- **A no-trace agent is refused as a team member** (403), because a walk records its members' answers.
- **Only an operator can take an agent's no-trace mode off, or change its hooks** (`hooks` / `tool_hooks` on create, fork and promote), and only an operator can change a HookDef that a no-trace agent runs unpinned. Each is audited.
- **Telemetry spans no longer carry a no-trace run's error text** (tool, iteration and MCP spans).
- `GET /v1/_retention` lists active no-trace overrides only to an operator, and deleting a sub-tree names `ancestor_transcripts` in `not_reached`.
- **A minted user token inherits the minter's restrictions.**
- **The Web UI shows a no-trace run** with "Run trace not saved" instead of blanking the runs page.

### Restricted tokens and A2A (#1847, #1851, #1855)

- **A token limited to some agents can no longer control other agents' runs.** Before, it could steer, review, retune, compact, cancel and answer interrupts on any run in its tenant, over HTTP, gRPC and MCP. A run is judged by the agent at the root of its spawn tree, so the allowed agent's own sub-agents stay reachable.
- **Every steer counts as a run start** against the caller's rate.
- **An A2A token reaches only its own tenant.** A tenant's token sent to another tenant's host, or with another `tenant` in the body, started runs there and could read and cancel its tasks.
- **An A2A start takes from the caller's run rate**, the token's own or the default `LOOMCYCLE_RUN_RATE_PER_MINUTE`. Answering a parked run's question is not a start. A2A refuses a token whose retention restriction it cannot apply, and requires `runs:create`.
- **A locked caller's `allowed_hosts` can only narrow the operator list.** With `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1`, it replaced it.
- **A token minted through `OperatorTokenDef` is held to the minter's restrictions,** and `rotate` refuses a token less restricted than the caller.
- `/v1/embeddings` counts against the token budget, and the expired-token purge keeps a tenant's newest row.

### Whose credential a trigger's run uses: the rest (#1849, #1858)

A trigger's run withholds a user's own `$cred:` credentials unless its author could act as that user (v1.114.0). Paths that dropped that:
- a team walk started by a webhook or schedule, and its members;
- a resumed or restarted run (a new `runs.credentials_no_user_scope` column, carried in snapshots), and the compaction, recap and hook calls made for it;
- a git-source clone;
- a continuation of a trigger's session;
- team subscription walks and channel hook runs, which run as user `_system`; a credential stored under `_system` never resolves.

Also: a run without the user scope cannot grant it to a schedule it writes, a webhook payload cannot name a stored credential in `user_credentials` (`400 bad_mapping`), and a no-trace caller cannot write a schedule or webhook.

### Forge webhooks (#1850, #1852)

- **A claimed event whose run never started is no longer held for the whole dedup lifetime.** Each claim is fenced: after 15 minutes a claim with no linked run is adopted (a run carries its keys) or taken over, and a taken-over claim's run is refused before its first model call. An event's run never runs twice. A run that failed before registering no longer blocks the retry, with or without a `dedup_key`.
- **A fork of a static webhook bootstraps the shared row only**, never one in the caller's tenant.
- **A duplicate a live run holds is answered by a read**, not a write; the `dedup_key` decode is bounded; a huge `dedup_ttl_seconds` no longer overflows into a disabled key.
- **Authoring checks** refuse a shape rule that always holds (`!$.x == true`), a path key with whitespace, `=` or a quote, and a compared number past 2^53−1, on create and fork and at yaml load.
- **A stored verification secret is cached 5 s** per tenant and name, instead of a database read and decrypt on every unauthenticated post.
- `"user_bearer": ""` in a fork overlay clears the bearer.

### Untrusted text stays fenced after the loop (#1838, #1856)

- **The `redact` plugin could delete a fence's closing tag:** a line ending `password=` took the next line as its value. Its rules no longer cross a line onto a tag.
- **History and the trace index filed untrusted blocks as the user's own words.** A webhook body saying "I'm the admin" came back as something the user said, to later runs, the memory extractor and recall. Each block is now its own turn, under its kind and fenced.
- **The memory consolidator** no longer splits a conversation at a `### ` heading inside a fence, and keeps a cut message's heading and opening tag.
- **The retention sweeper's evidence quote** keeps untrusted blocks fenced.

### Git sources, volumes and Read (#1843, #1845, #1859)

- **Git sources:** a diff bounded in lines (a change over 5,000 differing lines is listed `too_large` with no body), pack processing bounded in total and stopped by its context, one create per name at a time, links checked on disk on case-insensitive file systems, no token in errors or logs, IPv6 and `:443` URLs matched correctly, and a GitHub App credential named as `$cred:` refused (use `$ghapp:`).
- **A static volume at or above the dynamic root can no longer reach it**, so a read-only ephemeral volume is read-only through every binding (Read, Write, Edit, Glob, Grep and Bashbox). Bash and Bashbox's host-command fallback are not held to read-only, and the docs now say so.
- **`Read` by line range:** a huge `limit` no longer overflows, the way to `offset` is bounded (`MaxSeekBytes`, 1 GiB), and a long line honours cancellation.
- **A root that is a symlink is walked** by Glob, Grep, the skills loader and SQL Memory. SQL Memory skipped every scope under such a root, so a snapshot or an erasure silently missed it.

### MCP: a server that starts after loomcycle (#1857)

A YAML-declared MCP server that was down at boot is re-probed by the next run that names its tools. If its headers name the per-run bearer (`Bearer ${run.user_bearer}`) and the run's tenant is not `""`, that re-probe was refused before it was sent, so the server's tools were never advertised until a restart. The re-probe now goes without the bearer, as the boot handshake does; a tool call still needs it. The log line is `dropping header (run-start discovery handshake)`.

### Upgrade notes

- Two Postgres migrations: 0106 (`runs.credentials_no_user_scope`) and 0107 (the webhook dedup claim fence). SQLite adds the columns itself.
- **Behaviour changes:**
  - A no-trace agent cannot be a team member (403).
  - Only an operator can remove an agent's no-trace mode, change a no-trace agent's hooks, or change a HookDef a no-trace agent runs unpinned.
  - Every steer counts against a rate-limited caller's run rate.
  - A2A now applies the default per-user run rate.
  - A locked caller's `allowed_hosts` only narrows the operator list.
  - A token minted through `OperatorTokenDef` inherits the minter's restrictions, and a restricted caller cannot rotate a looser token.
  - Continuing a trigger's session withholds the session user's own credentials.
- **Webhooks:** the yaml boot refuses a mistyped shape rule or an oversized compared number. A rotated or deleted verification secret can still verify for up to 5 s per replica.
- **Git sources:** a credential saved with an IPv6 `allowed_urls` entry stops matching until it is saved again. A diff over 5,000 changed lines of one file has no body.
- **History:** a History page can hold several turns at one seq (a user's words, then each untrusted block). Trace rows written before this release keep their old form until their TTL.
- **Replay:** a block stored with `kind: "code"` before this release replays as a code block, so its session's first replayed turn misses the prompt cache once.

## What's in v1.114.0

*Every way a run starts now **waits in the run queue**, and so do a session's next message and the LLM gateway. While a run's model call waits for a busy local provider, the run **lends its run slot** to a run bound elsewhere. An agent, or one run, can **skip a busy provider** for an idle candidate of its tier, at its start and once more if it loses its place. A lane can cap **one model** inside it, for Ollama models that compute one request at a time. Forge **webhooks** can filter on the signed body, start **one run per event**, carry a stored **bearer**, tell the run which event it handles, and count their deliveries. A stored credential that cannot be opened **fails closed**.*

A minor release: #1805 to #1830.
- #1809, #1811, #1813 finish moving runs and direct requests onto the run queue;
- #1805 retries a scheduled fire the full queue refused;
- #1808, #1810, #1817, #1820 are call lanes: an exempt kind, a tool's waits reported, a Web UI tab, llama.cpp slot pinning;
- #1818, #1823, #1830 are queue-aware routing, per-model caps and run-slot lending;
- #1806, #1814, #1819, #1822, #1826, #1828, #1829 are forge webhooks;
- #1824 and #1807 change whose credential a run uses, and what happens when one cannot be opened;
- #1815 and #1816 fix the cloud deployment script;
- #1812 and #1821 change tests only.
- `@loomcycle/client` and the Python adapter are 1.114.0.

### Every run and direct request waits in the run queue (#1809, #1811, #1813, #1805)

v1.113.0 queued new runs, but a session's next message (`POST /v1/sessions/{id}/messages`) and the LLM gateway still waited `queue_timeout_ms` (30 s) and were then refused with 429.

- **A session continuation queues** like `POST /v1/runs`: its row is `queued` in the session, it is cancellable while it waits, and its stream sends `session`, `agent` and `queued` before it waits. It keeps the session's lock while queued, so another message to the session is `409 session_busy`. A queued continuation survives a restart and continues the same session.
- **The LLM gateway and `/v1/embeddings` wait for their run slot in the run queue**, round-robin with the runs and with no timeout. They are bounded as before by `max_queue_depth` and `max_queued_runs_per_user`, and they still never wait for a provider while holding a global slot.
- **`concurrency.queue_timeout_ms` is removed.** Nothing reads it any more. A config that still sets it loads unchanged.
- **A scheduled fire the full queue refuses is fired again on the next tick**, under the same run key, and does not count against `max_fires`. Before, the slot was lost.

### A run waiting for a provider lends its run slot (#1830)

A run whose model call waited for a saturated local provider held its global run slot (`max_concurrent_runs`) the whole time. A few such runs could keep a run bound for an idle cloud provider from starting at all.

- **While a run's own turn call waits for its lane, its slot goes to the next run waiting to start.** Before the call computes, the run takes a slot back, ahead of every new run.
- **It never waits for a global slot while holding a lane slot.** If no global slot is free when the lane grants the call, the run gives the lane slot back, waits for a global slot holding nothing, then queues for the lane again. Calls that do not lend (a tool's decision or embedding, a compaction summary, a sub-agent's call) wait holding the run's slot, and the two would otherwise wait on each other.
- **Lent runs count against `max_queue_depth`** with the queued ones, so runs alive stay within `max_concurrent_runs + max_queue_depth`. At that bound a run waits holding its slot, as before.
- **The cost.** A run taking its slot back waits for one to free. If every slot is held by runs that are not computing (a parked interactive run, a long tool call), that lasts until one ends.
- `GET /v1/_concurrency/stats` reports `lent`.

### Skip a busy provider: queue-aware routing (#1818, #1823, #1830)

The tier cascade took the first available candidate, however long its queue. A run could wait minutes for a capped local model while a cloud candidate of the same tier sat idle.

```yaml
agents:
  triage:
    tier: middle
    routing: { queue_aware: true, switch_after_ms: 20000 }   # default 20000
```

- **At a run's start** (a new run, a session continuation, a sub-agent), a candidate whose estimated wait is over `switch_after_ms` is passed over for the next available one. If all are over, the shortest wait wins. The estimate reads the chat lane and the model gate, times the model's median call time; a model with no measured call counts as no wait. A pass-over is logged.
- **Once more, after it loses its place.** At a call boundary after its first turn, a run that no longer holds a slot of its lane (preempted by `quantum_calls`, or its `affinity_hold_ms` expired) and whose next call would wait past `switch_after_ms` is re-resolved, and moves if that picks another candidate. It moves like a fallback: an `EventProviderFallback` with reason `queue_wait`. A run still holding its slot never moves. No move under `pin_after_success` after a successful turn, nor to a text-only provider for a run that carries an image.
- **Per run.** `routing` on `POST /v1/runs`, session messages, batch children and configured-run drafts, the MCP `spawn_run` / `spawn_runs` tools, gRPC `RunRequest.routing` / `ContinueRequest.routing` (a new `RunRouting` message), and both clients. It replaces the agent's block whole (`{}` turns it off), chooses only among the definition's candidates, and is lockable like any override.
- **Inherited and retunable.** A sub-agent of the same definition inherits it. It can be changed mid-run: `routing` on `POST /v1/runs/{id}/retune` and in `overrides` on `/input`, gRPC `RetuneRunRequest.routing` / `RunInputRequest.routing`, the MCP `retune_run` tool, both clients. A retuned block governs the move from the next call boundary.
- **Refused:** `queue_aware` on an agent or run pinned to a provider or model, and a negative `switch_after_ms`. Webhooks, A2A and schedules cannot set it.
- **Fixed on the way:** a sub-agent resolved its provider and model before inheriting its parent's per-run overrides, so an inherited model, tier or effort was recorded on the child but not run on. It is now resolved after.

### A lane can cap one model (#1823, #1808)

On Ollama 0.40.3 some models compute one request at a time whatever `OLLAMA_NUM_PARALLEL` says (measured: `qwen3.6`, `ornith-1.5`, `qwen3.8`, `nimble`, `clef`), while others (`gpt-oss`, `gemma4`) run four. A lane of 4 sent four calls to a one-slot model, three of them waiting inside Ollama where loomcycle could not see them.

```yaml
providers:
  ollama-local:
    max_models_in_flight: 1
    models_in_flight_exempt_kinds: [embedder]   # embeddings do not wait for the chat model
    lanes:
      chat:
        max_concurrent: 4
        model_caps: { qwen3.6:latest: 1, ornith-1.5:35b: 1 }
      embedder: { max_concurrent: 2 }
```

- **`lanes.<kind>.model_caps`** holds a model to `n` calls in flight inside the lane, keyed by model name or a `models:` alias of the provider. A name covers the model at every context size. Calls to other models take the free slots. A freed slot goes to the first waiter in turn whose model is under its cap.
- **`models_in_flight_exempt_kinds`** leaves a kind out of `max_models_in_flight`: its calls neither count as a model in flight nor wait for one, and its own lane still caps them. The lab host runs chat beside an embedding model without trouble, but not beside a second LLM.
- **Watching it.** Lane stats and Prometheus gain `blocked_on_model` (`loomcycle_provider_lane_queue_depth_blocked_on_model`). The queue-aware estimate leaves calls held back by their own model's cap out of the queue ahead of other models.

### Call lanes: the rest (#1810, #1817, #1820)

- **A tool's own model calls report their wait.** A `Decision` call, or a Memory or Document search that embeds its query, now puts `call_queued` / `call_admitted` (`kind` `decision` / `embedder`) on the run's stream, as the run's own calls do. Background work reports nothing.
- **Settings → Lanes** in the Web UI (admin only): the run queue, and each capped provider's lanes and models in flight, refreshed every 3 s.
- **llama.cpp slot pinning.** With `options.pin_slots: true`, the `llamacpp` driver sends each call with `id_slot` set to its lane slot and `cache_prompt: true`, so a run's lease (`affinity_hold_ms`) lands on the server slot that holds its prompt cache. Set the lane's `max_concurrent` to `llama-server --parallel`. Not yet run against a real llama-server.

See "Per-provider concurrency" in [`docs/PROVIDERS.md`](docs/PROVIDERS.md) and "Per-agent `routing:`" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Forge webhooks: filters, one run per event, a bearer, what the run receives (#1806, #1814, #1819, #1822, #1826, #1828, #1829)

- **Filters on the signed body.** `filters` are conditions every delivery must meet before anything starts: `eq`, `ne`, `glob`, `in` on a JSON path, against a literal or the definition's own `metadata`. A miss is `202 {"outcome":"filtered","filter":<index>}`, before any replay key is claimed. Comparison is typed; a missing or `null` path is a miss for every op.

  ```yaml
  filters:
    - { path: "$.ref",                  op: glob, value: "refs/heads/*" }
    - { path: "$.deleted",              op: eq,   value: false }
    - { path: "$.repository.full_name", op: eq,   value_from: metadata.repo }
  ```
- **One run per event.** `dedup_key` names the event by signed fields (e.g. `["$.repository.id", "$.ref", "$.after"]`). A redelivery with a new id or different bytes is `200 deduped` with the first run's id. The key is held in the database for `dedup_ttl_seconds` (default 7 days, at most 90), on every replica and across restarts. An event whose run **failed** is handled again. A body without one of the paths is `400 dedup_key_unresolvable`. Migration 0104.
- **Event shapes can say what must be absent or equal.** A shape rule is now `$.path`, `!$.path` (missing or `null`) or `$.path == <value>`. The presets use them to tell apart events whose bodies overlap: GitHub `issues` / `issue_comment` and `create` / `delete`, Gitea `issue_comment` / `pull_request_comment` (a new preset), GitLab `push` / `tag_push` and `issue` / `merge_request`. A Gitea `issue_comment` and a GitLab `issue` preset accepted the other event's body before.
- **A bearer for the run.** `user_bearer` on webhooks and schedules, as a stored credential reference only (`"$cred:<name>"`); a literal is refused when written. It reaches MCP servers through `${run.user_bearer}`, is resolved per request for that server's URL, and only at tenant level of the run's tenant.
- **The run knows its delivery.** A run started by a webhook that lists `events` gets `metadata.webhook = {provider, event, delivery_id}`. `delivery_id` is unsigned and passes only as a UUID, a hex digest or a number. A definition that lists events cannot set `metadata.webhook` itself (create and fork refuse it; a yaml one gets a boot warning).
- **Delivery counters.** Each delivery is counted under how it ended (accepted, duplicate, ping, ignored, filtered, refused by reason), in memory and written every 30 s, never per delivery. `WebhookDef op=get` shows them under `status.deliveries`, and `GET /v1/_webhooks/{name}/delivery-counts` is a new admin route. Migration 0105.
- **The console** (Integrations → Webhooks) reads and writes all of it, including stored-credential secrets and team webhooks, which it could not fork before, and shows the counts.
- **A new guide:** [`docs/FORGE_WEBHOOKS.md`](docs/FORGE_WEBHOOKS.md), GitHub, Gitea and GitLab end to end.

### Whose credential a run uses (#1824, #1822, #1807)

- **A trigger's run uses a user's own credentials only when its author could act as that user.** A webhook-started run never resolves `$cred:` in the run user's personal scope; its `user_id` can come from the delivery. A schedule does only when an operator wrote it, or the schedule's own user. This covers MCP header references, `user_credentials` values, A2A peer credentials, and a tenant's or user's own provider key and Bash host credential. **Existing runtime-written schedules fail closed** until an operator or their user saves them again; each fire of one that holds a `$cred:` logs how to restore it.
- **A2A peers resolve `$cred:`.** A peer credential that is a stored reference was sent as written. It is now resolved for the peer's endpoint, and a request pinned to it follows no redirect. A peer found through its agent card gets only an unrestricted credential.
- **A static MCP server resolves `$cred:` in its headers.** Its client was built before the resolver existed and sent the reference literally.
- **A stored credential that cannot be opened fails closed.** A wrong, rotated or missing `LOOMCYCLE_SECRET_KEY`, a corrupt seal or an unreadable backend used to count as no credential, so a tenant's own provider key fell back to the operator's. The call now fails, the operator key is never used, and a Bash variable is unset with the host's same-named variable removed. No stored row still falls back as before, and a store read fault stays fail-soft.

### Deployment (#1815, #1816)

The cloud deployment script stops syncing `searxng/` (the container owns it), keeps synced files readable by the containers, restores the previous files when a sync fails, prunes backups to 10, and, when a failed start follows a migration, restores the runtime's database from the pre-deploy dump before starting the old version again. The database the failed version left is kept as `loomcycle_failed_<stamp>`.

### Upgrade notes

- `concurrency.queue_timeout_ms` is ignored; remove it at leisure.
- Two migrations: 0104 (`webhook_deliveries.run_id`) and 0105 (`webhook_delivery_counts`).
- A runtime-written schedule whose `user_credentials` reaches a user's own `$cred:` stops using it until it is saved again by an operator or its user.
- A stored credential the server cannot decrypt now fails the call instead of falling back to the operator's key. Check `LOOMCYCLE_SECRET_KEY` before upgrading a deployment that rotated it.
- A Gitea `issue_comment` or GitLab `issue` webhook now refuses the other event's body (`400 event_shape_mismatch`).

## What's in v1.113.0

*A run that cannot start at once is now **queued**, not refused. It gets its row, its id and its events before it waits, it can be cancelled, and it comes back after a restart. A provider's `max_concurrent` now counts **model calls**, in a lane per kind of model, so a run holds no provider slot while it runs a tool or waits for a child. An agent can be given **a repository the runtime fetched itself**, so the forge token never enters a shell, and can **read it by line range** with the volume read-only. Forge **webhooks** can say which events start a run, hook deliveries can be **signed**, stored credentials **expire** and can be **limited to certain URLs**, and a **Microsoft Teams** bot can front an interactive agent.*

A minor release: #1745 to #1803.
- #1750, #1773, #1776, #1779, #1787 queue runs instead of refusing them;
- #1758, #1760, #1762, #1767, #1769, #1788 make a provider's cap count model calls, by kind;
- #1759, #1761, #1763, #1764, #1765, #1766, #1775 create an ephemeral volume from a git source;
- #1748, #1756, #1770, #1772, #1800 change how a stored credential is used;
- #1749, #1751 to #1754 are for reading code under review: `Read` by line range and read-only scratch volumes;
- #1781, #1785, #1801 are forge webhooks;
- #1798 signs hook deliveries;
- #1790 to #1796 fill gaps the n8n nodes found: Evaluation over HTTP and gRPC, run listing filters, a JSON 403, an overridable report, clearer refusals, change-feed errors, and MCP definitions that are refused by default;
- #1777, #1778, #1780, #1782 to #1784, #1789, #1802 are `@loomcycle/client` fixes and new methods, and #1803 sets it to 1.113.0;
- #1771 is the Teams bridge, a separate package;
- #1757 and #1799 are fixes; #1768 ships `LICENSE` and `NOTICE` in archives and images;
- #1745 and #1746 are `@loomcycle/def-fields` 0.5.0 and `@loomcycle/library` 0.8.0, published by their own tags;
- #1747, #1755 and #1774 are benchmark records, and #1786 and #1797 change tests only. None changes the runtime.

### A run that cannot start at once is queued (#1750, #1773, #1776, #1779, #1787)

A new run waited up to `queue_timeout_ms` (30 s) for a slot and was then refused with 429. A run over its user's cap was refused at once. A burst lost work, and while a run waited its caller had no row, no events and nothing to cancel.

A new run now never times out in the queue. When it must wait it gets its row with the new status **`queued`**, its cancel registration and its caller's announcement first, and then waits.

| Event | Payload (`admission`) | When |
|---|---|---|
| `queued` | `reason` (`user_cap` or `global_cap`), `position` | it starts to wait; a run can wait on its user's cap, then for a slot |
| `admitted` | `wait_ms` | it has its slot; the row moves to `running` |

- **Where it applies.** `POST /v1/runs`, gRPC, MCP, batches, configured-run starts, schedules, webhooks and A2A. On `POST /v1/runs` the SSE stream sends its session and agent frames before it waits.
- **Reading one.** A single-run read reports `queue_position` and `queue_reason` (HTTP, gRPC `Agent`, MCP `get_run`, both clients).
- **Cancelling one.** A queued run is cancellable; its row ends `cancelled`.
- **The old answer on request.** `admission: "reject"` refuses instead of waiting (HTTP, gRPC `RunRequest`, MCP `spawn_run` / `spawn_runs`, both clients). An unknown value is 400.
- **Bounds.** `max_queue_depth` (now 32, was 16) bounds all waiting runs together. `max_queued_runs_per_user` (16, `LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER`) bounds one user's runs waiting on its cap. With no per-user cap it bounds the places one user holds in the queue, so one caller cannot fill it. A run that names no user shares its tenant's places. Past a bound: `429 backpressure`.
- **Turns.** Waiting runs are served round-robin across (tenant, user), first in first out within one. A resumed run is served first. A schedule, webhook or A2A run now counts under its own tenant, where they all shared one count.
- **Budgets.** A token budget is checked before a run may queue and again when it starts.
- **Triggers.** A scheduled fire is counted when it queues, where it was recorded `skipped`. An async webhook answers 202 with the queued run's id, where it answered 503. A fire refused because the queue is full is still `skipped`.
- **Batches.** A detached batch child that queues is reported `queued` and outlives its caller.
- **Not yet queued.** `POST /v1/sessions/{id}/messages` and the LLM gateway keep `queue_timeout_ms`.

**A queued run survives a restart.** Its request is stored on the row (`runs.queued_input`). A run's `user_bearer` and `user_credentials` are sealed with `LOOMCYCLE_SECRET_KEY`, and the seal is bound to that run. At boot, each replica starts its own queued runs again, oldest first, under their own rows and idempotency keys. A caller re-attaches by run id. A run that cannot come back is failed with stop reason `restarted` and the reason, and its idempotency key is released:
- a run with per-run secrets when no `LOOMCYCLE_SECRET_KEY` is set;
- a no-trace run, whose prompt is stored nowhere;
- sealed secrets that the key at boot cannot open;
- a row an older build queued.

A restarted run has no principal on its context, as a resumed paused run has not. Work its first caller did in memory after the run (a webhook's `on_complete`, A2A task state) does not happen. The scheduler's `on_complete` does.

**A pause holds the queue.** A run whose slot frees during a pause keeps the slot, stays `queued` and starts on the resume.

**A snapshot carries queued runs** in a new `queued_runs` section: identity, keys, events and the stored request, less its sealed secrets. Restore queues exactly the runs it inserted. A run restored without its secrets runs without them, and the restore warns. The restore counts are `queued_runs` and `queued_runs_requeued`.

**Admitted work never times out on a provider.** A sub-agent, a resumed run and a mid-run fallback waiting for a provider slot used to be refused or failed after 30 s; a fallback ran uncapped after 2 s. They now wait. Time a sub-agent spends queued does not count against its `timeout_ms`. `provider_queue_timeout_ms` and `LOOMCYCLE_PROVIDER_QUEUE_TIMEOUT_MS` are removed.

See `loomcycle help fairness`.

### A provider's cap counts model calls, by kind (#1758, #1760, #1762, #1767, #1769, #1788)

A provider's `max_concurrent` counted whole runs. A run took a slot when it was admitted and kept it through tool calls, waits for sub-agents and waits for its operator. Measured with 8 agents on 4 slots, four agents finished while the other four waited for whole conversations: first token at p95 took 257 s, against about 120 s when slots are taken per call. The cap also did not hold: a child of a parent holding the same provider ran ungated, and interactive runs were never counted.

`max_concurrent` now counts model **calls**. A run takes a slot just before each call and gives it back when the call ends. Each kind of model call has its own queue, a **lane**:

```yaml
providers:
  ollama-local:
    max_concurrent: 4               # shorthand for lanes.chat.max_concurrent
    max_models_in_flight: 1         # different models computing at once; 0 = unlimited
    model_switch_max_wait_ms: 30000 # how long a waiting model can be held off
    lanes:
      chat:     { max_concurrent: 4, quantum_calls: 4, affinity_hold_ms: 5000 }
      decision: { max_concurrent: 2 }
      embedder: { max_concurrent: 2, background_share: 0.25 }
```

- **Fair turns.** Live runs go before background work, round-robin across (tenant, user). `background_share` guarantees background work (memory consolidation, reindex and describe sweeps) a share of turns.
- **Keeping a slot warm.** `affinity_hold_ms` keeps a slot for the run that just used it, so its next call lands on a warm prompt cache; `quantum_calls` bounds its turns while others wait. The default is 0, a plain fair queue: on Ollama, measurement showed that keeping a slot saves nothing.
- **Models in flight.** `max_models_in_flight` caps the different models computing at once on a provider. It counts every call to the provider, of any kind. On Ollama a model's identity is the model and its context size (`qwen3.6:latest@65536`): switching context size reloads the model. The driver warns once when an agent's context size differs from the provider's.
- **Every call is counted.** Runs, compaction, the LLM gateway, the OpenAI-compatible routes, the image-describe route, decision models, embedders, the memory reranker and the consolidation unit generator. Decisions and reranks with no lane configured are held to 4 in flight per provider, as before.
- **A run sees its calls wait.** A model call that has to wait puts `call_queued` on the run's stream and transcript (`admission.reason` `lane` or `models_in_flight`, `position`, `provider`, `kind`), then `call_admitted` with `wait_ms`. A call that gets its slot at once reports nothing. A decision or embedding a tool makes does not report yet.
- **Requests outside a run stay bounded.** The LLM gateway, `/v1/embeddings`, `/v1/_decide` and the describe route wait at most `provider_queue_depth` deep (default: `max_queue_depth`), counted against their user. Past it: `429 provider_concurrency_exhausted`. This answer now goes only to those requests, never to a run.
- **Watching it.** `GET /v1/_concurrency/stats` lists every provider that caps anything, with each lane (`max_concurrent`, `active`, `reserved`, `queued`, `queued_background`, `grants`, `preemptions`), `models_in_flight` and `model_waiters`. `active` / `queued` stay the chat lane. Prometheus gains `loomcycle_provider_lane_*` series labelled `provider` and `kind`, plus `loomcycle_provider_models_in_flight` and `loomcycle_provider_model_waiters`.

See "Per-provider concurrency (`max_concurrent`, `lanes`)" in [`docs/PROVIDERS.md`](docs/PROVIDERS.md).

### A repository in an ephemeral volume, fetched by the runtime (#1759, #1761, #1763, #1764, #1765, #1766, #1775)

An agent that needed a repository ran git through a shell tool, with the forge token in that process's environment. Code under review is text an outsider wrote, and it can ask the agent to print the token.

`VolumeDef op=create` on an ephemeral volume now takes a git source. The runtime fetches the commit and writes its tree into the volume; the token is a value in loomcycle's memory, sent to one host.

```json
{"op": "create", "name": "repo", "ephemeral": true, "mode": "ro",
 "source": {"git": {"url": "https://github.com/acme/app", "commit": "<40-digit sha>",
                    "ref": "refs/heads/feature-x", "credential": "$cred:repo-acme-app",
                    "diff_base": "merge-base:default"}}}
```

- **Off by default.** `LOOMCYCLE_GIT_SOURCE_HOSTS` lists the hosts a repository may come from. Unset, a request is refused with `git_source_disabled`.
- **Who may ask.** The agent needs `source:git` in `volume_def_scopes`, besides the grant for the name. `any` does not include it.
- **The credential.** `$cred:<name>` or `$ghapp:<name>`, and it must carry `allowed_urls` covering the repository. One without is refused: the URL can come from untrusted text. A public repository needs none.
- **A GitHub App token is narrowed.** For `$ghapp:` the runtime asks for a token for the one repository, with `contents: read` and nothing else. A config that lists its own repositories or permissions must already allow it; it is never widened.
- **What lands.** Regular files, mode `0644`. Links only when they stay inside the tree. A submodule is an empty directory. No `.git`. `_meta/omitted.json` lists what was left out and why.
- **A diff.** With `diff_base` (`merge-base:default` or a sha), the volume also holds `_meta/diff.patch` and `_meta/numstat.json`. The patch applies with `git apply`; it is not byte for byte what `git diff` prints.
- **All or nothing.** A failed clone leaves no directory, store or row, and the name is free. A failure is one word from a closed list (`auth_failed`, `not_found`, `commit_not_found`, `merge_base_not_found`, `source_too_large`, `timeout`, `fetch_failed`, …); the remote's own text goes to the operator's log.
- **No git process, no redirect.** HTTPS only, through the same network guard as the HTTP tools. No redirect is followed, even to the same host. Nothing from the repository runs.
- **Redaction.** The redactor now also masks `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_` and GitLab `glpat-` tokens.
- **Another replica.** A run of the tree on another replica finds the volume by name. If the files are on another replica's disk, the call says so.

| Setting | Default |
|---|---|
| `LOOMCYCLE_GIT_SOURCE_HOSTS` | unset: off |
| `LOOMCYCLE_GIT_SOURCE_MAX_BYTES` | 256 MiB, fetched and written; a request may lower it |
| `LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES` | 64 MiB; this is what bounds memory |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES` | 5 MiB; a larger file is left out and listed |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILES` | 100000 |
| `LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS` | 120, waiting for a slot included |
| `LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT` | 1 clone at once, per replica |
| `LOOMCYCLE_GIT_SOURCE_MAX_HISTORY` | 500 commits |

A setting that is not a positive number stops the server at boot. A test runs a code agent through the server against two private repositories and finds neither token in the stream, the database, the log, the volumes or a `Bash` environment. The binary grows by about 4 MiB (the go-git library).

See "9d.2.1 A volume from a git source" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Where a stored credential may go (#1748, #1756, #1770, #1772, #1800)

- **`allowed_urls`.** `CredentialDef op=create` takes a list of https URLs. The value is sent only to a URL under one entry, matched by host and whole path segments. `[]` means it is sent nowhere. Left out, nothing changes. The list is shown by `get` and `list` and is bound to the sealed value, so an edited list no longer opens. A rotation that leaves it out keeps it.
- **Where a restricted credential works.** An MCP server's `$cred:` / `$ghapp:` header, a hook's webhook header, a remote memory backend's or document source's key, and a git source, each to a covered URL. A request carrying one follows no redirect. A provider key and the `Bash` / `Bashbox` environment name no destination, so they refuse it.
- **`username`.** An optional user name, stored beside the value and shown by `get` and `list`. A git source sends it as the basic-auth user.
- **`expires_at` is enforced.** It was stored and reported, and an expired credential kept being sent. It is now refused at its expiry, everywhere it resolves. `get` and `list` add `expired`. To rotate, create again with the same name.
- **An unusable key is never replaced.** An expired or URL-restricted credential that holds a provider or tool key fails the call. It does not fall back to the operator's key, to a host variable of the same name, or to another provider. In `Bash` / `Bashbox` the variable is left unset and the host's variable of that name is removed. A credential that is simply missing still falls back, as before.
- **GitHub App token cache.** The cache key now covers the tenant, the server and a digest of the private key. A config that copied another credential's ids, with any key, was served that credential's cached token.

See [`docs/CREDENTIALS.md`](docs/CREDENTIALS.md).

### Reading code under review (#1749, #1751, #1752, #1753, #1754)

**`Read` by line range.** `Read` returned the first 256 KiB of a file and nothing else, and the cut was silent. It now takes `offset` (first line, from 1), `limit` (lines) and `line_numbers`. Any of them turns on line mode: whole lines, and a closing line that says where the read stopped.

```text
[lines 812-818 of 3044]
[lines 1-2210 of 3044; cut at 256 KiB, continue with offset 2211]
```

A call with none of the three returns exactly what it did before. A binary file is refused in line mode. See "`Read` by line range" in [`docs/TOOLS.md`](docs/TOOLS.md).

**Read-only scratch volumes.**
- `VolumeDef op=set_mode {name, mode: "ro"}` makes an ephemeral volume read-only for the whole run tree at once, children already running included. It is one way: `rw` is refused.
- `ephemeral_volumes: ro` on an agent gives it, and every agent it spawns, all of the run's ephemeral volumes read-only. Its own `volumes:` bindings keep their modes. It is set in the config, a markdown agent's frontmatter or an `AgentDef` overlay. A fork cannot clear it, and a run cannot override it.
- The mode holds on resume and on another replica: a write to an ephemeral volume first reads the volume's stored mode. That is one indexed read per write-type tool call.
- `Context op=self` lists `ephemeral_volumes` with the mode the agent gets.

See "9d.2 Ephemeral (run-scoped) volumes" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Forge webhooks (#1781, #1785, #1801)

A forge sends every kind of event to one URL. The receiver started a run for each delivery it accepted, a GitHub `ping` included.

- **`provider`: `github`, `gitea` or `gitlab`.** A preset of the auth kind, the signature, delivery-id and event headers, the ping, and the body shapes of the events it knows. Anything the definition sets wins.
- **`events`** lists the only events that start a run. It is checked after the signature and before a replay key is claimed:

| Delivery | Answer |
|---|---|
| no event header, two of them, or a malformed one | `400 bad_event` |
| GitHub's ping | `200 {"outcome":"ping"}`, no run |
| an event not listed | `202 {"outcome":"ignored"}`, no run |
| a listed event whose body lacks a path of its shape | `400 event_shape_mismatch` |

A definition without `events` is handled as before, a ping included.

- **`event_shapes`** gives a shape to an event the preset has none for. Every listed event needs a shape, or the definition is refused when written. Presets give shapes only to events whose bodies no other event holds, so GitHub `issues`, `create` and `delete` need one the author writes.
- **Secrets from stored credentials.** `auth.secret: "$cred:<name>"` names a tenant credential, read on every delivery, so a new value applies with no restart. `auth.previous_secret` is accepted beside it during a rotation. Store it with `allowed_urls: []`: a comparison sends it nowhere. A literal is refused.
- **`user_tier`** is now kept on a runtime definition; it was dropped.
- **Overlays are read as written.** An unknown, repeated or case-only key is refused, and so is a key the server sets.
- **Bounded bodies.** `body_size_limit_bytes` is capped at `LOOMCYCLE_MAX_REQUEST_BYTES`. A body more than 128 deep or with more than a million values is refused as `bad_mapping`.

See `loomcycle help input-webhooks`.

### Signed hook deliveries (#1798)

An http `HookDef` can name `body.signing_secret: "$cred:<name>"`. Each delivery then carries `X-Loomcycle-Signature: t=<unix>,v1=<hex>`, an HMAC-SHA256 of `t + "." + body`, plus `X-Loomcycle-Hook` and `X-Loomcycle-Delivery`.

- The delivery id is also inside the signed body as `delivery_id`. Receivers should dedupe on that and treat the headers as unsigned copies.
- A secret that cannot be resolved fails the call by its `fail_mode`. A delivery is never sent unsigned.
- The credential must be unrestricted or cover the hook's URL; a non-admin may name only one of its own tenant's.
- Unsigned hooks are byte for byte unchanged.
- Verifiers: TypeScript `verifyLoomcycleSignature` / `verifyLoomcycleDelivery`, Python `verify_loomcycle_signature` / `verify_loomcycle_delivery`.

See "Signed deliveries" in [`docs/TOOLS.md`](docs/TOOLS.md), which also now shows the current hook payload (#1777).

### Evaluation from outside a run (#1793)

Evaluation was reachable only inside a run and as an MCP tool. It is now `POST /v1/_evaluation` and the gRPC `Evaluation` RPC, and one grant rule serves HTTP, gRPC and MCP:

| Token | Ops | Reach |
|---|---|---|
| admin, legacy, open mode | all | every tenant |
| `substrate:tenant` | all | its own tenant |
| isolated `substrate:user`, or a member | `submit_self` | its own runs |
| no `runs:read`, or limited to the run endpoints | refused | |

A tenant operator on MCP now gets every op, where it had only `submit_self`. Outside a run, `submit_self` is refused today, because "self" means the run's own agent. Both clients have `evaluation()` and five helpers. See [`docs/TOOLS.md`](docs/TOOLS.md).

### Run listing, overrides and refusals (#1790, #1791, #1792, #1794, #1795, #1796)

- **Listing a user's runs.** `GET /v1/users/{user_id}/agents` takes several statuses (`?status=completed,failed`) and `?awaited_state=review,input`, which lists running runs blocked on those waits. The awaited scan stops at 1,000 runs and reports `truncated`. Also gRPC `ListUserAgents` (`statuses`, `awaited_states`, `truncated`) and both clients. MCP `list_runs` still takes one status.
- **Which overrides an agent permits.** `GET /v1/_agentdef/{name}/overridable` reports, for the calling token, each of 27 per-run settings: whether it may be set, the reason if not (`token_locked` or `agent_locked`), and the allowed values where the run checks a closed set. It uses the same checks as a run. TypeScript: `getAgentOverridable`. See "Asking which settings a run may set" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).
- **A missing scope is a JSON 403:** `{"code":"insufficient_scope","error":"insufficient scope","required_scope":"<scope>"}`. The text and the `WWW-Authenticate` header are unchanged. TypeScript: `InsufficientScopeError` with `requiredScope`.
- **Configured-run refusals name their reason** on gRPC (an `ErrorInfo` reason, and `RetryInfo` on `rate_limited`) and MCP. A 403 on gRPC is now `PermissionDenied`; it was `Internal`.
- **Change feeds report a broken store.** `GET /v1/_memory/changes` and `/v1/_document/changes` send `event: error` (`store_unavailable`) once per failure streak, back off, and skip no change on recovery. They used to look like a quiet feed.
- **MCP server definitions with an unattributed `${NAME}` are refused by default.** A runtime version that reads the server's environment but was not saved by an admin used to warn and dial. It now is not dialed. See the upgrade notes.

### `@loomcycle/client` (#1777, #1778, #1780, #1782, #1783, #1784, #1789, #1802)

- **Exports.** Every public type is exported from the package root; 23 were not. A test now fails on a type left out.
- **Errors.** `TokenLimitExceededError` is no longer a `BackpressureError`. Every error has `retryable`. A new `RateLimitedError` (a `BackpressureError`) carries `retryAfterMs` from `Retry-After`. See the upgrade notes.
- **Typed overlays.** `AgentDefOverlay` types all its fields, with wire-shaped (snake_case) nested blocks. `createHookDef` / `forkHookDef`, `createScheduleDef` / `forkScheduleDef` and `createWebhookDef` / `forkWebhookDef` check their overlays at compile time. `ensureHookDef(name, overlay)` registers a hook only when it changed.
- **New methods.** Schedules (`listAllSchedules`, `getScheduleState`, `runScheduleNow`, `pauseSchedule`, `resumeSchedule`), `listModels`, `getRouting`, eleven `list<X>DefNames`, `getCredential`, and the change feeds `streamMemoryChanges` / `streamDocumentChanges`, which yield `feed`, `change` and `error` items.
- **Fixes.** `runTeam` accepts a target whose mode is chosen at run time, and `TeamRunResult.steps` is optional. `forkTeam` takes `promote`, `parentDefId` and `description`. `TeamRunTarget.interruptOnCap` is sent.
- **Schedule times.** On the server, an unset `last_run_at` or `paused_until` on `GET /v1/_schedules/{def_id}/state`, and an unset `last_updated` on `GET /v1/_schedules/list-all`, are now left out. They were sent as `0001-01-01T00:00:00Z`.

### The Teams bridge (#1771)

`@loomcycle/teams-bridge` 0.1.0 is a Microsoft Teams bot for loomcycle interactive agents. It is a separate package that talks to loomcycle through `@loomcycle/client` and needs no runtime change. Run it with `npx @loomcycle/teams-bridge`, or use it as a library.

- Each Teams user gets their own loomcycle user (`teams-<entra object id>`) and a 12-hour token limited to the one agent. A group chat or channel runs as a user of its own, never as one of its members.
- 1:1 chats stream; groups and channels get one message per turn. An interruption or a review hold becomes an Adaptive Card that only the conversation's owner can answer.
- After a restart, the next message continues the stored session.
- Activities from Entra tenants outside the allowlist never reach loomcycle.

It publishes to npm on its own `teams-bridge-v*` tag, not with this release. Setup is in `packages/teams-bridge/README.md`.

### Fixes (#1757, #1799, #1768)

- **Ollama survives a model-runner crash** (#1757). Ollama's runner sometimes dies under load and fails every call on it before the stream starts. A chat or embedding call is now retried at most twice (after 2 s, then 5 s), a decision call once. Each retry emits the existing `retry` event. A fault inside a started stream is never retried.
- **A data race in the loop** (#1799). The loop wrote timing onto the provider's own usage value, which a provider may share between calls. It now stamps a copy. Nothing a client sees changes.
- **`LICENSE` and `NOTICE`** (#1768). The repository has an Apache-2.0 `NOTICE`. Release archives ship it beside `LICENSE`, and every Docker image has both under `/usr/share/doc/loomcycle/`. The browser image also carries pinchtab's MIT license under `/usr/share/doc/pinchtab/`.

### Upgrade notes

- **A config that sets `decision.max_concurrent` or `memory.reranker.max_concurrent` no longer loads** (#1767), in any layer or on hot reload. Move the cap to `providers.<id>.lanes.decision.max_concurrent`, or for a listwise rerank to `providers.<id>.lanes.chat.max_concurrent` (or `max_concurrent`). The error names the replacement.
- **Unattributed `${NAME}` MCP server definitions are refused by default** (#1796). Before upgrading, look in the boot log for `mcp_server_defs: WARNING:` lines; each listed version will stop dialing. An admin acting in that tenant re-saves it (`create` with the same overlay) or retires it. `LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0` keeps the old warn-and-dial while the re-saves are pending; any other value, or none, refuses.
- **A run that cannot start at once is queued, not refused** (#1776). A client that relied on an immediate 429 should send `admission: "reject"`. A run over its user's `max_concurrent_runs_per_user` now waits too (up to `max_queued_runs_per_user`); with `reject` it still gets `429 per_user_quota_exhausted`. Clients that switch on run status or event type will see the new status `queued` and the events `queued`, `admitted`, `call_queued` and `call_admitted`.
- **A run on a busy provider waits instead of failing** (#1750, #1762). `provider_concurrency_exhausted` now goes only to the LLM gateway, `/v1/embeddings`, `/v1/_decide` and the describe route.
- **Size `max_concurrent_runs` for call lanes** (#1762). A run waiting for a provider's lane holds its run slot. With a small `max_concurrent_runs` and a capped local provider, local runs can fill it and hold cloud runs at admission. Raise `max_concurrent_runs`; the lanes now bound the provider's load.
- **Queued runs and secrets** (#1779). A queued run's per-run secrets come back after a restart only when `LOOMCYCLE_SECRET_KEY` is set and unchanged. Without it, such a run is failed at boot with stop reason `restarted`.
- **Removed:** `provider_queue_timeout_ms` and `LOOMCYCLE_PROVIDER_QUEUE_TIMEOUT_MS` (#1750). An old yaml key is ignored.
- **Default changed:** `max_queue_depth` is 32, was 16 (#1776). `provider_queue_depth` inherits it when unset.
- **Credential `expires_at` is enforced** (#1800). An expired credential is refused wherever it resolves. An expired or URL-restricted provider or tool key is never replaced by the operator's key or a host variable of the same name; the call fails. Check for expired credentials before upgrading and create them again. A create with an `expires_at` that is not in the future, or outside 1678–2262, is refused.
- **A GitHub App token request goes through the network guard** (#1764). A GitHub Enterprise server on a private network must be listed in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`. This applies to `$ghapp:` in MCP headers too, not only to clones.
- **Webhooks** (#1781, #1785). A `WebhookDef` overlay with an unknown, repeated or case-only key, or a key the server sets, is refused where it was accepted or ignored. A fork of a definition whose `body_size_limit_bytes` is above `LOOMCYCLE_MAX_REQUEST_BYTES` is refused until it lowers it. A verification secret that names one of loomcycle's own secrets (`LOOMCYCLE_AUTH_TOKEN`, `LOOMCYCLE_PG_DSN`, `LOOMCYCLE_SECRET_KEY`, a principal's `token_env`, …) is refused when written; an existing one answers every delivery with 503 and gets a boot warning.
- **`Read` refuses an unknown argument** by name (#1749); it was ignored. The tool's description changed, so agents with `Read` miss the prompt cache once.
- **`GET /v1/users/{user_id}/agents`** (#1794): an unknown `status`, or `all` with other statuses, is now `400 invalid_status`, and an unknown `awaited_state` is `400 invalid_awaited_state`. They returned an empty list.
- **Schedule state** (#1783): an unset `last_run_at` / `paused_until`, and `last_updated` on list-all, are left out, not `0001-01-01T00:00:00Z`.
- **Scope 403s have a JSON body** (#1792). The status, the `insufficient scope` text and the `WWW-Authenticate` header are unchanged. A client that parsed the body as text should read `error`.
- **MCP `configured_run` refusals** (#1791): `configured_run_cap` is category `business` and not retryable (was transient); `no_store` is `business`; the 403s are `permission`; `rate_limited` carries `retryAfterSeconds`. On gRPC, a 403 is `PermissionDenied`, was `Internal`.
- **`@loomcycle/client`: `TokenLimitExceededError` is no longer a `BackpressureError`** (#1782). Code that retried every `BackpressureError` retried a hard budget refusal forever. Check `e.retryable` instead. The Python client keeps its classes and adds `retryable`, false for `token_limit_exceeded` and `configured_run_cap`.
- **Migration 0103** adds `runs.queued_input` (#1779). It is additive.
- **Snapshots** gain a `queued_runs` section (#1779), and the restore result two counts, `queued_runs` and `queued_runs_requeued`.
- **gRPC:** new RPC `Evaluation`; new fields `Event.admission` (`AdmissionInfo`, with `provider` and `kind`), `RunRequest.admission`, `Agent.queue_position` / `queue_reason`, and `ListUserAgents` `statuses` / `awaited_states` / `truncated`.
- **New HTTP routes:** `POST /v1/_evaluation` and `GET /v1/_agentdef/{name}/overridable`.
- **New settings:** the eight `LOOMCYCLE_GIT_SOURCE_*` settings above (off unless `LOOMCYCLE_GIT_SOURCE_HOSTS` is set); `LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER` (16); and on a provider, `lanes`, `max_models_in_flight` (0, unlimited) and `model_switch_max_wait_ms` (30000 when `max_models_in_flight` is set).
- **Clients:** `@loomcycle/client` 1.113.0 publishes with this tag (#1803). New in it: the queued-run and call-lane events, `admission`, `retryable` and `RateLimitedError`, `InsufficientScopeError`, the evaluation, schedule, routing, model, def-name, credential, overridable and change-feed methods, typed overlays, and the hook signature verifiers. The Python client gains `admission`, `queue_position`, the lane fields on `AdmissionInfo`, `retryable`, `evaluation()` and its helpers, `list_user_agents` statuses and awaited states, and the signature verifiers; it publishes on its own `python-v*` tag.
- **Packages:** `@loomcycle/def-fields` 0.5.0 (the `allow_overrides` and `retention` agent fields) and `@loomcycle/library` 0.8.0 (on def-fields `^0.5.0`) are on npm, published by their own tags. The `ephemeral_volumes` and `signing_secret` fields are in def-fields and reach npm with its next release. `@loomcycle/teams-bridge` publishes on its own tag.

## What's in v1.112.0

*A deployment can now hand a token to a machine it does not control, and run agents on content it may not keep. A token can **expire**, be **limited to certain agents**, be **locked out of per-run settings**, be **rate limited**, and be made **no-trace**. A **no-trace run** delivers what it says to its caller and stores none of it. A stored **run tree can be deleted**. Token budgets can be **enforced on runs in flight**, and the raw model endpoints are now gated and metered.*

A minor release: #1727 to #1743, except #1738 (README wording).
- #1727, #1730, #1731, #1734 restrict a token: lifetime, agents, per-run settings, rate;
- #1733 gates and meters the raw model endpoints; #1735 and #1736 make budgets strict and the gateway's estimate durable;
- #1737 locks an agent's per-run settings on its definition;
- #1728 lets a code agent forward untrusted input to a child by reference;
- #1732 deletes one run tree;
- #1739 to #1743 are no-trace runs and the timed override that debugs them;
- #1729 changes the bundled cloud-deployment config only.

### A token that is safe to hand out (#1727, #1730, #1731, #1734)

A token minted for a client machine can now be narrowed to one purpose. Each restriction is set where a token is minted — `operator-token create`, the `operatortokendef` tool, `POST /v1/_users/{subject}/tokens`, and a declared principal — and is carried over by a rotation.

- **Lifetime.** `ttl_seconds` gives a token a fixed life; it stops authenticating when it runs out, and a rotation does not extend it. A token cannot mint one that outlives it. A `substrate:admin` token cannot have one. The per-user mint also takes `name` and `max_active` (keep at most N of a user's tokens alive). `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` caps a per-user token's life.
- **Agents.** `agents: [...]` limits the token to starting those agents as top-level runs. Such a token is also held to the run endpoints on every transport: definitions, team walks, schedules, the model gateways, channels, memory, documents, the credential store and A2A are all refused (`restricted_token`), whatever its scopes.
- **Per-run settings.** `overrides: "none"` locks the model, provider, tier, effort, the limits, sampling, compaction, hooks and the rest for runs the token starts; a list names the ones it may still set. A locked setting is refused (`override_not_allowed`), never ignored.
- **Rate.** `rate: {per_minute, burst}` bounds how often the token starts runs (`429 rate_limited` with `Retry-After`). It is per user, not per token, and per replica. `LOOMCYCLE_RUN_RATE_PER_MINUTE` sets a default for tokens that carry none; it does not bind operators.

See "Multi-tenant authentication" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### The raw model endpoints are gated and metered (#1733, #1736)

`POST /v1/_llm/chat`, `/v1/chat/completions` and `/v1/embeddings` call a model with no agent, no tool allowlist and no run row.

- **A new scope, `llm:gateway`.** Admins and tenant operators have it. An isolated user's token needs it by name.
- **Budgets apply.** A caller at a hard token budget is refused before the call.
- **Usage is recorded.** A call the provider reports on is written to the usage ledger and counts against the caller's budget, cache tokens included.
- **A call that ends with no usage report** — the caller hung up, the provider failed first, or it reported zero — is counted against the budget by an estimate from its size. The estimate survives a restart and stays out of the usage ledger.

### Strict token budgets (#1735)

A hard budget was checked when a run starts and nowhere else, so a run already going, and every sub-agent it spawned, kept spending. `LOOMCYCLE_LIMITS_ENFORCE=strict` also checks it before each model call of a run in flight, before each sub-agent starts, and when a paused run is resumed.

- A run stopped this way ends `failed` with `stop_reason` `token_limit`.
- A refused sub-agent is the parent's tool error; no row is written for the child.
- The overshoot is bounded by one model call per run in flight.
- Any value other than `strict` stops the server at boot.

Unset, nothing changes.

### An agent that runs on its definition only (#1737)

`allow_overrides: false` on an agent's definition refuses the same per-run settings a token lock does, for every caller: an admin, a schedule and a parent agent included. A fork can add the lock and cannot remove it, and a lock written in the server config holds over a tenant's own version of the agent.

### Untrusted input by reference in a code agent (#1728)

A code agent receives its run's untrusted input as `input.untrusted`, as data. It can forward it to a child without copying it through a prompt: `untrusted: [{ref: "input"}]` on a spawn. The code-agent ABI is 1.3.0. A run request with a segment block type the server does not know is now refused with 400.

### Deleting one run tree (#1732)

`DELETE /v1/runs/{run_id}/tree` removes a run and every run it spawned; `DELETE /v1/sessions/{session_id}?cascade=children` does the same for every run of a session. Events, runs, sessions left empty, interrupts, evaluations, queued memory rows and indexed turns go; the usage ledger stays.

- It needs the new **`runs:delete`** scope and read access to the run. Admins and tenant operators have the scope; an isolated user and a member do not unless it was granted at mint.
- A tree with a live run is refused whole (409). A second call returns zero counts.
- The answer names what it could not reach: stored snapshots, export files already written, and facts consolidation derived from the sessions.
- Also the gRPC `DeleteRunTree` RPC, the MCP `delete_run_tree` tool and both clients.

### No-trace runs (#1739, #1740, #1741)

By default loomcycle keeps everything a run says. A **no-trace run** is for content with one purpose and one reader: what it says is delivered to whoever is waiting for it and stored nowhere.

The mode is `retention: "none"`. It is set on a run (`POST /v1/runs`, a batch child, gRPC `Run` / `SpawnRunBatch`, MCP `spawn_run` / `spawn_runs`), on an agent's definition, or on a token. A sub-agent of a no-trace run is one too. The strictest wins and nothing loosens it; an unknown value is refused.

- **Kept:** the run and session rows and their links, status, a stop reason from the runtime's own list, the error's class, model and token counts, the usage ledger, and a marker for every event (its type, time, size, tool name and ids).
- **Not stored:** prompts, the system prompt, model text and reasoning, tool arguments and results, the final answer, the error's text, the run's settings record, trace-index rows and banked turns.
- **Still delivered:** the caller on the connection gets the full stream and the full error; a parent gets its child's answer.
- **Refused** (`retention_none_unsupported`), because each needs stored content: interactive and review runs, joining or continuing a session, replay, compaction, recap, a draft, a message or a retune, an `Interruption` question, an evaluation, and a team walk.
- **A token whose runs are no-trace** is held to it on every route: it cannot write a draft, a message, an interrupt answer or review feedback either.
- **A pause does not park it.** It keeps running, a snapshot leaves it out, and the pause result counts it in the new `unpausable_runs_count`.
- **A restart loses it.** At boot it is failed with stop reason `not_resumable` and its idempotency key is released, so a retry with the same key starts a new run.
- **Tools that write on purpose, and hooks, are unchanged.** `Memory`, `Channel`, `Document`, `Write` and MCP tools store what the agent tells them to; a hook is sent what it always was.

See "No-trace runs" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Debugging a no-trace agent (#1742, #1743)

`PUT /v1/_retention/overrides {agent, until}` stores the runs of one no-trace agent, in one tenant, for at most 24 hours. `DELETE` ends it early and `GET` lists what is in force.

- It lifts a mode that came from the agent's definition or the run request. It never lifts one a token requires, and not the mode a sub-agent inherits from a no-trace parent.
- An admin sets one for any tenant, a tenant operator for its own. A member may not set, clear or list them.
- Setting and clearing are audited; with an audit log configured, an override whose record cannot be written is not set.
- The console shows a banner while one is in force.
- Also the gRPC `RetentionOverride` RPC, the MCP `retention_override` tool and both clients.

### Upgrade notes

- **Migrations 0098 to 0102** add columns for token expiry and restrictions, an estimate column on `usage_carry`, a `retention` column on `runs` and `sessions`, and a `retention_overrides` table. All are additive.
- **An isolated user's token loses the raw model endpoints** unless it is granted `llm:gateway`. Admin and tenant-operator tokens are unaffected.
- **Raw model calls now count against token budgets and appear in the usage ledger.** A deployment with a hard budget will see gateway callers refused at it.
- **Expired token rows are deleted** seven days after they expire (`LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS`; `0` keeps them). Only a token minted with a lifetime is ever removed this way.
- **A per-user token's name is stored as `u-<name>`,** and the operator tool no longer creates or acts on names in that namespace.
- **A run request with an unknown segment block type is refused (400).** It was passed through before.
- **gRPC:** new RPCs `DeleteRunTree` and `RetentionOverride`; new fields `RunRequest.retention`, `Agent.retention` and `PauseRuntimeResponse.unpausable_runs_count`.
- **MCP:** two new tools, `delete_run_tree` and `retention_override`.
- **Clients:** `@loomcycle/client` 1.112.0 publishes with this tag. The Python client is at 1.112.0 and publishes on its own `python-v1.112.0` tag.
- **`@loomcycle/def-fields`** gained the `allow_overrides` and `retention` agent fields and is still at 0.4.0; they reach npm with its next release.
- **New settings, all off or unbound unless set:** `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS`, `LOOMCYCLE_RUN_RATE_PER_MINUTE` / `LOOMCYCLE_RUN_RATE_BURST`, `LOOMCYCLE_LIMITS_ENFORCE`.

## What's in v1.111.0

*The runs a team walk's member starts are now **part of the walk**. A sub-agent, at any depth, carries its member's walk, state and visit, so it is in the walk's listing and on the walk's stream. Before, a member reported as waiting on its children named runs that a client following the walk could not see. And **any run can be asked for its children**: a listing by parent run, and the same filter on the run-state stream.*

A minor release: #1723, #1725 and #1724.
- #1723 puts the walk on a member's sub-agents;
- #1725 lists a run's children and filters the run-state stream by parent run;
- #1724 is `@loomcycle/def-fields` 0.4.0, published by its own tag, and changes nothing in the runtime.

### A member's sub-agents carry the walk (#1723)

A run a walk's member started with the `Agent` tool, waited for or with `mode: "poll"`, named the member as its parent run and carried nothing of the walk. So it was in neither `GET /v1/runs?walk_id=` nor the walk-filtered run-state stream. The only way to find it was the user's own run listing, which returns at most 100 runs and only running ones unless asked.

A run started under a member, at any depth, now carries the member's `walk_id`, `state` and `state_visit` on its `parent_context`:

- **It is in the walk's listing and on its stream,** running and ended, with the state it belongs to.
- **It is not a member.** Tell the two apart by `parent_run_id`: a member's is the walk's run id, a sub-agent's is the run that started it.
- **The wave and board fields stay the member's alone.** A wave's width is still the runs its Starter dispatched, and a board card is still pinned to the direct handler.
- **A walk a member starts** carries the outer walk's fields on its own run. The runs it starts carry its own `walk_id`.

**What changes for a client.** `GET /v1/runs?walk_id=`, the `ListWalkRuns` RPC, `list_runs` with `walk_id` and the walk-filtered stream return more rows for a walk whose members spawn sub-agents. A client that treated every row other than the walk's own as a member should check `parent_run_id`.

**What does not change.** A member's sub-agent still takes only a review verdict, not a retune or a steer, as every sub-agent does. The rule that decides it now asks whether the walk is the run's parent, where it asked whether the run carried a walk at all.

The Web UI's walk view lists a sub-agent under its member, indented, and a visit's count reads "N runs, M sub-agents".

`@loomcycle/client` 1.111.0: `listUserAgents` accepts `status: "all"` in its type. The server always took it.

See "Watching a team walk live" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### A run's children: a listing and a stream filter (#1725)

The runs an agent spawned could be found only in its user's listing (`GET /v1/users/{user_id}/agents`), filtered by parent run on the client. #1723 covers runs inside a walk; this covers any run.

```sh
curl "…/v1/runs?parent_run_id=r_0d07…&limit=100"
# → {"agents":[{"run_id":"r_2452…","parent_run_id":"r_0d07…",…}, …], "next_cursor":""}

curl -N "…/v1/users/{user_id}/agents/stream?parent_run_id=r_0d07…"
```

- **The listing** returns the run's direct children, the runs whose `parent_run_id` is that run, of every status, oldest first. It pages like the walk listing: `limit` up to 1000 (default 100) and `next_cursor`, empty on the last page.
- **Direct children only.** The run itself is not in it, and neither are its grandchildren; ask again with a child's id for those.
- **Who may list.** Whoever may read the run. Otherwise it is the 404 `unknown_parent_run_id` an unknown run gets, and each child is held to the same rule.
- **`walk_id` or `parent_run_id`, not both** on the listing: both is 400 `one_filter_only`. Neither is still 400 `walk_id_required`.
- **The stream filter** passes only events whose `parent_run_id` is that run. The `stream_open` event echoes it as `filter_parent_run_id`. With `walk_id` as well, an event must pass both.

| Transport | Listing | Stream filter |
|---|---|---|
| gRPC | `ListChildRuns` RPC (`runs:read`) | `StreamUserRunStatesRequest.parent_run_id` |
| MCP | `list_runs` with `parent_run_id` | `stream_user_run_states` with `parent_run_id` |
| TypeScript | `listChildRuns(parentRunId, { limit, cursor })` | `parentRunId` option |
| Python | `list_child_runs(parent_run_id, limit, cursor)` | `parent_run_id=` |

No schema change: it reads through an index the store already had. See "One run's children" in [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### `@loomcycle/def-fields` 0.4.0 (#1724)

The agent registry has had a "Decision models" field (`decision {default, models}`) since v1.107.0, and it had not been published: npm's latest was 0.3.0. A host that renders an agent's form from the published registry had no field for the block.

0.4.0 is that field and nothing else. It is published by the `def-fields-v0.4.0` tag, apart from this release.

`@loomcycle/library` depends on `@loomcycle/def-fields` at `^0.3.0`, which does not take 0.4.0. It is not re-released here.

### Upgrade notes

- **Nothing to migrate.** There is no schema change. The walk's listing reads a column that the new rows fill as every member's row does.
- **Runs already stored are not rewritten.** A sub-agent that ran before the upgrade stays out of its walk's listing.
- **A client of the walk listing or the walk-filtered stream** will see sub-agents. Use `parent_run_id` to separate them from members.
- **MCP `list_runs`:** the refusal for a wrong combination of arguments now reads "exactly one of user_id, walk_id or parent_run_id".
- **gRPC:** `ListChildRuns` is a new RPC, and `StreamUserRunStatesRequest.parent_run_id` a new optional field. A server older than this release ignores the field and streams every run; a client can tell from the missing `filter_parent_run_id` on the HTTP open frame.

## What's in v1.110.0

*A team's `decision` state now leaves **its answer on the walk's own run**. A walk started detached returns no steps, and a decision state starts no member run, so a client holding only the walk's run id could not read what a decision answered. Each decision is now one `team_decision` event on that run, read while the walk runs and after it has ended.*

A minor release: #1720 and #1721.
- #1721 is the decision record;
- #1720 deploys releases to the cloud stack from CI, and changes nothing in the runtime.

### A decision's answer is on the walk's run (#1721)

v1.109.0 put a decision's answer on the walk's step, and a step exists only in the response of a walk the caller waited for. A walk started with `mode: "detach"` answers `{run_id, status}`, and its run kept its final text and the end state it reached. A client that detaches, as a canvas does so that it can abort, watch and answer pauses, could only infer which way a decision went from the state that ran next. It could not show the option's probabilities, the answers to the unrouted questions, or which model answered.

Each visit of a walk to a `decision` state now appends one event to the walk's own run:

```json
{"type": "team_decision", "team_decision": {
  "state": "triage",
  "visit": 2,
  "edge": "conditional:billing",
  "next": "billing-desk",
  "answer": {"model": "decide",
             "answers": {"route": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.91, "support": 0.06, "sales": 0.03}, "confidence": 0.85},
                         "urgent": {"type": "noul", "noul": 0.4}},
             "usage": {"input_tokens": 1116, "output_tokens": 4}}
}}
```

- **`visit`** is the walk's count of state visits: 1 for the walk's first state, never reused. It is the number a member run carries as `parent_context.state_visit`, so a decision sorts among the walk's member runs, and a loop through one decision state gives one event per pass.
- **`edge`** is the transition the walk took: the answer's own `conditional:…`, or `success` when the answer had no transition of its own or the state has no `route`.
- **`next`** is the state that transition leads to.
- **`answer`** is the object a waited-for walk's step carries as `answer`.

**Where it is read.** The event is a row of the walk run's transcript, so the reads every run has return it:

- `GET /v1/runs/{run_id}/stream` on the walk's run (`streamRunByID` in `@loomcycle/client`, `StreamRun` over gRPC). While the walk runs, the decision arrives as it is made. Opened after the walk has ended, the stream replays the run's events, the decision among them, and closes.
- `GET /v1/sessions/{session_id}/transcript` for the walk run's session.

**Every walk records it,** however it was started: waited for, `detach` or `poll`.

**What is not recorded.**

- A decision whose call failed. The walk's run ends `failed` with the state and the model's code in its error, as before.
- The event is not on the user run-state stream. That stream carries a run's state changes, and a decision changes none.
- No MCP tool reads a run's events, for any run, so a client that has only MCP cannot read a decision.

`@loomcycle/client` 1.110.0 has `"team_decision"` in `EventType` and `TeamDecisionInfo` as `AgentEvent.team_decision`. The Python adapter has `TeamDecision` as `AgentEvent.team_decision`; its `answer` is the JSON as bytes. The gRPC `Event` gains `team_decision` (field 21). The Web UI's run transcript shows a decision as one line, with the answer when expanded. See [`docs/DECISION-MODELS.md`](docs/DECISION-MODELS.md).

### Releases deploy to the cloud stack from CI (#1720)

`cloud-deployment/` is the compose stack behind the hosted instance. It was upgraded by hand and had fallen 52 minor releases behind.

- The compose file takes the runtime version, the sidecar version and the loomboard ref from a `versions.env` file on the host, so a deploy changes three values and never edits the file.
- A deploy script on the host deploys one release: it checks that the tag exists, is published and is not older than what runs, dumps the databases, pulls, restarts and waits for `/healthz` to report the new version. If it does not, the previous files are put back.
- A workflow polls for the newest loomcycle and loomboard release tags every ten minutes and hands them to the script.

Nothing in the binary, the images or the adapters changes with it. See [`cloud-deployment/INSTALL.md`](cloud-deployment/INSTALL.md).

### Upgrade notes

- **Nothing to migrate.** There is no schema change. The event is a new row type in the existing events table.
- **A client that switches on every event type** meets a new one, `team_decision`, on a team walk's run only. No agent run emits it.
- **gRPC:** `Event.team_decision` is a new optional message; a client built against the older proto ignores it.

## What's in v1.109.0

*A team graph can now **ask a decision model directly**: a state of kind `decision` asks typed questions, takes the transition the answer selects and binds the answers to team variables, with no agent run. A team that could never route on one of the answers is refused when it is saved. And **starting a team walk is safe to retry**: `idempotency_key` on a team run returns the walk already started.*

A minor release: #1717 and #1718.
- #1717 is the `decision` state;
- #1718 is the idempotency key on a team run.

### A `decision` state in a team graph (#1717)

To route on a decision model, a team had to run an agent whose only job was to call the `Decision` tool and print a signal. That gave back the cost and latency a decision model exists to avoid, and hid the questions in a prompt or a code body where neither a reader nor `verify` could see them.

A state of kind `decision` makes one call to a decision model and starts no agent run:

```json
{"state": "triage", "handler": {
  "kind": "decision",
  "about": {"ticket": "{{thread.output}}", "customer_tier": "${var.tier}"},
  "questions": {
    "route":  {"type": "choice", "instructions": "Which team should handle this ticket?",
               "criteria": {"billing": "invoices, refunds", "support": "bugs, outages", "sales": "upgrades"}},
    "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"}
  },
  "route": "route",
  "capture": {"team": "$.answers.route.choice", "urgent": "$.answers.urgent.noul"}
}}
```

- **`about`** is the JSON object the questions are about. Its string values, at any depth, take `${var.<name>}`, `${now.*}`, `${team.*}` and `{{thread.output}}` (what the previous state handed over). No other `{{…}}` placeholder is allowed there.
- **`questions`** are the `Decision` tool's (`choice`, `noul`, `score`), checked by the same validator. **`model`** names one of the operator's decision models; omitted means the default.
- **`route`** names the question that picks the transition. A `choice` takes `conditional:<option>`. A `noul` takes `conditional:true` when the probability of yes is at or above `threshold` (default 0.5), else `conditional:false`. A `score` cannot route. Without `route` the state advances on `success`.
- **`success` is the "everything else" edge:** a routed answer with no transition of its own takes it.
- **`capture`** reads the answer: `$.answers.<question>.choice`, `.noul`, `.score`, `.confidence`, `.probabilities.<option>`.
- **The next state receives what the decision state was handed**, not the answer. The answer reaches later states through the variables it bound.
- **A walk's step gains `answer`**, the model's answer as an object. `output` stays what the state handed on, as on every other kind.
- **There is no confidence gate.** The probabilities are not calibrated. A team that wants one captures `confidence` and routes on it in a later state.
- **A failed call fails the walk** at the state, with the model's code: `state "triage" handler: decision: timeout: …`. `timeout_ms` bounds the call.
- **The call is charged to the walk's own run** and counts against its token budgets, as a `Decision` call inside a run does.

**Refused when the team is saved,** each with the JSON path of the value at fault:

- a routed answer with nowhere to go: every option of a routed `choice` needs a `conditional:<option>` transition, and a routed `noul` both `conditional:true` and `conditional:false`, unless the state has a `success` transition. The refusal names the option.
- a `conditional:` transition for an answer the question cannot give, any `conditional:` transition from a state with no `route`, and a `pushback:` transition from a decision state;
- a question that is not in the `Decision` tool's shape, with the tool's own code (`bad_question`, `bad_options`);
- an agent, a prompt or hooks on the state, and the decision fields on a state of another kind.

**Reported by `verify` and not refused,** so a team written on one deployment can be stored on another:

| Issue kind | Meaning |
|---|---|
| `decision_unconfigured` | this deployment declares no decision models |
| `decision_model_unknown` | `model` is not one the operator offers |
| `decision_limits` | more questions or options than that model takes |

**Who may write one.** A decision state reaches a model with no agent in between. An agent that authors a team must hold the `Decision` tool to give it a decision state, and may name only a model its own `decision` block allows; otherwise the save is refused as `decision_authority`. An operator is not narrowed. A state asks from the operator's list whoever starts the walk, as a team's member agents run under their own definitions.

`@loomcycle/client` gains `TeamDecisionHandler`, `TeamDecisionQuestion`, `TeamDecisionAnswer` and `TeamRunStep`, which is now the element type of `TeamRunResult.steps`. The team diagram notes which question a decision state routes on. See [`docs/DECISION-MODELS.md`](docs/DECISION-MODELS.md).

Not in this release: the answer while the walk is still running. A walk reports nothing per step until it ends, so a client sees a decision's answer on the finished walk's step.

### Starting a team walk is safe to retry (#1718)

A detached team walk answers `{run_id, status}` and runs on. If that answer was lost, the caller could not tell "never started" from "started", and starting again ran the whole team twice. A single run has had `idempotency_key` for this since v1.106.0; a team walk had none.

`TeamDef op=run` now takes `idempotency_key`, with a run's grammar (1 to 200 characters of `[A-Za-z0-9:._-]`) and a run's owner (the caller's tenant and user):

- **A second start with the key starts nothing.** It is answered with the walk that holds it: `name`, `def_id`, `run_id`, `status` and `deduplicated: true`, plus `final_state` and `final_output` once the walk has ended.
- **The key is resolved before the team is read,** before admission and before any row exists. A retry costs nothing and is answered even if the team has since been retired or re-promoted.
- **Two starts racing on one key** start one walk; the other is answered with it.
- **A retry never waits.** A walk still running is answered at once as `running`, whether or not the retry asked to wait.
- **A deduplicated answer has no `steps`.** The trace is built by the request that walks and is not stored; read the walk's member runs.
- **A walk's key is held apart from a run's.** The same key may name an agent run and a team walk.
- **Refused** with `board_chunk_id` (a board-bound walk resumes its stored position, so repeating it is already safe) and with `mode: "poll"`.

It is a field of the `teamdef` tool, so HTTP, gRPC and MCP carry it. `@loomcycle/client` has `idempotencyKey` on `TeamRunTarget` and `deduplicated` on both results. The Python adapter passes the tool's input through, so `idempotency_key` works there unchanged.

### Upgrade notes

- **Nothing to migrate.** There is no schema change, and a definition without a decision state hashes as it did.
- **`@loomcycle/client` 1.109.0:** `TeamRunResult.steps` is typed `TeamRunStep[]` where it was `Array<Record<string, unknown>>`. `TeamRunStep` keeps an index signature, so code reading other keys still compiles.
- **Internal:** the decision question types and their validator moved to `internal/decisionq`; `internal/decision` keeps every name as an alias.

## What's in v1.108.1

*Three provider fixes. A run on a DeepSeek thinking model failed on its first call after a context compaction that happened in the middle of a turn; it now continues. An agent with `effort: low`, or with no effort set, got a 400 from two Gemini 3 models; it now runs. A `model_pattern` alias could resolve to a suffixed variant of a model instead of the model itself.*

A patch on the v1.108 line: #1711 to #1715.
- #1713 and #1715 are the compaction fix;
- #1712 is the Gemini fix;
- #1714 is the pattern alias fix;
- #1711 updates the `cloud-deployment/` example.

### A compaction in the middle of a turn no longer ends the history on the runtime's own turn (#1713, #1715)

A run on `deepseek-v4-pro` failed on its first call after an automatic compaction:

```
openai 400: The `reasoning_content` in the thinking mode must be passed back to the API.
```

A compaction replaces the history with the summary, an assistant turn acknowledging it, and the most recent turns kept as they were. The kept part starts at a user turn. A run that is one long turn of tool calls has no such turn after its task, so nothing was kept and the history ended on the acknowledgement. The next request asked the model to continue an assistant turn that no model had written.

- **A compacted history now ends the way the history it replaces ended.** If that one ended on a user turn, with tool results waiting for the next call, and the replacement ends on an assistant turn, a user turn is added: "Continue the task from where the summary above leaves off."
- It applies to compaction and to recap, in the running loop and when a resumed run rebuilds its history from the transcript.
- A parked run's compaction is unchanged. Its history ended on the answer, and the operator's next message is the user turn.
- The run keeps its model and its effort. #1713 first moved a compacted run to the provider's non-thinking model; #1715 replaces that, and it never shipped in a release.

Measured against the DeepSeek API on `deepseek-v4-pro`: an assistant turn without reasoning is accepted anywhere inside the history, including one that calls tools. The request is refused only when it ends on that turn and carries tools. With the added user turn it is accepted.

### Gemini 3: a thinking level in place of a zero budget (#1712)

A run on `gemini-3.5-flash-lite` failed with a 400 "Request contains an invalid argument", often after a fallback from a local model. The driver sent `thinkingBudget: 0` for `effort: low` and for any effort whose `max_tokens` left no room for a budget. The Gemini 3 models cannot turn thinking off, and they disagree about that value:

| Model | `thinkingBudget: 0` | `thinkingLevel: low` |
|---|---|---|
| `gemini-3.5-flash-lite` | 400 | accepted |
| `gemini-3.8-flash` | accepted | accepted |
| `gemini-3.1-pro-preview` | 400 | accepted |
| `gemini-2.5-flash` | accepted | 400 |

- On a `gemini-3*` model a zero budget is now sent as `thinkingLevel: "low"`, the one value every model of the line accepts.
- A budget above zero is sent as before, and the 2.5 line still gets `thinkingBudget: 0`.
- The two fields are never sent together.

**Behaviour change:** `effort: low` on a Gemini 3 model now means the lowest thinking level, not thinking off. `gemini-3.8-flash` accepted the zero budget before and now gets the level too.

### A pattern alias prefers a model over its own variant (#1714)

A `models:` alias with `model_pattern` resolves to the newest model the provider lists that matches. When two matches had the same version, the longer id won. `gemini-*-pro*` resolved to `gemini-3.1-pro-preview-customtools` instead of `gemini-3.1-pro-preview`.

- When one id is the other plus a suffix and the versions are equal, the shorter id wins.
- Ids that are not related that way keep the old order (`qwen3.8:latest` over `qwen3.8:27b`).
- A newer variant still wins over an older base model.

### The cloud deployment example moves to 1.108.0 (#1711)

`cloud-deployment/` now pulls `loomcycle-browser:1.108.0` for the runtime and for the migration step, so the local `loomcycle.Dockerfile` is gone. Its config carries the current models and tiers, model kinds, the `decision:` and `memory:` blocks, and interruption, concurrency and cache settings. The four memory agents are pinned to `ollama-local`, so consolidation does not send transcripts to a cloud provider. `INSTALL.md` has an upgrade section.

### Upgrading

- No migration and no config change.
- A patch tag builds only `loomcycle-browser` for linux/amd64. The other images, the binaries and the Homebrew formula stay at v1.108.0.
- The clients are 1.108.1 with no API change.

## What's in v1.108.0

*The person who runs a team can now see and release what the team wrote to **its own channels**. A channel a team declares for itself was readable only by the team's agents; every channel call refused it. Three calls addressed by team and the channel's local name now cover it — **list** (with counts), **peek** and **release** — on HTTP, gRPC, MCP and both clients. Several guides and help articles that disagreed with the code are corrected.*

A minor release: #1705 to #1709.
- #1705 and #1706 are the operator's view of a team's own channels;
- #1708 fixes a config error message;
- #1707 and #1709 are documentation.

### A team's own channels, seen from outside the team (#1705, #1706)

A team can declare channels of its own under `local.channels`. They are stored under a name nothing may address, so `list_channels` never showed them and every peek refused them as not declared. An operator could not see what a team had written, how much was waiting or what was held. A channel declared with `hold: true` was worse: only an agent of the team with a publish grant could release it, so a team whose writers are its states, a schedule or a webhook held everything it wrote for good.

Three calls now address such a channel by **the team and the name the team gave it**:

| | List | Peek | Release |
|---|---|---|---|
| HTTP | `GET /v1/_teamdef/{team}/channels` | `GET /v1/_teamdef/{team}/channels/{name}/peek` | `POST /v1/_teamdef/{team}/channels/{name}/release` |
| MCP | `list_team_channels` | `peek_team_channel` | `release_team_channel` |
| gRPC | `ListTeamChannels` | `PeekTeamChannel` | `ReleaseTeamChannel` |
| TypeScript | `listTeamChannels()` | `peekTeamChannel()` | `releaseTeamChannel()` |
| Python | `list_team_channels()` | `peek_team_channel()` | `release_team_channel()` |

- **List** returns each channel's settings, which version of the team declares it (`declared_in`: `active`, `inactive` or `retired`, with `def_id` and `version`) and its counts: `message_count`, `held_count`, `awaiting_hooks_count`, and the oldest and newest visible message times. A channel only a retired version declares is still listed and readable.
- **Peek** returns messages and consumes nothing, so the team still reads every one. `from_cursor` and `max_messages` work as on the existing channel peek. Held messages are counted by the list, not shown.
- **Release** delivers the oldest held messages and answers `released` (message ids), `released_count` and `still_held`. `count` defaults to 1 and is capped at 1000. With nothing held it releases nothing and says so. It works whether or not the active version still declares `hold`, and it wakes readers waiting on the channel. The HTTP body refuses unknown keys.
- **Tenant and user.** The team is resolved in the caller's tenant; another tenant's team is the same `team_not_found` as one that does not exist. A user-scoped channel is read and released in the caller's own keyspace. `user_id` names another user for an admin; anyone else gets the same `team_channel_not_declared` as for a channel the team does not have. The list's counts for a user-scoped channel are summed over the tenant's users.
- **Scopes.** A `substrate:tenant` token may make all three calls. A member token also needs `channel:read` to list or peek and `channel:publish` to release, on HTTP and MCP, as the existing channel calls ask.
- The stored name stays unaddressable everywhere. There is no publish, ack or purge of a team's own channel from outside the team.

### Smaller changes

- The config-load error for an unknown `memory_scopes` value now lists `tenant` with `agent` and `user` (#1708).
- The MCP `path` tool's description said `mkdir` is a no-op; it creates an empty directory (#1707).

### Documentation (#1707, #1709)

The guides were checked against the source and against the document store they are exported from.

- `docs/CONFIGURATION.md`: `fallback_on_error` has no default; an unknown `user_tier` is refused with 400; unset scope grants resolve to the caller's own data; the config-load error table carries the real messages.
- `docs/MCP_SERVER.md`: which token may call which tool, and the tools added since v0.33.0.
- `docs/MULTI-REPLICA.md`: the variables and routes it names now exist.
- `docs/CLAUDE-CODE.md`: the plugin is a thin client.
- `docs/DOCUMENTS.md` gains "Search: indexing, re-indexing and reranking".
- `docs/SQL_MEMORY.md`, `docs/MEMORY-BACKENDS.md`, `docs/ARCHITECTURE.md`, `docs/MCP_INTEGRATION.md`, `docs/POSTGRES.md`, `docs/PLAN.md` and three help articles are corrected.
- `.env.insecure.example` drops three variables no code reads.

### Upgrade notes

- **No schema migration.**
- **Additive on the wire:** three HTTP routes, three RPCs, three MCP tools. Nothing existing changes.
- **Clients:** `@loomcycle/client` 1.108.0 and the Python client 1.108.0 add the three team-channel methods.

## What's in v1.107.0

*An agent, a code body or an outside caller can now ask a **decision model** typed questions — pick one option, yes or no, a position on a scale — and get each answer with probabilities instead of text. It is the **`Decision`** tool inside a run, **`POST /v1/_decide`**, the gRPC **`Decide`** RPC and the MCP **`decision`** tool, with `decide()` in the TypeScript and Python clients. A `models:` alias can now say what kind of model it is (**`kind: chat | decision | embedder`**), so a model cannot be wired where it cannot serve. An MCP session is now held to the scopes its token was granted.*

A minor release: #1691 to #1703.
- #1691, #1692, #1694 and #1697 are decision models; #1699, #1700 and #1702 complete them (client error classes, refusal wording, the OpenAPI contract);
- #1695 and #1703 hold a member token to its scopes, over MCP and for team runs;
- #1701 fixes the `local` preset;
- #1693 is the team-definition key fix;
- #1696 is a test fix.

### Decision models (#1691, #1692, #1694, #1697, #1699, #1700, #1702)

A decision model reads a piece of state and answers typed questions about it. It writes no text, so there is no reply to parse: the answer cannot be malformed and cannot name an option that was not offered. A call is a few hundred input tokens and a handful of output tokens. Until now loomcycle used one in a single place, the memory reranker. Guide: [`docs/DECISION-MODELS.md`](docs/DECISION-MODELS.md).

**Setting it up.** Decision models are declared like any other model and listed in a new `decision:` block:

```yaml
models:
  decide: { provider: ollama-local, model: nimble, kind: decision }
decision:
  default: decide
```

- Only Ollama serves decision models today (0.35 or later: `nimble`, `clef`, `clef-flash`, `tev1`). The driver sits behind an interface another provider can implement.
- `decision.models` is optional; omitted, every alias tagged `kind: decision` is allowed. `timeout_ms` defaults to 30000 and `max_concurrent` to 4, per provider.
- No `decision:` block means the capability is off and nothing changes. Changing the block needs a restart.

**The request and the answer** are the same JSON on every surface: `{model?, state, questions}` in, `{model, provider, served_model, answers, usage}` out.

- `choice` takes an object of option → description (or `null`), 2 to 26 options, and returns the option picked with a probability per option.
- `noul` is yes or no, and returns the probability of yes. `0` is an answer.
- `score` takes an array of level descriptions, lowest first, and returns the expected position with a probability per level.
- 1 to 64 questions per call. The request must fit the model's context and is **never shortened for you**.
- Each answer is returned exactly as the model gave it.
- **The probabilities are not calibrated.** Compare options within one answer; measure before gating on a threshold.

**The surfaces.**

- **In a run:** the `Decision` tool, granted by listing it in an agent's `tools:`. An agent's own `decision: {default, models}` block can narrow which models it may ask; it can only narrow, and it survives create, fork and reload. `Context op=help topic=Decision` gives the format with a worked request and answer for each question type.
- **HTTP:** `POST /v1/_decide`, and `GET /v1/_decide/models` for the allowed models and their limits.
- **gRPC:** `Decide` and `ListDecisionModels`. State, criteria and answers travel as JSON bytes, so numbers arrive unchanged.
- **MCP:** the `decision` tool.
- **Clients:** `decide()` / `listDecisionModels()` in `@loomcycle/client`, `decide()` / `list_decision_models()` in the Python client.
- The three run-less surfaces need the `runs:create` scope.
- **OpenAPI:** both HTTP routes are in the published contract (`/v1/openapi.yaml`, `/v1/docs`), which now covers memory, documents and decision models. Drift tests tie it to the tool's input and the handler's error codes.

**Errors** carry a code a caller can branch on: `invalid_input`, `bad_question`, `bad_options`, `too_many_questions`, `model_not_allowed`, `prompt_too_large`, `model_not_found`, `operator_key_restricted`, `token_limit_exceeded`, `decision_not_configured`, `timeout`, `call_failed`. The TypeScript client exposes it as `e.code` and the Python client as `e.reason`, on every endpoint, not only this one.

- A refusal is worded for its caller: one asked outside a run no longer speaks of "this agent" or "this run".
- **TypeScript error classes.** A 429 `token_limit_exceeded` now throws `TokenLimitExceededError`, with the budget on `e.scope`, `e.scopeId`, `e.used`, `e.limit` and `e.window`. It is not transient, unlike a plain `BackpressureError`, which it extends so that existing `instanceof BackpressureError` checks still match: test for it first. A 413 `prompt_too_large` throws `PromptTooLargeError`, and any other non-snapshot 413 throws `RequestTooLargeError`; both are still `instanceof SnapshotTooLargeError`.
- ⚠️ `e.name`, and the `meta_reason` of the client's `stream_close` frame, now read the more specific class name for these responses. Match with `instanceof` or `e.code`, not on the name.

**Who pays.**

- Inside a run, the call's input and output tokens are charged to the run and count against its budgets.
- Outside a run, the call is charged to the caller's own tenant and subject, on a usage row with no run. It counts against the operator, tenant and user budgets and shows in `GET /v1/_usage`.
- A caller at a hard budget is refused before the model is asked.
- With `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` on, a caller without `providers:operator-key` is served only on its own stored key for the provider.

### What kind of model an alias names: `kind` (#1691)

A `models:` alias can declare `kind: chat` (the default), `kind: decision` or `kind: embedder`.

- A tagged alias written where another kind is needed **fails config load**. The error names the alias, its kind and the place. Before, the failure surfaced as a provider error on the first call.
- ⚠️ An **untagged** alias used as an embedder, in a decision reranker or in the `decision:` block still loads, with a warning at boot and in `loomcycle validate`. A later release will make this a failure: add `kind: embedder` or `kind: decision` now.
- An untagged alias used as a chat model says nothing.
- `GET /v1/_models` reports a tagged alias's kind. The kind is never a routing input.

### A member token is held to its scopes: MCP sessions and team runs (#1695, #1703)

`POST /v1/_mcp` admits any non-isolated member token, and the per-tool gate did not look at the token's scopes. A token granted only `runs:read` could start, steer and cancel runs over MCP, and a token with no channel scope could publish and read channels; the same token was refused over HTTP and gRPC.

Each run and channel tool now needs the scope its gRPC twin needs:

| Tools | Scope |
|---|---|
| `spawn_run`, `spawn_runs`, `cancel_run`, `compact_run`, `retune_run`, `review_run`, `configured_run`, `interruption_resolve`, `decision` | `runs:create` |
| `get_run`, `list_runs`, `stream_user_run_states` | `runs:read` |
| `publish_channel`, `ack_channel` | `channel:publish` |
| `subscribe_channel`, `peek_channel` | `channel:read` |

- ⚠️ A member token that was starting runs over MCP without `runs:create` is now refused. The refusal names the scope it lacks.
- Stdio, admin, legacy and `substrate:tenant` sessions are unchanged.

**Team runs (#1703).** The same kind of token could start a team run through the `teamdef` definition tool, over `POST /v1/_teamdef` and the MCP `teamdef` tool: a member is admitted to both for authoring, and `op=run` rode in on that. gRPC was not affected.

- On those two surfaces `run` and `cancel` now need `runs:create`, and `poll` needs `runs:read`. The refusal is a 403 naming the scope (MCP: a forbidden error), before any walk is admitted.
- ⚠️ A member token without `runs:create` that was starting team runs this way is now refused. Tokens minted with the default member grants carry both run scopes and are unaffected.
- Authoring operations, an agent using the tool inside a run, and admin, legacy, `substrate:tenant` and open-mode callers are unchanged.

### The `local` preset no longer wipes the built-in providers (#1701)

The embedded `local` preset carried a `providers:` key with only comments under it. That is an empty value, and a later config layer's empty value replaces what an earlier layer set: `LOOMCYCLE_PRESETS=base,local` booted with no providers at all, `ollama-local` included, and `local` alone failed to boot under `LOOMCYCLE_CONFIG_STRICT=1`.

- The key is now commented out with its examples, and the `ollama-local` example names its driver, so copying it into your own config loads.
- ⚠️ **`base,local` gets the nine built-in providers back.** A cloud provider whose key is in the environment becomes enabled again, and `base`'s tiers list cloud candidates behind the local ones, so such a deployment can fall back to a cloud provider and be billed by it. This is what the preset's header always described ("cloud providers remain as fallback").
- `local` alone routes to `ollama-local` only; the built-in cloud providers are declared again, and only an agent that pins one reaches them.
- The preset now ships a `local-decide` alias (`ollama-local` / `nimble`, `kind: decision`) and a commented `decision:` block. Add `decision: { default: local-decide }` to your own config to turn decision models on.
- The merge rule is unchanged: an empty key in any layer still replaces. The Configuration guide now says so (§9e).

### A team definition's keys are read exactly as written (#1693)

A team definition was decoded leniently: a key matched a field with its case ignored, the last of two keys for one field won, and a key no field takes was dropped. A team could be saved that was not the one that was read. A create or fork is now refused when the overlay has a repeated key, a key that names a field only when its case is ignored, two spellings of one field, or a key no field takes; `verify` lists each one with its path.

### Smaller changes

- The decision reranker now retries with shorter candidates when Ollama refuses a request over its 64 KiB cap; before, it kept the search's own order (#1691).
- The per-run cost summary, asked for an empty run id, no longer sums every run-less usage row (#1694).
- A flaky driver test is fixed (#1696).

### Upgrade notes

- **No schema migration.**
- **Additive on the wire:** two HTTP routes, two RPCs, one MCP tool, one builtin tool, `kind` on `/v1/_models`, `decision` on the AgentDef overlay.
- **Config:** tag embedder and decision aliases with their `kind` to clear the new warning.
- **Member tokens:** check that tokens used over MCP, or to start team runs through `teamdef`, carry the scopes for what they call.
- **`local` preset:** a deployment on `base,local` with cloud keys in its environment regains cloud fallback. Unset the keys, or set `LOOMCYCLE_NO_DEFAULT_PROVIDERS=1` and declare only the providers you want, to stay local-only.
- **Clients:** `@loomcycle/client` 1.107.0 and the Python client 1.107.0 add the decision methods and the error code field; the TypeScript client adds `TokenLimitExceededError`, `PromptTooLargeError` and `RequestTooLargeError`.

## What's in v1.106.0

*A caller can now make starting a run safe to retry with an **`idempotency_key`**: a second request with the same key starts nothing and is answered with the first run. Any run can be given a lifetime limit of its own with **`max_wall_seconds`**, a detached run included, and ends `cancelled` with stop reason `wall_limit` when it runs out. A program that spawns a sub-agent can ask for the child's outcome as an **object** — status, answer and token usage — instead of text behind a header line. A blocking spawn now reports a run cancelled from outside as cancelled, where it said completed.*

A minor release: #1685 to #1689.
- #1686 and #1687 are the idempotency key;
- #1688 is the lifetime limit;
- #1689 is the object result;
- #1685 is a test fix.

Together with the untrusted-text changes in v1.105.0 (#1680, #1682), this is the set a code-js orchestrator that dispatches one run per unit of work asked for.

### Retrying a start safely: `idempotency_key` (#1686, #1687)

A caller that retried after a lost response — a timeout, a restart between the call and its answer — used to start a second run for the same work. Every surface that starts a fresh run now takes `idempotency_key` (1 to 200 characters of `[A-Za-z0-9:._-]`): `POST /v1/runs`, `POST /v1/runs:batch`, MCP `spawn_run` and `spawn_runs`, gRPC `Run` and `SpawnRunBatch`.

A second request with the same key, from the same tenant and user, starts nothing. It is answered with the run the first request started, marked `deduplicated`.

- **Detached batch child:** the existing run's ids and its current status.
- **Joined batch child and `spawn_run`:** the call waits for the existing run and returns its result, read from the run's row. The text is the stored (redacted) answer and `limits` is absent.
- **`POST /v1/runs` and gRPC `Run`:** the response is the existing run's stream: the `session` and `agent` frames a fresh run sends, the `agent` frame carrying `"deduplicated": true`, then the run's events from its first, ending when the run does. Closing the stream does not stop the run.
- **A caller joined to a run it did not start never cancels it.** Its timeout ends its own wait, and the run is reported as `running`. The MCP transport timeout follows the same rule.
- **A run waiting for a person** — parked for input, or held for review — is returned at once as `running`.
- **No slot, no session.** The key is looked up before admission. Two first requests at the same moment still make one run: the unique index refuses the second, which is answered with the first.
- **Scope.** The key is stored as `run:<tenant>:<user>:<key>`, so a caller's key cannot equal a webhook's or a schedule's, and one user of a tenant is never answered with another user's run. The lookup also applies the ownership check every other read of a run's content applies.
- **Refused:** a key on a continuation (`session_id`), on a configured run (`start: false`), and on two children of one batch.
- **Not compared:** the request body. The same key with a different prompt returns the first run, so build the key from what makes the work distinct.
- **Lifetime.** A key lives as long as its run's session is retained.
- **Snapshots.** A paused run's `idempotency_key` and `delivery_alt_key` are now captured and restored. Before, a run restored on another instance held no key, so the retry, redelivered webhook or re-fired schedule slot that followed started the work again. When the target already has a run holding the key, the paused run is restored without it and the restore warns.

### A lifetime limit that belongs to the run: `max_wall_seconds` (#1688)

A model-driven run had no time bound of its own, only its iteration limit and token budgets, and a detached batch child could not be given one: `timeout_ms` bounds a call, and a detached run has none. `max_wall_seconds` is accepted wherever `idempotency_key` is, for any agent, model-driven or code-js. 0 or absent means no bound; the ceiling is 30 days.

- **End state.** Past the limit the run is cancelled, with everything it started, and ends `cancelled` with `stop_reason` `wall_limit`.
- **What counts.** Waits count, an interactive run's time parked for input included. Time the runtime is paused does not.
- **Restarts.** The limit is part of the run's `spec`. A resumed run is charged the lifetime it had used; one resumed with no recorded clock is charged its whole age since it started.
- **Only this run.** The limit cancels the run it belongs to, not whichever run holds its agent id later, and never a run on another replica.
- **Backstop.** If the run is still going five seconds after its limit, the loop cancels it directly and it ends `failed`.
- **Children** take no limit of their own from it; they end because their parent's cancel reaches them.
- **Reading it.** `Context op=self` reports `run_budget.wall_limit_ms` and `wall_elapsed_ms`.
- **Detach.** A detached batch still refuses `timeout_ms`; the refusal names `max_wall_seconds`.

A code-js run's `run_timeout_seconds` is a different bound (active time only, waits excluded), and a code-js run that outlives `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` still ends `failed` with `code_agent_wall_limit`. Not on the agent definition, a continuation, or a schedule or webhook definition.

### A blocking spawn reports how its run ended (#1688)

`spawn_run` and a joined batch child worked a run's status out from what the call saw: an error, or the run's events. A run ended from outside — by `cancel_run`, or by its own limit — leaves neither, and was reported `completed`. The result now takes the run's recorded end: `status`, `stop_reason` and `error` are the row's.

### The child's outcome as an object: `result: "object"` (#1689)

`Agent op=spawn` returns the child's answer as text behind a `[sub-agent agent_id=… run_id=…]` line, so a parent model can attribute it. A parent program had to strip that line to parse the answer and look the child's run up afterwards to learn what it used. With `result: "object"`, on `spawn` and on each `parallel_spawn` entry (or on the call, as the entries' default), the result is

`{agent_id, run_id, status, stop_reason?, final_text?, structured?, state?, usage, error?, truncated?, structured_omitted?, state_omitted?}`

as JSON, which a code-js program receives parsed.

- **`final_text`** is the answer with no header line. `structured` is the answer parsed against the child's `output_format`; `state` is a stateful child's final state.
- **`usage`** is `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `model` and `provider`. Which key paid and what it cost are not in it.
- **A child that ran does not fail the call.** `status` is `completed`, `failed`, `cancelled`, `rejected`, `timeout` or `max_iterations`; anything but `completed` comes with `error` and its `usage`. A child that never started is still an error.
- **Hooks.** What a `subagent_stop` hook adds to an accepted answer is kept; a child whose answer a hook refused reports `rejected` and no answer.
- **Size.** The object takes at most a quarter of the parent's window, like a text result.
- **Fan-out.** A row whose entry asked for the object carries it in `result`, in place of `output` and `state`.
- **Refused** with `mode: "poll"`, and on `open` and `send`.

Without `result` the text result is byte-identical. The code-js ABI is 1.2.0.

### Other fixes

- **Tests (#1685, #1687).** A resident-child test waits for a running turn's partial output instead of assuming it, and the idempotency test that hides one lookup no longer swaps the server's store under a live run.

### Upgrade notes

- **Migrations:** none.
- **Proto:** additive. `RunRequest` gains `idempotency_key` (38) and `max_wall_seconds` (39); `SpawnResult` gains `deduplicated` (9).
- **New environment variables:** none.
- **Blocking spawns:** a caller of `spawn_run` or a joined `spawn_runs` / `POST /v1/runs:batch` that treated every `completed` result as a finished answer now sees `cancelled` for a run that was cancelled while it waited. The partial text is still in `final_text`.
- **Snapshots:** the paused-run entry gains `idempotency_key` and `delivery_alt_key`. An older snapshot restores as before, with no keys. Restoring a newer snapshot onto a store where another run already holds a key adds a warning and restores the run without it.
- **`Context op=self`:** a model-driven run started with `max_wall_seconds` now reports a `run_budget` object, with `wall_limit_ms` and `wall_elapsed_ms` only. A reader that assumed `run_budget` always carries `budget_ms` must check for it.
- **Adapters:** TS and Python are 1.106.0. TS gains `RunOptions.idempotencyKey` and `maxWallSeconds`, `deduplicated` on the `agent` frame and on `SpawnRunResult`, and `RunSpec.max_wall_seconds`. Python gains `idempotency_key` and `max_wall_seconds` on `run_streaming` and on a batch child, `RunHandle.deduplicated`, and `deduplicated` on a batch result.
- **Not built:** a default `max_wall_seconds` on the agent definition; an idempotency key on a continuation; the object result for a fan-out child that had already finished when its parent was restored from a snapshot (it comes back as text); `untrusted` on `Agent op=send`.

## What's in v1.105.0

*An agent can now start sub-agents and team walks in **poll mode**: the call returns at once, the children run in the background, the parent keeps working, is told when they finish and collects their results with `poll`; they survive a pause, a restart and a move to another instance. A child can be bounded with `timeout_ms`, and a child that stops at its iteration limit now reaches its caller as an error with its last answer kept. Scheduled runs are no longer capped at 10 minutes, every replica can run the scheduler, and a schedule says what an overlapping slot does and how many missed slots it makes up. Also: a team can be checked before it is saved, a cancel ends a run whose replica is gone, a parent can hand a sub-agent untrusted text, and the memory rerank can reorder recall.*

A minor release: #1629 to #1683 (every number in that range).
- #1635 to #1639, #1642, #1646, #1648 to #1650 and #1657 are poll mode, bounded sub-agents and the code-js run clock;
- #1651 to #1656, #1658 to #1663, #1665, #1666, #1669 to #1672, #1674 to #1676, #1679, #1681 and #1683 are the review of that line: capped children, pauses, resume, resident children and cancels;
- #1664, #1667, #1668, #1673, #1677 and #1678 are the scheduler;
- #1640, #1641, #1643 to #1645 and #1647 are teams; #1680 and #1682 are untrusted text;
- #1630 and #1633 are the memory rerank; #1631 is webhook dedup; #1629, #1632 and #1634 are benchmarks.

### Sub-agents in poll mode (#1638, #1642, #1646, #1650, #1657)

`Agent op=spawn` and `parallel_spawn` take `mode: "poll"`. The call returns as soon as the children are admitted: `spawn` answers `{child_run_id, agent, state: "running"}` and `parallel_spawn` answers `{batch_id, children: [{index, agent, child_run_id, state}]}`, where a child still waiting for a slot is `queued`. Validation, depth, admission and authority are the same as for a waited-for spawn, which is unchanged.

- **Notes.** When a child finishes, the parent's next turn carries a note naming it and the call that reads it (`notify`, default true; several endings share one note; a child already read is not noted).
- **`Agent op=poll`.** `child_run_ids`, a `batch_id`, or neither (every background child not yet read; each result is handed over once). `wait` is `none`, `any` or `all` with `wait_ms`, cut to `LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS`. The answer is `{children: [{child_run_id, agent, state, output?, error?, structured?, status?, truncated?}], pending}`. A poll by id can be repeated. A child held for review reads `held`.
- **`Agent op=cancel`** with `child_run_ids` ends background children (one of a batch leaves the rest running), waits up to 15 s and reports each final state.
- **A parent does not end over its children.** `on_parent_end` is `wait` (default) or `cancel`. With `wait`, a parent that ends its turn while children are queued, running or held parks: it emits `awaiting_children`, makes no model call, spends no iteration, and wakes once, when every child has ended, with a note listing each final state. Run reads and the run-state stream show `awaited_state: "children"` with the child ids in `awaited_on`. Cancelling a parked parent cancels its children. With `cancel`, the children are cancelled when the parent ends its turn.
- **Refusals.** An id this run did not start gets `not a child of this run`, whether it is unknown or another run's. Poll mode is refused on a run's last iteration and in a stateful run. Children still running when a parent ends its last iteration are cancelled, and an error event names them.
- **Team walks (#1642, #1646).** `TeamDef op=run` takes `mode: "poll"` and returns `{name, def_id, run_id, state: "running"}`. The walk is a background child of the calling run: it holds one live-children slot, takes `notify` and `on_parent_end`, and is cancelled when the parent run ends. `TeamDef op=poll` (`run_ids`, `wait`, `wait_ms`) returns the answer a waited-for run returns, and `TeamDef op=cancel` (`run_ids` required) cancels poll-mode walks and their members. Agent `poll` and `cancel` also accept a walk's run id (the row has `kind: "team"`). Synchronous and `mode: "detach"` runs are unchanged.
- **External batches can detach (#1650).** `POST /v1/runs:batch`, MCP `spawn_runs` and gRPC `SpawnRunBatch` accept `mode: "detach"`, which they used to refuse. The call returns once every child has started (`status: "running"` with `run_id`, `agent_id` and `session_id`) or been refused in its slot. The runs go on under the caller's identity, hold their admission slots until they end, and are read with `GET /v1/runs/{run_id}` and stopped by agent id. A caller that leaves while a child still waits for admission withdraws that child.
- **Transports and Web UI (#1650, #1657).** The gRPC `Event` gains `awaiting_children` and `children_note` (with the child run ids), and Python exposes both. The Web UI shows a "waiting for N children" pill on a parked parent and names each child in the run detail, and shows a code agent's budget.

### Bounding sub-agents (#1635, #1636, #1654, #1655, #1663)

- **`timeout_ms` on `spawn` and `parallel_spawn` (#1636),** per entry on `parallel_spawn` with the call's value as the default. When it runs out the child and everything below it are cancelled; `spawn` returns an error naming the timeout and the child's run, and a `parallel_spawn` row reads `ok: false`, `status: "timeout"`. Time the child is held for review is not counted, and neither is time the runtime is paused (#1666). `LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS` is an operator ceiling; a larger value is refused, not clamped.
- **A closing turn (#1636).** When the last turn `max_iterations` allows calls tools, the tools run and the model is called once more with their results and tools disabled, so the results are no longer thrown away. Its text is the run's answer and `stop_reason` stays `max_iterations`. The answer goes through `agent_stop` hooks and review like any other (#1655). Iteration-unbounded and stateful runs get no closing turn.
- **A child that stops at its iteration limit is an error to its caller (#1655, #1663).** The child's own run is unchanged (`completed`, stop reason `max_iterations`). Its caller is told it did not finish, with the answer kept: `spawn` returns an error result, `sub-agent "X" stopped at its iteration limit of N before it finished, so its last answer may be incomplete (run r_…). Its last answer:` followed by the answer; a `parallel_spawn` row reads `ok: false`, `status: "max_iterations"`, the answer in `output`; a `poll` row reads `state: "failed"` with the same `status`. The same holds for a resident child's turn, and for a team walk's member: an `agent` or `consolidator` state fails the walk, a `parallel` state or Starter wave reports the member `ok: false` and does not count it toward `wait`, and a Starter's sink message is `status: "error"` with the answer kept. `subagent_stop` hooks see `status: failed`. A top-level run (`POST /v1/runs`, MCP `spawn_run`) is not affected.
- **A live-children limit (#1636).** `LOOMCYCLE_MAX_LIVE_CHILDREN_PER_RUN` (default 32) caps the children one run has alive at once: spawned, queued, background and resident children, and poll-mode walks. A `parallel_spawn` past it is refused whole, before any child starts. The limit is per run, not per tree.
- **Results fit the parent's window (#1636, #1654).** A `spawn` answer is cut to a quarter of the parent's window with a note naming the child's run; `parallel_spawn` and `poll` rows share that quarter and say `truncated: true`. A row's error is cut first; a child's final state is kept whole or left out (`state_omitted` / `structured_omitted` with its size), never cut. Auto-compaction now counts a turn's tool results before they are sent.
- **Also (#1635).** `parallel_spawn` rows and the spawn result header carry the child's `run_id`. `Context op=self` reports `iterations_used`, `iterations_remaining` and `iterations_unbounded`. `Agent op=open` takes `timeout_ms` with `send`'s meaning: past it, `open` returns `state: "running"` with the partial output and the parent collects the rest with `poll`. A resident child running a turn is no longer reaped as idle; a turn longer than `LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS` is, and the parent's next call is told why.

### code-js: waiting does not spend the run budget (#1637, #1653, #1660)

A code agent's budget (`run_timeout_seconds` / `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS`) is now spent only by active time. Time blocked in an Agent call, a synchronous team run, a Channel `subscribe` or `await`, an `Interruption ask`, a review hold or a runtime pause does not count. `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` (default 86400; operator-only, no unlimited setting) bounds the run's lifetime, waits included and runtime pauses excluded; a run that reaches it fails with `code_agent_wall_limit`. A pause and a snapshot keep the clock, so a resume does not reset either limit. `Context op=self` reports `run_budget`. Model-driven agents are unchanged.

### Pause, restart and other instances (#1649, #1653, #1658, #1661, #1665, #1666, #1672)

- **Background children are durable (#1649, #1658).** A parent resumed after a snapshot restore or at boot gets its poll-mode children back from its transcript: results already read stay read, a child still running is read from its run wherever it runs, and a parent that was parked for its children parks again and wakes when the last one ends. Notes replay. A resident child is rebuilt with its parent. A child's `timeout_ms` is armed again from where it stood.
- **Team walks do not move (#1658, #1665).** A walk lives on the instance that started it. A walk still running when its instance stopped is ended `failed` with that reason, and its paused members are cancelled instead of resumed. A paused sub-run whose direct parent has ended is ended `cancelled` instead of resumed.
- **A runtime pause does not count against `timeout_ms` (#1666, #1672).** Every `timeout_ms` clock (an Agent child, a team state, a Starter run) stops from the moment the runtime is paused until it resumes, live and when re-armed after a restart or restore. Each run records the pauses it lived through as `spec.pauses` (`[{since, until?}]`) on `GET /v1/runs/{id}`; past 16 entries the oldest are folded into `spec.pauses_folded`.
- **Waiting runs respect a pause (#1653).** A poll with `wait` returns when the runtime pauses, so its parent can park and a snapshot carries it. Resident children are not reaped while the runtime is paused.
- **Cleanup waits for the tree (#1661).** A run tree's ephemeral volumes and run-scope SQL database are purged only after its background children, resident children and poll-mode walks have ended.

### Resident sub-agents (#1651, #1656, #1671, #1676, #1679)

- **Who may address one (#1651, #1656).** `Agent op=send`, `poll`, `cancel` and `close` by `child_run_id` reach a resident child from any run in its tenant; an isolated run (a `substrate:user` principal's) reaches only its own user's children. Any other caller gets the same not-found as an unknown id.
- **An ended child's last answer stays readable (#1671).** After a resident child's run ends (at its iteration limit, on a failure, or closed), `poll` and `cancel` still hand back its ending and last answer, and `send` says the child has ended. A closed child says who closed it.
- **From any replica (#1676, #1679).** A resident child held by another replica is read from the store and reached through the existing steer and cancel routes, where it used to answer "not found" (and `close` answered success while the child kept running). A poll from another replica counts as use, so the child is not reaped as idle. A wait on a child whose replica has died ends with that reason.

### Cancelling runs nothing holds (#1675, #1679, #1681, #1683)

A run's row reads `running` until something ends it. After a crash, a dead replica or a pause never resumed, a cancel found nothing to stop and left the row.

- **A cancel now ends such a row (#1679):** `cancelled`, with the caller's reason as its stop reason, its pause cleared, its state published, and the unheld runs below it ended with it, including a team walk's members (#1683). This applies to the run-id cancel, the agent cancel on HTTP, gRPC and MCP, a resident `close`, a parent's `timeout_ms` on a paused child, and a schedule's `replace`. A row is left alone when a loop in this process holds it, when its replica is recorded alive or its liveness cannot be read, or when it started or heartbeated in the last 10 s.
- **In a cluster (#1681).** A cancel for a run whose owner is recorded dead ends it the same way and answers `reason: "owner_dead_cancelled"` with the agents it cascaded to. It used to mark the run `failed` / `owner_replica_dead` and answer `owner_dead_marked_failed`.
- **`run_end` hooks fire (#1683)** for a run ended this way and for one the resume pass cancels as orphaned, from the hooks the run pinned at start.
- **A team walk can be cancelled from another replica (#1675).** A run-id cancel of a walk routed to the replica running it used to do nothing and answer 404 after the ack timeout.
- **The recorded iteration limit (#1679).** A run's `spec.iteration_limit` records its limit at start (0 when unbounded).

### Scheduled runs (#1664, #1667, #1668, #1673, #1677, #1678)

- **Every replica can run the scheduler (#1664).** Each slot is claimed once in the database before it fires, so a slot fires once however many replicas tick. Before, every replica with the scheduler enabled fired every slot.
- **A scheduled run outlives its tick (#1667).** The tick only starts the run; the run is no longer cut at 10 minutes, and a long run no longer delays other schedules. Each started run is tracked and finished once, by the replica that ran it or by any replica if that one died, so `on_complete` hooks fire exactly once. `last_status` reads `running` while a run is in flight; a team schedule records how its walk ended, not `completed` when it starts; a run whose loop fails is recorded `failed` and fires no hooks (it used to be recorded `completed`). A one-shot schedule is retired when its run finishes.
- **`concurrency_policy` (#1668, #1678)** says what a slot does when the schedule's previous run is still going, for agent and team schedules: `forbid` (default) starts nothing and records `last_status: skipped_overlap`, which does not count toward `max_fires`; `allow` starts another run; `replace` cancels the running one (it ends `cancelled`, with no hooks) and starts its own, on any replica. `replace` never starts over a run it could not stop. The field is refused on `delivery: channel`.
- **`catch_up_max` (#1673)** was stored but never read. `0` (default) collapses the slots missed in an outage into one fire and records the number dropped as `missed_slots`. `N` (at most 1000) runs the newest N in slot order; each catch-up agent run carries metadata `slot_at` and `catch_up: true`. Under `forbid` or `replace` the backlog runs one at a time; under `allow` up to N run at once. A gap of more than 100,000 slots collapses into one fire. A disabled schedule, a `channel` tick and a consolidation sweep never catch up.
- **What a schedule is doing (#1677).** `GET /v1/_schedules/{def_id}/state` and `ScheduleDef op=get` (`run_state`) add `active_runs`, `fire_count`, the last claim, `finished_at`, `catch_up_until` and `missed_slots`. The Web UI Schedules page shows them with a "Running now" list, sets both fields on create and fork, and has status chips for `running`, `skipped_overlap` and `cancelled`.

### Teams (#1640, #1641, #1643 to #1647)

- **Check a team before saving it (#1641, #1644, #1645).** `TeamDef op=verify` with an `overlay` builds what a save would store, runs every create or fork gate and returns every problem at once, writing nothing. Each issue has a `severity` (`refused`, `unrunnable` or `advisory`) and the JSON `path` of the value at fault; the answer says `valid` and `runnable`. It checks a fork when the name has an active version, otherwise a create (`as` overrides, `parent_def_id` pins the parent). The Web UI Teams editor has a Check button that sends exactly what Save would.
- **A detached walk outlives its starter (#1643, #1647).** A walk an agent started with `mode: "detach"` was cancelled with that agent, and the starter tree's ephemeral volumes and run-scope SQL database were purged under it. The walk now runs until it ends or its run id is cancelled, and that state is kept until the last detached walk on the tree ends.
- **Fork (#1640).** A fork that sends `max_iterations: 0` or `null` clears the parent's cap; an absent key keeps it.
- **Strict arguments in a run (#1646).** A `TeamDef` call made inside an agent's run with an argument its op does not take is refused; it used to be dropped silently.

### Untrusted text (#1680, #1682)

- **A parent can hand a sub-agent untrusted text (#1682).** `spawn`, each `parallel_spawn` entry and `open` take `untrusted: [{text, kind?}]`. Each entry is fenced in the child's first user turn, after the prompt, so the child reads it as data; before, a parent could pass only `prompt`, which the child reads as instruction. The text is not template-expanded, and a child's own spawns never receive it. `kind` is one of `untrusted`, `user_input`, `web_content`, `tool_output`, `search_result`, `uploaded_cv`, `qa_question`. At most 64 entries and 1 MiB per child. `send` refuses the field.
- **An untrusted block cannot close its own fence (#1680).** The body was escaped by replacing `<` only, so `&lt;/untrusted>`, a fullwidth `＜`, a zero-width character inside the tag or an escape such as `\x3c` still read to a model as the closing tag. Invisible characters are now dropped and every spelling of `<` becomes `‹`.

### Memory and webhooks (#1630, #1631, #1633)

- **The rerank can reorder memory recall (#1630).** `memory.reranker.sources` (operator-wide; `documents`, `facts`, `notes`, `traces`) lists what the rerank may reorder for `search` and `recall`. Unset means `[documents]`, as before. On LoCoMo memory, reranking recall lifted recall@5 from 0.588 to 0.74.
- **The listwise rerank's default pool is 40 candidates (#1633),** up from 20; the decision reranker stays at 20, and an agent that sets `candidates` is unchanged.
- **Channel webhooks are deduplicated across replicas and restarts (#1631).** A `delivery: channel` WebhookDef was deduplicated only by a per-process 10-minute cache. Accepted deliveries' keys are now claimed in the database before the publish and held for 24 hours, as for a team's own webhooks; a duplicate answers `200` with `deduped`.

### Other fixes

- **`Interruption ask` (#1674).** An answer, decline or cancel that landed between the ask's creation and the start of its wait was missed, and the ask slept until its poll backstop noticed, 15 s by default.
- **SSE (#1652).** A stream's keepalive could write after its handler had returned, into a response already handed back to the server. Every stream now ends its keepalive before the handler returns.
- **Volumes (#1659).** A `VolumeDef op=create` that fails or is cancelled no longer leaves a directory with no row.
- **MCP `teamdef` (#1654).** The tool no longer offers `poll`, `cancel` or `mode: "poll"`, which need a calling run and always failed over MCP; `mode: "detach"` is the route there.
- **Pause (#1635, #1650).** The unused per-tool pause policy is removed and the docs say what a pause has always done: a call in flight runs to completion and the run parks at its next iteration boundary.
- **Tests, docs and benchmarks.** #1639, #1648, #1662 and #1670 are test fixes; #1669 updates the architecture guide; #1629, #1632 and #1634 are benchmark runs with no runtime change.

### Upgrade notes

- **Migrations:** 0094 to 0097 on Postgres, with the same changes on sqlite, applied at boot. 0094 and 0096 add columns to `schedule_run_state`, 0095 adds the `schedule_active_runs` table, and 0097 adds a partial index on `runs(parent_run_id)`. The index is built in the migration's transaction, not concurrently, so on a large `runs` table the first boot holds writes to it while the index builds.
- **Proto:** additive. `Event` gains `awaiting_children` (19) and `children_note` (20).
- **New environment variables:**

  | Variable | Default | Meaning |
  |---|---|---|
  | `LOOMCYCLE_MAX_LIVE_CHILDREN_PER_RUN` | 32 | Children one run may have alive at once. 0 or unset means the default. |
  | `LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS` | 0 (no ceiling) | Largest `timeout_ms` an agent may give a child. A larger value is refused. Never a default. |
  | `LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS` | 60000 | Longest one Agent or TeamDef `poll` with `wait` blocks. A larger `wait_ms` is cut to it. |
  | `LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS` | 7200 | Longest turn of a resident child before it is reaped. No unlimited setting. |
  | `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` | 86400 | Lifetime of a code-js run, waits included, runtime pauses excluded. No unlimited setting. |
- **Removed:** `LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS` is no longer read. A scheduled run has no wall clock; it is bounded like any other run (iteration limit, token budgets, cancel). The consolidation sweep keeps a fixed 10-minute budget per sweep, so raising the variable to give slow local-model passes more time no longer works.
- **Capped children are errors:** a sub-agent, resident turn or team-walk member that stops at its iteration limit used to reach its caller as a success. It is now an error result (`ok: false` / `state: "failed"`, `status: "max_iterations"`) with the answer kept beside it, and a walk whose `agent` or `consolidator` member is capped fails. Raise the child's `max_iterations`, or read `output` from the failed row. The child's own run row still reads `completed` / `max_iterations`.
- **Closing turn:** a run whose last allowed iteration calls tools makes one more, billed model call. A consumer that treated a `max_iterations` run as having no answer now sees one.
- **Fan-out width:** a run is refused past 32 live children by default, with the number alive and the limit named. Raise `LOOMCYCLE_MAX_LIVE_CHILDREN_PER_RUN` if a workload relies on more.
- **Child answers are cut** to a quarter of the parent's window (shared across the rows of one `parallel_spawn` or `poll`), and a final state that does not fit is left out with `state_omitted` / `structured_omitted`. The whole answer stays on the child's run.
- **Spawn header:** a spawn result's header is now `[sub-agent agent_id=a_… run_id=r_…]`. A reader that expects exactly `[sub-agent agent_id=…]` must accept the extra field.
- **`timeout_ms`:** time the runtime is paused no longer counts against it, on any `timeout_ms` clock.
- **code-js:** a run that only waits is now stopped by the 24-hour wall limit (`code_agent_wall_limit`), not its budget, and one that waited on children or channels has more working time than before. Only the operator variable moves the wall limit.
- **Pause:** `force_cancelled_count` stays on every transport and is always 0, as it always was in production.
- **Resident children:** an isolated run can no longer address another user's resident child by id. `close` of a child that no process holds now ends its row; where its replica is alive but does not answer, `close` returns an error where it used to return success.
- **Cancels:** a cancel aimed at a run whose loop is gone now ends the row `cancelled`, where it answered "no in-flight run" or "already ended" and left it. In a cluster, the agent cancel's `reason` for a dead owner is `owner_dead_cancelled` (was `owner_dead_marked_failed`), and the run ends `cancelled` with the caller's reason (was `failed` / `owner_replica_dead`), with its unheld descendants. `owner_replica_unreachable` is unchanged. `run_end` hooks now fire for these runs.
- **Scheduler:**
  - The scheduler may be enabled on every replica; a deployment that enabled it on one replica to avoid double fires no longer needs to.
  - `concurrency_policy` defaults to `forbid`. Agent schedules already skipped an overlap; a team schedule used to start a walk over a running one and no longer does. Set `allow` to keep that.
  - `catch_up_max` defaults to 0, which is the previous behaviour (one fire after an outage). A schedule that already stored a non-zero value now runs that many missed slots.
  - `last_status` can be `running`, `skipped_overlap` and `cancelled`, and a failed run reads `failed` where it read `completed`. `on_complete` hooks no longer fire for a failed run. A team schedule's `completed` now means the walk finished.
- **Team walks:** a walk does not survive its instance; one left `running` by a restart is ended `failed` when its starter or members are next resumed. An agent-started detached walk no longer stops when its starter is cancelled (stop it by its run id), and its members' `parent_agent_id` is `team:<name>`.
- **TeamDef:** inside a run, an argument the op does not take is refused. A fork that sends `max_iterations: 0` clears the cap; omit the key to keep it. Over MCP, `teamdef` refuses `poll`, `cancel` and `mode: "poll"`.
- **Batches:** `mode: "detach"` is accepted on `POST /v1/runs:batch`, `spawn_runs` and `SpawnRunBatch`. `timeout_ms` with `detach` is refused (HTTP 400, gRPC `InvalidArgument`).
- **Untrusted blocks:** untrusted text shows `‹` where it showed `&lt;` (`a < b` reads `a ‹ b`). A session that holds an untrusted block misses the provider's prompt cache once after the upgrade. The code-js ABI is 1.1.0 for the new `untrusted` argument.
- **Channel webhooks:** a redelivery of an accepted `delivery: channel` delivery answers `200` deduped on every instance and after a restart, for 24 hours. With a body-only signature the key is the body, so an identical body re-sent within a day is not published again.
- **Memory rerank:** with the listwise rerank enabled and no `candidates` set, the rerank prompt now holds 40 candidates. `rerank_reason` can be `source_not_enabled` once `memory.reranker.sources` is set; an unknown source fails config load.
- **Run reads:** `spec` gains the optional fields `pauses`, `pauses_folded`, `iteration_limit`, `run_clock`, `spawn.resident` and `spawn.addressed_at`, and `awaited_state` can be `children`.
- **Adapters:** `@loomcycle/client` 1.105.0 adds the `awaiting_children`, `children_note` and `spawn_child_read` event types with their payloads, `"children"` on `AwaitedState`, `mode: "join" | "detach"` on `RunBatchOptions`, `"poll"` on `RunSpecTeam.mode`, the `RunSpec` fields above, the `verifyTeam(name, draft)` overload and the `TeamIssue`, `TeamIssueKind`, `TeamDraft` and `TeamDraftVerification` types. The Python package 1.105.0 adds `AgentEvent.awaiting_children` and `.children_note` (`AwaitingChildren`, `ChildrenNote`) and regenerated stubs.

## What's in v1.104.0

*A team definition can now carry its own agents, skills, channels, schedules and webhooks, and declare variables that a start sets, so a workflow is handed over as one definition. Schedules and inbound webhooks can start a team walk directly. An agent inside a run can no longer create an agent with more authority than it holds, and retired webhook, skill, memory-backend and A2A definitions are no longer served. Also: an opt-in memory reranker that asks a decision model, Web UI support for team variables and a team's own definitions, and Save new version in the Web UI now puts the saved version in force.*

A minor release: #1611 to #1627 (every number in that range).
- #1613, #1615 to #1617, #1620, #1622 and #1627 are a team's own definitions and variables; #1614 lets schedules and webhooks start a team;
- #1621 is the authoring ceiling for agents; #1611, #1616 and #1626 stop serving retired definitions;
- #1618 and #1619 are the decision-model reranker;
- #1624 and #1625 are the Web UI; #1612 is the team-name rule; #1623 is a test fix.

### A team carries its own definitions (#1615 to #1617, #1620, #1622, #1627)

A team definition may declare a `local` block with five kinds: `agents`, `skills`, `channels`, `schedules` and `webhooks`. Each entry is stored only in the team definition, is versioned and hashed with it, and is named from the definition as `./<name>`; a bare name keeps meaning a global definition, so no stored team changes meaning. A fork that sends one kind replaces that kind's whole list (`{}` declares none) and keeps the others. A team that declares none keeps its recorded content hash. Every kind is one `[A-Za-z0-9_-]` segment of at most 64 characters, works only in the team's own tenant, and cannot be declared by a team whose name has more than one segment.

- **Agents (#1615).** `local.agents` maps a name to the overlay `AgentDef op=create` takes. A team's own agent runs as `<team>/<name>`, appears in no agent list, and cannot be started outside a walk of its team: a run, schedule, webhook or A2A call on that name finds no agent. A session that ran inside the team can be continued on the team version it started under, until that version is retired or deleted. In a state, `./x` is the team's own agent and a bare `x` the global one. In the Agent tool, for every run below the walk (global agents included), a bare name resolves to the team's own agent when the team declares one, otherwise to the global one. Each agent passes every gate `AgentDef op=create` applies, under its full name, at create and at fork (a fork re-gates the agents it inherits against the forker). The full name must be free: a team cannot declare one while a global agent of that name exists, that agent cannot be created while the team declares it, and if both come to exist the walk refuses to start the team's agent. At most 64 per team.
- **Skills (#1616).** `local.skills` maps a name to `{body, description, tools}`. Only a team's own agent that lists the exact `./<name>` in its `skills` can load one, as `./<name>`; no pattern grants a team skill, and `./*` is refused as a grant. Each skill passes SkillDef create's gates, and its tools must be within those of every agent granted it. At most 64 per team.
- **Channels (#1617).** `local.channels` maps a name to a channel definition's fields; scope is `tenant` or `user`. The team publishes to and reads its own channels without an ACL entry, and its own agents may use them through the Channel tool by listing `./<name>` in their `channels`. Nothing outside the team reaches them, and no channel list shows them. They are stored under the reserved prefix `_team/<team>/<name>`. Deleting the team drops their messages and cursors; retiring keeps them. At most 64 per team.
- **Schedules (#1620).** `local.schedules` maps a name to `{schedule, channel, payload?}`: a cron expression or descriptor, at most every 10 seconds, publishing into one of the team's own channels. A schedule ticks only while a walk of the team runs: it is armed when the walk starts and disarmed before the walk's run is recorded as over, however it ends. Nothing is stored for it. No tick is published while the runtime is paused, and skipped ticks are not made up. At most 16 per team.
- **Webhooks (#1622, #1627).** `local.webhooks` maps a name to `{auth, channel, payload_mapping?}`. While a walk of the team runs, `POST /v1/_teams/{tenant}/{team}/webhooks/{name}` (or `/v1/_teams/{team}/webhooks/{name}` in the shared tenant) publishes each authenticated delivery's raw body, once, into one of the team's own channels. `auth` is a WebhookDef's `auth`, checked by the same rules; `payload_mapping` may map only `user_id`, which a user-scoped channel requires. With no walk running, or for a retired version, the route answers the same `404 {"error":"unknown_webhook"}` an unknown WebhookDef gets. Declaring one takes the authority to create a WebhookDef, which only the operator planes (HTTP, MCP, gRPC) hold, so an agent inside a run cannot create or fork a team that has one. A walk that declares webhooks is refused when `LOOMCYCLE_WEBHOOKS_ENABLED` is off. Any instance on the shared database answers (#1627): a walk records a lease for each webhook it opens, renews it every walk heartbeat (30 s) and deletes it when it ends; a crashed walk's lease lapses within three heartbeats. At most 16 per team.

### Team variables (#1613)

A team definition may declare `vars`, a variable name → default value, and states read them as `${var.<name>}`. `TeamDef op=run` takes `vars` to set values for one walk. A name the team does not declare is refused before anything runs, and the refusal lists the declared ones. Precedence, lowest first: the default, the value given at start, then whatever a `vars` state, a `capture` or a Starter's `binds` writes. Defaults and start values are literal text, never expanded; neither may contain `{{` or `}}` or name a credential, each is at most 4096 bytes, and a team declares at most 64. The values supplied at start are recorded on the walk's run as `spec.team.vars`, masked like the input; defaults are not copied there. Variables are not saved across a board resume.

### Schedules and webhooks can start a team (#1614)

A ScheduleDef or WebhookDef may set `delivery: team` with `team` and `vars` (a schedule may also set a literal `input`). On a schedule the `vars` are literals; on a webhook they are JSONPaths into the verified body, and an empty projection leaves the team's default in place. The walk starts detached through the same `op=run` path as any other start, in the trigger definition's tenant and as its user. Nothing in a webhook body can name the tenant, the team or the set of variables.

- **Schedule:** `last_status: completed` means the walk started and `last_run_id` is the walk's run. A team that cannot start is recorded as `last_error`, advances `next_run_at` and does not use up `max_fires`.
- **Webhook:** a verified delivery that starts no walk answers an opaque `400 {"error":"invalid_run"}`, whatever the cause, and is recorded as `rejected_team_start`; a spent budget is `429 token_limit_exceeded`, a paused runtime or failed start `503 runtime_unavailable`. A projected value carrying `{{`/`}}`, a credentials reference or more than 4096 bytes refuses the delivery. The walk's input is the raw body. Dedup is durable on the walk's run row, as for a spawned run. The admin dry-run gains `team` and `vars` preview fields.
- **Refused at write:** the run-shaped fields (`agent`, `channel`, `prompt`, credentials, `metadata`, `on_complete`, and on a webhook `user_tier`, `sync_response` and every `payload_mapping` target except `user_id`) with `delivery: team`, and `team` / `vars` / `input` on any other delivery.

### Security

- **An agent cannot grant authority it does not hold (#1621).** `AgentDef op=create` and `fork` from inside a run bounded only `tools` by the author, so an agent could create an agent with wider volumes, memory, SQL, history, channel, interruption, evaluation, skill or definition-authoring grants, then spawn it. Every capability field is now held to the author's own: `volumes`, `memory_scopes`, `sql_scopes`, the memory consolidation and recall flags, `core_blocks`, `history_scope`, `channels`, `interruption`, `evaluation_scopes`, `skills` and the `*_def_scopes`. A refusal names the field, the value out of reach and how to narrow it. A fork may also keep what the version it forks already holds. An unset `memory_scopes` or `sql_scopes` takes the ordinary memory default and is not judged; any other unset field is judged at what it resolves to (`volumes` the operator's `default` volume, `history_scope` the user's chats, interruption `kinds` `question`, `skills` every skill). A team's own agents are judged the same way, except that their `./<name>` channels and skills are the team's. Resource budgets are not judged. The operator planes (HTTP, MCP, gRPC substrate calls outside a run) are not narrowed.
- **Retired definitions are no longer served (#1611, #1616, #1626).** Retiring a definition flipped its flag but kept its active pointer, and the resolvers did not read the flag. A retired active WebhookDef, SkillDef, MemoryBackendDef, A2AServerCardDef or A2AAgentDef is now treated as absent: the name resolves from the next tier, and un-retiring restores it. A retired webhook answers `404 unknown_webhook` and starts nothing, a retired A2A peer is no longer dialed, and run start advertises the tools of the MCP server actually dialed rather than a retired version's.
- **`register_agent` discloses nothing (#1626).** Its refusal named a team's own agent, so any caller allowed to register could probe what a team declares. A name held by a team's own agent or a static agent is now refused with one message.
- **Snapshot restore of a team's channels (#1626).** A `_team/` message or cursor is restored only when its tenant holds a version of that team, including one the same restore just wrote. Skipped rows are counted in `team_channel_rows_skipped` and named in the warnings.
- **Durable dedup for a team's own webhooks (#1627).** Once any instance could accept a delivery, a captured signed delivery could be replayed to another instance or after a restart. Accepted deliveries' dedup keys are now claimed in the database, all or none, before the publish, and held for 24 hours; two instances racing on one delivery publish once. Only identities the signature covers are held: the body for a body-only signature (GitHub `sha256=`, bare hex), the id and the signed payload for a Stripe-style one. A refused request holds no key, and a failed publish releases them.

### Memory: a decision-model reranker (#1618, #1619)

`memory.reranker.kind: decision` asks a decision model (`nimble` on Ollama 0.35 or later, `POST /v1/systemone`) one typed choice over the candidates and orders them by its probabilities, so there is no reply to parse. The default, `kind: listwise` (or unset), is unchanged. Measured against listwise: ConditionalQA R@5 0.944 vs 0.935, LoCoMo memory recall@5 0.739 vs 0.741, about a third of the latency, slightly worse at rank 1. It needs an Ollama provider; at most 26 candidates are shown and the rest keep their places; a prompt refused as too large is retried at half and a quarter of the characters, then reports `rerank_reason: prompt_too_large`. Usage is booked on the searching run, and the keys follow the Ollama driver's rules. #1618 is the refactor that lets a search hold either kind, with no behaviour change.

### Web UI (#1624, #1625)

- **Team variables and a team's own definitions (#1624).** The Run dialog has a box per declared variable, prefilled with its default, and sends only the values that differ from it; values the server would refuse are flagged before the start. The walk view lists the variables set at start. The Teams page has a "This team's own definitions" panel listing the variables and each `local` entry with a one-line summary (a webhook shows its route, auth kind and the env-var name of its secret, never a value), with the Library's field editor for a team's own agents. A run inside a team carries a `team: <name>` badge. Deleting `vars` or a `local` kind in the editor now saves a version without it.
- **Save new version puts it in force (#1625).** It saved a new team version without promoting it, so Run, schedules and webhooks kept starting the previous one. It now sends `promote: true`.

### Other fixes

- **Team names (#1612).** A new team name must be one `[A-Za-z0-9_-]` segment of 1 to 64 characters, at create and fork.
- **Skill list and invoke agree (#1626).** `Skill op=list` offered shared SkillDefs and unpromoted names that `invoke` could not load. `invoke` now reads the run's tenant, then the shared tenant, then the static skills; `list` and the unknown-skill hint offer only names with a live active version.
- **`GET /v1/_tenants` in open mode (#1627).** A run started with no token has no tenant, and one such row failed the whole list with a 500 on both backends (and on gRPC and MCP). Those runs are now listed under the shared tenant `""`.
- **Tests (#1623, #1627).** The error-classification drift guard lists the team-webhook sentinels, and `test/runtime/webhooks/run.sh` delivers to the tenant route of the WebhookDef it creates.

### Upgrade notes

- **Migrations:** 0092 `team_webhook_arms` and 0093 `webhook_deliveries`, both new tables, on Postgres and sqlite, applied at boot. Snapshots do not carry them.
- **Team definition size:** `LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES` (default 1048576, 1 MiB; 0 or negative disables) now caps a TeamDef create or fork. Teams used to be held to `LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES` (default 128 KiB) and no longer are; if you set that to bound teams, set the new one.
- **Team names:** a new name must be one `[A-Za-z0-9_-]` segment of at most 64 characters. An existing team whose name breaks the rule keeps working for every op, and its tenant can still create and fork new versions under that name; such a team cannot declare `local` definitions. A tenant can no longer fork a shared-tenant team whose name is outside the rule unless it already holds a version of that name.
- **Agents authored inside a run:** a create or fork from inside a run is refused when any capability field reaches beyond the author's own. A meta-agent that authors agents must either hold what it grants or set each field explicitly within its own grants (`Context op=self` shows them): `volumes`, `history_scope` (`["-*"]` denies all), `interruption.kinds` and `skills` in particular, because leaving them unset is judged at their defaults; explicit `memory_scopes`, `sql_scopes`, `channels`, `core_blocks`, `evaluation_scopes` and `*_def_scopes` must be within its own. Otherwise an operator authors the agent. Stored definitions are not re-checked and nothing is refused at run time. Operators, unset `memory_scopes` / `sql_scopes` and resource budgets are unaffected.
- **Retired definitions:** a retired active WebhookDef, SkillDef, MemoryBackendDef, A2AServerCardDef or A2AAgentDef is no longer served; the name falls through to the next tier (a tenant's retired override is served by the shared or static definition of that name). A retired webhook's deliveries, dry-run and recent-deliveries answer 404 unless another tier serves the name. A webhook declared in yaml is not taken out of service by retiring its bootstrapped row: set `enabled: false`. Run start advertises the tools of the MCP server a run actually dials, not a retired version's. Un-retiring an MCPServerDef still does not make it reachable again until the next boot or snapshot restore.
- **A2A server card:** a configured `LOOMCYCLE_A2A_SERVER_CARD` whose active version is retired no longer serves; boot logs one line and starts with the A2A surface off. Un-retire it and restart to serve it. A card that does not exist still fails boot.
- **Skill tool:** a shared SkillDef that overrides a static skill now applies in tenant runs too. `list` no longer shows unpromoted or retired names.
- **`register_agent`:** a name held by a static agent or a team's own agent is refused with `agent name "<name>" is not available; choose a different name` (was `agent "<name>" is statically defined in yaml; cannot register over it`).
- **`GET /v1/_tenants`:** open-mode runs are listed under the shared tenant `""` instead of failing the request.
- **Web UI:** Save new version on the Teams page now promotes the saved version. TeamDef `op=fork` still defaults to `promote: false` for every other caller.
- **Teams in another tenant:** an admin's `op=run` by `def_id` of another tenant's team is refused when that team declares its own agents, schedules or webhooks, and a walk outside the team's tenant is refused its own channels. Run it as that tenant.
- **Reserved channel prefix:** `_team/` is refused as a yaml channel name (the config fails to load), a ChannelDef name, in any channel field or ACL, and by the Channel tool.
- **Team webhook dedup:** a redelivery of a delivery any instance accepted answers the idempotent `200 deduped` on every instance and after a restart, for 24 hours. With a body-only signature the key is the body, so an identical body re-sent within a day is not published again. A `delivery: channel` WebhookDef keeps its per-process 10-minute guard, and rate limits stay per instance.
- **Decision reranker:** opt-in; nothing changes until `memory.reranker.kind: decision` is set. An unknown `kind` fails config load, and so do `effort` or `context_tokens` with `kind: decision`. `rerank_reason` can now be `prompt_too_large`.
- **Adapters:** `@loomcycle/client` 1.104.0 adds `vars` on `TeamRunTarget` (sent by `runTeam`), `RunSpecTeam.vars`, `RunSpec.team_scope` and `RunSpec.agent_version.team_def_id`. The Python package is version-aligned with no API change. No gRPC proto change.

## What's in v1.103.0

*A message sent to a run while it writes its final answer is now answered instead of acknowledged and dropped; one sent after the run finished is refused. Team states can put the previous state's output inside a template and capture a plain-text answer into a variable, and a walk passes answers on without the sub-agent header. The Web UI follows a document reference into another document with a breadcrumb back, draws another tenant's team diagram for an admin, and has a reworked top bar. Also: a History result in a run never exceeds a quarter of the model's window.*

A minor release: #1600 to #1609 (every number in that range).
- #1601 and #1602 are operator messages to a running agent;
- #1607 to #1609 are team definitions;
- #1603 to #1606 are the Web UI; #1600 is History.

### An operator message sent during the final answer is answered (#1601, #1602)

`POST /v1/runs/{run_id}/input` answered `delivered: true` as soon as the message was queued, but a run that was not interactive read its queue only at the top of a turn. A message that arrived while the model wrote its final answer was discarded when the run ended, and nothing told the sender. A stateful run that was not interactive never read its queue at all.

- **Read before finishing:** as such a run finishes, it reads its queue once more. A waiting message becomes a user turn, the run answers again, and that answer is the run's result (and so a team walk's, when the run is a member). The same read applies after an approved review.
- **No false acknowledgement:** that last read closes the queue in one step, so a message arrives either before it and is answered, or after it and is refused with `404 run_not_in_flight`, the answer an ended run gives. Across replicas the owner's reply carries the closed state, so the sender gets the 404 at once rather than waiting out the acknowledgement timeout.
- **No turn left:** if a message is waiting but the run has reached its iteration cap, the run completes on its current answer and emits an error naming the unread message.
- **Stateful runs:** a stateful run that is not interactive now reads operator messages between its steps (the message is added to its task, so the result of the action it just ran is not lost) and once more before it finishes.

Interactive runs, runs held for review and sub-agents that take only a verdict behave as before.

### Team definitions (#1607 to #1609)

- **A template can include the hand-off (#1608).** A state's `input_template` used to replace what the previous state handed over. `{{thread.output}}` inside `input_template` is now replaced by that hand-off (the walk's input on the entry state) after the template is expanded, so the hand-off itself is never expanded. A template that does not name it replaces the hand-off, as before. The marker is refused in `system_prompt`, and a run no longer fills it into the system segment: before this, a state with the marker in its `system_prompt` had the previous agent's output written into its system prompt.
- **`capture` reads plain text (#1607).** `capture: {"draft": "$"}` binds an agent's whole answer when it is not JSON; any other path still binds nothing. Capture on agent and consolidator states reads the bare answer, so a JSON answer can be captured by field (`$.title`), which the header in front of it used to prevent. `$` on an `input` state binds a plain-text walk input.
- **A walk passes answers on without the header (#1609).** The hand-off to the next state, each entry's `output` in a parallel or Starter results envelope and the walk's own `final_text` are the agent's answer as it wrote it, without the `[sub-agent agent_id=…]` line. Each envelope entry already names its `agent` and `run_id`. The `Agent` tool's own sub-agent results keep the line.

### Web UI (#1603 to #1606)

- **Follow a reference into another document (#1604).** In the document view, a reference, backlink, related chunk or graph node that points into another document is now clickable: it opens that document at the referenced chunk, the tree and path follow, and a breadcrumb with a Back button returns to the document and chunk you came from. A target that cannot be read shows a message instead of an empty pane. This ships in `@loomcycle/explorer` 0.9.0.
- **Team diagrams for an admin (#1603).** The Teams page and the team-walk view asked for a diagram by team name, which the server resolves in the caller's own tenant, so an admin saw "team not found" for another tenant's team. Both now ask by version id, and the walk view draws the version the walk ran. Run, Save and Delete are disabled, with the reason shown, on a team outside your own tenant.
- **The top bar (#1605, #1606).** Every item sits on one centre line (the status chip was pushed up by a stylesheet rule meant for the Settings page). The tenant and user fields are comboboxes: a list of known tenants or users that filters as you type, with any id still typeable; the separate edit button is gone. The gear opens a menu of the settings sections, Users and Log out; each settings section has its own URL (`/settings/limits`), and Users is no longer in the left navigation.

### History (#1600)

In a run, a History result now fits a quarter of the window the model is actually running with. The page budget was sized from a configured `max_context_tokens` only, and `window`, `list`, `search` and `related` were bounded by row or turn counts, not size. All of them now fit the budget and mark a shortened result `truncated` with a note. The budget is only ever lowered. Results outside a run are unchanged.

### Upgrade notes

- **No migration.**
- **Operator messages:** a message sent to a finished run now gets `404 run_not_in_flight` where it got `delivered: true`. A message sent in time makes the run take one more turn, which is one more model call.
- **Team walks:** a hand-off, an envelope entry's `output` and a walk's `final_text` no longer start with `[sub-agent agent_id=…]`. A consolidator prompt or a client that stripped or parsed that line now receives the bare answer.
- **Team definitions:** `{{thread.output}}` in a `system_prompt` is refused at create and fork. A stored definition that has it there now sends the marker as written.
- **`capture`:** an answer that is exactly a JSON value (`"quoted"`, `42`, `true`, `null`) is read as JSON, so `$` binds `quoted` without its quotes.
- **History:** in-run pages are smaller on a small window, and a list, search or window result can now be `truncated`.
- **Adapters:** `@loomcycle/client` and the Python package are version-aligned at 1.103.0 with no API change. `@loomcycle/explorer` 0.9.0 publishes from its own `explorer-v0.9.0` tag.

## What's in v1.102.0

*DeepSeek's cheapest model works again: DeepSeek renamed it, and every default config was naming a model it no longer lists. A run whose model stream ends unfinished now fails instead of completing empty. Model speed measurement keys aliased models correctly, stops counting Ollama's own queue as slowness, and takes a per-model cap. Also: team walks report the end state they reached and appear on their own run-state stream, a Starter's sink messages carry bindable answers, the Web UI can start a team from its input form, and WebFetch no longer overflows a small model's window.*

A minor release: #1584 to #1597 (every number in that range).
- #1596 is the DeepSeek model ids; #1591 is the unfinished stream; #1592 is model speed;
- #1585 to #1590, #1594 and #1597 are teams and agent definitions;
- #1584 and #1595 are WebFetch; #1593 is docs.

### DeepSeek: `deepseek-flash` replaces `deepseek-v4-flash` (#1596)

DeepSeek's model list now has only `deepseek-flash` and `deepseek-v4-pro`. `deepseek-v4-flash` and `deepseek-chat` are still accepted, and DeepSeek serves both on `deepseek-flash`, but neither is listed. A tier candidate the provider does not list is skipped, so the base preset's DeepSeek cost floor was dead on every deployment using the defaults: `/v1/_routing` showed it `available: false`, and runs moved on to the next, dearer candidate.

- **Every shipped config names `deepseek-flash`:** the base preset, `loomcycle.example.yaml`, the document-agent oauth bundle and the examples. Alias names are unchanged. A new test checks that every shipped `provider: deepseek` entry names a model DeepSeek lists.
- **`deepseek-flash` is a thinking model at any effort.** A live probe sent it a forced `tool_choice` with no effort hint and DeepSeek refused: "Thinking mode does not support this tool_choice". So a forced tool choice is no longer sent to it (a stateful run on it failed every step before this), and a fallback onto it with a history that has no reasoning is downgraded to `deepseek-chat`, which DeepSeek serves without thinking.

**If your own config names `deepseek-v4-flash`, change it to `deepseek-flash`;** the old id still answers but is skipped as a tier candidate.

### A run whose model stream ends unfinished fails (#1591)

A provider stream that closed without a final `done` message was read as a normal, empty end of turn. The run was recorded `completed`, with no text, no stop reason and no usage, on every transport. Found on the lab when an Ollama runner crashed mid-call.

- **The loop:** a stream that ends with neither a done nor an error event, while the turn is still live, now fails the turn: "the response stream ended before the model finished its turn". A cancelled turn or run is not affected.
- **The Ollama driver:** a 200 response that closes without its `done:true` frame now reports an error, not a finish.

Every caller takes its status from the loop, so HTTP, gRPC, MCP `spawn_run`/`spawn_runs`, sub-agents, the scheduler and team walks all report `failed` now. The new error is not retried and does not trigger provider fallback. The Anthropic, OpenAI-compatible and Gemini drivers have the same "no final frame is a finish" shape; the loop check covers them, and their driver-level fix is a follow-up.

### Model speed measurement (#1592)

v1.101.2 started timing every model call and keeping a slowdown per model (measure only; no timeout changes). The first lab data found two faults and one gap:

- **Aliased models are measured under their concrete name.** Samples were filed under the alias a run used and looked up under the concrete model, so a model reached through a `models:` alias never showed its samples on `/v1/_routing`. Live samples, the boot reload from the usage ledger and every lookup now resolve the alias first.
- **Ollama's own queue no longer counts as slowness.** Queue time was wall time minus Ollama's `total_duration`, but that total includes the time a request waits inside Ollama for the runner. A call queued 150 s behind another scored about 10× slow. On Ollama the queue is now the wall time outside load, prompt evaluation and generation, and the time-to-first-token figure excludes it too.
- **A per-model cap.** `timeout_scaling.models.<model>.max_multiplier` replaces the global cap for that model, both for clipping each sample and for the multiplier, at every sample count. It must be between 1 and 100. On the lab, `qwen3.8:latest` measured about 20× slower than the reference and saturated the global 8×; `ollama-local/qwen3.8:latest: { max_multiplier: 24 }` lets it measure as it is. The routing view and `Context op=self` report the cap in effect as `max_multiplier`.

### Teams and agent definitions (#1585 to #1590, #1594, #1597)

- **A walk records the end state it reached (#1588).** A walk's run result is now `{final_text, terminal}`, where `terminal` is the id of the end state, so two endings of one team can be told apart. It is absent when the walk failed or was cancelled.
- **A walk appears on its own run-state stream (#1587).** Filtering the stream by `walk_id` now includes the walk's own run as well as its members: it is published when it starts, when it pauses at a breakpoint (with what it is waiting on) and when it ends.
- **A Starter's sink message is bindable (#1585, #1589, #1594).** `output` is the member's bare answer, without the `[sub-agent agent_id=…]` line meant for models, and `structured` carries the member's `output_format` result. A rejected or review-expired member now publishes the same shape, and its answer is parsed against `output_format` too, so `$.output` and `$.structured.<field>` bind the same way for every status.
- **A pin always wins over a tier (#1586, #1590).** A fork could not clear its parent's `tier`, so an agent forked onto a local pin kept the parent's tier and fell back to a paid provider. A fork now switches cleanly between a pin and a tier; AgentDef create/fork and `register_agent` refuse a pin and a tier together with the same rule as yaml; and a stored row that has both resolves to its pin.
- **The Web UI starts a team from its input form (#1597).** The Teams page has a Run button. It builds the form from the entry state's `schema`, with document and chunk pickers from the `x-loomcycle-picker` hints, and starts the team's active version.

### WebFetch (#1584, #1595)

A local-model run with a 32K window made three WebFetch calls, and its next request was 61K tokens; Ollama refused it.

- **A page is stripped before it is cut.** The raw body used to be capped at 256 KiB first. On a modern page that cut lands inside an inline `<style>` block, so the CSS came back as the page's text and the article after it was never read.
- **The cut fits the model's real window.** The default output cut now follows the window the model is actually running with (what the provider reports, lowered by any configured cap), not only a configured `max_context_tokens`.

### Upgrade notes

- **No migration.**
- **DeepSeek:** change any `deepseek-v4-flash` in your own config to `deepseek-flash`.
- **Run status:** a run whose model stream ended unfinished is now `failed` with an error, where it was an empty `completed`. A client that treated an empty completed run as "nothing to say" will see the failure.
- **Agent definitions:** creating or forking an agent with both a pin and a tier is refused; pick one.
- **Adapters:** `@loomcycle/client` 1.102.0 adds `RunResult.terminal`. The Python package is version-aligned with no API change.

## What's in v1.101.2

*The Web UI's Path tree loads one directory at a time, so a large directory (subject-homed facts) no longer hides every directory that sorts after it. Also: loomcycle now measures each model's speed (measure only, no timeout changes), and a team can be started from its own input form, with the input checked against the form before the walk starts.*

A patch on the v1.101 line: #1579 to #1582.
- #1581 is the Path tree fix;
- #1582 is throughput measurement;
- #1579 and #1580 are team input forms. #1577 (publish a walk's input as a JSON value) shipped in v1.101.1's binaries but was missing from its notes, so it is listed here.

### The Path tree no longer hides directories behind a large sibling (#1581)

The explorer loaded the whole tree with one recursive `Path ls /`. Since `ls` became paged (500 entries by default), that request returned only the first page, in path order. A user scope whose `/facts` directory holds more than about 500 subject-homed fact documents filled the page before the listing reached any directory sorting after `facts`. `/loomboard`, `/loomcycle` and `/memory` disappeared from the tree. The documents were intact and reachable by path or search. Only the tree was truncated.

- **Each directory is listed when it is first opened,** with a non-recursive `ls`. The root is listed the same way, so every top-level directory shows.
- **A large directory ends with "Load more (N shown)"** while the server reports more, fetching the next page by cursor. Nothing is dropped silently.
- **A deep link or a new item loads its ancestors on demand,** paging until the target appears.
- **mkdir, new document, move and delete** re-list only the affected loaded directories. Refresh reloads the root and every open directory, keeping as many entries as were shown.
- **Deleting a branch** lists its whole subtree, following cursors, before the confirmation, so the count and the cascade cover every document under it, not only those on screen.

**Behaviour changes:** the tree starts collapsed. Tree rows no longer carry the "implicit" badge; the detail pane still shows whether a directory is stored or implicit (the check is a `Path stat`, so an implicit directory logs an expected 422 in the browser console). `@loomcycle/explorer` is 0.8.0, with an optional page argument on `ExplorerDataLayer.pathLs` and an optional `pathIsStored`.

### Every model call is timed, and each model's speed is measured (#1582)

Wall-clock budgets that wrap model work are sized for a fast hosted model, and a slow local model is cut by them. This release measures how slow each model is. **It changes no timeout.** A later release will use the measurement to scale budgets.

- **Per-call timing.** Each usage event carries an optional `timing` block: `duration_ms`, `ttft_ms`, and on Ollama the server's own `load_ms`, `prefill_ms` and `decode_ms`, plus `queue_ms`. The loop, stateful runs, the summarizer, the reranker, Document unit generation and image description all stamp it. A failed or cut call is not stamped. The ledger stores it in six new nullable `token_usage` columns.
- **A slowdown per (provider, model)** against a reference of 100 output tok/s, 2000 prompt tok/s and a 1 s first token. It is smoothed in log space and clipped, and model load and queue wait are not counted as slowness. Below 5 samples it falls back to a per-model override, then the provider's other models, then a prior of 4× for a local provider. It lives in memory per replica and is re-seeded at boot from the last 7 days of timed calls.
- **Where to see it:** a `throughput` block per candidate on `GET /v1/_routing` (the queue-time median is admin-only), four gauges on `/metrics` (`loomcycle_model_slowdown`, `loomcycle_model_timeout_multiplier`, `loomcycle_model_decode_tps`, `loomcycle_model_throughput_samples`), and a `timeouts` block in `Context op=self`.
- **Config:** a new `timeout_scaling:` block (`mode: measure` by default, or `off`; `on` is refused at load), with env overrides `LOOMCYCLE_TIMEOUT_SCALING`, `LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS` and `LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER`. A change needs a restart. See `docs/CONFIGURATION.md`, "Measured model speed".
- The TS client's `Usage` type gains `timing`. The gRPC proto is unchanged.

### A team can be started from its input form (#1577, #1579, #1580)

- **A Starter can read the walk's own input (#1579).** `source: {"kind": "input"}` on the entry Starter fans out over the walk's input instead of a channel or document, so a channel team starts in one call and processes the input it was given, not the oldest message on a backlog. It is allowed only on the entry, takes no channel, path, wait, batch or ack, and may carry a `schema`.
- **The input is checked against the entry's form before the walk starts (#1580).** When the entry (an `input` state or an input-sourced Starter) has a `schema`, `TeamDef op=run` checks the top-level `type`, `required` fields and each present field's declared `type`, one level deep. Other keywords are ignored, never refused. A bad input is refused before the walk's run row is opened and before any model call: HTTP 422 `tool_refused` with category `validation`, the gRPC/MCP equivalents, and a message naming the field. `agent-teams` documents the form and the `x-loomcycle-picker` hints.
- **A walk's input, or any output, can be published as a JSON value (#1577).** `publish: {"channel": …}` on an `input` state publishes the input itself, and `payload: "raw"` on a publish node publishes its input unwrapped. Valid JSON is published compacted; other text as `{"text": …}`, without HTML escaping.

### Upgrade notes

- **Migration:** Postgres runs migration 0091 (six nullable timing columns on `token_usage`); SQLite adds them at boot. Both are additive.
- **No config change is required.** Measurement is on by default and changes no budget; set `LOOMCYCLE_TIMEOUT_SCALING=off` to disable it.
- **Teams with an entry `schema`** now refuse an input that does not match it. A team that was started with a non-matching input will fail at start instead of running.
- **Adapters:** `@loomcycle/client` 1.101.2 adds the usage `timing` type. The Python package is version-aligned with no API change.

## What's in v1.101.1

*Fixes a v1.101.0 regression: memory consolidation passes on slow local models were cut by the new per-pass budget, then re-read the same chats forever. Also: non-admin MCP server definitions can no longer reference the operator's env vars, the consolidation sweep reports operator-key refusals and runs isolated members isolated, a remote memory peer can key with its own tenant's `$cred:`, and the Web UI gains a team-walk view.*

A patch on the v1.101 line: #1568 to #1576 (every number in that range).
- #1576 is the consolidation regression fix; #1569 is the consolidation sweep;
- #1568 and #1570 are credentials for MCP server definitions and remote peers;
- #1572, #1573 and #1574 are team walks; #1575 is a help article;
- #1571 is bench-only.

### A cut consolidation pass no longer retries the same chats forever (#1576)

v1.101.0's per-pass budget (#1565) gives each consolidation target a slice of the fire budget, `FireTimeout / min(targets, 4)`. With two users and the default 600-second budget, each pass gets 5 minutes. A local-model extractor that takes 40–100 s per window does not fit.

The bundled consolidator advanced its watermark only at the end of a pass, and only if every chat on the page succeeded. A pass cut by its slice kept nothing it had finished, so the next pass started from the same watermark and was cut at the same point. In production every pass failed at exactly 5:00, re-sending the same transcript windows for hours and consolidating nothing. v1.100.0 had no slice, which is why it worked.

- **A pass cut by its own slice gets the whole budget on its next turn.** It is dispatched first on the next tick, ahead of the rotation and the cap, with no deadline of its own. A completed pass clears the escalation. An escalated pass that the batch budget cuts drops back to normal budgeting, so a wedged target holds at most every other tick and cannot starve the others. A pass cut only because the batch was already spent is not escalated. The scheduler tells the two cuts apart by the deadline's cause, never by `RunOnce`'s error.
- **The watermark advances after each chat.** A cut pass keeps the chats it finished, and the next pass resumes at the first one it did not finish. Once one chat fails, no later chat advances, so the watermark never skips a hole.
- **Trade-off, accepted by the owner:** retiring duplicates stays at the end of the pass and no longer holds the watermark. A cut or failed `supersede` leaves the duplicate rows it found in place, and never loses a fact: the chat's own facts have landed, and the duplicates were already stored. A queued-batch failure likewise no longer holds the chats' watermark; its items stay unacked.

**Still on v1.101.0?** Set `LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS=1800` until you upgrade, so each slice is long enough for a local-model pass.

### Consolidation sweep (#1569)

- **An operator-key refusal inside a pass is visible.** With `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` on, the code-js consolidator's `memory/extractor` spawn can be refused the operator's key. The pass used to read `completed`. It now reads `skipped` (deferred), counts toward `max_fires`, and is logged once per tenant per tick. The match is on the structured error classification, not on error text.
- **An isolated member's pass runs isolated.** A target whose `users.access_mode` is not `tenant` runs with `Isolated` set, so its memory reads and writes stay in that user's scopes. The member's mode only ever narrows the def's own bit. A failed member lookup records the tick `failed` and dispatches nothing.

### Credentials (#1568, #1570)

- **Only an admin may put `${NAME}` env refs in an MCP server definition (#1568).** A `substrate:tenant` author, or a caller in a run, could write a dynamic MCP server def whose url, headers or stdio command/args/env read `${LOOMCYCLE_…}`. The pool expanded it from the process environment at dial, sending the operator's secret, or another tenant's, to a host the author chose.
  - Such a `${NAME}` is now refused at create and fork, including one nested in a `${run.x:-…}` default. `${run.credentials.<name>}`, `${run.user_bearer}`, `${run.tenant_id}`, `${run.root_run_id}` and `$cred:<name>` stay allowed.
  - An admin, the open-mode or stdio operator, and a def that keeps the operator's connection fields unchanged keep working.
  - The server stamps an `operator_authored` bit on the body. It is never read from the overlay and is excluded from `content_sha256`. A boot audit lists each stored version that lacks it but holds a `${NAME}`, by field location, never by value.
- **Remote memory backends and document sources accept `api_key_env: "$cred:<name>"` (#1570).** It sends the tenant-level `CredentialDef` of the tenant that owns the definition: never the caller's tenant, and never a user or agent scope.
  - A non-admin may name only a tenant-level credential that already exists in its own tenant.
  - A shared (operator-layer) def sends the operator layer's credential for every tenant's runs.
  - A missing credential fails the call before anything is sent.
  - Yaml peers are unchanged and still refuse `$cred:`.

### Team walks (#1572, #1573, #1574)

- **The runs page no longer returns a 400 for a team walk (#1572).** A walk's run is filed under `agent_id = "team:<name>"`, which the agent-id routes reject. Cancel now goes through `POST /v1/runs/{run_id}/cancel`, a `?agent=team:…` link opens the newest listed walk, the memory and channels tabs are hidden for a walk, and switching runs clears the pane at once.
- **A team-walk view (#1573).** Selecting a walk opens a walk pane: the header and result, the team's Mermaid graph with the current state outlined, and members grouped by visit. While the walk is live it also offers review Approve / Reject, breakpoint continue / release / abort, a breakpoints editor and cancel. It is built only on existing endpoints, with no server change.
- **A walk's run records the team version and input it ran (#1574).** The walk's `run_config` carries `team`: name, `def_id`, `version`, `content_sha256`, `resolved_by`, the start arguments, and the input, masked by the redactor and cut at 16 KiB. It appears as `spec.team` on `GET /v1/runs/{id}`, `GET /v1/agents/{id}`, gRPC `GetRun` and MCP `get_run`, so a viewer can draw the version a walk ran even after a fork and promote. Walks started before this release have no `spec.team`.

### Other

- **Docs:** the `agent-teams` help topic now says when a team needs a Starter and when an agent state is enough (#1575). The `memory-consolidation`, `dynamic-mcp` and credential help articles describe the changes above. Repo `docs/CONFIGURATION.md`, `docs/MEMORY-BACKENDS.md` and `docs/EXTERNAL_API.md` changed (#1568, #1570); their doc-store copies need a sync.
- **Bench (no runtime change):** reranking memory recall on LoCoMo (1,535 questions) lifts recall@5 from 0.588 to 0.741 with the shipped listwise rerank; a one-call decision-model rerank is non-inferior and about 3× faster (#1571).

### Behaviour changes / upgrade notes

- **Consolidation:** a pass cut by its share of the fire budget is dispatched first on the next tick and may use the whole remaining budget. New log lines: `pass cut by its … share …`, `completed on its whole-budget turn`, `dropping it back to normal budgeting`. Escalation state is per replica and in memory; losing it costs one more cut pass, never progress (#1576).
- **The bundled consolidator advances its watermark per chat.** A pass report can read `watermark advanced to X, then held: …`. A cut or failed duplicate retirement, or a queued-batch failure, no longer forces the chats to be re-read; the duplicates stay until a later pass retires them (#1576).
- **With `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` on, a sweep pass whose sub-agent is refused the operator's key reads `skipped`, not `completed`,** and counts toward `max_fires`. One log line per tenant per tick replaces the per-target lines (#1569).
- **A consolidation pass for an isolated member runs isolated:** a fact the tenant ontology would share stays with the user. The sweep reads `UserList` once per tick that has targets; if that read fails, the tick records `failed` (#1569).
- **Non-admin MCP server defs cannot reference operator env vars** (`${NAME}` in url, headers, or stdio command/args/env). Use `${run.credentials.<name>}` or `$cred:<name>` (#1568).
- **Existing unattributed MCP server defs keep dialing this release, with a warning.** These are every runtime row holding a `${NAME}` written before this release, **including rows an admin wrote**, because the authority was never recorded, and rows restored from an older snapshot.
  - Each is listed in a boot `mcp_server_defs: WARNING:` line and gets one dial `mcp_servers: WARNING:` line per version per process.
  - To fix one, have an admin acting in that tenant re-save it with `create` and the same overlay, or retire it.
  - **`LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=1` refuses them now. The next release turns that on by default** (#1568).
- **Remote memory backends and document sources may use `api_key_env: "$cred:<name>"`.** Existing env-var and `key_per_tenant` keys behave as before (#1570).
- **A board-bound team walk whose board cannot be read is refused before its run row is created;** before, it left a row behind (#1574).
- **Web UI:** selecting a team walk opens the walk view, and cancel on a walk stops it instead of failing with 400 (#1572, #1573).
- No schema migrations and no proto change.

**Adapters:** `@loomcycle/client` 1.101.1 adds `RunSpec.team` (`RunSpecTeam`), the walk's team record on run reads (#1574). The Python adapter's version is 1.101.1, with no surface change. `python-v*` tags are still held, so no `python-v1.101.1` tag is pushed.

Tagged as a patch but released with `force_full`, because the fixes are in the Go runtime: the patch tier alone builds only `loomcycle-browser`.

## What's in v1.101.0

*A snapshot now carries the whole deployment (users and budgets, schedules, webhooks, peers, memory backends, document sources, volumes, the Path tree and the consolidation queue), strips literal trigger credentials, and re-checks every definition it restores against the target's own rules. A paused run resumes as the run that paused: on the agent version it started on, in its own tenant, and no wider than its parent allowed. A security review closed cross-tenant and isolated-member gaps in webhooks, triggers, remote peers, the private-address guard and volume paths. Also: team walks hold agent and parallel members for review, enforce `timeout_ms` and can fan out over a document's sections, and a run can be read by its run id on every transport.*

127 PRs, #1439 to #1565:
- #1483, #1487, #1489, #1492, #1493, #1499, #1503, #1512, #1516, #1527, #1530 and #1531 are snapshot coverage (RFC DP P0–P8); #1472, #1473, #1476, #1477, #1498, #1509, #1545, #1548, #1552, #1554 and #1557 fix what a restore carries and reports;
- #1480, #1481, #1484, #1486, #1491, #1496, #1497, #1504, #1511, #1520, #1541, #1543 and #1559 make a resumed run the run that paused;
- #1442, #1443, #1444, #1446, #1457, #1461, #1462, #1465, #1470, #1474, #1488, #1490 and #1515 are team walks and review;
- #1456, #1466, #1469, #1471, #1482, #1518 and #1522 are run reads and the run-state stream; #1445, #1447, #1448, #1451 and #1529 are live-run controls;
- #1452, #1454, #1455, #1458, #1459, #1460, #1464, #1485, #1494, #1500, #1502, #1506, #1523, #1528, #1535, #1536, #1538 and #1556 confine tenants and isolated members;
- #1501, #1507, #1532, #1540, #1549 and #1563 are webhooks; #1508, #1533, #1546, #1547 and #1550 are outbound requests and volumes; #1525, #1539, #1553, #1562, #1564 and #1565 are schedules and consolidation;
- #1467, #1468, #1505, #1514 and #1551 are stateful runs and the repeat guard;
- #1439, #1440, #1441, #1475, #1510, #1513, #1517, #1519, #1521, #1524, #1526, #1534, #1537, #1542, #1555 and #1560 are other fixes and docs;
- #1453, #1495 and #1544 are bench-only; #1449 and #1450 publish `@loomcycle/def-fields` 0.3.0 and `@loomcycle/library` 0.7.0; #1463, #1478, #1479, #1558 and #1561 are CI and test fixes.

### Snapshots carry the whole deployment (RFC DP)

A restore used to bring back agents, memory and paused runs, and little of what they depend on. Every store table now has a decided classification, and a test fails on a table or entry field nobody classified (#1483).

- **New sections:**
  - users, token budgets and month-to-date usage (#1492): an isolated user stays isolated, a budget stays enforced, and a nearly spent month does not restart at zero. Restored budgets are live in the tracker at once (Postgres migration 0088, `usage_carry`);
  - schedules with their fire counts (#1493), so a `max_fires` schedule does not get its budget back;
  - webhooks, A2A agents and A2A server cards (#1499);
  - memory backends and document sources (#1503), restored before the memory routed through them;
  - dynamic volumes (#1512). The section carries no path: restore derives it under the target's dynamic root and creates the directory;
  - the undrained consolidation queue (#1516), captured before memory, so an add is at worst consolidated twice, never lost. The consolidation sweep now also finds users who have queued rows and no session on the target;
  - the Path tree (#1527), restored last and checked against what the target holds;
  - runtime channel definitions (#1477), so a held or hooked channel keeps holding and keeps its hooks.
- **Credentials:**
  - schedules and webhooks travel with literal `user_credentials` values stripped and their keys listed as `stripped_credentials`; references (`$cred:`, `$ghapp:`, `${…}`) travel as written;
  - a trigger restored without a value comes back **disabled**, with a `capture_disabled` marker. A fork or create that re-supplies every stripped key, with a value the fire path will use, re-enables it (#1493, #1499, #1557);
  - a literal in a header (#1487), a stdio `env` map (#1531), URL userinfo, a credential query parameter, or a stdio `command`/`args` (#1548) travels as authored and is reported in the capture's `warnings` by location, never by value;
  - credential, token and usage-ledger tables never travel.
- **Every restored definition is re-validated** by its authoring rules on the target (#1499, #1503, #1530): MCP servers against this host's allowlists, hooks (a code-js body only where code hooks are on), channels, agents, teams, skills, webhooks, peers, memory backends, document sources, volumes and Path entries. A refused row is skipped with a warning and counted in `<section>_refused`. An active pointer at another tenant's definition is refused (#1498).
- **What a restore reports:** an extensible `restored` map (section → count) on every transport (#1487), including `paused_runs_resumed`. A section this reader does not know is warned about rather than dropped silently (#1483).
- **After a restore:** restored MCP server defs go live without a restart (#1487), and restored paused runs resume on a gRPC or MCP restore too, not only HTTP (#1489).

### What a restore carries and reports, fixed (#1472, #1473, #1476, #1509, #1545, #1548, #1552, #1554)

- A paused run restores under its own tenant, operator-key restriction and isolation (#1472), with its parent run id (#1476). Channel cursors restore under their own tenant (#1473).
- Restored transcript events get new seqs on the target (#1509). A target with history used to skip the colliding events, and on Postgres the sequence fell behind until every `AppendEvent` failed.
- Section versions are checked before anything is written (#1545). A restore that fails partway still enforces the budgets it wrote, and a budget changed through `/v1/_limits` during a restore keeps its stored value.
- A refused URL's userinfo, query and fragment are kept out of warnings, and an agent- or user-scoped `$cred:` is no longer reported missing (#1548).
- Rehydrating the MCP registry after a restore is serialized with MCPServerDef retire and promote, so it cannot undo either (#1552).
- `loomcycle restore` and the Snapshots page show refused and disabled counts apart from restored ones (#1554).

### A resumed run is the run that paused (#1480, #1481, #1484, #1486, #1491, #1496, #1497, #1504, #1511, #1520, #1541, #1543, #1559)

A paused run comes back after a pause, a snapshot restore or a restart. It used to come back on whatever was current, sometimes with more access than it had.

- **On the agent version it started on (#1486).** Each run records `agent_version` in its `run_config`. A registered agent changed since fails the run instead of running it on something else (#1543). A static agent is read from the configuration again, so a same-named agent that appeared meanwhile cannot take the run over; a static agent since removed fails it (#1559). Runs recorded before this release resolve by name, as before.
- **As its row, not as the restorer (#1504).** A restore no longer lends its admin principal to the runs it resumes.
- **As its parent's child (#1480, #1481).** Every sub-run row now records `parent_run_id`, and every run read carries it. A resumed child keeps its parent, its spawn tree's root and the parent's cancel cascade.
- **No wider than its parent allowed (#1484).** A child's volumes and fan-out width are recorded at spawn (`run_config.spawn`) and re-applied. A child with no record resumes without volumes and serial (#1520).
- **Hook pins stay in the run's tenant (#1496, #1497).** A pin or lookup in another tenant fails the run as a deleted pin would. A `def_id` pin to another tenant's AgentDef is refused at spawn, as an unknown id (#1491).
- **An Agent-tool child still takes only a verdict (#1541).** Compacting one is refused 409 `run_takes_only_verdicts`.
- **A run a live loop still owns is left alone (#1511).** A restore or boot no longer re-dispatches a parked run that this process, or another live replica, still holds.

### Team walks and review (#1442, #1443, #1444, #1446, #1457, #1461, #1462, #1465, #1470, #1474, #1488, #1490, #1515)

- **Review holds agent and parallel members (#1462)**, not only a Starter's. A consolidator is refused as a review target, since its answer is the walk's verdict.
- **A held Agent-tool child waits for a verdict, and its parent sees it (#1442).** The parent's stream gets a `subagent_hold` event (`held` / `released`) on SSE, gRPC, TS and Python.
- **`timeout_ms` is enforced (#1444)**, excluding time held for review. A Starter run that runs out publishes `status: "timeout"` to the sink.
- **A Starter can fan out over a document's sections (#1474):** `source: {kind: "document", path, scope, select: "chunks"}` with `fanout: {per: "chunk", max}`.
- **The live breakpoint set** (`PUT /v1/runs/{run_id}/breakpoints`) refuses a state the team does not have with 400 (#1461), releases held members at once when review is disarmed (#1443), and takes `review_ttl_seconds` (#1465).
- **A walk runs as its caller (#1457)**, not as the synthetic `http-admin` / `grpc-admin`: its members' memory, budgets, usage and pauses are the caller's.
- **Every walk member is stamped** with its walk, state and visit in `parent_context` (#1470).
- **A subscription walk runs as confined as the team's promoter (#1488, #1490)**, captured at promote (Postgres migration 0087).
- **A TeamDef state's hooks carry the team as their source (#1446)**, so an operator-authored team's hook may widen hosts where permitted. A nested walk does not inherit the outer walk's review arming (#1515).

### Reading runs (#1456, #1466, #1469, #1471, #1482, #1518, #1522)

- **A run by its run id (#1466):** `GET /v1/runs/{run_id}`, gRPC `GetRun`, MCP `get_run {run_id}`, TS `getRun`, Python `get_run`. An agent id can name many runs; every walk of a team shares one.
- **One walk's runs (#1471):** `GET /v1/runs?walk_id=`, gRPC `ListWalkRuns`, MCP `list_runs {walk_id}`, TS `listWalkRuns`, Python `list_walk_runs`, paged by cursor (Postgres migration 0086, `runs.walk_id`).
- **Every transport's run read carries what the HTTP read carries (#1456):** agent name, `parent_context`, provider, interactive, replica, and what a running run waits on.
- **The run-state stream announces waits (#1469):** a `running` transition with `awaited_state` (`review`, `input`, `channel`, `interrupted`), `awaited_on` and `hold_expires_at`. The status stays `running`. A run with several open waits stays held until the last ends (#1522). The stream carries `parent_run_id` (#1482).
- The Web UI runs tree keys each node by run, not agent id (#1518).

### Live-run controls (#1445, #1447, #1448, #1451, #1529)

- Retune takes `tool_choice` and `output_format`, applied at the next operator turn, and `Context op=self` reports both (#1448).
- A run's own `interruption` block is applied, narrowing only, on every start path (#1451); `POST /v1/runs` accepts it.
- A started interactive or review draft outlives its starter, like a `POST /v1/runs` interactive run (#1445).
- Retune keeps every record field it does not change (#1447), and `run_config` writes are compare-and-set (#1529), so concurrent writers no longer erase each other's fields.

### Tenant and isolated-member confinement (#1452, #1454, #1455, #1458, #1459, #1460, #1464, #1485, #1494, #1500, #1502, #1506, #1523, #1528, #1535, #1536, #1538, #1556)

- **Trigger execution tenant (#1502):** a non-admin may only store a webhook or schedule whose runs execute in its own tenant.
- **Trigger and team-walk authors' restrictions** are captured from the run as well as the principal; the stricter side wins (#1485).
- **Evaluation** reads and submits are confined to the caller's tenant (#1494), and an isolated run to its own user (#1523).
- **A fork parent or promote target in another tenant** reads as not found, naming nothing (#1500).
- **Run mutations:** whole-run cancel, compaction and routed turn-cancel check the run's owner (#1506); steer and turn-cancel fail closed on a session read error (#1535) or a missing session (#1556); the remote steer gate refuses an Agent-tool child (#1528) and a walk or hook run (#1538).
- **Run lists and streams:** an isolated member cannot list or stream another user's runs over gRPC or the connector (#1452, #1459, #1460); MCP `stream_user_run_states` is confined to the session's tenant (#1458); the list-by-user bound applies after the tenant filter (#1454).
- **Caller input:** runtime-owned `parent_context` fields (walk, wave, board) are dropped (#1455), and caller fields are length-bounded on every transport (#1464). An unowned `session_id` on `POST /v1/runs` is a plain 404 (#1536).

### Webhooks (#1501, #1507, #1532, #1540, #1549, #1563)

- **Dedup is scoped to the webhook (#1501, #1507).** It keyed on the bare delivery id across the whole database, so a byte-identical body to another webhook, in any tenant, was taken as a replay. It now keys on the resolved definition's owner, never the URL segment, and so do rate limits and the recent-deliveries list (#1532).
- **A signed delivery dedups on both identities, durably (#1540, #1563):** its delivery id and what its signature covers. A captured delivery can no longer be replayed into new runs by changing its delivery-id header (Postgres migration 0090, `runs.delivery_alt_key`).
- **Triage (#1549):** an admin's `/test` takes `?tenant=`, and `/test` refuses a def the receiver would refuse.

### Outbound requests and volumes (#1508, #1533, #1546, #1547, #1550)

- **Runtime remote memory backends and document sources** cannot reach private hosts (#1508) and must name an allowlisted host; a non-admin author can name only a credential bound to its own tenant (#1550). Before, a tenant operator could aim one at any public host with any credential-safe env var attached.
- **The private-address guard refuses every non-public range (#1547):** CGNAT `100.64.0.0/10` (Tailscale, a cloud metadata address), `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, and IPv6 forms that embed a private IPv4. The private-host allowlists accept CIDR entries. The A2A client uses the same guard and now honours `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`.
- **Volume paths:** a tenant id used as a path segment must be one `[a-zA-Z0-9_-]{1,64}` segment, and tenants differing only by case are refused (#1546). A symlink below the dynamic root is refused (#1512), and a symlinked root itself now works (#1533).

### Schedules and consolidation (#1525, #1539, #1553, #1562, #1564, #1565)

- **An operator-layer consolidation schedule reaches every tenant (#1525)**, each pass in its target's own tenant. Before, a `""` schedule saw only tenant `""`, so it never consolidated anyone signed in with the legacy token (`default`). Its `on_complete` hooks fire per tenant, and an operator change subscription sweeps every tenant (#1539).
- **That reach needs operator authority (#1562):** the def was saved by an admin (or in open mode / over stdio), or bootstrapped from the operator's yaml. A `""` def is not proof that the operator wrote it, since a run executing in `""` could author one.
- **The operator key is not spent in another tenant's sweep run** while `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` is on (#1564).
- **The sweep is fair (#1565):** tenants are interleaved, each pass gets a slice of the fire budget, and a budget, operator-key or pause deferral reads `skipped`, not `failed`.
- **Drained queue rows are pruned (#1553)** after 7 days.

### Stateful runs and the repeat guard (#1467, #1468, #1505, #1514, #1551)

- The kept memo also keeps the last action's result beside a Context result (#1468), shows a failed action as failed, and keeps only reference Context results, not live ones such as `time` or `state` (#1514). A kept failure drops its help hint once the help is read, and is capped at 8,000 bytes (#1551).
- A cut id ending in U+FFFD is refused as cut (#1467).
- **The repeat guard refuses a repeated call only when its result repeats too (#1505).** Polls and queue drains that return something new keep running; identical calls sent in one turn all run; every operator turn resets the count.

### Other fixes (#1439, #1440, #1441, #1475, #1510, #1513, #1517, #1519, #1521, #1524, #1526, #1534, #1537, #1542, #1555, #1560)

- **SQLite: tables that kept a tenant-blind key on upgrade are rebuilt (#1475).** On a database created before the tenant migrations, a second tenant writing the same old key failed with `UNIQUE constraint failed`, and a restore dropped its rows.
- **Hooks:** retention sweeps retired HookDef versions (#1439) but keeps those a live run pins (#1519); a manual compaction records its `pre_compact` decisions (#1440); the Web UI renders `hook_decision` as a readable line (#1441).
- **Channel hooks:** each claim has its own lease token, and a job whose lease ran out stops (#1510); a replica with hooks off delivers what waits for hooks instead of stranding it (#1513).
- **Global channels:** delete and purge take only the layers a channel owns (#1521); a tenant's first read gets its own messages from before the operator's cursor (#1526), and may repeat one (#1534).
- **Documents:** a subtree is re-indexed only when its header changes, one pass per document (#1517); a partial derived-unit set never reads as current (#1524); a body write racing a re-index no longer loses, and a type-only change re-indexes (#1542).
- **`PUT …/breakpoints` with only `review_ttl_seconds` keeps the arming**, and a long-poll `Channel op=await` reads as a channel wait (#1542).
- **Error bodies:** JSON error bodies are encoded, not `%q`-formatted, so every one decodes (#1537); `/v1/_metrics` errors are JSON with a `code` (#1555).
- `docs/TOOLS.md` is re-exported from the doc store (#1560).

**Bench (no runtime change):** derived search units show no measured benefit on PolicyQA (#1453); the shipped header and rerank hold on ConditionalQA, R@5 0.768 → 0.860 → 0.933 (#1495); a decision-model rerank is non-inferior at R@5 and faster, but its probabilities cannot gate (#1544).

### Behaviour changes / upgrade notes

- **Runtime remote memory backends and document sources need an allowlisted host.** A runtime-authored `kind: remote` MemoryBackendDef or DocumentSourceDef must have its `base_url` host on `LOOMCYCLE_HTTP_HOST_ALLOWLIST` or `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, for every author. An existing runtime def at an unlisted host stops dialing (memory falls back to in-process; Document `sync` fails), and so does a runtime fork of a yaml peer. A runtime def cannot name a literal private IP. Yaml-declared peers are unchanged (#1508, #1550).
- **Tenant authors lose free-form `api_key_env`.** A non-admin may name no credential, or a `key_per_tenant` `env_pattern` containing `{tenant_id}`, or fork a yaml peer with its `base_url` and credential unchanged. Open mode and the stdio `loomcycle mcp` count as the operator (#1550, #1562). Stored rows keep their `api_key_env`; review runtime defs that name one.
- **Tailnet / CGNAT (`100.64.0.0/10`) and the other non-public ranges are private now.** List them in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, which accepts CIDR (`100.64.0.0/10` admits the whole tailnet). Tenant hooks use `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST`; a change subscription to a tailnet callback needs `allow_private_host: true`. A malformed CIDR fails startup (#1547).
- **Config `principals:` tenants must match `[a-zA-Z0-9_-]{1,64}`.** A dotted tenant such as `acme.corp` now fails config load. Case-only tenant collisions are refused at mint, at config load and at boot, and so is a reserved `_shared` / `_ephemeral` tenant in any case (#1546).
- **A non-admin principal with no `tenant:` fails config load** (#1542). Set the tenant, or grant `substrate:admin`.
- **Runtime-authored `""` consolidation fan-outs lose their all-tenant reach until an admin re-saves them** (a fork or a hook edit); until then they consolidate tenant `""` only, and each fire logs why. Yaml schedules are unaffected. **Legacy schedule and webhook rows whose body names no tenant but tenant X owns now run in X** (#1562).
- **With `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` on, the operator-layer sweep does not spend the operator's key in other tenants.** A tenant with no key of its own and a keyed consolidator is not consolidated until it adds one (#1564).
- **Consolidation outcomes:** an over-budget, operator-key-refused or paused fire reads `skipped`, and a paused fire does not count toward `max_fires`. An explicit `LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY` now applies to code-js consolidators (#1565).
- **Webhook forks merge `user_credentials` and `user_credentials_from_env` key by key**, like schedule forks; an empty value removes a key. **A re-supply through an env var that is not allowlisted, or not set, leaves a restored def disabled** (named in `unusable_credentials`) (#1557).
- **Webhook triage:** an admin passes `?tenant=` to `POST /v1/_webhooks/{name}/test` and `GET …/recent-deliveries` to reach a tenant's webhook (#1532, #1549).
- **Drained `memory_pending` rows are deleted after 7 days**, hourly, on one replica (`LOOMCYCLE_MEMORY_PENDING_DRAINED_TTL_MS`; `0` disables). **Postgres migration 0089** builds a partial index once at upgrade, blocking writes to `memory_pending` while it builds (#1553).
- **`loomcycle restore` prints a separate `not restored …` line** for refused definitions and pointers and triggers disabled for credentials; the `restored …` line no longer carries those keys (#1554).
- **Snapshots carry many more sections.** Credentials are stripped from triggers, and a restored trigger without its credentials comes back disabled. A restored enabled schedule is active on the target: a snapshot is a copy, not a lease. A definition that restored before may now be skipped by re-validation, with a warning (#1493, #1499, #1530).
- **A resumed run stops rather than continue on something else:** its agent version deleted, its registered agent changed, its static agent removed, or a hook pin in another tenant. A resumed member of an admin's walk over another tenant's team fails closed, and a resumed resident child takes only a verdict (#1486, #1496, #1541, #1543, #1559).
- **Team walks:** a stored `timeout_ms` is now enforced (#1444). Walks started over `/v1/_teamdef` or gRPC run as the caller, so new walks' data lands under the caller, not `http-admin` (#1457). Teams promoted before this release run their subscription walks isolated (and, with the operator-key gate on, without the operator key) until promoted again (#1488, #1490).
- **Caller-sent `parent_context` walk, wave and board fields are dropped** (#1455).
- **SQLite:** on a database from before the tenant migrations, the first boot rebuilds 21 tables once, in one transaction; expect a longer first boot on a large `memory` table (#1475).
- **Postgres migrations 0086 to 0090** are additive: `runs.walk_id`, the team promoter columns, `usage_carry`, the `memory_pending` index and `runs.delivery_alt_key`.

**Adapters:** `@loomcycle/client` 1.101.0 adds `getRun`, `listWalkRuns` (`WalkRunsPage`), `reviewTtlSeconds` on `setRunBreakpoints`, `toolChoice` / `outputFormat` on retune, the `subagent_hold` event (`SubagentHoldInfo`), the run read fields (`interactive`, `awaited_state` with `input`, `awaited_on`, `replica_id`, `resident`, `resident_state`, `parent_run_id`), `state` / `state_visit` on `parent_context`, the run-state wait fields and `parent_run_id`, `RunSpec`'s `sourced_hooks` / `spawn` / `agent_version`, and `warnings`, `restored` and `paused_runs_resumed` on the snapshot responses. gRPC adds `GetRun` and `ListWalkRuns`, `Agent` fields 17–23, `AgentUsage.provider = 6`, `Event.subagent_hold = 18`, the retune fields (`RunInputRequest` 19/20, `RetuneRunRequest` 17/18), `SnapshotDescriptor.warnings = 8`, `RestoreSnapshotResponse.restored = 13` and `paused_runs_resumed = 14`, `RunStateEvent` 11–14 and `ParentContext` 10/11. The Python adapter's version is 1.101.0, with `get_run`, `list_walk_runs`, the retune fields, `SubagentHold` and the same read and snapshot fields.

## What's in v1.100.0

*Documents can carry derived search units, short texts about each chunk that a question can match; they are written by an operator pass and never returned in place of the chunk. A stateful run keeps what Context returned across its steps. Also: a run keeps the hooks it started with across a pause, a cut document id or a malformed path is refused instead of creating a second document, and steering works from any replica.*

Nine PRs, #1429 to #1437:
- #1432 and #1433 are derived search units (RFC DM C1, C2);
- #1434, #1436 and #1437 come from the 1.99.0 tool-usage eval;
- #1435 closes a hooks gap RFC DK recorded;
- #1429, #1430 and #1431 are fixes.

### Derived search units (#1432, #1433, RFC DM C1–C2)

For a document an operator opts in, a model writes short texts **about** each chunk: a description, atomic claims, and questions the chunk answers. A question can then match a unit's wording when it does not match the chunk's own.

- **A unit votes for its chunk and never takes a result slot.** The measured lesson behind the design: units stored as extra chunks crowded the reader's top five and cost answers.
  - A search collapses unit hits onto their chunks: the chunk takes its best rank, appears once, and carries `matched_unit: {kind, text}`.
  - A unit whose chunk is gone is dropped.
  - The rerank ranks the resolved chunks.
- **Which documents:**
  - a document's own `index_units` (on its root chunk) wins;
  - otherwise, a document named under a Path subtree the operator marks in `memory.unit_generator.subtrees`;
  - otherwise it is never touched.
  - The memory trees (`/facts`, `/memory`, entity-bearing documents) are refused whatever is set, and reported in the pass's result.
- **Generation is an explicit operator pass, `POST /v1/_document/derive_units`:**
  - `dry_run` is **true** by default;
  - `limit` counts model calls per request (default 25), and `after` is a resume cursor;
  - it needs `memory.unit_generator`, shaped like the embedder, with no default model.
  - Staleness is by the hash of the body a unit was written from, so a pass over an unchanged scope makes no model call, and editing one body rewrites exactly that chunk's units.
- **Lifecycle:** units go with their chunk (delete, prune, the dead-link sweeper, erasure), are re-indexed with a rename or move, and count toward no quota.
- **Guard:** the `doc.unit:` key namespace is server-only; Memory writes refuse it.
- **Per agent:** `memory_units: false` ignores units.
- `POST /v1/_memory/search` and `@loomcycle/client`'s `MemorySearchEntry` carry `matched_unit`.

### A stateful run keeps what Context returned (#1437)

A stateful run's model reads only its latest observation, so a help article it read two steps ago was gone. In the 1.99.0 eval the stateful models re-read the same articles again and again: ornith read two Document articles 3 times each in one run, and deepseek read `History/recap` 4 times in another.

Every successful Context result is now kept and shown on each later step, in a "what you already read" section, capped at 16,000 characters with the oldest dropped first. A help topic is kept **once** however it was asked for (`Memory/set`, `Memory.set`, an alias, another key order): its identity is the topic Context resolved.

### A run keeps the hooks it started with across a pause (#1435)

A run resumed after a pause used to re-resolve its hooks from the current definitions, so a HookDef promoted, created or edited while it was paused took effect mid-run.

- At start, a run now records `pinned_hooks` in its `run_config`: each HookDef lookup as `tenant/name@version → def_id` (a lookup that found nothing is pinned as `""`), and a fingerprint of the agent's own hooks.
- These are identities, not content, so no webhook URL or header lands in the run's spec.
- On resume the pins answer every lookup. The run **stops with an error** if its agent's hooks changed while it was paused, or a pinned version was deleted.
- A run recorded before this release resolves its hooks as before.

### Fixes

- **A cut document or chunk id is refused as cut (#1436).** Models copied a 32-character id and cut it short (`6fbd5245`, `36d88349ac1730a178...`, `abc26269b??`). They got "not found in this scope" and created a second document.
  - A bare hex run of 8–31 characters, or a hex prefix followed by `...`, `…` or `?`, is now refused before any lookup: "is cut short — a document or chunk id is 32 hex characters".
  - The check reads the characters only, so it says nothing about whether an id exists.
- **A malformed document path is refused before anything is written (#1434).** `create_document` used to create the document, fail to name it, and answer success with a `path_warning`, leaving it outside the Path tree. Two models then created a second document. The refusal suggests a corrected path (`/documents/zz-eval-Trip-plan`); `import_md` and `import_canvas` write nothing on a bad path either.
- **Model aliases work in `memory.embedder` and `memory.reranker` (#1429).** An alias was sent to Ollama as a model name, so every embedding and every Memory or Document search failed. A `model_pattern` alias there is refused at load.
- **Steering reaches a run from any replica (#1430).** `POST /v1/runs/{id}/input` (and its gRPC and MCP twins), retune and the run config reads answered 404 unless the call landed on the replica that owns the run. They now route to the owner.
- **A fan-out child that finishes after a snapshot passes its parent's `subagent_stop` hooks (#1431).** Pausing at the right moment let an awaited child's result reach the parent unchecked.

### Changes to note

- **`create_document` with a malformed `path` now fails**, where it used to create the document with a `path_warning`.
- **A value that looks like a cut id is refused** in the Document tool's id fields.
- **A resumed run stops if its hooks changed while it was paused**, where it used to continue under the new ones.
- **A stateful run's prompt is longer** once it has read help: at most 16,000 characters of kept Context results.
- **`memory.embedder` / `memory.reranker` with a `model_pattern` alias now fails at load.**

**Adapters:** `@loomcycle/client` 1.100.0 adds `memory_units` on the AgentDef overlay, `matched_unit` on a memory search entry, and `pinned_hooks` on `RunSpec`. The Python adapter's version is 1.100.0.

## What's in v1.99.0

*Channels can carry hooks that decide every message before a reader sees it, and a hold can ask a person. Document search finds chunks by their document and section, by their words as well as their meaning, and can rerank. Also: global channels are split by tenant, a DeepSeek thinking model runs in stateful mode, and a malformed call carries the help its run skipped.*

Seventeen PRs, #1411 to #1427:
- #1412, #1413, #1415, #1417, #1420 and #1421 are channel hooks (RFC DK-P5a–P5f); #1422 is the fixes from the RFC DK code review;
- #1426 splits global channels by tenant, from the same review;
- #1411, #1414, #1416, #1418 and #1419 are document retrieval quality (RFC DM);
- #1423 to #1425 and #1427 come from the 1.98.0 tool-usage eval.

### Channel hooks: every message decided before a reader sees it (#1412, #1413, #1415, #1417, #1420, #1421, RFC DK-P5)

A channel's `hooks` run on the new `channel_publish` event. Each message published to the channel, by anyone, waits unseen until its hooks decide it:
- `release` — delivered as written;
- `release` with `updated_body` — delivered rewritten;
- `drop` — removed;
- `hold` — kept from every reader until a person decides.

A hook's answer that does not say exactly one of these is a failed hook, not a release, so a mistake in a gate cannot let a message through.

- **Where hooks attach:**
  - a channel declared in the operator's yaml (`channels.<name>.hooks`), which resolves in the operator's tenant;
  - a channel created at runtime (create/update take `hooks`; `{}` removes them), which resolves in the tenant that created it, whoever publishes. A reference is checked when it is attached: it must be a HookDef that answers `channel_publish`, or the call gets 400 `channel_hooks_invalid`.

  A `_system/` channel, a `documents/` feed and a `publisher: system` channel cannot carry hooks.
- **The worker** (one per replica, `internal/channelhooks`):
  - claims messages under per-message leases, runs the chain a hook at a time, and saves progress between hooks;
  - settles each message exactly once; a worker that lost its lease settles nothing;
  - resolves the chain at decision time. A changed chain starts over from the first hook on the original body. A HookDef that no longer resolves fails closed, and so does a hooked channel that disappeared;
  - `fail_mode: open` lets a message through. `closed` retries with backoff until the message's deadline (15 minutes after publishing, within its TTL), then drops it;
  - a Starter's per-run result is never lost: a drop delivers it as a `status: error` result, and a rewrite keeps its wave fields;
  - bounded at 16 messages per replica and 4 per channel, so **delivery may be reordered**;
  - dials a tenant's webhook through the private-address guard, and resolves `$cred:` in the definition's tenant, never the publisher's.
- **A hold asks a person.** The message's first ask opens a `hook:<name>` run, filed in the tenant whose definition carries the hook (user `_system`):
  - that tenant's operators answer it the way they answer any interruption (Web UI, `POST /v1/runs/{id}/interrupts/{iid}/resolve`, MCP `interruption_resolve`);
  - `release` goes on to the next hook, while `drop`, a decline or a timeout drops the message;
  - answers are kept in the message's journal. An ask left pending by a worker that died is cancelled and asked again under the same run;
  - a code hook's own `Interruption.ask` works the same way;
  - a waiting ask gives up its concurrency slot.
- **One writer decides every message's fate** (`channels.StorePublisher.Write`). The Channel tool, Document's change feed, the scheduler, webhooks and the connector all go through it; a census test fails on any other writer. A hold is now honoured on every surface the same way.
- **What a publish reports:** `awaiting_hooks` on every transport, plus `held` and `dropped_oldest` where gRPC and TS lacked them. Channel descriptors gain `hooks`, `held_count` and `awaiting_hooks_count`.
- **Records:** decisions go to `_system/channel_hooks/decisions` at tenant scope. `/metrics` gains `loomcycle_channel_hooks_in_flight` and `loomcycle_channel_hooks_decisions_total{decision}`.
- **Wire:** `hook_decision` gains `channel` and `message_id` and the decisions `release`, `rewrite_body` and `drop`, on SSE, gRPC, TS and Python.
- **The Web UI:** the channel editor has a Hooks control and the tenant scope; the channel page shows the hooks badge and the held and awaiting counts.
- **Configuration:**
  - `LOOMCYCLE_CHANNEL_HOOKS` turns them on. **Off (the default), a channel's hooks are skipped:** the message is delivered at once, or held if the channel also holds, and the boot log says so. A runtime channel still accepts hooks while they are off, so turning them on enforces them.
  - Tuning: `LOOMCYCLE_CHANNEL_HOOKS_CONCURRENCY`, `…_PER_CHANNEL`, `…_MAX_WAIT`.
- **Postgres migrations 0084 and 0085** are additive (message hook context, the `channel_hook_state` table, `channels.hooks`).

### Fixes from the RFC DK code review (#1422, #1426)

- **A run's own hooks cannot name a credential.** A `$cred:` header on an inline webhook in a run request is refused; only a definition, reviewed on save, can name one.
- **An agent in a run cannot change an AgentDef's hooks.** A create or fork from inside a run must keep the parent's `hooks` and `tool_hooks` exactly.
- **A run's added hooks sit outside the agent's own:** the agent's `pre` gates decide last and its `post` hooks act first, so an added `post` hook sees the agent's redaction, never the raw output.
- **A resident child goes through its parent's subagent hooks.** `Agent op=open` now runs `subagent_start`, and every hand-back (`open`, `send`, `poll`, `cancel`) passes `subagent_stop`.
- **A tenant's channel list shows only its own:** no yaml webhook URLs or headers, and stats per tenant.
- **A HookDef reference falls back to the shared tenant only when it is not found**, never on a retired version or a store error.
- **A team walk's hooks resolve in the team's tenant**, and the walk's run is stored in that tenant.
- **Global channels are split by tenant over a shared operator layer (#1426).** Before, every message on a `global` channel lived in one keyspace, so a tenant could publish under the name of the operator's hooked or held global channel, through its own unhooked channel, where every tenant reads.
  - Now a tenant writes and reads its own layer; every tenant also reads the operator's layer.
  - The operator's writers, and an admin publishing on a yaml channel, write to the operator's layer.
  - Cursors are per tenant; release and purge act on the caller's layer, and an admin's purge empties every layer.
  - **No data migration:** everything written before is in the operator's layer, which every tenant still reads.
- **A Web UI fork can clear hooks.** Removing the last hook in the agent or team editor now removes it; it used to keep the parent's.

### Document retrieval: by document and section, by words, and reranked (#1411, #1414, #1416, #1418, #1419, RFC DM)

- **Each chunk is indexed under its document and section path (#1411):** `<document title> — <heading path>` then its content. It is derived at index time, never stored or shown. On the NQ probe it lifted R@5 from 0.912 to 0.935 (p=0.0046).
  - A bodyless root is no longer indexed.
  - The admin re-embed now keeps each chunk's index text and an image's description, where it used to embed the raw JSON value.
- **Renaming or moving a chunk re-indexes its subtree (#1414).** A heading that derives to no text has its stale vector removed.
- **`POST /v1/_document/reindex` brings an existing store up to date (#1414).** It is operator-admin, takes `scope`, `dry_run` (default **true**), `limit` and an `after` cursor, and makes no embed call for a chunk that is already current, so it is resumable.
- **`Document op=search` runs the memory search pipeline (#1418):** vector and full-text, fused. A chunk found by its words, such as an error code, now surfaces; where the word index exists, which needs Postgres + pgvector. Hits add `rank_score`, the value the order is built on.
- **Opt-in listwise rerank (#1416, #1419).** On NQ it moved R@1 from 0.515 to 0.754.
  - The operator declares `memory.reranker`, shaped like the embedder, with `max_concurrent` (default 4). There is no default model.
  - An agent opts in with `memory_rerank: {enabled, candidates, max_chars}`. There is no tool parameter.
  - It applies to `Memory op=search` and `Document op=search`, and reports `reranked` / `rerank_reason`.
  - Every fault keeps search's own order.
  - Each call is booked on the searching run's token ledger.
  - With `base_url` overridden and no `api_key_env`, it answers only to `LOOMCYCLE_RERANKER_API_KEY`, so a tenant's own provider key never reaches the operator's endpoint.

### From the 1.98.0 tool-usage eval (#1423, #1424, #1425, #1427)

- **A DeepSeek thinking model now runs in stateful mode (#1423).** DeepSeek refuses a `required` or named `tool_choice` in thinking mode, and the stateful loop forces one on every step, so `chat/medium` on `deepseek-v4-flash` failed before its first answer. The driver no longer sends a forced choice in thinking mode (the V4 family, reasoner, r1, `*-pro`, or any effort hint), as the Anthropic driver already does under thinking. A failed stateful step now says when the call was not enforced.
- **A malformed call carries the help its run skipped (#1427).** The in-band error object gains a `hint` key: the failing operation's help article (or the tool's, when the operation was wrong), the text `Context op=help` returns.
  - It is attached only on validation or unclassified failures, only while the run has read none of that tool's help, and once per tool and operation.
  - A stateful run gets it on every such failure until the help is read, since its model sees only its latest observation.
  - It is attached in runs only, and not on the SSE, gRPC or MCP `error_info`.
- **`Memory/set` says text is a plain string (#1424).** "`value` must be valid JSON" led a model to send `"\"text\""`, which stores the quote marks.
- **`create_chunk` checks the document before its parent (#1425).** A wrong scope used to be refused as "no such parent_id … pass the document's root_chunk_id", even when that was exactly what was passed.

### Changes to note

- **A tenant no longer sees another tenant's messages on a global channel**, and a legacy admin reading through the HTTP routes sees the operator's layer and its own tenant's (`default`), not other tenants'.
- **Run request hooks (new in 1.98.0) changed:** a `$cred:` header in them is refused, and they now run outside the agent's own hooks (added `pre` hooks first, added `post` hooks last), where 1.98.0 ran them after the agent's.
- **A channel's hold set during a run applies to the run's next publish**, and **a fault reading a channel's definition refuses the write** (it used to deliver). A `deliver_at` of either reserved instant (2200-01-01, 2200-01-02) is refused.
- **A snapshot restores a tenant's channel messages under that tenant**; before, they restored into the shared tenant.
- **Existing document chunks keep their old index text until reindexed.** Run `POST /v1/_document/reindex` (a dry run by default) to update them; new and edited chunks are indexed the new way at once.
- **`Document op=search` ordering changes** where full-text search exists, and every hit carries `rank_score`.
- **A failed tool call's in-band text may carry a long `hint` key** (1–10K characters), once per tool and operation.

**Adapters:** `@loomcycle/client` 1.99.0 adds the channel-hook wire (the `hook_decision` fields and decisions, `ChannelHookCall` / `ChannelHookResult`, `awaiting_hooks` / `held` / `dropped_oldest` on a publish, the descriptor fields, `hooks` on `createChannel` / `updateChannel`) and `memory_rerank` on the AgentDef overlay. The Python adapter's version is 1.99.0, with the same channel wire.

## What's in v1.98.0

*A failed tool call now tells the model what kind of failure it was, on every interface. Runs and teams may add hooks, and the Web UI edits them. Also three existence oracles closed, a hold bypass in the webhook relay, and fixes from measuring local models, including consolidator fact writes that had been refused since v1.95.0.*

Seventeen PRs, #1393 to #1409:
- #1404, #1405 and #1406 are structured tool errors (RFC DA amendment 1);
- #1393, #1395, #1400 and #1402 finish hooks-in-definitions (RFC DK-P4c, P4d);
- #1407 and #1409 are the prerequisites for channel hooks (RFC DK-P5);
- #1408 closes three isolation gaps;
- the rest come from measuring local models, plus one erasure fix.

### A failed tool call says what kind of failure it was (#1404, #1405, #1406)

A model used to get a failed call back as bare text. It could not tell "fix your input" from "this will never work" from "try again later", and the correct call format was prose tacked onto the error.

- **What the model receives.** A failed call's `tool_result` is one JSON object:

  ```json
  {"isError":true,"error":"…","errorCategory":"validation","isRetryable":false,
   "description":"…","correctCallFormat":{"tool":"Document","op":"create_chunk","example":{…}}}
  ```

  - `errorCategory` is `validation`, `business`, `permission` or `transient`. `description` is the next step.
  - `correctCallFormat` comes from the operation's help article. It is attached only where the call's shape may be the cause: a `validation` failure, or an unclassified one.
  - Keys with nothing to say are left out, and a success is unchanged.
  - The stateful loop's own refusals use the same object, without the old `ERROR:` prefix.
  - The object is rendered before the event is stored, so a resumed run replays the same text.
- **Every interface carries it.**
  - Every builtin call from MCP, HTTP and gRPC now goes through the same dispatcher a run uses. It gets the argument checks, the classification and the call format.
  - **MCP:** `structuredContent`.
  - **HTTP:** the `tool_refused` envelope.
  - **gRPC:** `ErrorInfo.call_format`, and `SubstrateResponse.error_info` on the substrate RPCs.
  - **SSE:** a failed `tool_result` event carries `error_info`.
  - **Adapters:** TS `ErrorInfo.correct_call_format` and Python `ErrorInfo.call_format`, with new `CallFormat` / `CallRef` types in both.
- **Eight tools are classified.** Every failure of Document, Memory, History, Path, Channel, Agent, Context and Skill carries a category (575 call sites).
  - The category is chosen where the code knows the reason, never inferred from the text.
  - A wrapped store error is classified by its type, so a database outage never reads as "fix your input".
  - Every message is byte-identical to before.
  - A test fails if any of these tools gains an unclassified failure.
  - The other builtins are not classified yet: their failures carry no category.
- **A row the caller may not see reads exactly like a missing one**, category and next step included, never `permission`. `permission` is only for the caller's own grant, where no row is read.

### Runs and teams add hooks; the Web UI edits them (#1393, #1395, #1400, #1402, RFC DK-P4c/P4d)

Since v1.97.0 a run fires exactly its AgentDef's hooks. A run may now **add** hooks, never remove one.

- **The run request takes `hooks` and `tool_hooks`**, in the AgentDef's shapes.
  - Supported on HTTP `POST /v1/runs` and the continuation, MCP `spawn_run` / `spawn_runs`, gRPC `hooks_json`, TS `hooks` / `toolHooks`, and Python `hooks=` / `tool_hooks=`.
  - Additions run after the agent's own hooks. They resolve in the run's tenant and never widen hosts.
  - A `tool_hooks` entry for a tool the agent lacks stops the run before it starts, because the caller believes a gate is there.
- **Sub-agents inherit** their parent run's additions. A resumed run fires them again.
- **A TeamDef adds hooks:**
  - a state's hooks go to every run it starts: its agent, fan-out members and consolidator;
  - the team's top-level `hooks` belong to the walk, `run_end` only;
  - a walk whose hooks cannot be resolved does not start;
  - a TeamDef fork no longer drops `hooks`, and a new test makes every Definition field survive a fork.
- **The Web UI:**
  - a Library Hooks tab to create, fork, promote and retire HookDefs;
  - hook editors in the agent editor (both surfaces) and the Teams editor;
  - a save stops on an entry the runtime would refuse.
  - The shared editor is in `@loomcycle/def-fields` 0.2.0, and `@loomcycle/library` 0.6.0 uses it.
  - Also fixed: "+ add entry" on any key/value field did nothing.

### Three existence oracles closed (#1408)

A row belonging to another tenant or user must read exactly like a missing one. Three places did not:

- **Memory `cursor_advance`** told another user's chat apart from a missing chat, so a guessed chat id could be probed.
- **The Skill "unknown skill" hint** listed skill names from every tenant. It now lists only the caller's tenant's.
- **Context `lineage` and `evaluations`** read any def by id with no tenant check. That exposed another tenant's def name, description, fork tree and evaluation scores.
  - Both ops now see the run's own tenant, the shared base, or every tenant for `substrate:admin`.
  - `lineage` also skips other tenants' forks of a shared def.

### Channels: definitions resolved in the right tenant, and Starter reads fixed (#1407, #1409)

Prerequisites for channel hooks.

- **A tenant's webhook relay delivered past the tenant's own `hold:` channel.** The hold was looked up in the shared tenant. It is now resolved in the tenant the message is published in.
- **A tenant's schedule could not publish to that tenant's runtime channel**, and a `scope: tenant` channel could never be a schedule's target. Both work now.
- **The MCP `channel` tool could reach no channel:** every one read as "not declared". It now uses the caller's tenant's channel catalog, with the same grants and confinement as a run.
- **A Starter reads from its committed cursor and waits for its batch.**
  - It re-read acked messages on every wave.
  - It ignored `want` and `wait_ms`.
  - A promoted team started a new walk on every subscription tick for work already done: 6 walks for one message.
  - `ack: after_read` never acked; it now acks before the wave.

### Fixes from measuring local models

- **Consolidator fact writes were refused since v1.95.0 (#1398).** Document's schema did not declare `source_session_id` or `source_run_id`, so the unknown-argument check refused every call passing one. That included the bundled memory consolidator's transcript-path facts, which were never written. A new test requires every argument a builtin decodes to be in its schema.
- **Runtime-created agents get the Context tool (#1394).** An agent created with `AgentDef create` or `register_agent` ran without Context. It had no help and no correct-call examples, and unknown arguments were not refused. The default is applied when the agent resolves and is not stored, so no definition's hash changes.
- **History `get` reads a chat in pages of turns (#1396):** `offset` / `limit` count turns, and `from` / `to` select a time range. Every page says where it sits (`turns_total`, `has_more`, `next_offset`). A page inside a run is capped to fit the run's context window. One long chat had been 33K–51K tokens and overflowed a 32K window.
- **History `list` returns 10 chats by default inside a run (#1399)**, down from 50, with `has_more` / `next_offset`. Off-run callers keep 50.
- **An argument that belongs to a sibling operation is refused (#1401).** For example, `parent` on `create_chunk` (whose argument is `parent_id`) used to be dropped silently, so the chunk landed under the root. The refusal names the op's own near name. All 141 operation help articles were audited against their handlers, adding 22 arguments.
- **The third identical call in a row is refused (#1403).** A local model re-sent one successful save 36 times. Another call in between, or any change to the arguments, starts the count over. A model that keeps sending it is stopped by the existing failed-call guard.

### Erasure previews count what the erasure deletes (#1397)

The erasure report, its dry run and the directory's per-subject inspection counted a subject's memory through a listing capped at 100 rows. That listing also skipped expired and superseded rows. A real subject's preview read "100". The erasure itself was always complete. A new store count matches exactly the rows the erasure deletes.

### Changes to note

- **A failed `tool_result`'s text is now a JSON object**, not bare text, and the stateful loop's `ERROR:` prefix is gone. Anything that parsed that text should read `errorCategory` / `error` instead.
- **SSE `error_info` now uses its declared keys:** `category`, `is_retryable`, `description`, `retry_after_ms`. It used to go out as Go field names (`Category`, `Retryable`, `RetryAfter` in nanoseconds), so a TS consumer reading `error_info.category` got `undefined`. Stored events in the old shape still decode.
- **`Context op=evaluations` with an unknown `def_id` now refuses** instead of returning a zero-count aggregate.
- **History `list` inside a run** returns 10 by default.
- **A Starter with `ack: after_read`** now acks, as documented, so its batch is not redelivered.

**Adapters:** `@loomcycle/client` 1.98.0 adds run `hooks` / `toolHooks`, `ErrorInfo.correct_call_format`, and exports `ErrorInfo`, `CallFormat`, `CallRef` and `HookDecisionInfo`. The Python adapter's version is 1.98.0, with `hooks=` / `tool_hooks=` and `ErrorInfo.call_format`.

## What's in v1.97.0

*⚠️ BREAKING: hooks move onto the run, and the hook registration API is removed. Also a data-loss fix in the dead-link sweeper, credentials in hook headers, and chat agents that can write for the whole tenant.*

Five PRs. #1387 is RFC DK-P4b, the switch-over; #1388 is its precondition for migrating existing hooks; #1389 fixes live document chunks losing their text; #1390 and #1391 come from measuring local models.

### ⚠️ A run carries its AgentDef's hooks; the registration API is removed (#1387, RFC DK-P4b)

A hook used to be registered at runtime into one process-wide registry, and it fired on every run in the tenant. Now an agent's **definition** carries its hooks. A run takes them when it starts and fires exactly those.

- **Attaching hooks:**
  - An agent's `tools` entry may be `{name, hooks: {pre, post}}` for that tool's own hooks.
  - The agent's top-level `hooks:` holds run events, and hooks for every tool.
  - Each entry is a HookDef name (`gate`, or `gate@3` to pin a version) or an inline webhook `{name, url, fail_mode, timeout_ms}`.
  - It works the same in yaml and in the AgentDef overlay.
  - Hooks are content: they are part of the definition's hash.
- **Checked when an AgentDef is saved:**
  - every event is a known one, and a tool's hooks use only tool events;
  - a tool's hooks are for a tool the agent has;
  - each HookDef exists and answers its event;
  - `match.tools` covers the tool it is attached to.
- **Order:** a tool's own hooks run first, then the agent's, each in listed order. `post` chains run reversed. The old tenant-first ordering is gone, since a run's chain is exactly what its definition named.
- **Where HookDef references resolve:**
  - They resolve in the tenant that owns the definition, then the shared tenant; never in the run's tenant. A tenant cannot shadow an operator's hook by reusing its name.
  - A run whose hooks cannot be resolved (a HookDef deleted or retired) **stops before any model call**.
  - A sub-agent fires its own definition's hooks, not its parent's.
- **Host widening** is decided per hook. It needs both the operator permit and a hook from an **operator-authored** definition.
- **Removed:**
  - `POST/GET/DELETE /v1/hooks`;
  - MCP `register_hook` / `list_hooks` / `delete_hook` (the catalogue drops to 53 tools);
  - the gRPC `RegisterHook` / `ListHooks` / `DeleteHook` RPCs;
  - TS `registerHook` / `listHooks` / `deleteHook` and `HookNotFoundError`;
  - Python `register_hook` / `list_hooks` / `delete_hook` and `HookNotFoundError`;
  - the cluster hook registry.

### Credentials in hook headers (#1388)

A secret a webhook hook needs had nowhere to go but its URL. The URL is stored in the definition body, where anyone who can read the tenant's AgentDefs can see it.

- **`headers`** are now accepted on inline webhooks and on HookDef `http` bodies.
- **A header value may name a credential**, `$cred:<name>` (or `$ghapp:`). It is resolved for the run on each call, by the resolver MCP server headers use, and the value is registered with the redactor. The definition stores only the reference.
- **A credential that does not resolve fails the call.** The literal reference is never sent, and the hook's `fail_mode` decides the outcome.
- **Validation:**
  - header names must be HTTP tokens;
  - the call's own headers (`Content-Type`, `Accept`, `Host`, …) and line breaks in a value are refused;
  - a code-js body takes no headers.

### ⚠️ The dead-link sweeper deleted live chunk text past 10,000 chunks (#1389)

The hourly dead-link sweeper runs by default and not as a dry run. In any scope holding more than 10,000 chunks, it **deleted the bodies of live chunks**. The chunks stayed in the tree with no text, so search and readers silently stopped seeing them.

- **Cause:** the set of live chunk ids was read in one SQL Memory query, which stops at `SqlMemMaxRows` (default 10,000). The truncation flag was ignored, so every chunk past the cap looked unreachable. Separately, the body listing asked for "no limit", which both stores read as **100**.
- **Fix:**
  - the live set is read by keyset paging until it is complete;
  - the body listing has an explicit ceiling;
  - the sweep **refuses to reconcile** when a listing is still truncated.

### Chat agents may write for the whole tenant (#1390)

Every `chat/*` agent (`chat/medium`, `chat/local`, `chat/local-small`) now holds `memory_scopes: [user, tenant]` and `sql_scopes: [user, tenant]`. "Save a note my whole team can read" works, for Memory notes and for Documents; a tenant-scope Document needs both grants.

⚠️ This reverses a deliberate default: one allowlist covers reads and writes, and these agents have WebFetch, WebSearch and Read. A page or file they read can ask them to store something that every user in the tenant then sees. To narrow an agent back, re-declare both keys as `[user]` in an operator overlay.

### Context says what it is for in the tool list (#1391)

The tool list in an agent's prompt shows each tool's first sentence, and Context's was "Runtime introspection.". On local models, 0 of 20 runs read a tool's help before calling it. Its entry now reads "Runtime and self introspection, with call instructions for all your tools (op=help) and your scopes (op=permissions)." A second line points past chats to History.

### Upgrade notes

- **⚠️ BREAKING — hooks.** Registered hooks stop firing: **migration 0083** (Postgres) drops the `hooks` table and the registration API is gone. Before upgrading, move each registered hook into the definition of each agent it gated: a HookDef reference, or an inline webhook on the tool entry. Put secrets in `headers` as `$cred:` credentials, not in the URL.
- **Host-widen permit:** the key is still `hooks.permit_host_widen.owners` (`LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS`). Entries now name `[tenant:]hook-name`, and the hook must come from an operator-authored definition.
- **Dead-link sweeper:** a scope past 10,000 chunks on an older version may already have lost chunk bodies; this release stops further loss. Until you upgrade, set `LOOMCYCLE_DEADLINK_GC_MS=0` or `LOOMCYCLE_DEADLINK_GC_DRY_RUN=1`.
- **Chat agents write the tenant plane by default.** Narrow them with an operator overlay if they are driven by untrusted input.
- **The adapters are bumped to 1.97.0.** The hook registration methods are removed from TS and Python, and TS gains `InlineWebhook.headers`.

## What's in v1.96.0

*Hooks become reusable, versioned definitions, and two stateful and looping failures from measuring local models stop costing whole runs.*

Three PRs. One is RFC DK-P4a (HookDef); two come from the same local-model measurements as v1.95.0.

### HookDef — reusable, versioned hook definitions (#1382, RFC DK-P4a)

A hook was registered one at a time and lived only as that registration. A **HookDef** is one hook as data, versioned and tenant-scoped like the other substrate Defs.

- **The definition:** `event` (the dispatcher's phase names), `match.tools`, a code-js or http body, `fail_mode`, `timeout`, and a `description` shown to whoever the hook stops. Validation refuses what the dispatcher could not run; the content hash is taken over the normalized definition, so two that behave the same hash the same. Names cannot contain `@` or `:`, the separators references and permits use.
- **Lifecycle:** `create` / `fork` / `get` / `list` / `promote` / `retire` / `verify` / `delete`, mirroring TeamDef. A code-js body is compiled on write, and refused without code hooks enabled. A fork's parent must be in the caller's own tenant (admin included), so a body never copies across tenants. **No agent may write the hook that gates it:** the tool refuses a call that carries a run id.
- **Every transport:** `POST /v1/_hookdef` and `GET /v1/_hookdef/names` (tenant-confined like AgentDef, so a tenant's non-isolated members may author too), the gRPC `HookDef` RPC at `ScopeTenant`, the MCP meta-tool `hookdef`, TS `hookDef()` and Python `hook_def()`.
- **Snapshots carry HookDefs** (`hook_defs` + `hook_def_active`), so a restored agent or team is gated by the hooks it named. An older snapshot restores with none.
- **Migration 0082** (Postgres) adds `hook_defs` and `hook_def_active`.

### A stateful answer written as prose is kept (#1384)

Measured on the lab deployment (`ornith-1.5:35b` behind `chat/local`): a stateful step found the right value and wrote it as plain text instead of an `emit_state` call, did so again when re-prompted, and the run failed with the correct answer discarded.

- The re-prompts still come first, on the same `max_patch_retries` budget. Only when it is spent is the last reply taken as `{done: true, final: <text>}`, which ends the turn exactly as a real final does (the interactive park included).
- Not under `on_invalid_patch: fail`, and not for a reply with no text, which fail as before.
- Trade-off: a model whose prose is only a plan ends the turn with that plan rather than an error; the operator sees what it said either way.

### A run that keeps re-sending a failed call is stopped (#1385)

Measured on the same deployment: a tenant write refused as not granted was re-sent, unchanged, until the run's 10-minute deadline.

- The dispatcher counts failures per exact call (tool name + canonical arguments). The first two failures run as usual. After that the call is **not run**, and the model gets a non-retryable "this exact call has already failed N times … change the arguments, take a different approach, or tell the user it cannot be done", with the operation's correct example.
- After three such refusals the run ends with the new stop reason **`repeated_failed_call`**, in both the append and the stateful loop.
- Changed arguments are a different call; a success clears that call's count. It covers builtin, MCP and lazily registered tools alike.

### Upgrade notes

- **Migration 0082** (Postgres) adds the HookDef tables. It runs at startup.
- **New stop reason:** `repeated_failed_call`. A client that switches on stop reasons should know it.
- **Behaviour change (#1384):** a stateful run whose model answers in prose after the retry budget now completes with that text instead of failing.
- **Behaviour change (#1385):** the third identical failed tool call in a run is refused without running.
- **The adapters are bumped to 1.96.0** (TS `hookDef()`, Python `hook_def()`).

## What's in v1.95.0

*Two isolation gaps closed, five more hook phases, and four fixes from measuring local models — a stateful run keeps its task, and an unknown tool argument is refused instead of silently dropped.*

Eight PRs. One finishes the first half of RFC DK (hooks v2), two are review fixes for RFC DI and RFC DJ that close two isolation gaps, one opens code hooks to tenant operators, and four come from measuring the lab deployment's local models against the new tool help.

### ⚠️ Security: an isolated member saw and answered other users' runs (#1376, #1377)

An isolated member (`substrate:user` alone) is confined to its own sessions, but two sets of reads checked only the tenant.

- **Run content (#1376).** `GET /v1/runs/{id}/prompt`, `GET /v1/agents/{id}` (result, spec, draft), gRPC `GetAgent`, MCP `get_run`, the event replays (`GET /v1/runs/{id}/stream`, gRPC `StreamRun`) and gRPC `GetTranscript` returned another user's run in the same tenant, prompt and answer included. One rule (`auth.OwnedRowVisible`) now decides all of them: for an isolated member the row's user must be the caller; any other principal keeps the whole-tenant view. A refusal is the opaque not-found a missing run gets.
- **Run questions (#1377).** The interruption resolve paths (HTTP/gRPC `ResolveInterrupt`, MCP `interruption_resolve`) and the two listings (a run's questions, a user's inbox) let an isolated member read and answer a question on a colleague's run. They now apply the review verb's rule: in the caller's tenant and, for an isolated member, its own session.
- **Also in #1376:** subject erasure now finds a subject's draft (configured) runs and archived chats, which it missed, so an erased subject's drafts no longer survive holding their prompt; and a run's content survives NUL bytes.

### Subagent, compaction and run_end hooks (#1374, RFC DK-P3b)

Five more run phases, selected by agent like `agent_start` / `agent_stop`:

- **`subagent_start` / `subagent_stop`** — the parent's view of a child the Agent tool spawns (one-shot and `parallel_spawn`). Either may deny (the child is not started / its result does not reach the parent) or add context (to the child's prompt / to its result). Each decision is recorded on the parent's stream.
- **`pre_compact`** — may deny a compaction. In the loop a denial is a declined compaction with the new reason `denied_by_hook`, so it is never silent.
- **`post_compact`** and **`run_end`** — observe only: run off the caller's path, bounded at 30 s, answers ignored, failures logged. A code body there may notify but not ask.

### Tenant operators may author code hooks (#1377)

A tenant operator registers code hooks for its own tenant's runs under `LOOMCYCLE_CODE_HOOKS_ENABLED` alone. The separate `LOOMCYCLE_CODE_HOOKS_TENANTS` opt-in that #1374 added is removed. A hook's question is its run's user's to answer (the user is the main actor; a hook gates the agent, not its user).

### Review hold fixes (#1375, RFC DJ)

- **A reviewer's reject on the last allowed iteration no longer completes the run on the refused answer.** A revise with no iteration left ends the run `rejected`, and an `agent_stop` block with no iteration left fails it `stop_blocked`.
- **A verdict given while the runtime is paused waits for the pause to lift** instead of ending the run under the pause. A verdict that waited out the pause wins over a deadline that passed in it.
- A reviewed spawn reports its status and answer as its row does.

### From measuring local models (#1378, #1379, #1380, #1381)

The lab deployment's local models (`gpt-oss`, `qwen3.6`, `ornith-1.5`) were measured on a fixed task set against 1.92.0, 1.93.0 and 1.94.0. The failed-call example worked — 8 of 11 retries after it were correct — but four failures remained that the help could not reach.

- **An unknown argument to a documented tool is refused (#1378).** Tools decode into a struct and dropped an undeclared argument silently, so `text` for `body` saved a chunk with an EMPTY body and reported success, and no error ever showed the correct call. The dispatcher now refuses such a call before the tool runs ("unknown argument "text" — nothing was done"), with the operation's correct example, a hint when the arguments were wrapped in `"input"`, and "Did you mean "chunk_id"?" for a respelling. Scope: tools with a help article and a closed schema; an operator's MCP tools and the MCP server's direct builtin calls are unaffected.
- **A stateful run keeps its task (#1379).** The request arrived only as the first observation and the next tool result replaced it, so from step 2 a model that had not copied it into the state had no task — it re-derived one, asked the user, adopted another chat's task, and once invented a value and reported it saved. Every step now shows "Your task (keep working on it until it is done): …" beside the state. An operator's turn replaces it; a resume uses the first user message. The runtime still does not write the state.
- **The prompt's tool inventory says to read a tool's call format first (#1380).** Non-stateful local models read help before the first call in 0 of 20 runs: what they read is the injected `## Tools` list, which carries only first sentences. It now gains one line naming the documented tools, with a real example topic. It depends on the tool set only, so the cached system prompt stays byte-stable.
- **Defaults that tripped models (#1381).** History's omitted scope is now `user` when granted, else `self` (it was always `self`, which the default grant refuses). A `format=markdown` transcript read inside a run is capped at 24K characters with a note pointing to `format=conversation`; off-run callers still get it whole. `documents_summary` with no arguments is refused with what to pass, instead of an empty list that read as "no documents exist".

### Upgrade notes

- **⚠️ Security:** upgrade if you use isolated members (`substrate:user`); see #1376 and #1377.
- **Behaviour change (#1378):** a loop tool call with an argument the tool's schema does not declare is now refused instead of silently ignored — only for documented builtin tools with a closed schema.
- **Behaviour change (#1381):** History with no `scope` now reads the caller's own chats (`user`) when that is granted.
- **Env:** `LOOMCYCLE_CODE_HOOKS_TENANTS` (added and removed within this release) does not exist; `LOOMCYCLE_CODE_HOOKS_ENABLED` alone enables code hooks, for tenant operators too.
- **New hook phases:** `subagent_start`, `subagent_stop`, `pre_compact`, `post_compact`, `run_end`.
- **The adapters are bumped to 1.95.0.**

## What's in v1.94.0

*A team walk can hold its members for review, hooks can see the run as well as its tool calls, write their bodies in JavaScript and ask an operator, and a tool's help tells the model to read it before the first call.*

Six PRs. Two finish RFC DJ (operator review of a finished run) by bringing review to team walks and removing the pause it replaces. Three are the first phases of RFC DK (hooks v2): run context and a record of every decision, code-js hook bodies, and hooks on the run itself. One is RFC DL (tool help): it changes the help pointer after a measurement showed models ignoring it.

### A walk holds its starter's members for review (#1367, RFC DJ-P3)

DJ-P1 gave team members a live review-arming seam, and nothing set it until now. A walk can now hold each member run of a starter state when it finishes, and the operator gives the verdict through that run's own review verb (`POST /v1/runs/{member}/review`, the gRPC `ReviewRun` RPC, the MCP `review_run` tool).

- **Arming:** `op=run {"review": ["<starter>"], "review_ttl_seconds": N}` at start, or the breakpoint `"<starter>:review"`, either at start or live through `PUT /v1/runs/{walk}/breakpoints`. Arming mid-wave holds the members that have not finished yet. Review is a new phase of the live set the debugger already reads, and the bare `"<state>"` form never implies it. Unlike a debug pause it needs no human-ask machinery. An unknown state, or one that dispatches no wave, is refused.
- **A rejected member** reaches the sink as `status: "rejected"` with the answer that was rejected, and does not count toward the wave's `wait`, the same as a failed member. The results envelope marks it. An unreviewed walk's envelope is byte-identical to before.
- **No short-circuit while review is armed.** A member still running may be held, and cancelling it would throw a person's review away. Each member's sink message is still published as it settles, so a downstream `wait: any` reader releases on the first approval.
- The walk's deadline expires a held member as `rejected` / `review_expired`. Aborting the walk closes a held member as `cancelled`.
- **Behaviour to know:** with the default `wait: all`, one rejected member leaves the wave short and fails the walk, as a failed member does. A walk that should tolerate rejections uses `wait: any` or `at_least`. Disarming review through the breakpoint `PUT` releases a held member at its hold's next heartbeat, which can take up to 30 s. A run retune releases it at once.
- TS `runTeam({review, reviewTtlSeconds})`. The MCP `teamdef` tool takes its schema from the builtin, so it has both.

### ⚠️ Breaking: the Starter's `after_collection` pause is removed (#1368, RFC DJ-P4)

The Starter's second debug pause held a finished wave's results off the sink until an operator released or withheld them. It could only publish or withhold and never send a result back to be revised, and because it was held in memory, a restart lost what it held. A run's own review hold (above) does all three: it can send a result back with feedback, it survives a pause and a restart, and it lives on the member run's row.

- **Removed:** the `after_collection` phase in `teamrun` and in the live breakpoint set, the Starter's staging loop and the code that existed only for it, and that half of the pause question and of the TeamDef tool's schema and description. The Starter now publishes every result as it settles. `before_dispatch` stays, because the composed prompts are something no run can show before it exists.
- **⚠️ Breaking — what to change:**
  - Arming `"<state>:after_collection"`, at `op=run` or through `PUT /v1/runs/{walk}/breakpoints`, is now refused with an error that names the replacement: arm `"<state>:review"` instead. A refused live arming leaves the walk's existing arming as it was.
  - A bare `"<state>"` breakpoint now arms `before_dispatch` only. Before this release it armed both pauses. It still does not imply review.
- It was a debug pause, not a stored contract, so there is nothing to migrate.

### Hooks see the run, and every decision is recorded (#1369, RFC DK-P1)

Tool hooks (`pre` / `post`, HTTP webhooks) covered nearly every tool call but nothing around the decision. A hook could not tell which run it was deciding for. A deny or a rewrite left no trace, and the transcript kept the model's original input for a call that ran with another.

- **Run context on every payload:** `run_id`, `parent_run_id` and `iteration`, so a hook can correlate one run's calls and tell a sub-agent's from its parent's.
- **A `hook_decision` event** is emitted and persisted beside the `tool_call` for each thing a hook did: `deny`, `rewrite_input` (with the input the tool actually ran with), `rewrite_output`, `context`, or `unavailable` (with the hook's fail mode). A pass-through emits nothing. It is on SSE, gRPC (`Event.hook_decision = 17`, `message HookDecision`), TS (`"hook_decision"` in `EventType`) and Python (`AgentEvent.hook_decision`, `HookDecision`).
- **A `post_failure` phase** runs only when the tool failed, before `post`, and its payload carries the failure's structured classification (category / retryable / description). `post` still sees failures, so existing hooks behave as before. A post rewrite keeps the classification while the call is still a failure and drops it when the hook turns the call into a success. It is accepted by `POST /v1/hooks`, MCP `register_hook` and the TS `HookPhase`.
- **`additional_context`:** a `post` or `post_failure` hook may return it, and it is appended to the tool_result text, so the model sees it and it survives a transcript replay.
- **Nested tool calls go through the hooks.** A tool that runs another tool on the model's behalf now dispatches through the run's own hooked path (`tools.ExecuteHooked`). The one such caller today is the Interruption tool's delivery through a consumer's tool, which no hook could see before. The nested call's id is `"<outer tool_use id>/<tool>"`.

### Code-js hook bodies that can ask an operator (#1370, RFC DK-P2)

A hook was a webhook only, so every policy check cost a network round-trip to a service the operator had to run, and no hook could hold a call for a person to decide. A hook's body can now be JavaScript that loomcycle runs in-process.

- **Registration:** `code` in place of `callback_url`; a hook has exactly one body (256 KiB cap). The body defines a top-level `function hook(ev)` that returns the decision; `ev` is the webhook payload plus `event`. A pre hook may return `{decision: "deny", reason}`, `{updated_input}` or `{allow_hosts}` (still limited to the operator's permit list); a post hook `{updated_output}` or `{additional_context}`. Decisions are read strictly: a misspelt field, or one that does not apply to the phase, counts as the hook failing, and its `fail_mode` decides.
- **The sandbox is the code agent's:** no fetch, require, filesystem, eval or Function, a deterministic clock and RNG, a bounded call stack and 1 MiB caps on the event and the decision. The body's only tool is `Interruption`. `ask` holds the tool call as an ordinary pending interrupt on the run (on its stream, in `GET /v1/runs/{id}/interrupts`, answered through the usual resolve endpoint) and returns the answer, or `null` if the operator declined. `notify` informs without waiting. The hook asks under its own grant, whether or not the agent may interrupt.
- **Replay, as code agents run:** each run of the body is a fresh runtime that replays the answers already given and stops at the first unanswered ask. The hook's timeout (50 ms default, 1 s cap) bounds each run of the code, not the wait for a person. At most 16 asks per call. There is deliberately no runtime pool, since a reused runtime would carry one call's globals into the next.
- **Cancellation:** a run cancelled while a hook is deciding never runs its tool, even when the hook fails open. This applies to webhook hooks too.
- **Its own gate, `LOOMCYCLE_CODE_HOOKS_ENABLED=1`**, separate from the code-agent gate, because a code hook runs on every matching call in its scope. With it unset, a code registration is refused and the error names the variable. With it set, the body is compiled and its top level evaluated at registration, so a body that cannot run is refused then, not at its first matching call.
- **Every transport:** `POST /v1/hooks`; MCP `register_hook` (`code` advertised, `callback_url` no longer required); gRPC `RegisterHookRequest.code = 9`, `Hook.code = 11`; TS `code` (`callbackUrl` now optional); Python `code=` (`callback_url` now defaults to `""`). Listing returns the body.
- **Migration 0081** (Postgres) adds `hooks.code`, so a code hook survives a cluster reload. The column defaults to `''`; every existing row is a webhook.
- **Known limit:** a tool call held by a hook's ask survives a pause or a restart only as well as an agent's own `Interruption.ask` does. The ask row is durable, but a resumed run does not re-dispatch a tool call that was in flight.

### `agent_start` and `agent_stop`: hooks on the run itself (#1371, RFC DK-P3a)

Hooks could see every tool call but not the run. No hook could stop a run before it spent a token, add context to its prompt, or check an answer before it was accepted. Both phases are registered like tool hooks (webhook or code body) and select by `agents` only; a `tools` selector is refused.

- **`agent_start`** runs once per run, after the prompt is composed and before any model call, in both loops. `{decision: "deny", reason}` ends the run failed with no model call made. `{additional_context}` is added to the prompt's last user turn as a text block of its own. A resumed run has already started and does not run it again.
- **`agent_stop`** runs each time the model finishes an answer, before the review hold and the interactive park. The payload adds `final_text` and `stop_reason`, and on a retry `stop_hook_active` and `stop_blocks`.
  - **`block`** (a reason is required) sends the reason back as a user turn and the model answers again. More than 3 blocks in a row fail the run, naming the hook and its last reason, so a validator that can never be satisfied cannot loop a run forever.
  - **`hold`** takes the review hold from RFC DJ, and `awaiting_review` names the hook in **`held_by`**. Disarming review does not release a hook's hold, a restored hook hold is held again whether or not review is armed, and a run nothing can deliver a verdict to (an Agent-tool sub-agent, which has no steer queue) ends `rejected` rather than accepting the answer.
  - Precedence is block > hold > allow, and the first block stops the chain. A hook that fails with `fail_mode: closed` holds the answer; an `open` one lets it through.
  - A stateful run reports `agent_stop` hooks inert, as it does review: its product is its state, not an answer.
- **Replay:** `replayTranscript` rebuilds a block's feedback turn and `agent_start`'s context from their persisted `hook_decision` events, on every run path. Without that, a resumed run's history would differ from what the model saw, and a block would leave two assistant turns in a row, which providers refuse.
- **Wire (additive):** `awaiting_review.held_by` on SSE, gRPC (`AwaitingReview.held_by = 4`), TS and Python; `hook_decision` gains the `block` and `hold` decisions, with `tool_use_id` / `tool_name` omitted for a run decision; the new phases are listed in MCP `register_hook`, TS `HookPhase`, the gRPC and Python docs, and the `hooks` help topic (a new "Run hooks" section).
- `subagent_start/stop`, `pre/post_compact` and `run_end` are the next phase (DK-P3b).

### Tool help the model actually reads (#1372, RFC DL)

v1.93.0 put a pointer to each tool's help article in its description and at the end of a failed call. Measured on the lab deployment's local models (`gpt-oss`, `qwen3.6`, `ornith-1.5`), none of the 7 failed calls whose error ended with a help pointer was followed by a help call; the models recovered from whatever the error text itself said. The one model that read an article unprompted made no malformed calls afterwards, against 7 in the baseline. The help works, but a line that only says where it is does not get it read.

- **The description line is an instruction.** It now ends: "Before your first call to Memory, read the call format for the operation you need: call Context with `{"op":"help","topic":"Memory/get"}` (put your operation in place of get). It gives the exact arguments and an example." The topic is a real one, not a placeholder. The help tool gets no instruction to call itself.
- **A failed call carries a correct call.** A failed call on a documented operation now ends with the first example from that operation's article, compacted to one line and labelled as an example ("A correct Path/mv call looks like this (an example — use your own values)"), then the help call for the full article. When the op itself is what is wrong, the error lists the tool's valid operations instead.
- **Stateful runs keep the instruction.** The stateful loop lists each tool as one line of at most 240 bytes, which always cut off the appended instruction, so a stateful agent never saw it. `ToolSpec` now carries the instruction separately (`Help`, never sent on the wire), and the stateful list shortens only the description and keeps the instruction whole.
- A test derived from the corpus checks that every operation article yields a one-line example for its own op.

### Upgrade notes

- **⚠️ Breaking (#1368):** replace any `"<state>:after_collection"` breakpoint with `"<state>:review"`; it is now refused. A bare `"<state>"` breakpoint arms `before_dispatch` only.
- **Migration 0081** (Postgres) adds `hooks.code`. It runs at startup, like every migration.
- **New env knob:** `LOOMCYCLE_CODE_HOOKS_ENABLED=1` enables code-js hook bodies. Off by default; unset, a code registration is refused.
- **New hook phases:** `post_failure`, `agent_start`, `agent_stop`. A lifecycle hook selects by `agents` only.
- **New wire fields, all additive:** the `hook_decision` event (gRPC `Event.hook_decision = 17`), `awaiting_review.held_by` (gRPC field 4), and `RegisterHookRequest.code = 9` / `Hook.code = 11`. A client that switches on event type should know `hook_decision`.
- **Hook payloads gain `run_id`, `parent_run_id` and `iteration`.** A webhook receiver that decodes strictly should accept them.
- **Tool descriptions and failed-call errors change wording** (#1372). Anything that matches on the old "How to call it: …" text will not find it.
- **The adapters are bumped to 1.94.0 WITH new surface:** TS `HookPhase` gains `post_failure` / `agent_start` / `agent_stop`, `"hook_decision"` in `EventType` and its `hook_decision` event field, `code` on `RegisterHookOptions` and `Hook` (`callbackUrl` now optional), `awaiting_review.held_by`, and `runTeam` `review` / `reviewTtlSeconds`; Python `register_hook(code=)` (`callback_url` now defaults to `""`), `AgentEvent.hook_decision` (`HookDecision`), and `AwaitingReview.held_by`.

## What's in v1.93.0

*A run can answer to a schema, wait as a draft until someone starts it, and be held for an operator's verdict before it counts; every tool now says where its manual is, and the manual is written from the code.*

Nineteen PRs. Six continue RFC DI (the Run as the unit a caller configures and reads), five build RFC DJ (operator review of a finished run), two are RFC DL (tool help), and four are fixes the help articles turned up when they were written against the code instead of the descriptions.

### Structured answers: `output_format` per agent and per run (#1347, RFC DI-P2b)

Assistant prefill was the old way to make a model answer in a given shape, and current Claude models refuse it. A JSON schema the provider enforces replaces it.

```
"output_format": {"type": "json_schema", "name": "city", "schema": {"type": "object", ...}}
```

- **Where it is set:** agent yaml, the AgentDef overlay (with a def-fields editor entry), and per run on `POST /v1/runs`, continuation, gRPC (`RunRequest.output_format = 34`, `ContinueRequest.output_format = 31`, schema as JSON bytes), MCP `spawn_run(s)`, TS `outputFormat` and Python `output_format`. A per-run value **replaces** the agent's whole, as `tool_choice` does. `type` may be omitted; it means `json_schema`. The schema's root must be an object.
- **The answer is parsed** into `result.structured`, beside `final_text` (a surrounding ```json fence is tolerated). An answer that is not a JSON object leaves it unset and says so. TS `RunResult.structured`.
- **Each driver maps it to its own field:** Anthropic `output_config.format`; OpenAI `response_format.json_schema` (strict only when every object is closed and every property required); Gemini `responseJsonSchema`; vLLM, llama.cpp and `ollama-local` a grammar, on a tool-free request only. DeepSeek and hosted Ollama cannot enforce it, nor can the model families that predate structured outputs. A target that cannot enforce it gets one `capability_inert` report, not a field it drops.
- **The schema also goes in the system prompt** wherever the provider does not show it to the model itself. Found live: `ornith-1.5:35b` behind Ollama's grammar returned a well-formed object full of wrong values, because a grammar guarantees the shape and never shows the model what the fields mean. Only Anthropic, OpenAI and Gemini are treated as native.
- It is sent on every model call, since the model cannot know which turn is its last. It is persisted in the run's config, so resume and retune keep it. A stateful run reports that it ignores it; its product is Σ.

### A run's spec stays readable after it ends (#1348, RFC DI-P2c)

`GET /v1/runs/{id}/config` found the run through the steer registry, so a run's configuration could be read only while it ran, and only on the replica that owned it.

- The single-run reads now carry the persisted record as **`spec`**, beside `result` and under the same tenant gate: HTTP `GET /v1/agents/{id}`, MCP `get_run`, gRPC `GetAgent` (`bytes spec = 15`), TS `Agent.spec` (`RunSpec`), Python `get_agent`. Listings leave it out.
- It is absent for a run that overrode nothing. It holds no secrets; per-run credentials are never persisted. `/config` stays the in-flight retune view.

### Configured runs: create a run now, start it later (#1349, #1350, #1351, #1352, RFC DI-P3)

A caller could prepare a run only by starting it. A **configured run** is a draft: validated exactly as a run would be (identity made authoritative, agent resolved, overrides checked), then stored. It holds no slot, no budget and runs nothing until it is started.

- **HTTP (#1350):** `POST /v1/runs` with `"start": false` creates one; `PATCH /v1/runs/{run_id}` replaces fields (`null` removes one); `POST /v1/runs/{run_id}/start` starts it and streams SSE like `POST /v1/runs`; `DELETE /v1/runs/{run_id}` discards it and its session. All `runs:create`. `GET /v1/agents/{id}` shows the draft; the run's status is `configured`.
- **gRPC and MCP (#1351):** `CreateConfiguredRun` / `UpdateConfiguredRun` / `StartConfiguredRun` (streams) / `DeleteConfiguredRun` RPCs, and one MCP tool, `configured_run` (`op=create|update|start|delete`; `start` blocks like `spawn_run`). The rules live in one connector core that every transport calls, so HTTP, gRPC and MCP cannot drift. TS `createConfiguredRun` / `updateConfiguredRun` / `startConfiguredRun` / `deleteConfiguredRun`; the Python client has the same four.
- **Identity is the draft's, fixed at create.** A start does not re-derive tenant or user from whoever starts it, so an admin starting a tenant's draft does not make it the admin's run. Confinement bits captured at create are OR-ed with the starter's, so a start never loosens them. Secrets are never stored: `user_bearer` / `user_credentials` with `start:false` are a 400 and are given to `/start` instead.
- **Starting goes through `RunOnce`**, the path gRPC, MCP, the scheduler and webhooks already share. One guarded store transition moves the row `configured → running`: of two concurrent starts exactly one wins (the other gets 409), and an admission refusal (429 / 503) leaves the draft as it was.
- **Bounds:** a per-(tenant, user) cap, `LOOMCYCLE_MAX_CONFIGURED_RUNS_PER_USER` (default 100, then 429 `configured_run_cap`); a caller-chosen `agent_id` reserved at create; an expiry sweep, `LOOMCYCLE_CONFIGURED_RUN_TTL_MS` (default 24h, 0 = off). A session holding a draft is left out of `ListSessions` until the draft starts.
- **Web UI (#1352):** "Save as draft" beside "Run agent", built by the same request builder, so a draft carries exactly what the run would. The runs list has a `configured` filter. A draft's detail pane lets you edit the prompt, then Start or Discard; Start is disabled while an edit is unsaved. Only a plain-text prompt is editable, because a richer draft would be flattened.
- **Migration 0080** adds `runs.draft`: the raw request that will start the run, minus secrets (the merge against the definition happens at start).
- **Known gap, by decision:** a started draft does not get `POST /v1/runs`'s detached interactive goroutine or per-turn cancel until the start paths are unified.

### Review: hold a finished run for an operator's verdict (#1354, #1356, #1357, #1359, RFC DJ-P2)

An agent's finished answer counted the moment the model stopped. A run started with **`review: true`** is held instead: it emits `awaiting_review` and waits for an operator.

```
approve              the run completes on the held answer
reject + feedback    the feedback is its next user turn; it revises and is held again
reject               the run ends with the new terminal status "rejected"
```

- **HTTP and TS (#1354):** `POST /v1/runs/{run_id}/review` (`runs:create`, like steering; 409 `not_held` for a live run that is not held); TS `reviewRun()`. `review` is also a retune override, and disarming a held run releases it as approved. `GET /v1/agents` reports `awaited_state: "review"` while held. A held run started through `POST /v1/runs` detaches like an interactive one, so a slow reviewer does not cancel it by outlasting the client's connection.
- **The verdict is never a tool,** so an agent cannot approve its own answer. It gates on the tenant-scoped store and the session owner rather than on the local steer registry, so a verdict sent to another replica still reaches the run.
- **gRPC, MCP and Python (#1356):** a `ReviewRun` RPC; `review` on `RunRequest` / `ContinueRequest` and as an optional override on `RunInputRequest` / `RetuneRunRequest` (optional, so `false`, which releases a held run, differs from unset). An MCP `review_run` tool (tenant-confinable) plus `review` on `spawn_run` / `spawn_runs` / `retune_run`. Python `review_run()`, the `review` option, and an `AwaitingReview` payload on `AgentEvent`, which had been dropping it.
- **A held run survives a pause and a restart (#1357).** A run blocked waiting on a person never reached the loop's pause gate, so a pause timed out on it and a snapshot or restart could not bring it back. Every park (review hold or interactive wait) now takes part in a pause: it records itself paused without leaving its wait. Resume restores a held run into the hold at the round it was in, before any model call. Previously it was refused as idle and marked failed, which lost the answer under review. A parked interactive chat is restored the same way.
- **An unreviewed hold expires as rejected (#1359).** `review_ttl_seconds` ends a hold with no verdict as `rejected`, stop reason `review_expired`: an answer nobody looked at is never approved. Each hold gets the full window. The deadline is kept in the run's config, so a hold restored after a restart expires when it would have, and a deadline that passes during a pause waits for the pause to lift. `awaiting_review` carries `expires_at`. It is set at start only, on HTTP, continuations, drafts, MCP, TS, gRPC (`RunRequest` 36, `ContinueRequest` 33, `AwaitingReview.expires_at` 3) and Python.
- A stateful run reports that review does not apply to it. The Web UI names the `rejected` status in the runs filter and status pill.

### Team members are reachable, reviewable and report their status (#1364, RFC DJ-P1)

This is the plumbing a team walk needs before review can apply to its members.

- `SpawnResult.Status` carries a member's terminal status (`completed`, `failed`, `cancelled`, `rejected`). A rejected member did its work and returns no error, so the error alone could not tell it from a success. The status comes from the same rule `finishRun` writes the row with, and a test pins that the two agree.
- Team members get a steer queue, as resident children do, so an operator can steer one and a verdict can reach one held for review. An ordinary Agent-tool sub-agent still has none, because its parent drives it.
- A walk can attach a live review arming to each member's spawn. Nothing sets it yet; DJ-P3 will.

### Tool help: an article per tool and per operation, and a pointer to it (#1355, #1358, RFC DL)

Help was organised by feature, so most tools had no topic under their own name, and no topic showed a call. A model that misused a tool had its manual one call away and no way to know the call.

- **Tool and operation articles (#1355):** `tools/<Tool>.md` indexes a tool's operations and `tools/<Tool>/<op>.md` documents one, with call examples. Articles resolve under the spellings a model uses (`Path/ls`, `Path.ls`, `"Path ls"`). A lint holds the corpus to its rules, and every call example is validated against the tool's live input schema. Operators can add articles under `LOOMCYCLE_HELP_ROOT/tools`, including for an MCP tool, named `mcp__<server>__<tool>`.
- **A pointer in every tool description (#1355):** each run's tool array now carries, for a tool with an article, the exact `Context` call that returns it, and for every scoped tool, the scope values **this run** may use ("Scopes this run may use: agent, tenant (not user)"). The grants come from each tool's own scope check, so the report is the enforcement. It is rendered once per run, so the tool array stays byte-identical across a run's calls.
- **`Context op=self` returns a `scopes` block**; `op=permissions` gains the `sql_scopes` and `history_scope` it left out. `op=help` returns a tool article with its `operations`, and an unknown operation lists the ones that exist.
- **The scopes primer is rewritten (#1355).** It taught `agent | user | global` across Memory, Channel and Evaluation; Memory has no `global`, takes `tenant`, and Document, Path and History have their own values and defaults. The new primer has one table of every scoped tool with its values, default and grant.
- **159 per-operation articles (#1358)** for Memory (25), Document (47), History (11), Channel (8), Context (14), Skill (2), plus Recall and Agent. Each covers the arguments with defaults, the real result keys, the errors a model meets and what to do about each, and one to three examples.
- **A failed call points at its help (#1358).** A failed call on a documented tool now ends with the help call for the operation it tried (`How to call it: call Context with {"op":"help","topic":"Path/mv"}`). Successes, undocumented tools and failures marked retryable are left alone. `Context op=doc` now returns the tool's article too.
- **Memory's input schema lists `path`** (#1358). `set` and `get` accepted it, but a model could learn that only from somewhere else.
- **Descriptions corrected (#1355, #1358).** Writing the articles from the code turned up descriptions that taught behaviour the tools lack, on every request:
  - Document declared `limit` twice, and the duplicate key lost `graph_recall`'s description. Its scope list and Memory's omitted `tenant`, and "transactions are refused" read as if `sql_begin` did not exist.
  - History: an omitted scope means `self` while the default grant is `user`, so the most natural call was refused; the description now says to pass `scope`. It no longer claims content search is unavailable (`match=content` is), and the status filter lists `rejected`. The MCP `history` tool listed 7 of its 11 ops.
  - Recall said results come newest-first; they are ranked by score.
  - Channel promised at-least-once delivery, but `subscribe` commits past what it returns. Its hint named a nonexistent `scope` argument, and it misstated `peek`'s default start.
  - Context's op list left out `compact` and `state`, and called itself side-effect-free, which `compact` is not. Path's hint said `mkdir` is a no-op; it creates an empty directory.
- The `document` and `history` feature topics move into the Document and History tool articles. `topic=Skill` now reaches the Skill tool article, not the `skills` topic.

### Fixes

The first four were found by writing the help articles against the code.

- **A refused Memory write was stored anyway (#1361).** `merge`, `append_dedupe` and `bounded_list` checked the core block's `limit_bytes` and the scope quota after the atomic update had committed, so an over-cap write was reported as refused and kept. The caps now run inside the reducer, on the value about to be committed, so a refusal rolls it back. Each refusal names its own op (they all said `Memory.set:`).
- **Claude and Gemini saw Agent as spawn-only, with every field required (#1362).** Agent's schema was a top-level `oneOf`. Both drivers flatten a combinator by keeping the first branch's `op` enum and the union of every `required` list, so the model got `op: ["spawn"]`, `required: [name, prompt, op, spawns, child_run_id]`. The schema is now one flat object listing all seven ops, with nothing required at the top level (each op already checks its own). A guard forbids a top-level combinator in any builtin schema.
- **A chunk with no parent went beside the document, not in it (#1363).** `create_chunk` without `parent_id`, and `move_chunk` with an empty `new_parent_id`, left the chunk parentless, and `export_md` rendered it as a second top-level heading. An omitted parent now means the document's root, and a mistyped `document_id` is refused ("document … not found in this scope") instead of creating a chunk no document shows. Existing parentless chunks are left as they are.
- **Whole-document ops accept `document_id` (#1360).** `get_document`, `delete_document`, `set_remote`, `sync` and `diff_remote` took only `id` or `path`, so the call a model makes right after `create_document` failed. When `id` and `document_id` are both given and differ, the call is refused rather than acting on one of two documents.
- **A compaction pushed to a run on another replica arrived as an operator turn (#1354).** The cross-replica steer payload carried only text, so the summary was appended as a user message and the history it replaced stayed. It now carries the control's kind.
- **A held run whose caller disconnected was recorded completed (#1354),** on an answer nobody approved, on the paths that do not detach (MCP, gRPC, a draft's start). It is now recorded cancelled.
- **The composed prompt leaked onto re-attach streams (#1354).** `prompt_snapshot` (v1.92.0) is store-only, but the re-attach tail did not skip it, so a re-attach, and a detached run's own stream, sent it as an SSE frame.
- **The override parity guard checked nothing for two fields (#1356).** Its TS lookup returned `""` for `interactive` and `interruption`, which matched any optional field. An unmapped name is now an error.

### Also

- **Every `chat/*` agent holds History (#1365).** `chat/medium`, `chat/local` and `chat/local-small` can list, search, read and resume the user's past chats. Before this, the only answer to "what did we decide last week?" was whatever consolidation had distilled into Memory. No `history_scope` is set, so the grant is the default: the caller's own chats, nothing wider.
- **bench (#1353):** a LoCoMo rating under the rovemark protocol on a fully local pipeline (everything but the judge on `ornith-1.5:35b`): binary-J **0.706** (95% CI 0.683–0.730) over 1,535 questions. It is not comparable to rovemark's gpt-4o-mini numbers. On the way it found that the earlier "local" runs' extractor had silently fallen through the tier cascade to paid cloud `deepseek-v4-pro`.

### Upgrade notes

- **Migration 0080** adds `runs.draft`. It runs at startup, like every migration.
- **New run statuses:** `configured` (a draft) and `rejected` (a reviewer's reject, or an expired hold). A client that switches on run status should know both.
- **New env knobs:** `LOOMCYCLE_MAX_CONFIGURED_RUNS_PER_USER` (default 100) and `LOOMCYCLE_CONFIGURED_RUN_TTL_MS` (default 24h; 0 turns the sweep off).
- **Document `create_chunk` / `move_chunk` with no parent** now put the chunk under the document's root. A caller that relied on the old parentless placement will see the chunk move into the document.
- **The Agent tool's schema is flat.** A Claude or Gemini agent now sees all seven operations, where it saw only `spawn` before.
- **Help topics moved:** the `document` and `history` topics are now the Document and History tool articles, and the `skill` alias is gone (use `Skill` for the tool, `skills` for the background).
- **The MCP catalogue grows to 55** (`configured_run`, `review_run`).
- **The adapters are bumped to 1.93.0 WITH new surface:** TS `createConfiguredRun` / `updateConfiguredRun` / `startConfiguredRun` / `deleteConfiguredRun`, `reviewRun`, `outputFormat`, `review`, `reviewTtlSeconds`, `RunResult.structured`, `Agent.spec`; Python the configured-run methods, `review_run`, `output_format`, `review`, `review_ttl_seconds`, `spec`.

## What's in v1.92.0

*A run keeps its answer and its prompt, an operator can tell an agent which tool to call first, and a local model's stateful chat stops mid-task no more.*

Eight PRs. Two start RFC DI (the Run as the unit a caller configures and reads), two close security gaps, and the rest are fixes found by live use and by review.

### A run keeps its result and its prompt (#1342, RFC DI-P1)

Nothing durable answered "what did this run say?" or "what was it asked?".

- **`runs.result`** holds the run's final text (and a stateful run's final Σ). It is written by `FinishRun` in the same update that makes the run terminal, and returned on single-run reads: HTTP `GET /v1/agents/{id}`, MCP `get_run`, gRPC `Agent.result`, TS `Agent.result`, Python `result`. Listings omit it to stay small. It has the transcript's retention and erasure tier. **Migration 0079.**
- **`GET /v1/runs/{run_id}/prompt`** returns the request the run's first model call was actually sent — the node role, injected metadata, `{{…}}` expansion and the stateful loop's instructions included. It is recorded once per loop entry as a store-only `prompt_snapshot` event. The recorded text is redacted, it is never streamed, and the Web UI hides it. TS `getRunPrompt()`.
- **Team members are addressable:** starter sink messages and parallel result envelopes carry each member's `run_id`, a failed member's included.
- **⚠️ Security, found on the way:** MCP `get_run` / `list_runs` could read **other tenants'** runs over a per-tenant MCP session. They are tenant-gated now, as HTTP and gRPC already were.

### `tool_choice` per agent and per run (#1344, RFC DI-P2a)

```
"tool_choice": {"mode": "auto|none|required|tool", "name": "WebSearch", "until": "first_call|until_called|always"}
```

- **What it is for:** "this agent must search before it answers", "this classifier must answer through its schema tool".
- **Where it is set:** agent yaml, the AgentDef overlay (with a Web UI editor), and per run on `POST /v1/runs`, continuation, gRPC, MCP `spawn_run(s)`, TS `toolChoice` and Python `tool_choice`.
- **`until` exists because a choice forced on every call never lets a run finish.** `first_call` (the default) forces only the first call; `until_called` forces until the model makes the call; `always` is refused for `required` and `tool`.
- **A provider that cannot enforce the choice** runs on and emits `capability_inert` (re-checked after a fallback).
- **Resume** does not force a choice the run already spent. A stateful run reports that it ignores the setting.

### Stateful mode: a plan is not an answer (#1343, #1345)

Observed live on `ornith-1.5:35b`: a stateful step carried a patch and a plan — "I will run a web search for the exact figures" — with **no action and no answer**. The loop read "no action" as "turn over" and showed the plan as the reply, so the chat stopped mid-task two turns running. Replaying that step against the model reproduced the shape in 4 of 9 replies.

- **A turn ends only when the model says so** — `done`, or an answer in `final`. A step that would end one without an answer is sent back, told what it missed, on the `max_patch_retries` budget. The same rule stops the model's inner monologue ("Operator said continue…") being shown as its reply.
- **The stateful prompt now shows the two accepted reply shapes as examples**, and names the wrong ones. Measured on the same model, first reply only: 5/9 → **10/10** valid at a working step, 5/8 → **7/8** at an answer step.
- **(#1345)** The stdio MCP client could report a delivered result as `server exited` when a server answered and exited straight after — a Go `select` picking at random between two ready cases. That was the flaky `TestServerCrashFailsInFlightCalls`, and a real defect for one-shot MCP servers.

### Fixes

- **⚠️ Tenant hook callbacks can no longer reach private addresses (#1339).** A hook receives every matching tool input, and a `substrate:tenant` operator could point one at the cloud metadata endpoint or an internal service — SSRF plus exfiltration. Tenant hooks now dial through the network guard (checked after DNS and on every redirect). Operator-global hooks are unchanged.
- **Anthropic: stop sending parameters current models reject (#1340).** Opus 4.7 and later, Sonnet 5, Fable and Mythos 400'd on the old request shape:
  - reasoning depth now goes out as `output_config.effort` with adaptive thinking, not a thinking budget;
  - non-default sampling is dropped on those models;
  - a forced `tool_choice` is dropped on Opus 5.5, Fable 5.1 and Mythos 5.1, which refuse it under any configuration. That forced choice is exactly what broke **every stateful run** on those models.

  Older ids are byte-identical.
- **A Post hook no longer strips a tool's error classification (#1338).** The server always wires a hook dispatcher, so the category, retry hint and backoff (RFC DA) were being dropped from every tool call in production.
- **A running team walk can be cancelled (#1341)** through `POST /v1/runs/{run_id}/cancel` and gRPC `CancelTurn`. Every member run it spawned stops with it, and the walk's row finishes `cancelled` with the reason. Single replica for now.

### Upgrade notes

- **Migration 0079** adds `runs.result`. It runs at startup, like every migration.
- **⚠️ A tenant hook whose callback is on a private network now needs the operator to allowlist that host** — `hooks.private_host_allowlist`, or `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST`. Until then the call follows the hook's `fail_mode`.
- **Anthropic on current models:** a configured `temperature` / `top_p` / `top_k` is now dropped (with a log line) instead of failing the request.
- **Stateful agents on weaker models** may spend a retry where they used to end a turn early. That is the intended trade: a correction call instead of a wrong answer.
- **The adapters are bumped to 1.92.0 WITH new surface:** `getRunPrompt`, `toolChoice`, `Agent.result` (TS) and `tool_choice`, `result` (Python).

## What's in v1.91.0

*A stateful answer the model wrote into its state reaches the operator.*

One runtime fix (#1336), found the morning after v1.90.0 shipped, on the same local model that produced v1.90.0's report.

### ⚠️ The answer was in the state, and the operator saw an empty turn

Observed live on `ornith-1.5:35b` (ollama-local, `mode: stateful`). The model ended its turns with its reply fields **inside** the patch:

```
{"patch": {"done": true, "final": "<the whole answer>"},
 "action": {"tool": "emit_state", "input": {}}}
```

The loop reads only the top-level `final`. What followed:

- The answer was merged into Σ, and naming `emit_state` as the action drew a "not an action" error.
- v1.90.0's empty-turn re-prompt could not help. Asked for its answer "in `final`", the model believed it had given one and repeated the same shape, so the operator saw *"(the model ended its turn without an answer; it updated its state: done, final)"*.
- The stale answer then stayed in Σ into the next turn, where the model read it back as something it knew.

What changed:

- emit_state's own fields — `final`, `done`, `action`, `reasoning` — found inside the patch are moved to where they belong and **deleted from the state**. That also clears a stale copy left there by an earlier turn.
- A field the **state schema declares** is left alone.
- An `emit_state` action that carries an answer, or says it is done, ends the turn instead of being refused. One with neither is still refused as before.
- The tool description, the system prompt and the re-prompt now say plainly that these fields sit **next to** `patch`, never inside it.

### Also on main

- **Memory benchmarks (#1335):** the cascade gate's routing measured on LoCoMo, with results under `bench/` and a section in `docs/MEMORY-ARCHITECTURE.md`. Docs and data only; no runtime change.

### Upgrade notes

- **No wire, schema or config change.**
- **An existing stateful session may already carry a stale `final` / `done` in Σ** from before this release. It is removed the next time the model writes that field into its patch again. A model that now answers correctly, at the top level, leaves the stale copy in place — start a new chat if one is confusing the model.
- **The adapters are bumped to 1.91.0 with no surface change**, to keep the client version matching the runtime.

## What's in v1.90.0

*A stateful chat keeps its state and stays live — v1.88.0 made it park, and this makes the park worth something.*

One PR (#1333), 23 commits, one per finding of a code review of the structured-state loop. The review and the per-commit status are in the doc store at `/loomcycle/docs/stateful-context-review-digest` (§10, §11).

### ⚠️ An interactive `mode: stateful` chat threw its state away every turn

v1.88.0 gave the stateful loop a park, and the runtime did park — but the embedded `/run` terminal saw something else.

1. The loop emitted `done` at **every** turn boundary before parking. `done` is terminal to every consumer, so the terminal marked the chat **completed** after its first answer.
2. The terminal then sent the operator's next message as a **session continuation**: a new, non-interactive run.
3. That run started from an **empty state**, and its first observation was the whole replayed transcript, relabelled `Task:` — the history stateful mode exists not to feed back.
4. The original run stayed parked forever, heartbeating and holding its concurrency slot.

So a chat looked like it worked while discarding its state each turn and leaking a run per chat.

Now:

- The run emits **one** `done`, when it actually ends. The turn boundary is `awaiting_input`, exactly as in the append loop.
- The terminal steers a parked run whatever its status says.
- Resume and session continuation start a stateful run from its **last recorded state** and the observation it was waiting on — the result of the action it chose, or the operator's message — never from the replayed transcript.
- An action that was mid-flight when the process stopped is reported to the model as interrupted and is **not** re-run.

### The stateful loop skipped what every other run gets

`runStateful` branched out of the run before most of what a run sets up. Each of these is fixed:

| missing | consequence |
|---|---|
| **heartbeat** | a working stateful run sent none, so the stale-run sweeper failed it as `heartbeat_timeout` 10 minutes in, while it kept running and spending tokens |
| **tool-use hooks** | an operator's Pre-hook **deny did not apply** to a `mode: stateful` agent; Post-hooks, host-widening audit and the parallel_spawn ledger were skipped too |
| **context-transform plugins** | the `redact` secret scrubber never saw a stateful request — and Σ and the observation are where tool output lands |
| **secret masking on persist** | tool results were masked in the events BLOB, but the copy of them in Σ, stored on every step, was not |
| **retry and fallback** | one 429 or "overloaded" killed a stateful run that an append run would ride out |
| **runtime pause** | a stateful run kept calling its provider while the runtime reported itself paused, and never reached the one state resume re-dispatches |
| **`context_size` / `op=self`** | a per-agent context size never reached the provider (Ollama used its own), and `Context op=self` reported no provider, model or footprint |

### Smaller fixes

- **`POST /v1/runs/{id}/compact` on a stateful run returns 409 `stateful_run`.** It used to summarise the replayed transcript (a model call), bank the span, answer `compacted: true`, and then the loop dropped the summary.
- **A turn whose answer went into the state instead of `final` is asked again**, and is never shown as an empty message.
- **An action missing a required input field is named back to the model** before dispatch, e.g. `{"tool":"Interruption","input":{}}`.
- **The Web UI matched tool results by a field the server never sends**, so an `Interruption` chip read `interrupted: ?` and the busy hint named a tool as running for the rest of the run. This affected every mode.
- Stateful action ids are unique across a session's runs.
- A large patch is weighed before the next request goes out, not one step late.
- A correction request replays the model's own thinking block (Anthropic and DeepSeek reject a replayed turn without it).
- An object patched where there was none drops its nulls (RFC 7386).

### Upgrade notes

- **Consumers of the event stream:** an interactive stateful run no longer emits a per-turn `done` — only one, when it ends. That is what the append loop has always done. A client that rendered each stateful answer on `done` should use `text` / `awaiting_input`, as it already must for append runs.
- **`mode: auto` still sends an interactive run to recap.** This is kept on purpose until interactive stateful chats have been proven in the field. An explicit `mode: stateful` is honoured.
- **Still not wired for stateful runs:** turn-cancel (`POST /v1/runs/{id}/cancel` still 409s) and a per-turn step budget (RFC DH P3).
- **No wire, schema or config change.** The adapters are bumped to 1.90.0 with **no surface change**, to keep the client version matching the runtime.

## What's in v1.89.0

*A stateful run reports the tokens it spends — and until now it spent them against no budget at all.*

One PR. Small, and worth deploying for the second paragraph rather than the first.

### ⚠️ `mode: stateful` was exempt from token budgets (#1331)

Reported as `tokens: 0 in / 0 out` on a RUNNING interactive chat, which is
the visible corner of it and the least of it.

`runStateful` accumulated its totals into the final `EventDone` and emitted **no
per-call `EventUsage` at all**. That event is what three separate subsystems key
off:

| consumer | consequence |
|---|---|
| the UI gauge | a stateful run read `0 in / 0 out` for its whole life |
| the RFC AV ledger | `recordCallUsage` never fired → `token_usage` had no rows → `GET /v1/_usage` was blind to stateful runs → `runs.cost` summed an empty set to NULL |
| **the RFC AW budgets** | `limits.Add` never fired → a `mode: stateful` agent spent tokens that counted against **no per-scope budget** |

The last row is why this is not a display bug. **An operator who set a hard
token limit was not protected from this mode**, and nothing said so — the run
looked like it cost nothing.

The event is now stamped like the append loop's: the effective window, so the
gauge has a denominator, and the SERVING provider, so the ledger records which
key paid across a mid-run fallback.

### A test that leaked a parked goroutine

Found by CI going red on a DATA RACE that named the wrong test.
`parkHeartbeatInterval` is a package-level var one test lowers to keep itself
fast — safe only while no other goroutine is inside `parkForInput`, which reads
it. v1.88.0's resume test started an interactive run and never awaited it, so
the leaked park read the var while the next test wrote it, and the failure
surfaced a long way from the cause. Awaited now, with a note at the var saying
what mutating it costs.

### Upgrading

- **Check your usage figures for stateful agents.** Every `mode: stateful` run
  before v1.89.0 wrote NO ledger rows, so its tokens are absent from
  `GET /v1/_usage`, its `runs.cost` is NULL, and it never incremented a budget
  counter. The history cannot be reconstructed — the per-call figures were never
  recorded. Counters are correct from this release forward.
- **⚠️ If you rely on RFC AW token budgets, re-check them against a tenant that
  runs stateful agents.** Its month-to-date total was under-counted by exactly
  those runs.
- **The adapters are bumped to 1.89.0 with NO surface change**, as in v1.88.0 —
  the bump keeps "the client version matches the runtime version" true rather
  than announcing anything new.

## What's in v1.88.0

*A stateful chat waits for you instead of ending, and remembers what it knew when it wakes up.*

Three PRs, all from one operator report against v1.87.0, and all in the
stateful loop. RFC DH P1 + P2 — released together on purpose, because P1 alone
is honest in-process and a lie the first time a replica restarts.

### The chat stopped after every turn (#1328, RFC DH P1)

> The chat/local model with stateful context still stops the agent after each
> turn regardless of interactive flag state.

It did, and `grep SteerQueue internal/loop/stateful.go` returned **zero hits**.
The loop reached the model's `done` and RETURNED, where the append/recap loop
calls `parkForOperatorTurn` instead. `interactive: true` was not ignored so much
as unimplemented — the flag reached a loop with nowhere to put it.

⚠️ **AND THE RUNTIME ALREADY BELIEVED THESE WERE INCOMPATIBLE.**
`resolveAutoContextMode` refuses to route an interactive run to stateful and
says why in its own comment — "that loop has no steer/park". But it governs
`mode: auto` only, so an **explicit** `mode: stateful` walked past the guard in
silence. The protection you got depended on how little you specified.

A stateful run now parks at `done`, and your next message becomes its next
**observation**, prefixed `operator: `. The park belongs at `done` and not at
every step: the step loop is internal machinery you never see, and the thing
you wait for is the answer.

⚠️ **THE RUNTIME STILL DOES NOT WRITE Σ.** A reserved key filled by the runtime
would make Σ half model-authored and half not, with `state_schema` validating
only one of the halves. The prompt instead tells the model that this observation
is a person speaking and that nothing survives the turn except the state, then
leaves it to decide what is durable — which is its job in this mode. That
paragraph is added for interactive runs only; an autonomous one cannot receive
such an observation.

The iteration cap is also now computed once, in `Run`, and handed down. This
loop derived its own and missed every lift `Run` applies — including the one
that matters, since each PARK consumes an iteration. A parked chat on the
default 16 would have died after a handful of exchanges reporting
`max_iterations`, which is an answer about the wrong thing.
`unbounded_iterations` stops being inert in this mode.

### And it forgot everything when it woke up (#1329, RFC DH P2)

Σ survives a live park because the goroutine holding it does. Across a pause, a
snapshot restore or a replica move it was lost.

⚠️ **A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES.** `replayTranscript` rebuilds
a conversation, which is precisely what stateful mode exists to *not* have — the
model is fed only (Σ, observation). A resumed run handed only `PriorMessages`
started from an **empty Σ** and cheerfully continued a conversation whose every
established fact it had forgotten. That is worse than refusing to resume: it
looks like it worked.

No new column: each `context_state` marker already carries the whole post-merge
Σ, so the last one is the answer. ⚠️ Which makes that marker **load-bearing**,
where its own comment used to say "still persisted for audit" — both comments
now point at each other, so anyone trimming it to save transcript bytes meets
the reason not to.

### `tool not found: emit_state` (#1327)

From the same chat. The model named `emit_state` as its ACTION, and the action
name went straight to the dispatcher unchecked — so the answer was a lookup that
could only miss.

⚠️ `emit_state` is not a tool. It is the channel the model is already speaking
through, offered as the only entry in `tools` on every step, and its `action`
field names a DIFFERENT tool for the runtime to run. Both that and any unoffered
name are now refused in the loop, which is the only place that knows which tools
THIS agent was offered — the dispatcher knows every tool in the process, so "not
found" is the most it can ever say. The observation names the mistake AND the
alternatives.

### Upgrading

- **Nothing to change.** An interactive `mode: stateful` agent — `chat/local` in
  the bundled `chat` preset — starts behaving as a terminal chat on upgrade:
  it parks for your next message instead of ending the run.
- **Autonomous stateful runs are unaffected.** They still end at `done`; the
  park requires both `interactive` and a steer queue.
- **⚠️ Do not trim `context_state` events from a transcript.** They are how a
  resumed stateful run recovers its state.
- **The adapters are bumped to 1.88.0 with NO surface change.** Nothing in
  `@loomcycle/client` or the Python client differs from 1.87.0 — the bump keeps
  "the client version matches the runtime version" true rather than announcing
  anything new.
- Still open from RFC DH: **P3** — RFC BH turn-cancel needs a defined resting
  place in the stateful loop (cancelling mid-dispatch leaves an action whose
  result nothing will read), and a per-turn step budget.

## What's in v1.87.0

*A mistyped key can no longer mint an admin token, a summarizer gets the budget to answer, and the emit_state contract is finally on the wire.*

Five PRs, four of them opened by one operator report and one by a security
incident. ⚠️ **One breaking change — see Upgrading.**

### ⚠️ A mistyped key minted admin tokens (#1324)

Hit live while minting bench tenants. The caller sent `allowed_scopes` — which
is what the *response* echoes and what the stored column is named, so it is not
an unreasonable guess — `json.Unmarshal` dropped the unknown key, `scopes` came
through empty, and the documented default fired:

```go
if len(scopes) == 0 {
    scopes = []string{auth.ScopeAdmin}   // "single token, full power"
}
```

Three tenant tokens were minted as **admin**. HTTP 200, a plausible-looking
response, no error anywhere.

**And the mistake is self-locking.** One non-retired admin-scoped def turns OFF
the `LOOMCYCLE_AUTH_TOKEN` fallback, so the accidental token revokes the very
bearer that minted it — every later admin call returns an opaque 401 and there
is no way back through the API. Two of the three deployments needed SQL to
recover.

Two layers now, and neither alone is the fix. An omitted scope list is
**refused** rather than escalated — admin is stated, not inherited, so a typo is
an inert 400 instead of maximum privilege. And the mint tool refuses **unknown
keys**, deliberately unlike the rest of the substrate: everywhere else a stray
field just means you get a def that differs from what you typed, but here the
dropped field decided *privilege*. `import_token` keeps the old default, since
binding the legacy token already says which scope it carries.

### The stateful loop stopped killing runs over a conversational reply (#1320)

`chat/local` FAILED on its first turn with *"model did not call emit_state"* —
4287 in / 557 out, 45.4s, before the operator's "Hello. What can you do?" got
any answer. The model was not broken: it was asked a conversational question and
answered conversationally, and the loop threw 557 tokens of perfectly good prose
away and gave up.

`on_invalid_patch` / `max_patch_retries` covered a patch that ARRIVED and failed
validation; a model that never called the tool got no retry at all. Both are the
same condition from the runtime's side — the output was not usable and the model
can fix it — so a missing call is now retried under the same budget, re-prompted
with what the model actually said. What it produced is no longer discarded
either, so the terminal error can say what the run *got* rather than only what
it wanted. Thinking is kept separately and never replayed: a reasoning-only
reply looks identical to silence from outside, and those need opposite fixes.

### The emit_state contract is on the wire (#1321, RFC DG P1)

`grep -rn "tool_choice" internal/` returned **one** hit before this release, in
a comment about OpenAI-compat fields the gateway ignores. loomcycle had never
sent `tool_choice` on any driver — so a model that ignored "call `emit_state`
exactly once" was not disobeying anything the wire ever said.

`providers.ToolChoice{Mode,Name}` now maps onto all three dialects (Anthropic
`{type:any|tool|none}`, OpenAI `required`/the nested named form, Gemini
`functionCallingConfig`), with deepseek/vllm/llamacpp inheriting through the
OpenAI driver. The zero value is auto, so every unopted request is
byte-identical.

⚠️ **Ollama is non-mandatory, by decision.** `/api/chat` has no `tool_choice`
and neither does its OpenAI shim, so a forced request there DEGRADES rather than
being refused — refusing would make every local-model agent unrunnable to buy a
guarantee it never had. The run says so when it finally fails, because "the model
ignored the constraint" and "there was no constraint" call for opposite next
moves.

⚠️ **Anthropic drops a forced choice under extended thinking**, which the driver
attaches whenever an agent sets `effort` on a reasoning-capable model. That pair
400s, so thinking wins and the choice is dropped with a log line — the same
precedence already applied to temperature.

### A summarizer gets the budget to answer, not just to think (#1322)

Reported from the terminal on `chat/local-small`:

> Context recap declined at 87% of the window: the summarizer returned no text …
> **raise recap_max_chars**, or choose an effort that stops the model thinking

That advice is the tell — it asks you to lengthen the summary to fix a *budget*.
`recap_max_chars` asks the PROMPT for a length, and the runtime derived the
model's OUTPUT CAP from the same number: 192 tokens at the default. A reasoning
model spends that thinking before it writes a word.

The cap now has a floor independent of the requested length, and the call asks
for the least reasoning the provider will give it — it previously sent **no**
effort at all, and on Anthropic and Ollama an unset hint means "the model's own
default", which for qwen3/deepseek-r1 is to think. If that hint breaks a
provider (OpenAI passes it through as `reasoning_effort`, which a non-reasoning
model rejects) the call drops it and retries once, which can only turn a decline
into a summary.

### Recap can run on its own model (#1323)

`compaction:` has had a `model:` override since it shipped; `context:` never got
one, so a recap always ran on the agent's own model. ⚠️ The point is the model's
BEHAVIOUR, not its context window: a recap reads a span and writes a short note,
so it is never the call that runs out of window — what breaks it is a reasoning
model. Point `context.model` at a cheap non-thinking summarizer beside a
thinking chat model.

Carried across all six surfaces, with a guard that reflects over
`config.Context`'s json tags rather than holding a list. It found a real
pre-existing gap on its first run: `recall` and `harvest_to_memory` were missing
from the MCP per-run context schema, so an MCP caller could not set either.

### Upgrading

- ⚠️ **BREAKING: `loomcycle operator-token create` now requires `--scopes`.** It
  refuses in the CLI and prints both spellings plus the lockout warning.
  `--copy-from-env` is exempt. The Web UI is unaffected — it already sent
  `scopes` explicitly and refused an empty set. If you script token minting,
  add the flag.
- ⚠️ **Check your existing tokens.** If any were minted without an explicit
  scope list, they are admin. `select name, allowed_scopes from
  operator_token_defs where retired_at is null;` — and note that an unintended
  admin row also disables the legacy `LOOMCYCLE_AUTH_TOKEN` login.
- **`effort` on an Ollama stateful agent now reaches the wire as a forced tool
  call** where the provider supports one. Nothing to change; a local model that
  ignored the prompt contract is now re-prompted rather than fatal.
- **The `History op=recap` token cap rose from 160 to 1024.** A deliberate
  trade: 160 was a tight guard against a verbose model and a guaranteed EMPTY
  result against a reasoning one.
- **Adapters at 1.87.0** (`@loomcycle/client` on npm; Python rides its own
  `python-v1.87.0` tag). `context.model` is new public surface on both.
- Cut as a minor, not a patch: a patch tag builds only
  `denngubsky/loomcycle-browser`, and these fixes have to reach the images a
  deployment pulls.

## What's in v1.86.0

*The distillation reports v1.85.0 added now wait for a number the provider returned — and every wire event is finally nameable from a typed client.*

Three PRs, all fixes, cut as a minor because the TS adapter gains real public
API and a deployment needs the full image build.

⚠️ **THE REGRESSION v1.85.0 SHIPPED WITH, REPORTED FROM PRODUCTION WITHIN A
DAY.** An operator's interactive terminal stopped rendering after `started` on
every run — three agents, two context modes — and reported the runs as hung.
They were not hung. v1.85.0 had begun emitting three NEW frames between
`started` and the first turn, then answering and parking exactly as before:

```
v1.84.0  session agent steer started                                              text usage awaiting_input
v1.85.0  session agent steer started  ctx_distill_declined ×2  context_exhausted  text usage awaiting_input
```

The `409` on cancel was the same fact seen from another angle: turn-cancel is
disarmed while a run waits at `awaiting_input`, so it correctly refuses a run
that is parked rather than stuck.

And the frames were **false**. The run had sent nothing:

> context recap declined: context.keep_last_n 6 pins all **1 message(s)**, leaving nothing to distil — `severity: warning`
>
> context not reclaimed: **88%** of the window (17600/20000 tokens) is in use and distillation did not shrink it

One message, because the conversation had not started. 88%, because before the
first turn the footprint is a chars/4 estimate of a request no tokenizer has
seen, measured against the static capability a driver may still revise — Ollama
reads the model's actually-loaded window from `/api/ps` only after a call.

**Three compounding causes, each correct on its own.** #1305 seeded the
footprint so a one-iteration continuation could distil before sending an
oversized prompt; its own commit message names the failure it was introducing —
*"a short history must NOT distil at start"*. #1308 then added the preamble to
that seed, which is right: on a small window the system prompt and tool
catalogue **are** the request. But a preamble is not a short history. It is
fixed and irreducible, so on an agent whose preamble is most of its window it
sits above the threshold on iteration ZERO and on every iteration after, and the
gate opens on a quantity distillation cannot move. #1312 then hung a second tier
and an exhaustion report on that same gate, so one silent decline became three
alarms.

**The fix is one rule: report what you measured.** The gate still OPENS on the
seed and distillation still runs — that protection is the point of seeding it,
and a resumed transcript already over the window is still reduced before the
first call. Only the operator-facing reports — the declines, the exhaustion
report, and the value `Context op=self` carries — wait for a footprint a
provider actually returned. `runStateful` gets the same rule, where a driver
that reports no usage would otherwise claim an empty Σ cannot be reduced.

### Every wire event value is nameable from TypeScript (#1317)

`context_exhausted` reached that terminal as a type **no typed consumer could
name**: it was never added to `@loomcycle/client`'s `EventType`. It was not
alone — **sixteen** wire values were missing from that union, some for a dozen
releases:

`context_recap` · `context_state` · `context_distill_declined` ·
`context_exhausted` · `thinking` · `turn_cancelled` · `provider_fallback` ·
`fallback_suppressed` · `model_downgraded` · `cache_invalidated` ·
`reasoning_invalidated` · `channel_publish` · `channel_delivery` ·
`interruption_pending` · `spawn_child_started` · `spawn_child_result`

The **payloads** were missing too, so even a consumer casting out of the union
had nothing to reach into: eleven fields on `AgentEvent` — the five context
payloads, `fallback`, `channel`, `interruption`, `turn_cancelled`,
`spawn_child`, and the `reasoning` trace on `done`.

Two hand-written per-event parity guards already existed and neither caught it,
because each only guards its own event. So the new guard **does not hold a
list**: it enumerates the runtime's own `EventType` constants out of
`provider.go` and requires each value on the union's member lines. A list in the
test would be a third copy to forget.

### The effective-config report answers from the run (#1318)

`GET /v1/runs/{id}/effective-config` merged the run's `Routing`, `Resources` and
`Tuning` onto the stored definition and computed its `inert` advisory array from
the result — but never passed `Context`. So the array answered from the stored
definition while `fields.context` **in the same response** answered from the
run: one payload, two verdicts about one setting. Seeing a per-run override
introduce or clear a trap is the entire reason that array exists over the
boot-time warning, and it structurally could not.

Two more values that existed and never arrived: `used_tokens` had been declared
on every decline since the type existed and was set by nothing — so a decline
reported the window size and left the reader to guess how full it was, and a
consumer saw `0` and read *"0 tokens in use"* rather than *"not reported"*. And
`EffectiveConfigResponse` declared `fields` and stopped, so `inert` — the half
`fields` structurally cannot express, because a setting can be in force **and**
inert — was unreachable from a typed client.

### Upgrading

- **A genuine `context_exhausted` still fires.** This release removes the FALSE
  pre-turn report, not the feature: a run whose window really is full, measured,
  will still say so. A consumer that renders the event stream needs a case for
  it — `@loomcycle/client` 1.86.0 finally lets you write one.
- **Adapters at 1.86.0** (`@loomcycle/client` on npm; the Python adapter rides
  its own `python-v1.86.0` tag). Not ceremonial: #1317 and #1318 add public
  surface, and adapter surface that ships without its version bump is stranded.
- **Cut as a minor, not a patch.** A patch tag builds only
  `denngubsky/loomcycle-browser`, and this fix has to reach the images a
  deployment actually pulls.
- Nothing to change in configuration. If you deleted `compaction.autocompact_at_pct`
  on v1.84.0's advice, v1.85.0's upgrade note still applies — put it back.

## What's in v1.85.0

*Context distillation now reclaims the window in every mode — or says loudly that it cannot.*

Seven PRs. v1.84.0 made a failed distillation **visible**; this release makes it
**recoverable**, and closes the gap that visibility immediately exposed.

⚠️ **THE DEFECT v1.84.0 SHIPPED WITH, FOUND IN PRODUCTION.** The footprint that
drives the distillation gate counted the CONVERSATION only, while the value it
was compared against — and the value the provider bills — counts the system
prompt and tool catalogue too. On a `chat/local` agent with a 2048-token window
the estimate read **2 tokens (0.1%)** against a real request of **3340 (163%)**:
the run sent 163% of its window with the gate never opening. A small window with
a large preamble is the worst possible ratio, and no unit fixture had that shape
— every one was built large-in-messages, which is exactly where the estimate and
reality agree. Reported by a consumer running a real configuration, not by the
suite.

That fix also corrects `Context op=self`'s gauge, which under-reported by the
whole preamble on every turn after a distillation — so an agent deciding whether
to self-compact was reading the same wrong number it was being judged by.

A DECLINED DISTILLATION NOW HAS A SECOND TIER. The gate branched once — recap
mode took recap, everything else took compaction — so `compaction.autocompact_at_pct`
was inert in recap mode **by construction**, and a recap that declined left
nothing else to try while the run climbed to the provider's limit.

Compaction is now reachable from every mode, and it is the right last resort
precisely because it fails DIFFERENTLY: `empty_summary` is a property of the
recap budget and the recap prompt, and a compaction summary runs on neither. A
second tier that failed for the same reasons would be theatre.

THE WINDOW NOW BEATS A PINNING `keep_last_n`. The declined-split path never
reached the tail cap, so a run whose kept tail alone exceeded the window had no
escape — `keep_last_n` could veto every distillation path. The precedence is now
explicit: `keep_last_n` is a PREFERENCE about how much to keep verbatim, the
window is a HARD LIMIT, and a preference does not override a limit. It fires only
when the tail genuinely does not fit.

STATEFUL MODE IS BOUNDED AT LAST. Its transcript is rebuilt from (Σ, observation)
each step so it cannot accumulate — which is why the gate was never wired there —
but **Σ itself accumulates**, and the whole of it is serialised into the prompt
every step. Nothing measured it and nothing bounded it.

Summarising is the wrong operation on a state object: Σ is validated against
`state_schema`, and prose is not a Σ. The structural equivalent is EVICTION, by a
retention class declared per property — `core` never dropped, `derived`
recomputable, `scratch` dropped first, least-recently-written as the tiebreaker
within a class. **The default is `core`**, so no agent starts losing state on
upgrade; the cost is that an undeclared schema gets no relief and reports
exhaustion instead. Evicted entries are banked before they go, because Σ is the
run's working memory and losing it silently would trade a context problem for a
data-loss one.

⚠️ **AND THE ESCAPE HATCH THAT ALREADY EXISTED WAS NOT ONE.** A model can prune Σ
by emitting `null`. Relying on that is relying on the model to ELECT a behaviour,
and the measurement for exactly that pattern is that it does not: a tool
parameter was passed on 51 of 128 calls, and making it imperative took compliance
to 100% while accuracy collapsed to 0.003. A bound the model must choose is not a
bound.

EXHAUSTION IS NOW LOUD. When nothing reclaims the window and the footprint is at
the point where something should have, the run emits `context_exhausted` carrying
every tier's verdict — because "the mechanism ran and refused, here is the fix it
named" and "nothing ran at all" call for opposite next moves. Banded by ten
points rather than reported once, since a run stuck at 82% and the same run at
95% is news twice.

Declines also carry a SEVERITY now. Most are routine — `reasoning_keep` is the
operator's own instruction, `not_smaller` is a correct refusal — but
`split_declined` is a warning: that path will decline identically every time and
the window keeps filling. If every decline warned, none of them would.

⚠️ **THE TWO `keep_last_n` KEYS ARE NAMED APART EVERYWHERE.** Recap reads
`context.keep_last_n` (default 6); compaction reads `compaction.keep_last_n`
(default 4). A message naming the bare key sends half its readers to edit the
setting that was not the problem, and they then watch the window keep filling and
conclude the fix does not work.

## Upgrade notes

⚠️ **IF YOU FOLLOWED THE v1.84.0 ADVISORY AND DELETED `compaction.autocompact_at_pct`,
PUT IT BACK.** That release warned it "does nothing" outside `append` mode. It was
true then. It is false now: the threshold is the BACKSTOP for every mode, and in
`stateful` it is the only bound Σ has. The advisory is retired in this release,
but configuration already edited on its advice will not repair itself.

**Distillation now fires where it previously could not**, so agents near their
thresholds will distil EARLIER than before. Three changes compound here: the
footprint now includes the system prompt and tool catalogue (so the same
conversation reads as a larger fraction of the window), compaction is reachable
from every mode, and a run that answers in a single iteration can distil at all.
This is the intended behaviour, and it is a visible change for every `recap` and
`stateful` agent.

⚠️ **`chat/local` IS NO LONGER LOCAL.** It moves from a pinned `ollama-local` model
to `tier: high`, which is a cloud tier — every prompt and tool result now leaves
the machine. Its system prompt no longer claims otherwise. It also moves to
`mode: stateful`, which means **the `/run` terminal cannot steer or park it**: the
stateful loop has no steering and no `end_turn` parking. Pin the previous
behaviour in your own overlay if you were relying on either.

**A `stateful` agent wanting Σ eviction must declare retention classes** on its
`state_schema` properties (`x-retention: scratch | derived | core`). Without them
every key defaults to `core`, nothing is evictable, and the run reports exhaustion
rather than shrinking.

Adapters: `@loomcycle/client` **1.85.0**, `loomcycle` (PyPI) **1.85.0** — carrying
a corrected `reasoning: "keep"` doc. The old text promised a decline
unconditionally; it is only reported once the threshold is reached, and that
sentence is what led a consumer to expect a frame that was never owed.

## What's in v1.84.0

*Context distillation could fail in five different ways and say nothing. It cannot any more.*

Twelve PRs. One line dominates — a whole subsystem made observable before it was
changed — plus three grpc vulnerabilities closed on the serving path.

A LIVE CHAT CLIMBED TO THE TOP OF ITS WINDOW WHILE AUTO-DISTILLATION NEVER FIRED
ONCE, and every mechanism that could have reported why was silent. The threshold
was crossed at 72%; the next call was at 99%. Zero recap markers, zero errors.

The leading cause, on the evidence: the recap summariser's token budget is
derived from `recap_max_chars` (`/4 + 64`), so the default allows about **192
tokens** — and `summarizeWith` accumulates only text, while Ollama routes a
thinking model's reasoning to a separate channel. That session logged **1293**
thinking events. A thinking model given 192 tokens spends them thinking and
returns nothing, which took a branch that emitted no event, no error and no
marker.

⚠️ THE SILENCE WAS THE DEFECT, NOT THE DECLINE. Telling "the threshold was never
crossed" from "it was crossed and the summariser returned empty" required reading
the summariser's source and counting event types in a raw transcript. That is not
a diagnosis an operator can make, which is why visibility came first and the
behavioural fixes second.

**`context_distill_declined`** is a new event carrying one of five reasons,
because they call for five different actions: `split_declined` (carrying the
message count and `keep_last_n` — those two numbers ARE the diagnosis),
`empty_summary`, `summarize_failed`, `not_smaller`, `reasoning_keep`. It is
emitted at most once per (mode, reason) per run — a condition that is a property
of the configuration must not repeat until it buries itself — and surfaces on
`Context op=self` beside the footprint, because an agent told it is at 99% will
otherwise call `op=compact` into the same decline and learn nothing.

⚠️ A DISTILLATION THAT MADE THINGS WORSE WAS BEING APPLIED. In the observed
session an operator's manual compact went `14230 -> 14334` tokens and was applied
anyway: both numbers were already measured at every site and never compared. All
three sites now refuse when the result is not smaller — `after >= before` exactly,
not a margin, because a 10% rule would have refused the first GOOD compaction of
that same session.

The harvests moved with it. `Recall.Harvest` and the memory bank ran ABOVE the
measurement, so a declined distillation had already handed away a span it then
kept — and with the gate re-firing every iteration, wrote the same content
repeatedly.

A RUN THAT ANSWERS IN ONE ITERATION CAN NOW DISTIL. The footprint was zero until
the first provider call RETURNED, while the gate runs at the top of an iteration —
so a continuation answering at `end_turn`, which is a single iteration, could
never distil however full its replayed prompt. The observed session's last run
sent 30100 tokens of a 32768 window this way and reclaimed nothing. It is now
seeded from the messages the first request will actually carry.

A MANUAL COMPACT OPERATES ON THE SESSION, which is what the loop holds. It used
to fetch the session transcript and then filter it to one `run_id`, so a
continuation chat presented 2-3 messages and answered "nothing to compact" at 92%
of the window — and when the split DID succeed, the kept-tail count was computed
from that run-scoped slice and applied against the session-scoped history,
producing a summary of a span the model never held.

"Nothing to compact" is now three answers with their numbers: `noop` (too short),
`noop_keep_spans_all` (`keep_last_n` pins everything — lower it), and
`noop_not_smaller`. The existing `noop` value is unchanged, so a consumer matching
it keeps working.

DEAD CONFIGURATION SAYS SO, at boot and in the effective-config report. The agent
in that session declared `compaction.autocompact_at_pct: 70` and
`compaction.memory_flush: true`, and both were inert: **only `mode: append`
consults the compaction path at all**. Every other mode distils by its own route —
`recap` takes the other branch, `stateful` returns before the gate exists, and
`auto` resolves to one of those two, so it is inert either way. Reported with the
live knob to set instead.

**`context` reaches the typed client.** The server has accepted a per-run
`context` block since it shipped and no typed consumer could send one — and there
was no workaround, because the request body is assembled from an allow-list, so
the key was dropped even by a caller casting past the type checker. That is what
made A/B-ing distillation modes impossible, which is how the rest of this work
gets verified.

THREE GRPC VULNERABILITIES CLOSED, all reaching loomcycle's own serving path
rather than merely present in the module graph: a server panic via a missing
`:authority`/`Host` header, heap exhaustion via HTTP/2 DATA frame fragmentation,
and the xDS RBAC + HTTP/2 transport issue. Two are unauthenticated remote DoS.
grpc 1.80.0 -> 1.83.2; `govulncheck` goes from three affecting findings to zero.

ON THE MEMORY SIDE, a retrieval grant and an honest negative result. Recall can
now search the trace index with the caller's own query and return those turns as a
separate block — worth **+31pp** on a local answerer where fact-anchored
attachment reaches 0.5473 and direct search reaches 0.7877.

⚠️ BUT THE GATE WAS NOT MET, and the notes record that rather than the headline.
On a second conversation the effect is +12.3pp against the +29.6pp of the first, so
the original figure was a property of that conversation rather than of the
architecture. The cause is a gap in the grant: it enriched `op=recall` only, and
the model elects the op — on the second conversation it chose `search` 49 times of
85, and the inert half moved by **exactly zero**, which rules out the alternative
explanation that `search` simply marked harder questions. The grant now covers
`search` too.

**Upgrade notes.** No configuration changes are required.

`POST /v1/runs/{run_id}/compact` now operates on the SESSION rather than the run.
Byte-identical for a single-run session, which is most autonomous runs; a
continuation chat will now find something to compact where it previously reported
a noop.

A compaction or recap whose result is not smaller is now REFUSED rather than
applied. If you have a fixture or a monitor asserting that a tiny conversation
compacts successfully, it will now report `noop_not_smaller` — which is the
correct answer: the compaction preamble alone is ~190 characters, so a very short
history genuinely grows.

Adapters: `@loomcycle/client` **1.84.0**, `loomcycle` (PyPI) **1.84.0**. The TS
bump carries the new `context` block, which loomboard's distillation panel needs.

## What's in v1.83.0

*The run controls that shipped write-only can now be read back — and a granted tool that could never work says so at run start.*

Eighteen PRs. Two lines finishing at once — the per-run override surface gets
its reads, its last two transports and its interactive fields, and the
capability-gate work gets its defaults, its warning and its correction — plus
three phases of recall provenance, which turned into a dating fix once the
measurements came back.

A RUN'S CONFIGURATION CAN BE READ, NOT JUST WRITTEN. The previous release let a
run carry its own model, budgets and tuning; every bit of it was write-only. A
caller could set an override and had no way to read it back, and a retune that
moved no model left no trace at all. Three reads close that:

  - `GET /v1/runs/{run_id}/config` reports what the RUN overrides. There was
    nothing to extend — there is no `GET /v1/runs/{run_id}` at all. Absent means
    "not overridden", which at this layer beats a resolved value, and a run that
    was never retuned is a 200 with an empty config: "this run overrides
    nothing" is an answer, not a 404.
  - `GET /v1/runs/{run_id}/effective-config` reports what it will ACTUALLY use,
    field by field, with the layer that decided each — `run` / `definition` /
    `user_tier` / `operator` / `resolved` / `default`. The SOURCE is the point.
    `max_iterations: 16` cannot distinguish a deliberate setting from a default
    nobody chose, and those call for opposite actions.
  - `POST /v1/runs/{run_id}/retune` now answers with the merged record it
    already computed and threw away. A caller cannot recompute it: naming a
    `model` clears the `provider` so a previous choice cannot linger and
    contradict the new pin, and naming a `tier` clears the `model`. A panel
    echoing its own request back would display something the run does not hold.

AN OVERRIDE EVENT THAT COULD NEVER FIRE, NOW TWO THAT DO. `OverrideInfo.Fields`
documented itself as carrying the keys a request actually set — "so a reader can
see a budget or tuning change that moved no model at all" — and was hardcoded to
`{"model"}`. Worse than a wrong value: the loop returns early unless ROUTING
changed, so a retune of only `max_tokens` emitted nothing whatsoever. A consumer
had built a branch against a documented capability that could not run.

Making the loop report more would have been the wrong fix. The loop cannot see a
request; it sees a re-resolved routing outcome. So there are now two events
answering different questions — the server says WHAT WAS ASKED FOR at retune
time, the loop says WHAT THE RUN IS NOW USING once it knows, with the from/to
pair the server could not yet have. A `from`/`to` pair present is the
discriminator, and the doc comment that misled the consumer now says so.

A RUNNING AGENT CAN BE PROMOTED TO INTERACTIVE. An override could change how a
run was routed and budgeted but not whether it could be TALKED TO, so a long
autonomous run that needed a correction could not be given one — it had to be
cancelled and restarted. `interactive` and `interruption` are now override
fields, evaluated at the turn boundary rather than only at start, so a run
already in flight parks for input from the next boundary on.

RETUNE REACHES ALL FIVE SURFACES. gRPC gained `RetuneRun`, and a steer over gRPC
can carry overrides with its text. MCP gained `retune_run` — it has no steer
tool at all and never got one. The TS adapter gained `getRunConfig`,
`getEffectiveConfig` and a `retuneRun` whose return type is no longer stale,
plus the source union as a type so a consumer switching on it cannot misspell a
case. And `metadata`, the per-run layered-`context` block and cost-attribution
lineage reached the gRPC wire, all three reachable from HTTP since they shipped
and all three silently absent rather than refused.

A TOOL THE AGENT CANNOT USE NOW SAYS SO, AT RUN START. `tools` and the
capability gates are two independent grants, and the second silently voids the
first: an agent granted `AgentDef` with no `agent_def_scopes` was refused
mid-task, and the operator saw "the agent didn't do it" rather than "the agent
could not". A server-generated `capability_inert` event now rides the run's own
event channel at start — the path the budget warnings take, for the same reason
— reaching a live SSE or gRPC consumer and persisted. Once per run, not per
call: the condition belongs to the definition, not to any invocation. The
payload carries the FIX as well as the fact, because a reader told "AgentDef is
inert" still has to work out which yaml key governs it.

ONLY GENUINELY INERT GRANTS ARE REPORTED, which is the part that needed thought
rather than typing. `sql_scopes` and `evaluation_scopes` now join
`memory_scopes` and `history_scope` in resolving to what the caller already owns
— `sql_scopes` to `["user"]` and `evaluation_scopes` to `["submit_self"]` — so
an unset one is no longer inert, and an event for it would fire on nearly every
run of most agents. That is how a signal becomes noise and then gets ignored,
taking the real warnings with it. What remains reportable: the def-authoring
gates, the two A2A gates, and Channel with neither side of its ACL.

⚠️ THE SCOPE DEFAULTS MADE EXISTING WARNINGS FALSE. `agentGateWarnings` has
warned about empty capability gates since F21, and two of its lines stopped
being true the moment those gates gained defaults: "every Memory op will
default-deny" and "every Evaluation op will default-deny". A warning that states
the wrong consequence sends an operator to fix what is not broken and teaches
them the channel is noise — which costs the warnings that ARE right. The text
now distinguishes what RESOLVES from what still grants nothing. The core-blocks
and consolidation advisories needed more than a reword: their default is the
WRONG SCOPE for them, so they name the scope to set instead.

Those advisories also gained REACH. They were logged once at boot, where an
operator debugging a week later never sees them. `doctor` inspected no agents at
all — so the most common "why is my agent not doing that" went unmentioned by
the command people run precisely to be told what is wrong — and `validate` did
not print them either. Both now do.

TWO SILENT-FAILURE PATHS FIXED. A fan-out child with no prompt reached the model
as a null user turn: it got the system prompt, answered whatever that implied,
and COMPLETED, so the caller read a green envelope and the emptiness was visible
only in the thinking trace. The same caller mistake was already a 422 on the
single-spawn path. And the Python adapter could not send `user_credentials` at
all — absent from all three enumerations — so a caller whose MCP headers carry
`${run.credentials.<name>}` got an empty map and a downstream 401, with nothing
client-side saying the credential had been dropped.

RECALL CAN ATTACH THE TURN A FACT CAME FROM — ON THE OPERATOR'S SAY-SO. A
distilled fact is tenseless; the turn it came from opens with a timestamp. On
LoCoMo conv-26 reaching the turns moved accuracy 0.396 → 0.788 (McNemar +61/-3,
p=4.7e-15), almost all of it in temporal questions (0.176 → 0.797).

⚠️ BUT THE SECOND RETRIEVAL ONLY HAPPENS IF THE ANSWERER ELECTS IT, AND MOST DO
NOT. Same tool, same prompt, same store: deepseek issued 214 trace retrievals
across 150 questions; qwen3.6 issued 7; an agentic-tuned ornith-1.5:35b issued
17, for +2.4pp (p=0.508). And it cannot be fixed by insisting — replacing the
advisory prompt with an imperative procedure took qwen3.6 to 100% compliance and
accuracy to 0.0034 of 1.0, emitting tool-call JSON into the answer field. On a
small model a mandatory protocol competes with the task for attention.

So the retrieval happens in the RUNTIME, where nothing has to elect it. A
`recall_include_turns` grant on the agent def — operator-resolved, never
model-supplied, the same trust posture as `memory_scopes` — makes every recall
that agent issues carry provenance. The `include_turns` tool parameter stays;
either alone is enough.

Turns are resolved through `source_session_id` rather than the trace index (the
index is forward-only and empty unless an operator backfilled it;
`source_session_id` is populated on 87% of facts in the reference store and needs
no flag), and nothing is re-ranked — the fact list is chosen exactly as today and
each chosen fact is EXPANDED, so turns never compete for a slot. Default off,
bounded per turn and per response, with `turns_attached` / `turns_dropped_for_budget`
so a caller can tell "nothing resolvable" from "budget spent".

⚠️ AND IT ENFORCES `history_scope`, NOT ONLY THE GRANT. Recall-attached turns are
history reach wearing a memory hat: without the second gate an agent denied
History could read the same words by asking for facts. `recall_include_turns`
decides whether turns are OFFERED; `history_scope` decides whether they may be
READ; both must say yes. The field is `notOverridable` per-run for the same
reason — a run that could set it would obtain transcript through the memory path,
which is exactly what an operator declines when they narrow `history_scope`.

RECALL NOW RETURNS WHEN A FACT WAS OBSERVED. `Memory op=recall` returned no
date on a hit, so "when did X happen" had nothing to answer from — the distilled
sentence is tenseless. The date was believed to arrive with the source span, and
measured on a live store that is true of a third of them: of 150 hits, 111
carried a span and only 36 of those (32%) carried a date. The cause is in span
derivation — a turn reads `[1:56 pm on 8 May, 2023] Caroline: …` and spans are
cut on sentence punctuation as well as line breaks, so the FIRST sentence keeps
the stamp and every later one loses it.

Meanwhile `observed_at` is populated on 87% of facts and was being discarded at
this boundary: the date the question needs was already on the row, one column
away from the projection that read it. It is returned as RFC3339 rather than the
stored unix nanos, because this field is read by a model answering "when" and a
nanosecond integer is not an answer it can give. A row carrying ONLY a date is
now kept — the old rule required a span or a pointer, so a fact whose span was
never derived but whose observation time WAS recorded was dropped entirely, the
exact row this change exists to surface.

⚠️ WHAT IT DELIBERATELY DOES NOT DO is prepend the turn's stamp to the span.
That was the first attempt and it breaks a load-bearing invariant: the span must
be a verbatim substring of the transcript, which is what makes fabrication
impossible rather than merely unlikely, and what a verification judge checks
against. Synthesising a span that never appeared in the source would trade a
retrieval problem for an evidence one.

AND AN `observed_at` LATER THAN ITS OWN TURN IS THE INGESTION DATE, NOT THE
EVENT. Measured on the same store, 5 of 263 facts carried a 2026 `observed_at`
on a 2023 corpus — two of them while their own span still read
`[7:55 pm on 9 June, 2023]`. Not a parse failure: the stamping is gap-filling by
design and a model-supplied value always wins, so when the extractor emits the
date it is RUNNING on, nothing corrects it. `observed_at` is when it was SAID,
and a turn cannot have been said after its own timestamp, so that one direction
is now corrected to the span. The model-wins rule is otherwise intact — a value
EARLIER than the turn is exactly the relative date ("last month") the parser
cannot resolve, which is why the rule exists.

⚠️ 2% WOULD HAVE BEEN COSMETIC WHILE THE FIELD WAS WRITE-ONLY. Recall returns
`observed_at` as of this release, so a wrong date now reaches the reader AS AN
ANSWER — and a confidently wrong date is worse than a missing one.

THE TS AGENT-DEF MIRROR STOPS DRIFTING. The substrate carries an agent def as an
overlay whose shape the in-process tool owns; gRPC declares it as opaque bytes
and the Python adapter takes a plain mapping, so both already passed any key
through. TypeScript is the exception — `AgentDefOverlay` is a hand-written
mirror with no index signature, so excess-property checking REFUSES any key it
does not declare, and a field absent there is not a documentation gap but a
field a typed TS caller cannot set at all. `recall_include_turns` is added.

⚠️ THE MIRROR IS ALREADY 30 FIELDS BEHIND the Go overlay's 48 —
`memory_consolidation`, `sampling`, `compaction`, `history_scope`, `volumes` and
the entire `*_def_scopes` family are unreachable from a typed TS caller. That is
pre-existing and is NOT fixed here: which of them the adapter should expose is a
decision about its surface, not about this feature. What IS fixed is that it
stops growing — the 29 remaining gaps are frozen in a register that may only
SHRINK, so a 31st unmirrored field fails, and an entry that later gets mirrored
must be removed or it fails the other way. The drift reached 30 precisely
because nothing objected as it grew.

**Upgrade notes.** `sql_scopes` and `evaluation_scopes` change posture on
upgrade, the same way `memory_scopes` and `history_scope` did in v1.82.0: an
agent that holds `Memory`/`Evaluation` with the scope list unset previously had
every such call refused and now resolves to the caller's own data (`sql_scopes`
only when the run carries a user id). Set the list explicitly to widen or
narrow, or `["-*"]` to grant none — the same deny-all sentinel `skills` uses.
Existing agent rows stay byte-stable: the defaults resolve at policy time, not
by materialising into the definition, so `content_sha256` does not fork.

`Memory op=recall` hits now carry an `observed_at` they did not before — additive,
but a consumer that pins the response shape should expect the field. Turn
attachment stays off unless an operator grants `recall_include_turns`.

Adapters: `@loomcycle/client` **1.83.0**, `loomcycle` (PyPI) **1.83.0**.

## What's in v1.82.0

*A run can now choose how it runs — and a tool an agent was granted but could never use now works.*

Eighteen PRs. Two features worth the version, plus the memory-graph work
continuing underneath them.

A RUN CHOOSES ITS OWN MODEL, BUDGET AND TUNING — WITHIN WHAT THE DEFINITION
ALLOWS. Everything about how an agent ran used to be fixed at the definition: to
try one question on a bigger model, or give one job a longer leash, you forked
the agent. A run now carries its own `model` / `provider` / `tier` / `effort`,
its own `max_tokens` / `max_iterations` / `unbounded_iterations`, and the tuning
row (`retry_attempts`, the memory budgets, `inject_tool_guide`).

Three rules make it safe rather than a hole in the definition:

  - naming a model PINS it, clearing the tier — a cascade that may route
    elsewhere does not answer "use this model";
  - naming a provider NARROWS and keeps the tier, because collapsing it would
    trade away fallback the caller never gave up;
  - an unknown `effort` is REFUSED, not dropped. Ignored and applied look
    identical from outside, and that is the whole problem with silent defaults.

The budget knobs are raisable except `max_concurrent_children`, which may only
be LOWERED — it is the only bound on sub-agent fan-out that exists, since a
child takes no admission slot and is not budget-checked at spawn. Raising it is
refused rather than clamped.

The overrides survive the places a per-run value usually dies: a provider
fallback re-resolves WITH them rather than from the definition, a paused run
comes back carrying them, a parked run can be RETUNED mid-conversation, and a
child inherits them only when it is the same definition (re-validated at spawn,
and dropped rather than refused when it does not fit, because inheritance is
offered rather than requested). They reach gRPC, the MCP spawn tools, and both
adapters.

Untrusted triggers cannot route: webhook, A2A and scheduled runs build their
input from the definition, and an AST test over the input literals keeps it that
way.

A GRANTED TOOL THAT COULD NOT WORK NOW WORKS. An agent could hold `Memory` in
its tools and be unable to use it, because `memory_scopes` was default-deny when
empty and nothing said so — the refusal arrived when the model called the tool,
mid-task. An unset `memory_scopes` now resolves to what the caller already OWNS:
`user`, plus `tenant` for a non-isolated member. An unset `history_scope`
resolves to `user`, since a user always has access to their own chats. A
declared list stays authoritative and is never widened.

⚠️ UPGRADE: an agent holding `Memory` or `History` with NO scope list gains
user-scope access it did not have before. That is the intent — the grant was
meant to work — but it is a posture change, so it is stated here rather than
left to be discovered. To grant nothing, say so: `memory_scopes: ["-*"]`, the
same spelling `skills: [-*]` already uses. An empty list cannot carry that
meaning, because the stored agent shape omits empty lists and one reads back
indistinguishable from never having been set.

The default is applied at policy-resolution time, never written into the
definition: these fields are content-identifying, so materialising a default
would change every affected agent's `content_sha256` and fork it on upgrade.

A MISSING PER-RUN CREDENTIAL NOW REFUSES THE CALL instead of dropping the header
and sending it anyway. An unresolved `${run.user_bearer}` / `${run.credentials.
<name>}` used to go out as an anonymous request — and a peer that does not
authenticate served it. It is a business failure, not a retryable one, and the
distinction matters: the retry path was previously spending 30 seconds of
backoff on a request that could never succeed.

GRAPH RECALL SEEDS SEMANTICALLY AND WALKS FAR ENOUGH. Traversal now starts from
a semantic match rather than a title hit, bounds its hops, batches its id
queries, and honours `limit`. Provisioning a scope's schema moved to once per
process rather than once per call.

Also: the MCP batch spawn tool advertises the per-run knobs it already accepted
(it was accepting them and documenting none of them, which for a model-facing
tool is the same as not having them), the Python adapter's override inputs reach
the generated stubs and both request builders, and the transport-parity guard
now checks each SURFACE rather than each file — it had been satisfied by one
spawn tool on behalf of a sibling that carried nothing.

## What's in v1.81.0

*A fact naming two things was stored joined to neither, and the benchmark built to catch that also answered whether graph traversal is worth building.*

Five PRs, two themes: the memory graph got its edges back, and a resumed run
comes back as the run that paused.

THE EXTRACTOR'S OUTPUT SCHEMA OMITTED A FIELD, AND THE GRAPH HAD NO CHAINS. A
fact naming two entities — "Dave works at the shop" — was stored with one
`about` edge, to its own subject, and nothing joining it to the other. The store
filled with spokes and no chains, so a question needing A→B and B→C had no path
to walk.

Nothing failed while this was true. The pass succeeds, the facts are written,
recall returns them. Only the structure was missing, which is why it survived.

The cause was the JSON template at the end of the extractor prompt — the shape
the model copies, and deliberately the last thing it reads before the
transcript. `also_about` was named in the prose above it and absent from the
template. Measured paired, pinned, on a corpus where every fact names two
entities:

  pre-fix prompt                      0/72
  pre-fix template + the new prose    0/72
  this prompt                        72/72    two-sided exact p = 0.00049

The middle arm is the one that settles it: rewording the prose changes nothing.
The template is the whole effect. End to end on a full corpus, facts carrying
both entities went 36% -> 93%, and consolidation recall 352/376 -> 376/376.

THE MINIMAL FORM LEADS THE TEMPLATE FOR A REASON. Adding the field with the full
shape first cost the invented-entity guarantee: on a transcript naming no
person, service or organisation, the extractor invented one. The subject rate
rose 0.60 -> 0.80 and the eval gate refused the baseline. Leading with the
no-entity shape restores it — 0 violations at three seeds — and a test now holds
the invariant: every field the prose names in backticks must appear in the
template.

AND THE BENCHMARK THAT FOUND IT ANSWERED ITS OWN QUESTION. RFC DB asked whether
following typed relations beats retrieval at all, or whether the idea is
unmeasurable. On 120 synthesis questions over 63 sessions, every arm capped at
the same content budget:

  oracle              99.6%   the ceiling; the answerer is not the bottleneck
  traversal           58.6%   +130/-4 vs single-hop, two-sided exact p < 1e-6
  single-hop           5.8%   today's retrieval
  shuffled-relation    0.8%   the same walk over randomly rewired edges
  no-memory            0.0%   nothing leaks from training

The shuffled arm is what makes the win mean something. Same code, same depth,
same budget, the same 28 facts in the prompt; only the edge targets permuted. It
does not merely fail to beat single-hop — it loses to it. The gain is the
relations, not the volume.

Bounded honestly: that is a corpus BUILT to contain relational structure, and it
measures the mechanism with retrieval done by the harness. Whether real corpora
carry chains, and whether an agent handed the tool would invoke it, are both
still open.

A RESUMED RUN COMES BACK AS THE RUN THAT PAUSED. Every call-time value a run
started with — temperature, compaction policy, layered-context mode, context
window, run timeout — was rebuilt from the AGENT DEFINITION on resume, so a run
that started with a per-run override came back without it and never said so. It
now restores from itself, or refuses and says why.

A CHAT THAT WAS WAITING COMES BACK WAITING, NOT FAILED. A run parked awaiting
its operator ends on an assistant turn, and re-entering the loop would hand the
provider a trailing assistant message. Resume refused it and marked the run
FAILED — an operator's idle chat, destroyed by a restart. It now comes back
parked, which is what it was.

- the chat agents inject what is true of the PERSON, not only of the deployment

@loomcycle/client 1.81.0 — no wire change; version parity for the release.
loomcycle (PyPI) 1.81.0 — no wire change; version parity for the release.

## What's in v1.80.0

*A failed tool call now says what kind of failure it was.*

Fifteen PRs. One theme dominates: a tool failure stopped being a boolean and a
sentence, and became something an agent can act on.

An agent receiving a failed tool call has exactly one decision to make —
resend, change the call, or stop — and every failure in this runtime arrived as
one boolean and one English string. "No rows matched" and "the database was
unreachable" are both short sentences. Guessing wrong means retrying a call
that can never succeed, or abandoning one that would have worked.

The runtime ALREADY knew the answer and discarded it. The HTTP surface maps
~135 typed codes and emits Retry-After; gRPC maps the same conditions onto
ResourceExhausted and PermissionDenied; the provider layer classifies failures
for fallback. All of it was thrown away at the boundary an agent stands on.

  errorCategory   transient | validation | business | permission
  isRetryable     will resending THIS call fail?
  description     what went wrong AND what to do next
  retryAfterSeconds  optional, and only where waiting actually helps

The distinction that earns its keep is validation vs business: validation is
recoverable by the agent alone, business is not. An agent that treats a policy
refusal as a formatting mistake will loop until something stops it.

ONE CLASSIFICATION, FOUR RENDERINGS. Derived from the typed errors the runtime
already had, so the surfaces cannot drift apart:

  MCP      structuredContent on the tool result
  gRPC     google.rpc.ErrorInfo + RetryInfo on the status, AND error_info on
           streamed error frames
  HTTP     error_info on the SSE error event
  in-band  a [category · retryable · retry in Ns] prefix in the tool_result
           text, on EVERY provider

That last one is not redundancy. is_error reaches the model on ANTHROPIC ONLY —
the OpenAI dialect, Gemini and Ollama have no slot for it in their wire
formats — so a classification living only in a field would have been an
Anthropic-only feature, silently absent everywhere else.

A TOKEN BUDGET IS NOT BACKPRESSURE. Both render 429, and both arrived as
codes.ResourceExhausted on gRPC, indistinguishable except by string-matching
the message. But a concurrency cap clears in seconds and a budget clears when
an operator raises it or the month rolls over. Telling an agent to retry the
second on the schedule of the first burns its attempts against a wall.

A CROSS-TENANT MISS IS NEVER A PERMISSION ERROR. It stays an opaque not-found,
because a permission category on such a read confirms the row exists and turns
the error into an existence oracle. This is guarded by a test, since it is
exactly the invariant a later "improve the error message" change reintroduces.

A successful query that matched nothing now looks nothing like a failed one.
Grep and Glob returned the bare fragment "no matches" — a fragment an agent can
read either way — and both now state that the search completed. 27
collection-returning ops report resultCount, and ZERO IS EMITTED, never
omitted: omitting it is precisely what makes a successful empty query
indistinguishable from a failed one.

A tool that does not count emits no key at all. "Did not count" and "counted
zero" are different statements.

A spawn_run whose run FAILED now returns isError: true. It previously came back
success-shaped — isError unset, the failure buried in a string inside the JSON
payload — so a caller had to parse the payload to learn the call had failed at
all. A cancelled run is still not an error: the caller asked for it.

Event.error_info is a new proto field. Taken as a protocol change rather than
bolted alongside, since there are no gRPC consumers yet.

- a chat is searchable by what was SAID, not only by what it was called —
  user turns first, then assistant turns, with a bounded backfill
- one subject reads across every scope the caller can reach
- the empty-dossier sweep could silently un-adopt a subject; fixed

- release notes stopped reaching the release page. goreleaser's mode: replace
  discards the body GitHub populates from the annotated tag, and the changelog
  is disabled, so v1.79.0 shipped a 2-byte body. The tag annotation is now
  rendered explicitly.

@loomcycle/client 1.80.0 — ErrorInfo on RunEvent.
loomcycle (PyPI) 1.80.0 — ErrorInfo on AgentEvent, absent-vs-zero preserved
through HasField.

## What's in v1.79.0

*Every tool says what NOT to use it for, and a memory subject is proposed rather than minted.*

Nine PRs. Two themes: the tool surface stopped being a list of names, and the
memory layer learned to hand an unknown subject to an operator instead of
guessing.

A tool description is the only text that reaches a model on EVERY request. The
material was mostly already written, in Go doc comments the model never sees:
`Read` shipped as "Read a UTF-8 text file from disk." while its comment
documented the 256 KiB cap, the volume sandbox and the refusal behaviour.

Every tool now states its purpose, its inputs and their constraints, what it
refuses or caps, and — the element none of them had — WHICH NEIGHBOUR OWNS THE
JOB IT DOES NOT.

  Read        not for finding files; Glob by name, Grep by content
  Write       not for partial edits; it drops what you did not resend
  Bash        not for work Read/Write/Edit/Glob/Grep already cover
  WebFetch    not for discovery, and not for a non-GET method
  subscribe_channel   at-most-once; peek+ack is the at-least-once pair
  register_agent      a TTL scratch agent; agentdef is the durable one
  get_snapshot        read it; export_snapshot moves it
  a2aservercarddef    advertises US; a2aagentdef registers a peer WE call
  path rm             removes the NAME, not the thing
  volumedef delete    keeps the files; purge deletes the tree

The MCP surface matters most here: it is read by EXTERNAL agents with no
loomcycle system prompt doing the disambiguating, so the description is the
entire briefing. Seven of those descriptions also claimed "Operator-admin-only"
when the authorization map says otherwise — stale prose that told tenant
operators they were locked out of tools they can use.

Both rubrics are enforced by tests rather than convention, and the MCP one
ranges over the tool surface itself, so a new tool is held to it the day it is
added.

INTROSPECTION LEAKED THE CATALOGUE. The Context tool is deliberately pointed at
the runtime-wide tool set, so every introspection op is a disclosure decision.
Its filter was a plain map lookup that failed in BOTH directions: an unstamped
or empty allowlist disclosed EVERYTHING, while the "*" that every operator
surface passes matched nothing and disclosed NOTHING. A grant of
`mcp__slack__*` disclosed none of that server's tools either. All three ops now
use the same matcher the exposure layer and the substrate tool-ceiling checks
already agree on, and an absent list discloses nothing.

ONE AGENT COULD CHOOSE WHAT ANOTHER READS. A team node's threaded input is the
previous state's OUTPUT, and it was being handed to the placeholder expander.
An agent ending a turn with `{{document:/secrets/…}}` had that document inlined
into the NEXT agent's prompt, under the runtime's authority, with reach neither
needs the Document tool to use. Threaded output now travels in a data slot —
substituted after expansion, never scanned — the mechanism already built for a
Starter's source message.

- a fact records every subject it names, not just the first
- an unknown subject is PROPOSED for an operator to adopt, never minted from a
  transcript; adoption shares it from that point on and never backfills
- retiring a fact now closes the k/v plane and the fact graph in one op
- a reclaimed scope no longer leaves its chunk bodies behind
- empty subject dossiers are swept, opt-in and off by default

- the TS adapter's TESTS are typechecked. vitest strips types without checking
  them, so no adapter type was guarded by anything — deleting a field from
  types.ts broke no build and failed no test. Turning the check on surfaced two
  live bugs where a mocked error body was never the JSON the test was named for.
- generated stubs are gated against the .proto, ignoring generator version
  stamps so the gate answers "do the stubs match the schema" rather than "was
  the same binary used". It caught a `make python-proto` target that had never
  worked on Linux.
- goreleaser is `mode: replace`, so a release re-run no longer dies on
  already_exists before reaching the Homebrew step.

@loomcycle/client 1.79.0 — gains `propose_subject` on DocumentToolInput.

loomcycle (PyPI) 1.79.0 — first publish since 1.67.0, and it carries the gRPC
`walk_id` filter plus `parent_context` lineage: the team-walk view that reached
HTTP, MCP and TypeScript earlier is finally reachable from Python. Released on
its own python-v1.79.0 tag.

## What's in v1.78.0

*Prompt bindings that reach, behind a guard that holds.*

RFC CY Track A completes the prompt-expansion families, and the walk filter
reaches every transport. Nine PRs.

An operator writing an agent's prompt could bind a document or the tool
inventory into it, but not a memory key, not a search, and not a page. A second
colon now turns both families into an argument form:

    {{memory:key:launch}}                 one stored entry, the run's own scope
    {{memory:search:deploy checklist}}    the top matches
    {{tool:WebFetch:https://docs/x}}      that page, fetched once at run start
    {{tool:WebSearch:release notes}}      that search

All four fold into the SINGLE combined regex the families already share, so a
placeholder sitting inside injected content — a fetched page, an agent-written
memory — is still text rather than an instruction the runtime executes.

GATED TWICE, and the two gates fail in different directions.

  Authorship. Only a definition an OPERATOR wrote may use the argument forms.
  The guard covers ONLY what was added: every family that worked before keeps
  working for every definition, legacy rows included. Gating those would strip
  expansion from working defs the moment the migration ran — a guard that
  breaks working defs on upgrade is an outage, not a guard.

  Trust rule 5d — a placeholder guard is not a NETWORK guard. The argument
  admits ${…}, and variables bind from untrusted sources. The operator authors
  the template; an attacker must not pick the target, and a charset check
  cannot help because a URL charset spells any host. So a resolved value that
  becomes a network target must be on the operator's STATIC
  http_host_allowlist. Parameterised fetches work; choosing an unlisted host
  does not, and the refusal names the host.

Prompt assembly can now block and fail, so the network work is bounded by one
5s budget for the WHOLE assembly and fails soft: a refused, unreachable, slow
or empty page renders nothing and the run proceeds.

Who wrote a definition is now recorded on both def planes — agent_defs and
teamdefs — stamped from the runtime's own view of the caller, never from
anything the caller supplies, and excluded from content_sha256 so a fork across
deployments still verifies.

IN A TEAM NODE'S PROMPT THE AUTHOR IS THE TEAM, and the two segments have
different authors: `system` is always the team's text, `input` is the team's
template when the node declares one and the PREVIOUS STATE'S OUTPUT when it
does not. Threaded output is never treated as authored — otherwise an agent
could emit {{tool:WebFetch:…}} and have the next node's assembly fetch it,
which is 5d arriving through the back door.

`?walk_id=` landed on the HTTP run-state stream in 1.77.0 but reached no typed
client. A walk's own run id IS its walk id, so a caller that started a team
detached filters by the handle it already holds:

  TS     streamUserRunStates(userId, { walkId })  + filter_walk_id on the open
         frame, + walk_id / wave_id / wave_index on ParentContext
  gRPC   StreamUserRunStatesRequest.walk_id, and RunStateEvent.parent_context —
         a new message, carrying lineage gRPC has never had
  Python stream_user_run_states(..., walk_id=) and a parent_context key on
         every event dict

wave_id groups a fan-out and wave_index is the position inside it, so a live
view can place an agent WITHIN a walk rather than merely inside it.

- a fact references every subject it names, not just the first
- the extractor is windowed by default — 8 turns, guarded on short sources
- a run knows its chat, so a queued fact can be followed home
- the evaluation gate stops being a coin flip: sampling is pinned, so the
  suite measures the prompt instead of an unpinned sampler

- operator_authored survives a snapshot. Capture/restore silently DEMOTED
  every operator-authored definition, so an operator restoring onto a new
  deployment found their own defs no longer resolving bindings that worked
  before the capture.

@loomcycle/client 1.78.0 · loomcycle (PyPI) 1.78.0 — both publish on this tag.

## What's in v1.77.0

*The TeamDef workflow substrate (RFC CY), and facts get one home (RFC CV).*

Two lines land in this release.

A team is no longer just a state machine you invoke. It reads its own work,
fans it out, publishes results onward, and can be watched and stepped while it
runs.

**The `starter` primitive.** The dispatcher the design was missing: it reads
ONE channel, fans out a wave (one run per message, dynamic N), and the RUNTIME
publishes each result to a declared sink. One subscriber means one cursor, so
a fan-out is correct by construction, and agents in a wave need no channel
grant in either direction — the team is the ACL subject. Every spawned run
emits exactly one sink message on a guaranteed path, so a downstream fan-in is
unblocked by failure rather than hanging on it.

**A walk is a run.** `TeamDef op=run` now opens a session and a `runs` row
filed under `team:<name>`, and `mode: "detach"` returns `{run_id, status}`
immediately with the walk continuing behind it. That run_id is also the walk's
correlation id, stamped on the `parent_context` of every agent the walk
spawns — one handle for the response, the run row, the event stream and the
debugger.

**Debug a workflow while it runs.** Breakpoints on a Starter pause before
dispatch (every prompt composed, nothing spawned) or after collection (the
wave done, nothing published). Answer `continue` / `release:<n>` / `abort`
through the existing Interruption machinery. The armed set is read at every
pause, not captured at dispatch, so a state can be armed AFTER a run
started — the case you are actually in when you watch a wave go wrong.

    PUT /v1/runs/{run_id}/breakpoints   {"breakpoints": ["review"]}
    GET /v1/users/{user_id}/agents/stream?walk_id=<run_id>

**Authoring catches what used to fail at run time.** create/fork preflight a
definition's channel references and refuse an unrunnable one with the exact
block to paste; `op=verify` reports `runnable` plus the issues a stored def has
accumulated (a channel deleted, an ACL gap, a member retired).

**Armed subscriptions** (`LOOMCYCLE_TEAM_SUBSCRIPTIONS=1`, default off) drive a
promoted team when its source has work, one replica at a time via a per-team
Postgres advisory lock. This is the only part of the runtime that starts agent
runs unprompted — turning it on is a spend commitment, and it is off until you
say otherwise.

**`system-channels` bundle** (`LOOMCYCLE_PRESETS=base,system-channels`)
declares the runtime's own channels a workflow can read — three heartbeats and
the interruption pair. A bundle rather than a default because a ticker writes a
row every period on every deployment, forever.

**TypeScript adapter 1.77.0** carries the whole surface: `runTeam({mode:
"detach", breakpoints})`, `getRunBreakpoints`, `setRunBreakpoints`.

The memory line moves facts onto the chunk plane: a subject is its own
document and its facts are its children, a fact is reachable from every
subject it is about, and it can be traced back to the turn it came from. The
tenant entity registry is curator-gated.

- **The wave correlation was being dropped on write.** `ParentContext.IsZero()`
  did not list the wave fields, so every run a Starter spawned stored a NULL
  `parent_context`. The guard is now derived from the struct.
- **An "open" channel ACL granted nothing.** Three def-authoring planes
  declared `Publish: ["*"]`, which the matcher never matches — so no Starter
  team was authorable-and-runnable over MCP, gRPC or the admin HTTP surface.
- **Channel `hold` is honoured by every writer**, not only the two that
  resolved the definition.
- **A resolved `{{document:}}` ref is re-checked against its charset**, closing
  a path where an untrusted variable could escape the frame into a system
  prompt.

Nothing is required. Every new behaviour is opt-in: a team without a `starter`
runs exactly as before, `breakpoints` and `mode` are absent unless passed,
armed subscriptions and the system-channels bundle are off until selected.

## What's in v1.76.0

*Temporal memory that answers the question, and an agent editor with no textarea.*

A minor release: full build (all binary variants, multi-arch Docker, sandbox
image). Two lines land here — the memory subsystem's temporal fields becoming
real end to end, and the Library agent editor exposing every parameter an agent
has. No schema migration; no wire-breaking change.

The benchmark number this line is chasing: the raw-turns arm answers temporal
questions at 0.873 while the distilled-facts arm answers them at 0.18. The
fields meant to close that gap existed but were not being filled, not being
selected, and not being followed.

- `observed_at` is now parsed FROM THE TURN rather than asked of the model
  (#1143). Measured across three full runs, asking for it filled it on 0 of
  ~240 facts — the model kept dates in the fact prose instead of the field.
- An unresolved time reference is now kept and `valid_at` resolved from it
  (#1146). `observed_at` ("when it was said") was already saturated at 100%;
  `valid_at` ("when it was true in the world") sat at ~19% and is what the
  questions actually ask — 60 of 63 temporal gold answers name the EVENT's
  date, not the utterance's.
- `recall` now reaches through to the span a fact was distilled from (#1147).
  The store already held that provenance and nothing at retrieval followed it.
- The extractor is asked for the time on the path that has one (#1139), and a
  question is no longer taken as evidence for a fact (#1153).
- An extraction-granularity knob plus rolling fact context (#1155), with the
  window reaching the queued path (#1156), a chat scanned WHOLE rather than
  trimmed (#1157), and the queued batch windowed by MESSAGE rather than by
  queued item (#1158).

Three read-path fixes make those columns survive the round trip: `MemoryList`
returns the temporal columns it was silently dropping (#1144), the remaining
Postgres memory reads carry them too (#1148) — a read path that does not SELECT
them hands back the zero instant, and zero is MEANINGFUL on these columns — and
a memory snapshot round trip keeps them (#1149).

`path op=ls` takes a `limit` and an opaque `cursor` (#1140). It previously
accepted neither, so a caller could not ask for less than the whole directory —
fine today, and not fine for a listing whose size is the tenant's entity count.
`@loomcycle/client`'s `PathToolInput` carries both fields.

The agent editor gained a second, switchable edit surface, and the raw
JSON/YAML overlay box is deleted (#1159, #1160).

An agent has 47 persisted overlay parameters. The editor gave structured
controls to 19 and put the rest behind a free-text textarea with no validation
until submit, no bounds, and no explanation of what any key meant. It was the
third attempt at that idea; each failed the same way, because a textarea cannot
explain a parameter.

- **Form** — the curated layout, unchanged and still the default.
- **All parameters** — every parameter, one row each, under collapsible groups:
  a typed control, its bounds, its overlay key and an always-visible hint per
  row; per-group "n set" badges so a CLOSED group still says what the def
  overrides; groups open by default at exactly the ones holding values.

Both surfaces are driven by one declarative registry, so a parameter is a
single entry rather than a state hook plus JSX plus an overlay builder plus a
hand-maintained covered-keys set. A drift test pins the registry against the
persisted overlay's Go shape, so a new backend parameter cannot ship
unreachable.

The registry and the editor ship as **@loomcycle/def-fields 0.1.0**, a
standalone package with react/react-dom as its only peers, so the loomboard
canvas can reuse the same controls without pulling the Library UI.
**@loomcycle/library 0.5.0** consumes it as a required peer dependency.

LongMemEval added as a second corpus with abstention scored correctly (#1142),
its non-string answers handled (#1154), and a `-answer-only` mode that grades
the store as it stands (#1152). The retention export is pinned to carry
`doc.chunk` body rows (#1141), coordination test replica ids now pass
`ValidateReplicaID` (#1150), and the coordination tier is gated on the Postgres
job (#1151). `make build-ui` installs the new package's deps (#1161) — the
release build runs that target rather than CI's own install list, and the two
had diverged.

`@loomcycle/client` remains at 1.72.0, so the npm publish is skipped for this
tag and the `PathToolInput` additions above are in-tree but not yet on npm.

## What's in v1.75.1

*The memory embedder resolves its endpoint from `providers:` yaml.*

A patch on the v1.75 line. One runtime fix plus the memory-architecture guide.

Repointing `ollama-local` at a new Ollama host the documented way —
`providers: ollama-local: base_url:` in loomcycle.yaml — moved CHAT but not the
EMBEDDER. providerbuild prefers the map entry, while the embedder read
cfg.Env.OllamaBaseURL only, so the two halves of one provider account resolved
independently.

Observed live: chat reached a newly-installed Ollama box and billed 50 calls
while every embed failed `dial tcp: lookup <host> on 127.0.0.11:53: no such
host` — the env var named a Tailscale MagicDNS host, which does not resolve
against Docker's embedded DNS inside the container.

An embed failure warns but does not fail the write (by design: the k/v row is
kept, the response carries `embedded:false` + `embed_warning`), so a caller that
ignores that field just accumulates unembedded rows. A live scope reached 108
fact rows with ~0 embeddings and `/v1/_memory/search` 500'd, while every write
still reported 200.

The two copies of the per-provider base-URL switch are now one shared function.
base_url precedence, highest first:

    memory.embedder.base_url > providers.<id>.base_url > the per-id env default

YAML is the config home; the env var is only the floor. Chat resolution is
unchanged — byte-identical inputs to every driver factory.

Two carve-outs, both covered by tests: `anthropic` inherits NEITHER half of its
chat entry (that embedder slot is a Voyage AI proxy, so an Anthropic proxy
base_url would receive Voyage requests and ANTHROPIC_API_KEY would be sent to
Voyage), and the per-id env KEY stays the last resort for a deployment with no
`providers:` block (the openai embedder accepts an empty key at construction and
only fails later at 401).

Operators: if the embedder endpoint is set via `memory.embedder.base_url` it
still wins by design — remove it to inherit the `providers:` entry. Either way
the address must be one the CONTAINER can resolve; prefer a tailnet IP or a
compose service name over a MagicDNS name. `PUT /v1/_memory/scopes/<scope>/<id>/keys/<key>?embed=true`
returns `embed_warning` naming the endpoint and the error — the whole diagnosis
in one call.

The memory architecture guide, with structure and state diagrams, exported from
the document store to docs/MEMORY-ARCHITECTURE.md.

Tagged as a patch but released with force_full, because the fix is in the Go
runtime: the patch tier alone builds only loomcycle-browser. Adapters are
unchanged and their publish jobs skip clean on version mismatch (no wire change).

## What's in v1.75.0

*Recall the agent can actually reach; one tenant mapping; a benchmark.*

that refuses a rigged partition

THE RECALL TOOL IS AUTO-GRANTED WHEN RECALL IS ON (#1135)

v1.73.0 shipped recall-augmented distillation: an opt-in context.recall harvests
every evicted span into a run-scoped index. The agent could not query it. An
empty `tools:` allowlist is default-DENY rather than default-all, and a populated
one easily omits Recall — so a recall-enabled agent had the index and no way to
reach it, and the docs implied a default-all toolset that does not exist.

An end-to-end run found it the way these things get found: the agent, asked for a
value distillation had evicted, CONFABULATED rather than recalled, because Recall
was never in its toolset at all. Enabling recall now auto-grants the builtin at
every toolset-resolution site — RunOnce, the HTTP run path, session-continue,
sub-agent spawn and resume. Read-only over the run's own evicted spans and the
agent's own memory scope, so it widens the allowlist and not the trust boundary;
a no-op when recall is off or no embedder is configured.

ONE TENANT MAPPING (#1136)

The `"" -> "default"` mapping that turns a runtime tenant into a SQL Memory scope
key had FOUR implementations across three packages, each with a comment naming a
DIFFERENT one as the source of truth and nothing asserting they agreed. They did
agree, byte for byte, which is the only reason this was latent rather than live.

Divergence is not cosmetic, and the erasure call site already spelled out why: a
DropScope built from a raw "" tenant matches nothing, so a single-tenant
deployment's subject erasure would leave the subject's ENTIRE SQL Memory database
in place while REPORTING SUCCESS. The rule now lives once, in internal/sqlmem —
the package whose own constraint creates it, since pgScopeNames must reject an
empty tenant or every single-tenant deployment would share one schema and one
LOGIN role. Pinned by a test covering the empty case, a tenant literally NAMED
"default", and that distinct tenants still derive distinct schemas.

A BENCHMARK THAT REFUSES A RIGGED PARTITION (#1136)

The answer axis already refused an EMPTY store, on the stated grounds that
"accuracy 0.0000" is a number about the plumbing wearing the costume of a result
about memory. That was not enough. With the corpus tenant's ontology declaring a
tenant memory scope for its own entity types, the consolidator PLACED most facts
into the tenant scope while the answerer recalls from the user scope only. Enough
rows remained for the empty check to pass, the run scored 0.0216 with 95%
abstention, and three plausible mechanisms were reasoned on top of that number
before anyone compared the counter to the store.

The guard now compares them: how many facts the pass reported writing against how
many are reachable where the answerer looks, refusing on a shortfall of more than
half and naming placement as the usual cause. Chunk-body rows are deliberately
not counted — counting them is exactly what made a diverted partition look
populated.

WHAT THE MEASUREMENT SAID, since three releases served it

Consolidated facts versus raw turns, same corpus, same 199 paired questions:
0.7383 for turns against 0.1574 for facts, McNemar exact p = 2.4e-29. The
mechanism is abstention (76% versus 15%) rather than retrieval, and not yield —
7.2:1 and 9.0:1 compression with exact counters. Temporal is the sharpest slice:
0.873 against 0.036, because a raw turn carries its timestamp in the text and
distillation strips it. Consolidation's demonstrated gain on that corpus is about
one question in 199.

The operational consequence is why #1135 matters more than it looks: `Memory add`
with the default infer=true stores no retrievable row, so a deployment's recall
path IS the losing arm. Recall-augmented distillation is the answer to that, and
until this release the tool was unreachable.

THE SELF-GUARD LIMITATION, STATED (#1134)

Placement's most reassuring promise was overstated. "It never places a fact about
you" holds only for facts learned from that owner's OWN conversations: another
user recording the same thing is recording a fact about a third party, so their
copy is placed. Measured on a two-user corpus, each user published the other
speaker's facts tenant-wide, leaving each owner's facts MORE exposed than before
placement was enabled. Not a bug — what per-scope decisions with no global view
produce. The shipped ontology template now says so in the place an operator reads
while deciding, and docs/TOOLS.md documents the mechanism for the first time.

VERSIONS

@loomcycle/library is 0.4.0 for the capability-gates renderer (#1133) and
publishes on its own library-v tag. The TS adapter stays at 1.72.0 and the Python
adapter at 1.67.0, so both skip clean here. @loomcycle/memory-view 0.5.0 is
already on npm.

## What's in v1.74.0

*What distillation drops can now outlive the run.*

PERSISTENT-MEMORY HARVEST ACROSS DISTILLATION MODES (#1131, RFC CT P2)

v1.73.0 made an evicted span recoverable WITHIN its run, through a
run-scoped index and the Recall tool. This is the cross-run sibling: an
opt-in per-agent `context.harvest_to_memory` banks each evicted span for the
memory consolidator, so what a distillation drops can become a durable fact
instead of being lost at the end of the run.

It generalizes what already existed for exactly one mode. `compaction.memory_flush`
banked the span behind a compaction cut and nothing else; banking now happens at
the recap and stateful boundaries too — the same three places the P1 recall index
harvests. The banking callback is installed when either flag is set, so an
operator relying on memory_flush is unaffected.

BANKING RATHER THAN INLINE EXTRACTION, and the choice was measured. Banking
hands raw spans to the existing consolidator instead of extracting per span on
the hot path. RFC CU Probe 2 found that per-span isolated extraction LOSES
coreference-dependent facts — a subject named two spans earlier — that the
consolidator's whole/batched extraction keeps: 0.75 versus 0.00 on user-project
coreference, McNemar p=0.0010. Broader context and no model call on the
distillation path is the right shape.

A misconfiguration is now loud rather than silent: no store, no user scope, or no
user_id surfaces as an EventError instead of a run that simply never harvests. A
banking failure never fails the run. The banked span's metadata source
generalizes from "compaction" to "distillation" since it now covers all three (no
consumer branches on it). Deliberately absent on resume, which replays past
distillations. Off by default, and content-identifying like the rest of the
context block, so a fork that flips it mints a distinct content_sha256 while every
pre-feature agent row stays byte-stable.

The server now resolves the merged context once per run, removing three redundant
recomputes in the run builders.

RELEASE HISTORY AND THE RECALL TOOL DOCUMENTED (#1132)

REVISIONS.md had stopped at v1.71.0 while three tags shipped; v1.72.0 and v1.73.0
now have sections. The v1.72.0 entry also records what the memory-placement work
MEASURED, including the part that does not flatter it: duplicate copies fell 30 to
23, which is -23% and not elimination, because cross-user duplication was replaced
by tenant-to-owner duplication. The payoff that does hold is sharing — 94 facts
readable by both users where none were before — and the self-guard's limitation is
stated as a limitation: it cannot protect what the counterparty also recorded.

The `Recall` tool shipped in v1.73.0 appearing in no tool list at all is fixed:
README, docs/TOOLS.md and the project guide now carry it. The TOOLS.md entry as
first written was wrong, taken from a commit message, and reading the registration
site corrected it — the tool is registered UNCONDITIONALLY like Memory, because
only the run-scoped index is conditional and the durable-memory search is useful
to any agent that grants the tool.

VERSIONS

The TS adapter is unchanged at 1.72.0 and the Python adapter at 1.67.0, so both
publishes skip clean on this tag. @loomcycle/memory-view sits at 0.5.0 and
@loomcycle/library at 0.3.0; both publish only on their own `memory-view-v` /
`library-v` tags, and the embedded Web UI consumes both from SOURCE, so this
binary ships their current state regardless.

KNOWN DOC GAP

README's capability table does not yet mention `context.harvest_to_memory`
alongside `context.recall`; the operator reference in docs/CONFIGURATION.md
covers it.

## What's in v1.73.0

**Recall over what distillation threw away (#1129, RFC CT P1).** Every context
retention mode in this runtime discards work by design: a compaction drops the
turns behind its cut, a recap keeps the reasoning and drops the rest, and a
stateful step feeds forward `(Σ, O)` and discards how it got there. That is the
point of them — but the dropped span is often exactly where the one value the
model now needs was stated.

An opt-in per-agent `context.recall` harvests each evicted span, at the last
moment it exists, into a run-scoped embedded index, and gives the agent one
`Recall(query)` tool to read it back. **Free text rather than identifiers**,
because a model queries fluently in plain language and barely reproduces ids or
its own exact prior wording — which also makes the tool far likelier to be
invoked at all. It searches the run index, silently falls back to the agent's
durable memory across its permitted scopes, merges by score, and returns the
originals verbatim.

Run-scoped and in-memory deliberately: the persistent vector store is per-scope
and durable, so indexing every evicted turn there would pollute the agent's
memory and add store writes to the distillation hot path. A harvest is never
fatal, a nil embedder makes both halves a clean no-op, and the index is
FIFO-capped. The resume path is deliberately **not** wired — a resumed run
replays its past distillations without harvesting, so nothing double-indexes.

Off by default: unopted runs and no-embedder deployments are byte-identical, and
`recall` threads through the same content-identifying plumbing as the rest of the
context block, so a fork that flips it mints a distinct `content_sha256` while
every pre-feature agent row stays byte-stable.

**The Memory console honours the tenant focus (#1130).** The last hop of the
admin-focus fix below. The server had accepted `?tenant=` since v1.72.0 and
`@loomcycle/client` 1.72.0 could send it, but the console was not asking, so a
super-admin's Memory page could only read its own tenant — usually the empty one
— while that tenant's operator saw its rows. Every other browse surface
(Documents, Paths, Agents, Users) already consumed the topbar focus; Memory was
the one that never did.

The focus binds at data-layer **construction**, not per call. Which tenant's
workspace an operator is looking at is a property of the console session, so
threading it through `MemoryDataLayer` would have put the same never-varying
value into twenty call sites and changed a 21-method interface to carry it. A
`browse` option captured by `dataLayerFromClient` reaches every method without
any of them knowing it exists. `listScopes` deliberately does not carry it: that
route answers "what *kinds* of scope exist", a constant set, and sending a tenant
would imply the answer varies by tenant.

Verified on a live deployment before the tag, with two tenants holding 0 and 136
rows: an admin naming none still gets 0 (the default is unmoved), an admin naming
the tenant gets 136 (the capability is real), and a tenant operator naming
*another* tenant gets its own 136 — the wire value ignored, not honoured and then
checked.

`@loomcycle/memory-view` is 0.5.0, with its `@loomcycle/client` peer raised to
`^1.72.0` so a consumer resolving an older client gets a dependency error rather
than a console that silently sends nothing. The TS adapter is unchanged at
1.72.0 and the Python adapter at 1.67.0.

## What's in v1.72.0

Three fixes found by *using* the runtime rather than reading it. Each was an
operator question the console answered wrongly, or not at all.

**A super-admin may focus a tenant on the memory browse routes (#1127).** An
admin was strictly **less** capable than a tenant operator here, which is
backwards: `substrate:admin` satisfies every scope gate and then could not read
the data the gate admits. The five memory browse handlers resolved the tenant
from the bearer and took nothing from the URL, so a `substrate:tenant` token read
its own tenant's memory while an admin saw only its own — usually empty — and had
no parameter available to look elsewhere. From the console that reads as a
permissions failure.

The inconsistency was inside one file: the sibling maintenance routes on the same
store — embed stats, reembed, backfill, purge — had accepted `?tenant=` via
`principalTenantScope` all along. Only the reads and writes an operator actually
browses with were pinned.

Only an **explicit** focus widens. With no `?tenant=` the resolution is
byte-identical to before, so this adds a capability rather than moving a default,
and a non-admin's wire value is ignored rather than honoured-then-checked — no
tenant can widen its own scope.

Three helpers resolve a tenant and they disagree on the admin default. That
disagreement is irreducible: a **list** read can mean "every tenant" while a
single-tuple read must name exactly one. It is now written down once, naming all
three and the invariant they share, with every cell of the table pinned by a
test. What made it a defect was not the difference — it was that nothing
asserted it, so the difference read as an accident, and on these routes it was.

**The Library shows what an agent is allowed to reach (#1126).** An operator
asking "what can this agent do?" got half an answer, and it cost real diagnosis
time: while checking why memory placement was not working, the Library showed no
`sql_scopes` at all, which reads as the grant being absent. The grant was
present. Only the field was missing.

Two independent gaps produced one symptom. The wire shape was a hand-maintained
third mirror of the substrate agent shape — its own comment said mirroring was
its purpose — and it had drifted to **19 of 46 fields**: `sql_scopes`,
`history_scope`, `memory_consolidation`, `internal`, `sampling`, `compaction`,
`context`, the five `*_def_scopes` authoring gates and eighteen more never
reached the client. Rather than add the missing 27, the mirror is deleted: a
converter makes the authoritative struct the wire shape, so there is no third
thing left to fall behind. And the renderer showed no capability gate at all, not
even `memory_scopes`, so even served none of it would have appeared — an agent
granted the tenant plane looked identical to one confined to its own scope.

The guard is two assertions because they catch different faults. **Coverage**
populates every field and requires none left at zero, which catches the forgotten
assignment. The **round trip** requires equality, which catches a field wired to
the *wrong* source — `SqlScopes: def.MemoryScopes` leaves nothing zero and is
still wrong. A round trip alone would also miss a symmetric omission, since a
field dropped by both directions survives it untouched. Both faults were verified
by injection rather than argued.

**Tenant focus on the client's memory admin methods (#1128).**
`@loomcycle/client` 1.72.0: the five memory browse methods take an optional
`tenant`. `listMemoryScopes` deliberately does not. A blank focus is dropped
rather than sent, because the Web UI's switcher is empty until an admin types one
and empty there means "my own tenant", not a tenant named `""`.

### Known state, stated plainly

**This release closes a chain that began with ontology-declared memory
placement.** v1.71.0 made placement reachable at all — the shipped consolidator
had held no `sql_scopes`, so neither half of a placed fact could reach the tenant
plane — and v1.71.1 fixed placed facts arriving **orphaned** from their subjects,
because the `about` edge joining them was written in the caller's scope where
neither endpoint existed. Neither has its own section here: patch releases carry
their notes on the annotated tag.

**Placement has now been measured.** On a two-user corpus, duplication fell from
30 duplicate copies to 23 — cross-user duplication was eliminated and *replaced*
by tenant↔owner duplication, so the honest figure is −23%, not elimination. The
real payoff is sharing: **94 facts are readable by both users where 0 were
before**. A second-order effect was unpredicted — cross-user type stability, with
`subject types held stable` at 4 for the user who wrote first and **91** for the
second, whose extractor kept proposing new types for subjects already on file in
the shared plane.

**The self-guard cannot protect what the counterparty also recorded.** Each user
played one speaker in the corpus. The guard correctly kept a speaker's own facts
in their scope, while the *other* user, for whom that speaker is a third party,
published the same facts to the tenant plane. So placement left the owner's facts
more exposed than baseline, which is precisely what the guard exists to prevent.
Not a bug — the consequence of per-scope decisions with no global view. In any
multi-party corpus the guard's promise is defeated by the other party, and the
narrow declarations (`organization`, `project`, `service`) are the safer default.

## What's in v1.71.1

*A placed fact stays reachable from its subject.*

A PATCH, BUILT FULL. The fix is in the embedded memory bundle, so it ships in
the runtime binary and image rather than the browser image a patch tag
normally builds alone. Released with force_full.

WHAT WAS WRONG (#1125)

v1.71.0 made ontology-declared placement reachable for the first time. Running
it end to end showed that a placed fact was stored in the shared plane and
orphaned there.

`mirrorEntity` threads the placed scope into every write it makes — the
entities document, the canonical-type lookup, the subject node, the fact node
— except the `about` edge that joins the two nodes, which used the pass's
configured scope. So both nodes landed in the tenant plane and the edge was
written in the caller's scope, where neither endpoint exists. The link failed,
a counter incremented, and the pass carried on.

Measured on the live store: 3 facts placed, 3 edges lost, 0 inbound edges on
the subject node. The facts sit in the plane every user reads and a graph walk
from the person's name reaches none of them — which is the single property the
entity tier exists to provide. One word: the placed scope, not the configured
one.

The judge had the same defect one function over. A candidate carried its id but
not the scope it was written in, so judge_fact looked for a placed fact in the
configured scope and could only fail. The scope now travels with the candidate.

Two silent failure sites now record the reason, matching the two that already
did. This is why the diagnosis needed a code read: the deployed build reported
"3 graph write(s) failed" and nothing more, while the mechanism for saying why
was already there and simply not wired at these sites.

WHY NO TEST CAUGHT IT

The test double was scope-blind. Its chunk store was natural_key -> id with no
record of where a chunk was written, so every scope answered identically, and
the sibling test — which asserts the row, the chunks AND the entities document
all reach the declared scope — passed throughout without ever looking at the
link. A double has to model the dimension the feature varies. It now records
each chunk's write scope and refuses a link naming a scope neither endpoint
lives in, as the real store does, and reproduces the production report
verbatim.

The adapters and protos are unchanged and remain at 1.67.0.

## What's in v1.71.0

Two lines land together: the layered-context work reaches the multi-agent case, and
ontology-declared memory placement becomes reachable at all.

**A stateful sub-agent hands its state up, not its transcript (#1122).** When a fan-out
parent spawns a stateful child, the parent now receives the child's final structured
state Σ as the result rather than only its prose. A single spawn folds Σ into the
child's `tool_result` as JSON; `parallel_spawn` carries it as a structured `state` field
on each envelope entry; the spawn ledger captures it so a parent restored from a
snapshot keeps a completed child's Σ. That turns N growing transcripts into N compact
structured results — `O(T²)`-per-agent × N becomes N independent `O(T)`. Non-stateful
children are unchanged, and the Team orchestrator is string-only end to end, so
carrying Σ there is still a follow-on.

**`context.mode: auto` routes by the model that actually resolved (#1120).** An
operator sets `auto` once and the agent runs schema-free **recap** on a local backend
and structured **stateful** on a frontier API, instead of hand-picking per deployment —
because structured state needs reliable structured output and a weaker local model
cannot be relied on for it. Providers gained a `Local` capability to make that a routing
fact rather than a guess (`ollama-local`, vllm and llamacpp are local; the hosted
`ollama` is not). The mode resolves once at run start on a clone of the context, so the
shared agent def is never mutated; an interactive run never resolves to stateful, since
that loop has no steer/park. An explicit mode still wins, and an agent with no context
block stays `append` — byte-identical, so `auto` is opt-in. The same PR closed a
transport gap: the per-run `context` override now flows through the connector and MCP
`spawn_run`, matching `sampling` and `compaction`, which were already there.

**A model may propose a state schema; only an operator adopts it (#1121).** A stateful
agent can suggest the shape its task's state should hold via `emit_state`'s
`propose_schema`. The proposal is inert — recorded on the transcript, returned on
`RunResult.ProposedSchema`, surfaced only when it differs from the active schema — and
changes validation for no run. Adoption reuses the versioned agent-def substrate: an
operator forks the def with the schema in `context.state_schema` and promotes it. Same
fail-safe as the ontology's propose→adopt, and no bespoke store for it.

**Ontology-declared placement was unreachable, in every deployment (#1123).** v1.68.0
shipped the mechanism and v1.70.0 fixed the extractor that was starving it, but nothing
could place a fact: the shipped consolidator held `memory_scopes: [agent, user]` and no
`sql_scopes` at all. A placed fact is stored **twice** — a k/v row, which `recall`
searches, and a typed chunk, which a graph walk reaches — so `tenant` has to be on both
grants or the two halves cannot land in the same scope. The bundle now grants them.

The absence had been deliberate, on the argument that an unused grant is the capability
an injected instruction reaches for. That argument is written for a *prompt-driven*
agent, where model output steers control flow. The consolidator is `provider: code-js`:
a deterministic body in which model output is data that gets written and never code that
gets run. What bounds the tenant write is the placement resolver, which fails closed on
every uncertainty — an unknown or inconsistently-typed subject, or a fact about the
profile owner, stays with the user. The grant is also inert on its own: no shipped
ontology type declares a scope, so nothing is placed until an operator declares one for
a type, per tenant. Placement remains **off** by default.

**Both ways to change a bundled agent's grants were broken, and finding out cost an
instance (#1123).** A runtime `AgentDef fork` to flip one grant returned
`fork: definition (139169 bytes) exceeds max 131072` — for a code body the overlay never
touched. `MaxCodeBytes` (256 KiB) exists precisely so executable source is not judged by
the whole-definition cap (128 KiB), but the definition check measured the JSON with the
body inside it, so any body between the two passed the cap written for it and was
refused by the smaller one. The consolidator's ~133 KB sits in that dead zone; the
dedicated cap could never bind. It now measures the definition without the body, on
create and fork alike, and the stored definition is unchanged.

The config route merges correctly — a re-declared bundle agent keeps its body, provider,
tools and gates — but layered onto a stack that does *not* contain the bundle, the same
overlay is indistinguishable from a new agent, so validation refuses it with
`no model, no tier, and no defaults.model`. Accurate, and it sends the reader to inspect
the overlay they just wrote rather than their `LOOMCYCLE_PRESETS`; boot is fatal, so the
wrong first guess costs a down instance. A declaration carrying nothing but capability
grants now says so and names the layer stack to check. Two statements of the `sql_scopes`
enum that listed `agent, user, run` long after `tenant` was added — one of them the
validation error itself, so a *correct* config read as rejected — are fixed, and the
message is now rendered from the set.

### Known state, stated plainly

**Whether declared placement is worth having is still unmeasured.** It is now reachable
for the first time, which is the precondition, not the result. The baseline it has to
beat was measured on a two-user corpus before placement could fire: **30 duplicated
facts, 22%** of the store.

**The `sql_scopes` grant is wider than this needs.** The gate consults a single list, so
opening the Document tenant-chunk path opens the raw `Memory sql_exec` surface on the
tenant keyspace with it; there is no narrower grant today. The consolidator body issues
no SQL op, and a test now asserts it does not, so adding one means re-arguing the grant.

**A failed fork can leave an orphan def row.** `AgentDefCreate` bootstraps a v1 row from
static config before the cap check runs, and does not set the active pointer — so
nothing serves it and resolution still falls through to static config. Do not `promote`
such a row: it captured the pre-change definition.

The adapters and protos are unchanged and remain at 1.67.0, so no `python-v` tag
accompanies this release.

## What's in v1.70.0

*(This entry was reconstructed from the annotated tag, which is authoritative — the
release was cut without a `REVISIONS.md` section.)*

**The extractor is told who the owner is, or told nothing (#1118).** v1.68.1 gave the
extraction prompt a rule worse than no rule, and a live two-speaker run proved it: it
said *"a fact about THEM takes the subject `user`"* and never said who THEM was, because
the declared Identity names live in the user-root document, which reaches
`{{memory:user_info}}` and not the extractor. So the model guessed the more prominent
speaker. In a scope whose profile declared Dave, it made `user` = Calvin — **inverting
the self-guard**, protecting Calvin's facts as if they were the owner's while leaving
Dave's, the actual owner's, placeable. Protecting the wrong person while exposing the
right one is worse than doing neither. `Context op=self` now reports `self_names`,
parsed server-side by the one function that owns the column-0 rule that stops the
template's own indented example from naming every unedited profile after it. The names
are **omitted**, not empty, when nobody declared any, so a caller can tell "nobody said"
from "said nothing"; the prompt then asks only for consistent spelling rather than
inviting the model to identify an owner it cannot know.

**Layered execution context (#1117, #1119).** L1 reasoning-recap context retention and
L2 structured execution state — the first two phases, with an RFC 7386 merge plus a
minimal schema validator for the structured-state layer.

**What the measurement found.** On the only real corpus available, the local extractor
produced **zero** facts from 568 turns while a cloud model produced **74** from the same
input — so the earlier "typing is inconsistent" finding had been measuring a model that
was barely functioning, not a design defect. With a capable extractor the typing bars
pass: 62/62 linkage, 0% of claims on a multiply-typed subject, repeat-subject
consistency 1.00 against 0.00 before.

## What's in v1.69.0

v1.68.0 shipped ontology-declared placement. A two-user run on a real corpus then
showed it could not work: **every typed fact was being lost before it reached the
graph.** This release is what that measurement found, and it is worth reading in
that order — none of it was predicted from the code.

**A subject keeps the type it is already filed under.** A subject node's natural key
is `type + ":" + slug`, so the type is part of the identity. Each extraction call
picked a type fresh, with no knowledge of how that subject was typed before, and
nothing reconciled them. On a 142-claim benchmark store one person existed **five
times** — `event`, `location`, `object`, `organization`, `person` — carrying 89 claims
across five nodes, and **94% of all claims hung off a multiply-typed subject**. So
*"what else do we know about her"*, the question the entity tier exists to answer,
could reach at most a fifth of what was stored.

The type a subject already has now wins over the current call's guess, for both the
key and the field. **First write wins**, and that is the design rather than a
tie-break: it is the only *stable* choice, because any rule that can change a
subject's type later re-partitions every fact already filed under the old one. A
wrong-but-stable type keeps one subject's facts together, which is what matters.
Existing multiply-typed nodes are **not** migrated — this prevents new splits.

**An undeclared type becomes a candidate instead of a lost write.** The extractor
invents kinds the tenant has not declared — `experience`, on the corpus measured — and
`upsert_chunk` refused those writes, so the fact landed in key/value with no graph
presence at all. The gate's own message is that an undeclared-type node *"becomes a
node nobody can find"*; refusing produces **no** node, which loses the subject
entirely.

Such a type is now filed as an **inert ontology candidate** and the subject written
under `object`. The candidate changes nothing for any run until an operator accepts
it, so an invented kind becomes something a person can adopt rather than a silent
loss. A statement class misused as a kind — `preference:user` reads as *"the entity of
type preference named user"* — also falls back rather than being dropped, but is not
proposed: those names are already declared and are simply being misused. The key moves
with the type, because the type is identity; and the retry is matched against the
gate's own wording, so a store fault is never mistaken for an undeclared type.

**A failing consolidation pass says why.** The consolidator counted its graph failures
and discarded the reason — and there is no second place to look, because it is an
`internal: true` agent whose runs are kept out of the run and history surfaces and
whose transcript cannot be read back. The pass report is the only thing it ever gets to
say. It now carries the first error text, and counts a failed subject-type **lookup**
separately from a failed chunk **write**: one counter for both is what made a live
signal uninterpretable.

**Identity documents on every user-creation path.** v1.68.0 provisioned the user-root
and tenant-root documents when a principal was established — for one of the three paths
that establish one. The Web UI drives the other two, so users created there got no
profile: no Identity section, no way to declare their own names, and placement cannot
tell a fact about that person from a fact about a colleague without them.

### Known state, stated plainly

**RFC CQ's gate is still not satisfied.** On the only real corpus available, 1,136
turns of input across two users produced **four facts**, with 35–39 malformed extractor
replies dropped per pass. Placement cannot matter at that rate, and a
typing-consistency number computed over a handful of subjects is not a result — that
mistake has already been made once in this line and corrected. The yield question is
what gates a real answer, and it is open.

The adapters are unchanged and remain at **1.67.0**.

## What's in v1.68.2

*A failing consolidation pass says why.*

One fix. The memory consolidator counted its entity-graph failures and threw
away the reason:

    entities 0 fact(s) across 0 subject(s), 2 graph write(s) failed

That is unactionable, and there is no second place to look. The consolidator
is an `internal: true` agent, so its runs are deliberately kept out of the run
and history surfaces and its transcript cannot be read back — confirmed with a
404 from both /v1/runs/{id} and its events. The pass report is the only thing
it ever gets to say, so a swallowed message is a message lost for good. A live
two-user run on a real corpus stalled on exactly this: every entity write
failed and the cause was unrecoverable afterwards.

Three changes, none of which alter what gets stored:

  - The FIRST error text is kept and reported, truncated. First only, because a
    per-failure list would put unbounded model-adjacent text in the report, and
    the first one is what a person acts on.

  - A failed subject-type LOOKUP now has its own counter and message. It is a
    different failure from a failed chunk WRITE — a bad lookup means the type
    may drift, a bad write means the fact is missing from the graph entirely.
    Sharing one counter is what made the live signal uninterpretable as soon as
    v1.68.1's canonicalType began contributing to it.

  - A reply that PARSES but carries no id is now counted. It previously
    returned empty and incremented nothing, so a pass could lose every entity
    write and still report a clean sheet.

BUILD NOTE

Tagged as a patch but built with force_full, because the fix is in the runtime
and a patch tag otherwise builds only the loomcycle-browser image.

The adapters are unchanged and remain at 1.67.0, so no python-v tag
accompanies this.

## What's in v1.68.1

*The entity graph keeps one subject as one subject.*

Two runtime fixes on the v1.68.0 memory line, both found by measuring a real
store rather than by reading the code.

A SUBJECT KEEPS THE TYPE IT IS ALREADY FILED UNDER

A subject node's natural key is `type + ":" + slug`, so the type is part of
the identity. Each extraction call saw one transcript and picked a type
fresh, with no knowledge of how that subject was typed before, and nothing
reconciled them. Measured on a benchmark store of 142 claims:

    caroline -> event, location, object, organization, person  (5 nodes, 89 claims)
    melanie  -> event, object, person                          (3 nodes, 43 claims)
    nia      -> event, object                                  (2 nodes,  4 claims)

Every subject mentioned more than once was typed inconsistently, and 94% of
all claims hung off a multiply-typed subject. Facts about one person were
scattered across five graph nodes, so "what else do we know about her" — the
question the entity tier exists to answer — could reach at most a fifth of
what was stored. It also meant ontology-declared placement would refuse 94%
of that corpus, since it correctly declines an inconsistently typed subject.

The type a subject is already filed under now wins over the current call's
guess, for both the natural key and the type field. FIRST WRITE WINS, and
that is the design rather than a tie-break: the first type is the only STABLE
choice, because any rule that can change a subject's type later
re-partitions every fact already filed under the old one.

Existing multiply-typed nodes are NOT migrated — this prevents new splits.

The extraction prompt also now states whose memory it is filling, as a rule
("a fact about THEM takes the subject `user`") rather than by handing the
model a subject id. A transcript between two other people has no fact about
the owner, so a benchmark corpus is unaffected.

IDENTITY DOCUMENTS ARE PROVISIONED ON EVERY USER-CREATION PATH

v1.68.0 provisioned the user-root and tenant-root documents when a principal
was established — for one of the three paths that establish one. The hook sat
on the OperatorTokenDef substrate tool; `POST /v1/_users` and
`POST /v1/_users/{subject}/tokens` write their rows directly and share no
code with it. The Web UI drives those two, so a user created there got no
profile, hence no Identity section, hence no way to declare their own names —
and placement cannot tell a fact about that person from a fact about a
colleague without them. The feature was inert for the people it was built
for.

Both paths are now hooked. A user created before this ships stays without a
profile until a token is minted for it, or the document is created directly.

ALSO

A long-horizon context-retention benchmark harness (bench only, no runtime
surface).

BUILD NOTE

Tagged as a patch but built with force_full, because the fixes are in the
runtime and a patch tag otherwise builds only the loomcycle-browser image.

The adapters are unchanged and remain at 1.67.0, so no python-v tag
accompanies this.

## What's in v1.68.0

**Organisation knowledge has somewhere to live (RFC CQ).** Some of what a
consolidator learns is not about the user — *"the checkout-api service requires two
approvals"* is true for everyone in the tenant. Until now every fact went into one
user's scope, so a colleague either re-learned it from their own conversations or
never learned it at all, and there was no surface anywhere that would tell an
operator this was happening.

**Placement is operator config, not per-fact inference.** An entity type in the
tenant ontology may declare which memory scope facts about that kind of thing belong
in:

```
## service
- `@memory_scope` tenant
- `name` — what people call it
```

A fact reaches its scope through the subject entity it is already linked to. The
alternative — asking a model per fact — would put a judgement in front of every
write, thousands of independent chances to put a private sentence in front of the
whole tenant, and it would land on the extractor, whose own prompt is documented as
*"a mitigation, not a guarantee"*. A handful of declarations, authored once and
versioned in a document, replaces all of that. It also costs nothing at write time:
the type is already assigned.

**Every uncertainty declines to move the fact**, and that asymmetry is the design
rather than caution. Declining costs exactly what the system already costs — the fact
stays in one user's scope. Moving one wrongly is not recoverable. So a placement is
refused for an undeclared or unknown type, a draft ontology, no ontology at all, a
subject that names the run's own user, a subject the store types inconsistently, an
isolated member, and a scope the writing agent has not been granted — each with a
reason an operator can act on.

**Both halves of a fact move together, or neither does.** A fact is stored twice: the
key/value row semantic recall searches, and a chunk mirror the graph walks. Split
those across scopes and `recall` finds the fact in one place while `graph_recall`
finds it in another, which is worse than never moving it. So the decision is made
once per batch, before either write, by the writer that owns both — a new read-only
`Memory op=placement`. A tenant placement consequently needs the tenant grant on
*both* `memory_scopes` and `sql_scopes`, because the mirror is a Document write.

**A user can say who they are.** The per-user profile document gained an Identity
section — `@name` and `@alias` bullets — because nothing else can tell a fact about
the user under their own name from a fact about a colleague: *"Ada prefers Go"* and
*"Maria owns the release process"* are the same shape. With names declared, the
user's own facts stay theirs whatever their type says. Undeclared, that gap remains
exactly as it was, which the test suite asserts out loud rather than assuming away.

**The identity documents now exist when a principal does**, at token mint and at boot
for config-declared principals, instead of appearing on the first run that happened
to reference them. A template that arrives after the moment it was needed is not a
template. `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS=0` restores the old lazy-only
behaviour.

**Reads are unchanged, and now say so.** A memory read touches exactly one scope —
it always did, but the `scope` description described *who* could reach the tenant
keyspace and never that a separate call is required to read it, so an agent asked
once, got nothing, and concluded the organisation knew nothing. Both `Memory` and
`Document` now state the invariant and the remedy: one scope per call, two calls to
consult two, merge by score.

**Inert until an operator turns it on.** Nothing is declared out of the box, the
consolidator's grants are untouched, and a deployment that edits nothing behaves
exactly as it did in v1.67.0.

### Also in this release

**A sqlite upgrade blocker.** Any deployment whose `memory` table predated RFC CL
(v1.65.0) could not start on v1.66+: `migrate` created three partial indexes ahead of
the `ALTER`s adding the columns they name, and on an existing table
`CREATE TABLE IF NOT EXISTS` is a no-op, so the index failed on a missing column and
the store never opened. A fresh database gets those columns from `CREATE TABLE`, which
is why every test passed — they all start from an empty file. Both halves are now
tested: a legacy-shaped fixture, and a guard that reads the two statement lists and
fails for any table.

**Wikidata benchmark harnesses** for knowledge updates, ontology typing and
cross-lingual recall, plus a bulk fact-corpus builder and importer.

### Known state, stated plainly

RFC CQ's own gate — whether a real store's entity typing is consistent enough to
carry a scope decision — **has not been run on a real multi-user store**, and its
preliminary reading on a small development scope *fails*: linkage was complete, but
two of five subjects carried two types, and one of them would place a personal fact
in the shared plane. That is a data-quality problem rather than a design one, and it
is why placement declines an inconsistently typed subject instead of guessing. Until
that measurement exists, treat a `@memory_scope` declaration as something to try on a
store whose typing you have looked at.

The adapters are unchanged in this release and remain at **1.67.0**.

## What's in v1.67.0

**Per-user credential self-service (RFC CN).** A logged-in user can now store its
OWN API tokens — a personal Slack/Telegram bot token, a per-user webhook secret —
without handing the secret to a tenant operator. The *consumption* side already
bound per-user credentials (`$cred:<name>` resolves **agent > user > tenant**, per
run); the missing half was letting a user *put a token in*. Now every transport does.

Before this, `POST /v1/_credentialdef` was `substrate:tenant`-gated: an isolated
`substrate:user` user was refused at the tenant-member isolation floor, and the Web
UI's only credential control lived in the operator Settings hub. The rule now: a user
may manage **only `scope=user`** credentials keyed on its own subject; an omitted
scope defaults to `user`; `scope=tenant`/`agent` still requires `substrate:tenant`.
One shared check (`credential.ConstrainToUserScope`) enforces it identically on every
surface, so the transports cannot drift.

### Across every transport

- **HTTP** (#1093) — `POST /v1/_credentialdef` admits an isolated user (additively —
  tenant/admin authoring and the RFC CB member path are unchanged), confined to
  `scope=user` by the handler.
- **MCP** (#1095) — an isolated `substrate:user` session may open `/v1/_mcp` and call
  **only** the `credentialdef` meta-tool (the `loomcycle mcp --upstream` thin client).
- **Web UI** (#1096) — a standalone **"My Credentials"** page, visible to every login
  (including a delegated user with no Settings gear); the operator Settings →
  Credentials tab (tenant authoring) is unchanged.
- **gRPC + adapters** (#1097) — a new `CredentialDef` RPC, plus
  `createCredential`/`listCredentials`/`deleteCredential` on `@loomcycle/client` and
  `credential_def()` on the Python client. Both adapters ship at **1.67.0**.

The worked example — each user receives its own GitHub webhook and publishes to *its*
Slack/Telegram channel with *its* bot token — needs no per-user def authoring: an
operator authors the flow once referencing `$cred:telegram`, and each user self-serves
only its token, which wins per run over the tenant fallback.

### Also

- **fix(history):** `op=recap` now writes a **short** chat-list summary (at most two
  sentences, under 256 chars) instead of reusing the compaction prompt — which on a
  long chat returned several paragraphs that no list surface could rely on (#1092).
- **bench:** the LoCoMo harness gains an **answer axis** (`-mode answer` — whether an
  agent can *answer* from what it retrieved, not just whether the right rows come
  back) plus the RFC CL/CM measurement instrumentation (#1094).
- Refreshed the default local-provider model aliases in the `local` preset.

## What's in v1.66.0

Two feature lines land together: **RFC CK** makes local inference a first-class
bundle and lets an operator reload a running config without dropping in-flight runs,
and **RFC CL** gives a memory row a sense of *time* — when a thing was said and when
it was true — so a question can ask about a moment, not just a topic. Minor rather
than a patch: both add runtime primitives the binary must carry, and the Memory tool
plus the adapters gain surface.

### RFC CK — local providers in bundle YAML, and on-the-fly config reload

**Local inference is now configured in YAML, not scattered across ENV.** Dedicated
`vllm` and `llamacpp` drivers (OpenAI-compatible, on the DeepSeek delegate pattern)
join `ollama-local`, and the whole local-provider matrix — `base_url`, the advertised
context window, header/idle timeouts, enablement — is settable in a bundle's
`providers:` block, with `loomcycle.yaml` and ENV still overriding per the existing
layer order (#1079).

**`POST /v1/_config/reload` reloads a changed config in place** — the retune that used
to mean a restart (a bigger `num_ctx`, a new endpoint, a longer timeout) now takes
effect without dropping in-flight runs. It re-assembles the same layered stack the
server booted from, **validates the candidate before applying** (a typo is rejected
`422` and the running config keeps serving), applies the sections it can apply live,
and reports the rest under `restart_required`; `?dry_run=1` returns the section diff
without applying.

- The endpoint + in-place resolver/provider rebuild (#1080), a config `Holder` that
  makes `user_tiers` / `agents` / `defaults` reload live (#1081), and subsystem
  reloaders for concurrency caps, `scheduled_runs`, and channels (#1086).
- **Section-per-file config**: a `loomcycle.yaml` base auto-layers its
  `loomcycle.*.yaml` siblings (deep-merged, lexical), so a large config splits by
  section — `loomcycle.providers.yaml`, `loomcycle.memory.yaml`, … (#1083, #1084).
- **Re-glob on reload**: adding or removing a config or section file after boot is
  picked up on the next reload — no restart (#1088).
- Deliberately restart-required, with reasons reported: the memory embedder (a
  model/dimension change invalidates every stored vector), skills, the listen
  address, and the store DSN.

### RFC CL — a memory row learns *when*

A key/value memory row used to carry one time — `created_at`, when loomcycle stored
it, which on a bulk import is one clustered instant for the whole corpus and answers
none of the questions people actually ask. It now carries three:

- **`observed_at`** — when the thing was *said or written* (#1085). A new **`when`**
  predicate on `search` / `recall` narrows by it, and `set` takes it to date a row.
  Caller-supplied and never inferred — a guessed date is worse than none, because it
  silently filters the right row out of a window it belongs in. Soft by default:
  undated rows survive the window for the ranker to demote (`missing: prefer`).
- **`valid_at` / `invalid_at`** — when the thing was *true* (#1087). An **`as_of`**
  predicate answers "what was true on the 3rd" exactly, where the observed window
  could only approximate it. `as_of` is always a hard filter — a row valid over a
  different interval is not a weaker answer, it is a wrong one. Half-open
  `[valid_at, invalid_at)` with NULL meaning still true.
- The consolidator now **dates the durable facts it already keeps** (#1089), so
  `as_of` can answer "what medication was I on in April". Scoped honestly to semantic
  (durable-state) memory — it does not attempt episodic "what did I do that day"
  coverage, which belongs in its own RFC.

The adapters gain the memory-search time surface: **`@loomcycle/client` 1.66.0** and
the **Python `loomcycle` 1.66.0**.

### Also

- **fix(store):** a Postgres def-INSERT placeholder-count regression (landed with the
  phase-2a memory change) failed every `agent_def` / `skill_def` / …`_def` create on
  the Postgres store; the def INSERTs are restored to their correct column count
  (#1090). The sqlite path was unaffected.

## What's in v1.65.0

**`recall` told the model every row was a fact, and carried no kind.** Minor rather
than a patch for the same two reasons as v1.64.0: the fix is in the runtime binary,
which a `vX.Y.Z` patch tag does not build, and the shape of `recall`'s result changes
— the array is renamed and each row gains a field.

### It promised a `kind` and never sent one (#1077)

The Memory tool's input schema has always said, of `sources`, that *"each result
carries a matching kind"*. For `search` that is true. For `recall` it was not: the
projection rendered `{id, memory, score}` and nothing else, so a caller could not
tell a consolidator-distilled fact from a remark an agent had jotted down from
document prose. The class was already being computed — the source selector filters on
it — so nothing was missing but carrying it out to the caller.

This is the same defect class as v1.64.0's, which added `kind` to `search` and left
`recall` alone.

An **empty kind stays empty**. A remote memory layer handing back opaque server-side
ids cannot classify its own rows, and defaulting those to `"fact"` would recreate the
very thing being fixed, so the field is omitted instead.

### And the array called them all facts anyway

Recall's default admits notes as well as facts, and on a corpus of raw ingested turns
EVERY row is a note — so `facts[]` asserted, of every result, a status most of them
did not have.

Not cosmetic. The LoCoMo answer axis run twice over one corpus (1,535 questions, same
judge, same answerer model, 1,524 graded by both), changing only which op the answerer
called:

| slice | `op=search` | `op=recall` | delta |
|---|---|---|---|
| **overall** | **0.6906** | **0.6692** | **-0.0214** |
| single-hop | 0.7873 | 0.7873 | 0.0000 |
| multi-hop | 0.5344 | 0.5283 | -0.0060 |
| temporal | 0.6589 | 0.5727 | **-0.0862** |
| open-domain | 0.3571 | 0.3258 | -0.0313 |

**Retrieval was identical between the two.** Probed in-band with the same query, both
ops returned the same ten keys, in the same order, with the same scores — expected
here, because with no consolidation pass every row is a note, so recall's facts+notes
default and search's unfiltered scan select the same set (`facts_written=0` in both
runs). The entire gap is the projection the model reads.

It concentrates in temporal questions, and the failure has one shape: of 43 temporal
answers that went correct to wrong, 29 reported the timestamp of the UTTERANCE as the
date of the EVENT.

```
"When did Caroline go to the LGBTQ support group?"    gold: 7 May 2023
  through search:  "7 May 2023 (she went 'yesterday' on 8 May)"   correct
  through recall:  "8 May 2023"                                   wrong
```

Reading a dated remark as a standing fact is what "facts" invites. The other 14 were
abstentions (NOT_FOUND 0.134 → 0.167).

So the array is now `memories`, each row carries its `kind`, and the op's description
says what a remembered remark IS: something recorded, not something established, often
stamped with the time it was SAID rather than the time it happened.

### Upgrade note

`recall` returns `{"memories": [{id, memory, score, kind}]}` where it previously
returned `{"facts": [{id, memory, score}]}`. Anything reading `.facts` off a recall
result needs `.memories`.

There is deliberately **no compatibility alias on the wire**: this is model-visible
tool output, and emitting both names would leave the misleading one in front of the
model, which is the whole point of the change. The one in-tree consumer — the code-js
consolidator's `recall()` — reads `memories || facts`, because a code-js body replays
against tool results recorded earlier in the SAME run, so a pass straddling the
upgrade would otherwise read undefined, recall nothing, and write a duplicate instead
of merging.

`Document op=list_facts` is untouched and still returns `facts` — those are facts.

### Measured after the fact — it held

The figures above are from the OLD projection; this section originally said the
recovery was a hypothesis, not a result. It has since been run. Same corpus, same
answerer def, same judge, same model — only the projection differs:

| slice | `op=search` | recall (old) | **recall (v1.65.0)** | vs old | vs search |
|---|---|---|---|---|---|
| **overall** | 0.6906 | 0.6692 | **0.6915** | **+0.0224** | +0.0009 |
| single-hop | 0.7873 | 0.7873 | 0.7860 | -0.0013 | -0.0013 |
| multi-hop | 0.5344 | 0.5283 | 0.5356 | +0.0073 | +0.0012 |
| **temporal** | 0.6589 | 0.5727 | **0.6701** | **+0.0974** | +0.0112 |
| open-domain | 0.3571 | 0.3258 | 0.3678 | +0.0420 | +0.0107 |

1,535 questions, 1,454 graded. `recall` went from 2.2pp behind `search` to level with
it, and every category is now within about one question of `search` — the gap is
closed, not narrowed.

The recovery landed where the diagnosis said it would. Temporal gained 9.7pp;
single-hop, which does not depend on resolving a date against an utterance time,
stayed flat at -0.13pp. A uniform lift across categories would have been WEAKER
evidence — it would have suggested some general effect rather than the specific
mechanism claimed. Of the 43 temporal questions that `search` answered correctly and
the old projection got wrong, 30 are now correct, 12 still wrong, 1 unparsed.
Abstentions fell 0.167 to 0.158.

### What the remaining 12 are, and they are not this bug

Investigated, because "mostly fixed" is not a finding. They split in two, neither of
which the projection can reach:

**Five are the deictic step failing at the last inch.** The model now demonstrably
UNDERSTANDS the offset and still answers the wrong date — one reply reads *"8 May 2023
(she went 'yesterday,' stated 8 May)"*, which states the correct reasoning and then
emits the speaking date anyway. Nothing about the row's framing is misleading it any
more; it is an instruction-following failure at the final-answer step, and it belongs
to whatever prompt is asking the question.

**Seven need a time PREDICATE, which this memory plane does not have.** They ask
things like *"which city was Calvin at on October 3, 2023"* or *"what was Dave doing
in the first weekend of October 2023"*. Cosine similarity over prose cannot answer
that: the embedding of a date-constrained question does not retrieve the turn that
happens to carry that timestamp, because dates are not semantically encoded. Five
abstained and two answered confidently wrong, which is the worse failure.

That splits cleanly in the aggregate, across all 294 graded temporal questions and not
just the 12:

| temporal question shape | n | accuracy | abstention |
|---|---|---|---|
| carries an absolute date / window constraint | 31 | 0.5484 | 0.323 |
| topic-shaped | 263 | 0.6844 | 0.171 |

So a date-constrained question is roughly 14pp harder and abstains about twice as
often. The machinery for it already exists elsewhere in the runtime — the bi-temporal
entity sidecar's `valid_at` / `invalid_at` and `graph_recall`'s `as_of` predicate — but
the k/v memory plane's `recall` and `search` expose no date filter at all. Closing
that is a feature, not a fix, and wants its own RFC.

## What's in v1.64.0

**`recall` could not see an agent's own notes.** Minor rather than a patch for two
reasons: the fix is in the runtime binary, which a `vX.Y.Z` patch tag does not
build, and a bare `recall` now returns notes alongside facts — a visible behaviour
change for anything reading it.

### Two defects, either of which alone empties the result (#1075)

`Memory op=recall` returned NOTHING on a scope holding 419 embedded rows, while
`op=search` returned three hits for the same query in the same partition:

```
search sources=[notes]        → 3 results (cosine 0.546, 0.539…)
recall sources=[notes]        → 0
recall sources=[facts,notes]  → 0
recall (no sources)           → 0
```

**The in-band parser silently dropped `notes`.** `parseSources` had cases for
facts and documents and none for notes — while the op's OWN input-schema enum
advertises all three. Unknown values are dropped rather than rejected by design
(rejecting would break an older runtime against a newer value name, which is the
right call), so an explicit `sources:["notes"]` became NO selector. One cause, two
opposite symptoms: `search` widened to everything and looked correct, `recall`
fell through to its default and returned nothing. The HTTP parser
(`parseMemorySources`) had always handled notes, so the same selector meant
different things depending on which surface a caller used.

**Recall's default excluded notes, contradicting its own schema.**
`inprocess.Recall` defaulted to facts alone; the schema promises "facts+notes". A
row written with `set` — or with an off-run PUT — carries no provenance, so
`ClassifyMemoryRow` calls it a NOTE, and the default hid every one of them. The
facts/notes split landed after that default was written and the line was never
revisited: before the split, "facts" WAS the whole of an agent's own memory.
Documents stay excluded, which is the separate and still-correct reason the
default exists at all (a horizontal rule outranking the fact holding an answer).

The `SourceFacts` doc comment still described facts as including notes; reading it
that way is what made the default look intentional. Corrected.

### How it surfaced, and what it was costing

The LoCoMo answer axis. An answerer whose prompt says `op=recall` scored **0.0000
with a 94% abstention rate** over 419 embedded conversation turns — it was finding
nothing, not answering wrongly. Pointed at `op=search`, which by accident of the
first defect applied no filter at all, the same store, same embedder and same
questions scored **0.7353 with zero abstentions**.

So the practical cost was: any agent following the documented advice to read its
own memory with `recall` saw only what a consolidator had distilled, and nothing
it had written itself.

### The fixture could not have caught it, which is also fixed

The inprocess test double honoured only `filter.KeyPrefix`. It ignored
`filter.Provenance` and `filter.ExcludeKeyPrefix` and never populated
`MemorySearchEntry.Origin`, so NO test in that package could distinguish a fact
from a note or exclude a document — source filtering was structurally untestable
there. The double now records each row's origin on the way in (the real stores
keep it in a column `MemoryEntry` does not expose, so a double reading rows back
cannot recover it), applies both filter dimensions, and carries `Origin` through.

That mattered immediately: the fail-before for the second defect initially PASSED,
because the assertion could not observe the thing it named. Making the double
faithful is what turned it into a regression test.

### Adapters

None. The fix is runtime-internal, so `@loomcycle/client` and the Python package
stay at 1.61.0 and `@loomcycle/library` at 0.3.0.

## What's in v1.63.0

**A curator that never ran, and the two agent fields you could not set without
curl.** Minor: the changes are in the runtime binary and the embedded Web UI,
neither of which a `vX.Y.Z` patch tag builds.

### memory/ontologist named its own tool in lowercase and never ran (#1072)

It failed its FIRST tool call on every run, then spent the rest of its budget
reasoning about which tools it had. A live pass: `tool not found: document`,
followed by 1,937 output tokens concluding — wrongly — that the Document tool was
missing from its environment, and inventing two tools
(`generate_tool_suggestion`, `search`) that exist nowhere in loomcycle.

The def was fine. It grants `tools: [Document]`. The PROMPT told the model to call
`document` — lowercase, three times — and tool dispatch is an exact-match map
lookup (`tools.Dispatcher.Execute` does `d.tools[name]`), so the call never
reached the tool and never could.

Nothing caught it because the two halves are checked by different things and
neither checks the pair: the ACL is validated at config load, the prose is not
validated at all. The agent looked correctly configured in every listing, and the
only symptom was a curator producing confident nonsense.

`TestBundlePrompts_NameToolsExactly` now scans every bundle agent's system prompt
for the backticked `` `name` op=… `` form the bundles use to teach a tool call and
asserts the name is one that agent is granted. Scoped to that form rather than to
every mention of a word, because "ordinary document chunks" is legitimate prose in
the same paragraph — and it fails when the pattern matches nothing, so it cannot
pass by examining zero instructions.

### The agent modal can set max_context_tokens and internal (#1073)

Both round-trip the AgentDef create/fork overlay and neither was reachable from
the Library modal, so setting either meant hand-rolling an API call.

`max_context_tokens` sits one row from `max_tokens` and they are trivially
confusable with different consequences — one truncates the reply, the other
truncates the prompt — so both inputs carry a hint. The hint leads with what
decides whether you want it: on a local model this becomes THAT agent's own
`num_ctx`, so a smaller window is a cheaper and faster call; on a cloud model it
can only lower the effective window, never raise it.

`internal` is a checkbox that says ONE-WAY, because `applyOverlay` does
`if ov.Internal { d.Internal = true }`. A plain checkbox would let an operator
untick it, fork, and get an agent that is still internal with nothing saying so.
The overlay emits the key only when true, mirroring that merge instead of sending
a `false` the server ignores.

Also fixed the text that hid both: the overlay's own schema `description` listed
neither, so an MCP caller reading the tool schema could not discover them either.
That description still documents only 24 of the overlay's 46 fields — the 22 it
omits include `sampling`, `compaction`, `volumes`, `sql_scopes`,
`memory_consolidation` and the four `*_def_scopes` gates. Left for its own change,
because a coverage test would fail on twenty fields unrelated to this one.

### Packages

`@loomcycle/library` 0.3.0 (the modal change), published from its own
`library-v0.3.0` tag. The Web UI embedded in the binary compiles that source
directly, so the runtime carries the change either way. `@loomcycle/client` and
the Python adapter are unchanged at 1.61.0.

## What's in v1.62.0

**Three ways a consolidation pass wasted a deployment's time, all found by running
one.** Minor: the changes are in the runtime binary and the embedded bundle, neither
of which a `vX.Y.Z` patch tag builds. No adapter or wire change.

The occasion was a LoCoMo memory benchmark on a local-model deployment. Every item
below is measured from that run's event log rather than reasoned about.

### A killed pass no longer strands its lease (#1069)

The consolidation pass releases its lease in a `finally`, which covers every path
its own code can take — and none of the paths where the **run** is killed out from
under it. The owner of a lease is the run id and `cursor_release` is
ownership-scoped, so a dead run can never hand its lease back and nothing else is
permitted to. The target stayed leased until the TTL elapsed.

What that looked like: a pass took the lease at 14:21:46 and spawned an extractor
child that never returned — neither run emitted a `done` event in the following two
hours. The parent hung on that one call, exhausted its 25-minute
`run_timeout_seconds`, and was killed before reaching the `finally`; its 30-minute
lease then sat until 14:51:46. The harness polls every three minutes, so **ten
consecutive passes** bailed with "target busy, nothing read, nothing written" across
a window where nothing was running at all.

Note the shape: `run_timeout_seconds` is deliberately *below* `lease_ttl_ms` so a
live pass can never be stolen from — which means a killed pass was *guaranteed* to
leave a stranded lease, every time.

`MemoryCursorReleaseByOwner(owner)` now frees a run's lease from
`finishRunWithCancel`, the terminal path for every run, as a deferred best-effort
hook. Owner-only with no tenant argument, because a run id is globally unique and a
dead run cannot say which target it had leased. An empty owner is a no-op, since
`''` is what an *unleased* row stores in `leased_by`. The TTL keeps its job as the
backstop for a process that dies without reaching any cleanup; what is gone is the
case where the runtime *knew* the run was over and let the lease sit anyway.

### The queue no longer takes a whole pass per batch, and one bad item stops blocking the rest (#1070)

`consolidatePending` took exactly one extractor call's worth of queued items and
deferred the remainder, so throughput was **one batch per pass** however long the
queue was — a 419-turn conversation measured ten-to-sixteen passes and about an
hour. The items are independent; nothing about correctness required stopping after
one call, only the absence of a loop. Now bounded by `max_pending_calls` (4),
because a pass still has to finish inside `run_timeout_seconds`.

The second half was a **livelock**, not slowness. An empty extraction left the whole
batch queued — right in itself, because an ack is the one irreversible step here and
an empty reply cannot distinguish "these turns hold nothing durable" from "the model
glitched". But it also meant a batch the extractor keeps answering empty for was
re-drained every pass, spent a call, acked nothing, and blocked everything behind it
forever. Observed live as ten consecutive replies of **two output tokens** with the
queue head never moving, and it is why a pre-ingest drain spent ~70 minutes without
reaching the run's own data.

The fix does **not** ack on empty. An empty multi-item batch narrows to its head (a
multi-item batch does not say which item the model could not read), and a single
item that still yields nothing is **stepped over**: left queued for a later pass
while this pass continues with what is behind it. The blockage is gone, no input is
traded for throughput, and a permanently unreadable item settles at one call per
pass instead of every call forever. It is reported, because a silent step-over looks
exactly like a pass that spent its budget and moved nothing.

Worth recording: the first version of this *did* ack a single item that came back
empty, reasoning that the model had examined it alone — the same evidence the chat
path accepts when it lets an empty chat move the watermark.
`TestConsolidator_EmptyBatchReplyLeavesTheQueuedItemsQueued` refused it, correctly:
a transient extractor glitch would have dropped those turns permanently. Step-over
solves the blocking without that trade, and the test passes unmodified.

### The consolidator's children ask for the window they actually need (#1070)

They ran at whatever window the `ollama-local` registration was pinned to, because
until v1.61.0 that was the only knob. RFC CJ made it per-agent and the Ollama driver
prefers `Request.MaxContextTokens` over the construction-time `num_ctx`, so each
child can now size its own — and on a local deployment the KV cache is allocated per
request, so a smaller window is a cheaper and faster call.

Sized from observed input tokens: **`memory/extractor` 16384** (a real extraction
measured 3,826 input tokens; its prompt budget is `max_part_chars`, 12,000 chars ≈
3–4k tokens, plus the injected ontology), **`memory/judge` and
`memory/conflict-judge` 8192** (715–731 input tokens across a live batch of eight),
**`memory/ontologist` 32768** (it surveys the store rather than reading one supplied
text; a live pass ran 86k across seventeen calls).

The extractor's *window* and its *prompt budget* bound different failures, so raising
`max_part_chars` to fill the window would be wrong — 12,000 is measured against the
model, where a 21,635-char prompt came back empty while 15,684 and 15,599 extracted
cleanly.

### Not changed, deliberately

The `lease_ttl_ms` / `run_timeout_seconds` ratio (30 and 25 minutes against a
four-minute pass) still looks generous. #1069 removes the wedge that made it hurt,
and #1070 makes a pass *longer* — up to four extractor calls — so tightening the
budget now would cause more kills rather than fewer. Worth revisiting once the new
pass duration has been measured.

### Adapters

None. Both changes are runtime-internal, so `@loomcycle/client` and the Python
package stay at 1.61.0.

## What's in v1.61.0

**Size each agent's context window — per agent and per run.** Minor rather than a
patch: the changes land in the runtime binary and the adapters, neither of which a
`vX.Y.Z` patch tag builds. It completes RFC CJ across both phases, adds a
memory-retrieval benchmark harness, and fixes a load-dependent test flake.

### A context window per agent (#1065)

One Ollama host served every local agent the same context window — the global
`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` (or a per-provider `options.num_ctx`), or, unset,
Ollama's silent 4096-token floor. A fetch-heavy `researcher` that needed 128K and a
`chat` agent that wanted 16K could not coexist on one registration without standing
up a second provider.

A per-agent `max_context_tokens` now sizes the window for **that agent only**. On a
local (Ollama) model it is sent as `options.num_ctx`, winning over both the env knob
and any per-provider value; the driver reports the source as `(request)`. On a cloud
model — where the window is fixed by the model — it instead caps the agent's
*effective* window (the compaction budget the context gauge and `autocompact_at_pct`
read), clamped to the model's maximum, so it can only lower, never enlarge. It is
distinct from `max_tokens` (the output cap), content-identifying (a fork that changes
it mints a new `content_sha256`), and flows down the spawn tree. `0`/unset is
byte-identical to before.

### Per-run override, op=self visibility, adapter parity (#1067)

The same knob became a per-**run** override, mirroring the existing per-run
`sampling`/`compaction` overrides exactly: a value on `POST /v1/runs` (or a
continuation), on the gRPC `RunRequest`/`ContinueRequest`, on the MCP `spawn_run` /
`spawn_runs` and the HTTP `/v1/runs:batch` fan-out, and on both adapters. A per-run
value `> 0` wins over the agent's own; `0`/absent inherits it (which itself falls
through to the provider/driver default). Because a window is never meaningfully `0`,
absence and zero coincide — so it is a plain scalar (a new `config.MergeMaxContextTokens`
helper beside `MergeSampling`/`MergeCompaction`) and a plain `int32` on the wire, not
a proto3 `optional`. Sub-agent spawns and resumed runs deliberately use the agent-def
value only — a per-run override is neither inherited by children nor snapshotted, the
same posture per-run sampling takes at those two sites.

An agent can now read its resolved cap for its own run via `Context op=self`
(`max_context_tokens`), reported even before the first turn; the *effective* window
after the first turn continues to appear there as `context.max_tokens`.

One pre-existing gap fell out of this: the MCP streaming-spawn path was silently
dropping per-run `sampling` **and** `compaction` on the way to the run input. It now
carries all three overrides, like the blocking path.

### A memory-retrieval benchmark harness (#1064)

`bench/cmd/locomo` adds a convert / ingest / search / all / purge harness over the
LoCoMo long-conversation QA set, so memory-retrieval quality (recall@k) can be
measured against a fixed corpus rather than by feel — the `qa.evidence` dialogue ids
give free retrieval ground truth. It needs pgvector (the SQLite tier has no vectors),
and the corpus is CC BY-NC, so it is never vendored.

### De-flaked the deferred-channel store contract (#1066)

Two deferred-channel store-contract subtests asserted a message was hidden before its
`visible_at` using a 150 ms window; on a loaded `-race` CI runner the window elapsed
before the assertion ran, producing a spurious "visible too early" failure. The
windows are widened to 2 s. Test-only.

### Adapters

Both bump to **1.61.0**. `@loomcycle/client` gains `maxContextTokens` on the run and
continue options (serialized as `max_context_tokens`); the Python package gains a
`max_context_tokens` argument mapped onto the gRPC request. Additive — existing
callers are unchanged.

## What's in v1.60.0

**A fact that contradicts a stored one is now noticed.** Minor rather than a patch
because the change lives in the runtime binary and the embedded bundle, which a
`vX.Y.Z` patch tag does not build. One feature, and it deliberately writes nothing.

### Conflict detection, report-only (#1062)

A fact that contradicted one already in the store did not retire it. Both persisted,
both came back from recall, and nothing marked either as suspect — the failure a
neutral survey of 2026 memory systems calls *hallucinations of the past* and
attributes specifically to append-only fact stores.

Retirement did exist, but it was reached from exactly **one** place. `applyOne` fills
`supersede_queue` only from near-duplicate collapse, where a neighbour qualifies on
cosine similarity plus subject overlap. So the decision to retire a fact was made
entirely by **similarity**, and nothing anywhere asked whether two claims can both be
true. That cuts both ways: two facts that genuinely conflict but score below the merge
band both survive and both recall, while two near-duplicates that do *not* conflict
risk being collapsed into one.

Now, for each fact written, the neighbours in the "related but distinct" band — at or
above `related_threshold`, below `merge_threshold` — go to a new tool-less judge as
**pairs**, which answers `contradicts` / `independent` / `unclear`.

**It writes nothing.** Every verdict is counted, reported and discarded:

```
conflict candidates 3, judged 3 (1 would be retired as contradicted,
2 independent, 0 unclear) — reporting only, nothing was changed
```

That is the design rather than a limitation. The band has never been measured against
real conflicts, and a detector that retires facts on an unmeasured threshold is the
failure the merge band already taught this pipeline about once. The report says what
enforcement *would* do so an operator can calibrate first, and acting on a verdict
will require **its own key** — a deployment that turns detection on today cannot start
retiring facts on a later upgrade without a second, separate decision.

Turn it on with `memory.consolidation.detect_conflicts: true`. Off by default, read
through the capabilities report like `verify_writes` and the similarity bands, and
bounded at four extra judge calls per pass.

### Three things this needed less of than the design expected

- **No new threshold.** `related_threshold` has been in the operator config *and* the
  capabilities report all along, documented as exactly this band's lower edge — it
  outlived the code that used to read it. That code appended `" (related: …)"` to the
  fact TEXT and was removed because the tails nested into the stored value and
  therefore into the embedding, defeating the very merge band they were meant to
  inform. Its epitaph — *"there is nowhere structured to put the linkage"* — is what
  stopped being true: the entity sidecar carries `invalid_at` / `expired_at` /
  `judged_by`, and supersede records which chunk replaced which.
- **No new wire op.** The write path enforcement will need already exists.
- **One Go field.** `detect_conflicts`, beside `verify_writes`.

### One deliberate asymmetry

The subject-overlap gate is **not** applied to conflict candidates, though the merge
path uses it two lines above. There it stops a destructive merge resting on a single
similarity number, and it earns its place. Here it would defeat the purpose: the
reason to ask a model at all is that lexical overlap misses real conflicts — it scores
**0.000** for a non-Latin paraphrase pair, because the tokenizer only sees `[a-z0-9]`,
and around 0.5 for two different-language facts sharing nothing but a brand name.
Screening candidates with the signal whose blind spots this exists to cover would
inherit every one of them. A test with a Cyrillic neighbour fails if the gate returns.

The **class** refusal does carry over, including the half that refuses a key the pass
cannot parse — an opaque row from a remote backend, or one a user wrote themselves.
Detection writes nothing today, so that half is not load-bearing yet; it is enforced
from the start so enforcement needs no widening later.

### A separate judge agent

`memory/conflict-judge` is a new tool-less agent on the same tier as the entailment
judge, not a second question added to it. That judge's system prompt is deliberately
single-purpose and its tier choice rests on a measured eval; a differently-shaped
question in the same prompt would put that result in doubt every time either question
changed. Two single-purpose agents cost one extra def.

### Adapters

None. The flag is yaml, and the capabilities payload is untyped in both adapters, so
`@loomcycle/client` and the Python package are unchanged at 1.58.0 — as at v1.59.0.

## What's in v1.59.0

**The document viewer surfaces the document id and every chunk id, each with a
copy button.** Minor rather than a patch: the Web UI is embedded in the runtime
binary, and a `vX.Y.Z` patch tag builds only the browser sidecar — so a patch
could not ship a UI change.

### Copy document + chunk ids from the viewer (#1060)

Wiring a document or a specific chunk into an external workflow — n8n over the
MCP/HTTP API — needs the exact `document_id` / chunk id the API expects. The
viewer used those ids only internally (React keys, data-layer calls) and never
displayed them, so there was no way to grab one from the UI.

The `@loomcycle/explorer` document viewer now surfaces them, each with a
click-to-copy button:

- the **document id** in the toolbar,
- the **selected chunk id** in the content head,
- a compact copy on **every chunk-tree row** (faint until the row is hovered or
  selected, so the tree keeps its clean scan-line).

The on-screen id is monospace and ellipsized, but the clipboard always receives
the FULL id — a long UUID displays compactly yet copies completely. The copy
click never selects the chunk or toggles the tree, and a failed copy (an
insecure context with no clipboard API) degrades to a `✗` rather than throwing.

## What's in v1.58.0

**A search over the document store now says where its results came from, and an
embedder migration runs in batches.** Minor rather than a patch: the search response
gains two wire fields, and a `vX.Y.Z` patch tag builds only the browser sidecar —
both changes here live in the runtime binary, so a patch tag could not ship either.

### A document search hit names its document and heading (#1058)

A semantic search that spans the doc store returned prose addressed only by an opaque
`doc.chunk:<32 hex>` key. The id is the right ADDRESS — stable, unique, what
`get_chunk` takes — but it is not an identity. A live query for *"the judge withholds
a fact whose span does not support it"* returned six hits, all six of them the right
document, and nothing in the response said so:

```
doc.chunk:2f51f960973ec940301a4d4aebcf35ff
doc.chunk:f5e7492bc3f2065dad0272c969d8beb0
…
```

Anything wanting to cite a result had to fetch each row just to learn its heading.

**Keying by title instead was the tempting fix and is wrong**: titles are not unique,
not stable under a rename, and not addressable. So the key stays and the hit carries
its readable identity beside it — `document` (the document's title) and `title` (the
chunk's own heading), both additive and `omitempty`, on the off-run
`POST /v1/_memory/search` and the in-band `Memory op=search` alike.

The in-band surface also gains **`chunk_id`**, which it never had. The two projections
are hand-maintained copies, so an agent could be shown a document body it had no way
to fetch, while an operator on the HTTP endpoint got the id — a five-release drift
that only became visible once both were touched at once.

Labels resolve in ONE batched query per search, and are **best-effort by
construction**: they live in SQL Memory, a different plane from the bodies, so a
deployment without it, an unkeyable scope, a store fault, or a chunk deleted
mid-search each cost the label and never the result. The resolver owns the
Memory→SQL Memory scope mapping so neither caller restates it — the two planes key the
*tenant* scope differently (`""` against the tenant itself), and a restated rule is
how that axis drifts. A drift test compares the mapping against
`Document.resolveScope` directly, because a divergence there resolves against an empty
schema and the labels simply stop appearing, which is indistinguishable from "no
labels available".

The Memory console names a hit `document › heading` instead of printing a 32-hex id,
keeping the id on the tooltip. A document's root chunk carries the document's own
title, so the two are collapsed rather than rendered as `Verified writes › Verified
writes`.

One assertion in this change was **found vacuous and replaced**, which is worth
recording because the failure mode is not the usual one. To prove the resolver
deduplicates chunk ids, the test asked for each id twice and checked the label count
— and it passed with the dedup pass deleted, because `WHERE id IN (a,a,b,b)` returns
one row per DISTINCT id regardless: SQL `IN` has set semantics, so the assertion could
not see the difference. Nothing about the representation was misread; the layer below
already provided the property. The replacement pins a consequence only this code
controls — ids are deduplicated BEFORE the lookup is capped, so a page of hits from
one document cannot crowd out a different chunk's label.

### reembed embeds in batches, not one call per row (#1057)

A real embedder migration measured **~12 rows/minute**: 3,633 document-chunk rows
moving from a 768d model to a 1024d one, about five hours, because the loop called
`Embed` once per row and every row paid a fresh HTTP round trip and prefill setup
against a local Ollama. The `Embedder` interface has always been batch-shaped (N texts
in, N vectors out, chunked again by the driver's own batch size), so the per-row call
was leaving that on the floor.

Rows now reach the embedder in batches of 64 — two orders of magnitude fewer round
trips — while the STORE WRITE stays per row. That is what keeps the operation
resumable: a client timeout or a cancelled context mid-sweep costs only the current
batch, and the next call picks up what is left. It is also how an operator paginates a
scope too large for one request, so it is preserved deliberately. Not "all of them" in
one call, either: a single `Embed` over a whole 1000-row page would make one failure
cost the page, and document bodies can be large.

**A batch error falls back to per row.** One unembeddable row must not cost its
batch-mates: before batching, a bad row was counted and skipped while the rest
migrated, and that accounting is what an operator reads to decide whether a sweep is
done. Retrying singly restores it exactly — and as a side effect a transient one-shot
fault now recovers instead of stranding a row. The store write moved into one shared
helper so the batch path and its fallback cannot drift in what they record, and the
recorded dimension stays the OBSERVED width of the returned vector rather than the
embedder's advertised one (a driver that cannot know its own dimension answers 0, and
a row written with 0 makes every later search in that scope report a spurious
mismatch).

The existing partial-failure test had to be **reworked, not extended**: it asserted
the handler's CALL PATTERN rather than a guarantee. Its stub failed "the next call",
which under batching describes exactly the transient fault the fallback now retries,
so it reported 2 reembedded where it wanted 1 and 1. The stub now models a
permanently bad ROW and the test asserts the contract — an unembeddable row is
reported in `failed` and `failed_keys`, never silently dropped.

**Measured on the deployment that prompted it**: the migration that had been running
at ~12 rows/minute completed a 3,650-row scope, which now reports a single embedder at
a single width.

### Adapters

`@loomcycle/client` **1.58.0** carries the two new optional fields on
`MemorySearchEntry`. `@loomcycle/memory-view` **0.4.0** renders them. The Python
adapter is republished at **1.58.0** to keep the install-name version aligned with the
runtime; it has no changes this release.

## What's in v1.57.1

**An embedder migration could not reach the tenant holding the data.** Patch — one
server-side fix; no wire, schema or adapter change.

### reembed ignored ?tenant=, and reported a truthful-looking zero

`POST /v1/_memory/reembed` took the tenant from the caller's principal ALONE. An
operator migrating another tenant's scope after an embedder change therefore swept their
OWN partition, found nothing, and got `rows_total: 0` — indistinguishable from "already
migrated".

Observed on a live deployment mid-migration from `embeddinggemma` (768d) to `bge-m3`
(1024d): a legacy admin token carries no tenant, so every call resolved to the default
partition and reported zero, while `embed_stats` for the real tenant showed **3,633 rows
still on the old embedder against 17 on the new**. The console's own reembed plan showed
the same zero for the same reason.

What makes that worse than a wrong number: once ANY row of the new dimension exists, the
search-time dimension pre-check (which samples one row) stops firing. Semantic search
then returns only the post-switch slice, with plausible scores, instead of failing
loudly — so the operator has no signal that 99.5% of the index has gone unreachable.

`principalTenantScope` now resolves the tenant, so `?tenant=` reaches both the read and
the write-back, and an authenticated admin who names none is REFUSED rather than
defaulted — a reembed writes, and spends one embedder call per row, so sweeping an
unnamed partition is not merely a misleading count.

**The refusal is narrower than its siblings, deliberately.** `principalTenantScope`
reports all=true for a request with no principal, which is every request on an open-mode
deployment — and an open-mode install has no tenants, so demanding one there would break
a route that has worked since v0.9.0 to buy no safety. It fires only for an authenticated
admin; all five pre-existing reembed tests pass unchanged.

This was the third variant of one defect. `embed_stats` was already fixed to honour
`?tenant=`, and `backfill_embeddings`, `purge_stale_embeddings`, erasure, directory and
orphan-repair all guard the unnamed-tenant case. reembed was the one sibling with
neither.

**Why it survived:** the test double was blind to the tenant in three places —
`vectorAdminStore` dropped the tenant argument on three embed methods and keyed rows on
`scope|scope_id` alone, its list method joined the k/v row with a hardcoded empty tenant,
and its stats method prefixed without one. A double that ignores the dimension under test
cannot fail a bug in it. All three now carry the tenant through a shared `embedKey`.

### Release mechanics

A `vX.Y.Z` patch tag builds only the browser sidecar, and this fix is in the runtime
binary — so this release was published with `force_full=true` on the release workflow to
produce binaries and images. goreleaser's `mode: keep-existing` preserves the
annotated-tag notes the lean job pre-created.

## What's in v1.57.0

**Two defects that reached an operator, and a signal for the failure neither of them
could report.** Minor rather than a patch for two reasons: the change feed gains a new
wire field, and a `vX.Y.Z` patch tag builds only the browser sidecar — the fix that
matters here is embedded in the runtime binary, so it needs a full build to ship at all.

### The console's Backfill and Purge buttons threw (#1052)

Pressing either in the Memory console produced `e.backfillEmbeddings is not a function`.
Both methods landed in `@loomcycle/client` at 1.55.0; `web/package.json` depended on
`^1.49.0`.

**Why nothing caught it** is the part worth knowing. `@loomcycle/memory-view` compiles
from SOURCE through a Vite alias, and `web/vite.config.ts` lists `@loomcycle/client` in
`resolve.dedupe` — added, per its own comment, "for good measure — a duplicate is
wasteful even if not fatal". Dedupe makes web's copy the one that lands in the BUNDLE,
which turned a wasteful duplicate into a version DOWNGRADE. Both plausible guards are
structurally blind to it: `tsc --noEmit` (which web's build does run) resolves the
client from the memory-view source file — the package's own, correct copy — and
typechecks clean against a version the bundle will not contain; `vite build` strips
types and checks nothing. Green in CI and in `make build-ui`, broken only in a browser
at the moment a button was pressed.

`reembed` kept working throughout, because `reembedMemory` has existed since 1.49.0 —
which is why three buttons failed in two ways and looked like three unrelated bugs.

Fixed by bumping web to `^1.56.0`, verified by asserting the built bundle contains the
methods rather than by inference. A new **`TestWebDedupedDeps_SatisfySourcePackagePeers`**
makes it non-recurring: for every package web consumes from source, a peer dependency
that is also in vite's dedupe list must be satisfied by web's own range. It is a Go test
because `go test ./...` is the gate everyone runs and it needs no `node_modules`, and it
parses the dedupe list from the real `vite.config.ts` so removing a name there relaxes
the test honestly.

Also fixed alongside: `purge_stale_embeddings` advertised `agent, user, tenant` in its
validator and then demanded `scope_id` unconditionally, refusing the tenant scope its
siblings accept (the tenant keyspace is one partition with an empty store scope_id).
**Latent** — the client never had the method, so that request never reached the server.

### Non-Latin facts collided on one memory key (#1047, #1053)

`rawWords` tokenises on `/[^a-z0-9]+/`, so a fact in Cyrillic, Greek, Hebrew or any CJK
script reduced to no words and `slug()` fell back to the constant `"unnamed"`. `factKey`
is the consolidation pass's idempotency mechanism and the write path upserts on it, so
the SECOND such fact silently overwrote the first: a scope's entire non-Latin population
collapsed onto ONE row. Confirmed against the real engine — two different Ukrainian facts
both keyed `memory/fact/unnamed`. The entity-tier mirror used the same string as its
`natural_key` and collided identically.

An empty slug now appends a fingerprint of the text: a pure function of it (or a re-run
of the pass would duplicate rather than converge), two 32-bit hashes rather than one (a
single space collides at roughly a percent by ten thousand facts, and a collision is the
failure being fixed), shift-and-add only (the engine is ES5.1-era: no `Math.imul`, and a
32-bit multiply loses precision past 2^53), and base36 so the key stays inside
`[a-z0-9-]` — it is interpolated into SQL by the lookup path.

**It does not widen the tokeniser.** A non-Latin fact still derives no source span, so it
stores unverified-and-visible rather than verified. That change redefines what a "word"
is and needs the `merge_min_subject_overlap` floor re-measured against its labelled
pairs; it is deliberately separate. This stops the data loss.

⚠️ **It cannot recover what was already overwritten.** On a store that held several
non-Latin facts, only the last survives, and this release prevents further loss rather
than restoring the earlier ones.

### The change feed says what the embedder is doing (#1051)

An embedder outage is invisible at every call site, by design: a failed embedding on a
content write is not fatal — the body is stored and the embedding skipped with a log line
— because losing an author's text to an unreachable embedder is worse than losing its
searchability. The consequence is that writes succeed, change frames keep arriving, and
search quietly stops finding anything written during the outage. A reader watching the
Activity tab to answer "is the memory pipeline healthy" saw a busy feed and concluded yes.

The opening `feed` frame now carries an additive `embedder` block: `state`
(`absent` | `untried` | `ok` | `failing`), provider, model, calls, failures,
`last_failure_kind`, and the last ok/failure stamps. Not a probe — a probe would cost a
model call per reader and describe one instant; a new `providers.ObservedEmbedder`
decorator records what the traffic that already went through actually did, so the answer
cannot disagree with reality (the same argument as `cdc.Store.CapturesChanges()`).

Four decisions with a plausible wrong answer, each pinned by a test: `untried` is its own
state, because a configured-but-never-called embedder must not report `ok` — that is the
state a freshly booted deployment is in; the failure COUNT is reported, not a boolean,
because it is roughly how many rows need `backfill_embeddings`; no error TEXT escapes,
since a transport failure reads `dial tcp 192.168.0.77:11434: …`, a map of the operator's
network, so failures are a classified kind matched by `errors.Is`; and it is separate from
`enabled`, because capture being on and the embedder working are independent failures.

`@loomcycle/memory-view` **0.3.1** surfaces it as one sentence for a failing or absent
embedder, and stays silent for `untried`, `ok`, and a runtime too old to send the field —
warning about the normal boot state would train an operator to ignore the only line that
matters when it fires.

### Adapters

`@loomcycle/client` and `loomcycle` (PyPI) go to **1.57.0** as version-aligned lockstep
releases; no client-surface change (the `embedder` block rides an SSE frame the TS client
already forwards, and the fixes are runtime-side). `@loomcycle/memory-view` **0.3.1**
publishes on its own tag.

### Upgrading

Nothing to migrate, and the two operator-visible notes are above: the console fix is
embedded in the binary so it arrives only with this build, and the keyspace fix prevents
further collisions without recovering earlier ones.

## What's in v1.56.1

**A tool-call regression on qwen-via-Ollama, and the v1.56.0 bundle change that caused
it.** Patch — one runtime fix and one config revert; no wire, schema, or adapter change.

### qwen on Ollama stopped calling tools

A `chat/medium` run on a local qwen3.6 model wanted to search but emitted its call as
TEXT, copying the framing of loomcycle's own injected `{{tool:Context.*}}` reference
blocks — `<tool_result> {"type":"function","function":{"name":"WebSearch", …}} </tool_result>`.
Ollama's extractor expects `<tool_call>`, so it recovered nothing, and the existing
text-recovery parser only understood a flat `{name, arguments}` object. The wrapped,
OpenAI-nested shape fell through, and the run ended with the un-executed block as its
"answer" — no search ran.

- The Ollama driver's `tryParseToolCallsFromText` now peels one wrapper tag
  (`<tool_call>` / `<tool_result>` / `<function_call>`, hyphen or underscore, with or
  without attributes) and accepts the OpenAI-nested `{"function":{name,arguments}}`
  envelope, with arguments as an object or a JSON-encoded string. Same strict,
  tools-gated contract — prose and non-tool JSON still recover nothing.

### inject_tool_guide is no longer a bundle default

Making `inject_tool_guide` a bundle default in v1.56.0 was premature: its target is
small local models, and that is exactly where the three injected `<tool-result>`-framed
blocks both bloat the prompt and get copied by the model as its tool-call format. The
flag is removed from every bundled agent; the mechanism — `Context op=guide`, the
`HintedTool` hints, the injectable refs, and the per-agent flag — stays intact for
explicit opt-in on a strong-model agent.

## What's in v1.56.0

**Agents that know how to call their tools, a live view of the memory pipeline, and
three defects the first live verified-writes run turned up.** Minor — two feature
lines and three fixes, two of which change numbers an operator may be watching.

### Agents start with runtime knowledge instead of blind

The loop already sends every tool's full JSON schema on each request, but small and
local models under-attend to that array: they guess which op to call and which fields
are required, then learn from refusals. The system-prompt injector could restate tool
NAMES and nothing about how to use them.

- **`Context op=guide`** returns, for this run's resolved tools, `{name,
  side_effect_class, ops[], required[], hint}`. The ops and required fields are parsed
  from each tool's OWN input schema, so the digest cannot drift from the schema the
  model is being sent; the hint comes from a new optional `tools.HintedTool`,
  implemented on the builtins that produce the most call errors — Memory, Document,
  Path, Channel, Agent, Skill.
- **`{{tool:Context.guide}}` and `{{tool:Context.capabilities}}`** are now injectable
  prompt references (both pure reads, resolvable at assembly time), sorted for
  byte-stable prompt caching.
- **A per-agent `inject_tool_guide`** appends both refs automatically, so an operator
  need not hand-place placeholders. **Default OFF** — every existing custom agent stays
  byte-identical until it opts in, and a flag-OFF agent takes the unchanged fast path.
  Round-tripped through the AgentDef create/fork overlay, the read adapter, the
  MD-frontmatter loader and `content_sha256`, mirroring `memory_protocol`.
- **The bundled LLM agents opt in**: `chat/*`, `doc/manager`, the agent-teams and
  team-examples agents, and `memory/ontologist`. Deliberately not flagged: the doc/team
  sub-agents are SKILLS injected into the flagged agents (the field has no meaning on a
  skill), the `code-js` agents run no prompt, and `memory/extractor` / `memory/judge`
  hold no tools so the guide would render nothing.

### The memory console can watch the pipeline work

RFC CF's last phase. A consolidation pass takes minutes and reports only at the end, so
there was nothing to look at in between.

- **Both change feeds now open with a `feed` frame** stating whether writes are actually
  being captured, plus the cursor in force. This closes a real trap: with
  `LOOMCYCLE_MEMORY_CHANGES_ENABLED` unset nothing writes the change table, so the
  stream connects, keepalives flow, and no frame ever arrives — indistinguishable from a
  healthy feed over a quiet store. The answer comes from the STORE (a new
  `cdc.Store.CapturesChanges()`), because the CDC decorator is in the write path exactly
  when the feed is on and therefore cannot disagree with reality the way a second reading
  of an env var can. Additive: a subscriber that switches on the change type ignores an
  event name it does not know.
- **An Activity tab** in `@loomcycle/memory-view` 0.3.0 tails both families, showing
  coordinates and never values (the feed is value-free by design). It keeps three
  non-live states apart — this build cannot tail / the runtime says the feed is off /
  connected-and-quiet — because collapsing them into "no rows yet" makes two
  misconfigurations look like "the pass is doing nothing".

### Three fixes, from running verified writes on real data for the first time

The verified-writes line shipped in v1.54.0 and had never been enabled anywhere. Turning
it on against real chats, with a local extractor and judge, worked — and exposed three
defects that only appear on real data.

- **`verification_stats` counted entity IDENTITY nodes as facts.** The pass mirrors each
  fact as two chunks — the claim, and an identity node for the subject it is about — and
  both carry entity metadata. An identity node can never carry a span (a subject is a
  name), so every new subject added a permanently unverifiable row and the reported share
  FELL as the store got richer. Measured on a live store: `0.579` reported where `0.846`
  was true, and 7 facts called impossible to verify where the answer was 1.
  **⚠️ Your numbers will move on upgrade** — the verified share rises and
  `unverifiable_no_span` falls, with no data change behind it. `list_facts` gains an
  opt-in **`claims_only`**; the default listing stays wide because document federation
  reconciles identity nodes too. The obvious fix was wrong and measurement caught it:
  filtering `type = 'fact'` drops operator-`remember`ed facts, one of which landed as
  type `object` while carrying a span.
- **Span derivation attached SEARCH PATTERNS as evidence.** Two consecutive passes each
  withheld a TRUE fact whose recorded span was a tool-call line or a regex — a local chat
  model had written its tool calls as prose, and Dice overlap rewards a short candidate
  whose every token hits, so a 6-token pattern beat a 30-token sentence that stated the
  fact outright. The judge was right to refuse both; the evidence was mis-attached
  upstream. `splitSentences` now drops a candidate with no assertion in it — one that is
  only tags, or dense enough in punctuation to be code. Such a fact gets NO span
  (unverified and visible) rather than a false one. Does not re-derive spans already
  stored.
- **`loomcycle validate` exited 2 on every bundled agent preset.** It applied the pin-path
  rule to agents that name a `tier:`, so it named agents the running server resolves
  fine — and because it returns at the first agent, MCP servers and everything after went
  unchecked. `agents list --json` bailed mid-array, leaving unparseable output. The tier
  is now checked first, mirroring the runtime; an agent with neither a pin nor a tier
  still fails, which the resolver also refuses.

### Adapters

- **`@loomcycle/client` 1.56.0** — carries `claims_only` on the Document tool input.
- **`loomcycle` (PyPI) 1.56.0** — no functional change; realigned so the python tag
  publishes (PyPI was left at 1.54.0 when v1.55.x shipped without a python tag).
- **`@loomcycle/memory-view` 0.2.0 → 0.3.0**, published on its own tags: the facts and
  verdict surface, the coverage strip, `remember`, the two embedding-maintenance ops, the
  claims-only fact list, and the Activity tab.

### Upgrading

Nothing to migrate. The change feed stays opt-in (`LOOMCYCLE_MEMORY_CHANGES_ENABLED=1`),
`inject_tool_guide` is off unless an agent sets it, and `verify_writes` is still off by
default. The one visible difference on an existing store is the corrected coverage
arithmetic described above.

**Known issue:** facts whose text is in a non-Latin script all slug to one memory key and
overwrite each other ([#1047](https://github.com/denn-gubsky/loomcycle/issues/1047)) —
pre-existing since the deterministic-key consolidator, unaffected for Latin-script
deployments.

## What's in v1.55.1

**An operator may vouch for a fact that has no span.** Patch — one reported bug, its root
cause, and two pieces of copy that were saying something untrue.

- **The bug.** Opening a fact stored before spans existed, writing "User confirmed", and
  being refused. Both controls failed there — only `unclear` was allowed without a span —
  so the console offered two buttons the server would never accept.
- **The rule's own justification had expired.** `judge_fact` refused because a verdict
  without evidence "would be indistinguishable from one that was checked". That stopped
  being true when `judged_by` landed in v1.54.0: the store now records whether a machine
  or a human reached a verdict, so an operator putting their name to a fact is no longer
  mistakable for an entailment check. **An operator** may now record `supported` or
  `unsupported` on a span-less fact — no span is invented, it still counts as span-less in
  coverage, and the reason plus `judged_by` carry whose word it is. **An agent still
  cannot**: a run affirming what it cannot check is exactly the claim a span exists to
  substantiate. **`mistyped` is excluded for anyone** — it says the span carries the claim
  but the filing is wrong, so with no span there is nothing for it to be about.
- **Copy.** The console now distinguishes CHECKING from VOUCHING at the moment your name
  goes on a verdict; and the coverage strip no longer describes span-less facts as ones
  that "can never be verified by anyone" — they cannot be checked against a source, but a
  person can still vouch.

Built FULL via a `force_full` dispatch despite the patch tag: the fix is in the Go runtime,
and the lean patch tier builds only the `loomcycle-browser` image, which would have left
the change unable to reach a deployment. The Web-UI half lives in
`@loomcycle/memory-view`, which publishes on its own `memory-view-v*` tag.

## What's in v1.55.0

**One memory console, and an erasure you cannot perform unrecorded.** Minor — the memory
surfaces built over the previous release are consolidated into a single console, an
operator can write a fact down themselves, retention is finally readable, and subject
erasure grows a durable record of who asked for it. Plus Web-UI authoring for the two
peer-federation substrates that shipped headless in v1.54.0.

### RFC CF — the memory console

- **One console, not two.** v1.54.0 added a facts panel and a search panel to the Web UI
  beside `/memory` — which is a thin wrapper around `@loomcycle/memory-view`, a published
  package that already shipped `entries` | `facts` | `search` tabs. The duplicates are
  removed and the genuinely new capability folded into the package: **spans, verdicts,
  `include_refuted`, and the two controls that let a person overrule the judge**, plus a
  coverage strip over the fact list. The line drawn: the package owns views over one
  scope's data, the app owns the operator plane — so starting a consolidation pass is a
  host-supplied callback rather than a URL the package knows.
- **The safety valve exists now.** Verified writes shipped with "a wrong verdict is always
  recoverable by re-judging" as its argument for withholding rather than deleting, and
  that recovery was reachable only by API. An operator can now see the claim, the span it
  was drawn from, and the verdict — and overrule it in either direction.
- **`judged_by`** records WHO reached a verdict, server-stamped from the call's own
  context exactly as `origin` is. There is no wire field to set it: an agent able to
  record "an operator decided this" would be one injected instruction away from laundering
  a machine's verdict into a human's. An agent's verdict carries the agent's NAME; a
  verdict predating the column reads as unknown rather than as either party.
- **`Document op=remember`** stores a statement a person supplied as a fact that **cites
  itself** — the text becomes both the claim and its source span, filed `evidential`. An
  operator's instruction is a source, not a claim; storing it self-citing makes
  operator-authored memory the best-evidenced kind rather than the worst. Additive only:
  there is no "forget", because an instruction that deletes on a fuzzy match is how data
  disappears quietly.
- **Retention is readable.** `GET /v1/_retention` gets a Settings surface: what the sweeper
  is configured to remove, per family, with the sweeper's own on/off state stated first.
  A `purgeable` count is never rendered alone — it is computed regardless of mode, so
  beside an `off` family a bare number reads as a countdown to a deletion that never
  happens.
- **Settings tabs get the three-tier role class** the left nav already had. The binary
  `admin` boolean could not express a tenant operator, so a delegated user reaching
  `/settings` directly was shown five tabs whose every call 403s.

### Subject erasure — audited, or refused

- **An erasure now writes its own durable record, and refuses without one.** The audit sink
  says recording "must never block the caller's primary operation"; erasure is the
  deliberate exception, because it is the one operation nothing can undo. A deployment
  that cannot say who erased which subject does not get to perform the deletion — refusing
  is recoverable, an unrecorded erasure is not. With no `LOOMCYCLE_AUDIT_LOG_PATH`,
  erasure is disabled and boot says so.
- **Two records, ordered.** `erase_intent` is written BEFORE any deletion and its failure
  refuses the operation; `erase_result` follows with the planes deleted and retained. A
  crash between them still leaves evidence that an erasure was attempted, by whom, against
  which subject. Dry runs write nothing.
- **The console: report before execute, always.** The erase control does not lift until a
  report has been shown for the SAME subject — otherwise an operator could read what would
  go for one person and confirm the deletion of another, which no confirmation dialog
  catches. Confirmation is the subject id typed back and compared exactly: a modal with a
  red button is dismissed by reflex, and the failure mode here is erasing the WRONG
  subject rather than erasing accidentally. The three tiers are never summed — they are
  degrees of REACH, so each is labelled by what it does ("will be deleted" / "held, but
  NOT deleted" / "cannot be reached") — and a residue of zero across zero sessions reads
  as UNKNOWN rather than none, since with no sessions examined there was nothing to trace
  derived data from.

### Web UI — the peer-federation substrates

- **Remote memory backends** (RFC CD Part B) are authorable from Integrations: a kind
  selector reveals the peer connection — `base_url`, `api_key_env` (an env-var NAME, never
  a secret) and an optional `api_version`.
- **Document Sources** (RFC CE) join them as a fifth Integrations family, so a peer
  `DocumentSourceDef` can be authored, forked and retired from the Web UI rather than only
  from yaml, the substrate API or MCP.

### Adapters

- `@loomcycle/client` gains **`backfillEmbeddings()`** and **`purgeStaleEmbeddings()`** —
  the two embedding-maintenance ops that had no client method. `@loomcycle/memory-view`
  will surface them once it can depend on this release.

## What's in v1.54.0

**External data access, document federation, and verified writes.** Minor — three feature lines land together: any app reaches loomcycle's memory + documents over a documented HTTP contract or a peer loomcycle (RFC CD), a document is replicated to and reconciled with a peer instance in both directions (RFC CE), and a fact records the source it came from and is checked against it before it is trusted (RFC CC). Plus the loomboard saved-views scaffold (RFC BT) and a memory/facts Web-UI surface.

### RFC CD — external data-access channels

- **OpenAPI 3.1 contract + Swagger UI.** A hand-authored `api/openapi.yaml` documents the whole data surface — the memory REST family + `POST /v1/_memory/search`, the `/v1/_document` and `/v1/_path` op-dispatch (modeled as a `oneOf` on `op`), and the asset GET — served at `GET /v1/openapi.{yaml,json}` with a self-contained Swagger UI console at `/v1/docs` (vendored, no CDN). Any language gets a generated SDK; the contract is public, the data still bearer-gated. A drift test pins the spec's op enums to the tool op sets.
- **A peer as a memory backend (`kind: remote`).** A memory backend can now proxy an agent's memory to *another* loomcycle instance's `/v1/_memory/*` — the peer embeds server-side; get/set/list/delete/search/stats all round-trip. Auth is a credential-allowlisted `api_key_env` (never an infra secret, never `${...}`-interpolated), resolved at use time; the peer host is dialed through the SSRF guard; `fallback_on_error` degrades to local.
- **Change feed (CDC), pull + push.** An opt-in, value-free change feed (`LOOMCYCLE_MEMORY_CHANGES_ENABLED`) emits at the store write choke point so both in-run and external CRUD land in one stream. Consumers subscribe over SSE (`GET /v1/_memory/changes`, `/v1/_document/changes`) or register a config-declared `change_subscriptions:` HMAC-signed webhook with a persisted at-least-once cursor. Tenant-scoped, SSRF-allowlisted; the feed is operator observability (`substrate:tenant`, not member-accessible).
- **Python gRPC memory parity.** A generic `Memory` RPC + `client.memory()` gives the Python adapter the memory surface it lacked, riding the same op-dispatch shape as documents and paths.
- **Ops:** every published image is mirrored to `ghcr.io` so a Docker-Hub-rate-limited operator has a fallback.

### RFC CE — remote document backend + federation

- **Bind + reconcile a document with a peer.** Declare a peer under `document_sources:` (or author one at runtime — see the substrate below), bind a local document to a peer document with `Document op=set_remote`, and reconcile with `op=sync`. `direction:pull` (default) copies the peer's chunks in; `direction:push` writes this document's chunks up to the peer. Reconciliation keys on `natural_key` and carries each keyed chunk's **body, tags, hierarchy** (it lands under its keyed parent at its sibling position) and its **manual cross-reference links**; a diverged body is updated in place with the overwritten body kept in the *losing side's* chunk history (retire-not-delete); a chunk without a `natural_key` is excluded and counted.
- **`op=diff_remote`** — a read-only dry-run that classifies keyed chunks into `only_local` / `only_remote` / `diverged` / `retagged` / `reparented` / `same` (plus unkeyed + edge-drift counts) without touching either side, so you see exactly what a sync would change first.
- **`DocumentSourceDef` substrate** — a source can be authored, forked, and retired at runtime as a versioned, tenant-scoped substrate Def over every transport (HTTP / gRPC / MCP / TS / Python), a faithful mirror of `MemoryBackendDef`; `set_remote`/`sync` resolve a name tenant-substrate → static yaml → shared substrate.
- **Reviewed + hardened.** A whole-line adversarial code review surfaced eight findings, all fixed with fail-before regression tests: sync now converges on a refuted chunk (no create-churn), the `DocumentSourceDef` HTTP route is `substrate:tenant` in parity with gRPC/MCP, title/type/status edits propagate, `diff_remote`'s reparent prediction matches what a sync does, a retired source stops resolving, a tags-only sync no longer writes a duplicate-body revision, and the runtime + static source validators accept the same values.

### RFC CC — verified writes

- **A fact records where it came from and is checked against it.** A fact now stores the exact `source_quote` span it was derived from (RFC CC P1); the subject becomes structured data the ontology gates entity writes against (P2); a write-time judge checks each fact against its own quote and **withholds** what fails (a refuted fact is retained but hidden from `list_facts`/`graph_recall`, readable with `include_refuted`), rather than deleting it. `verbatim_answer` answers a lookup question with a stored fact quoted verbatim plus its span and no generated text; a backfill sweep judges facts that predate the feature; `judged_at` records *who* judged a fact in a column the caller cannot set.
- **Web UI.** A Memory tab surfaces verification coverage and lets an operator start a pass; a facts surface shows evidence, verdicts, and a way to overrule the judge; semantic search over memory is the front door to the facts panel.

### Adapters + packages

- **`@loomcycle/client` 1.54.0** (npm) — adds `documentSourceDef()` and the accumulated substrate/memory surface.
- **`loomcycle` 1.54.0** (PyPI) — adds `memory()` (gRPC) and `document_source_def()`.
- **RFC BT** — the `@loomcycle/loomboard` saved-views scaffold (P1) + the board-bound `TeamDef op=run` task-key tagging (P4); loomboard versions on its own `loomboard-v*` tags. The React `explorer`/`library`/`memory-view` packages version independently.

## What's in v1.53.2

**Tenant-member access: a non-isolated user reads and writes the tenant plane (RFC CB).** Patch (Go, auth only). A delegated per-user token that is not isolated is now a full tenant *member* over HTTP.

**The gap.** A non-isolated user token (a "member" — `runs:*`/`channel:*`, `access_mode "tenant"`) already had whole-tenant *data* access inside a run — loomcycle's rule is "whole-tenant data access is conferred by NOT being isolated." But the same token was 403'd at every HTTP `/v1/_*` tenant surface by the route scope-gate alone, so a member could not browse or author its tenant's shared Library, Documents, or Memory over HTTP, and consumers (loomboard) hid those views rather than render a wall of 403s.

**The change.** `authMiddleware` now admits a non-isolated principal on a *member-accessible* tenant route: `tenantMemberAccessible(method, path) && !auth.IsIsolated(p)`. The predicate opens any route that requires `substrate:tenant`, **minus** the operator-control carve-outs — user create / roster mutation / user-token minting, per-subject erasure, budget *writes* (`PUT`/`DELETE /v1/_limits`), and tool-use hooks — which stay operator-only. `substrate:admin` routes (cross-tenant enumeration, runtime admin, token minting) are excluded automatically, since they are not `substrate:tenant`.

**No new scope, no re-mint.** It keys on the existing isolated/non-isolated boundary — generalizing RFC BY's discovery tiering to the route gate — so no token changes. The isolation floor is untouched: an isolated `substrate:user` token still fails every tenant gate, `auth.IsIsolated` stays true for it, and `ConfineIsolatedScope` plus the run-start isolation stamps are unchanged. A member is still confined to its own tenant by each handler's `principalTenantScope`/`tenantFromCtx`.

**Scope.** The HTTP gate. gRPC/MCP substrate-plane parity is deferred (a member's runs already reach tenant data on those planes since they are not isolated). loomboard's `canTenant` relax — so a member renders the Library/Documents/Memory nav — ships in loomboard separately. No adapter or Web UI changes here.

## What's in v1.53.1

**The routing view stops blanking on an empty tier cascade.** Patch (Go + Web UI). The Settings → Routing page went blank on a real deployment, and the cause was a `null` where the UI expected a list.

**The bug.** `GET /v1/_routing` builds each tier's candidate cascade by appending to a nil slice, so a `user_tier × tier` that resolves to **zero candidates** — a perfectly valid config state, e.g. a `high` user-tier whose `high` tier has no available model — came back as JSON `"cascade": null`. The routing view's `TierCard` does `tier.cascade.length` on that; `null.length` throws, React unmounts, and the whole page is blank. Same class as the `/v1/_usage` nil→null crash in v1.11.1.

**The fix, at both ends.** `handleRouting` now initializes the `Tiers` and `Cascade` slices to non-nil empties, so an empty resolve serializes as `[]`; and `RoutingView` adds `?? []` guards on `user_tiers` / `tiers` / `cascade` so a single bad tier can never blank the page again. A regression test asserts an empty cascade decodes to a non-nil slice (verified failing on the old code).

**Built with `force_full` on purpose.** Like v1.52.1, this Web-UI-touching patch was cut with the full pipeline so it publishes the deployable `denngubsky/loomcycle` image with the fixed UI embedded — a lean browser-patch would ship only `loomcycle-browser`, which the standard deployment does not consume.

Adapter versions are unchanged — no adapter surface was touched.

## What's in v1.53.0

**Ontology curation: an agent can suggest entity types, and only an operator can accept one.** Minor — a new inert entity status, two operator actions, one narrow authoring op, a bundled curator agent, and a refreshed MCP tool surface. No schema change; one behaviour change for agents that were writing the ontology document directly (below).

**The problem.** The ontology had a single gate — the root chunk's `draft`/`confirmed` status — so anything appended to a confirmed document was live on the next run. There was nowhere to put a suggestion that is real but not in force, which meant an agent could not *suggest* a type, only add one. Separately, overriding a standard type meant reading its field names off the panel and retyping them by hand.

**A proposal is the entity in its final place, switched off.** `chunks.status` was already a column the ontology reader ignored, so `proposed` and `rejected` are now reserved as inert: such a chunk is not an entity, not rendered into any prompt, and not expanded in retrieval — but it *is* reported to the operator, in its position in the tree, with its fields and its body. Accepting clears the status in place, so an accepted subclass lands under the parent it was filed under and inherits from it. Nothing is copied out of a staging area.

**Only those two words are inert, and that matters more than the feature.** Every other status — including one the build has never heard of — leaves the entity in force. The shorter inverse rule ("anything not blank or confirmed is inert") would silently drop types from any document where an operator had used the status field for their own purposes, which is exactly the failure the chunk-tree reader was written to remove. Children of an inert chunk re-parent to the nearest in-force ancestor for the same reason: rejecting a parent must never switch off a live type beneath it. A **rejection is kept** as a tombstone, so an automated curator can see what was already turned down instead of re-proposing it.

**Adopt a standard type.** One action copies a standard type into the document with its fields, so it can be extended or subclassed without transcription. The fields come from the seed **server-side** — a client-supplied list could disagree with the type it claims to copy, and the document would look like an override of `event` while declaring something else. It records provenance (what was copied, from where, at which build) in the chunk's structured fields, never its body: adoption **freezes** your copy, since an override replaces the standard term wholesale, and without provenance that is undiscoverable later.

**An agent may propose, never decide.** On the tenant ontology document alone, a tool call from a run may only add a chunk whose status is `proposed`. Update, delete, move, reorder, supersede, `import_md`, `import_canvas`, `delete_document` and `set_path` are all refused there — the last two reach the whole ontology, `set_path` by re-homing `/memory/ontology` so the reader resolves somewhere else. The operator/agent distinction is a server-stamped context marker with **no wire form**: the off-run path also carries a synthetic agent id, and `POST /v1/runs` accepts a caller-supplied `agent_id`, so keying on that would have let a run claim to be the operator.

**`propose_entity`** is the front door: it resolves the ontology itself, takes the parent **by name** (the names the agent was given in its prompt — it cannot know a chunk id), stamps the inert status so the contract cannot be spelled wrong, bounds the evidence body, and refuses a name already in force, already proposed, or already rejected. It needs **no tenant grant**, deliberately: a suggestion cannot change what any run is told, so requiring the authority live authoring needs would mean only an agent that could already author live types could offer one.

**The bundled `memory/ontologist`** does a pass over one user's stored facts and files suggestions with evidence — counts and example titles — reachable from Settings → Ontology by a link that *stages* the run in the terminal rather than starting one from a settings click. On demand, not scheduled: a curator filing proposals nobody reads is clutter with a cron job. It is one agent definition; no new runtime primitives. Measured on a local model it is useful but not reliable, and its value depends on facts carrying types at all — measure your extraction model's type-emission rate before scheduling a pass.

**The MCP tool surface was refreshed, and had two problems.** The Document description was 13 ops behind the tool (`query_documents`, `list_facts`, the tag ops, the history ops, `backlinks`, `related`, `unlinked_mentions`, the canvas ops) — for an MCP client the description *is* the documentation, so an unlisted op is one no model will call. And 18 internal design-document citations had accumulated, 13 inside `Description` strings that go in front of every client's model. Both are now enforced by tests. The Claude Code plugin's reference is updated to match (plugin PR #26); it ships no tool schemas of its own, since the thin client proxies this `tools/list`.

**⚠️ Behaviour change.** An agent that was writing the ontology document directly will now be refused everything except filing a proposal. An MCP session is an agent surface, so an operator driving the thin client can no longer flip the ontology's confirm status either — the Web UI and `POST /v1/_ontology` remain the operator path.

Adapter versions are unchanged: `@loomcycle/client` 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/memory-view` 0.1.0. `@loomcycle/explorer` 0.7.0 publishes on its own `explorer-v0.7.0` tag, carrying the `+ child` nesting fix from v1.52.1 to external consumers.

## What's in v1.52.1

**Patch: the ontology was not editable through the UI, for two independent reasons.** v1.52.0 shipped RFC BZ's reader, inheritance, retrieval expansion and panel tree — and no working path for the operator who owns the taxonomy to author one. Both bugs were reported from a real deployment.

**The document viewer could not nest anything, anywhere.** `+ text` inserts a SIBLING (`after_id`), which is right for prose flow and wrong for structure — so a hierarchy could only ever be created by `import_md` or by an agent. Selecting a type and adding a sub-item put it BESIDE that type at the top level. A new **`+ child`** action passes `parent_id`, appending the new chunk under the selection and opening the editor on it so it can be named immediately (a type's name is its heading). `+ text` keeps its sibling behaviour; the tooltips now say which is which.

**And the Settings panel's "Edit ontology →" link never worked.** It pointed at `/documents/<id>` with no scope. The ontology lives at *tenant* scope and the viewer folds an absent scope to `user`, so the link opened the right document id in the wrong store, the read 422'd, and the operator got "No chunks" and no create buttons at all — indistinguishable from "editing is not possible here", and most of why the UI read as unusable. The URL is now built by a tested helper rather than an inline template literal, because a dropped query param is exactly what nobody notices. This was the only `/documents/` deep link in the codebase, so no other surface carried the same defect.

The panel also now names the steps — select a type, then `+ child` — which is not something anyone can guess.

**Release note for operators:** this is a Web-UI fix, and the Web UI is embedded in the binary and the `denngubsky/loomcycle` image. The patch build tier publishes only `loomcycle-browser`, so this tag was released with the `force_full` dispatch to produce the full artifact set (binaries, Homebrew, and every image variant) — the escape hatch the release workflow documents for exactly this case. The RFC BR sandbox sidecar images are unchanged and were not rebuilt.

Nothing else changed: no Go code, no schema, no wire surface. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0 and `@loomcycle/memory-view` 0.1.0. The `+ child` fix reaches the Web UI through the package source (Vite alias); publishing it to external `@loomcycle/explorer` consumers needs an `explorer-v*` bump and tag.

## What's in v1.52.0

**RFC BZ: the tenant ontology gets classes and subclasses — one chunk is one entity, and a child chunk is a subclass.** Minor — no schema change and no wire-breaking change, but retrieval returns strictly more for a hierarchical ontology, and a bug fix makes types live that were silently inert. Read the upgrade note below before confirming an ontology you had already nested.

**The bug it fixes, and why it needed a new reader.** An operator who organised their ontology into a hierarchy — the natural move in a document UI — lost every nested type. `ParseOntologyMarkdown` matched only `"## "`, so a `### incident` was recognised as neither a term nor a title and was dropped: no error, no warning, nothing in the Settings panel. Their document read correctly and their ontology did not match it. "Also match `###`" cannot fix it, because the ontology was read from `export_md`, which renders chunk depth as `strings.Repeat("#", level)` — after flattening, a subclass's title and a heading inside a body are the same bytes, so the information needed to tell a nested TYPE from a nested COMMENT is destroyed before the parser sees it. The reader now walks the chunk tree, where `parent_id` *is* the hierarchy: the chunk's title names the entity, backticked bullets are its fields, and a child chunk is a subclass. Depth is capped at four levels and a deeper type is flattened onto the cap rather than dropped — silently discarding an entity is the bug being fixed, and a cap that did it one level down would be the same failure.

**A subclass inherits its parent's fields.** `incident` under `event` carries `occurred_at` without restating it, transitively; a child redeclaring a field wins. An inherited field cannot be removed, which is correct rather than a limitation — a subclass lacking its parent's field is not a subclass, and someone who wants a type without those fields wants a sibling. Removal lives on the other axis, where a tenant term replaces a same-named seed term wholesale, so resolution runs *after* layering: a subclass of a type the tenant redefined inherits the tenant's fields, not the seed's. To subclass a *standard* type, declare it as your own root (which overrides the seed by name) and nest beneath your copy.

**The prompt renders a tree, and a flat ontology renders byte-identically.** Extraction now sees indentation plus one instruction — use the most specific type that fits — because a model handed a ladder with nothing telling it to climb sits at the top and the subclasses go unused. The tree form engages **only** when a hierarchy is actually present, and a dangling parent does not count: this string is in the system prompt of every extracting agent, so a whitespace or ordering change would invalidate provider prompt caches and shift extraction results for every deployment that never nests. That guarantee is asserted by test, not assumed.

**Retrieval expands subtypes — the payoff.** `list_facts` and `query_chunks` now match a type *and its subclasses*, transitively and downward only: `event` finds `incident` and `outage`, while `outage` still finds only outages. Expansion happens at query time and storage keeps the concrete type only, so re-parenting a type takes effect immediately and retroactively; materialising ancestors into each row would mean a correction silently invalidates every fact written before it. It applies **only when the ontology is confirmed** — a draft is inert everywhere else, and retrieval must not be the one surface where an unconfirmed edit changes answers — and the response reports `type_expanded_to` when the filter widened, because a wider answer with no explanation reads as a bug. `graph_recall` is not included: its seeds come from title text or explicit ids, so it has no type filter to expand.

**The Settings panel shows the taxonomy it now parses.** Both columns render as trees with field counts, declared fields plainly and inherited ones dimmed, plus three signals that were previously invisible: the depth cap says when it flattened a document, a leaf declaring no fields of its own is flagged (almost always a section chunk that silently became a subclass), and an awkward type name gets an advisory. Two columns are kept rather than merged into one tree — the gap between "this deployment defines" and "in force now" is the only visible evidence that a draft is inert.

**`preference` and `fact` are pinned as roots.** They may be *subclassed* — `dietary-preference` under `preference` is fine — but they may not be given a parent: that inverts the memory tier, and with subtype expansion a query for that parent would sweep in every preference the user ever expressed. Enforced by clearing the parent rather than rejecting the document, and reported in the panel. **Type names are warned about, never rewritten:** a name is part of a stored fact's key, so normalising `Notes on naming` after facts exist under that spelling would split the type in half.

**The extraction eval can now measure hierarchy use, and the measurement overturned the assumption behind it.** A new `specificity` ability, a hard type assertion, and a separate `--corpus hierarchy` fixture (kept separate so the shipped corpus digest — and its recorded baselines — stay valid). The RFC expected *over-specification*: given a ladder, a model reaching for `incident` where `event` was right. Across four runs of `ollama-local qwen3.6` it never once climbed past the evidence; what it does is decline to type at all on the subtype cases and fall back to the standard roots. Under-engagement, not over-reach. No baseline entry was recorded on purpose — four cases swinging 0.25–1.00 cannot support a 0.15 tolerance, and a flapping gate teaches everyone to pass `--no-gate`. Separately verified: the seed-only extractor prompt still digests to a recorded baseline's value, so nothing in this release invalidated the committed numbers.

**⚠️ Upgrade note.** The fix is a migration event. Types you nested and saw no effect from were inert; after this release they are live subclasses and they steer extraction, and a type filter returns strictly more than it did. Re-read your ontology in Settings → Ontology before confirming it — the panel now shows the tree it actually parses, so the change is visible in one screen rather than discovered through extraction results.

`/v1/_ontology` gains `parent`, `inherited` and `name_issue` per term and reports `notes` (a list) in place of the single-string `note` introduced earlier in this same unreleased line, so no released consumer saw the old shape. `@loomcycle/client` stays 1.51.0, `loomcycle` (PyPI) 1.46.0 and `@loomcycle/explorer` 0.6.0 — none was touched.

## What's in v1.51.0

**RFC BY: a user token can now read its own chats and discover the agents it may run.** Minor — one new endpoint, one additive `@loomcycle/client` method, and a widened-then-reconfined gate. No schema change, no wire-breaking change. Completes RFC BX's "user access = own + bundled + tenant-if-enabled" model on the read side.

**The gap it closes.** RFC BX gave a `substrate:user` token the run plane but no read surfaces. A delegated user could start and read its own runs and nothing else — it could not see its own past chats, and it had no way to find a runnable agent without being told the name out of band. Both surfaces were gated at `substrate:tenant`, which no delegated user token holds; and a **tenant-mode** user token holds neither `substrate:tenant` nor `substrate:user` (only *isolated* users carry `substrate:user`), so keying the gate on either scope would lock one user type out.

**A member-read gate, not a scope hole.** The user-read surfaces are gated on `runs:read` — the honest floor every such principal already holds (isolated users imply it, tenant-mode users hold it directly, tenant operators and admin sit above), so opening them grants no capability a user did not have. The security-bearing decision moves into the handler, keyed on the authenticated principal, never the wire.

**Own history is capped, not just admitted.** `/v1/_history` now admits a member token, and a delegated user's history scope is capped to `[self, user]` — its own chats, in both access modes — while a tenant operator keeps `[self, user, tenant]` and admin adds cross-tenant `global`. The owner subject/tenant is stamped from the principal, so `[self, user]` is exactly the caller's own history and nothing else. The cap is applied on both the HTTP and gRPC paths, so a member is confined identically on either transport. The TS `history()` method is unchanged — it already carried the caller's bearer.

**Discovery is tiered server-side by access mode.** A new `GET /v1/_runnable-agents` returns the agents the caller may run: bundled/system agents always (the shared floor), the tenant's shared agents only when the caller may use them, and own user-scoped agents (reserved — none today). An **isolated** token never sees the tenant's agents — a hard floor read from the token scopes, so a stale `access_mode` column can never widen it; a tenant-mode user is governed by its authoritative `users.access_mode` row. Entries are lean (`name` + `source` tier) — no operator metadata (version counts, retired badges, content hashes) and no system prompts; that stays in the `substrate:tenant` Library. A different tenant's agent is never leaked.

**`@loomcycle/client` 1.51.0** adds `runnableAgents()` (+ `RunnableAgent` / `RunnableAgentsResponse`). Additive; existing callers unchanged.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched. The gRPC/MCP discovery twins and a longer-term scope-hierarchy cleanup are noted as follow-ons in the RFC.

## What's in v1.50.0

**RFC BX Phase 2 follow-ups: an isolation invariant made structural, and the delegated-users API reaches `@loomcycle/client`.** Minor — additive adapter surface, one defense-in-depth runtime stamp, one CI deflake. No schema change, no wire change.

**The isolation bit is now stamped where it was merely unreachable.** RFC BX Phase 2 confines an isolated member (a `substrate:user`-topped principal) to its own data scope through `RunIdentity.Isolated`, which `ConfineIsolatedScope` reads to refuse tenant/global scopes. Every HTTP run-start site stamped it; the MCP-direct path (`mcpPrincipalCtx`) and the two gRPC substrate paths (`substrateGRPCCtx`, `substrateGRPCUserCtx`) did not. That was safe — but only because all three gate on `substrate:tenant`, a scope an isolated token cannot hold, so no isolated principal ever reached them. Safe-by-unreachability is a coupling between a route gate and a confinement in a different file: widen the gate and the confinement disappears silently. It is now stamped from `auth.IsIsolated(principal)` at all three sites, so the property holds by construction rather than by the current gate. Behaviour is unchanged today; the regression tests fail on the unstamped code.

**`@loomcycle/client` 1.50.0 gains the user + token management surface.** The RFC BX Phase 2 routes — `/v1/_users` CRUD and `/v1/_users/{subject}/tokens` mint/list/revoke — shipped with a Web UI console but no adapter method, so a programmatic operator had to hand-roll `fetch`. Added `createUser` / `updateUser` / `deleteUser` / `mintUserToken` / `listUserTokens` / `revokeUserToken` and their types, mirroring the Web UI client. The tenant is server-derived from the bearer, so none of them send one; the mint result carries the plaintext bearer exactly once. These are HTTP-only admin routes — the Python adapter is gRPC-only and there are no gRPC user RPCs, so it is unchanged.

**A CI flake removed.** `TestProviderGate_ZeroOverheadWhenUnconfigured` asserts five runs overlap in the provider (`peak > 1`) using a bare 20 ms delay, which a loaded runner could stagger into a false `peak=1` — it failed exactly this way on a recent `Go 1.26.x` job. It now uses the harness's existing `holdUntil` rendezvous so the observed peak is deterministic; a genuine under-admit still fails (the 2 s safety valve fires, peak stays < 2).

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.49.1

*Memory search: the source filter is a visible dropdown.*

Patch. UI-only — no wire, adapter, or runtime API change. The TS adapter stays at
1.49.0, so its publish is correctly skipped (nothing to republish).

The RFC BW `source` field in the @loomcycle/memory-view search panel (shipped in
v1.49.0) rendered as a bare <input list> + <datalist>: a plain empty text box
whose facts / notes / documents choices only surface on focus, with a
subtle/browser-dependent dropdown affordance — so operators read it as an empty,
non-functional field. #961 replaces it with an explicit combobox: a text input
with a visible ▾ chevron that opens a real option list (all sources / facts /
notes / documents) on click, closing on outside-click or Escape, and still
free-text editable (an unknown value is dropped server-side). Same `sources`
selector on POST /v1/_memory/search — only the control's presentation changed.

@loomcycle/memory-view is versioned on its own memory-view-v* tag (0.1.0,
unpublished); this runtime tag does not publish it.

## What's in v1.49.0

**🎯 Targeted memory search: ask for facts, notes, or documents by name.** RFC BW, all three phases, plus the `@loomcycle/memory-view` console package. One wire field changes value set — see the note below.

**The bug it fixes, measured.** A user asked their chat agent *"remind me which medicine do I use"*. Seven of ten `recall` hits were document chunks, and the fact naming their medication ranked **fifth** — below a horizontal rule (`"---"`, 0.477), a shell fence (`` "```sh" ``, 0.452) and a bare `#` (0.451).

Two independent causes, both ours. Markdown scaffolding was being embedded: a heading-split import turns a fence line into its own chunk, and a short syntax token embeds near the centroid of everything, so it ranks mid-high for *every* query — it does not waste a row, it buries answers. Those are now rejected, by one predicate covering both a chunk's body and its title fallback (a first attempt rejected the body and then fell through to an equally scaffold-ish title, moving the noise rather than removing it).

The structural cause was worse. **RFC BU §6 promised that `recall` does not reach documents** — and that held only because chunk bodies had no embeddings. RFC BU phases 1–2 embedded ~2,900 of them into the same per-scope vector plane `recall` searches, so the guarantee went false without a line of `recall` changing. The RFC that declared the invariant is the one that broke it, which is the argument for putting the boundary in the API rather than leaving it emergent from whatever happens to be indexed.

**A named selector, not another reserved string.** The reported failure was an agent not knowing that document bodies live under `doc.chunk:`, so answering it with a second magic string would have answered the symptom:

```
Memory op=recall scope=user query="which medicine"          → facts + notes (new default)
Memory op=search scope=user query="…" sources=[documents]    → document text
Memory op=search scope=user query="…" sources=[facts]        → distilled facts only
```

`prefix` survives as the escape hatch and composes as an AND. **`recall` now defaults to memory rather than everything** — that is the fix, not a side effect; an operator wanting the old behaviour passes all three sources explicitly. `search` still spans every plane by default, because `/v1/_memory/search` exists to answer "where did I record this" and narrowing it would break what v1.47.0 shipped.

**Facts vs notes is decided by `origin`, not `class`.** Both are provenance columns, but `origin` is stamped by the server from the writer's identity while `class` is model-supplied — so keying "is this a consolidated fact" off `class` would let an agent promote its own note to a fact by labelling it. Legacy rows predating the column count as **notes** rather than vanishing from both halves of the split.

**Two source combinations are refused rather than approximated.** `documents` together with only one of facts/notes needs a disjunction across two independent dimensions. It could be built and deliberately is not, because the alternative that matters is what happens instead: silently widening to "everything" hands back rows the caller excluded *while it believes the filter applied*. The error names the supported sets.

**A backend that ignores the selector must say so.** `sources_applied: false` is surfaced with a note, and false is the zero value on purpose — an external memory-layer service that drops the selector must not be able to do so silently, because the caller would then trust a label the result does not deserve. Recall is where it matters most: its default excludes documents, so a backend that ignored it returns exactly what the default exists to keep out.

**The filter runs in SQL, and that was forced.** The candidate pool is capped at 51 rows, so on a scope with ~2,900 chunks against ~42 facts a post-filter would leave a caller asking for ten facts with two.

**Measured before shipping**, because the RFC made it a gate: excluding documents on a 2,942-row scope is **~31× faster**, not slower — 30 ms against 928 ms. The reason matters more than the number: migration 0017 deliberately creates no ANN index, so there is nothing to degrade and a predicate cutting 2,942 candidates to 42 cuts the work proportionally. The risk returns if an operator opts into HNSW, which 0017 invites, so the regression assertion stays.

**Also: the Memory tool now teaches itself.** All three `chat/*` agents pointed at `Document graph_recall` for "what do we know about X" — which walks out from facts carrying entity metadata and returns `seeds: 0` on a store without them, reading as "nothing is remembered". Nothing named `Memory op=recall` or its required `scope`+`query`, so an agent guessed its way there through three missing-field errors. The prompts and the `memory-layer` help topic now name the invocation first and position `graph_recall` as the second step.

**New package: `@loomcycle/memory-view`** — the operator Memory console as a reusable React component (scope/scope_id/key browser, entry editor, fact viewer, unified search panel, reembed flow), scoped under `.loomcycle-memory-view`. The Web UI consumes it from source. Published on its own `memory-view-v*` tag; **0.1.0 is not published yet.**

### ⚠️ Wire change

`kind` on `POST /v1/_memory/search` was `"memory" | "document"` and is now **`"fact" | "note" | "document"`**. A reader switching on `"document"` is unaffected; one asserting `kind == "memory"` must move to `fact`/`note`. The refinement is the point — "the user told me this" and "an agent jotted this down" are different claims, and collapsing them is what let document prose read back as remembered fact. `@loomcycle/client` **1.49.0** moves in lockstep.

### Upgrade note

Existing deployments benefit immediately for new writes. Scaffolding rows already embedded stay in the index until re-swept; `POST /v1/_memory/backfill_embeddings` does not remove an existing embedding, so clearing them means deleting those chunks or re-embedding the scope.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched.

## What's in v1.48.0

**🔖 Heading chunks became searchable, and `@loomcycle/client` ships the memory-view SDK.** Minor: one embedding-policy change and three additive adapter methods. No schema change, no wire/proto change.

**A bodyless chunk now embeds its TITLE.** Every embedding policy derived text from the *body*, so a chunk whose body yielded none was excluded from semantic search altogether — which silently removed the most navigable part of a document from retrieval. On the reference deployment **186 of ~3,000 chunks are heading-only**, with titles like `RFC BE — History Tool (browse / search / rename / annotate past chats)` and `Phase 2 — name-links + transclusion`.

The rule applies uniformly after the per-type switch: when the derived text is empty, fall back to the title. It is a **fallback, never an addition** — appending it would double-weight whatever the author put in the heading on every chunk in the corpus, and for prose the body usually restates the heading anyway. Being uniform, it also covers a diagram whose labels were all syntax and an image with neither caption nor description, which would have made the deployment's two images findable before any vision call.

The only quality test is **"contains a letter"**, not a length. Measured before building: of 20 sampled bodyless chunks 18 had meaningful titles, and the shortest were `Active RFCs` (11 characters) and `Configuration` (13) — a length filter would discard exactly what someone searching a document would type. `---`, `42`, `1.2.3` carry no language and are dropped, so no vector is stored for them.

The admin backfill applies the same fallback, or existing documents would stay permanently less searchable than ones authored after this landed — and a bodyless chunk is unreachable any other way, since the sweep sees memory *rows* while a title lives in SQL Memory. Both surfaces route through one judgement so they cannot disagree.

**`@loomcycle/client` 1.48.0** adds three HTTP-only methods backing the memory view: `memorySearch()` (the off-run unified search over k/v entries and document-chunk bodies from v1.47.0, each hit tagged `kind: memory|document`), `memoryEmbedStats()`, and `reembedMemory()` — whose `dryRun` stays a safe dry run when omitted, since the server defaults it true. Fact reads (`list_facts`, `get_chunk`'s `entity` block) ride the existing `document()` passthrough rather than gaining bespoke methods. Additive: existing callers are unchanged.

**Why npm skips 1.47.0.** The adapter's version was bumped to 1.47.0 in the same window that tag was cut, and a publish only fires when the package version equals the tag — so `@loomcycle/client@1.47.0` never existed on npm. It is realigned to 1.48.0 here, and npm goes 1.46.0 → 1.48.0. Nothing is missing; there was never a 1.47.0 package.

`loomcycle` (PyPI) stays 1.46.0 and `@loomcycle/explorer` 0.6.0 — neither was touched; the SDK methods are TS-only because the endpoints they wrap have no gRPC twin.

## What's in v1.47.0

**🔎 A human-facing memory view, the memory surfaces opened to tenant operators, and the last of the RFC BU sweep fixes.** Minor rather than a patch because RFC BV Phase 1 adds new surface — one HTTP endpoint and two Document ops — and the `/v1/_memory/*` family changes who can reach it. No schema change, no wire/proto change, no adapter change.

**RFC BV Phase 1 — reading the memory plane like a human would.** The entity tier stores a fact as a chunk plus a `chunk_memory_meta` sidecar (bi-temporal timelines + provenance), but nothing could *read* that sidecar in a typed way: `get_chunk` returned a chunk with no way to tell a fact from a plain section, and nothing enumerated facts for a browse surface.

- `get_chunk` now attaches an **`entity`** block when the chunk has a sidecar (omitted otherwise): raw unix-nanos timestamps, so the viewer formats them rather than the server guessing, and an always-present `retired` bool keyed on *system* time (`expired_at`) so a future world-time `invalid_at` is not misreported as retired.
- **`list_facts`** browses the scope's facts newest-first, metadata only — the viewer fetches a body on click — filterable by type/class/document_id and using the same temporal filter `graph_recall` does. Its per-fact `entity` block reuses the same renderer, so the two surfaces cannot drift.
- **`POST /v1/_memory/search`** is an off-run semantic search with an empty key prefix, so one query spans both plain k/v entries and document-chunk bodies — what answers "where did I record this" across the whole stack. Each hit is tagged `kind=memory` or `kind=document` (+`chunk_id`) so a document hit can be followed to `get_chunk`.

  Security-critical detail: the in-process backend resolves the tenant from the *run* identity, and off-run there is none — so the handler stamps the authenticated principal's tenant before searching. Without it the search would run at the shared `""` tenant and could read another tenant's rows. `TestMemorySearch_TenantIsolation` fails if the stamp is removed.

**The `/v1/_memory/*` family is reachable by tenant operators.** It was pinned to `substrate:admin` by the `/v1/_*` catch-all, so a `substrate:tenant` operator 403'd at the gate — even though memory rows carry a `tenant_id` and every handler already sources the tenant from the authenticated principal rather than the wire. The same gap the Library had in v1.6.3: the handler confines, but the route never lets a tenant token reach it. The routes now grant `ScopeTenant` and the handlers still confine a non-admin to its own tenant, with `TestHandleMemory_TenantOperatorConfined` proving a `?tenant=` naming another tenant is ignored — the invariant the re-gate rests on.

**`repair-tenant` deliberately stays operator-admin**: it rewrites rows across every scope in one statement to re-stamp the legacy `""` partition, and a cross-tenant bulk rewrite is not a tenant operator's authority. The Web UI's memory nav item moves from admin to tenant with it; **channels stays admin**, because it still has no tenant column — the reason memory used to be admin too.

**An uncaptioned image could never be embedded.** Found by verifying the v1.46.1 deploy rather than trusting its output: the describe pass reported `described=2 failed=0` with two accurate descriptions persisted, and the images stayed unsearchable while the backfill's candidate count never moved.

`embedBody` guarded on the **raw body** before the per-type switch — and an image's body *is* its caption, so an image with no caption returned early and its generated description was never consulted, no matter how many times a describe pass wrote one. The body is only one of the sources for an image, and may not be a source at all.

This is the failure mode `SetAssetDescription`'s own comment warns about, reached by a different route: `get_asset` shows a description, the sweep reports success, the row holds 472 characters — and nothing is indexed. Correct-looking from every surface an operator would check. Both existing image tests used a *captioned* chunk, which is why it went uncovered, and an uncaptioned image is the common case for an uploaded asset. The fix derives the text first and checks it after; prose and diagram behaviour is unchanged.

**Upgrade note.** If a describe pass already ran on v1.46.0/v1.46.1, its descriptions are persisted and need no second vision call — rewriting each image chunk's body re-enters the embed path and indexes it. The embedding backfill *cannot* do this: it reads the chunk-body envelope from the memory plane and has no view of `chunk_assets`, so an uncaptioned image is invisible to it.

Adapters unchanged: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.1

**🩹 Both v1.46.0 upgrade sweeps were broken in practice.** Patch release — no new surface, no schema change. Found by running the sweeps against a real deployment (154 documents, 3,143 chunks, 2 images); neither fault was visible from the unit tests, because each needs scale or a real model to appear.

**The embedding backfill starved instead of finishing.** Throughput decayed 189 / 179 / 169 / 160 / 147 embedded per 200-row window, heading for zero with ~2,300 rows still unembedded. `limit` bounded how many rows were *looked at*, not how many were embedded — and a row with no body text (a document root, a section heading) can never gain an embedding, so it stays a candidate forever. Because the query is `ORDER BY key`, those rows accumulate at the *front* of the window: the residue obeys `R' = R + p·(limit − R)`, whose fixed point is `R = limit`. Eventually the window is entirely rows that cannot be embedded and the sweep does nothing, while its own notes promise "re-invoke until candidates reaches 0" — a state it can never reach. `MemoryEmbedListMissing` now takes a keyset cursor and the handler pages with it until it has *embedded* `limit` rows, reporting `skipped_empty` and `more`.

Widening the window did not help either: the store reset a limit over 1000 back to the 200 default, so asking for 5000 returned a **smaller** page than asking for 1000. That is now a clamp.

**The describe pass truncated before it said anything.** The image sweep reported 0 failures and both images as answered-empty. The model was fine — asked directly, `qwen3.6` describes the image in 198 characters. The token ceiling was 300, and a thinking model emits its reasoning trace *first*: 1281 characters of it, `done_reason=length`, and no description at all. 300 looked ample because the same prompt answers in 84 tokens with thinking off.

Worse than a failed call: the pass recorded it as answered-empty and stamped `described_at`, whose whole purpose is to separate "a model looked and found nothing" from "nothing has looked yet" — so a code bug became a permanent fact about the data that no re-run would revisit. A turn that stopped at the ceiling is now a **failure**, left retryable, with the reason named. The ceiling is 1500, and it is a ceiling rather than a target, so a generous value costs nothing on a normal call. Deliberately *not* fixed by forcing `effort: low` (which the Ollama driver maps to `think:false`): that flag errors on a model which cannot reason, so it would break a non-thinking vision model such as llava to accommodate a thinking one.

**Also:** an admin token that names no tenant on the backfill is refused with `400 tenant_required`, mirroring the erasure, directory and orphan-repair surfaces. Memory rows are keyed on the tenant, so omitting it resolved to the *default* tenant and reported a truthful-looking `candidates: 0` against a tenant the operator never meant — demonstrated on a three-tenant deployment, where the same request with and without `?tenant=` returned 200 candidates and 0.

**Upgrade note.** If you already ran the v1.46.0 sweeps, re-run the backfill — it will now finish rather than stall. Images stamped by the buggy describe pass need clearing to become candidates again:

```sql
UPDATE chunk_assets SET described_at = NULL
 WHERE described_at IS NOT NULL AND coalesce(description,'') = '';
```

Adapters unchanged from v1.46.0: `@loomcycle/client` 1.46.0, `loomcycle` (PyPI) 1.46.0, `@loomcycle/explorer` 0.6.0.

## What's in v1.46.0

**📄 Documents became a searchable, linked knowledge store.** Two RFCs completed end to end — **RFC BS** (structure primitives: tags, links, transclusion, history, discovery, canvas) and **RFC BU** (searchable bodies: embed-on-write per chunk type, backfill, diagram and image handling) — plus a **directory** surface on every transport. No schema migration on the main store; the document scopes self-provision their own tables.

**Documents are now findable by their content.** They were not, and the reason was structural rather than a bug: the searchable half of a document (the SQL `chunks` table) has no body column, while the half holding the text (`doc.chunk:<id>` rows in Memory) had no index — `writeBody` called plain `MemorySet`. The keyword leg did not rescue it either, since full-text ranks a column on the *embeddings* table, so an unembedded row was invisible to both search paths. Neither component was faulty; the gap was in the seam, which is why neither one's tests could surface it. Chunk bodies are now embedded on write, and `memory op=search` with prefix `doc.chunk:` is the entry point.

What gets embedded is **per chunk type**, and the rule that fell out of it is worth stating: *use a model only when the content is not already text.*

| chunk type | embedded text | model? |
|---|---|---|
| prose | the body verbatim | no |
| `mermaid` | extracted node/edge labels + the diagram kind | no |
| `image` | the author's caption + a persisted vision description | for the description only |

Mermaid is text pretending to be a picture; an image is a picture. A diagram's labels are extracted deterministically across ten dialects — and the extractor is two passes, because a first version using label-shape regexes alone silently lost the content of half of them (`erDiagram` entity names are bare identifiers, `gantt` task names sit *before* the colon while the metadata follows it, `mindmap` nodes are bare indented words). An unrecognised diagram type degrades rather than vanishing.

For images, the **caption is embedded on write with no model at all**, so an image is searchable the moment it is written. A vision description is generated by an explicit operator pass (`POST /v1/_document/describe_images`, tier-resolved, dry-run by default, resumable) and **persisted** rather than regenerated: a description is model output, so regenerating yields different text, and an index that silently re-ranks on every re-embed is worse than one that is merely stale. Persisting it also makes it auditable and survives an embedder swap without a second vision call per image.

**Prose became a graph.** A chunk body's `[[name]]` links are materialised as `references` edges, re-derived on every body write and resolved through the Path dirent tree; `![[target]]` **embeds** are expanded inline at export time (transclusion), degrading to the literal text on a cycle, a depth cap or an unresolved target — a rendering nicety must never abort an export. Plus the discovery half: **`backlinks`** (what links here, manual and parser edges alike), **`related`** (semantic neighbours, a straight reuse of the new body embeddings), **`unlinked_mentions`** (chunks that name a target without linking to it), and a per-chunk **body-change log** with `history` / `get_version` / `diff`.

**Tags and document-level type/status** are first-class query axes now, in their own SQL join tables. A chunk's `fields` live in a Memory k/v blob, unreachable from SQL, so a tag stored there could never be queried — and "every draft RFC" previously meant querying root chunks. **JSON Canvas** import/export round-trips a document to the open spatial-graph format Obsidian Canvas uses.

**A directory surface**, on HTTP, MCP, gRPC, TS and Python: `users`, `inspect`, `tenants`. Read-only and derived — there is deliberately no create or update, because a "user" here is not stored (`ListUsers` is a `GROUP BY` over `runs.user_id`), so "user CRUD" has no create/update half and its delete half is already the subject-erasure surface. What was missing was the *read*: answering "what does alice actually have here" meant five calls against five surfaces, each with its own tenant-scoping rule, where getting one wrong yields not an error but a plausible number from the wrong tenant. Listing tenants is admin-only and **refuses** rather than filtering, because a filtered list still confirms the caller's own tenant in a shape indistinguishable from "you are the only tenant here".

**Two bugs worth calling out**, both found by building on the code rather than by review:

- A mermaid chunk was embedded as **raw diagram source** when created natively, while the identical diagram arriving through `import_md` was skipped. The classifier recognised only the fenced form, but a mermaid chunk *stores its bare source* — so the common path was the broken one. Behaviour now branches on the authoritative chunk type, since no content predicate can separate "a diagram" from "prose that opens with the word pie".
- The describe route calls `provider.Call` **directly** without the credential-override path, exactly like the LLM gateway — so without the same refusal it would have been a cost-isolation bypass, letting an operator-key-restricted tenant spend the operator's key. It now returns 403 `operator_key_restricted`.

**Also:** the memory extractor runs at `effort=low` **in the copy that actually ships** (the fix was in a bundle file that was not the embedded one), and an `effort=medium` evaluation baseline is recorded under the current prompt.

**Upgrade notes.** Two one-time sweeps make *existing* content searchable, and neither runs automatically by design — thousands of model calls at boot is a bill nobody approved:

```
POST /v1/_memory/backfill_embeddings?scope=user&scope_id=<subject>&prefix=doc.chunk:&dry_run=false
POST /v1/_document/describe_images?scope=user&scope_id=<subject>&dry_run=false
```

Both are resumable with no cursor: an embedded row or a described image drops out of its candidate set, so re-invoke until `candidates` reaches 0. Every chunk write now makes an embedder call — best-effort, so a cold or absent embedder logs and moves on rather than failing an author's write. Expect per-scope vector growth roughly equal to the chunk count, which brings the existing per-scope quota closer.

**The shared document viewer surfaces all of it.** `@loomcycle/explorer` gains read-only tag chips, a **Connections** panel with three lazily-fetched lists (backlinks — marking which edges came from `[[name]]` parsing — plus related and unlinked mentions, the three reads under `Promise.allSettled` so one failure never blanks the others, and a missing embedder showing a muted "not configured" note rather than an error), a **History** modal with the revision list, one revision's exact body and a unified diff, and a **Download .canvas** button beside Download .md. Board, kanban and spatial views stay a loomboard concern.

Adapters: `@loomcycle/client` **1.46.0**, `loomcycle` (PyPI) **1.46.0** — the Python adapter had been pinned at 1.38.0, so this release publishes its erasure and directory methods — and `@loomcycle/explorer` **0.6.0**.

## What's in v1.45.0

**🧹 Subject erasure on every transport, and a nine-pass audit of the memory subsystem.** One new wire surface; the rest is fixes. Additive index only (0065, from v1.44.0); no schema or wire break.

**Erasure is no longer HTTP-only.** v1.44.0 shipped it as the one substrate surface without transport parity; it now runs over MCP (an `erasure` tool, `op=report|execute`), gRPC (`ErasureReport` / `ErasureExecute`), the TS client (`erasureReport()` / `erasureExecute()`), and the Python client. The logic moved into a shared service so all four run the SAME code — an erasure that removed a different set of planes depending on which client asked would make "we erased them" a claim about a library rather than about the data. **The safety guards moved with it**: requiring `confirm` to equal the subject is a property of the erasure, not of HTTP, and leaving it to each caller would mean the newest transport is the one missing it. On MCP and gRPC the tenant comes from the principal with no wire field at all, so a session cannot reach another tenant's subject.

**Then nine review passes over the memory subsystem, which found fourteen defects.** Every one sat in state reconstruction, tenant keying, or reachability — none in transport, dispatch or gating. The ones an operator should know about:

- **A single-tenant deployment's erasure silently spared the subject's entire SQL Memory database** — every document they authored and their whole entity graph. SQL Memory rejects an empty tenant and stores the default one as `"default"`, while the k/v plane keeps the raw `""`; the drop key was built from the raw value and matched nothing. The live deployment test could not have caught it: its tenant was non-empty, where the two spellings coincide.
- **The correction chain could fork.** Nothing stopped two chunks from each superseding the same fact — both stayed live and `graph_recall` returned both as current, which is exactly what supersede-not-delete exists to prevent.
- **A known future end date deleted the fact.** "The contract runs until 2027" vanished from every default recall the moment the end date was written, because the filter required `invalid_at IS NULL`. The decisive argument is internal consistency: the default *is* "as_of now", so it must reduce to the `as_of` predicate with the current time substituted — and it did not.
- **`import_md` read fenced code as document structure.** Any document containing a Markdown sample re-imported with more chunks than it had, its body truncated at the fence. That is most technical documentation, including this project's own RFCs.
- **`create_chunk` / `move_chunk` accepted a parent that does not exist**, returning success for a chunk nothing could ever reach — absent from `get_document` and `export_md`, and invisible to the dead-link sweeper, which looks for a missing *document* rather than a missing *parent*.
- **Vector search died instead of degrading in a mixed-dimension scope** — the state migration 0017 calls the typical steady state mid-migration. One row at another dimension aborted the whole query with a raw driver error, so recall was dead for that scope until every row was re-embedded.

**Three hardening fixes where a guarantee held only by accident.** The postgres schema and LOGIN role for a SQL Memory scope are derived by hashing a separator-joined key, and the separator-free requirement was asserted in a comment rather than enforced — three distinct scopes could derive one schema and one role. It was unreachable only because the tenant component happens to come first. The SQL validator's comment-stripper was dialect-blind, so a legitimate `VALUES ($$x; y$$)` was refused while `$$--$$` hid a real statement separator — safe only because the driver's extended protocol rejects multi-statement strings, a dependency nobody had written down. And a `MemoryBackendDef` could name any env var as its credential source: `api_key_env: LOOMCYCLE_AUTH_TOKEN` beside a def-supplied `base_url` is a one-request exfiltration of the operator bearer, and the def persisted holding it.

**⚠️ Two CI gaps, and the second explains several of the above.** The pgvector round-trip contract **never ran** — it is gated behind an opt-in flag the workflow never set, even though the store job's service image is already pgvector. A skipped suite is worse than a missing one: it reports green and reads as covered. It could not have run even with the flag, because the per-test schema excluded `public` and migration 0017's `vector` type is unqualified. Both are fixed, and the postgres-tier `-run` filter — which the workflow *asks in writing* to be maintained — is now enforced by a test rather than a comment.

**🔬 One eval finding.** A change to the extractor's system prompt **un-gates every measured configuration**, invisibly: the baseline keys on the prompt hash, and no match is deliberately not a regression. `effort=medium` has been ungated since the prompt changed. The gate now says so instead of reporting a clean pass with nothing behind it.

**Adapters:** `@loomcycle/client` **1.45.0** publishes on this tag with the erasure methods. The Python client's erasure methods ship on its own `python-v*` tag.

## What's in v1.44.1

**🩹 Patch — the erasure report omitted planes it had examined.** Reporting accuracy only; nothing about what gets deleted changes.

Found by driving v1.44.0 against a live deployment rather than a fixture. A dry run for a real subject returned `{"chats":2,"interrupts":0,"memory_rows":2}` — but `credentials`, `token_limits`, `path_entries` and `sql_memory_scopes` are all examined on that path, and all four were missing. An operator reading that cannot tell *"this subject had no credentials"* from *"credentials were never looked at"*, which in a compliance report is the entire question being asked. It is the same ambiguity between FAILED and EMPTY that the `errors` field exists to prevent, reintroduced one field lower down.

Two causes. The executor accumulated with `deleted[k]++` from an absent key, so a plane that removed nothing left no trace; every considered plane is now registered at zero up front, which establishes the invariant that **a key's presence means the plane was examined and its value means how many rows went**. And the report set `sql_memory_scopes` only when it found one, so absence meant either no scope or no look.

SQL Memory is the one genuinely conditional plane: with the subsystem unconfigured it really is unexamined, so rather than omit it the report now SAYS so in `notes`. Exactly one of the two must hold — counted, or declared unexamined — and the regression test asserts that as an exclusive-or rather than checking either alone.

⚠️ **No schema change, no wire change, no adapter change.** Existing `/v1/_erasure` responses gain keys; none are removed or renamed.

## What's in v1.44.0

**🧹 A subject erasure you can run, and — more to the point — one that tells you what it did not reach.** Two new endpoints, one new store method, one additive index. The memory bundle stays opt-in.

**`GET /v1/_erasure?tenant=&subject=` reports one person's footprint in three tiers**, and the tiers are separated by what they *guarantee* rather than by what they contain:

| tier | what | deletable |
|---|---|---|
| 1 | chats, user-scope memory, the user's SQL Memory scope, path entries | ✅ existing primitives |
| 2 | credentials, token limits, interrupts, usage ledger | ❌ nothing, before this release |
| 3 | facts about the subject in a shared agent's or another user's scope | ❌ not addressable at all |

Tier 2 is listed apart because the distinction is not row count but whether anything can remove them. A subject's **encrypted credentials** surviving an "erasure" is the worst entry on that list, and it is invisible unless something counts it.

**`POST /v1/_erasure` executes tiers 1 and 2, and defaults to doing nothing.** `dry_run` is a nullable boolean on purpose: an omitted field — or `POST {}` from a client with a buggy serialiser — must mean *do nothing*, and a plain boolean would make the zero value destructive. A live run additionally requires `confirm` to equal `subject`; a subject id that matches nothing is harmless, one that matches the **wrong person** is not. An admin token must name the tenant explicitly, because a subject id is only unique within one and an unnamed tenant would not merely report the wrong people — it would delete them.

⚠️ **The residue report is one-shot, and this is the property to understand before using it.** Tier 3 is reachable only by tracing provenance from the subject's chats, and the erasure deletes those chats. Afterwards the trace handle is gone: a report run later shows `residue: 0` **while the facts are still stored**. That is not a defect to be fixed at this layer — it is what "the subject's chats are deleted" means when chats are the only index into derived facts. So the residue is measured before anything is removed, the response states that **it is the only durable record of what was not reached**, and the report closes the matching lie from its own side by rendering tier 3 **UNDETERMINABLE** rather than `0` when there are no sessions to trace from. That is exactly the state a subject is left in after an erasure, and exactly when someone would read a zero as confirmation the job is done. **Retain the erasure response.**

**The usage/cost ledger is retained by design, and says so.** Cost rows are accounting records an operator may be legally required to keep; the correct treatment is to break the personal linkage rather than destroy the totals. That is a row merge rather than a delete — `usage_archive` carries `user_id` inside its primary key, so anonymising means folding rows into the empty-string bucket and summing — and it is left for its own change rather than swept in. It appears under `retained` with the reason, because an erasure that lists only its successes reads as complete.

**Deleting an interrupt is not filtered by status.** An interrupt row holds the model's question *and* the user's free-text answer, so removing only the pending ones would report success having left the conversation behind. The new `InterruptDeleteAllByUser` is also uncapped, unlike the lister it mirrors: a partial delete that reports success is the one outcome an erasure must never produce.

**🧹 Dead chunk references are now collected.** A chunk is referenced from five places, and neither `delete_chunk` nor the read-time guard reaches all of them — the body delete runs after the transaction commits (a different store, so it cannot join the txn), an out-of-band delete bypasses the tool entirely, and the read-time guard makes an orphan *invisible*, which is precisely why nothing ever notices it. This is always-on integrity rather than opt-in policy: everything it removes is unreachable by definition. Two guards refuse rather than proceed — a fault reading the live chunk set aborts the scope, and a scope holding bodies but zero chunk rows is treated as mis-resolved rather than fully deleted, because being wrong there costs every body in the scope.

**🔬 The memory eval never sent the ontology it was scoring.** `{{memory:ontology}}` reached the model as nineteen literal characters, which explains the v1.43.0 finding about entity types outside the ontology: the model was never shown one. The A/B rerun settles it — **a confirmed ontology eliminates statement-class-as-entity-type entirely (5 → 0)**, retiring the type-validation follow-up that release proposed.

⚠️ **One additive migration (0065)** — a partial index on `memory(tenant_id, source_session_id)`. No wire change, no adapter change. The erasure surface is **HTTP-only in this release**: there is no MCP tool, no gRPC RPC, no adapter method and no Web UI page, unlike every other substrate surface. An operator drives it with `curl`.

## What's in v1.43.2

**🔎 The entity graph was correct and unfindable.** The first live consolidation pass built a graph, and inspecting it showed why the retrieval half was unusable. Runtime + bundle; no migration, no wire change.

**`graph_recall` searched every chunk in the scope, and the scope is where the document store lives.** Measured on the reference deployment: **2 entity chunks among 3,071 chunks across 150 documents**. So "what do we know about X" answered with documentation prose. Discovery now requires a `chunk_memory_meta` row — a chunk without one is prose, not part of the graph — and it is restricted for DISCOVERY only, never for explicit `seed_ids`, which exist precisely to hand in results found some other way. That is the same division the temporal filter already draws.

This is the failure mode a fixture corpus cannot show. Every test corpus is a clean room; signal-to-noise is a property of the corpus rather than the code, and 2-of-3071 is only visible where the other 3,069 exist.

**The title match was a bare substring, so `statin` matched `re-statin-g`.** Verbatim from the deployment: a query for "statin" returned a configuration paragraph reading "an overlay that puts OAuth on top WITHOUT restating the base matrix". Whole-word matching now happens in Go, because neither storage tier has a portable word boundary and the padded-LIKE trick misfires on punctuation — a title ending in a colon, or a hyphenated term, would stop matching. The LIKE remains a coarse prefilter and the fetch over-fetches, so filtering can never silently return fewer seeds than were asked for, which would read as "the graph holds nothing else" rather than "the prefilter was noisy". A query with no word character at its edges falls back to substring instead of matching nothing.

**A statement class is not an entity type.** The pass typed "the user prefers statin alternatives" as `preference:user` — the entity of type *preference* named *user*. That is not a harmless mislabel: **every preference about the user collapses onto that one node**, producing a hub that means nothing and burying the facts it should organise. An invented type such as `process` makes one strangely-labelled node; a statement class makes a magnet. `preference` and `fact` are now refused as entity types (they belong to the extractor's `class` vocabulary, and are in the ontology because the memory tier needs them as chunk types), and the refusals are COUNTED — a pass that mirrored nothing because every type was rejected must not look like a pass with nothing to mirror.

⚠️ **A claim in the v1.43.0 notes was wrong, and is corrected there.** Those notes said the extractor's inconsistent subject spelling forked one person into two graph nodes. It does not: `slug()` lowercases and its stopword list already drops `a`/`an`/`the`, so `the user`, `user` and `The user` all reduce to one key. The claim had been read out of the code rather than run — and it surfaced because the fix written for it changed nothing when removed, which is only possible if it was doing nothing. The regression test is kept regardless, because the guarantee is load-bearing and currently emerges from a stopword list nobody would think to protect: deleting the articles from it forks every subject whose name carries one.

**Still accepted, and stated rather than hidden:** entity types OUTSIDE the ontology (`profession`, `setting`, `process`, `task` all appeared in the eval). Refusing them needs the effective ontology reachable from the consolidator, which by design cannot read a tenant-scope document — a follow-up, and far lower stakes than the magnet above.

**🧰 Deploy fixes for the serve-and-test posture.** The sandbox sidecar pin is bumped so `expose` actually works: #891 set `SANDBOX_EXPOSE_NETWORK` but left the sidecar on an image predating the expose seam it added in the same change, so the old sidecar silently dropped the unknown field, opened a plain session and returned no exposed host — a serve-and-test run "succeeded" while the browser could not resolve the alias. The rest of the family had drifted too. And the TrueNAS deploy gains a **browser + sandbox variant**: build a feature in an isolated sandbox, serve it, and test it in a headless browser in one run, which was previously reachable only in the cloud deployment.

⚠️ **No migration, no schema change, no wire change.** Adapters unchanged. The memory bundle remains opt-in with its schedule at `enabled: false`. If you have already run a pass, entity nodes typed `preference:` or `fact:` from before this release are still in the store — they are inert rather than harmful (nothing reads them as entities now) but worth deleting if you want a clean graph.

## Earlier releases

Notes for **v1.43.1 back to v0.4.0** are not repeated here.

They were dropped from this file deliberately rather than lost: every tag carries
its own annotated message, and every release has a GitHub page with the same
notes. This file had grown to 185 sections and ~700 KB, which made it useless for
its actual job — telling you what changed *recently*.

- **Per-release notes:** <https://github.com/denn-gubsky/loomcycle/releases>
- **From a checkout:** `git tag --sort=-v:refname` to list, `git show <tag>` for one
- **What a version contains:** `git log --oneline <older>..<newer>`

For the shape of the runtime as it stands rather than how it got there, see
[`README.md`](README.md); for where it is going, [`docs/PLAN.md`](docs/PLAN.md).
