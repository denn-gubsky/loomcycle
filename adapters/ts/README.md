# @loomcycle/client

TypeScript client for the [loomcycle](https://github.com/denn-gubsky/loomcycle) sidecar — the agentic-OS substrate for production agents.

`@loomcycle/client` speaks HTTP+SSE to the loomcycle server's `/v1/*` surface. The same operation surface is exposed via gRPC (`adapters/python/loomcycle`) and stdio MCP (`loomcycle mcp`); this client is the HTTP-side adapter, suitable for Node.js orchestrators, automation scripts, and operator tooling.

## Status

**v0.8.18** — full Python-adapter parity. 24 methods covering run streaming, agent metadata, transcript, pause/resume/state, snapshot lifecycle, memory admin, interruption resolve, and health.

## Install

```bash
npm install @loomcycle/client
```

Requires Node ≥ 18. Bun and Deno likely work but are untested. Browser support is not a target — for browser-side operator control, use loomcycle's built-in Web UI at `/ui`.

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
| `runStreaming(opts: RunOptions)` | `AsyncIterable<AgentEvent>` | Server-streams provider events for a fresh run. |
| `continueSession(opts: ContinueOptions)` | `AsyncIterable<AgentEvent>` | Continues an existing session. |

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

## Errors

Non-2xx responses throw typed subclasses of `LoomcycleError`. The original HTTP status is on `e.status`; the truncated response body is on `e.bodyText` (≤1 KiB).

| HTTP status / body | Exception class |
|---|---|
| 400 | `InvalidArgumentError` |
| 401 | `AuthError` |
| 404 + "snapshot" | `SnapshotNotFoundError` |
| 404 + "session" | `SessionNotFoundError` |
| 404 (other) | `AgentNotFoundError` (catch-all) |
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
