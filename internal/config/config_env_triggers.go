package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// applyEnvCodeAgents reads the code-js provider, interactive idle and batch
// join knobs.
func applyEnvCodeAgents(cfg *Config) error {
	// Refused rather than defaulted when it does not parse: "60" meant as
	// minutes, read as the hour default, would end chats nobody expected to.
	cfg.Env.InteractiveIdleTimeout = time.Hour
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT: %q is not a duration such as 1h or 30m (0 waits forever)", v)
		}
		cfg.Env.InteractiveIdleTimeout = d
	}
	// An hour, as for a parked interactive run: a join that has waited that
	// long is waiting on a run nobody is driving to an end. Refused rather
	// than defaulted when it does not parse, for the same reason as above.
	cfg.Env.BatchJoinTimeout = time.Hour
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_BATCH_JOIN_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("LOOMCYCLE_BATCH_JOIN_TIMEOUT: %q is not a duration such as 1h or 30m (0 = no server bound)", v)
		}
		cfg.Env.BatchJoinTimeout = d
	}
	// Code-js provider: the root defaults to ./agent_code (mirrors the skills
	// bundling convention); the run timeout is floored at 1s.
	cfg.Env.CodeAgentsRoot = "./agent_code"
	if v := os.Getenv("LOOMCYCLE_CODE_AGENTS_ROOT"); v != "" {
		cfg.Env.CodeAgentsRoot = v
	}
	cfg.Env.CodeAgentsDeterministic = os.Getenv("LOOMCYCLE_CODE_AGENTS_DETERMINISTIC") == "1"
	cfg.Env.CodeAgentsRunTimeout = 120 * time.Second
	if v := os.Getenv("LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Second
			if d < time.Second {
				d = time.Second
			}
			cfg.Env.CodeAgentsRunTimeout = d
		}
	}
	cfg.Env.CodeAgentsMaxWall = 24 * time.Hour
	if v := os.Getenv("LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.CodeAgentsMaxWall = time.Duration(n) * time.Second
		}
	}
	return nil
}

// applyEnvMetrics reads the process-resource metrics sampler knobs.
func applyEnvMetrics(cfg *Config) {
	// Default OFF; the operator opts in via LOOMCYCLE_METRICS_ENABLED=1.
	cfg.Env.MetricsEnabled = os.Getenv("LOOMCYCLE_METRICS_ENABLED") == "1"
	cfg.Env.MetricsSampleInterval = 5 * time.Second
	if v := os.Getenv("LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Millisecond
			// Floor 1s to prevent accidental write-storms from a
			// typo'd value like 50 (interpreted as 50ms not 5s).
			const minInterval = 1 * time.Second
			if d < minInterval {
				d = minInterval
			}
			cfg.Env.MetricsSampleInterval = d
		}
	}
	cfg.Env.MetricsRetentionDays = 7
	if v := os.Getenv("LOOMCYCLE_METRICS_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Env.MetricsRetentionDays = n
		}
	}
	cfg.Env.MetricsCollectSystem = os.Getenv("LOOMCYCLE_METRICS_COLLECT_SYSTEM") == "1"
}

// applyEnvScheduler reads the scheduler, team subscription and
// consolidation fan-out knobs.
func applyEnvScheduler(cfg *Config) {
	cfg.Env.SchedulerTickSeconds = 30
	if v := os.Getenv("LOOMCYCLE_SCHEDULER_TICK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.SchedulerTickSeconds = n
		}
	}
	// RFC BL P2 consolidation fan-out caps. Left at 0 the scheduler applies its
	// own documented defaults (32 targets, 4 concurrent), so an unset env is not
	// "unbounded".
	if v := os.Getenv("LOOMCYCLE_MAX_CONSOLIDATION_TARGETS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.MaxConsolidationTargets = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.MaxConsolidationConcurrency = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST"); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Env.SchedulerEnvAllowlist = append(cfg.Env.SchedulerEnvAllowlist, name)
			}
		}
	}
}

// applyEnvWebhooks reads the inbound-webhook receiver knobs.
func applyEnvWebhooks(cfg *Config) {
	if v := os.Getenv("LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST"); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Env.WebhooksEnvAllowlist = append(cfg.Env.WebhooksEnvAllowlist, name)
			}
		}
	}
	cfg.Env.WebhooksAllowUnauthenticated = os.Getenv("LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED") == "1"
}

// applyEnvA2A reads the A2A server knobs and refuses a bad routing mode.
func applyEnvA2A(cfg *Config) error {
	// v1.x RFC G A2A server surface. Default OFF; the operator opts in by
	// naming the active card to serve, which the surface cannot run without.
	// Declaring cards in the yaml does not opt in: it does not say which one
	// is served. Tenancy routing is host/path-authoritative (trust boundary),
	// never from a request body.
	cfg.Env.A2AServerCardName = strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_SERVER_CARD"))
	cfg.Env.A2AServerEnabled = cfg.Env.A2AServerCardName != ""
	cfg.Env.A2ATenancyRouting = strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_TENANCY_ROUTING"))
	cfg.Env.A2APublicBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("LOOMCYCLE_A2A_PUBLIC_BASE_URL")), "/")
	if cfg.Env.A2AServerEnabled {
		switch cfg.Env.A2ATenancyRouting {
		case "", "none", "host", "path":
		default:
			return fmt.Errorf("LOOMCYCLE_A2A_TENANCY_ROUTING: must be one of none|host|path, got %q", cfg.Env.A2ATenancyRouting)
		}
	}
	return nil
}
