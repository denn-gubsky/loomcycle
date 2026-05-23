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

  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message);
    this.name = "LoomcycleError";
    this.status = opts?.status;
    this.bodyText = opts?.bodyText;
  }
}

/** Base class for every HTTP 404 the client surfaces. Lets callers
 *  catch any not-found case with a single `instanceof NotFoundError`
 *  check, regardless of which specific resource was missing
 *  (agent / session / snapshot / generic 404 like a missing memory
 *  row or interrupt). */
export class NotFoundError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "NotFoundError";
  }
}

export class AgentNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "AgentNotFoundError";
  }
}

export class SessionNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "SessionNotFoundError";
  }
}

export class SessionBusyError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "SessionBusyError";
  }
}

export class AgentIDInUseError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "AgentIDInUseError";
  }
}

export class BackpressureError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "BackpressureError";
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
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "AuthError";
  }
}

export class UnavailableError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "UnavailableError";
  }
}

export class InvalidArgumentError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "InvalidArgumentError";
  }
}

// ---- v0.8.18 — Pause/Snapshot typed errors ----

/** Subclasses UnavailableError for back-compat: code that broadly
 *  catches UnavailableError keeps working when this more-specific
 *  variant fires. */
export class PauseNotConfiguredError extends UnavailableError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "PauseNotConfiguredError";
  }
}

export class AlreadyPausingError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "AlreadyPausingError";
  }
}

export class NotPausedError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "NotPausedError";
  }
}

export class SnapshotNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "SnapshotNotFoundError";
  }
}

export class SnapshotTooLargeError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "SnapshotTooLargeError";
  }
}

export class SnapshotVersionError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "SnapshotVersionError";
  }
}

/** HookNotFoundError — raised by deleteHook when no hook has the
 *  supplied id (HTTP 404 with "hook" in the body). Extends
 *  NotFoundError so consumers catching the broader category get this
 *  one too. */
export class HookNotFoundError extends NotFoundError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "HookNotFoundError";
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
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
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
    opts?: { status?: number; bodyText?: string; tool?: string },
  ) {
    super(message, opts);
    this.name = "SubstrateToolRefusedError";
    this.tool = opts?.tool ?? "";
  }
}
