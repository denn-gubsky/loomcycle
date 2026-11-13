package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The modes of the dead-link collection (LOOMCYCLE_DEADLINK_GC).
const (
	// DeadLinkGCOn collects and deletes. The default.
	DeadLinkGCOn = "on"
	// DeadLinkGCDryRun counts and logs what it would collect; deletes nothing.
	DeadLinkGCDryRun = "dry-run"
	// DeadLinkGCOff does not run the collection.
	DeadLinkGCOff = "off"
)

// DeadLinkGCInterval is how often the dead-link collection runs. Hourly
// rather than every fifteen minutes because an orphan costs nothing while it
// sits there (the read-time guard already hides it), so this is about not
// accumulating forever, not about latency.
const DeadLinkGCInterval = time.Hour

// applyEnvDeadLinkGC reads the dead-link collection's mode. A sweeper that
// deletes data must not guess at a typo: any value but the three fails the
// load, so "of" does not silently mean on.
func applyEnvDeadLinkGC(cfg *Config) error {
	switch v := strings.TrimSpace(os.Getenv("LOOMCYCLE_DEADLINK_GC")); v {
	case "":
		cfg.Env.DeadLinkGC = DeadLinkGCOn
	case DeadLinkGCOn, DeadLinkGCDryRun, DeadLinkGCOff:
		cfg.Env.DeadLinkGC = v
	default:
		return fmt.Errorf("LOOMCYCLE_DEADLINK_GC must be off, dry-run or on (unset is on)")
	}
	return nil
}

// applyEnvMemory reads the memory, document feed and vector memory knobs,
// each with its default.
func applyEnvMemory(cfg *Config) {
	// Memory tool defaults: per-scope 1 MB. Negative values are treated as
	// "disable".
	cfg.Env.MemoryMaxScopeBytes = 1024 * 1024
	if v := os.Getenv("LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryMaxScopeBytes = 0
			} else {
				cfg.Env.MemoryMaxScopeBytes = n
			}
		}
	}
	cfg.Env.MemoryPendingDrainedTTL = 7 * 24 * time.Hour
	if v := os.Getenv("LOOMCYCLE_MEMORY_PENDING_DRAINED_TTL_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if n <= 0 {
				cfg.Env.MemoryPendingDrainedTTL = 0
			} else {
				cfg.Env.MemoryPendingDrainedTTL = time.Duration(n) * time.Millisecond
			}
		}
	}

	// v0.9.0 Vector Memory env vars.
	cfg.Env.PgvectorEnabled = os.Getenv("LOOMCYCLE_PGVECTOR_ENABLED") == "1"
}
