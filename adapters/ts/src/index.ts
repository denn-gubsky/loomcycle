/**
 * @loomcycle/client — TypeScript client for the loomcycle sidecar.
 *
 * Public surface (v0.8.18 — Python-adapter parity in progress):
 *
 *   class LoomcycleClient
 *     constructor(opts: ClientOptions)
 *     runStreaming(opts: RunOptions): AsyncIterable<AgentEvent>
 *     // ...21 more methods land in PR 5b
 *
 *   Errors (typed subclasses of LoomcycleError):
 *     LoomcycleError, AgentNotFoundError, SessionNotFoundError,
 *     SessionBusyError, AgentIDInUseError, BackpressureError,
 *     AuthError, UnavailableError, InvalidArgumentError,
 *     PauseNotConfiguredError, AlreadyPausingError, NotPausedError,
 *     SnapshotNotFoundError, SnapshotTooLargeError,
 *     SnapshotVersionError
 *
 *   Types (wire shapes):
 *     AgentEvent, EventType, ToolUse, Usage, PromptContent,
 *     PromptSegment, RunOptions, ClientOptions
 *
 * Transport: HTTP+SSE. Auth: Bearer token via the Authorization
 * header. Designed for Node ≥18 (engines pinned); Bun/Deno likely
 * work but untested. Browser support is not a target (use the
 * Web UI for browser-side operator control).
 *
 * See `adapters/ts/README.md` for usage examples and the full
 * API table.
 */

export { LoomcycleClient } from "./client.js";

export type {
  AgentEvent,
  ClientOptions,
  EventType,
  PromptContent,
  PromptSegment,
  RunOptions,
  ToolUse,
  Usage,
} from "./types.js";

export {
  AgentIDInUseError,
  AgentNotFoundError,
  AlreadyPausingError,
  AuthError,
  BackpressureError,
  NotFoundError,
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
