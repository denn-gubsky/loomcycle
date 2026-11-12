package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// applyEnvConcurrency reads the per-user and provider queue overrides of the
// yaml concurrency block.
func applyEnvConcurrency(cfg *Config) {
	// v0.10.1 per-tenant fairness env var. Yaml is the canonical source
	// (cfg.Concurrency.MaxConcurrentRunsPerUser); the env override lets
	// containerized operators set it without editing the mounted yaml.
	// Negative values clamp to 0 (= disabled, back-compat).
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				cfg.Concurrency.MaxConcurrentRunsPerUser = 0
			} else {
				cfg.Concurrency.MaxConcurrentRunsPerUser = n
			}
		}
	}

	// How many runs one (tenant, user) may have queued on its per-user cap.
	// Negative values are ignored (keep the default).
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Concurrency.MaxQueuedRunsPerUser = n
		}
	}

	// How many requests that hold no run slot may wait for a capped provider.
	// Defaults to the global max_queue_depth (see ProviderQueueDepthOrDefault);
	// this env override lets a containerized operator tune it without editing
	// mounted yaml.
	// Negative values are ignored (keep the inherit-0).
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_PROVIDER_QUEUE_DEPTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Concurrency.ProviderQueueDepth = n
		}
	}
}

// applyEnvToolLimits reads the payload caps of the tools and the run
// ingest, refusing a cap that does not parse where a default would widen it.
func applyEnvToolLimits(cfg *Config) error {
	// Channel tool defaults (v0.8.4). Per-write 64 KB, 15-minute
	// TTL sweep cadence, 30 s long-poll cap — same shape as Memory's
	// negative-as-disable convention.
	cfg.Env.ChannelsMaxValueBytes = 64 * 1024
	if v := os.Getenv("LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsMaxValueBytes = 0
			} else {
				cfg.Env.ChannelsMaxValueBytes = n
			}
		}
	}
	// Run-ingest body cap (RFC AT). Default 16 MiB so a request can carry an
	// inline base64 image; a positive override tunes it, a non-positive value
	// is ignored (keep the default — the cap is a security floor).
	cfg.Env.MaxRequestBytes = 16 << 20
	if v := os.Getenv("LOOMCYCLE_MAX_REQUEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.MaxRequestBytes = n
		}
	}
	// The audio routes' engine queues. A non-positive value is ignored: zero
	// slots would park every request for good.
	cfg.Env.SpeechMaxInflight = AudioMaxInflightDefault
	cfg.Env.TranscriptionMaxInflight = AudioMaxInflightDefault
	for name, dst := range map[string]*int{
		"LOOMCYCLE_SPEECH_MAX_INFLIGHT":        &cfg.Env.SpeechMaxInflight,
		"LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT": &cfg.Env.TranscriptionMaxInflight,
	} {
		if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
			*dst = n
		}
	}
	// Bashbox in-memory write cap (RFC ES follow-up): a positive override
	// tunes it; a non-positive or malformed value keeps the default, since
	// the cap is what keeps one call from exhausting the shared heap. gbash
	// writes a few KiB of command stubs into the overlay as each call starts,
	// so a value under 1 MiB is raised to 1 MiB rather than failing every
	// call on a read-only volume.
	cfg.Env.BashboxOverlayMaxBytes = BashboxOverlayMaxBytesDefault
	if v := os.Getenv("LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.BashboxOverlayMaxBytes = max(n, 1<<20)
		}
	}
	// Inbox and MCP frame caps (RFC FF). Refused rather than defaulted when
	// they do not parse: a cap meant to be lowered and read as the default
	// would keep more than the operator allowed.
	for _, c := range []struct {
		env string
		dst *int64
		def int64
	}{
		{"LOOMCYCLE_FILE_MAX_BYTES", &cfg.Env.FileMaxBytes, FileMaxBytesDefault},
		{"LOOMCYCLE_FILE_INBOX_MAX_BYTES", &cfg.Env.FileInboxMaxBytes, FileInboxMaxBytesDefault},
		{"LOOMCYCLE_MCP_MAX_FRAME_BYTES", &cfg.Env.MCPMaxFrameBytes, MCPMaxFrameBytesDefault},
	} {
		*c.dst = c.def
		if v := strings.TrimSpace(os.Getenv(c.env)); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s: %q is not a positive number of bytes", c.env, v)
			}
			*c.dst = n
		}
	}
	// Extraction concurrency (RFC FZ Part B). Unset or non-positive takes
	// GOMAXPROCS; a value that does not parse is refused rather than
	// defaulted, so a bound the operator meant to lower is never read as a
	// higher one.
	cfg.Env.ExtractConcurrency = runtime.GOMAXPROCS(0)
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_EXTRACT_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("LOOMCYCLE_EXTRACT_CONCURRENCY: %q is not a whole number", v)
		}
		if n > 0 {
			cfg.Env.ExtractConcurrency = n
		}
	}
	cfg.Env.KMSVaultAllowHTTP = os.Getenv("LOOMCYCLE_KMS_VAULT_ALLOW_HTTP") == "1"
	return nil
}

// applyEnvChannels reads the channel sweep, long-poll and deferred caps.
func applyEnvChannels(cfg *Config) {
	cfg.Env.ChannelsSweepInterval = 15 * time.Minute
	if v := os.Getenv("LOOMCYCLE_CHANNELS_SWEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsSweepInterval = 0
			} else {
				cfg.Env.ChannelsSweepInterval = time.Duration(n) * time.Millisecond
			}
		}
	}
	cfg.Env.ChannelsLongPollCapMS = 30000
	if v := os.Getenv("LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsLongPollCapMS = 0
			} else {
				cfg.Env.ChannelsLongPollCapMS = n
			}
		}
	}
	cfg.Env.ChannelsMaxPendingDeferred = 10000
	if v := os.Getenv("LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.ChannelsMaxPendingDeferred = 0
			} else {
				cfg.Env.ChannelsMaxPendingDeferred = n
			}
		}
	}
}
