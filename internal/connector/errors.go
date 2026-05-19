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
)
