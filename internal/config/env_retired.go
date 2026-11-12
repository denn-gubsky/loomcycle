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
	"LOOMCYCLE_SQLMEM_ROOT":                         "set storage.sqlmem_root in the yaml",
	"LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS":         "set storage.sqlmem_statement_timeout_ms in the yaml",
	"LOOMCYCLE_SQLMEM_MAX_ROWS":                     "set storage.sqlmem_max_rows in the yaml",
	"LOOMCYCLE_SQLMEM_MAX_RESULT_BYTES":             "set storage.sqlmem_max_result_bytes in the yaml",
	"LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS":               "set storage.sqlmem_txn_timeout_ms in the yaml",
	"LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS":                "set storage.sqlmem_max_open_txns in the yaml",
	"LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT":     "set storage.sqlmem_max_open_txns_per_tenant in the yaml",
	"LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH":                "set storage.sqlmem_max_txn_depth in the yaml",
	"LOOMCYCLE_SQLMEM_GC_INTERVAL_MS":               "set storage.sqlmem_gc_interval_ms in the yaml",
	"LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES":     "set storage.sqlmem_snapshot_max_scope_bytes in the yaml",
	"LOOMCYCLE_GIT_SOURCE_MAX_BYTES":                "a clone's byte limit is fixed at 256 MiB",
	"LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES":         "a clone's object limit is fixed at 64 MiB",
	"LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES":           "a clone's per-file limit is fixed at 5 MiB",
	"LOOMCYCLE_GIT_SOURCE_MAX_FILES":                "a clone's file-count limit is fixed at 100000",
	"LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS":          "a clone's time limit is fixed at 120s",
	"LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT":           "one clone runs at a time per replica",
	"LOOMCYCLE_GIT_SOURCE_MAX_HISTORY":              "a clone's history limit is fixed at 500 commits",
	"LOOMCYCLE_GIT_PUSH_MAX_BYTES":                  "one Git push may send at most 100 MiB",
	"LOOMCYCLE_FILE_BASE64_MIN_BYTES":               "a base64 string of 4 KiB or more is captured as a file",
	"LOOMCYCLE_CRYPTO_MAX_BYTES":                    "a Crypto data argument is capped at 1 MiB",
	"LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN":              "a run may make 100 Crypto calls",
	"LOOMCYCLE_MAX_DOCUMENT_ASSET_BYTES":            "a document image and the /v1/_document body are capped at 8 MiB",
	"LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS":      "a filtered document search binds at most 50000 chunk ids",
	"LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES":            "a channel message is capped at 64 KiB",
	"LOOMCYCLE_CHANNELS_SWEEP_MS":                   "expired channel messages are deleted every 15 minutes",
	"LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED":       "at most 10000 deferred publishes hold a timer",
	"LOOMCYCLE_CHANNEL_HOOKS_CONCURRENCY":           "a replica's hook worker decides 16 messages at once",
	"LOOMCYCLE_CHANNEL_HOOKS_PER_CHANNEL":           "a replica's hook worker decides 4 messages of one channel at once",
	"LOOMCYCLE_CHANNEL_HOOKS_MAX_WAIT":              "a message waits 15 minutes on fail-closed hooks before it is dropped",
	"LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS":   "a registered agent lives 24h unless its request names ttl_seconds",
	"LOOMCYCLE_INTERACTIVE_CHILD_IDLE_TTL_MS":       "a resident child is reaped after 30 minutes idle; open's idle_ttl_seconds overrides it per child",
	"LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS":           "a resident child's turn is cut at 2h",
	"LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS":          "a spawn's timeout_ms has no ceiling",
	"LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS":              "one Agent poll blocks at most 60s",
	"LOOMCYCLE_CLIENT_TOOL_TIMEOUT_MS":              "a client-tool call blocks at most 60s",
	"LOOMCYCLE_CLIENT_TOOL_MAX_BYTES":               "a client-tool frame is capped at 1 MiB",
	"LOOMCYCLE_CLIENT_TOOL_MAX_CONNS":               "a principal holds at most 8 client-tool connections",
	"LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS":               "a cross-replica cancel waits 5s for its acknowledgement",
	"LOOMCYCLE_PAUSE_CACHE_TTL_MS":                  "the shared pause state is cached for 1s",
	"LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS":            "a pause waits 30s unless the request names timeout_ms",
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
