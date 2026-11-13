package config

import (
	"fmt"
	"sort"
	"strconv"
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
	"LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS": "set interruption.heartbeat_interval_ms in the yaml",
	"LOOMCYCLE_SNAPSHOT_MAX_BYTES":                 "pass max_bytes on the snapshot request",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT":        "retired: use OTEL_EXPORTER_OTLP_ENDPOINT, with its scheme (http://host:4318)",
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS":         "retired: use OTEL_EXPORTER_OTLP_HEADERS or LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE",
	"LOOMCYCLE_OTEL_SERVICE_NAME":                  "retired: use OTEL_SERVICE_NAME",
	"LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO":          "retired: use OTEL_TRACES_SAMPLER=parentbased_traceidratio with OTEL_TRACES_SAMPLER_ARG",

	// Feature gates removed: the feature is always on.
	"LOOMCYCLE_SCHEDULER_ENABLED":                   "the scheduler is always on; a schedule fires unless its definition is disabled",
	"LOOMCYCLE_WEBHOOKS_ENABLED":                    "the inbound webhook receiver is always mounted",
	"LOOMCYCLE_CODE_AGENTS_ENABLED":                 "the code-js provider is registered whenever the config declares it (the default providers layer does)",
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
	"LOOMCYCLE_MCP_MAX_CONCURRENT_CALLS":            "the stdio MCP server dispatches 16 tool calls at once",
	"LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS":      "the thin client waits 60s for the upstream's response headers",
	"LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS":     "the thin client retries a transport error 5 times",
	"LOOMCYCLE_LIMITS_RELOAD_INTERVAL":              "token limit rows are re-read every minute",
	"LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS":     "an expired token's row is deleted 7 days after it expired",
	"LOOMCYCLE_MEMORY_CHANGES_RETENTION_HOURS":      "change-feed rows are kept 24h",
	"LOOMCYCLE_CHANGE_SUB_INTERVAL_MS":              "change subscriptions are delivered every 5s",
	"LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE":      "a knowledge-base relation keeps at most 32 supporting quotes",
	"LOOMCYCLE_TENANT_TELEMETRY_QUEUE":              "a tenant telemetry pipeline queues 512 spans",
	"LOOMCYCLE_TENANT_TELEMETRY_MAX_PIPELINES":      "256 tenant telemetry pipelines are open at once",
	"LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS":      "the tenant-root and user-root documents are always created when a principal is established",
	"LOOMCYCLE_MEMORY_MAX_VALUE_BYTES":              "a Memory value is capped at 64 KiB",
	"LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS":           "a principal holds at most 4 document feed streams",
	"LOOMCYCLE_TOOL_PARALLELISM":                    "8 tool calls of one turn run at once",
	"LOOMCYCLE_USAGE_RUN_RETENTION_MS":              "the usage archiver is gone: set retention.chats.mode and retention.chats.max_age in the yaml",
	"LOOMCYCLE_USAGE_RUN_RETENTION_MODE":            "the usage archiver is gone: set retention.chats.mode in the yaml",
	"LOOMCYCLE_USAGE_EXPORT_DIR":                    "the usage archiver is gone: set retention.export_dir in the yaml",
	"LOOMCYCLE_READ_ROOT":                           "the filesystem jail is gone: declare a `volumes:` block and bind agents to it",
	"LOOMCYCLE_WRITE_ROOT":                          "the filesystem jail is gone: declare a `volumes:` block and bind agents to it",
	"LOOMCYCLE_BASH_CWD":                            "the filesystem jail is gone: declare a `volumes:` block; Bash runs in the root of the agent's rw volume",

	// Two variables became one mode.
	"LOOMCYCLE_DEADLINK_GC_MS":      "set LOOMCYCLE_DEADLINK_GC=off to stop the dead-link collection; it otherwise runs every hour",
	"LOOMCYCLE_DEADLINK_GC_DRY_RUN": "set LOOMCYCLE_DEADLINK_GC=dry-run to count unreachable chunk references without deleting them",

	// Inferred from another setting.
	"LOOMCYCLE_SPEECH_MAX_INFLIGHT":        "set LOOMCYCLE_AUDIO_MAX_INFLIGHT, which sizes the queue of both audio routes",
	"LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT": "set LOOMCYCLE_AUDIO_MAX_INFLIGHT, which sizes the queue of both audio routes",
	"LOOMCYCLE_AUTH_VERBOSE":               "set LOOMCYCLE_LOG_LEVEL=debug: a rejected credential is logged at debug level",
	"LOOMCYCLE_CHANNEL_DEBUG":              "set LOOMCYCLE_LOG_LEVEL=debug: each channel publish is logged at debug level",
	"LOOMCYCLE_OLLAMA_DEBUG_THINK":         "set LOOMCYCLE_LOG_LEVEL=debug: each Ollama request's model, effort and think flag are logged at debug level",
	"LOOMCYCLE_A2A_ENABLED":                "A2A is on when LOOMCYCLE_A2A_SERVER_CARD names the server card to serve; unset it to turn A2A off",
	"LOOMCYCLE_RETENTION_ENABLED":          "set retention.enabled in the yaml",
	"LOOMCYCLE_SQLMEM_ENABLED":             "SQL Memory is on with a SQLite store, and with a Postgres store when LOOMCYCLE_SQLMEM_PG_DSN is set; storage.sqlmem_enabled: false in the yaml turns it off",
	"LOOMCYCLE_STORAGE_BACKEND":            "the store is Postgres when LOOMCYCLE_PG_DSN (or storage.pg_dsn) is set and SQLite otherwise; storage.backend in the yaml still overrides",

	// Moved to the yaml `retention:` block. Ages are durations there (30d,
	// 12h), not milliseconds.
	"LOOMCYCLE_RETENTION_INTERVAL_MS":              "set retention.interval in the yaml, as a duration such as 1h",
	"LOOMCYCLE_RETENTION_EXPORT_DIR":               "set retention.export_dir in the yaml",
	"LOOMCYCLE_RETENTION_DEFS_MODE":                "set retention.defs.mode in the yaml",
	"LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS":          "set retention.defs.max_age in the yaml, as a duration such as 30d",
	"LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N":         "set retention.defs.keep_last_n in the yaml",
	"LOOMCYCLE_RETENTION_CHATS_MODE":               "set retention.chats.mode in the yaml",
	"LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS":         "set retention.chats.max_age in the yaml, as a duration such as 90d",
	"LOOMCYCLE_RETENTION_MEM_MODE":                 "set retention.mem.mode in the yaml",
	"LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS":           "set retention.mem.max_age in the yaml, as a duration such as 30d",
	"LOOMCYCLE_RETENTION_MEM_CONTENT_MODE":         "set retention.mem_content.mode in the yaml",
	"LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS":   "set retention.mem_content.max_age in the yaml, as a duration such as 30d",
	"LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE":       "set retention.empty_dossier.mode in the yaml",
	"LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS": "set retention.empty_dossier.max_age in the yaml, as a duration such as 30d",
	"LOOMCYCLE_USAGE_DETAIL_RETENTION_MS":          "set retention.usage_detail.max_age in the yaml, as a duration such as 30d",
	"LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS":            "set retention.trace.max_age in the yaml, as a duration such as 30d",

	// Moved to each provider's `options:` in the yaml.
	"LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS":      "set providers.<id>.options.header_timeout_ms in the yaml for each provider that needs another value than 60000 (the anthropic-oauth-dev provider has no options: it always waits 60000)",
	"LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS":        "set providers.<id>.options.idle_timeout_ms in the yaml for each provider that needs another value than 90000 (the anthropic-oauth-dev provider has no options: it always waits 90000)",
	"LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS":  "set providers.ollama-local.options.header_timeout_ms in the yaml",
	"LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS":    "set providers.ollama-local.options.idle_timeout_ms in the yaml",
	"LOOMCYCLE_OLLAMA_NUM_CTX":                  "set providers.ollama.options.num_ctx in the yaml",
	"LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU":            "set providers.ollama-local.options.num_gpu in the yaml",
	"LOOMCYCLE_CODE_AGENTS_DETERMINISTIC":       "set providers.code-js.options.deterministic in the yaml",
	"LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS": "set providers.code-js.options.run_timeout_seconds in the yaml",
	"LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS":    "set providers.code-js.options.max_wall_seconds in the yaml",

	// Moved to the yaml `timeout_scaling:` block.
	"LOOMCYCLE_TIMEOUT_SCALING":                "set timeout_scaling.mode in the yaml",
	"LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS":  "set timeout_scaling.reference.decode_tps in the yaml",
	"LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER": "set timeout_scaling.max_multiplier in the yaml",

	// Moved to the yaml `memory:` block.
	"LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE":      "set memory.embedder.batch_size in the yaml (1 sends one text per call)",
	"LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS":      "set memory.embedder.timeout_ms in the yaml",
	"LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL": "set memory.projection.interval in the yaml, with memory.projection.enabled: true",

	// Moved to the yaml `concurrency:` block.
	"LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER": "set concurrency.max_concurrent_runs_per_user in the yaml",
	"LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER":     "set concurrency.max_queued_runs_per_user in the yaml",
	"LOOMCYCLE_PROVIDER_QUEUE_DEPTH":         "set concurrency.provider_queue_depth in the yaml",

	// Lists and maps moved to the yaml `observability:` block.
	"LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS":     "set observability.tracing.tenant_sampler_ratios in the yaml, as a map of tenant to ratio",
	"LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS": "set observability.tenant_telemetry.allowed_hosts in the yaml, as a list",

	"LOOMCYCLE_UI_LOGIN_ORIGINS": "set auth.login_origins in the yaml, as a list",

	// The third way to name config layers; --config and the config dir stay.
	"LOOMCYCLE_CONFIG_FILES": "pass each file as --config <path> (repeatable, in the same order), or place the files in LOOMCYCLE_CONFIG_DIR",

	// The hooks lists are yaml-only.
	"LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS": "list each [tenant:]owner under hooks.permit_host_widen.owners in the yaml",
	"LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST":   "list each host or CIDR range under hooks.private_host_allowlist in the yaml",

	// The process metrics sampler moved to the yaml `metrics:` block.
	"LOOMCYCLE_METRICS_ENABLED":            "set metrics.enabled: true in the yaml",
	"LOOMCYCLE_METRICS_COLLECT_SYSTEM":     "set metrics.collect_system: true in the yaml",
	"LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS": "set metrics.sample_interval in the yaml, as a duration such as 5s",
}

// retiredLayerEnv names the retired variables that chose config layers. Their
// rules need no yaml, and the layer assembly applies them before the load.
var retiredLayerEnv = map[string]bool{"LOOMCYCLE_CONFIG_FILES": true}

// retiredStoreEnv names the retired variables that said which store to open.
// Their refusal is never relaxed, for any caller: a tool that let it pass
// would read or write the store the variable did not mean.
var retiredStoreEnv = map[string]bool{"LOOMCYCLE_STORAGE_BACKEND": true}

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

// RetiredEnvAdvisories is retiredEnvWarnings for a command that loads no
// config and so never sees Config.Warnings: the MCP thin client, whose
// environment is a plugin's and may still carry the names it used to read.
func RetiredEnvAdvisories(environ []string) []string {
	return retiredEnvWarnings(environ)
}

// retiredEnvCtx is what a refusal rule may look at besides its own value.
type retiredEnvCtx struct {
	// env is the whole environment, for a rule that depends on a second
	// variable.
	env map[string]string
	// storage is the yaml's storage block before any env override, for the
	// settings that moved to a yaml key.
	storage *StorageConfig
	// cfg is the whole yaml before any env override, for the settings that
	// moved to a block other than storage.
	cfg *Config
}

// retiredRefusal is a retired variable whose value may have asked for
// something tighter than what the runtime now does, for data to live
// somewhere other than where it now goes, or for different data to be kept.
type retiredRefusal struct {
	// loosens reports whether ignoring value leaves the deployment looser
	// than the operator set it, moves its data, or keeps or deletes data
	// they had not.
	loosens func(value string, c retiredEnvCtx) bool
	// instead replaces the retiredEnv text in the refusal when what to do
	// about a refused value differs from what to do about a dead line.
	instead string
}

// retiredEnvInsteadFor builds the refusal text from the value, for the rules
// whose "what to do" depends on it: the two hooks lists name the entry the
// yaml lacks.
var retiredEnvInsteadFor = map[string]func(value string, c retiredEnvCtx) string{
	"LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS": hookOwnersInstead,
	"LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST":   hookPrivateHostsInstead,
}

// retiredEnvRefuse holds the retired variables that stop the load instead of
// only being advised. An advisory is enough when ignoring the variable changes
// a cadence or raises nothing; it is not enough when the operator had turned a
// feature off or lowered a bound, because the deployment would come up looser
// than they configured it with one more line in a log to show for it. The
// same holds for a variable whose value decided where data lives or whether it
// is deleted: ignoring it would open another store or start a sweep.
//
// A rule is judged on the value: the same variable set to what the runtime
// now does anyway is advised, not refused.
//
// A variable whose setting moved to a yaml key in the same release as the
// `retention:` block is not here: retiredEnvMoved holds those, under one rule.
var retiredEnvRefuse = map[string]retiredRefusal{
	// The seven gates. Each old read was `== "1"`, so any other value of a set
	// variable, the empty one included, was the operator keeping the feature
	// off.
	"LOOMCYCLE_SCHEDULER_ENABLED":   {gateWasOff, "the scheduler is always on; to keep it unused, keep no schedule definitions (or disable each one)"},
	"LOOMCYCLE_WEBHOOKS_ENABLED":    {gateWasOff, "the inbound webhook receiver is always mounted; to keep it unused, keep no webhook definitions"},
	"LOOMCYCLE_CODE_AGENTS_ENABLED": {gateWasOff, "the code-js provider is registered whenever the config declares it; to keep code agents unused, run without the default providers layer (LOOMCYCLE_NO_DEFAULT_PROVIDERS=1) and declare your own `providers:` with no code-js entry"},
	"LOOMCYCLE_CODE_HOOKS_ENABLED":  {gateWasOff, "a hook body may always be code-js; to keep code hooks unused, keep hook definitions out of the hands of anyone who should not run JavaScript in this process"},
	"LOOMCYCLE_CHANNEL_HOOKS":       {gateWasOff, "the channel-hook worker always runs; to keep it unused, declare no `hooks:` on any channel"},
	"LOOMCYCLE_BASHBOX_ENABLED":     {gateWasOff, "Bashbox is always registered; to keep it unused, do not name Bashbox in any agent's `tools`"},
	"LOOMCYCLE_RESUME_FANOUT":       {gateWasOff, "a fan-out parent always parks on pause and is resumed with its children; this can no longer be turned off"},

	// The store used to be chosen here. Ignoring a value that disagrees with
	// the DSN would open a different store: an empty SQLite file where the
	// operator's Postgres was expected, or the reverse.
	"LOOMCYCLE_STORAGE_BACKEND": {backendNamedAnotherStore, "the store is Postgres when LOOMCYCLE_PG_DSN (or storage.pg_dsn) is set and SQLite otherwise, which is not the backend this variable names; set storage.backend in the yaml to pin the backend"},

	// SQL Memory used to be off unless this said "1". Kept off, it would now
	// come up on a SQLite store and start hosting per-scope databases; asked
	// for on a Postgres store with no aux DSN, it used to stop the boot and
	// would now stay off with the agents' SQL and Documents refusing.
	"LOOMCYCLE_SQLMEM_ENABLED": {sqlMemSwitchDisagrees, "SQL Memory follows the store, which is not what this variable asked for; to keep it off set storage.sqlmem_enabled: false in the yaml, and to have it on with a Postgres store set LOOMCYCLE_SQLMEM_PG_DSN (with storage.sqlmem_enabled unset or true)"},

	// The sweeper ran only when this said "1"; retention.enabled in the yaml
	// is that switch now. Set to 1 with the yaml not turning it on, retention
	// the operator committed to would stop; set to anything else with the
	// yaml turning it on, data they held back would be deleted.
	"LOOMCYCLE_RETENTION_ENABLED": {retentionSwitchDisagrees, "the retention sweeper runs when retention.enabled is true in the yaml, which is not what this variable asked for; set retention.enabled to match what you want (true to keep retention running, false or absent to keep it off)"},

	// The hosted Ollama context window and the local GPU offload are provider
	// options now. Dropped without the option, requests run in the model's
	// own window (a truncated prompt) or on whatever Ollama picks (often the
	// CPU): nothing fails, the answers get worse or slower. A yaml that names
	// the option always won over the variable, so only its absence refuses.
	"LOOMCYCLE_OLLAMA_NUM_CTX":       {providerOptionAbsent("ollama", "num_ctx"), "requests to hosted Ollama no longer carry this context window; set providers.ollama.options.num_ctx in the yaml to the same value, then remove this variable"},
	"LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU": {providerOptionAbsent("ollama-local", "num_gpu"), "requests to local Ollama no longer carry this GPU offload; set providers.ollama-local.options.num_gpu in the yaml to the same value, then remove this variable"},
	// The files it named would silently not be layered. Judged by the layer
	// assembly as well (RetiredLayerEnvRefusals), which runs first.
	"LOOMCYCLE_CONFIG_FILES": {loosens: valueNotEmpty},
	// The variables added to the yaml lists. An entry the yaml lacks is a
	// hook that stops working: its allow_hosts dropped, or its callback to a
	// private address refused.
	"LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS": {loosens: hookOwnersNotInYAML},
	"LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST":   {loosens: hookPrivateHostsNotInYAML},
	// A2A used to be served only when this said "1". With the card named and
	// the switch off, the A2A endpoints would now be mounted.
	"LOOMCYCLE_A2A_ENABLED": {a2aWasOff, "A2A is served whenever LOOMCYCLE_A2A_SERVER_CARD is set, and it is; unset LOOMCYCLE_A2A_SERVER_CARD to keep A2A off"},

	// Each audio route had its own queue size; one setting now sizes both. A
	// route held to fewer engine requests at once (a small GPU host) would be
	// sent more than the operator allowed.
	"LOOMCYCLE_SPEECH_MAX_INFLIGHT":        {audioQueueLowered, "both audio queues are sized by LOOMCYCLE_AUDIO_MAX_INFLIGHT, which gives this route more engine requests at once than this variable allowed; set LOOMCYCLE_AUDIO_MAX_INFLIGHT to the size you want"},
	"LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT": {audioQueueLowered, "both audio queues are sized by LOOMCYCLE_AUDIO_MAX_INFLIGHT, which gives this route more engine requests at once than this variable allowed; set LOOMCYCLE_AUDIO_MAX_INFLIGHT to the size you want"},

	// The filesystem jail stopped the boot before it was retired and still
	// does: a deploy that relied on it must be moved to volumes by a person.
	"LOOMCYCLE_READ_ROOT":  {loosens: valueNotEmpty},
	"LOOMCYCLE_WRITE_ROOT": {loosens: valueNotEmpty},
	"LOOMCYCLE_BASH_CWD":   {loosens: valueNotEmpty},

	// The usage archiver deleted aged chats. Its replacement only does so when
	// it is configured, so a deployment that relied on the archiver would keep
	// chats it had committed to deleting.
	"LOOMCYCLE_USAGE_RUN_RETENTION_MODE": {loosens: archiverWasPruning},

	// Caps that are now fixed. The number is the fixed value; the old reads
	// took zero or a negative number as "the default" or as "no cap", never
	// as something tighter.
	"LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES":      {loosens: loweredBelow(128 << 10)},
	"LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES":     {loosens: loweredBelow(8 << 10)},
	"LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES":            {loosens: loweredBelow(256 << 10)},
	"LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES":       {loosens: loweredBelow(1 << 20)},
	"LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES":            {loosens: loweredBelow(128 << 10)},
	"LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES":     {loosens: loweredBelow(8 << 10)},
	"LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES":      {loosens: loweredBelow(32 << 10)},
	"LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES":      {loosens: loweredBelow(8 << 10)},
	"LOOMCYCLE_GIT_PUSH_MAX_BYTES":                  {loosens: loweredBelow(100 << 20)},
	"LOOMCYCLE_GIT_SOURCE_MAX_BYTES":                {loosens: loweredBelow(256 << 20)},
	"LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES":         {loosens: loweredBelow(64 << 20)},
	"LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES":           {loosens: loweredBelow(5 << 20)},
	"LOOMCYCLE_GIT_SOURCE_MAX_FILES":                {loosens: loweredBelow(100_000)},
	"LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS":          {loosens: loweredBelow(120)},
	"LOOMCYCLE_GIT_SOURCE_MAX_HISTORY":              {loosens: loweredBelow(500)},
	"LOOMCYCLE_CRYPTO_MAX_BYTES":                    {loosens: loweredBelow(1 << 20)},
	"LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN":              {loosens: loweredBelow(100)},
	"LOOMCYCLE_MAX_DOCUMENT_ASSET_BYTES":            {loosens: loweredBelow(8 << 20)},
	"LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS":      {loosens: loweredBelow(50_000)},
	"LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS":           {loosens: loweredBelow(4)},
	"LOOMCYCLE_MEMORY_MAX_VALUE_BYTES":              {loosens: loweredBelow(64 << 10)},
	"LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES":            {loosens: loweredBelow(64 << 10)},
	"LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED":       {loosens: loweredBelow(10_000)},
	"LOOMCYCLE_CLIENT_TOOL_TIMEOUT_MS":              {loosens: loweredBelow(60_000)},
	"LOOMCYCLE_CLIENT_TOOL_MAX_BYTES":               {loosens: loweredBelow(1 << 20)},
	"LOOMCYCLE_CLIENT_TOOL_MAX_CONNS":               {loosens: loweredBelow(8)},
	"LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS":              {loosens: loweredBelow(60_000)},
	"LOOMCYCLE_INTERACTIVE_CHILD_IDLE_TTL_MS":       {loosens: loweredBelow(30 * 60 * 1000)},
	"LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS":           {loosens: loweredBelow(2 * 60 * 60)},
	"LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS": {internalChatsAgeDiffers, "chats of internal agents are kept 3 days and then deleted (when chat retention is on); the age can no longer be set, here or by inheriting retention.chats.max_age"},

	// Settings that decided what data is kept or deleted. Here a difference in
	// either direction stops the boot: rows the operator meant to keep would
	// be deleted, or rows they meant to be rid of would stay.
	"LOOMCYCLE_USAGE_SWEEPER":                  {switchWasZero, "the usage rollup always runs: per-call usage rows older than retention.usage_detail.max_age (30 days unless set) are deleted, and their daily totals are kept; to keep per-call rows longer, raise retention.usage_detail.max_age in the yaml"},
	"LOOMCYCLE_HEARTBEAT_SWEEPER":              {switchWasZero, "the run heartbeat sweeper always runs: a run whose heartbeat went stale is marked failed; this can no longer be turned off"},
	"LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS": {boolWasOff, "the tenant-root and user-root documents are always created when a principal is established; this can no longer be turned off"},

	// The dead-link collection deletes chunk bodies it finds unreachable. An
	// operator who had it off, or counting only, must say so again in the
	// variable that replaced both before anything is deleted; one who had it
	// run less often than the fixed hour would have rows deleted sooner.
	"LOOMCYCLE_DEADLINK_GC_MS":      {deadLinkGCWasOffOrRarer, "the dead-link collection runs every hour and deletes unreachable chunk references unless LOOMCYCLE_DEADLINK_GC says otherwise; set LOOMCYCLE_DEADLINK_GC=off to keep it off (or dry-run to count without deleting), and remove this variable: its interval is no longer read"},
	"LOOMCYCLE_DEADLINK_GC_DRY_RUN": {deadLinkGCWasDryRun, "the dead-link collection deletes unreachable chunk references unless LOOMCYCLE_DEADLINK_GC says otherwise; set LOOMCYCLE_DEADLINK_GC=dry-run to keep counting without deleting (or off), and remove this variable"},

	// The ceiling is gone, so every ceiling was tighter.
	"LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS": {loosens: loweredBelow(1<<63 - 1)},
	// Zero here was a setting, not "the default": a registration had to name
	// its own ttl.
	"LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS": {loosens: func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && (!ok || n < 86400)
	}},

	// SQL Memory bounds moved to the yaml: judged against what the yaml gives
	// (its own default when the key is absent), so an operator who has already
	// copied the bound into the yaml is not stopped.
	"LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS": {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemStatementTimeoutMS, 30000) })},
	"LOOMCYCLE_SQLMEM_MAX_ROWS":             {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemMaxRows, 10000) })},
	"LOOMCYCLE_SQLMEM_MAX_RESULT_BYTES":     {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemMaxResultBytes, 8<<20) })},
	"LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS":       {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemTxnTimeoutMS, 30000) })},
	"LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS":        {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemMaxOpenTxns, 64) })},
	"LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT": {loosens: loweredBelowYAML(func(s *StorageConfig) int64 {
		// Unset, sqlmem gives a tenant a quarter of the process cap, at least 1.
		return yamlOr(s.SqlMemMaxOpenTxnsPerTenant, int(max(yamlOr(s.SqlMemMaxOpenTxns, 64)/4, 1)))
	})},
	"LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH": {loosens: loweredBelowYAML(func(s *StorageConfig) int64 { return yamlOr(s.SqlMemMaxTxnDepth, 16) })},
	// No yaml value is "no cap", so any cap was tighter than that.
	"LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES": {loosens: loweredBelowYAML(func(s *StorageConfig) int64 {
		if s.SqlMemSnapshotMaxScopeBytes > 0 {
			return s.SqlMemSnapshotMaxScopeBytes
		}
		return 1<<63 - 1
	})},
}

// backendNow is the store backend this version opens: the yaml's explicit
// storage.backend, else Postgres when a DSN is configured. The rules are
// judged before applyEnvStorage runs, so the env DSN is applied here the way
// it is there.
func backendNow(c retiredEnvCtx) string {
	dsn := c.storage.PgDSN
	if v := c.env["LOOMCYCLE_PG_DSN"]; strings.TrimSpace(v) != "" {
		dsn = v
	}
	return inferStorageBackend(c.storage.Backend, dsn)
}

// backendNamedAnotherStore reports a retired backend variable that names a
// backend other than the one this version opens. The old read let a non-empty
// value win over the yaml, and an unknown one stopped the boot when the store
// was opened.
func backendNamedAnotherStore(value string, c retiredEnvCtx) bool {
	return value != "" && value != backendNow(c)
}

// sqlMemNow is whether this version turns SQL Memory on: the yaml's explicit
// choice, else what the store gives. Like backendNow it applies the env DSN
// itself, applyEnvSQLMem not having run yet.
func sqlMemNow(c retiredEnvCtx) bool {
	if c.storage.SqlMemEnabled != nil {
		return *c.storage.SqlMemEnabled
	}
	dsn := c.storage.SqlMemPgDSN
	if v := c.env["LOOMCYCLE_SQLMEM_PG_DSN"]; v != "" {
		dsn = v
	}
	return inferSQLMemEnabled(backendNow(c), dsn)
}

// sqlMemSwitchDisagrees reports a retired SQL Memory switch whose old effect
// is not what this version does. The old read turned SQL Memory on for exactly
// "1", over the yaml; any other value left the yaml's answer, off unless it
// said true.
func sqlMemSwitchDisagrees(value string, c retiredEnvCtx) bool {
	was := value == "1" || (c.storage.SqlMemEnabled != nil && *c.storage.SqlMemEnabled)
	return was != sqlMemNow(c)
}

// retentionSwitchDisagrees reports a retired retention switch whose old effect
// (the sweeper ran for exactly "1") is not what retention.enabled gives.
func retentionSwitchDisagrees(value string, c retiredEnvCtx) bool {
	return (value == "1") != c.cfg.Retention.Enabled
}

// Provider stream timeout defaults, in milliseconds: every provider's, and
// the local Ollama registration's own. providerbuild applies the same ones.
const (
	ProviderHeaderTimeoutMS    = 60_000
	ProviderIdleTimeoutMS      = 90_000
	OllamaLocalStreamTimeoutMS = 600_000
)

// providerOptionInt is a provider option as the positive whole number the
// runtime applies, or false when the yaml gives none it would apply.
func providerOptionInt(pc ProviderConfig, key string) (int64, bool) {
	var n int64
	switch v := pc.Options[key].(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		n = int64(v)
	default:
		return 0, false
	}
	return n, n > 0
}

// providerOptionAbsent is the rule of a retired variable that set one
// provider's option: a positive number while the yaml does not name the
// option. A yaml option always replaced the variable's value, whatever it
// was (a zero included), so beside one the variable changed nothing and its
// loss changes nothing: that case is reported only. The old reads ignored a
// value that was not a positive number.
func providerOptionAbsent(provider, option string) func(string, retiredEnvCtx) bool {
	return func(value string, c retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		if !set || !ok || n <= 0 {
			return false
		}
		_, named := c.cfg.Providers[provider].Options[option]
		return !named
	}
}

// What memory.embedder.timeout_ms and batch_size are when unset.
const (
	EmbedderTimeoutMSDefault = 30_000
	EmbedderBatchSizeDefault = 100
)

// a2aWasOff reports a retired A2A switch that kept A2A off (the old read
// served it for exactly "1") while the server card that now turns it on is
// named.
func a2aWasOff(value string, c retiredEnvCtx) bool {
	return value != "1" && strings.TrimSpace(c.env["LOOMCYCLE_A2A_SERVER_CARD"]) != ""
}

// audioQueueLowered reports a retired per-route audio queue size under the
// size both queues now get: LOOMCYCLE_AUDIO_MAX_INFLIGHT, read the way
// applyEnvToolLimits reads it, or its default.
func audioQueueLowered(value string, c retiredEnvCtx) bool {
	now := AudioMaxInflightDefault
	if n, err := strconv.Atoi(c.env["LOOMCYCLE_AUDIO_MAX_INFLIGHT"]); err == nil && n > 0 {
		now = n
	}
	return loweredBelow(int64(now))(value, c)
}

func gateWasOff(value string, _ retiredEnvCtx) bool { return value != "1" }

// deadLinkGCNow is the dead-link collection's mode as this version reads it
// (applyEnvDeadLinkGC, which has not run yet): unset is on. A value that is
// none of the three fails the load there; here it counts as on, the mode
// that deletes.
func deadLinkGCNow(c retiredEnvCtx) string {
	switch v := strings.TrimSpace(c.env["LOOMCYCLE_DEADLINK_GC"]); v {
	case DeadLinkGCOff, DeadLinkGCDryRun:
		return v
	}
	return DeadLinkGCOn
}

// deadLinkGCWasOffOrRarer judges the retired interval. Zero or a negative
// number turned the collection off, which only the new "off" reproduces. A
// positive interval longer than the fixed one deleted later than this version
// would, which matters only while it deletes. A shorter one, and a value the
// old read could not parse (it kept the hourly default), change nothing that
// is kept.
func deadLinkGCWasOffOrRarer(value string, c retiredEnvCtx) bool {
	n, set, ok := retiredInt(value)
	if !set || !ok {
		return false
	}
	now := deadLinkGCNow(c)
	if n <= 0 {
		return now != DeadLinkGCOff
	}
	return n > DeadLinkGCInterval.Milliseconds() && now == DeadLinkGCOn
}

// deadLinkGCWasDryRun judges the retired dry-run switch: exactly "1" made the
// collection count without deleting, so this version must not delete.
func deadLinkGCWasDryRun(value string, c retiredEnvCtx) bool {
	return value == "1" && deadLinkGCNow(c) == DeadLinkGCOn
}

// switchWasZero is the rule of a sweeper switch whose old read was `!= "0"`:
// only that exact value had turned the sweeper off.
func switchWasZero(value string, _ retiredEnvCtx) bool { return value == "0" }

// boolWasOff is the rule of a default-on switch read as a boolean word: the
// four "off" spellings turned it off, anything else kept the default.
func boolWasOff(value string, _ retiredEnvCtx) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// internalChatsAgeDiffers reports an internal-chat age other than the 3 days
// now fixed. Zero inherited the chat age, whose own zero (or absence) was "no
// minimum age": internal chats went as soon as they finished. The chat age is
// retention.chats.max_age now, so that is the age a zero is judged against (a
// retired LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS with no retention.chats.max_age
// in the yaml stops the boot under its own rule). A negative number was
// ignored by the old read, which left the 3-day default.
func internalChatsAgeDiffers(value string, c retiredEnvCtx) bool {
	const fixed = 3 * 24 * 60 * 60 * 1000
	n, set, ok := retiredInt(value)
	if !set || !ok {
		return set
	}
	if n == 0 {
		return age(c.cfg.Retention.Chats.MaxAge).Milliseconds() != fixed
	}
	return n > 0 && n != fixed
}

func valueNotEmpty(value string, _ retiredEnvCtx) bool { return value != "" }

// loweredBelow is the rule of a cap now fixed at fixed: a positive value under
// it was a tightening, and so, for all the boot can tell, was a value that
// does not parse.
func loweredBelow(fixed int64) func(string, retiredEnvCtx) bool {
	return func(value string, _ retiredEnvCtx) bool {
		n, set, ok := retiredInt(value)
		return set && (!ok || (n > 0 && n < fixed))
	}
}

// loweredBelowYAML is loweredBelow against the bound the yaml gives.
func loweredBelowYAML(effective func(*StorageConfig) int64) func(string, retiredEnvCtx) bool {
	return func(value string, c retiredEnvCtx) bool {
		return loweredBelow(effective(c.storage))(value, c)
	}
}

// yamlOr is a yaml bound, or its default when the key is absent: for these
// keys zero (and a negative number) takes the default.
func yamlOr[T int | int64](v T, dflt int) int64 {
	if v > 0 {
		return int64(v)
	}
	return int64(dflt)
}

// archiverWasPruning reports the legacy archiver configured to delete chats
// (a delete-bearing mode with a positive age, the condition it ran under)
// while nothing configures the retention sweep that replaced it.
func archiverWasPruning(mode string, c retiredEnvCtx) bool {
	if mode != "prune" && mode != "export+prune" {
		return false
	}
	age, set, ok := retiredInt(c.env["LOOMCYCLE_USAGE_RUN_RETENTION_MS"])
	return set && ok && age > 0 && c.cfg.Retention.Chats.Mode == ""
}

// retiredEnvRefusals returns one "NAME: what to do" entry per retired variable
// in environ (the os.Environ form) whose value the runtime would loosen by
// ignoring, sorted by name. Values are never included, with one exception: a
// retired hooks list names the first entry the yaml lacks, when that entry is
// plainly an owner name, a host or a range (missingEntry).
func retiredEnvRefusals(environ []string, cfg *Config) []string {
	c := retiredEnvCtx{env: make(map[string]string, len(environ)), storage: &cfg.Storage, cfg: cfg}
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok {
			c.env[name] = value
		}
	}
	var out []string
	for name, value := range c.env {
		if moved, ok := retiredEnvMoved[name]; ok {
			if what, refused := moved.refusal(name, value, c); refused {
				out = append(out, name+": "+what)
			}
			continue
		}
		rule, ok := retiredEnvRefuse[name]
		if !ok || !rule.loosens(value, c) {
			continue
		}
		instead := rule.instead
		if build, ok := retiredEnvInsteadFor[name]; ok {
			instead = build(value, c)
		}
		if instead == "" {
			instead = retiredEnv[name]
		}
		out = append(out, name+": "+instead)
	}
	sort.Strings(out)
	return out
}

// retiredInt parses a retired variable's value. set is false for an empty
// value, which every old read treated as unset.
func retiredInt(value string) (n int64, set, ok bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, true, err == nil
}
