/**
 * Typed exceptions raised by LoomcycleClient. Mirrors the Python
 * adapter's `errors.py` taxonomy 1:1 — same names, same semantics,
 * just adapted to HTTP status codes (no gRPC StatusCode equivalents).
 *
 * Every error stores the raw HTTP status (`status`) and the raw
 * response body (`bodyText`, truncated to 1 KiB) for log correlation
 * when the typed class doesn't carry enough.
 *
 * Dispatch from raw HTTP response to typed error lives in
 * `fetch-helpers.ts:raiseFromResponse` — that's the one place to
 * look when adding a new error type.
 *
 * PR 5a foundation: classes defined; dispatch wiring lands here +
 * in fetch-helpers.ts. The current `runStreaming` (the only public
 * method in v0.1.0-alpha) throws a plain Error today; PR 5a keeps
 * that behavior. PR 5b switches `runStreaming` + every new method
 * to raise typed errors via `raiseFromResponse`.
 */

import type { AuthzReason } from "./types.js";

export class LoomcycleError extends Error {
  readonly status?: number;
  readonly bodyText?: string;
  /** The machine-readable `code` of a JSON error body (`model_not_allowed`,
   *  `token_limit_exceeded`, `per_user_quota_exhausted`, …), on whichever
   *  class the status maps to. Undefined when the body is not JSON or carries
   *  no string `code`. Read from the whole body, so it survives where
   *  `bodyText` is cut at 1 KiB. Branch on this, not on the message. */
  readonly code?: string;

  constructor(
    message: string,
    opts?: { status?: number; bodyText?: string; code?: string },
  ) {
    super(message);
    this.name = "LoomcycleError";
    this.status = opts?.status;
    this.bodyText = opts?.bodyText;
    this.code = opts?.code;
  }

  /** Whether the same call, sent again unchanged, can succeed once time has
   *  passed: true for a refusal that clears on its own (backpressure, a rate
   *  limit, a per-user run quota, a paused runtime, a decision-model call
   *  that failed or timed out), false for everything else, including a hard
   *  token budget, a cap the caller must make room under, a bad request and
   *  a missing resource. A retry loop can test this instead of a list of
   *  classes.
   *
   *  It is a getter rather than a constructor field so that a subclass states
   *  its answer by overriding it, without a constructor option any caller
   *  could set. False is the default because a wrong `true` turns a refusal
   *  that never clears into a loop that never ends, while a wrong `false`
   *  only surfaces an error the caller could have waited out. The client
   *  itself never retries; bound your own retries even when this is true. */
  get retryable(): boolean {
    return this.code !== undefined && TRANSIENT_CODES.has(this.code);
  }
}

/** Codes that clear on their own although the class their status maps to
 *  does not: a 503 is mostly a feature the deployment lacks, and a 502 / 504
 *  is any upstream failure, so the class alone says false. The server
 *  classifies each of these as transient:
 *  - `runtime_paused` (503): new runs are refused until the operator resumes.
 *  - `call_failed` (502): a decision-model call did not complete.
 *  - `timeout` (504): a decision-model call did not answer in time. */
const TRANSIENT_CODES: ReadonlySet<string> = new Set([
  "runtime_paused",
  "call_failed",
  "timeout",
]);

/** 429 codes that do NOT clear by waiting: the caller has to act first, so
 *  a retry loop on them never ends. `configured_run_cap`: the user already
 *  holds as many configured (not yet started) runs as the operator allows,
 *  and only starting or discarding one makes room. (A hard token budget,
 *  `token_limit_exceeded`, has a class of its own outside BackpressureError.) */
const CALLER_ACTION_429_CODES: ReadonlySet<string> = new Set(["configured_run_cap"]);

/** Base class for every HTTP 404 the client surfaces. Lets callers
 *  catch any not-found case with a single `instanceof NotFoundError`
 *  check, regardless of which specific resource was missing
 *  (agent / session / snapshot / generic 404 like a missing memory
 *  row or interrupt). */
export class NotFoundError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "NotFoundError";
  }
}

export class AgentNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "AgentNotFoundError";
  }
}

export class SessionNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "SessionNotFoundError";
  }
}

export class SessionBusyError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "SessionBusyError";
  }
}

export class AgentIDInUseError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "AgentIDInUseError";
  }
}

/** An HTTP 429 that clears on its own: the server is busy (`backpressure`),
 *  a provider's queue is full (`provider_concurrency_exhausted`), or the
 *  caller is starting runs too fast (`rate_limited`, raised as the
 *  RateLimitedError subclass). Back off and retry; `retryable` is true.
 *
 *  One code under this class is the exception: `configured_run_cap` (the user
 *  holds as many configured runs as allowed; start or discard one first)
 *  needs the caller to act, so its `retryable` is false. It stays a
 *  BackpressureError, the class every other 429 lands in.
 *
 *  Two 429s are not this class: a per-user run quota
 *  (PerUserQuotaExhaustedError, also retryable) and a hard token budget
 *  (TokenLimitExceededError, not retryable). */
export class BackpressureError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "BackpressureError";
  }

  override get retryable(): boolean {
    return this.code === undefined || !CALLER_ACTION_429_CODES.has(this.code);
  }
}

/**
 * RateLimitedError signals that the caller is starting runs faster than its
 * per-caller run-start rate allows: HTTP 429 with body code `rate_limited`.
 * The server says when the next start will be admitted in `Retry-After`,
 * surfaced here as `retryAfterMs`; wait that long rather than backing off on
 * a guess. Retryable, like every BackpressureError.
 */
export class RateLimitedError extends BackpressureError {
  /** Server-suggested retry window in milliseconds, from the Retry-After
   *  header. Null when the header is absent or not a whole number of
   *  seconds. */
  readonly retryAfterMs: number | null;
  constructor(
    message: string,
    opts?: { status?: number; bodyText?: string; code?: string; retryAfterMs?: number },
  ) {
    super(message, opts);
    this.name = "RateLimitedError";
    this.retryAfterMs = opts?.retryAfterMs ?? null;
  }
}

/**
 * TokenLimitExceededError signals that the caller is at a hard token budget:
 * HTTP 429 with body code `token_limit_exceeded`, from run creation and from
 * `decide()`.
 *
 * Do not retry it the way you retry backpressure. The budget is a calendar
 * month's tokens for an operator, tenant or user; the same call is refused
 * until an operator raises the budget or the window rolls over.
 *
 * It is not a BackpressureError, and `retryable` is false. It used to
 * extend BackpressureError so that an older `instanceof BackpressureError`
 * check kept catching it, but such a check is almost always a retry loop, and
 * that loop then retried a refusal that cannot clear for the rest of the
 * month. The relation was dropped on purpose: a retry loop keyed on
 * BackpressureError or on `retryable` now stops here, and code that wants to
 * report the budget tests for this class.
 *
 * The fields come from the response body and are null when the server did
 * not send them.
 */
export class TokenLimitExceededError extends LoomcycleError {
  /** Whose budget was hit: "operator", "tenant" or "user". */
  readonly scope: string | null;
  /** The id of that scope: the tenant id or the user subject. Null for the
   *  operator-wide budget, and where the server withholds it. */
  readonly scopeId: string | null;
  /** Tokens the scope has used in the current window. */
  readonly used: number | null;
  /** The hard ceiling that was reached, in tokens. */
  readonly limit: number | null;
  /** The budget window, e.g. "month" (a calendar month, UTC). */
  readonly window: string | null;
  constructor(
    message: string,
    opts?: {
      status?: number;
      bodyText?: string;
      code?: string;
      scope?: string;
      scopeId?: string;
      used?: number;
      limit?: number;
      window?: string;
    },
  ) {
    super(message, opts);
    this.name = "TokenLimitExceededError";
    this.scope = opts?.scope ?? null;
    this.scopeId = opts?.scopeId ?? null;
    this.used = opts?.used ?? null;
    this.limit = opts?.limit ?? null;
    this.window = opts?.window ?? null;
  }
}

/**
 * PerUserQuotaExhaustedError signals that the caller has hit their
 * per-user cap on in-flight (active+queued) runs. Distinct from
 * BackpressureError because the appropriate retry strategy differs:
 * backpressure is operator-wide load (exponential backoff with jitter),
 * per-user quota is "you specifically need to wait" (fixed window —
 * server hint: `Retry-After: 5` seconds).
 *
 * v0.10.1+. Maps from HTTP 429 + JSON body
 * `{"code":"per_user_quota_exhausted","user_id":"...","cap":N}`.
 *
 * The `userId` and `cap` fields are populated from the JSON body when
 * the response is parseable; null when the server didn't include them
 * (very old loomcycle binaries or non-JSON 429 responses).
 *
 * Typical handling:
 *
 *   try { await client.runStreaming(...); }
 *   catch (e) {
 *     if (e instanceof PerUserQuotaExhaustedError) {
 *       // Wait the server-suggested window, then retry.
 *       await sleep(e.retryAfterMs ?? 5000);
 *       return client.runStreaming(...);
 *     }
 *     if (e instanceof BackpressureError) {
 *       // Operator-wide load — jittered backoff.
 *       await sleep(jittered(2000, 30000));
 *       return client.runStreaming(...);
 *     }
 *     throw e;
 *   }
 */
export class PerUserQuotaExhaustedError extends LoomcycleError {
  /** Server-side user identifier the cap applies to. Null when the
   *  server didn't include it in the JSON body. */
  readonly userId: string | null;
  /** Per-user cap value as configured on the server (active+queued).
   *  Null when the server didn't include it. */
  readonly cap: number | null;
  /** Server-suggested retry window in milliseconds, from the
   *  Retry-After header. Null when absent. */
  readonly retryAfterMs: number | null;
  constructor(
    message: string,
    opts?: {
      status?: number;
      bodyText?: string;
      code?: string;
      userId?: string;
      cap?: number;
      retryAfterMs?: number;
    },
  ) {
    super(message, opts);
    this.name = "PerUserQuotaExhaustedError";
    this.userId = opts?.userId ?? null;
    this.cap = opts?.cap ?? null;
    this.retryAfterMs = opts?.retryAfterMs ?? null;
  }

  /** The quota counts the caller's own in-flight runs, so it frees up as
   *  they finish. */
  override get retryable(): boolean {
    return true;
  }
}

export class AuthError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "AuthError";
  }
}

/** An HTTP 403 because the bearer token lacks a scope the call needs: body code
 *  `insufficient_scope`. The token is valid; it was minted without that
 *  grant. Ask an operator for a token that carries `requiredScope`; sending
 *  the same call again never succeeds, so `retryable` is false.
 *
 *  Any other 403 (`restricted_token`, `operator_key_restricted`, …) stays a
 *  plain LoomcycleError: those refuse the caller for something a new scope
 *  does not fix. */
export class InsufficientScopeError extends LoomcycleError {
  /** The scope the token lacks, e.g. `runs:create` or `substrate:tenant`.
   *  Read from the body's `required_scope`, or, from a server that predates
   *  that field, from the `WWW-Authenticate: Bearer scope="…"` header. Null
   *  when neither names one (a browser does not see that header on a
   *  cross-origin response unless the server exposes it). */
  readonly requiredScope: string | null;
  constructor(
    message: string,
    opts?: { status?: number; bodyText?: string; code?: string; requiredScope?: string },
  ) {
    super(message, opts);
    this.name = "InsufficientScopeError";
    this.requiredScope = opts?.requiredScope ?? null;
  }
}

/** An HTTP 403 because an access-control list refused the call: body code
 *  `acl_denied`. The token's scopes would allow it, but a rule bound to the
 *  caller (to its subject, a group it is in, its token or its tenant) denies
 *  it, or, where the deployment enforces lists, no rule allows it.
 *  `reasons` names the rules that decided — empty when none did — so an
 *  operator can find the list to change with `authzExplain`. Not retryable:
 *  the same call is refused until a list or binding changes.
 *
 *  Other access-control refusals (`acl_self_elevation`, `authz_subject`,
 *  `operator_plane`, …) stay a plain LoomcycleError with that `code`. */
export class AclDeniedError extends LoomcycleError {
  /** The verb asked, e.g. `read`, `author`, `manage`. Null when the body did
   *  not carry one. */
  readonly action: string | null;
  /** The resource asked about, `type:name` (e.g. `def:acl/ops`). Null when
   *  the body did not carry one. */
  readonly resource: string | null;
  /** The rules that decided, denies first; empty when no rule matched. */
  readonly reasons: AuthzReason[];
  constructor(
    message: string,
    opts?: {
      status?: number;
      bodyText?: string;
      code?: string;
      action?: string;
      resource?: string;
      reasons?: AuthzReason[];
    },
  ) {
    super(message, opts);
    this.name = "AclDeniedError";
    this.action = opts?.action ?? null;
    this.resource = opts?.resource ?? null;
    this.reasons = opts?.reasons ?? [];
  }
}

/** Every HTTP 503. The status is shared between a condition that clears on
 *  its own and features this deployment does not have
 *  (`decision_not_configured`, `store_unavailable`, `vector_unsupported`, …),
 *  which never will, and many 503 bodies carry no code at all. So
 *  `retryable` is true only for `runtime_paused` (new runs are refused while
 *  the operator has the runtime paused) and false otherwise. A code-less 503
 *  from run creation can also be a model tier with no provider available
 *  right now; retry that one on your own judgement, with a bound. (The
 *  inherited `retryable` reads `runtime_paused` from TRANSIENT_CODES.) */
export class UnavailableError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "UnavailableError";
  }
}

export class InvalidArgumentError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "InvalidArgumentError";
  }
}

// ---- v0.8.18 — Pause/Snapshot typed errors ----

/** Subclasses UnavailableError for back-compat: code that broadly
 *  catches UnavailableError keeps working when this more-specific
 *  variant fires. */
export class PauseNotConfiguredError extends UnavailableError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "PauseNotConfiguredError";
  }

  /** The server has no pause manager; asking again does not give it one. */
  override get retryable(): boolean {
    return false;
  }
}

export class AlreadyPausingError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "AlreadyPausingError";
  }
}

export class NotPausedError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "NotPausedError";
  }
}

export class SnapshotNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "SnapshotNotFoundError";
  }
}

/** A snapshot envelope over the size cap (HTTP 413, body code
 *  `snapshot_too_large`).
 *
 *  For compatibility it is ALSO the base class of every other 413: until
 *  RequestTooLargeError and PromptTooLargeError existed, the client raised
 *  this class for any 413, so `instanceof SnapshotTooLargeError` is true for
 *  all of them and stays true. To single out a snapshot that is too large,
 *  test `e.code === "snapshot_too_large"`. */
export class SnapshotTooLargeError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "SnapshotTooLargeError";
  }
}

/** An HTTP 413 that is not about a snapshot: a request body over the server's
 *  size limit (`request_too_large`, or a plain-text 413 with no code), a
 *  memory value or quota (`memory_value_too_large`, `memory_quota_exceeded`),
 *  a channel payload (`payload_too_large`). `e.code` says which.
 *
 *  It extends SnapshotTooLargeError only because that is the class these
 *  responses were raised as before this one existed; the relation keeps an
 *  existing `instanceof SnapshotTooLargeError` matching and says nothing about
 *  snapshots. */
export class RequestTooLargeError extends SnapshotTooLargeError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "RequestTooLargeError";
  }
}

/** A `decide()` request that does not fit the decision model's context (HTTP
 *  413, body code `prompt_too_large`). The server never shortens it for you:
 *  shorten `state` or ask fewer questions, then send it again. */
export class PromptTooLargeError extends RequestTooLargeError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "PromptTooLargeError";
  }
}

export class SnapshotVersionError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "SnapshotVersionError";
  }
}


/** ChannelCursorRegressionError — raised by `client.ackChannel()`
 *  when the caller-supplied cursor is older than the currently-
 *  committed cursor for the (channel, scope, scope_id) tuple. HTTP
 *  409 with `{code: "channel_cursor_regression", ...}` body.
 *
 *  Mirrors `store.ErrChannelCursorRegression` on the loomcycle
 *  side. Distinct from `SessionBusyError` etc. (which also map to
 *  409) so the n8n adapter can distinguish "this cursor is stale,
 *  re-fetch and retry from the new committed position" from other
 *  409 conditions. */
export class ChannelCursorRegressionError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "ChannelCursorRegressionError";
  }
}

/** SubstrateToolRefusedError — raised by `client.agentDef()` /
 *  `client.skillDef()` when the in-process tool refused the call
 *  (scope deny, empty body, allowed-tools widening, etc.). HTTP
 *  status 422 with `{code: "tool_refused", error, tool}` body.
 *
 *  Distinct from transport failures: the request reached the
 *  server, the substrate tool ran, and the tool itself returned
 *  IsError=true. Operators catching this error should surface the
 *  reason in `message` to the calling agent / user rather than
 *  retrying. */
export class SubstrateToolRefusedError extends LoomcycleError {
  /** Which substrate tool refused — "AgentDef" or "SkillDef". */
  readonly tool: string;
  constructor(
    message: string,
    opts?: { status?: number; bodyText?: string; code?: string; tool?: string },
  ) {
    super(message, opts);
    this.name = "SubstrateToolRefusedError";
    this.tool = opts?.tool ?? "";
  }
}

/** A device login ({@link LoomcycleClient.deviceLogin}) that ended without a
 *  token. `code` is the server's RFC 8628 error: `access_denied` (the person
 *  denied it, or the approval no longer holds — the approving session ended
 *  or its credential was retired), `expired_token` (nobody approved it in
 *  time), or another refusal. Start a new login; the same one never
 *  succeeds. */
export class DeviceLoginError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "DeviceLoginError";
  }
}
