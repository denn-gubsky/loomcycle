# @loomcycle/client

TypeScript client for the [loomcycle](https://github.com/denn-gubsky/loomcycle) sidecar — the agentic-OS substrate for production agents.

`@loomcycle/client` speaks HTTP+SSE to the loomcycle server's `/v1/*` surface. The same operation surface is exposed via gRPC (`adapters/python/loomcycle`) and stdio MCP (`loomcycle mcp`); this client is the HTTP-side adapter, suitable for Node.js orchestrators, automation scripts, and operator tooling.

## Status

**v0.18.0** — 51 methods covering run streaming, agent metadata, transcript, pause/resume/state, snapshot lifecycle, memory admin, interruption resolve, hook registration, **v0.8.22 substrate admin (agentDef + skillDef)**, **v0.9.x n8n Phase 0 (listChannels + streamUserRunStates)**, **v0.9.x content_sha256**, **v0.9.x dynamic MCP server registration (mcpServerDef)** + **v0.18.0 typed `mcpServerDefVerify` + `ensureMcpServer` (idempotent register-if-changed)**, **v0.10.3 Library v2 enumeration (listLibraryAgents/Skills/McpServers)**, **v0.11.0 LLM Gateway (llmChat + llmStream)**, **v0.11.4 OpenAI Embeddings (embeddings)**, **v0.17.0 OSS multi-tenant auth (operatorTokenDef + whoami + tenant-scoped listUsers / listUserAgents — RFC L)**, and health.

> Migrating from raw `fetch` against `/v1/*`? See **[docs/MIGRATING-FROM-HTTP.md](./docs/MIGRATING-FROM-HTTP.md)** for a side-by-side walkthrough.

### What's new since v0.8.18

- **Breaking — `tools` replaces `allowedTools`** — the per-run tool allowlist is now a single canonical key. The `runStreaming` / `continueSession` option is renamed from `allowedTools` to **`tools`**, and the `AgentDefOverlay` + library wire field is likewise **`tools`**, matching the loomcycle YAML rename and aligning with Claude Code agent frontmatter. No dual-key fallback — update call sites to `runStreaming({ …, tools: [...] })`. Requires a loomcycle server carrying the same rename.
- **Path/Document browse-by-subject + the full Document op set** (v1.12.1, RFC AS/AK) — `path(input, opts)` and `document(input, opts)` accept optional `scopeId` / `tenant` browse overrides, sent as `?scope_id=` / `?tenant=` query params (the server reads them from the URL and re-checks authorization — a tenant principal's `tenant` is ignored, `scopeId` picks any subject it may see); omit both to browse your own subject (byte-identical to the pre-RFC-AS request). `DocumentToolInput.op` now covers all 16 backend ops — adds **`set_path`** (attach/re-home a Path-tree name for an existing document), **`export_md`** (render to Markdown; `include_metadata: false` for clean human-facing output), and **`import_md`** (build a document from export_md-shaped `markdown`). Additive — existing `path()` / `document()` callers are unchanged.
- **`interactiveSession` / `sendRunInput` / `streamRunByID` + the `interactive` flag** (v1.1.1, RFC AI) — the interactive agentic session, the adapter port of the Web UI's run terminal. Pass `interactive: true` to `runStreaming` / `continueSession` to start a **persistent** run that parks at end_turn (an `awaiting_input` frame) instead of ending; **`sendRunInput(runId, text)`** steers it (the response arrives on the same stream); **`streamRunByID(runId, {fromSeq})`** re-attaches by run_id (the operator's prior turns replay as `steer` events, `user_input.source === "replay"`, so a cold client — e.g. another device — reconstructs the whole conversation). The high-level **`client.interactiveSession({agent, segments})`** returns an `InteractiveSession` with `events()` / `send()` / `cancel()`; **`attachInteractiveSession(runId)`** resumes one. The `AgentEvent` union gains `awaiting_input` / `steer` / `context_compaction`.
- **`volumeDef` / `listVolumes` / `listEphemeralVolumes`** (v0.35.0, RFC AH) — the dynamic filesystem-volume surface. `volumeDef` is the op-discriminated substrate tool (`create` / `get` / `list` / `delete` / `purge`); a Volume is **flat** (a pointer to mutable on-disk state, not a versioned def), so `delete` unmaps + leaves files while `purge` removes the row **and** the directory tree — there is no retire/promote/fork. Tenant-confined (`ScopeTenant`): the runtime derives the path inside an operator-blessed `dynamic_root`, so you pass `{name, mode}`, never a host path. `listVolumes()` / `listEphemeralVolumes()` return the tenant's persistent + live run-scoped volumes; host paths are redacted (`""`) for a non-operator caller.
- **`ensureMcpServer` / `mcpServerDefVerify`** (v0.18.0) — typed ergonomics for the dynamic-MCP dedup flow. `ensureMcpServer({name, url, headers?, rediscover?})` registers a callback MCP server **idempotently**: it runs `create` (a no-op in loomcycle ≥ v0.18.0 when the active def already carries identical content) plus an optional `rediscover` (a no-op on unchanged tools), and returns `{defId, version, changed, discoveredToolCount?}` — so a consumer re-registering on every startup gets `changed: false` once its registration content is stable. Keep `${run.*}` / `${LOOMCYCLE_*}` header placeholders **literal** (don't bake a per-restart token) or the content varies each boot and dedup can't engage. `mcpServerDefVerify(name, sha)` is the typed `op: verify` wrapper (`matches: true` = no-op signal).
- **`operatorTokenDef` / `whoami` + tenant-scoped reads** (v0.17.0, RFC L) — the OSS multi-tenant authorization surface. `operatorTokenDef` is the op-discriminated admin tool over the `OperatorTokenDef` substrate (create / rotate / retire per-principal bearer tokens); `whoami()` returns the authoritative `(tenant, subject, scopes, is_admin)` resolved from the calling bearer; `listUsers({ tenant })` / `listUserAgents(userId, { tenant })` accept a super-admin tenant-focus (ignored server-side for a tenant principal — its own tenant is forced).

- **`llmChat` / `llmStream`** (v0.11.0) — direct LLM call surface that bypasses the agent loop. Provider routing + auth + retry without the ~50-200 ms per-turn overhead of a full `runStreaming` spawn. Drives n8n's `LoomCycleChatModel` AI Agent sub-node + any LangChain `BaseChatModel` consumer.
- **`listLibraryAgents` / `listLibrarySkills` / `listLibraryMcpServers`** (v0.10.3) — typed wrappers around the v0.9.3 Library v2 endpoints. Each returns a `LibraryListResponse<T>` with source-tagged entries (`"static-only"` / `"dynamic-only"` / `"both"`) merging yaml + substrate views.
- **`mcpServerDef`** (v0.9.x) — runtime registration of HTTP / Streamable-HTTP MCP servers without yaml edits. Same op grammar (create / fork / promote / retire / rediscover) as `agentDef` / `skillDef`.
- **`agentDef` / `skillDef`** (v0.8.22) — runtime fork / promote / retire / get / list / `verify` on the substrate.
- **`listChannels`** (v0.9.x) — list operator-declared channels with aggregate stats (message_count, oldest/newest visible_at).
- **`streamUserRunStates`** (v0.9.x) — SSE stream of run state transitions scoped to one `user_id`. Yields `{ kind: "open" | "event", payload }` items until the connection closes (30-min server cap).
- **Channel CRUD** (v0.9.x) — `publishChannel` / `subscribeChannel` / `peekChannel` / `ackChannel` with both admin scope (`scope: "global"`) and per-user scope (`scope: "user"` + `userId`).
- **Content signatures** (v0.9.x) — every `agent_defs` / `skill_defs` row carries a deterministic `content_sha256`. Combined with the `verify` op gives operators a one-call answer to *"is what I have identical to what's deployed?"*.
- **Transcript first-cycle types** (v0.9.1) — `UserInputPayload` + `SystemPromptPayload` typed interfaces for the two transcript events that surface "what the agent actually received" as the first frames of every run.
- **Dual ESM + CJS distribution** (v0.10.1) — n8n's community-node loader (CommonJS) now works alongside ESM consumers.
- **First-run UX on the binary** (v0.11.1) — paired CLI commands `loomcycle init` (bootstrap config) + `loomcycle doctor` (health check) + auto-discovery of `~/.config/loomcycle/loomcycle.yaml`. No adapter changes; lockstep version bump only.
- **Docker image + brew formula polish** (v0.11.2) — multi-arch image at `docker.io/denngubsky/loomcycle`; brew formula caveats refreshed to reference `loomcycle init` / `loomcycle doctor`; new `installation` Context.help topic. No adapter changes; lockstep version bump only.
- **OpenAI Chat Completions compatibility shim** (v0.11.3) — new `POST /v1/chat/completions` endpoint serves OpenAI's exact wire shape; consumers using the OpenAI SDK can point at loomcycle by changing only base URL + auth token. `@loomcycle/client` consumers should still prefer `llmChat()` / `llmStream()` over the shim for richer typing (per-frame discriminated unions vs OpenAI's flat chunks). No adapter changes; lockstep version bump only.
- **OpenAI Embeddings compatibility shim** (v0.11.4) — new `POST /v1/embeddings` endpoint serves OpenAI's Embeddings API shape. New `embeddings()` adapter method + four typed exports (`LLMEmbeddingsOptions`, `LLMEmbeddingsResponse`, `LLMEmbeddingItem`, `LLMEmbeddingsUsage`). Dispatches to the single configured `providers.Embedder` (the same instance Memory tool uses); RAG tools / vector DBs / LangChain `OpenAIEmbeddings` consumers point at loomcycle by changing only the base URL.

## Install

```bash
npm install @loomcycle/client
```

Requires Node ≥ 18. Bun and Deno likely work but are untested. Browser support is not a target — for browser-side operator control, use loomcycle's built-in Web UI at `/ui`.

### Module systems

From v0.10.1 the package ships as a **dual ESM + CommonJS** distribution:

```ts
// ESM (recommended)
import { LoomcycleClient } from "@loomcycle/client";

// CommonJS (legacy consumers — n8n's community-node loader, older Node
// scripts, anything not yet on ESM)
const { LoomcycleClient } = require("@loomcycle/client");
```

The `exports` field routes each consumer to the right build:
- `import` → `dist/index.js` (ESM)
- `require` → `dist/cjs/index.js` (CJS)
- `types` → `dist/index.d.ts` (single .d.ts set; works for both)

## Quick start

```ts
import { LoomcycleClient } from "@loomcycle/client";

const client = new LoomcycleClient({
  baseUrl: process.env.LOOMCYCLE_BASE_URL ?? "http://127.0.0.1:8787",
  authToken: process.env.LOOMCYCLE_AUTH_TOKEN,
});

// Run an agent, stream events
for await (const ev of client.runStreaming({
  agent: "qa-agent",
  segments: [
    { role: "user", content: [{ type: "trusted-text", text: "Hello, world." }] },
  ],
})) {
  if (ev.type === "text") process.stdout.write(ev.text ?? "");
}
```

## Cancellation

Every method accepts an optional `signal?: AbortSignal`. The streaming methods (`runStreaming`, `continueSession`) also break out of the iterator when the abort fires.

```ts
const ac = new AbortController();
setTimeout(() => ac.abort(), 30_000); // 30s budget

try {
  for await (const ev of client.runStreaming({ agent: "...", segments: [...], signal: ac.signal })) {
    // ...
  }
} catch (e) {
  if (e instanceof DOMException && e.name === "AbortError") {
    // timed out
  }
}
```

## API

All methods are async / return `Promise<T>` unless noted; streaming methods return `AsyncIterable<AgentEvent>`.

### Run lifecycle

| Method | Returns | Notes |
|---|---|---|
| `runStreaming(opts: RunOptions)` | `AsyncIterable<AgentEvent>` | Server-streams provider events for a fresh run. `interactive: true` parks at end_turn for steering (RFC AI). |
| `continueSession(opts: ContinueOptions)` | `AsyncIterable<AgentEvent>` | Continues an existing session. |
| `sendRunInput(runId, text)` | `{run_id, delivered}` | RFC AI — steer a live interactive run (`POST /v1/runs/{id}/input`). |
| `streamRunByID(runId, {fromSeq})` | `AsyncIterable<AgentEvent>` | RFC AI — re-attach by run_id (`GET /v1/runs/{id}/stream`); replays operator turns as `steer` events. |
| `interactiveSession(opts)` / `attachInteractiveSession(runId)` | `InteractiveSession` | RFC AI — high-level driver: `events()` / `send()` / `cancel()`. |

### Interactive sessions (RFC AI)

```ts
const sess = client.interactiveSession({
  agent: "assistant",
  segments: [{ role: "user", content: [{ type: "trusted-text", text: "help me debug" }] }],
});
for await (const ev of sess.events()) {
  if (ev.type === "text") process.stdout.write(ev.text ?? "");
  if (ev.type === "awaiting_input") {
    await sess.send(await prompt("you> ")); // steers; response arrives on this same loop
  }
  if (ev.type === "done") break;
}
// later, from anywhere (another process / device): resume the same run
const resumed = client.attachInteractiveSession(sess.runId);
```

The low-level primitives (`runStreaming({interactive:true})` + `sendRunInput` + `streamRunByID`) are the escape hatch if you'd rather drive the stream yourself.

### Agent metadata

| Method | Returns | Notes |
|---|---|---|
| `getAgent(agentId)` | `Promise<Agent>` | One agent's status + usage. Raises `AgentNotFoundError` if unknown. |
| `getRun(runId)` | `Promise<Agent>` | One run by its run id — `getAgent`'s shape. Use it when an agent id names several runs (every walk of a team is `team:<name>`). Raises `NotFoundError` if unknown. |
| `cancelAgent(agentId, opts?)` | `Promise<{ cancelledCount: number }>` | Cascades to children via `parent_agent_id`. Idempotent. |
| `listUserAgents(userId, opts?)` | `Promise<AgentList>` | Up to 100 runs. `status`: one status or an array (a run in any of them; omitted = running, `"all"` = every status). `awaitedState`: one or more of `channel` / `interrupted` / `review` / `input` / `children` — only running runs blocked on one of those waits; the server reads at most 1,000 running runs and the returned array carries `truncated: true` when it left some unread. An unknown value is a 400. `AgentList` is an `Agent[]`. |
| `listWalkRuns(walkId, { limit?, cursor? })` | `Promise<WalkRunsPage>` | One page of a team walk's runs — its own run and every member, oldest first: `{ agents, next_cursor }`. Pass `next_cursor` back as `cursor`; `""` on the last page. `limit` defaults to 100, at most 1000. Raises `NotFoundError` for a walk you cannot read. |
| `listChildRuns(parentRunId, { limit?, cursor? })` | `Promise<WalkRunsPage>` | One page of a run's direct children (the runs it started itself), of every status, oldest first, paged like `listWalkRuns`. Not the run itself, and not its grandchildren. Raises `NotFoundError` for a run you cannot read. |
| `deleteRunTree(runId)` | `Promise<RunTreeDeletion>` | Permanently delete a run and every run it spawned, with what is stored about them. The usage ledger is kept. Needs the `runs:delete` scope. Returns `{ deleted_runs, counts, kept, not_reached }`; an unknown, already deleted or unreadable run returns `deleted_runs: 0` instead of raising. A tree with a live or paused run deletes nothing and raises with `code: "run_tree_live"`. |
| `deleteSession(sessionId, { cascade: "children" })` | `Promise<RunTreeDeletion>` | The same for every run of a session, each with the runs it spawned. `cascade: "children"` is required. |
| `getTranscript(sessionId)` | `Promise<TranscriptResponse>` | Persisted event log; one row per event with seq/run_id/ts_ns/type/event. |
| `health()` | `Promise<HealthResponse>` | Liveness probe. Hits `/healthz` (no `/v1` prefix). Unauthenticated. |
| `getConfig(opts?)` | `Promise<ConfigResponse>` | v1.38.0 — instance configuration: build identity, the feature matrix, and the live provider/model/search cascade with `active`/`selected`. `view` names the disclosure level (`public` / `authenticated` / `admin`). Also readable with **no bearer** against a deployment running `LOOMCYCLE_PUBLIC_CONFIG=1`, which serves the narrower `public` view — the landing-page case. |
| `listUsers()` | `Promise<ListUsersResponse>` | Admin: known users with running-count summary. |

### Pause / Resume / State (v0.8.17 / v0.8.18)

| Method | Returns | Notes |
|---|---|---|
| `pauseRuntime(opts?: { timeoutMs? })` | `Promise<PauseResult>` | Quiesce the runtime. Raises `AlreadyPausingError` on 409, `PauseNotConfiguredError` on 503. |
| `resumeRuntime()` | `Promise<ResumeResult>` | Release the quiesce. Raises `NotPausedError` on 409. |
| `getRuntimeState()` | `Promise<RuntimeStateResponse>` | Current state + paused-runs count. |
| `resolveProbe()` | `Promise<ResolverMatrix>` | Force an immediate provider re-probe; returns the refreshed availability matrix. Raises `UnavailableError` on 503 (no resolver / no probe loop). |

### Snapshot lifecycle (v0.8.17 / v0.8.18)

| Method | Returns | Notes |
|---|---|---|
| `createSnapshot(opts?: CreateSnapshotOptions)` | `Promise<SnapshotCreateResponse>` | Capture envelope. Raises `SnapshotTooLargeError` on 413. |
| `listSnapshots(opts?: { limit?, labelContains? })` | `Promise<SnapshotDescriptor[]>` | Metadata only. |
| `getSnapshot(id)` | `Promise<SnapshotEnvelope>` | Full envelope including `json_content`. Raises `SnapshotNotFoundError` on 404. |
| `exportSnapshotURL(id)` | `string` | **Synchronous** — returns the download URL. Suitable for `<a href>` or piping to a HTTP download tool. |
| `restoreSnapshot(opts: { snapshotId? \| json?, includeHistory? })` | `Promise<SnapshotRestoreResponse>` | Restore from same-instance id OR inline envelope. Raises `SnapshotVersionError` on 422. |
| `deleteSnapshot(id)` | `Promise<void>` | Idempotent — 204 on both new and missing rows. |

Round-trip example:

```ts
const created = await client.createSnapshot({ label: "before-deploy" });
const env = await client.getSnapshot(created.id);
// ... move bytes to another loomcycle instance ...
const result = await otherClient.restoreSnapshot({ json: env.json_content });
console.log(`restored memory rows: ${result.memory_restored}`);
```

### Memory admin

| Method | Returns | Notes |
|---|---|---|
| `listMemoryScopes()` | `Promise<MemoryScopesResponse>` | Scope kinds (agent, user, etc.). |
| `listMemoryScopeIDs(scope)` | `Promise<MemoryScopeIDsResponse>` | scope_ids with row counts. |
| `listMemoryEntries(scope, scopeID, opts?)` | `Promise<MemoryEntriesResponse>` | Optional `prefix` + `limit`. |
| `getMemoryEntry(scope, scopeID, key)` | `Promise<MemoryEntryResponse>` | Single row read. |

### Memory / document change feeds (RFC CD Part C)

Live SSE tails of what changed in the caller's tenant's memory and documents — for a cache, an index, or a console that reacts to writes without polling.

| Method | Returns | Notes |
|---|---|---|
| `streamMemoryChanges(opts?)` | `AsyncIterable<ChangeFeedFrame>` | `memory.set` / `memory.delete` / `memory.scope_deleted`. Mirrors `GET /v1/_memory/changes`. |
| `streamDocumentChanges(opts?)` | `AsyncIterable<ChangeFeedFrame>` | `document.chunk.updated` / `document.chunk.deleted`, naming the `chunk_id`. Mirrors `GET /v1/_document/changes`. |

Each change is a **value-free coordinate** (`seq`, `type`, `scope`, `scope_id`, `key` or `chunk_id`, `at`): it says *what* changed, and you pull the current value through the data API. The first item is always `{ kind: "feed", payload: { enabled, since, embedder? } }`, then one `{ kind: "change", payload: MemoryChange }` per change, oldest first.

- **Off by default.** The server only records changes with `LOOMCYCLE_MEMORY_CHANGES_ENABLED`. Without it the stream opens and idles forever, so check `payload.enabled` on the feed frame rather than waiting.
- **Tenant-scoped, operator-only.** You see your own tenant's changes, never another's. The routes need `substrate:tenant` (or admin); a tenant member's token gets a 403 (`LoomcycleError`, `status: 403`).
- **Resumable.** Persist the last `seq` you processed and pass it back as `since` on reconnect; delivery is strictly after it. `since` must be a non-negative integer — the client throws otherwise, because the server would read a bad value as `0` and replay everything it retains.
- **Read failures are reported, not fatal.** A `{ kind: "error", payload: { code, error } }` item (`code: "store_unavailable"`) means the server could not read its change log. Keep iterating: the server retries with backoff without advancing its cursor, so changes resume after recovery with nothing skipped. You get one item per failure streak.
- **The embedder's health rides along.** An embedding failure does not fail the write, so a busy feed can sit over rows that are no longer searchable; `payload.embedder.state === "failing"` (with a `failures` count) is the signal to run `backfillEmbeddings`.

```ts
let since = await loadCursor(); // your own persistence
const ac = new AbortController();

for await (const item of client.streamMemoryChanges({ since, signal: ac.signal })) {
  if (item.kind === "feed") {
    if (!item.payload.enabled) throw new Error("change feed is off on this server");
    continue;
  }
  if (item.kind === "error") {
    console.warn(`change feed: ${item.payload.code}: ${item.payload.error}`); // stream continues
    continue;
  }
  const ch = item.payload; // { seq, type, scope, scope_id, key?, at }
  await invalidate(ch.scope, ch.scope_id, ch.key);
  since = ch.seq;
  await saveCursor(since);
}
// The loop ends when the server closes the stream (e.g. a restart): reconnect with `since`.
```

### Interruption (v0.8.16)

| Method | Returns | Notes |
|---|---|---|
| `listUserInterrupts(userId, opts?)` | `Promise<InterruptListResponse>` | Default filter: `status=pending`. |
| `listRunInterrupts(runId, opts?)` | `Promise<InterruptListResponse>` | Per-run interrupts. |
| `resolveInterrupt(runId, interruptId, opts: ResolveInterruptOptions)` | `Promise<unknown>` | Answer a pending interrupt. |

### Hook management (v0.8.18)

| Method | Returns | Notes |
|---|---|---|
| `hookDef(input)` | `Promise<SubstrateToolResponse>` | Reusable hook definitions (create / fork / get / list / promote / retire / verify / delete). An agent attaches hooks in its own definition (`tools` entries `{name, hooks}` and `hooks`). `input` is a `HookDefInput` or the loose `SubstrateToolInput` — see [Typed HookDef / ScheduleDef / WebhookDef inputs](#typed-hookdef--scheduledef--webhookdef-inputs-rfc-eo). |
| `createHookDef(name, overlay, opts?)` | `Promise<HookDefRowResponse>` | `op: "create"` with a strictly-typed `HookDefOverlay` — a misspelt key is a compile error even in a bare literal. Promoted unless `promote: false`. |
| `forkHookDef(name, overlay, opts?)` | `Promise<HookDefRowResponse>` | `op: "fork"` with a `HookDefForkOverlay` (partial; `null` clears a field). `opts.parentDefId` picks the parent (default: the active version). Not promoted unless `promote: true`. |
| `ensureHookDef(name, overlay, opts?)` | `Promise<EnsureHookDefResult>` | Idempotent register-if-changed for a `HookDefOverlay` — safe to call on every boot. `changed: false` when the active version already carries the same definition. |

A **HookDef** is a stored, versioned hook definition in your tenant: the event it answers, the tools it matches, its body (a `code-js` script run in-process, or a webhook URL), its fail mode and timeout. It fires on nothing by itself. An agent definition, a team definition or a run attaches it by name — in a tool entry `{name, hooks}` or the agent's own `hooks` — as `"scan"` (the active version) or `"scan@3"` (pinned). A hook can also be written inline as a webhook `{name, url, …}` in the definition that attaches it. For a webhook body, loomcycle posts to its URL during the agent's runs. The other side is the **callback receiver**, a small HTTP endpoint your app runs at that URL. The adapter exports the wire shapes (`PreHookCall` / `PostHookCall` / `PreHookResult` / `PostHookResult`) so you can type the handler against the same JSON loomcycle posts.

**Attach it in the agent's definition:**

```ts
await client.agentDef({
  op: "fork",
  name: "researcher",
  overlay: {
    tools: [
      "Read",
      { name: "WebFetch", hooks: { post: [{ name: "scan-webfetch", url: "https://jobember.example/hooks/scan", fail_mode: "open", timeout_ms: 3000 }] } },
    ],
  },
  promote: true,
});
```

**Run the callback receiver** (Next.js App Router example — adapt to your framework):

```ts
// app/hooks/scan/route.ts
import { NextResponse } from "next/server";
import type { PostHookCall, PostHookResult } from "@loomcycle/client";

export async function POST(req: Request) {
  const body = (await req.json()) as PostHookCall;
  // body.phase === "post", body.agent, body.tool_call.{id,name,input}, body.tool_result.{text,is_error}

  // Telemetry-shaped: log + pass through.
  console.log(`[hook] ${body.agent}.${body.tool_call.name} -> ${body.tool_result.is_error ? "error" : "ok"}`);

  // Empty body = pass through unchanged. Return a PostHookResult to rewrite:
  const reply: PostHookResult = {}; // or { result: { text: "redacted", is_error: false } }
  return NextResponse.json(reply);
}
```

**Pre-hook example** (short-circuit a tool call):

```ts
// app/hooks/guard/route.ts
import { NextResponse } from "next/server";
import type { PreHookCall, PreHookResult } from "@loomcycle/client";

export async function POST(req: Request) {
  const body = (await req.json()) as PreHookCall;

  // Deny outbound fetches to disallowed hosts
  const input = body.tool_call.input as { url?: string };
  if (input.url && new URL(input.url).hostname.endsWith(".internal")) {
    const reply: PreHookResult = {
      deny: { text: "internal hosts are not reachable from agents", is_error: true },
    };
    return NextResponse.json(reply);
  }

  return NextResponse.json({}); // pass through
}
```

**Verify a signed delivery.** A HookDef whose webhook body names a `signing_secret` (a credential, `"$cred:<name>"`) signs every call: `X-Loomcycle-Signature: t=<unix>,v1=<hex HMAC-SHA256 of t + "." + raw body>`. **Only the body is signed.** It carries `delivery_id` (a retry keeps it), `hook_name` and `owner`; the `X-Loomcycle-Delivery` and `X-Loomcycle-Hook` headers are unsigned copies a replayer can change, so dedupe and identify the hook from the verified body, never from them. Verify against the **raw** body, before parsing it:

```ts
import { verifyLoomcycleDelivery } from "@loomcycle/client";
import type { PreHookCall } from "@loomcycle/client";

export async function POST(req: Request) {
  const raw = await req.text(); // the exact bytes; JSON.stringify(await req.json()) will not match
  const d = await verifyLoomcycleDelivery<PreHookCall>(raw, req.headers.get("x-loomcycle-signature") ?? "", process.env.HOOK_SIGNING_SECRET!);
  if (!d) return new Response(null, { status: 401 });
  if (await seen(d.deliveryId)) return Response.json(previousAnswer(d.deliveryId)); // keep ids ≥ 300 s
  // d.hookName / d.owner say which hook sent it; d.body is the payload.
}
```

`verifyLoomcycleDelivery(rawBody, header, secret, opts)` resolves the body's `{deliveryId, hookName, owner, body}`, or `null` when the signature fails or the body has no `delivery_id`/`hook_name`. `verifyLoomcycleSignature(rawBody, header, secret, { toleranceSec = 300, now?, subtle? })` is the bare check: `true` only for a signature made within the tolerance; every malformed header, stale or future `t`, or wrong MAC is `false`. Both use Web Crypto (`crypto.subtle`), so they run in Node 19+, browsers and edge runtimes; on Node 18 pass `subtle: require("node:crypto").webcrypto.subtle`. Give each receiver its own signing secret, so a delivery captured at one cannot be replayed to another.

**Important constraints**:

- HookDefs are **stored and versioned**, not in-memory: they survive restarts, and every `create` mints a new version (the active one is what a by-name reference runs). To register from app startup without piling up versions, use `ensureHookDef`, which writes only when the definition changed. `fork` + `promote` (or `forkHookDef(…, {promote: true})`) derives a version from an existing one.
- Auth flows one-way: loomcycle → your receiver. Loomcycle attaches no credential of its own. To authenticate the caller, send a secret in the hook's `headers` as `$cred:<name>` (a stored credential, resolved for the run when the hook is called) rather than in the URL.
- `fail_mode: "open"` (default) is right for telemetry hooks where a down receiver shouldn't break tool dispatch. `"closed"` is right for security hooks where a down receiver should fail the tool call (don't let bypassed payloads through).
- `allow_hosts` in `PreHookResult` is a **trust-sensitive surface** — it widens the agent's outbound network policy for one tool call. Server enforces an operator-yaml allowlist (`hooks.permit_host_widen.owners`); your owner has to be on that list for `allow_hosts` to take effect. See the SECURITY note in `internal/hooks/types.go` before using.

### Typed HookDef / ScheduleDef / WebhookDef inputs (RFC EO)

**For checked keys, use the create / fork methods.** They take only the typed overlay, so a bare object literal is checked against it:

| Method | Returns | Notes |
|---|---|---|
| `createHookDef(name, overlay, opts?)` / `forkHookDef(name, overlay, opts?)` | `Promise<HookDefRowResponse>` | See [Hook management](#hook-management-v0818). Any unknown key is a compile error. |
| `createScheduleDef(name, overlay, opts?)` / `forkScheduleDef(name, overlay, opts?)` | `Promise<ScheduleDefRowResponse>` | Both promoted unless `promote: false`; a fork defaults to the active version, or the yaml template of `name`. |
| `createWebhookDef(name, overlay, opts?)` / `forkWebhookDef(name, overlay, opts?)` | `Promise<WebhookDefRowResponse>` | Same defaults as the schedule pair. |

`opts` is `{description?, promote?, signal?}`, plus `parentDefId?` on a fork. `promote` is sent only when you set it, so each def's own default applies otherwise.

The op-discriminated methods keep working as before. `hookDef`, `scheduleDef` and `webhookDef` accept a typed per-op input — `HookDefInput`, `ScheduleDefInput`, `WebhookDefInput` — or the loose `SubstrateToolInput` they always took, so existing calls compile unchanged. There the checking is opt-in: annotate the input (or use `satisfies`). A bare object literal with a misspelt key still compiles, because it matches the loose shape.

| Method | Returns | Notes |
|---|---|---|
| `scheduleDef(input)` | `Promise<SubstrateToolResponse>` | Scheduled-run definitions (create / fork / get / list / retire / add_hook / remove_hook). Mirrors `POST /v1/_scheduledef`. `add_hook` / `remove_hook` are expressible only through `ScheduleDefInput`. |
| `webhookDef(input)` | `Promise<SubstrateToolResponse>` | Inbound webhook definitions (create / fork / get / list / retire). Mirrors `POST /v1/_webhookdef`. |

- **`HookDefOverlay` is closed.** The server decodes a HookDef overlay strictly and refuses an unknown key, so the type has no index signature: a misspelt key is a compile error. `body` is a union on `kind` — `{kind: "code-js", code}` or `{kind: "http", url, headers?}` — because the server refuses a body that mixes them. A fork takes `HookDefForkOverlay`: every field optional, and `null` clears one.
- **`ScheduleDefOverlay` and `WebhookDefOverlay` stay open** (`[extra: string]: unknown`): the server decodes them loosely and drops keys it does not know. So even through `createScheduleDef` / `createWebhookDef`, a misspelt key compiles. What fails is a wrong type or value on a declared key, such as `concurrency_policy: "skip"`, `delivery: "run"` on a webhook, or an `mcp.call` hook without `tool`. They leave out the fields the server stamps itself (`operator_key_restricted`, `isolated`, `capture_disabled`, and on schedules `operator_layer`) — an overlay can neither set nor clear those.
- `on_complete` entries on both are a `ScheduledRunHook`: `channel.publish` (`channel`), `mcp.call` (`server` + `tool`) or `memory.set` (`scope` + `key`).

```ts
await client.createHookDef("scan-webfetch", {
  event: "post",
  match: { tools: ["WebFetch"] },
  body: { kind: "http", url: "https://jobember.example/hooks/scan", headers: { Authorization: "$cred:scan" } },
  fail_mode: "open",
  timeout_ms: 3000,
});

await client.createScheduleDef("weekly-digest", {
  agent: "digest",
  schedule: "0 6 * * 1",
  timezone: "Europe/Berlin",
  prompt: [{ role: "user", content: [{ type: "trusted-text", text: "Write this week's digest." }] }],
  on_complete: [{ kind: "channel.publish", channel: "digests" }],
});

await client.createWebhookDef("gh-push", {
  delivery: "spawn",
  agent: "ingest",
  auth: { kind: "hmac", algorithm: "sha256", header: "X-Hub-Signature-256", signing_secret_env: "LOOMCYCLE_GH_WEBHOOK_SECRET" },
  payload_mapping: { goal: "$.head_commit.message" },
});

// The op-discriminated form, checked by annotation:
import type { ScheduleDefInput } from "@loomcycle/client";
const addHook: ScheduleDefInput = {
  op: "add_hook",
  def_id: "sdf_…",
  hook: { kind: "memory.set", scope: "user", key: "last_digest" },
};
await client.scheduleDef(addHook);
```

**Register a HookDef on every boot** with `ensureHookDef`. A HookDef `create` always mints a new version, so this reads the active version first and compares its stored definition with yours, normalized the way the server stores it (`fail_mode` defaults to `"open"`; empty `match.tools`, description, headers and timeout are dropped). Equal → no write. A difference it cannot see through mints a redundant version rather than skipping a real change; `opts.description` is a rationale, not part of the definition, and does not count as a change.

```ts
const r = await client.ensureHookDef("scan-webfetch", {
  event: "post",
  match: { tools: ["WebFetch"] },
  body: { kind: "http", url: "https://jobember.example/hooks/scan" },
});
console.log(r.changed ? `registered v${r.version}` : "already current");
```

### Substrate admin: AgentDef + SkillDef (v0.8.22)

Two op-discriminated methods that mirror the in-process `AgentDef` / `SkillDef` built-in tools over HTTP. The same `op` values an agent's tool_use would invoke are reachable directly from your app code — useful for runtime fork / promote / retire / list, and for the `verify` op covered in [Content signatures](#content-signatures-v09x).

| Method | Returns | Notes |
|---|---|---|
| `agentDef(input)` | `Promise<SubstrateToolResponse>` | Op-discriminated. Mirrors `POST /v1/_agentdef`. |
| `skillDef(input)` | `Promise<SubstrateToolResponse>` | Op-discriminated. Mirrors `POST /v1/_skilldef`. |
| `getAgentOverridable(name, { tenant? })` | `Promise<AgentOverridableResponse>` | Which per-run overrides a run of `name` may set, **for this caller**: one entry per override (`model`, `effort`, `max_tokens`, …) with `overridable`, and a `reason` (`token_locked` / `agent_locked`) when refused. Folds the agent's `allow_overrides` with your token's overrides policy, as a run would. An overridable field may carry `narrow_only`, `max`, or `values` (a closed set the definition declares). For building run forms. `substrate:tenant`; a token limited to certain agents may ask about those. Raises `AgentNotFoundError` for an unknown or another tenant's agent. Mirrors `GET /v1/_agentdef/{name}/overridable`. |

The response type is intentionally `unknown` because the shape varies per op (`create`/`fork` return a row envelope; `list` returns `{name, versions: [...]}`; `verify` returns `AgentDefVerifyResult` / `SkillDefVerifyResult`). Cast / narrow as needed:

```ts
import type { AgentDefRowResponse } from "@loomcycle/client";

const forked = (await client.agentDef({
  op: "fork",
  name: "researcher",
  overlay: { system_prompt: "be very thorough", max_iterations: 32 },
  promote: true,
})) as AgentDefRowResponse;

console.log(`forked def_id=${forked.def_id} hash=${forked.content_sha256}`);
```

Operations on AgentDef: `create` / `fork` / `get` / `list` / `promote` / `retire` / **`verify`** (v0.9.x). SkillDef has the same set minus `retire`'s edge cases. See `internal/tools/builtin/agentdef.go` for the canonical input schema; each op enforces the agent's capability gate from the operator yaml — `agent_def_scopes` for AgentDef, and (RFC BA / v1.14.0) the agent's `skills:` pattern allowlist for SkillDef (the former `skill_def_scopes` gate was removed).

Refusals throw `SubstrateToolRefusedError` (a scope deny / empty body / allowed-tools widening); transport failures throw the usual typed errors (`AuthError`, `UnavailableError`, etc.).

### Dynamic filesystem volumes (v0.35.0 — RFC AH)

Per-tenant, ro/rw filesystem roots an agent can be bound to. `volumeDef` provisions and manages them at runtime; the two list methods render the volume universe. Tenant-confined (`ScopeTenant`).

| Method | Returns | Notes |
|---|---|---|
| `volumeDef(input)` | `Promise<SubstrateToolResponse>` | Op-discriminated (`create` / `get` / `list` / `delete` / `purge`). Mirrors `POST /v1/_volumedef`. |
| `listVolumes()` | `Promise<PersistentVolumesResponse>` | Static (read-only floor) + the tenant's dynamic volumes. `GET /v1/_volumes`. |
| `listEphemeralVolumes()` | `Promise<EphemeralVolumesResponse>` | Live, run-scoped volumes (auto-purged at run completion). `GET /v1/_volumes/ephemeral`. |

A Volume is **flat** — a pointer to mutable on-disk state, not a versioned definition — so the op set is `create` / `get` / `list` / `delete` / `purge` (no retire/promote/fork). The runtime DERIVES the path inside an operator-blessed `dynamic_root` (`<root>/<tenant>/<name>`), so you pass `{name, mode}` and never a host path:

```ts
// Provision a writable per-tenant volume (the runtime mkdir's it).
await client.volumeDef({ op: "create", name: "repo-a", mode: "rw" });

// Unmap (keeps files) vs. destroy (RemoveAll's the tree).
await client.volumeDef({ op: "delete", name: "repo-a" }); // non-destructive
await client.volumeDef({ op: "purge",  name: "repo-a" }); // destructive

const { entries } = await client.listVolumes();
// entries[].path is "" (redacted) unless the caller is operator-equivalent.
```

Refusals throw `SubstrateToolRefusedError` (collision with a static volume name, no `dynamic_root` configured, cross-tenant); transport failures throw the usual typed errors.

### Schedules admin

The schedules surface the Web UI drives: one merged list, a schedule's runtime state, and run-now / pause / resume. Authoring stays on `scheduleDef(input)`. Tenant-confined: a `substrate:tenant` token sees and acts on its own tenant's ScheduleDefs only, and another tenant's `def_id` answers 404 like an unknown one.

| Method | Returns | Notes |
|---|---|---|
| `listAllSchedules(opts?)` | `Promise<ScheduleListResponse>` | Static yaml `scheduled_runs` + ScheduleDefs, merged by name. Statics appear for an admin only; `opts.tenant` focuses one tenant. `GET /v1/_schedules/list-all`. |
| `getScheduleState(defId)` | `Promise<ScheduleStateView>` | Last/next fire, pause, last slot claim, catch-up backlog, runs in flight. `GET /v1/_schedules/{def_id}/state`. |
| `runScheduleNow(defId)` | `Promise<ScheduleRunNowResult>` | Fires on the scheduler's next tick; the cadence continues after it. `POST .../run-now`. |
| `pauseSchedule(defId)` | `Promise<SchedulePauseResult>` | Indefinite (no duration). `POST .../pause`. |
| `resumeSchedule(defId)` | `Promise<SchedulePauseResult>` | `POST .../resume`. |

The per-schedule methods take the ScheduleDef's `active_def_id` from the list; a static-only schedule has none. Every optional instant in `ScheduleStateView` (`last_run_at`, `paused_until`, `slot_at`, `catch_up_until`, …) is **absent** when unset — test for presence. (Servers before this release sent `last_run_at` and `paused_until` as `"0001-01-01T00:00:00Z"`.)

```ts
const { entries } = await client.listAllSchedules();
for (const e of entries.filter((e) => e.active_def_id)) {
  const st = await client.getScheduleState(e.active_def_id!);
  if (st.paused_until) console.log(`${e.name} is paused`);
}
```

### Models, routing + provider models

| Method | Returns | Notes |
|---|---|---|
| `listModels()` | `Promise<ModelAliasesResponse>` | The configured model aliases (`models:`), alias → `{provider, model, kind?}`. Tenant-readable. `GET /v1/_models`. |
| `getRouting()` | `Promise<RoutingResponse>` | Per user tier × tier, the ordered provider/model cascade with live availability and the `selected` candidate. Tenant-readable; a non-admin gets no raw `last_error`, and under the operator-key gate only providers it can key. `GET /v1/_routing`. |
| `listProviderModels(providerId)` | `Promise<ProviderModelsResponse>` | A live listing from the provider. **Admin token only** — a tenant token gets a 403 `LoomcycleError`; fall back to `listModels()` plus free-text entry. `GET /v1/_providers/{id}/models`. |

Store an alias from `listModels()` on an agent or fork rather than the model it names: the agent then follows the operator's retarget of the alias.

### Def name listings

One method per substrate family returns every declared name with its version roll-up (`version_count`, `active_def_id`, `latest_version`, `last_updated`) — what a family's `list` op cannot, since it lists the versions of one name. Tenant-scoped server-side; `opts.tenant` focuses one tenant for an admin and is ignored for a tenant token. `names` is `[]` when nothing is declared.

| Method | Route |
|---|---|
| `listAgentDefNames(opts?)` | `GET /v1/_agentdef/names` |
| `listSkillDefNames(opts?)` | `GET /v1/_skilldef/names` |
| `listHookDefNames(opts?)` | `GET /v1/_hookdef/names` |
| `listMcpServerDefNames(opts?)` | `GET /v1/_mcpserverdef/names` |
| `listScheduleDefNames(opts?)` | `GET /v1/_scheduledef/names` |
| `listA2AServerCardDefNames(opts?)` | `GET /v1/_a2aservercarddef/names` |
| `listA2AAgentDefNames(opts?)` | `GET /v1/_a2aagentdef/names` |
| `listWebhookDefNames(opts?)` | `GET /v1/_webhookdef/names` |
| `listMemoryBackendDefNames(opts?)` | `GET /v1/_memorybackenddef/names` |
| `listDocumentSourceDefNames(opts?)` | `GET /v1/_documentsourcedef/names` |
| `listOperatorTokenDefNames(opts?)` | `GET /v1/_operatortokendef/names` — **admin token only**; one row per (tenant, name): `{name, tenant_id, subject, token_count, has_current, current_def_id?, last_updated}`, never a token. Rotate or retire by `current_def_id`; a per-user (`u-`) name is refused as a target. |

All but the last return `DefNamesResponse`; `live_version_count` / `active_retired` are reported by the AgentDef, SkillDef, HookDef and MCPServerDef families. Teams have `listTeams()`.

Team options ride on existing methods: `createTeam(name, overlay, { promote, description })` sends the create's own fields (omitted, a create is promoted — the server default; `promote: false` authors it without putting it in force), `forkTeam(name, overlay, { promote, parentDefId, description })` sends the fork's (omitted, the fork is authored but not active — the server default for a fork), and `runTeam({ ..., interruptOnCap: true })` asks a human whether to continue, reroute or abort when a state hits its iteration cap, instead of ending with `iteration_cap`.

### Access control (RFC EY / EZ)

Groups, access-control rule lists, the bindings that attach a list to a subject, and the AuthZEN-shaped decision questions. Groups, lists and bindings are the tenant operator's (`substrate:tenant`, an admin, or the legacy shared token; any other principal gets an `InsufficientScopeError`); the decision questions are open to any principal about itself. Every call acts in your own tenant; `opts.tenant` focuses another for an admin and is ignored for a tenant token. Rows the config declares (`acls:` / `groups:` / `acl_bindings:`) are listed beside the stored ones with `source: "config"`, without versions or timestamps, and cannot be written here. Every call needs a persistent store and is refused from inside a run.

| Method | Route | Returns |
|---|---|---|
| `listGroups(opts?)` | `GET /v1/_groups` | `GroupListResponse` — stored and config groups, by name |
| `getGroup(name, opts?)` | `GET /v1/_groups/{name}` | `GroupDetail` — a stored group, its direct members, `contained_in`; a config-only group is a `NotFoundError` |
| `getGroupEffective(name, opts?)` | `GET /v1/_groups/{name}/effective` | `GroupEffective` — every user through nested groups, config members included |
| `createGroup({name, description?, external_id?}, opts?)` | `POST /v1/_groups` | `GroupRow` — an upsert by name |
| `deleteGroup(name, opts?)` | `DELETE /v1/_groups/{name}` | `{deleted}` |
| `addGroupMember(name, {kind, id}, opts?)` | `POST /v1/_groups/{name}/members` | `GroupMemberChange`; `kind` is `user` or `group` (nesting) |
| `removeGroupMember(name, kind, id, opts?)` | `DELETE /v1/_groups/{name}/members/{kind}/{id}` | `GroupMemberChange` with `removed: true` |
| `aclDef(input, opts?)` | `POST /v1/_acldef` | the op's body; `op: "names"` reads `GET /v1/_acldef/names` |
| `listAclDefs({includeRetired?}?)` | `aclDef` op `list` | `AclDefListResponse` — `acls` plus the read-only `builtin` baselines |
| `getAclDef(name, opts?)` | op `get` | `AclDefRow` |
| `putAclDef({name, rules, description?}, opts?)` | op `put` | `AclDefRow` — creates or replaces, bumping `version` |
| `retireAclDef(name, opts?)` | op `retire` | `{retired}` |
| `validateAclRules(rules, opts?)` | op `validate` | `AclValidateResult`; bad rules are an `InvalidArgumentError` with code `acl_invalid` |
| `getAclCatalog(opts?)` | op `catalog` | `AclCatalog` — types, verbs, scopes, transports |
| `listAclDefNames(opts?)` | `GET /v1/_acldef/names` | `{names}` |
| `listAclBindings({includeRetired?}?)` | `GET /v1/_aclbinding` | `AclBindingListResponse` |
| `createAclBinding({acl, subject_kind, subject_id?, expires_at?}, opts?)` | `POST /v1/_aclbinding` | `AclBinding`; a missing list or a past expiry is an `InvalidArgumentError` with code `acl_binding_invalid` |
| `deleteAclBinding(bindingId, opts?)` | `DELETE /v1/_aclbinding/{id}` | `{retired}` |
| `authzEvaluation(req, opts?)` | `POST /v1/_authz/evaluation` | `AuthzDecision` — `decision` is true only when a rule allowed |
| `authzEvaluations(req, opts?)` | `POST /v1/_authz/evaluations` | `{evaluations}` — up to 256, in order |
| `authzExplain(req, opts?)` | `POST /v1/_authz/explain` | `AuthzExplainResponse` — every rule read and why |
| `authzSimulate(req, opts?)` | `POST /v1/_authz/simulate` | `AuthzSimulateResponse` — `now` and `with_draft` per request; nothing is written |

`whoami()` also reports `acl_mode` (`compat` / `shadow` / `enforce`), the caller's `groups` and `acl_version`, each absent when the server does not send it. A delegated writer changing its own authority is refused with code `acl_self_elevation`; a write to a group an identity provider owns (`external_id` set) with a 409 `group_external`. A rule that refuses any call — here or on any other route — throws `AclDeniedError` (see [Errors](#errors)).

```ts
const { names } = await client.listAclDefNames();
await client.putAclDef({
  name: "support-tier-1",
  rules: [{ effect: "deny", actions: ["invoke"], resources: ["tool:Bash"] }],
});
await client.createAclBinding({ acl: "support-tier-1", subject_kind: "group", subject_id: "support" });
const why = await client.authzExplain({
  subject: { type: "user", id: "alice" },
  action: { name: "invoke" },
  resource: { type: "tool", id: "Bash" },
});
console.log(why.decision.outcome, why.traces);
```

### Decision models

Ask a **decision model** typed questions about a piece of text and get each answer with its probabilities. A decision model does not write: it reads the `state` you give it and answers a choice, a yes/no or a score, so there is no reply to parse and a call costs a few output tokens. Use it to route, gate, rank or grade text you already have. No run is involved; the tokens are charged to the caller and count against its token budget.

| Method | Returns | Notes |
|---|---|---|
| `decide(req: DecideRequest)` | `Promise<DecideResponse>` | `{model?, state, questions}` → `{model, provider, served_model, answers, usage}`. Mirrors `POST /v1/_decide`. |
| `listDecisionModels()` | `Promise<DecisionModelsResponse>` | The models a call may name, each with its limits, and the default. Mirrors `GET /v1/_decide/models`. |

`questions` is keyed by names you choose, and each answer comes back under its question's name. `criteria` is typed per question type:

| `type` | `criteria` | Answer |
|---|---|---|
| `choice` | required — `Record<string, string \| null>`: each key is an option, its value describes it (`null` when the key explains itself) | `{type, choice, probabilities, confidence}` |
| `noul` | optional — `{true?: string; false?: string}` | `{type, noul}` — the probability of YES, 0 to 1 |
| `score` | required — `string[]`, level descriptions **lowest first** | `{type, score, legend, probabilities, confidence}` — `score` is the expected position, counted from 0 |

```ts
const res = await client.decide({
  state: { ticket: "My invoice for March was charged twice and I want my money back." },
  questions: {
    route: {
      type: "choice",
      instructions: "Which team should handle this ticket?",
      criteria: { billing: "invoices, refunds, charges", support: "bugs, outages", sales: null },
    },
    urgent: { type: "noul", instructions: "Does this ticket need a reply within the hour?" },
    detail: {
      type: "score",
      instructions: "How complete is the problem report?",
      criteria: ["no detail", "some detail", "everything needed"],
    },
  },
});

const route = res.answers.route;
if (route?.type === "choice") console.log(route.choice, route.probabilities);
const urgent = res.answers.urgent;
if (urgent?.type === "noul" && urgent.noul > 0.5) escalate();
```

- **`noul: 0` is an answer** (a definite no), not a missing one — never test it for truthiness.
- **The probabilities are not calibrated.** Compare options within one answer; do not treat a fixed threshold as a guarantee.
- **An answer is the model's own JSON, passed through unchanged.** The answer types name the documented fields and stay open to others, so a field a later model adds is readable without an adapter release.
- Omit `model` for the deployment's default; name one only from `listDecisionModels()`.

A refusal throws with the decision's own code on `e.code` (see [Errors](#errors)):

| Status | `e.code` | What to do |
|---|---|---|
| 400 | `invalid_input` / `bad_question` / `bad_options` / `too_many_questions` | Fix the request; the message names the fault. |
| 400 | `model_not_allowed` | Omit `model`, or use a name from `listDecisionModels()`. |
| 403 | `operator_key_restricted` | The caller may not use the operator's provider key and has none of its own. Do not retry. |
| 413 | `prompt_too_large` | The request does not fit the model's context and is never shortened for you: shorten `state` or ask fewer questions. |
| 429 | `token_limit_exceeded` | The caller is at a hard token budget (`TokenLimitExceededError`, `retryable` false). Retrying does not help until the budget is raised or the month rolls over. |
| 502 | `model_not_found` / `call_failed` | The provider does not serve that model (the same call fails again) / the call did not complete (try once more; `retryable` is true). |
| 503 | `decision_not_configured` | This deployment declares no decision models. Do not retry. |
| 504 | `timeout` | Send the same call again (`retryable` is true); if it repeats, make it smaller. |

```ts
try {
  await client.decide(req);
} catch (e) {
  if (e instanceof LoomcycleError && e.code === "prompt_too_large") {
    // shorten state and retry
  } else throw e;
}
```

Two of these have a class of their own: a `prompt_too_large` throws `PromptTooLargeError` and a `token_limit_exceeded` throws `TokenLimitExceededError` (with the budget on `e.scope` / `e.used` / `e.limit`). For the rest, branch on `e.code`, not on the class: the class follows the HTTP status alone.

### Credentials (RFC AR)

The encrypted credential store an agent's `$cred:<name>` references resolve against. `scope` is `user` (your own subject's credentials) or `tenant` (shared across the tenant; needs `substrate:tenant`). Every read is metadata only — a stored value is never returned.

| Method | Returns | Notes |
|---|---|---|
| `listCredentials(scope)` | `Promise<CredentialListResponse>` | Every credential's metadata in the scope. `{op: "list"}`. |
| `getCredential({ scope, name })` | `Promise<CredentialMeta>` | One credential's metadata: `backend`, `created_at` / `updated_at`, and `expires_at` / `allowed_urls` / `username` when set. Raises `SubstrateToolRefusedError` for an absent name. `{op: "get"}`. |
| `createCredential({ scope, name, value, allowed_urls?, username?, expires_at?, backend? })` | `Promise<CredentialMeta>` | Store or rotate. `value` is write-only. `expires_at` is RFC3339; `backend` is `"inline"`, the only one accepted. `{op: "create"}`. |
| `deleteCredential({ scope, name })` | `Promise<{ name, scope, deleted }>` | Anything referencing `$cred:<name>` at that scope stops resolving. `{op: "delete"}`. |

All four post to `POST /v1/_credentialdef`.

```ts
const meta = await client.getCredential({ scope: "user", name: "GIT_TOKEN" });
console.log(meta.allowed_urls ?? "unrestricted");
```

### Crypto keys + the Crypto tool (RFC EP)

A **crypto key** is a credential with `usage: "crypto"`: a `key_type` fixed for its life, numbered versions, and a `min_version` below which versions are retired. No call ever returns key material. `getCredential` / `listCredentials` report a crypto key's `usage`, `key_type`, `current_version`, `min_version` and `versions` (`{version, created_at, retired}`).

| Method | Returns | Notes |
|---|---|---|
| `generateCryptoKey({ scope, name, key_type, expires_at? })` | `Promise<CredentialMeta>` | The server makes the material; nobody sees it. `{op: "generate"}`. |
| `createCryptoKey({ scope, name, key_type, value, key_encoding?, expires_at? })` | `Promise<CredentialMeta>` | Bring material you already have; `value` is write-only. `{op: "create", usage: "crypto"}`. |
| `createKmsCryptoKey({ scope, name, key_type, external, expires_at? })` | `Promise<CredentialMeta>` | Reference a key held in Vault Transit, AWS KMS or Google Cloud KMS (`external.backend`); only the reference is stored and every op is a KMS call. Credential fields are `$cred:<name>` in the key's scope. Rotate and retire it in the KMS. `{op: "create", usage: "crypto", backend, external}`. |
| `rotateCryptoKey({ scope, name, value?, key_encoding? })` | `Promise<CredentialMeta>` | Adds a version and makes it current (generated unless `value` is given). `{op: "rotate"}`. |
| `setCryptoKeyMinVersion({ scope, name, min_version })` | `Promise<CredentialMeta>` | Retires every version below it. Only goes up, at most to current+1. `{op: "set_min_version"}`. |
| `deleteCryptoKeyVersion({ scope, name, version })` | `Promise<CredentialMeta>` | Deletes one **retired** version. `{op: "delete_version"}`. |
| `exportCryptoKey({ scope, name, passphrase })` | `Promise<CryptoKeyExport>` | `{name, scope, blob}`: every version, encrypted under the passphrase (≥ 16 chars). Admin only, outside any run. `{op: "export_key"}`. |
| `importCryptoKey({ scope, name, blob, passphrase })` | `Promise<CredentialMeta>` | The other side of `exportCryptoKey`, under the same `name`. Admin only, outside any run. `{op: "import_key"}`. |

All post to `POST /v1/_credentialdef`. To delete a crypto key, retire every version (`min_version` = current+1), then `deleteCredential`.

`crypto(input)` runs one Crypto-tool operation outside any run (`POST /v1/_crypto`): `encrypt` / `decrypt` / `rewrap` / `mac` / `verify_mac` / `sign` / `verify` / `public_key` / `totp` with a held key (`key`, or `cred` + `key_type`), and `hash` / `random` with none. The input is a `CryptoRequest` discriminated by `op`, and the return type follows it (`CryptoResultByOp`). The reachable keys are the token's tenant's and its own subject's user keys; an admin may add `tenant` to use another tenant's keys. Needs `substrate:tenant` (a tenant member's token gets a 403). A refusal — including a failed decrypt, which never says why — raises `SubstrateToolRefusedError`; each call counts against the token's run-start rate, and a spent rate raises `RateLimitedError` (`retryAfterMs`).

```ts
await client.generateCryptoKey({ scope: "tenant", name: "customer-fields", key_type: "aes-256-gcm" });
const { ciphertext } = await client.crypto({
  op: "encrypt", key: "customer-fields", plaintext: card, context: "customer:42/card",
});
const { plaintext } = await client.crypto({ op: "decrypt", ciphertext, context: "customer:42/card" });
```

### Evaluations

Scores attached to runs: record a verdict on a run, read the scores back, and roll them up per definition version to compare versions. All of these post to `POST /v1/_evaluation`; `evaluation(input)` takes the op-discriminated input directly and its return type follows `op`.

| Method | Returns | Notes |
|---|---|---|
| `submitEvaluation(runId, score, { dimensions?, judgement?, rationale? })` | `Promise<EvaluationSubmitResult>` | Additive: submitting again records another evaluation. `{op: "submit"}`. |
| `getEvaluation(evalId)` | `Promise<EvaluationRecord>` | `{op: "get"}`. |
| `listEvaluationsForRun(runId, { limit? })` | `Promise<EvaluationListForRunResult>` | `{op: "list_for_run"}`. |
| `listEvaluationsForDef(defId, { limit? })` | `Promise<EvaluationListForDefResult>` | Your tenant's scores of that definition version. `{op: "list_for_def"}`. |
| `aggregateEvaluations(defId, { includeLineage? })` | `Promise<EvaluationAggregate>` | Mean / median / min / max / latest, per dimension and per emitter role. `{op: "aggregate"}`. |

What a token may do: `substrate:tenant` every op inside its own tenant; admin every op in every tenant; any other token (an isolated `substrate:user`, a tenant member) only `submit_self`, which a call from outside a run cannot use, so each op is refused with `SubstrateToolRefusedError`. A token without `runs:read` gets a 403. Another tenant's evaluation or run reads exactly like one that does not exist.

```ts
await client.submitEvaluation(runId, 0.8, { rationale: "correct, but slow" });
const agg = await client.aggregateEvaluations(defId);
console.log(agg.count, agg.score.mean);
```

### Team runs

`runTeam(target)` walks a team's state graph. Its return type follows `target.mode`:

| Call | Returns |
|---|---|
| `runTeam({ name, input, mode: "detach" })` | `Promise<TeamRunDetached>` — the `run_id` at once; the walk keeps running. |
| `runTeam({ name, input })` | `Promise<TeamRunResult>` — waits for the walk; `steps` is the per-state trace. |
| `runTeam(target: TeamRunTarget)` | `Promise<TeamRunResult \| TeamRunDetached>` — for a target whose `mode` is not known statically (built from a form or a node parameter). Narrow by the `mode` you sent. |

A start with an `idempotencyKey` another start already holds is answered with that walk, marked `deduplicated: true`, and carries no `steps` — the trace is not stored on the run. Once that walk has ended it also has `final_output`, and `final_state` / `stop_reason` / `error` where they were recorded.

```ts
import type { TeamRunTarget } from "@loomcycle/client";

const target: TeamRunTarget = { name: "triage", input, mode: detach ? "detach" : undefined };
const res = await client.runTeam(target);
console.log(res.run_id);
```

### Channels + run-state stream (v0.9.x n8n Phase 0)

Two substrate-side surfaces added in the n8n integration's Phase 0 wire-API work. Useful for any orchestrator (not just n8n) that needs to see channel state or subscribe to run-state transitions.

| Method | Returns | Notes |
|---|---|---|
| `listChannels()` | `Promise<ListChannelsResponse>` | Operator-declared channels + aggregate stats (`message_count`, `oldest_visible_at`, `newest_visible_at`). Mirrors `GET /v1/_channels`. |
| `streamUserRunStates(userId, opts?)` | `AsyncIterable<RunStateStreamItem>` | SSE stream of run state transitions for one user. Yields one `{ kind: "open", ... }` frame then one `{ kind: "event", payload: RunStateEvent }` per matching transition until close. `opts.walkId` (v1.78.0) narrows it server-side to one team walk's agents. |

**Streaming run-state events** — for orchestration UIs that want to react when an agent run completes / fails / cancels:

```ts
import type { RunStateEvent } from "@loomcycle/client";

const ac = new AbortController();
const stream = client.streamUserRunStates(userId, {
  statuses: ["completed", "failed", "cancelled"], // optional filter
  agent: "researcher",                            // optional filter
  signal: ac.signal,
});

for await (const item of stream) {
  if (item.kind === "open") {
    console.log(`stream open for user=${item.payload.user_id}`);
    continue;
  }
  const evt: RunStateEvent = item.payload;
  console.log(`${evt.agent}/${evt.run_id} -> ${evt.status} (stop_reason=${evt.stop_reason ?? "-"})`);
  // ... persist to DB, push to UI websocket, fire webhook, etc.
}
```

The stream stays open for up to 30 minutes (server-enforced); reconnect on close for long-running orchestrators. Filters apply server-side; an empty filter delivers all transitions.

### Content signatures (v0.9.x)

**The bundle-vs-deployed comparison feature.** Every persisted `agent_defs` and `skill_defs` row carries a deterministic SHA-256 of its content-bearing fields (`content_sha256`). Combined with the CLI helper `loomcycle hash agent|skill <path>`, this lets Docker-bundled operators answer *"is what I have in my image identical to what's deployed?"* with one cheap call instead of fetching the full Definition JSONB and diffing it field by field.

**The workflow** — three steps, fully Dockerfile-friendly:

1. **At image-build time** (in your Dockerfile or CI): run the CLI against each bundled MD to capture the expected hash.

   ```dockerfile
   # Dockerfile
   COPY agents/    /bundle/agents/
   COPY skills/    /bundle/skills/
   RUN /usr/local/bin/loomcycle hash agent /bundle/agents/researcher.md > /bundle/agents/researcher.sha256
   RUN /usr/local/bin/loomcycle hash skill /bundle/skills/summariser   > /bundle/skills/summariser.sha256
   ```

2. **At container startup**: ask the deployed loomcycle whether each agent is in sync. Use `agentDef({op:"verify"})` / `skillDef({op:"verify"})` and narrow the response to `AgentDefVerifyResult` / `SkillDefVerifyResult`:

   ```ts
   import { readFile } from "node:fs/promises";
   import type { AgentDefVerifyResult } from "@loomcycle/client";

   const localHash = (await readFile("/bundle/agents/researcher.sha256", "utf-8")).trim();
   const verify = (await client.agentDef({
     op: "verify",
     name: "researcher",
     content_sha256: localHash,
   })) as AgentDefVerifyResult;

   if (verify.matches) {
     console.log("researcher in sync");
   } else if (!verify.deployed) {
     console.log("researcher not deployed yet; pushing first version");
     await pushAgent("/bundle/agents/researcher.md"); // your set-agent helper
   } else {
     console.log(`researcher drifted; deployed=${verify.current_sha256} local=${localHash}; pushing update`);
     await pushAgent("/bundle/agents/researcher.md");
   }
   ```

3. **Pushing on mismatch** is `agentDef({op:"set"|"fork", overlay: {...}})` with the same content the YAML expresses, parsed from your bundle.

| Method | Returns | Notes |
|---|---|---|
| `agentDef({op:"verify", name, content_sha256})` | `Promise<AgentDefVerifyResult>` | `{matches, current_sha256, current_def_id, version, name, deployed}`. |
| `skillDef({op:"verify", name, content_sha256})` | `Promise<SkillDefVerifyResult>` | Same shape. |

**Key invariants:**

- `matches: true` only when both hashes are non-empty AND equal. An empty caller hash NEVER matches (no false-positive when the deployed row's hash is also empty due to a not-yet-completed backfill).
- `deployed: false` ⇒ `matches: false`. Use this to distinguish "no active row" (first deploy) from "drift" (push update).
- The CLI hash and the substrate's hash are guaranteed identical for matching content — both compute through the same Go function in `internal/agents.Sign`.
- Agent hash covers `name + description + system_prompt + tools + skills + model + provider + tier + effort + max_tokens + max_iterations + providers + models + memory_scopes + memory_quota_bytes`. Explicitly excluded: `def_id`, `version`, `created_at`, `retired`, **plus** `channels` and `*_scopes` (operator-yaml-only ACL fields that don't round-trip through `set` / `fork`).
- Skill hash covers `name + description + body + tools`. Skill bodies are normalised before hashing (CRLF → LF; trailing whitespace stripped) so editor drift doesn't cause spurious mismatches.

See `help(topic="content-signatures")` from inside an agent run for the full operator narrative.

### Transcript first-cycle types (v0.9.1)

Every run's persisted transcript now records two events that describe **what the agent actually received** before any model output:

- **`system_prompt`** — the resolved system prompt (AgentDef body + skill bodies, after overlay + merge), with provenance (`agent_def_id` + `skill_def_ids` map).
- **`user_input`** — the caller's `segments` from the original `POST /v1/runs`.

Surface them via `getTranscript(sessionId)` and narrow on `event.type`:

```ts
import type {
  SystemPromptPayload,
  TranscriptEvent,
  UserInputPayload,
} from "@loomcycle/client";

const { events } = await client.getTranscript(sessionId);

for (const ev of events as TranscriptEvent[]) {
  if (ev.type === "system_prompt") {
    const p = ev.payload as SystemPromptPayload;
    console.log(`prompt (def_id=${p.agent_def_id ?? "-"}): ${p.system_prompt.slice(0, 80)}...`);
    if (p.skill_def_ids) {
      for (const [skill, defId] of Object.entries(p.skill_def_ids)) {
        console.log(`  skill ${skill} resolved to def_id=${defId}`);
      }
    }
  } else if (ev.type === "user_input") {
    const segs = ev.payload as UserInputPayload[];
    console.log(`caller sent ${segs.length} segment(s):`);
    for (const seg of segs) {
      const firstText = seg.content.find((c) => c.type.endsWith("text"))?.text ?? "";
      console.log(`  [${seg.role}] ${firstText.slice(0, 80)}`);
    }
  }
}
```

These events are part of the persisted transcript (not the live `runStreaming` event channel — they fire before the first model call, before the SSE stream consumer typically attaches). Existing transcript readers that don't know the new types see them as `event: unknown` with the typed body in `payload` and ignore them safely.

## Errors

Non-2xx responses throw typed subclasses of `LoomcycleError`. The original HTTP status is on `e.status`; the truncated response body is on `e.bodyText` (≤1 KiB).

When the error body is JSON with a string `code` (`token_limit_exceeded`, `per_user_quota_exhausted`, `model_not_allowed`, …), that code is on `e.code`, whichever class the status maps to. A status is shared between unrelated conditions — a 429 is backpressure, a per-user quota or a token budget — so branch on `e.code` when you need to tell them apart. It is `undefined` for a plain-text body.

| HTTP status / body | Exception class |
|---|---|
| 400 | `InvalidArgumentError` |
| 401 | `AuthError` |
| 403 + code `insufficient_scope` | `InsufficientScopeError` (not retryable; `e.requiredScope` names the scope the token lacks — from the body's `required_scope`, or from `WWW-Authenticate` on an older server that sent plain text) |
| 403 + code `acl_denied` | `AclDeniedError` (an access-control rule refused the call; `e.action`, `e.resource` and `e.reasons` — the rules that decided, denies first, empty when none matched; not retryable) |
| 403 (other) | `LoomcycleError` (base; e.g. `restricted_token`, `operator_key_restricted`, `acl_self_elevation`, `authz_subject` on `e.code`) |
| 404 + "snapshot" | `SnapshotNotFoundError` ⎫ |
| 404 + "session" | `SessionNotFoundError` ⎬ all extend `NotFoundError` |
| 404 + "agent" | `AgentNotFoundError` ⎬ |
| 404 (other) | `NotFoundError` (base) ⎭ catch any 404 with one `instanceof` |
| 409 + code `group_external` / `group_exists` / `group_member_exists` / `acl_exists` / `acl_binding_exists` | `LoomcycleError` (base; matched by code before the keywords below, which a group named e.g. `session-ops` would trip) |
| 409 + "already_pausing" / "already paused" | `AlreadyPausingError` |
| 409 + "not_paused" / "not paused" | `NotPausedError` |
| 409 + "session" | `SessionBusyError` |
| 409 + "agent_id" | `AgentIDInUseError` |
| 409 (other) | `LoomcycleError` (base) |
| 413 + code `snapshot_too_large` | `SnapshotTooLargeError` ⎫ |
| 413 + code `prompt_too_large` | `PromptTooLargeError` ⎬ all are `instanceof SnapshotTooLargeError` (back-compat) |
| 413 (other) | `RequestTooLargeError` ⎭ |
| 422 | `SnapshotVersionError` |
| 429 + code `per_user_quota_exhausted` | `PerUserQuotaExhaustedError` (retryable; `e.retryAfterMs` from `Retry-After`) |
| 429 + code `token_limit_exceeded` | `TokenLimitExceededError` (NOT a `BackpressureError`; not retryable) |
| 429 + code `rate_limited` | `RateLimitedError` (subclass of `BackpressureError`; retryable; `e.retryAfterMs` from `Retry-After`) |
| 429 + code `configured_run_cap` | `BackpressureError` (NOT retryable: start or discard a configured run first) |
| 429 (other) | `BackpressureError` (retryable) |
| 503 + "pause manager not configured" | `PauseNotConfiguredError` (subclass of `UnavailableError` — back-compat) |
| 503 (other) | `UnavailableError` |
| 500 / other | `LoomcycleError` (base) |

**`e.retryable`: can the same call succeed later?** Every error carries a read-only `retryable`. It is `true` only for a refusal that clears on its own: a `BackpressureError` (the server or a provider queue is busy), a `RateLimitedError` (the caller is starting runs faster than its run-start rate allows), a `PerUserQuotaExhaustedError` (the caller's own in-flight runs are at their cap), a 503 `UnavailableError` with code `runtime_paused` (the operator has paused the runtime), and a `decide()` call that failed (502 `call_failed`) or timed out (504 `timeout`). It is `false` for everything else, a hard token budget included, and so is a `BackpressureError` with code `configured_run_cap`: the user already holds as many configured runs as allowed, and waiting never makes room — start or discard one first. A 503 is shared between a paused runtime and features the deployment does not have (`decision_not_configured`, `store_unavailable`, …), and many 503 bodies carry no code, so only `runtime_paused` counts; a code-less 503 from run creation can also be a model tier with no provider available, which you may retry on your own judgement. The default is `false` because a wrong `true` turns a refusal that never clears into an endless loop. The client never retries on its own, so bound your loop even when `retryable` is true, and prefer `e.retryAfterMs` where the class has it.

**429: a token budget is not backpressure.** A `BackpressureError` is transient: back off and retry. `TokenLimitExceededError` is not: the caller is at a hard token budget (run creation and `decide()` both refuse with it), and the same call is refused until the budget is raised or the calendar month rolls over. **It is no longer a `BackpressureError`** (it was, through 1.112.x). It extended that class so that an older `instanceof BackpressureError` check kept catching it, but such a check is nearly always a retry loop, which then retried the refusal for the rest of the month; that relation was dropped on purpose. If your code caught the budget through `instanceof BackpressureError`, catch `TokenLimitExceededError` (or test `e.code === "token_limit_exceeded"`) instead.

```ts
import {
  LoomcycleError,
  PerUserQuotaExhaustedError,
  RateLimitedError,
  TokenLimitExceededError,
} from "@loomcycle/client";

for (let attempt = 0; ; attempt++) {
  try {
    for await (const ev of client.runStreaming({ /* ... */ })) {}
    break;
  } catch (e) {
    if (e instanceof TokenLimitExceededError) {
      // Not retryable. e.scope is "operator" | "tenant" | "user"; e.scopeId is
      // the tenant or user (null for the operator-wide budget); e.used and e.limit
      // are tokens; e.window is "month". Each is null if the server did not send it.
      console.error(`token budget reached for ${e.scope}: ${e.used}/${e.limit} this ${e.window}`);
      throw e;
    }
    if (!(e instanceof LoomcycleError) || !e.retryable || attempt >= 5) throw e;
    // These two carry the server's own hint; otherwise back off exponentially.
    const hint =
      e instanceof RateLimitedError || e instanceof PerUserQuotaExhaustedError
        ? e.retryAfterMs
        : null;
    await new Promise((r) => setTimeout(r, hint ?? 1000 * 2 ** attempt));
  }
}
```

**413: three classes, one legacy base.** Every 413 used to throw `SnapshotTooLargeError`, whatever was too large. A 413 is now named for its cause: `PromptTooLargeError` for a `decide()` request that does not fit the model's context (shorten `state` or ask fewer questions), `RequestTooLargeError` for any other non-snapshot 413 (a request body over the server's limit, a memory value or quota, a channel payload — `e.code` says which), and `SnapshotTooLargeError` itself for a snapshot over its cap. `PromptTooLargeError` extends `RequestTooLargeError`, which extends `SnapshotTooLargeError`, so an existing `instanceof SnapshotTooLargeError` still matches every 413; that relation is for compatibility and says nothing about snapshots. To single out a snapshot that is too large, test `e.code === "snapshot_too_large"`.

`e.name` (and the `meta_reason` of a `stream_close` frame) is the most specific class name, so it reads `TokenLimitExceededError`, `RateLimitedError`, `PromptTooLargeError` or `RequestTooLargeError` where it used to read `BackpressureError` or `SnapshotTooLargeError`. Match with `instanceof` or `e.code`, not on the name.

Priority within `404`: most-specific keyword wins (`snapshot` → `session` → `agent` → base). The dispatch is keyword-matched on the response body lowercase, so a body that names none of them — a missing hook, memory row or interrupt — is a plain `NotFoundError`, while one that happens to contain "agent" (a hook id like `hook_agent_scan`) is an `AgentNotFoundError`. There is no hook-specific class; catch `NotFoundError` for any 404.

```ts
import {
  BackpressureError,
  SnapshotNotFoundError,
  LoomcycleError,
} from "@loomcycle/client";

try {
  for await (const ev of client.runStreaming({ /* ... */ })) {}
} catch (e) {
  if (e instanceof BackpressureError) {
    console.warn(`loomcycle backpressure (status=${e.status}): ${e.message}`);
  } else if (e instanceof LoomcycleError) {
    console.error(`loomcycle error ${e.status}: ${e.bodyText}`);
  } else {
    throw e;
  }
}
```

## Patterns

A short field guide for the common consumer shapes — when to use which method, what each one costs, and how the v0.9.x polish hooks (`debug`, `parentAgentId`) fit in.

### Sync vs async run consumption

`runStreaming` and `continueSession` are **sync**: the iterator stays alive for the FULL duration of the run. Use them when:

- You have a single agent run and want to render its output progressively (UI streaming, CLI tail-like display).
- The caller can hold a connection per active run without worker-thread starvation.

For async fire-and-forget patterns (the n8n trigger node's model), use `streamUserRunStates` instead:

```ts
// Don't do this in an n8n worker — blocks the worker for the full run:
for await (const ev of client.runStreaming({ agent: "long-task", segments })) { ... }

// Do this instead — kick off the run, get back a tracking ID, and watch run-state transitions:
const seedRun = await runOnce(...);  // your one-shot dispatch
for await (const item of client.streamUserRunStates(userId, {
  statuses: ["completed", "failed", "cancelled"],
})) {
  if (item.kind === "event" && item.payload.agent_id === seedRun.agentId) {
    // fire downstream workflow, persist to DB, etc.
    break;
  }
}
```

`streamUserRunStates` holds ONE connection per user regardless of how many concurrent runs that user has. Server-enforced 30-minute timeout; reconnect on close.

### `walkId` — watching ONE team walk's agents (v1.78.0)

A detached team walk returns a `run_id`, and that id **is** the walk id every run it spawns carries in `parent_context.walk_id`. Pass it back as `walkId` and the server filters the stream to that workflow — no second identifier, nothing to map:

```ts
const { run_id } = await client.runTeam({ name: "triage", mode: "detach" });

for await (const item of client.streamUserRunStates(userId, { walkId: run_id })) {
  if (item.kind === "open") {
    // The server echoes the filter it applied. Check it: a filter that matched
    // nothing and a filter the server did not understand are both a quiet
    // stream, and only this tells them apart.
    if (item.payload.filter_walk_id !== run_id) throw new Error("walk filter not applied");
    continue;
  }
  if (item.kind !== "event") continue;
  const pc = item.payload.parent_context;
  // wave_id groups one fan-out; wave_index is the position inside it, so a
  // live view can place an agent WITHIN the walk, not merely inside it.
  render(item.payload.agent, item.payload.status, pc?.wave_id, pc?.wave_index);
}
```

Unlike `parentAgentId`, which the adapter applies after parsing each frame, `walkId` is applied by the server — it reduces what crosses the wire, not just what your callback sees.

### `debug: true` — synthetic open/close frames

All three streaming methods (`runStreaming`, `continueSession`, `streamUserRunStates`) accept `debug?: boolean`. Default off; behaviour is exactly the pre-v0.9.x shape.

When `debug: true`:
- `runStreaming` / `continueSession` brackets the real events with `{ type: "_meta", meta_subtype: "stream_open" | "stream_close", meta_reason }` frames. The leading-underscore type signals "client-synthesized; never on the wire." The `meta_reason` is `"eof"` on clean close or an error class name (e.g. `"AuthError"`) when the inner iterator threw mid-stream.
- `streamUserRunStates` yields an extra `{ kind: "close", payload: { reason } }` item on stream end (in addition to the existing `kind: "open" | "event"` frames).

```ts
for await (const ev of client.runStreaming({ agent: "qa", segments, debug: true })) {
  if (ev.type === "_meta") {
    console.log(`[stream ${ev.meta_subtype}] reason=${ev.meta_reason}`);
    continue;
  }
  // ... handle real events
}
```

Use case: n8n trigger nodes that surface "stream re-opened / closed" log entries to the operator without inferring from event timing. Non-n8n consumers don't need to know the toggle exists.

### `parentAgentId` — client-side narrowing

`listUserAgents(userId, { parentAgentId })` and `streamUserRunStates(userId, { parentAgentId })` apply a client-side filter on the run's `parent_agent_id`. The server still returns / streams the full set; the adapter trims before yielding.

```ts
// All sub-runs spawned by a specific parent (one-shot snapshot)
const subRuns = await client.listUserAgents(userId, {
  parentAgentId: "ag_parent_abc",
  status: "running",
});

// Same shape, but as a live stream
for await (const item of client.streamUserRunStates(userId, {
  parentAgentId: "ag_parent_abc",
  statuses: ["completed", "failed"],
})) {
  // Only events whose payload.parent_agent_id === "ag_parent_abc"
  // (open and close frames always pass through).
}
```

**Cost note:** because the filter is client-side, the server doesn't shed any load. If the result set is large enough that you care about server-side narrowing, raise an issue — server-side `?parent_agent_id=` is a planned addition.

## Why HTTP, not gRPC

Loomcycle's HTTP+SSE surface is the canonical wire contract — every gRPC RPC has an HTTP equivalent (see `internal/api/http/server.go` for the route registrations). The Python adapter (gRPC) and this TS adapter (HTTP) cover the same surface; the choice between them is about ecosystem fit, not capability. HTTP+SSE works through every reverse proxy without special config; gRPC needs HTTP/2 + protoc round trips. For Node.js orchestrators that already have `fetch` in scope, HTTP is the simpler dependency.

## Development

```bash
cd adapters/ts
npm install
npm run typecheck     # tsc --noEmit
npm run build         # tsc → dist/
npm test              # vitest run
npm run test:watch    # vitest --watch
```

Tests use Vitest with a Node environment. They mock `fetch` via constructor injection (no global monkey-patching). See `tests/helpers.ts` for the request-mock pattern.

## License

Apache-2.0.
