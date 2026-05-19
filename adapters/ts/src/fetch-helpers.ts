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
  InvalidArgumentError,
  LoomcycleError,
  NotPausedError,
  PauseNotConfiguredError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  UnavailableError,
} from "./errors.js";

/** Snapshot of the constructor inputs each method needs. Passed
 *  to the helpers below as a small bag rather than threading
 *  individual fields. */
export interface FetchContext {
  baseUrl: string;
  authToken: string | undefined;
  fetchImpl: typeof fetch;
}

/** authHeaders builds the standard request header set: JSON Accept
 *  + Bearer token when the client was constructed with one. The
 *  caller adds Content-Type when posting a body. */
export function authHeaders(ctx: FetchContext): Record<string, string> {
  const h: Record<string, string> = { Accept: "application/json" };
  if (ctx.authToken) h.Authorization = `Bearer ${ctx.authToken}`;
  return h;
}

/** jsonFetch performs a GET and unwraps the JSON body. Non-2xx
 *  status maps to a typed error via raiseFromResponse. */
export async function jsonFetch<T>(
  ctx: FetchContext,
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
  ctx: FetchContext,
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
  ctx: FetchContext,
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
 *   404 + "snapshot"   → SnapshotNotFoundError
 *   404 + "session"    → SessionNotFoundError
 *   404 + (other)      → AgentNotFoundError (the catch-all)
 *   409 + "already_pausing" / "already paused" → AlreadyPausingError
 *   409 + "not_paused" / "not paused"          → NotPausedError
 *   409 + "session"                            → SessionBusyError
 *   409 + "agent_id"                           → AgentIDInUseError
 *   409 + (other)                              → LoomcycleError (base)
 *   413         → SnapshotTooLargeError
 *   422         → SnapshotVersionError (snapshot-version-too-new/unknown)
 *   429         → BackpressureError
 *   503 + "pause manager not configured" → PauseNotConfiguredError
 *   503 + (other)                        → UnavailableError
 *   500-599 (other) → LoomcycleError (base)
 *   default     → LoomcycleError (base)
 *
 * Priority within a status group is most-specific-first; an unknown
 * 409 falls through to base LoomcycleError so callers see a
 * meaningful message + status.
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
  const msg = bodyText.trim() ? bodyText.slice(0, 1024) : `${status} ${resp.statusText}`;
  const opts = { status, bodyText: bodyText.slice(0, 1024) };

  switch (status) {
    case 400:
      throw new InvalidArgumentError(msg, opts);
    case 401:
      throw new AuthError(msg, opts);
    case 404:
      if (bodyLower.includes("snapshot")) throw new SnapshotNotFoundError(msg, opts);
      if (bodyLower.includes("session")) throw new SessionNotFoundError(msg, opts);
      throw new AgentNotFoundError(msg, opts);
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
    case 422:
      throw new SnapshotVersionError(msg, opts);
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
