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

import type { AuthzReason } from "./types.js";
import {
  AclDeniedError,
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  ChannelCursorRegressionError,
  InsufficientScopeError,
  InvalidArgumentError,
  LoomcycleError,
  NotFoundError,
  NotPausedError,
  PauseNotConfiguredError,
  PerUserQuotaExhaustedError,
  PromptTooLargeError,
  RateLimitedError,
  RequestTooLargeError,
  SessionBusyError,
  SessionNotFoundError,
  SnapshotNotFoundError,
  SnapshotTooLargeError,
  SnapshotVersionError,
  SubstrateToolRefusedError,
  TokenLimitExceededError,
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

/** queryString turns an optional param map into a leading-`?` query
 *  string, dropping empty-valued entries. Returns "" for an absent or
 *  all-empty map so a caller that passes no query builds the same URL as
 *  before. Insertion order is preserved (URLSearchParams is stable). */
function queryString(query?: Record<string, string>): string {
  if (!query) return "";
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v) qs.set(k, v);
  }
  const s = qs.toString();
  return s ? `?${s}` : "";
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
 *  omitted). `opts.query` appends URL query params (empty-valued
 *  entries are dropped; an all-empty/absent map yields no `?`, so
 *  existing callers produce a byte-identical URL). */
export async function postJSON<T>(
  ctx: _FetchContext,
  path: string,
  body?: unknown,
  opts?: { signal?: AbortSignal; query?: Record<string, string> },
): Promise<T> {
  const headers = authHeaders(ctx);
  let bodyStr: string | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    bodyStr = JSON.stringify(body);
  }
  const resp = await ctx.fetchImpl(ctx.baseUrl + path + queryString(opts?.query), {
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

/** putJSON sends a JSON-encoded body via PUT and unwraps the
 *  response. Idempotent — REST-canonical verb for "create or
 *  overwrite by full identifier." */
export async function putJSON<T>(
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
    method: "PUT",
    headers,
    body: bodyStr,
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
  if (resp.status === 204) return null as T;
  return (await resp.json()) as T;
}

/** patchJSON sends a JSON-encoded body via PATCH and unwraps the
 *  response. For partial-update endpoints. */
export async function patchJSON<T>(
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
    method: "PATCH",
    headers,
    body: bodyStr,
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
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

/** deleteJSON is deleteRequest for the DELETE endpoints that DO return a
 *  body (e.g. a revoke that echoes {def_id, retired_at}). 204 → null. */
export async function deleteJSON<T>(
  ctx: _FetchContext,
  path: string,
  opts?: { signal?: AbortSignal },
): Promise<T> {
  const resp = await ctx.fetchImpl(ctx.baseUrl + path, {
    method: "DELETE",
    headers: authHeaders(ctx),
    signal: opts?.signal,
  });
  if (!resp.ok) {
    await raiseFromResponse(resp);
  }
  if (resp.status === 204) return null as T;
  return (await resp.json()) as T;
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
 *   403 + code "insufficient_scope" (or an older server's
 *         WWW-Authenticate scope hint) → InsufficientScopeError
 *   403 + code "acl_denied" → AclDeniedError (action, resource, reasons)
 *   403 (other) → LoomcycleError (base)
 *   404 + "snapshot"   → SnapshotNotFoundError ────────┐
 *   404 + "session"    → SessionNotFoundError          │  All extend
 *   404 + "agent"      → AgentNotFoundError            │  callers can
 *   404 + (other)      → NotFoundError (base)          │  catch any 404
 *                                                      │  with one
 *                                                      │  instanceof.
 *   409 + an access-control code (group_external, *_exists) → LoomcycleError
 *   409 + "already_pausing" / "already paused" → AlreadyPausingError
 *   409 + "not_paused" / "not paused"          → NotPausedError
 *   409 + "session"                            → SessionBusyError
 *   409 + "agent_id"                           → AgentIDInUseError
 *   409 + (other)                              → LoomcycleError (base)
 *   413 + code "snapshot_too_large" → SnapshotTooLargeError ──┐ all are
 *   413 + code "prompt_too_large"   → PromptTooLargeError     │ instanceof
 *   413 (other)                     → RequestTooLargeError ───┘ SnapshotTooLargeError
 *   422         → SnapshotVersionError (snapshot-version-too-new/unknown)
 *   429 + code "per_user_quota_exhausted" → PerUserQuotaExhaustedError
 *   429 + code "token_limit_exceeded"     → TokenLimitExceededError
 *                                           (subclass of BackpressureError)
 *   429 (other)                           → BackpressureError
 *   503 + "pause manager not configured" → PauseNotConfiguredError
 *                                          (subclass of UnavailableError)
 *   503 + (other)                        → UnavailableError
 *   500-599 (other) → LoomcycleError (base)
 *   default     → LoomcycleError (base)
 *
 * Whatever class the status maps to, a JSON body's string `code` is on the
 * error as `code` (see bodyCode).
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
  const opts = { status, bodyText: bodyText.slice(0, 1024), code: bodyCode(bodyText) };

  switch (status) {
    case 400:
      throw new InvalidArgumentError(msg, opts);
    case 401:
      throw new AuthError(msg, opts);
    case 403: {
      // A missing scope says so in its code. A server from before the JSON
      // body sent "insufficient scope" as plain text; the scope hint in
      // WWW-Authenticate, which no other 403 carries, still identifies it.
      const hinted = scopeFromWWWAuthenticate(resp);
      if (opts.code === "insufficient_scope" || (opts.code === undefined && hinted !== undefined)) {
        throw new InsufficientScopeError(msg, {
          ...opts,
          requiredScope: stringField(bodyObject(bodyText)?.required_scope) ?? hinted,
        });
      }
      if (opts.code === "acl_denied") {
        const body = bodyObject(bodyText) ?? {};
        throw new AclDeniedError(msg, {
          ...opts,
          action: stringField(body.action),
          resource: stringField(body.resource),
          reasons: aclReasons(body.reasons),
        });
      }
      throw new LoomcycleError(msg, opts);
    }
    case 404:
      // Priority: most-specific keyword wins.
      // - "snapshot" → SnapshotNotFoundError
      // - "session"  → SessionNotFoundError
      // - "agent" or "agent_id" → AgentNotFoundError
      // - otherwise → NotFoundError (base) — e.g. memory rows, interrupts,
      //   or any future 404-returning endpoint that doesn't fit the
      //   existing keyword set.
      if (bodyLower.includes("snapshot")) throw new SnapshotNotFoundError(msg, opts);
      if (bodyLower.includes("session")) throw new SessionNotFoundError(msg, opts);
      if (bodyLower.includes("agent")) throw new AgentNotFoundError(msg, opts);
      throw new NotFoundError(msg, opts);
    case 409:
      // The access-control conflicts carry a code, and their messages quote
      // caller-chosen names (a group called "session-ops"), which the
      // keyword matches below would misread.
      if (opts.code !== undefined && ACL_CONFLICT_CODES.has(opts.code))
        throw new LoomcycleError(msg, opts);
      if (bodyLower.includes("already_pausing") || bodyLower.includes("already paused"))
        throw new AlreadyPausingError(msg, opts);
      if (bodyLower.includes("not_paused") || bodyLower.includes("not paused"))
        throw new NotPausedError(msg, opts);
      // v0.9.x — Channel CRUD ack with a stale cursor. Distinct so
      // the n8n adapter / consumer can branch on `instanceof`.
      if (bodyLower.includes("channel_cursor_regression"))
        throw new ChannelCursorRegressionError(msg, opts);
      if (bodyLower.includes("session")) throw new SessionBusyError(msg, opts);
      if (bodyLower.includes("agent_id")) throw new AgentIDInUseError(msg, opts);
      throw new LoomcycleError(msg, opts);
    case 413:
      // Every 413 used to be a SnapshotTooLargeError, so the two classes
      // below extend it: an existing `instanceof SnapshotTooLargeError`
      // still matches all three.
      if (opts.code === "snapshot_too_large") throw new SnapshotTooLargeError(msg, opts);
      if (opts.code === "prompt_too_large") throw new PromptTooLargeError(msg, opts);
      throw new RequestTooLargeError(msg, opts);
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
            { status, bodyText, code: parsed.code, tool: parsed.tool },
          );
        }
      } catch (e) {
        // Re-throw our typed error if we matched; fall through to
        // SnapshotVersionError on any JSON-parse failure.
        if (e instanceof SubstrateToolRefusedError) throw e;
      }
      throw new SnapshotVersionError(msg, opts);
    }
    case 429: {
      // A hard token budget is not transient the way backpressure is, so
      // it gets its own class, deliberately outside BackpressureError: a
      // retry loop keyed on that class must not spin on it all month.
      if (opts.code === "token_limit_exceeded") {
        const body = bodyObject(bodyText) ?? {};
        throw new TokenLimitExceededError(msg, {
          ...opts,
          scope: stringField(body.scope),
          scopeId: stringField(body.scope_id),
          used: numberField(body.used),
          limit: numberField(body.limit),
          window: stringField(body.window),
        });
      }
      if (opts.code === "rate_limited") {
        throw new RateLimitedError(msg, { ...opts, retryAfterMs: retryAfterMs(resp) });
      }
      // v0.10.1: distinguish per-user quota exhaustion from
      // operator-wide backpressure. The shapes share the 429 status
      // but the JSON body's `code` field discriminates. Consumers
      // branch retry strategies on the typed error.
      try {
        const parsed = JSON.parse(bodyText) as {
          code?: string;
          user_id?: string;
          cap?: number;
          error?: string;
        };
        if (parsed.code === "per_user_quota_exhausted") {
          throw new PerUserQuotaExhaustedError(parsed.error ?? msg, {
            status,
            bodyText,
            code: parsed.code,
            userId: parsed.user_id,
            cap: parsed.cap,
            retryAfterMs: retryAfterMs(resp),
          });
        }
      } catch (e) {
        // Re-throw the typed error; fall through on JSON-parse fail.
        if (e instanceof PerUserQuotaExhaustedError) throw e;
      }
      throw new BackpressureError(msg, opts);
    }
    case 503:
      if (bodyLower.includes("pause") && bodyLower.includes("not configured"))
        throw new PauseNotConfiguredError(msg, opts);
      throw new UnavailableError(msg, opts);
    default:
      throw new LoomcycleError(msg, opts);
  }
}

/** The 409 codes of the access-control management routes: a group an
 *  identity provider owns, and a row or membership that already exists. */
const ACL_CONFLICT_CODES: ReadonlySet<string> = new Set([
  "group_external",
  "group_exists",
  "group_member_exists",
  "acl_exists",
  "acl_binding_exists",
]);

/** aclReasons reads an `acl_denied` body's `reasons`, keeping only entries
 *  with the fields a reason always has: a proxy's or an older server's
 *  stray value must not surface as a rule. */
function aclReasons(v: unknown): AuthzReason[] {
  if (!Array.isArray(v)) return [];
  const out: AuthzReason[] = [];
  for (const r of v) {
    if (r === null || typeof r !== "object") continue;
    const o = r as Record<string, unknown>;
    const acl = stringField(o.acl);
    const rule = numberField(o.rule);
    const effect = o.effect;
    if (acl === undefined || rule === undefined || (effect !== "allow" && effect !== "deny")) continue;
    const reason: AuthzReason = { acl, rule, effect };
    const version = numberField(o.version);
    if (version !== undefined) reason.version = version;
    out.push(reason);
  }
  return out;
}

/** retryAfterMs reads a `Retry-After` header given in seconds, as the server
 *  always sends it, and returns milliseconds; undefined when the header is
 *  absent or not a number (an HTTP-date form is not parsed). */
function retryAfterMs(resp: Response): number | undefined {
  const raw = resp.headers.get("retry-after");
  if (!raw) return undefined;
  const ms = Number.parseInt(raw, 10) * 1000;
  return Number.isFinite(ms) ? ms : undefined;
}

/** scopeFromWWWAuthenticate reads the scope from the RFC 6750 hint the server
 *  sends with a scope refusal (`Bearer scope="runs:create"`); undefined when
 *  the header is absent or names no scope. */
function scopeFromWWWAuthenticate(resp: Response): string | undefined {
  const m = /\bscope="([^"]*)"/.exec(resp.headers.get("www-authenticate") ?? "");
  return stringField(m?.[1]);
}

/** bodyCode returns the string `code` of a JSON error body, or undefined when
 *  the body is not a JSON object or has none. Statuses are shared between
 *  unrelated conditions (a 400 is any bad request; a 429 is backpressure, a
 *  user quota or a token budget), and the code is what tells them apart. */
function bodyCode(bodyText: string): string | undefined {
  return stringField(bodyObject(bodyText)?.code);
}

/** bodyObject returns a JSON error body as an object, or undefined when the
 *  body is not JSON or is a JSON array or scalar. */
function bodyObject(bodyText: string): Record<string, unknown> | undefined {
  try {
    const parsed: unknown = JSON.parse(bodyText);
    if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    // not JSON: a plain-text error body has no fields
  }
  return undefined;
}

/** stringField / numberField read one body field, dropping a value of the
 *  wrong type: an error's typed fields must not carry whatever a proxy or an
 *  older server put under that name. An empty string counts as absent. */
function stringField(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

function numberField(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
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
