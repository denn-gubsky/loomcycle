package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// HeartbeatSpec declares one cadence channel for the runner. Name is
// the channel as declared in operator yaml (must start with `_system/`
// for the prefix-reserved convention); Period is the parsed time.Duration
// from yaml; DefaultTTL is the per-message TTL in seconds (0 = no TTL,
// messages live until max_messages trims them).
type HeartbeatSpec struct {
	Name        string
	Period      time.Duration
	DefaultTTL  int
	MaxMessages int
}

// HeartbeatPayload is the fixed minimal shape every heartbeat carries.
// Consumers can rely on these three fields; future additive fields are
// allowed but existing keys won't change.
type HeartbeatPayload struct {
	Timestamp string `json:"ts"`       // RFC3339Nano
	Version   string `json:"version"`  // buildCommit / "unknown"
	UptimeSec int64  `json:"uptime_s"` // process uptime
}

// HeartbeatRunner manages one goroutine per declared `_system/*`
// cadence channel. It publishes a fixed-shape payload at each tick
// via the SystemPublisher; messages land with
// `published_by_user_id = "_system"`.
//
// Stopping: call Stop() to cancel all tickers + drain. Safe to call
// once; subsequent calls are no-ops.
type HeartbeatRunner struct {
	publisher SystemPublisher
	specs     []HeartbeatSpec
	version   string
	startedAt time.Time

	cancel  context.CancelFunc
	stopped sync.Once
	wg      sync.WaitGroup
}

// NewHeartbeatRunner constructs the runner. version is typically the
// build commit (read from main's `buildCommit` ldflags var); empty
// is fine and falls through to "unknown" in the published payload.
func NewHeartbeatRunner(publisher SystemPublisher, version string, specs []HeartbeatSpec) *HeartbeatRunner {
	if version == "" {
		version = "unknown"
	}
	return &HeartbeatRunner{
		publisher: publisher,
		specs:     specs,
		version:   version,
		startedAt: time.Now(),
	}
}

// Start launches one goroutine per spec. Returns immediately;
// goroutines run until Stop() is called or the parent context is
// cancelled. Each goroutine publishes the first tick AFTER one period
// — no immediate-publish-on-start, to avoid an artificial flurry on
// every restart.
func (h *HeartbeatRunner) Start(parent context.Context) {
	if len(h.specs) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	h.cancel = cancel
	for _, spec := range h.specs {
		if spec.Period <= 0 {
			log.Printf("heartbeat: skip %q (non-positive period)", spec.Name)
			continue
		}
		h.wg.Add(1)
		go h.run(ctx, spec)
	}
}

// Stop cancels all heartbeat goroutines + waits for them to drain.
// Idempotent — safe to call multiple times.
func (h *HeartbeatRunner) Stop() {
	h.stopped.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		h.wg.Wait()
	})
}

// run is one heartbeat goroutine. Publishes at Period intervals;
// terminates on ctx.Done. Skip-on-pause semantics: if the parent
// context blocks Publish (e.g. v0.8.9 pause holds the system loop),
// we just skip that tick — heartbeats represent "now," not historical
// "then." Errors are logged + the loop continues.
func (h *HeartbeatRunner) run(ctx context.Context, spec HeartbeatSpec) {
	defer h.wg.Done()
	t := time.NewTicker(spec.Period)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			payload := HeartbeatPayload{
				Timestamp: now.UTC().Format(time.RFC3339Nano),
				Version:   h.version,
				UptimeSec: int64(now.Sub(h.startedAt).Seconds()),
			}
			body, err := json.Marshal(payload)
			if err != nil {
				log.Printf("heartbeat %s: marshal: %v", spec.Name, err)
				continue
			}
			if _, err := h.publisher.PublishNow(ctx, spec.Name, store.MemoryScopeGlobal, "",
				body, SystemPublisherUserID, spec.MaxMessages, spec.DefaultTTL); err != nil {
				// ctx-cancel during publish = clean shutdown; don't
				// noisy-log.
				if ctx.Err() != nil {
					return
				}
				log.Printf("heartbeat %s: publish: %v", spec.Name, err)
			}
		}
	}
}

// LoadHeartbeatSpecs walks the operator-declared channels block and
// returns one HeartbeatSpec per `publisher: system` + non-zero Period
// channel. Event-driven system channels (no Period) are returned in
// the caller's iteration order via a separate path; this helper
// covers cadence channels only.
//
// Returns (specs, error) where error is non-nil for any malformed
// duration string — the validation layer should have caught these,
// but the helper double-checks rather than panicking.
//
// The cfg argument is the v0.8.6 Channel struct shape via a callback
// to avoid importing the config package (preserves channels → config
// dep direction).
func LoadHeartbeatSpecs(channels map[string]struct {
	Period      string
	Publisher   string
	DefaultTTL  int
	MaxMessages int
}) ([]HeartbeatSpec, error) {
	var specs []HeartbeatSpec
	for name, ch := range channels {
		if ch.Publisher != "system" || ch.Period == "" {
			continue
		}
		d, err := time.ParseDuration(ch.Period)
		if err != nil {
			return nil, fmt.Errorf("channel %q: parse period %q: %w", name, ch.Period, err)
		}
		specs = append(specs, HeartbeatSpec{
			Name:        name,
			Period:      d,
			DefaultTTL:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
		})
	}
	return specs, nil
}
