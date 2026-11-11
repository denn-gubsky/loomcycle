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
	// Git push upload cap (RFC FB). Refused rather than defaulted when it
	// does not parse: a cap the operator meant to lower, read as the default,
	// would let a push send more than they allowed.
	cfg.Env.GitPushMaxBytes = GitPushMaxBytesDefault
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_GIT_PUSH_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("LOOMCYCLE_GIT_PUSH_MAX_BYTES: %q is not a positive number of bytes", v)
		}
		cfg.Env.GitPushMaxBytes = n
	}
	// Inbox and MCP frame caps (RFC FF). Refused rather than defaulted when
	// they do not parse, as the push cap is: a cap meant to be lowered and
	// read as the default would keep more than the operator allowed.
	for _, c := range []struct {
		env string
		dst *int64
		def int64
	}{
		{"LOOMCYCLE_FILE_MAX_BYTES", &cfg.Env.FileMaxBytes, FileMaxBytesDefault},
		{"LOOMCYCLE_FILE_INBOX_MAX_BYTES", &cfg.Env.FileInboxMaxBytes, FileInboxMaxBytesDefault},
		{"LOOMCYCLE_FILE_BASE64_MIN_BYTES", &cfg.Env.FileBase64MinBytes, FileBase64MinBytesDefault},
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
	// Crypto tool limits (RFC EP): a positive override tunes each; a
	// non-positive or malformed value keeps the default.
	cfg.Env.CryptoMaxBytes = 1 << 20
	if v := os.Getenv("LOOMCYCLE_CRYPTO_MAX_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.CryptoMaxBytes = n
		}
	}
	cfg.Env.CryptoMaxOpsPerRun = 100
	if v := os.Getenv("LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.CryptoMaxOpsPerRun = n
		}
	}
	cfg.Env.KMSVaultAllowHTTP = os.Getenv("LOOMCYCLE_KMS_VAULT_ALLOW_HTTP") == "1"
	cfg.Env.MaxDocumentAssetBytes = 8 << 20 // RFC BO
	if v := os.Getenv("LOOMCYCLE_MAX_DOCUMENT_ASSET_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Env.MaxDocumentAssetBytes = n
		}
	}
	cfg.Env.DocumentSearchFilterMaxIDs = 50000
	if v := os.Getenv("LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.DocumentSearchFilterMaxIDs = n
		}
	}
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

// applyEnvDefCaps reads the substrate definition size caps.
func applyEnvDefCaps(cfg *Config) {
	// v0.8.5 substrate caps. Same negative-as-disable convention as
	// Memory + Channel sibling caps.
	cfg.Env.AgentDefMaxDefinitionBytes = 131072
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxDefinitionBytes = 0
			} else {
				cfg.Env.AgentDefMaxDefinitionBytes = n
			}
		}
	}
	cfg.Env.TeamDefMaxDefinitionBytes = 1048576
	if v := os.Getenv("LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.TeamDefMaxDefinitionBytes = 0
			} else {
				cfg.Env.TeamDefMaxDefinitionBytes = n
			}
		}
	}
	cfg.Env.AgentDefMaxDescriptionBytes = 8192
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxDescriptionBytes = 0
			} else {
				cfg.Env.AgentDefMaxDescriptionBytes = n
			}
		}
	}
	cfg.Env.AgentDefMaxCodeBytes = 262144
	if v := os.Getenv("LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.AgentDefMaxCodeBytes = 0
			} else {
				cfg.Env.AgentDefMaxCodeBytes = n
			}
		}
	}
	// v0.8.22 SkillDef caps. Mirror of AgentDef caps.
	cfg.Env.SkillDefMaxBodyBytes = 131072
	if v := os.Getenv("LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SkillDefMaxBodyBytes = 0
			} else {
				cfg.Env.SkillDefMaxBodyBytes = n
			}
		}
	}
	cfg.Env.SkillDefMaxDescriptionBytes = 8192
	if v := os.Getenv("LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SkillDefMaxDescriptionBytes = 0
			} else {
				cfg.Env.SkillDefMaxDescriptionBytes = n
			}
		}
	}
	cfg.Env.EvaluationMaxJudgementBytes = 32768
	if v := os.Getenv("LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EvaluationMaxJudgementBytes = 0
			} else {
				cfg.Env.EvaluationMaxJudgementBytes = n
			}
		}
	}
	cfg.Env.EvaluationMaxRationaleBytes = 8192
	if v := os.Getenv("LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.EvaluationMaxRationaleBytes = 0
			} else {
				cfg.Env.EvaluationMaxRationaleBytes = n
			}
		}
	}
}
