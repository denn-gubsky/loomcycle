# @loomcycle/client

TypeScript client for the [loomcycle](https://github.com/denn-gubsky/loomcycle) sidecar — the agentic-OS substrate for production agents.

`@loomcycle/client` speaks HTTP+SSE to the loomcycle server's `/v1/*` surface. The same operation surface is exposed via gRPC (`adapters/python/loomcycle`) and stdio MCP (`loomcycle mcp`); this client is the HTTP-side adapter, suitable for Node.js orchestrators, automation scripts, and operator tooling.

## Status

**v0.18.0** — 51 methods covering run streaming, agent metadata, transcript, pause/resume/state, snapshot lifecycle, memory admin, interruption resolve, hook registration, **v0.8.22 substrate admin (agentDef + skillDef)**, **v0.9.x n8n Phase 0 (listChannels + streamUserRunStates)**, **v0.9.x content_sha256**, **v0.9.x dynamic MCP server registration (mcpServerDef)** + **v0.18.0 typed `mcpServerDefVerify` + `ensureMcpServer` (idempotent register-if-changed)**, **v0.10.3 Library v2 enumeration (listLibraryAgents/Skills/McpServers)**, **v0.11.0 LLM Gateway (llmChat + llmStream)**, **v0.11.4 OpenAI Embeddings (embeddings)**, **v0.17.0 OSS multi-tenant auth (operatorTokenDef + whoami + tenant-scoped listUsers / listUserAgents — RFC L)**, and health.

> Migrating from raw `fetch` against `/v1/*`? See **[docs/MIGRATING-FROM-HTTP.md](./docs/MIGRATING-FROM-HTTP.md)** for a side-by-side walkthrough.

### What's new since v0.8.18

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
| `cancelAgent(agentId, opts?)` | `Promise<{ cancelledCount: number }>` | Cascades to children via `parent_agent_id`. Idempotent. |
| `listUserAgents(userId, opts?)` | `Promise<Agent[]>` | Optional filter by status (`running` / `completed` / `failed` / `cancelled`). |
| `getTranscript(sessionId)` | `Promise<TranscriptResponse>` | Persisted event log; one row per event with seq/run_id/ts_ns/type/event. |
| `health()` | `Promise<HealthResponse>` | Liveness probe. Hits `/healthz` (no `/v1` prefix). Unauthenticated. |
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

### Interruption (v0.8.16)

| Method | Returns | Notes |
|---|---|---|
| `listUserInterrupts(userId, opts?)` | `Promise<InterruptListResponse>` | Default filter: `status=pending`. |
| `listRunInterrupts(runId, opts?)` | `Promise<InterruptListResponse>` | Per-run interrupts. |
| `resolveInterrupt(runId, interruptId, opts: ResolveInterruptOptions)` | `Promise<unknown>` | Answer a pending interrupt. |

### Hook management (v0.8.18)

| Method | Returns | Notes |
|---|---|---|
| `registerHook(opts: RegisterHookOptions)` | `Promise<RegisterHookResponse>` | Register a pre- or post-tool webhook. Re-registering the same `(owner, name)` replaces in-place with a fresh id. Raises `InvalidArgumentError` on 400 (bad URL / phase / missing field). |
| `listHooks()` | `Promise<Hook[]>` | Every registered hook. **In-memory only — empty after a loomcycle restart.** |
| `deleteHook(id)` | `Promise<void>` | Raises `HookNotFoundError` on 404. |

Hook registration is one side; the other side is the **callback receiver** — a small HTTP endpoint your app runs at the URL you registered. The adapter exports the wire shapes (`PreHookCall` / `PostHookCall` / `PreHookResult` / `PostHookResult`) so you can type the handler against the same JSON loomcycle posts.

**Register from your app's startup:**

```ts
import { LoomcycleClient } from "@loomcycle/client";

const client = new LoomcycleClient({
  baseUrl: process.env.LOOMCYCLE_BASE_URL!,
  authToken: process.env.LOOMCYCLE_AUTH_TOKEN,
});

await client.registerHook({
  owner: "jobember-web",                     // (owner, name) is the identity tuple
  name: "scan-webfetch",                     // re-registering same pair replaces in place
  phase: "post",                             // "pre" or "post"
  tools: ["WebFetch"],                       // empty/omitted = all tools
  callbackUrl: "https://jobember.example/hooks/scan",
  failMode: "open",                          // "open" = errors pass through; "closed" = errors fail the tool call
  timeoutMs: 3000,                           // 0 = registry default (5s)
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

**Important constraints**:

- Hook registrations are **in-memory** on the loomcycle server. Re-register on every app startup; the `(owner, name)` idempotency contract makes this safe (replaces in place).
- Auth flows one-way: loomcycle → your callback URL. Loomcycle does NOT attach a bearer token to callback POSTs by default. If you need to authenticate the caller, validate by source IP or include a shared secret in the `callback_url` path/query (`https://jobember.example/hooks/scan?secret=...`).
- `fail_mode: "open"` (default) is right for telemetry hooks where a down receiver shouldn't break tool dispatch. `"closed"` is right for security hooks where a down receiver should fail the tool call (don't let bypassed payloads through).
- `allow_hosts` in `PreHookResult` is a **trust-sensitive surface** — it widens the agent's outbound network policy for one tool call. Server enforces an operator-yaml allowlist (`hooks.permit_host_widen.owners`); your owner has to be on that list for `allow_hosts` to take effect. See the SECURITY note in `internal/hooks/types.go` before using.

### Substrate admin: AgentDef + SkillDef (v0.8.22)

Two op-discriminated methods that mirror the in-process `AgentDef` / `SkillDef` built-in tools over HTTP. The same `op` values an agent's tool_use would invoke are reachable directly from your app code — useful for runtime fork / promote / retire / list, and for the `verify` op covered in [Content signatures](#content-signatures-v09x).

| Method | Returns | Notes |
|---|---|---|
| `agentDef(input)` | `Promise<SubstrateToolResponse>` | Op-discriminated. Mirrors `POST /v1/_agentdef`. |
| `skillDef(input)` | `Promise<SubstrateToolResponse>` | Op-discriminated. Mirrors `POST /v1/_skilldef`. |

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

Operations on AgentDef: `create` / `fork` / `get` / `list` / `promote` / `retire` / **`verify`** (v0.9.x). SkillDef has the same set minus `retire`'s edge cases. See `internal/tools/builtin/agentdef.go` for the canonical input schema; each op enforces the agent's `agent_def_scopes` / `skill_def_scopes` capability gate from the operator yaml.

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

### Channels + run-state stream (v0.9.x n8n Phase 0)

Two substrate-side surfaces added in the n8n integration's Phase 0 wire-API work. Useful for any orchestrator (not just n8n) that needs to see channel state or subscribe to run-state transitions.

| Method | Returns | Notes |
|---|---|---|
| `listChannels()` | `Promise<ListChannelsResponse>` | Operator-declared channels + aggregate stats (`message_count`, `oldest_visible_at`, `newest_visible_at`). Mirrors `GET /v1/_channels`. |
| `streamUserRunStates(userId, opts?)` | `AsyncIterable<RunStateStreamItem>` | SSE stream of run state transitions for one user. Yields one `{ kind: "open", ... }` frame then one `{ kind: "event", payload: RunStateEvent }` per matching transition until close. |

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
- Agent hash covers `name + description + system_prompt + allowed_tools + skills + model + provider + tier + effort + max_tokens + max_iterations + providers + models + memory_scopes + memory_quota_bytes`. Explicitly excluded: `def_id`, `version`, `created_at`, `retired`, **plus** `channels` and `*_scopes` (operator-yaml-only ACL fields that don't round-trip through `set` / `fork`).
- Skill hash covers `name + description + body + allowed_tools`. Skill bodies are normalised before hashing (CRLF → LF; trailing whitespace stripped) so editor drift doesn't cause spurious mismatches.

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

| HTTP status / body | Exception class |
|---|---|
| 400 | `InvalidArgumentError` |
| 401 | `AuthError` |
| 404 + "snapshot" | `SnapshotNotFoundError` ⎫ |
| 404 + "session" | `SessionNotFoundError` ⎬ all extend `NotFoundError` |
| 404 + "hook" | `HookNotFoundError` ⎬ |
| 404 + "agent" | `AgentNotFoundError` ⎬ |
| 404 (other) | `NotFoundError` (base) ⎭ catch any 404 with one `instanceof` |
| 409 + "already_pausing" / "already paused" | `AlreadyPausingError` |
| 409 + "not_paused" / "not paused" | `NotPausedError` |
| 409 + "session" | `SessionBusyError` |
| 409 + "agent_id" | `AgentIDInUseError` |
| 409 (other) | `LoomcycleError` (base) |
| 413 | `SnapshotTooLargeError` |
| 422 | `SnapshotVersionError` |
| 429 | `BackpressureError` |
| 503 + "pause manager not configured" | `PauseNotConfiguredError` (subclass of `UnavailableError` — back-compat) |
| 503 (other) | `UnavailableError` |
| 500 / other | `LoomcycleError` (base) |

Priority within `404`: most-specific keyword wins (`snapshot` → `session` → `hook` → `agent` → base). The dispatch is keyword-matched on the response body lowercase; a hook with id `hook_agent_scan` still routes to `HookNotFoundError`, not `AgentNotFoundError`.

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
