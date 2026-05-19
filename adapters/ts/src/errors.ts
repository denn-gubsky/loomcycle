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

export class AgentNotFoundError extends LoomcycleError {
  constructor(message: string, opts?: { status?: number; bodyText?: string }) {
    super(message, opts);
    this.name = "AgentNotFoundError";
  }
}

export class SessionNotFoundError extends LoomcycleError {
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

export class SnapshotNotFoundError extends LoomcycleError {
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
