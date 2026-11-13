package http

import (
	"time"

	"github.com/denn-gubsky/loomcycle/internal/capabilities"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RetentionRunning is the retention configuration the background sweepers
// were started with: the retention sweeper's switch, cadence and families,
// and the usage rollup's detail age. Both sweepers keep what they were given
// at boot, and `retention` is not a section a reload applies, so after a
// reload the config holder describes the next start, not this one. What the
// server says about retention comes from here.
//
// The trace age (retention.trace.max_age) is not part of it: no sweeper holds
// it. The trace index reads it from the config holder at each write, as the
// TTL of the row it writes, so a reload applies it to every later turn and
// the holder's value is the one in force.
type RetentionRunning struct {
	Enabled            bool
	Interval           time.Duration
	DefsMode           string
	DefsMaxAge         time.Duration
	DefsKeepLastN      int
	ChatsMode          string
	ChatsMaxAge        time.Duration
	MemMode            string
	MemMaxAge          time.Duration
	MemContentMode     string
	MemContentMaxAge   time.Duration
	EmptyDossierMode   string
	EmptyDossierMaxAge time.Duration
	ExportDir          string
	UsageDetail        time.Duration
}

// RetentionRunningFrom is the retention configuration env gives a sweeper,
// with the fallbacks the sweepers apply, so that two configs that run the
// same retention compare equal.
func RetentionRunningFrom(env config.Env) RetentionRunning {
	mode := func(m string) string {
		if m == "" {
			return config.RetentionModeOff
		}
		return m
	}
	r := RetentionRunning{
		Enabled:            env.RetentionEnabled,
		Interval:           env.RetentionInterval,
		DefsMode:           mode(env.RetentionDefsMode),
		DefsMaxAge:         env.RetentionDefsMaxAge,
		DefsKeepLastN:      env.RetentionDefsKeepLastN,
		ChatsMode:          mode(env.RetentionChatsMode),
		ChatsMaxAge:        env.RetentionChatsMaxAge,
		MemMode:            mode(env.RetentionMemMode),
		MemMaxAge:          env.RetentionMemMaxAge,
		MemContentMode:     mode(env.RetentionMemContentMode),
		MemContentMaxAge:   env.RetentionMemContentMaxAge,
		EmptyDossierMode:   mode(env.RetentionEmptyDossierMode),
		EmptyDossierMaxAge: env.RetentionEmptyDossierMaxAge,
		ExportDir:          env.RetentionExportDir,
		UsageDetail:        env.UsageDetailRetention,
	}
	if r.Interval <= 0 {
		r.Interval = time.Hour // retention.New's fallback
	}
	if r.UsageDetail <= 0 {
		r.UsageDetail = 30 * 24 * time.Hour // usage.New's fallback
	}
	return r
}

// SetRetentionRunning records what the retention and usage sweepers were
// started with. cmd/loomcycle calls it once, where it starts them.
func (s *Server) SetRetentionRunning(r RetentionRunning) { s.retentionAtBoot.Store(&r) }

// retentionRunning returns the retention configuration in force and whether
// the current config asks for another, which takes a restart. A server never
// told what its sweepers run with (a test fixture, a command that starts
// none) answers from the config, with nothing pending.
func (s *Server) retentionRunning() (r RetentionRunning, pendingRestart bool) {
	var now RetentionRunning
	if cfg := s.cfg(); cfg != nil {
		now = RetentionRunningFrom(cfg.Env)
	}
	boot := s.retentionAtBoot.Load()
	if boot == nil {
		return now, false
	}
	return *boot, *boot != now
}

// capabilityInputs is what the shared capability probe reads for this
// server.
func (s *Server) capabilityInputs(admin bool) capabilities.Inputs {
	in := capabilities.Inputs{
		Cfg:      s.cfg(),
		Store:    s.store,
		Embedder: s.embedder,
		SQLMem:   s.sqlMem != nil,
		Admin:    admin,
	}
	if boot := s.retentionAtBoot.Load(); boot != nil {
		in.RetentionRunning = &boot.Enabled
	}
	return in
}
