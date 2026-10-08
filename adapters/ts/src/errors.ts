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
}

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

/** Every HTTP 429 that is not a per-user quota. A plain BackpressureError is
 *  the transient case (the server is busy; back off and retry). It has one
 *  subclass that is NOT transient, TokenLimitExceededError — check for that
 *  first when deciding whether to retry. */
export class BackpressureError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "BackpressureError";
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
 * It extends BackpressureError only so that code written before this class
 * existed, which caught this refusal with `instanceof BackpressureError`,
 * keeps catching it. Test for this class BEFORE BackpressureError:
 *
 *   if (e instanceof TokenLimitExceededError) {
 *     // not retryable now: report e.scope / e.used / e.limit
 *   } else if (e instanceof BackpressureError) {
 *     // transient: back off and retry
 *   }
 *
 * The fields come from the response body and are null when the server did
 * not send them.
 */
export class TokenLimitExceededError extends BackpressureError {
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
}

export class AuthError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string; code?: string }) {
    super(message, opts);
    this.name = "AuthError";
  }
}

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
