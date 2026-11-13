package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"os"
	"strconv"
	"time"
)

// applyEnvMemory reads the memory, document feed, dead-link GC and vector
// memory knobs, each with its default.
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
	cfg.Env.DeadLinkGCInterval = time.Hour
	if v := os.Getenv("LOOMCYCLE_DEADLINK_GC_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.DeadLinkGCInterval = 0
			} else {
				cfg.Env.DeadLinkGCInterval = time.Duration(n) * time.Millisecond
			}
		}
	}
	cfg.Env.DeadLinkGCDryRun = os.Getenv("LOOMCYCLE_DEADLINK_GC_DRY_RUN") == "1"
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
