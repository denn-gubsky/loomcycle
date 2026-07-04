# Migrating from direct HTTP to `@loomcycle/client`

This guide is for callers who currently speak HTTP+SSE to loomcycle directly via `fetch` (typed by hand, hooks registered with `POST /v1/hooks`) and want to switch to the official TypeScript adapter.

The adapter is a thin wrapper. There is **no new wire protocol**; the same HTTP+SSE endpoints are exercised. The benefit is typed inputs/outputs, typed errors, `AbortSignal` plumbing, and an SSE parser that doesn't lose trailing frames — all things you'd otherwise hand-roll. Migration is incremental: the adapter and direct `fetch` calls coexist fine, so you can move one endpoint at a time.

## Why migrate (and when not to)

**Reasons to move**:

- **Typed error dispatch.** `catch (e: BackpressureError)` reads better than `if (resp.status === 429)` and your code paths don't bit-rot when a new HTTP status gets added.
- **Cancellation.** Every method takes `signal?: AbortSignal`; the streaming methods stop cleanly on abort.
- **Hook callback receiver typing.** The `PreHookCall` / `PostHookCall` / `PreHookResult` / `PostHookResult` types are exported, so your webhook handler is type-safe against the same shapes the server emits.
- **SSE parser correctness.** The adapter's `parseSSE` handles partial chunks, mid-line splits, and trailing frames without a final newline. Hand-rolled parsers commonly miss the trailing case.
- **One source of truth for snake_case ↔ camelCase translation.** Method parameters use camelCase (JS norm); request bodies use snake_case (Go server norm). The adapter is the one place that translation lives.

**Reasons to wait**:

- You're on Node < 18. The adapter targets Node ≥ 18 (Bun/Deno likely work, untested).
- You have a fully-working vendored copy and migrating just creates churn with no functional change. Migrate when you next need a new method (e.g., hooks via `registerHook`, snapshot via `getSnapshot`), and bring the rest along.
- Browser code. The adapter isn't designed for browsers; use the Web UI at `/ui` for operator surfaces or call the HTTP API directly from the server.

## Install

```bash
npm install @loomcycle/client
```

Peer requirements: Node ≥ 18 (uses global `fetch` + `AbortController`).

## Construction

| Direct HTTP | TS adapter |
|---|---|
| `const baseUrl = process.env.LOOMCYCLE_BASE_URL!` | `const client = new LoomcycleClient({` |
| `const token = process.env.LOOMCYCLE_AUTH_TOKEN!` | `  baseUrl: process.env.LOOMCYCLE_BASE_URL!,` |
| (auth header threaded into every `fetch`) | `  authToken: process.env.LOOMCYCLE_AUTH_TOKEN,` |
| | `});` |

Share one `LoomcycleClient` per process. The class is stateless beyond the constructor args; concurrent calls are safe. There's no connection-pool tuning to do — Node's global `fetch` reuses the underlying HTTP/2 connection.

## Method-by-method translation

### Run streaming

```ts
// Before — direct HTTP
const resp = await fetch(`${baseUrl}/v1/runs`, {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    Authorization: `Bearer ${token}`,
    Accept: "text/event-stream",
  },
  body: JSON.stringify({
    agent: "qa-agent",
    segments,
    tools: tools,
    user_id: userId,
  }),
});
if (!resp.ok) throw new Error(`run failed: ${resp.status}`);
// ... hand-rolled SSE parse loop reading resp.body.getReader() ...
```

```ts
// After — adapter
for await (const ev of client.runStreaming({
  agent: "qa-agent",
  segments,
  tools: tools,
  userId,
})) {
  // typed AgentEvent — switch on ev.type
}
```

**Behavioural notes**:

- The `Accept` header is `text/event-stream, application/json` (both — error responses come back as JSON; the adapter handles either content-type and dispatches to typed errors when non-2xx).
- Synthetic registration frames (the `session` and `agent` frames the server emits before the first provider event) currently surface as `ev.type === "session"` / `"agent"` events — the adapter does not swallow them like the Python `RunHandle` capture does. If your code branches on event types it doesn't recognise, expect those.
- Errors raised by the loop (network drop, server 4xx) throw typed errors. Errors **inside** the run (a tool failed) surface as `{ type: "error", error: "..." }` events — same as the wire.

### Continue an existing session

```ts
// Before
fetch(`${baseUrl}/v1/sessions/${encodeURIComponent(sessionId)}/messages`, {
  method: "POST",
  headers: { /* ... */ },
  body: JSON.stringify({ segments, tools, agent_id }),
});

// After
client.continueSession({
  sessionId,
  segments,
  tools,
  agentId,           // optional pin to a specific running agent
});
```

Raises `SessionNotFoundError` (404) or `SessionBusyError` (409) when applicable — no need to status-code-switch.

### Agent metadata

```ts
// Before
const r = await fetch(`${baseUrl}/v1/agents/${id}`, { headers });
if (r.status === 404) throw new MissingAgent(id);
const agent = await r.json();

// After
import { AgentNotFoundError } from "@loomcycle/client";
try {
  const agent = await client.getAgent(id);
} catch (e) {
  if (e instanceof AgentNotFoundError) throw new MissingAgent(id);
  throw e;
}
```

Same shape for `cancelAgent(id, { reason })`, `listUserAgents(userId, { status })`, `getTranscript(sessionId)`.

### Hooks (the marquee migration target)

Before (~30 LOC including idempotency retry logic):

```ts
// register-hooks.ts (vendored pattern)
async function registerHook(spec: HookSpec) {
  const resp = await fetch(`${baseUrl}/v1/hooks`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({
      owner: spec.owner,
      name: spec.name,
      phase: spec.phase,
      tools: spec.tools,
      callback_url: spec.callbackUrl,
      fail_mode: spec.failMode ?? "open",
      timeout_ms: spec.timeoutMs ?? 5000,
    }),
  });
  if (!resp.ok) {
    const body = await resp.text();
    throw new Error(`hook register failed: ${resp.status} ${body}`);
  }
  const { id } = await resp.json();
  return id;
}
```

After:

```ts
const { id } = await client.registerHook({
  owner: "jobember-web",
  name: "scan-webfetch",
  phase: "post",
  tools: ["WebFetch"],
  callbackUrl: "https://jobember.example/hooks/scan",
  failMode: "open",
  timeoutMs: 5000,
});
```

Re-registering with the same `(owner, name)` replaces in-place with a fresh `id` (server-side idempotency), so you don't need wrapping logic. Just call `registerHook` on every app startup.

**Callback receiver** is the half the adapter doesn't run — it's your HTTP framework. The adapter exports the types so you can write a fully-typed handler:

```ts
// app/hooks/scan/route.ts (Next.js)
import { NextResponse } from "next/server";
import type { PostHookCall, PostHookResult } from "@loomcycle/client";

export async function POST(req: Request) {
  const body = (await req.json()) as PostHookCall;

  // ... do your scan ...

  const reply: PostHookResult = { /* {} = pass through */ };
  return NextResponse.json(reply);
}
```

For Pre-hooks, the result type is `PreHookResult` and you can rewrite `input`, `deny` with a synthetic result, or grant `allow_hosts` for one tool call.

### Pause / Resume / Snapshot

```ts
// Before
await fetch(`${baseUrl}/v1/_pause`, {
  method: "POST",
  headers: { /* ... */ },
  body: JSON.stringify({ timeout_ms: 30_000 }),
});

// After
import { AlreadyPausingError, PauseNotConfiguredError } from "@loomcycle/client";
try {
  const result = await client.pauseRuntime({ timeoutMs: 30_000 });
  console.log(`paused in ${result.duration_ms}ms, force-cancelled ${result.force_cancelled_count}`);
} catch (e) {
  if (e instanceof AlreadyPausingError) {
    // idempotent — fine to no-op
  } else if (e instanceof PauseNotConfiguredError) {
    // operator hasn't configured pause; either configure or skip
  } else throw e;
}
```

Same pattern for `resumeRuntime`, `getRuntimeState`, `createSnapshot`, `listSnapshots`, `getSnapshot`, `restoreSnapshot`, `deleteSnapshot`.

`exportSnapshotURL(id)` is **synchronous** — returns the download URL string. Suitable for `<a href>` or a server-side `fetch`-with-Authorization download to a file.

### Memory + Interruption

```ts
// Memory admin
await client.listMemoryScopes();
await client.listMemoryScopeIDs("agent");
await client.listMemoryEntries("agent", "scout-1", { prefix: "events/", limit: 50 });
await client.getMemoryEntry("user", userId, "preferences/last_seen");

// Interruption
await client.listUserInterrupts(userId, { status: "pending" });
await client.resolveInterrupt(runId, interruptId, { answer: "yes", resolvedBy: "dashboard" });
```

All raise typed errors for the documented failure modes; see [README.md#errors](../README.md#errors).

## Error handling

Direct `fetch`:

```ts
const resp = await fetch(url, opts);
if (!resp.ok) {
  if (resp.status === 429) await sleep(2_000), retry();
  else if (resp.status === 401) throw new AuthFailure();
  else throw new Error(`loomcycle ${resp.status}`);
}
```

Adapter:

```ts
import {
  BackpressureError,
  AuthError,
  NotFoundError,
  LoomcycleError,
} from "@loomcycle/client";

try {
  /* ... */
} catch (e) {
  if (e instanceof BackpressureError) { await sleep(2_000); retry(); }
  else if (e instanceof AuthError) { throw new AuthFailure(); }
  else if (e instanceof NotFoundError) { /* any 404 — agent, session, hook, snapshot, or generic */ }
  else if (e instanceof LoomcycleError) { throw new Error(`loomcycle ${e.status}: ${e.bodyText}`); }
  else { throw e; } // non-loomcycle errors propagate as-is
}
```

The error hierarchy is: `Error` → `LoomcycleError` → (concrete subclasses). `NotFoundError` is a base so you can catch any 404 with one `instanceof`; the specific subclasses (`AgentNotFoundError`, `SessionNotFoundError`, `HookNotFoundError`, `SnapshotNotFoundError`) also extend it.

## Production patterns

### Shared client per process

```ts
// src/lib/loomcycle.ts
import { LoomcycleClient } from "@loomcycle/client";

let _client: LoomcycleClient | null = null;
export function loomcycle(): LoomcycleClient {
  if (!_client) {
    _client = new LoomcycleClient({
      baseUrl: process.env.LOOMCYCLE_BASE_URL!,
      authToken: process.env.LOOMCYCLE_AUTH_TOKEN,
    });
  }
  return _client;
}
```

There's no shutdown logic to write — the underlying `fetch` releases connections on its own.

### Retry on backpressure

`BackpressureError` is the documented "wait + retry" signal. A bounded exponential backoff is the standard pattern:

```ts
import { BackpressureError } from "@loomcycle/client";

async function withBackpressureRetry<T>(fn: () => Promise<T>, max = 3): Promise<T> {
  for (let i = 0; i < max; i++) {
    try { return await fn(); }
    catch (e) {
      if (!(e instanceof BackpressureError) || i === max - 1) throw e;
      await new Promise(r => setTimeout(r, 500 * 2 ** i));
    }
  }
  throw new Error("unreachable");
}

await withBackpressureRetry(() => client.cancelAgent(id, { reason: "user" }));
```

Do **not** retry on `AuthError`, `InvalidArgumentError`, or any of the `*NotFoundError` family — those are caller errors, not transient.

### Timeout per request

```ts
const ac = new AbortController();
const timer = setTimeout(() => ac.abort(), 30_000);
try {
  const agent = await client.getAgent(id, { signal: ac.signal });
} finally {
  clearTimeout(timer);
}
```

For streaming methods, the abort also breaks out of the `for await` loop.

### Logging the typed error correctly

Every typed error carries `.status` (HTTP code) and `.bodyText` (server response body, truncated to 1 KiB). When you log:

```ts
catch (e) {
  if (e instanceof LoomcycleError) {
    logger.warn({ status: e.status, body: e.bodyText, name: e.name }, "loomcycle call failed");
  } else throw e;
}
```

`e.bodyText` is the source of truth for the actual server message. Don't reach for `e.message` — that's a derived string and may be truncated.

## Migration sequence (recommended)

For a vendored-copy consumer like JobEmber:

1. **Add the npm dep, leave the vendored copy in place.**
   ```bash
   npm install @loomcycle/client
   ```
2. **Migrate one call site at a time** to the new client. The two coexist fine because they speak the same wire.
3. **Migrate hooks first** — `registerHook` + the typed callback receiver is the biggest delta vs hand-rolled.
4. **Migrate streaming next** — `runStreaming` / `continueSession`. Be careful to preserve any local SSE-parser patches you have (the adapter's parser handles trailing-frame and partial-chunk correctly, but if you've added custom event types you have to either union them into `EventType` upstream or branch in your consumer).
5. **Migrate metadata RPCs** (`getAgent` / `cancelAgent` / `listUserAgents` / `getTranscript` / `health`) — these are mechanical 1:1 swaps.
6. **Delete the vendored copy** when no call site references it.
7. **Type your callback handlers** using `PreHookCall` / `PostHookCall` / `PreHookResult` / `PostHookResult` from `@loomcycle/client`.

### Forward-compat patches

If your vendored copy has local patches (custom event types, extra wire fields), the migration is the right time to **either**:

- **Upstream them**: open a PR against `adapters/ts/` in loomcycle. Anything that's a real wire-shape addition that the server emits SHOULD be in the typed adapter for everyone.
- **Wrap them at your boundary**: keep your patches in a thin file in your repo that re-exports the adapter's types with your additions (`export type { AgentEvent, ToolUse } from "@loomcycle/client"; export interface AgentEventExt extends AgentEvent { my_field?: T }`). Avoids drift the next time the adapter version bumps.

## Sanity checks after migration

Drop these in your test suite or run them once manually:

```ts
// 1. The shared client is constructable
const c = loomcycle();
expect(c).toBeInstanceOf(LoomcycleClient);

// 2. Health round-trip
const h = await c.health();
expect(h.ok).toBe(true);

// 3. 404 dispatch (one call site you know returns 404)
await expect(c.getAgent("definitely-not-real")).rejects.toBeInstanceOf(AgentNotFoundError);

// 4. Hook idempotency
const r1 = await c.registerHook({ owner: "test", name: "x", phase: "post", tools: ["WebFetch"], callbackUrl: "https://e.test/h" });
const r2 = await c.registerHook({ owner: "test", name: "x", phase: "post", tools: ["WebFetch"], callbackUrl: "https://e.test/h" });
expect(r1.id).not.toEqual(r2.id); // re-register mints a fresh id
const all = await c.listHooks();
expect(all.filter(h => h.owner === "test").length).toBe(1); // but only one row
await c.deleteHook(r2.id);
```

## Known limitations and footguns

- **Hook registrations don't survive a loomcycle restart.** They're in-memory on the server. Register on every app startup; the `(owner, name)` idempotency makes this safe.
- **Callback receiver auth is your problem.** Loomcycle posts to your `callback_url` without a bearer by default. Validate by source IP or include a shared secret in the path/query if it matters.
- **`exportSnapshotURL` returns a URL but does not download.** The endpoint still requires `Authorization: Bearer <token>` — you have to attach the header when actually fetching the URL (or use a tool that does, like a server-side download proxy).
- **No client-side rate limiting.** If you hammer the server, `BackpressureError` is what you'll get. Either back off explicitly (see the retry pattern above) or hold a semaphore in your code. The Go server's semaphore is the floor, not a substitute for caller-side discipline.
- **No browser support.** ESM-only, Node ≥ 18 targets. CJS shim is a v0.9.x candidate; browser is not a target.

## Getting help

When you hit a gap not covered here: open an issue on the loomcycle repo with the call site and the question. If it's a real API confusion, the answer ends up in this guide.
