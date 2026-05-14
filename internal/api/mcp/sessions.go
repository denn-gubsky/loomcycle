package mcp

import "sync/atomic"

// Session carries per-connection MCP state. For stdio there's exactly
// one Session per loomcycle-mcp process; HTTP transport (v0.8.15.x)
// will create one Session per HTTP connection.
//
// Fields are read/written by both the main read-dispatch loop and
// the per-request goroutines; access is via atomics so concurrent
// reads after initialize don't need a mutex on the hot path.
type Session struct {
	// initialized flips true after the client sent
	// notifications/initialized following its initialize call.
	// Currently informational — we don't refuse tool calls before
	// it (some clients fire tools/list immediately after the
	// initialize response without sending the initialized
	// notification first).
	initialized atomic.Bool

	// runEventsEnabled is true when the client opted into
	// notifications/loomcycle/run_event via
	// initialize.capabilities.loomcycle.runEvents=true. Read by
	// spawn_run to decide which code path to take.
	runEventsEnabled atomic.Bool

	// clientName / clientVersion captured from the initialize
	// client_info block. Logged at handshake; not otherwise
	// load-bearing.
	clientName    atomic.Value // string
	clientVersion atomic.Value // string
}

// NewSession returns a fresh Session in the pre-handshake state.
func NewSession() *Session { return &Session{} }

func (s *Session) MarkInitialized()           { s.initialized.Store(true) }
func (s *Session) IsInitialized() bool        { return s.initialized.Load() }
func (s *Session) SetRunEventsEnabled(v bool) { s.runEventsEnabled.Store(v) }
func (s *Session) RunEventsEnabled() bool     { return s.runEventsEnabled.Load() }

// SetClientInfo records the client's name/version (captured from
// initialize.clientInfo). Best-effort logging input.
func (s *Session) SetClientInfo(name, version string) {
	if name != "" {
		s.clientName.Store(name)
	}
	if version != "" {
		s.clientVersion.Store(version)
	}
}

// ClientName returns the client's name or "" if not set.
func (s *Session) ClientName() string {
	v, _ := s.clientName.Load().(string)
	return v
}

// ClientVersion returns the client's version or "" if not set.
func (s *Session) ClientVersion() string {
	v, _ := s.clientVersion.Load().(string)
	return v
}
