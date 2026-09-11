package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestSystemChannelsBundle_SourceMatchesEmbedded: the bundle exists twice — the
// readable source tree and the flat file the binary go:embeds. Only the
// EMBEDDED copy ships, so editing the source alone changes nothing at runtime
// while looking, in a diff, like it changed everything.
func TestSystemChannelsBundle_SourceMatchesEmbedded(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bundles", "system-channels", "loomcycle.yaml"))
	if err != nil {
		t.Fatalf("read bundle source: %v", err)
	}
	embeddedCopy, err := os.ReadFile(filepath.Join("embedded", "bundles", "system-channels.yaml"))
	if err != nil {
		t.Fatalf("read embedded bundle: %v", err)
	}
	if !bytes.Equal(src, embeddedCopy) {
		t.Error("bundles/system-channels/loomcycle.yaml and cmd/loomcycle/embedded/bundles/system-channels.yaml differ — only the embedded copy ships, so copy the source over it")
	}
}

// heartbeatSpecsFor mirrors main.go's adapter into channels.LoadHeartbeatSpecs,
// so these tests exercise the same path production takes rather than a
// reimplementation of the rule.
func heartbeatSpecsFor(t *testing.T, cfg *config.Config) []channels.HeartbeatSpec {
	t.Helper()
	in := map[string]struct {
		Period      string
		Publisher   string
		DefaultTTL  int
		MaxMessages int
	}{}
	for name, ch := range cfg.Channels {
		in[name] = struct {
			Period      string
			Publisher   string
			DefaultTTL  int
			MaxMessages int
		}{ch.Period, ch.Publisher, ch.DefaultTTL, ch.MaxMessages}
	}
	specs, err := channels.LoadHeartbeatSpecs(in)
	if err != nil {
		t.Fatalf("LoadHeartbeatSpecs: %v", err)
	}
	return specs
}

func systemChannelsConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "system-channels")...)
	if err != nil {
		t.Fatalf("base + system-channels must load + validate cleanly: %v", err)
	}
	return cfg
}

// TestSystemChannelsBundle_DeclaresOnlyWhatSomethingPublishes is the rule this
// bundle exists under, made executable.
//
// A declared channel nothing writes is worse than an absent one: it reads as a
// broken feature rather than an unused one, and a consumer wired against it
// waits forever. `_system/runtime-state` and `_system/provider-events` are in
// the runtime's event-driven registry and have NO publisher — a sweep of every
// SystemPublisher call site finds only the interrupts pair — so they must not
// appear here. Neither must the `alarms/*` set, which never had one either.
func TestSystemChannelsBundle_DeclaresOnlyWhatSomethingPublishes(t *testing.T) {
	cfg := systemChannelsConfig(t)

	want := map[string]bool{
		"_system/heartbeat-1m":        true,
		"_system/heartbeat-5m":        true,
		"_system/heartbeat-1h":        true,
		"_system/interrupts/pending":  true,
		"_system/interrupts/resolved": true,
	}
	for name := range want {
		if _, ok := cfg.Channels[name]; !ok {
			t.Errorf("bundle does not declare %q", name)
		}
	}
	for name := range cfg.Channels {
		if len(name) > 8 && name[:8] == "_system/" && !want[name] {
			t.Errorf("bundle declares %q, which has no publisher in the runtime — a channel nothing writes reads as a broken feature", name)
		}
	}
}

// TestSystemChannelsBundle_HeartbeatTTLsAreTwoPeriods: a heartbeat means "now".
// A TTL longer than a couple of periods lets a Starter that has been down fire
// immediately on a stale tick it was never meant to see — the tick would be
// read as "it is time" when the time has passed.
func TestSystemChannelsBundle_HeartbeatTTLsAreTwoPeriods(t *testing.T) {
	cfg := systemChannelsConfig(t)
	for _, name := range []string{"_system/heartbeat-1m", "_system/heartbeat-5m", "_system/heartbeat-1h"} {
		ch, ok := cfg.Channels[name]
		if !ok {
			t.Fatalf("%s is not declared", name)
		}
		period, err := ch.PeriodDuration()
		if err != nil || period == 0 {
			t.Fatalf("%s: period %q does not parse to a cadence: %v", name, ch.Period, err)
		}
		if want := 2 * int(period.Seconds()); ch.DefaultTTL != want {
			t.Errorf("%s: default_ttl = %d, want %d (2x the %s period)", name, ch.DefaultTTL, want, ch.Period)
		}
		if ch.MaxMessages == 0 || ch.MaxMessages > 10 {
			t.Errorf("%s: max_messages = %d, want a small bound — a clock keeps no history", name, ch.MaxMessages)
		}
	}
}

// TestSystemChannelsBundle_InterruptScopeMatchesThePublisher: the publishers
// pass store.MemoryScopeUser with the run's user id. A `global` declaration
// would put every tenant's pending asks on one wire, and a `agent` one would
// hide them from the user who has to answer.
func TestSystemChannelsBundle_InterruptScopeMatchesThePublisher(t *testing.T) {
	cfg := systemChannelsConfig(t)
	for _, name := range []string{"_system/interrupts/pending", "_system/interrupts/resolved"} {
		ch := cfg.Channels[name]
		if ch.Scope != "user" {
			t.Errorf("%s: scope = %q, want user — the publishers pass MemoryScopeUser with the run's user id", name, ch.Scope)
		}
		if ch.Period != "" {
			t.Errorf("%s: has a period %q — it publishes on an EVENT, never on a clock", name, ch.Period)
		}
	}
}

// TestSystemChannelsBundle_EveryChannelIsSystemPublished: agents must not be
// able to forge a heartbeat or a resolved-interrupt. The `_system/` prefix is
// reserved at the tool layer too, so this is belt to that brace — and it is the
// declaration an operator reads.
func TestSystemChannelsBundle_EveryChannelIsSystemPublished(t *testing.T) {
	cfg := systemChannelsConfig(t)
	for name, ch := range cfg.Channels {
		if len(name) > 8 && name[:8] == "_system/" && ch.Publisher != "system" {
			t.Errorf("%s: publisher = %q, want system", name, ch.Publisher)
		}
	}
}

// TestSystemChannelsBundle_TickersLoadAsHeartbeatSpecs closes the loop from the
// declaration to the goroutine that acts on it: the three cadence channels must
// produce runnable specs, and the event-driven pair must produce none.
//
// Without this the bundle could declare a period the runner never reads and
// look correct in every other test.
func TestSystemChannelsBundle_TickersLoadAsHeartbeatSpecs(t *testing.T) {
	cfg := systemChannelsConfig(t)
	specs := heartbeatSpecsFor(t, cfg)
	got := map[string]int{}
	for _, s := range specs {
		got[s.Name] = int(s.Period.Seconds())
	}
	for name, wantSec := range map[string]int{
		"_system/heartbeat-1m": 60, "_system/heartbeat-5m": 300, "_system/heartbeat-1h": 3600,
	} {
		if got[name] != wantSec {
			t.Errorf("heartbeat spec for %s = %ds, want %ds", name, got[name], wantSec)
		}
	}
	for _, name := range []string{"_system/interrupts/pending", "_system/interrupts/resolved"} {
		if _, ticking := got[name]; ticking {
			t.Errorf("%s produced a heartbeat spec — an event-driven channel must never tick", name)
		}
	}
	if len(specs) != 3 {
		t.Errorf("loaded %d heartbeat specs, want exactly 3", len(specs))
	}
}

// TestSystemChannelsBundle_IsOptIn pins the cost decision: a deployment that
// does NOT select the bundle gets no `_system/` channels and therefore no
// ticker, so it never writes a row a minute for something nothing reads.
func TestSystemChannelsBundle_IsOptIn(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base")...)
	if err != nil {
		t.Fatalf("base alone must load: %v", err)
	}
	for name := range cfg.Channels {
		if len(name) > 8 && name[:8] == "_system/" {
			t.Errorf("base alone declares %q — the tickers must be opted into, not inherited", name)
		}
	}
	if n := len(heartbeatSpecsFor(t, cfg)); n != 0 {
		t.Errorf("base alone loaded %d heartbeat specs, want 0", n)
	}
}
