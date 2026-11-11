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
	// Memory tool defaults. Per-write 64 KB, per-scope 1 MB,
	// 15-minute TTL sweep cadence. Negative values are treated as
	// "disable" (matches the SSE keepalive convention above).
	cfg.Env.MemoryMaxValueBytes = 64 * 1024
	if v := os.Getenv("LOOMCYCLE_MEMORY_MAX_VALUE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryMaxValueBytes = 0
			} else {
				cfg.Env.MemoryMaxValueBytes = n
			}
		}
	}
	cfg.Env.DocumentFeedMaxStreams = 4
	if v := os.Getenv("LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Env.DocumentFeedMaxStreams = max(n, 0)
		}
	}
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
	cfg.Env.DocumentProjectionInterval = os.Getenv("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL")
	cfg.Env.MemorySweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_MEMORY_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemorySweepInterval = 0
			} else {
				cfg.Env.MemorySweepInterval = time.Duration(n) * time.Millisecond
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
	cfg.Env.SqliteVecPath = os.Getenv("LOOMCYCLE_SQLITE_VEC_PATH")
	cfg.Env.MemoryEmbedBatchSize = 100
	if v := os.Getenv("LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryEmbedBatchSize = 0
			} else {
				cfg.Env.MemoryEmbedBatchSize = n
			}
		}
	}
	cfg.Env.MemoryEmbedTimeoutMs = 30000
	if v := os.Getenv("LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.MemoryEmbedTimeoutMs = 0
			} else {
				cfg.Env.MemoryEmbedTimeoutMs = n
			}
		}
	}
}
