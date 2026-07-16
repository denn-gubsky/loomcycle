package connector

import "errors"

// Typed errors returned by the Connector's Pause/Snapshot methods.
// Wire transports translate these into protocol-specific error codes:
//
//	HTTP    — 503 / 409 / 404 / 413 / 422 status + JSON error body
//	gRPC    — codes.Unavailable / FailedPrecondition / NotFound /
//	          ResourceExhausted / FailedPrecondition
//	MCP     — tool_result with IsError=true + a descriptive Text
//	Python  — typed subclasses of LoomcycleError in adapters/python/
//
// Implementations (currently only internal/api/http) return these
// sentinels (or wrap them) so transport layers can use errors.Is /
// errors.As to map cleanly. Returning a plain fmt.Errorf("...") is
// always allowed for paths that don't have a typed-error equivalent;
// transports fall back to a generic 500 / Internal / "unknown error".
var (
	// ErrPauseNotConfigured is returned when a Pause/Resume/State or
	// Snapshot method is called on a Server that wasn't wired with a
	// pause Manager (e.g. main.go didn't call SetPauseManager because
	// the store backend is missing). Transports map this to
	// "feature unavailable on this deployment".
	ErrPauseNotConfigured = errors.New("connector: pause manager not configured on this server")

	// ErrAlreadyPausing is returned by PauseRuntime when the runtime
	// is already in StatePausing or StatePaused. Idempotent for the
	// caller (a scripted "pause if not paused" loop using
	// `set -e + ||true` keeps working), but a typed error so
	// transports can distinguish "no-op" from "happy path".
	ErrAlreadyPausing = errors.New("connector: runtime is already pausing or paused")

	// ErrNotPaused is returned by ResumeRuntime when the runtime is
	// in StateRunning. Symmetric with ErrAlreadyPausing.
	ErrNotPaused = errors.New("connector: runtime is not paused")

	// ErrSnapshotNotFound is returned by GetSnapshot / ExportSnapshot /
	// RestoreSnapshot / DeleteSnapshot when no snapshot exists with
	// the supplied id. Transports map to 404 / NotFound.
	ErrSnapshotNotFound = errors.New("connector: snapshot not found")

	// ErrSnapshotTooLarge is returned by CreateSnapshot when the
	// serialised envelope exceeds the configured cap
	// (LOOMCYCLE_SNAPSHOT_MAX_BYTES; default 512 MiB).
	ErrSnapshotTooLarge = errors.New("connector: snapshot exceeds size cap")

	// ErrSnapshotVersionTooNew is returned by RestoreSnapshot when a
	// section's declared version is newer than the reader supports.
	// Operators upgrade loomcycle before restoring.
	ErrSnapshotVersionTooNew = errors.New("connector: snapshot section version newer than reader supports")

	// ErrSnapshotVersionUnknown is returned by RestoreSnapshot when a
	// section's declared version isn't in the migration registry at
	// all (corrupted snapshot or pre-history version).
	ErrSnapshotVersionUnknown = errors.New("connector: snapshot section version unknown")

	// ErrHookInvalidRegistration is returned by RegisterHook when the
	// supplied request fails the hooks.Registry validation (missing
	// owner/name/callback_url, unsupported phase, non-http(s) scheme).
	// Transports map to 400 / InvalidArgument. Wrap with %w + the
	// underlying message so the caller sees what specifically failed.
	ErrHookInvalidRegistration = errors.New("connector: invalid hook registration")

	// ErrHookNotFound is returned by DeleteHook when no hook is
	// registered with the supplied id. Transports map to 404 / NotFound.
	ErrHookNotFound = errors.New("connector: hook not found")

	// ErrHookNotConfigured is returned by the hook methods when the
	// Server was constructed without a hookRegistry (e.g. a test harness
	// that builds *Server directly via struct literal). The HTTP New()
	// constructor always wires one, so production deployments never hit
	// this — it's a defensive guard for the gRPC/MCP code paths that
	// dispatch through Connector and would otherwise nil-panic.
	ErrHookNotConfigured = errors.New("connector: hook registry not configured on this server")

	// ErrRunStateStreamUnavailable is returned by StreamUserRunStates
	// when the underlying server has no runstate.Bus wired (operator
	// embedding skipped SetRunStateBus). Transports map to
	// Unavailable / HTTP 503.
	ErrRunStateStreamUnavailable = errors.New("connector: run-state stream not configured on this server")

	// ErrResolverUnavailable is returned by ResolveProbe when the
	// Server has no resolver wired (degraded startup, or the v0.6.x
	// explicit-pin path that doesn't populate a matrix). Transports
	// map to Unavailable / HTTP 503 (code "resolver_unavailable").
	ErrResolverUnavailable = errors.New("connector: resolver not configured on this server")

	// ErrResolveProbeUnavailable is returned by ResolveProbe when a
	// resolver exists but no probe loop is wired (e.g. a degraded
	// startup). ForceProbe would be a silent no-op, so the
	// method fails rather than return a matrix it never refreshed.
	// Transports map to Unavailable / HTTP 503 (code "probe_unavailable").
	ErrResolveProbeUnavailable = errors.New("connector: no probe loop wired; cannot trigger an immediate re-probe")

	// ErrStopStreaming is the visitor-side sentinel a RunStateVisitor
	// returns to end the stream cleanly. StreamUserRunStates returns
	// nil (not this sentinel) when the visitor returns it; the
	// sentinel is the visitor's way of saying "I have what I need."
	ErrStopStreaming = errors.New("connector: stop streaming")

	// ErrChannelNotDeclared is returned by the Channel CRUD methods
	// when the requested channel name isn't in the operator's yaml
	// `channels:` block. Transports map to NotFound / HTTP 404.
	ErrChannelNotDeclared = errors.New("connector: channel not declared in operator yaml")

	// ErrChannelScopeInvalid is returned when the scope field on a
	// Channel CRUD request is not one of "global" / "user". Transports
	// map to InvalidArgument / HTTP 400.
	ErrChannelScopeInvalid = errors.New("connector: channel scope must be 'global' or 'user'")

	// ErrChannelCursorRegression is returned by AckChannel when the
	// caller-supplied cursor is older than the currently-committed
	// cursor. Mirrors store.ErrChannelCursorRegression at the connector
	// boundary so transports can surface it without importing
	// internal/store. Transports map to FailedPrecondition / HTTP 409.
	ErrChannelCursorRegression = errors.New("connector: channel ack cursor is older than committed")

	// ErrSystemPublisherUnwired is returned by PublishChannel when the
	// underlying server has no SystemPublisher wired (operator
	// embedding skipped SetSystemPublisher). Transports map to
	// Unavailable / HTTP 503. Mirror of the existing
	// ErrRunStateStreamUnavailable pattern.
	ErrSystemPublisherUnwired = errors.New("connector: system publisher not configured on this server")

	// ErrChannelYamlImmutable is returned by the channel CRUD admin
	// methods when the requested channel name matches a yaml-declared
	// channel. yaml is the floor: edit the loomcycle.yaml and restart
	// rather than mutating from the runtime API. Transports map to
	// Conflict / HTTP 409.
	ErrChannelYamlImmutable = errors.New("connector: channel is declared in operator yaml; edit yaml + restart to change")

	// ErrChannelAlreadyExists is returned by CreateChannel when a
	// runtime-substrate channel with the same name already exists.
	// Transports map to Conflict / HTTP 409.
	ErrChannelAlreadyExists = errors.New("connector: channel already exists in runtime substrate")

	// ErrChannelNotFound is returned by UpdateChannel / DeleteChannel
	// when the requested name is neither yaml-declared nor in the
	// runtime substrate. Transports map to NotFound / HTTP 404.
	ErrChannelNotFound = errors.New("connector: channel not found")

	// --- RFC AI interactive sessions ---

	// ErrSteeringUnavailable is returned by SteerRun when the server has no
	// steer registry wired (a test harness, or steering disabled).
	// StreamRunEvents returns it when no persistence backend is wired.
	// Transports map to Unavailable / HTTP 503.
	ErrSteeringUnavailable = errors.New("connector: steering not configured on this server")

	// ErrRunNotInFlight is returned by SteerRun when no live (registered) run
	// holds run_id, AND by StreamRunEvents when the run_id is unknown or
	// cross-tenant. Opaque on purpose — run_ids are not secret, so the gate
	// must not become an existence oracle. Transports map to NotFound / 404.
	ErrRunNotInFlight = errors.New("connector: no in-flight run for run_id")

	// ErrSteerQueueFull is returned by SteerRun when the run's steer buffer is
	// full (back-pressure against a stuck run). Transports map to
	// ResourceExhausted / HTTP 429.
	ErrSteerQueueFull = errors.New("connector: run input queue full")

	// --- RFC BH turn-cancel (stop the current turn, keep the session) ---

	// ErrTurnCancelUnavailable is returned by CancelTurn when the server has no
	// turn-cancel / steer registry wired. Transports map to Unavailable / 503.
	ErrTurnCancelUnavailable = errors.New("connector: turn-cancel not configured on this server")

	// ErrTurnNotMidTurn is returned by CancelTurn when the run holds no armed
	// per-turn token (already parked / terminal, or the turn just ended). The
	// HTTP surface renders this as 409 {code:"not_mid_turn"}; gRPC maps it to
	// FailedPrecondition.
	ErrTurnNotMidTurn = errors.New("connector: run is not mid-turn")

	// ErrTurnNotInteractive is returned by CancelTurn for a non-interactive run
	// (stopping its only turn would terminate it — use whole-run cancel). HTTP
	// renders 409 {code:"not_interactive"}; gRPC maps to FailedPrecondition.
	ErrTurnNotInteractive = errors.New("connector: turn-cancel applies only to interactive runs")

	// --- RFC BH interruption resolve / decline ---
	//
	// ResolveInterrupt returns these wrapped via WithMessage so each transport
	// classifies with errors.Is while rendering the exact caller-facing text
	// verbatim (the HTTP surface's byte-identical bodies). These back the new
	// gRPC ResolveInterrupt RPC + are distinct from the MCP InterruptionResolve
	// method, which keeps its own shipped wording.

	// ErrInterruptStoreUnavailable — no persistence backend wired. HTTP 503 /
	// gRPC Unavailable.
	ErrInterruptStoreUnavailable = errors.New("connector: interrupts require persistence")

	// ErrInterruptUnsupportedKind — kind is not the supported "question". HTTP
	// 422 / gRPC InvalidArgument.
	ErrInterruptUnsupportedKind = errors.New("connector: unsupported interrupt kind")

	// ErrInterruptUnsupportedDisposition — disposition is not "" / "answer" /
	// "declined". HTTP 422 / gRPC InvalidArgument.
	ErrInterruptUnsupportedDisposition = errors.New("connector: unsupported disposition")

	// ErrInterruptDeclineWithAnswer — a decline must carry no answer. HTTP 422 /
	// gRPC InvalidArgument.
	ErrInterruptDeclineWithAnswer = errors.New("connector: declined disposition must not carry an answer")

	// ErrInterruptInvalidAnswer — the answer is missing (free-text) or not one
	// of the declared options. HTTP 422 / gRPC InvalidArgument.
	ErrInterruptInvalidAnswer = errors.New("connector: invalid answer")

	// ErrInterruptNotFound — unknown interrupt, wrong run, or cross-tenant
	// (opaque, no existence oracle). HTTP 404 / gRPC NotFound.
	ErrInterruptNotFound = errors.New("connector: interrupt not found")

	// ErrInterruptAlreadyTerminal — the interrupt is already resolved / declined
	// / timed out / cancelled. HTTP 409 / gRPC FailedPrecondition.
	ErrInterruptAlreadyTerminal = errors.New("connector: interrupt already terminal")

	// ErrInterruptExpired — the interrupt's TTL elapsed before resolution. HTTP
	// 410 / gRPC FailedPrecondition.
	ErrInterruptExpired = errors.New("connector: interrupt expired")
)

// codedError pairs a classification sentinel (matched via errors.Is) with the
// exact caller-facing message a transport renders verbatim. It lets the shared
// connector core preserve each transport's byte-identical wording (the HTTP
// error bodies) while every transport still maps the error to its own status
// code by matching the sentinel. (RFC BH P3b.)
type codedError struct {
	sentinel error
	msg      string
}

func (e *codedError) Error() string { return e.msg }
func (e *codedError) Unwrap() error { return e.sentinel }

// WithMessage wraps sentinel with an exact caller-facing message.
// errors.Is(WithMessage(s, m), s) is true and the wrapper's Error() is m —
// so a transport classifies on the sentinel yet surfaces the verbatim text.
func WithMessage(sentinel error, msg string) error {
	return &codedError{sentinel: sentinel, msg: msg}
}
