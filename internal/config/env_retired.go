package config

import (
	"fmt"
	"sort"
	"strings"
)

// retiredEnv lists LOOMCYCLE_* variables the runtime used to read, or that
// its own examples used to advertise, and no longer honours. A variable is
// removed outright (there is no compatibility read), so without this table an
// operator who still sets one gets no sign that the line does nothing.
//
// It is a table of names loomcycle itself retired, not "every name the
// runtime does not read": operators use their own LOOMCYCLE_* names from YAML
// (${LOOMCYCLE_MY_WEBHOOK_SECRET}, token_env, an env allowlist), and those
// must not be reported.
//
// Each PR that deletes a variable's read adds its row here. Instead says what
// to do, in a form that completes "… is no longer read: ".
var retiredEnv = map[string]string{
	"LOOMCYCLE_INTERRUPTION_DEFAULT_TIMEOUT_MS":    "set interruption.default_timeout_ms in the yaml",
	"LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS":        "set interruption.max_timeout_ms in the yaml",
	"LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN":   "set interruption.max_pending_per_run in the yaml",
	"LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS": "the interval is fixed",
	"LOOMCYCLE_SNAPSHOT_MAX_BYTES":                 "pass max_bytes on the snapshot request",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT":        "retired: use OTEL_EXPORTER_OTLP_ENDPOINT, with its scheme (http://host:4318)",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS":         "retired: use OTEL_EXPORTER_OTLP_HEADERS or LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE",
	"LOOMCYCLE_OTEL_SERVICE_NAME":                  "retired: use OTEL_SERVICE_NAME",
	"LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO":          "retired: use OTEL_TRACES_SAMPLER=parentbased_traceidratio with OTEL_TRACES_SAMPLER_ARG",

	// Feature gates removed: the feature is always on.
	"LOOMCYCLE_SCHEDULER_ENABLED":                   "the scheduler is always on; a schedule fires unless its definition is disabled",
	"LOOMCYCLE_WEBHOOKS_ENABLED":                    "the inbound webhook receiver is always mounted",
	"LOOMCYCLE_CODE_AGENTS_ENABLED":                 "the code-js provider is always registered",
	"LOOMCYCLE_CODE_HOOKS_ENABLED":                  "a hook body may always be code-js",
	"LOOMCYCLE_CHANNEL_HOOKS":                       "the channel-hook worker always runs; a channel's hooks are always applied",
	"LOOMCYCLE_BASHBOX_ENABLED":                     "Bashbox is always registered; an agent gets it through its tools list",
	"LOOMCYCLE_RESUME_FANOUT":                       "a fan-out parent always parks on pause and is resumed with its children",
	"LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES":      "the cap is fixed at 128 KiB",
	"LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES":     "the cap is fixed at 8 KiB",
	"LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES":            "the cap is fixed at 256 KiB",
	"LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES":       "the cap is fixed at 1 MiB",
	"LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES":            "the cap is fixed at 128 KiB",
	"LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES":     "the cap is fixed at 8 KiB",
	"LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES":      "the cap is fixed at 32 KiB",
	"LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES":      "the cap is fixed at 8 KiB",
	"LOOMCYCLE_HEARTBEAT_SWEEPER":                   "the run heartbeat sweeper always runs",
	"LOOMCYCLE_HEARTBEAT_SWEEP_INTERVAL_MS":         "the sweep runs every 60s",
	"LOOMCYCLE_USAGE_SWEEPER":                       "the usage rollup always runs",
	"LOOMCYCLE_USAGE_SWEEP_INTERVAL_MS":             "the rollup runs every hour",
	"LOOMCYCLE_SESSION_LOCK_GC_INTERVAL_MS":         "session locks are collected every 5 minutes",
	"LOOMCYCLE_SESSION_LOCK_MAX_IDLE_MS":            "a session lock idle for 10 minutes is collected",
	"LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS":           "old metrics samples are deleted every 15 minutes; LOOMCYCLE_METRICS_RETENTION_DAYS sets how many are kept",
	"LOOMCYCLE_TEAM_SUBSCRIPTIONS_TICK_SECONDS":     "armed team subscriptions are swept every 15s",
	"LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS":     "expired registered agents are deleted every 15 minutes",
	"LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS":           "ephemeral volumes of ended runs are purged every 60s",
	"LOOMCYCLE_MEMORY_SWEEP_MS":                     "expired Memory entries are deleted every 15 minutes",
	"LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS":           "providers are re-probed every 15 minutes",
	"LOOMCYCLE_SSE_KEEPALIVE_MS":                    "a run stream sends a keepalive every 20s",
	"LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS": "chats of internal agents are deleted after 3 days when chat retention is on",
}

// retiredEnvWarnings returns one advisory per retired variable present in
// environ (the os.Environ form), sorted by name. A variable set to the empty
// string counts: the line is still dead weight in the operator's file. Values
// are never included; several retired names held secrets.
func retiredEnvWarnings(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		instead, retired := retiredEnv[name]
		if !retired {
			continue
		}
		out = append(out, fmt.Sprintf("%s is set but no longer read: %s", name, strings.TrimPrefix(instead, "retired: ")))
	}
	sort.Strings(out)
	return out
}
