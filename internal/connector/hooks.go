package connector

import "github.com/denn-gubsky/loomcycle/internal/hooks"

// RegisterHookRequest is the input to Connector.RegisterHook. It mirrors
// internal/hooks.Hook minus the loomcycle-assigned fields (ID,
// RegisteredAt) — the same shape POST /v1/hooks consumes today.
//
// Lives in the connector package (not internal/api/http) so gRPC + MCP
// transports can import it without touching the HTTP package. Imports
// internal/hooks for the Phase / FailMode named types; the hooks
// package has no dependency on connector, so the import direction is
// safe.
type RegisterHookRequest struct {
	Owner       string         `json:"owner"`
	Name        string         `json:"name"`
	Phase       hooks.Phase    `json:"phase"`
	Agents      []string       `json:"agents,omitempty"`
	Tools       []string       `json:"tools,omitempty"`
	CallbackURL string         `json:"callback_url"`
	FailMode    hooks.FailMode `json:"fail_mode,omitempty"`
	TimeoutMs   int            `json:"timeout_ms,omitempty"`
}

// RegisterHookResponse is what Connector.RegisterHook returns: the
// loomcycle-assigned ID that callers use on DeleteHook.
type RegisterHookResponse struct {
	ID string `json:"id"`
}

// ListHooksResponse wraps the slice of currently-registered hooks.
// Mirrors GET /v1/hooks. Carries the full hooks.Hook (including ID +
// RegisteredAt) so callers can correlate with their own bookkeeping.
type ListHooksResponse struct {
	Hooks []*hooks.Hook `json:"hooks"`
}
