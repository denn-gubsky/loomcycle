// Package heartbeat runs the periodic stale-run sweeper alongside the
// HTTP server. Without it, runs whose host process crashed mid-loop
// stay status="running" forever (UpdateHeartbeat never reaches the
// store, FinishRun never fires) and pollute the active-run lists.
//
// The sweeper is store-agnostic — it calls store.SweepStaleRuns on a
// timer and logs the count. The Store adapter is responsible for the
// atomic UPDATE that picks the right rows.
package heartbeat

import (
	"context"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config carries the sweeper's tuning knobs. Zero/missing values fall
// back to defaults documented per-field.
type Config struct {
	// Interval is how often the sweeper wakes up to look for stale
	// runs. Default 60s. Setting it shorter than StaleAfter is fine
	// (more frequent checks → faster cleanup) but doesn't help below
	// the loop's heartbeat tick rate.
	Interval time.Duration

	// StaleAfter is the cutoff: runs whose last heartbeat (or
	// started_at, if they never heartbeated) is older than this are
	// marked failed. Default 10 minutes. Should be ≥ 2× the loop's
	// expected per-iteration time to avoid sweeping live runs that
	// happen to be in a long tool call.
	StaleAfter time.Duration

	// Logger is the structured logger used for sweep results. Defaults
	// to log.Printf when nil. Errors are logged at every tick;
	// successful sweeps with zero rows are logged only periodically
	// (the runtime is fine; chattering would just be noise).
	Logger func(format string, args ...any)
}

const (
	defaultInterval   = 60 * time.Second
	defaultStaleAfter = 10 * time.Minute
)

// Sweeper periodically marks stale running rows as failed. Construct
// via New, then call Run(ctx) on a goroutine that owns the lifecycle.
type Sweeper struct {
	store      store.Store
	interval   time.Duration
	staleAfter time.Duration
	logf       func(format string, args ...any)
}

// New constructs a Sweeper with the supplied tuning. A nil store means
// "no persistence" (the Server can run without a Store) — Run is then a
// no-op.
func New(st store.Store, cfg Config) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaultStaleAfter
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Printf
	}
	return &Sweeper{
		store:      st,
		interval:   cfg.Interval,
		staleAfter: cfg.StaleAfter,
		logf:       cfg.Logger,
	}
}

// Run drives the sweep loop until ctx is done. Each tick calls
// SweepStaleRuns with cutoff = now - StaleAfter. The first tick fires
// after Interval (not immediately) so a fresh process doesn't sweep
// rows that legitimately survived a graceful restart.
//
// This function blocks. Caller is expected to start it on a goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.logf("heartbeat: sweeper starting (interval=%s, stale_after=%s)", s.interval, s.staleAfter)
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// Track consecutive zero-row sweeps so the no-op log line is
	// emitted on the first sweep, then suppressed until either a
	// non-zero sweep or every Nth tick (every hour at default
	// interval — enough to confirm the goroutine is alive without
	// flooding the log).
	const noOpHeartbeatEvery = 60
	noOpStreak := 0

	for {
		select {
		case <-ctx.Done():
			s.logf("heartbeat: sweeper stopping (ctx done)")
			return
		case <-t.C:
			n, err := s.sweepOnce(ctx)
			if err != nil {
				s.logf("heartbeat: sweep failed: %v", err)
				continue
			}
			if n > 0 {
				s.logf("heartbeat: marked %d stale run(s) as failed", n)
				noOpStreak = 0
				continue
			}
			if noOpStreak == 0 || noOpStreak%noOpHeartbeatEvery == 0 {
				s.logf("heartbeat: sweep tick — 0 stale runs (streak=%d)", noOpStreak)
			}
			noOpStreak++
		}
	}
}

// sweepOnce runs one sweep. Exposed as a method (vs inlined in Run) so
// tests can drive ticks deterministically without a real Ticker. ctx
// is the parent's; the SweepStaleRuns call gets a derived 30s timeout
// so a hung backend can't stall the goroutine indefinitely.
func (s *Sweeper) sweepOnce(parent context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-s.staleAfter)
	return s.store.SweepStaleRuns(ctx, cutoff)
}
