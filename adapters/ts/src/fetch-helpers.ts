/**
 * Shared HTTP plumbing for LoomcycleClient. Three responsibilities:
 *
 *   1. Build the Authorization header from the client's bearer token.
 *   2. JSON encode/decode the request + response.
 *   3. Map non-2xx responses to typed errors from errors.ts (single
 *      source of truth — mirrors Python's _raise_from_grpc).
 *
 * Method-level code in client.ts stays focused on URL + body shape;
 * the boring fetch + error-translation machinery lives here.
 */

import {
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  HookNotFoundError,
  InvalidArgumentError,
  LoomcycleError,
  NotFoundError,
  NotPausedError,
  PauseNotConfiguredError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  SubstrateToolRefusedError,
  UnavailableError,
} from "./errors.js";

/**
 * @internal — not part of @loomcycle/client's public API surface.
 *
 * Snapshot of the LoomcycleClient constructor inputs threaded through
 * the helpers below. The leading underscore + this docstring signal
 * "internal type; consumers MUST NOT depend on it". TypeScript's
 * declaration emit still publishes it under dist/fetch-helpers.d.ts
 * (no --stripInternal in tsconfig today), but the rename + comment
 * make accidental dependence implausible: a consumer doing
 * `import type { _FetchContext } from "@loomcycle/client"` would have
 * to deliberately reach for a name marked internal, which is a
 * "contract violation" gesture rather than a casual import. The
 * fields here (authToken in plaintext, fetchImpl) are implementation
 * details that may change without notice. */
export interface _FetchContext {
  baseUrl: string;
  authToken: string | undefined;
  fetchImpl: typeof fetch;
}

/** authHeaders builds the standard request header set: JSON Accept
 *  + Bearer token when the client was constructed with one. The
 *  caller adds Content-Type when posting a body. */
export function authHeaders(ctx: _FetchContext): Record<string, string> {
  const h: Record<string, string> = { Accept: "application/json" };
  if (ctx.authToken) h.Authorization = `Bearer ${ctx.authToken}`;
  return h;
}

/** jsonFetch performs a GET and unwraps the JSON body. Non-2xx
 *  status maps to a typed error via raiseFromResponse. */
export async function jsonFetch<T>(
  ctx: _FetchContext,
  path: string,
  opts?: { signal?: AbortSignal },
): Promise<T> {
  const resp = await ctx.fetchImpl(ctx.baseUrl + path, {
    method: "GET",
    headers: authHeaders(ctx),
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
  return (await resp.json()) as T;
}

/** postJSON sends a JSON-encoded body and unwraps the response.
 *  When `body` is undefined, no body is sent (Content-Type
 *  omitted). */
export async function postJSON<T>(
  ctx: _FetchContext,
  path: string,
  body?: unknown,
  opts?: { signal?: AbortSignal },
): Promise<T> {
  const headers = authHeaders(ctx);
  let bodyStr: string | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    bodyStr = JSON.stringify(body);
  }
  const resp = await ctx.fetchImpl(ctx.baseUrl + path, {
    method: "POST",
    headers,
    body: bodyStr,
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
  // Some endpoints return 204 No Content; tolerate that with a
  // null cast — typed methods that know they return 204 use a
  // void wrapper instead.
  if (resp.status === 204) return null as T;
  return (await resp.json()) as T;
}

/** deleteRequest sends a DELETE and tolerates 204/200/404-with-
 *  idempotent-semantics per the loomcycle wire contract. */
export async function deleteRequest(
  ctx: _FetchContext,
  path: string,
  opts?: { signal?: AbortSignal },
): Promise<void> {
  const resp = await ctx.fetchImpl(ctx.baseUrl + path, {
    method: "DELETE",
    headers: authHeaders(ctx),
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
}

/**
 * raiseFromResponse — the single point where HTTP status + body
 * text get mapped to typed errors. Always throws; the function
 * signature returns `never` only because TypeScript needs the
 * return type for control-flow narrowing.
 *
 * Mapping table:
 *
 *   400         → InvalidArgumentError
 *   401         → AuthError
 *   404 + "snapshot"   → SnapshotNotFoundError ────────┐
 *   404 + "session"    → SessionNotFoundError          │  All extend
 *   404 + "hook"       → HookNotFoundError             │  NotFoundError —
 *   404 + "agent"      → AgentNotFoundError            │  callers can
 *   404 + (other)      → NotFoundError (base)          │  catch any 404
 *                                                      │  with one
 *                                                      │  instanceof.
 *   409 + "already_pausing" / "already paused" → AlreadyPausingError
 *   409 + "not_paused" / "not paused"          → NotPausedError
 *   409 + "session"                            → SessionBusyError
 *   409 + "agent_id"                           → AgentIDInUseError
 *   409 + (other)                              → LoomcycleError (base)
 *   413         → SnapshotTooLargeError
 *   422         → SnapshotVersionError (snapshot-version-too-new/unknown)
 *   429         → BackpressureError
 *   503 + "pause manager not configured" → PauseNotConfiguredError
 *                                          (subclass of UnavailableError)
 *   503 + (other)                        → UnavailableError
 *   500-599 (other) → LoomcycleError (base)
 *   default     → LoomcycleError (base)
 *
 * Priority within a status group is most-specific-first; an unknown
 * 409 falls through to base LoomcycleError so callers see a
 * meaningful message + status. For 404, the catch-all is NotFoundError
 * (base) so the v0.8.18-added memory + interrupt routes don't
 * misclassify into AgentNotFoundError when the 404 body doesn't
 * mention "agent".
 */
export async function raiseFromResponse(resp: Response): Promise<never> {
  const status = resp.status;
  // Read body with a cap; many error bodies are JSON {error, message}
  // shape but raw text is fine for matching keywords.
  let bodyText = "";
  try {
    bodyText = await resp.text();
  } catch {
    // network-level body read failure — fall through with empty body
  }
  const bodyLower = bodyText.toLowerCase();
  // HTTP/2 strips reason phrases — Node's undici fetch returns "" for
  // resp.statusText on HTTP/2 responses. Fall back to a stock phrase
  // for the common status codes so the error message reads cleanly
  // ("401 Unauthorized" not "401 " with a trailing space).
  const statusPhrase = resp.statusText || stockStatusPhrase(status);
  const msg = bodyText.trim() ? bodyText.slice(0, 1024) : `${status} ${statusPhrase}`;
  const opts = { status, bodyText: bodyText.slice(0, 1024) };

  switch (status) {
    case 400:
      throw new InvalidArgumentError(msg, opts);
    case 401:
      throw new AuthError(msg, opts);
    case 404:
      // Priority: most-specific keyword wins.
      // - "snapshot" → SnapshotNotFoundError
      // - "session"  → SessionNotFoundError
      // - "hook"     → HookNotFoundError (must precede "agent" — the
      //                hooks 404 body is `no hook with id "..."`,
      //                doesn't mention "agent")
      // - "agent" or "agent_id" → AgentNotFoundError
      // - otherwise → NotFoundError (base) — e.g. memory rows, interrupts,
      //   or any future 404-returning endpoint that doesn't fit the
      //   existing keyword set.
      if (bodyLower.includes("snapshot")) throw new SnapshotNotFoundError(msg, opts);
      if (bodyLower.includes("session")) throw new SessionNotFoundError(msg, opts);
      if (bodyLower.includes("hook")) throw new HookNotFoundError(msg, opts);
      if (bodyLower.includes("agent")) throw new AgentNotFoundError(msg, opts);
      throw new NotFoundError(msg, opts);
    case 409:
      if (bodyLower.includes("already_pausing") || bodyLower.includes("already paused"))
        throw new AlreadyPausingError(msg, opts);
      if (bodyLower.includes("not_paused") || bodyLower.includes("not paused"))
        throw new NotPausedError(msg, opts);
      if (bodyLower.includes("session")) throw new SessionBusyError(msg, opts);
      if (bodyLower.includes("agent_id")) throw new AgentIDInUseError(msg, opts);
      throw new LoomcycleError(msg, opts);
    case 413:
      throw new SnapshotTooLargeError(msg, opts);
    case 422: {
      // 422 is shared between snapshot version errors (existing)
      // and v0.8.22 substrate tool refusals. Discriminate by body:
      // the substrate path returns `{code: "tool_refused", tool,
      // error}` JSON; the snapshot path returns a free-form text.
      try {
        const parsed = JSON.parse(bodyText) as { code?: string; tool?: string; error?: string };
        if (parsed.code === "tool_refused") {
          throw new SubstrateToolRefusedError(
            parsed.error ?? msg,
            { status, bodyText, tool: parsed.tool },
          );
        }
      } catch (e) {
        // Re-throw our typed error if we matched; fall through to
        // SnapshotVersionError on any JSON-parse failure.
        if (e instanceof SubstrateToolRefusedError) throw e;
      }
      throw new SnapshotVersionError(msg, opts);
    }
    case 429:
      throw new BackpressureError(msg, opts);
    case 503:
      if (bodyLower.includes("pause") && bodyLower.includes("not configured"))
        throw new PauseNotConfiguredError(msg, opts);
      throw new UnavailableError(msg, opts);
    default:
      throw new LoomcycleError(msg, opts);
  }
}

/** stockStatusPhrase returns a stock reason phrase for the common
 *  HTTP statuses raiseFromResponse handles. Used as a fallback when
 *  Response.statusText is empty (HTTP/2 strips reason phrases). */
function stockStatusPhrase(status: number): string {
  switch (status) {
    case 400: return "Bad Request";
    case 401: return "Unauthorized";
    case 403: return "Forbidden";
    case 404: return "Not Found";
    case 409: return "Conflict";
    case 413: return "Payload Too Large";
    case 422: return "Unprocessable Entity";
    case 429: return "Too Many Requests";
    case 500: return "Internal Server Error";
    case 502: return "Bad Gateway";
    case 503: return "Service Unavailable";
    case 504: return "Gateway Timeout";
    default: return "HTTP " + status;
  }
}
