package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRetiredEnvWarnings_NamesEachRetiredVariableSetAndNoOther(t *testing.T) {
	got := retiredEnvWarnings([]string{
		"LOOMCYCLE_SNAPSHOT_MAX_BYTES=1048576",
		"LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS=", // an empty value is still a dead line
		"LOOMCYCLE_LISTEN_ADDR=127.0.0.1:8787",   // read today
		"LOOMCYCLE_MY_WEBHOOK_SECRET=hunter2",    // the operator's own name
		"PATH=/usr/bin",
		"malformed",
	})
	if len(got) != 2 {
		t.Fatalf("got %d warnings, want 2: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS is set but no longer read: ") ||
		!strings.HasPrefix(got[1], "LOOMCYCLE_SNAPSHOT_MAX_BYTES is set but no longer read: ") {
		t.Errorf("warnings = %q", got)
	}
	for _, w := range got {
		if strings.Contains(w, "1048576") || strings.Contains(w, "hunter2") {
			t.Errorf("a warning carries a variable's value: %q", w)
		}
	}
}

func TestLoadLayers_WarnsAboutARetiredVariable(t *testing.T) {
	t.Setenv("LOOMCYCLE_SNAPSHOT_MAX_BYTES", "1")
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults:\n  provider: mock\n")})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	for _, w := range cfg.Warnings {
		if strings.HasPrefix(w, "LOOMCYCLE_SNAPSHOT_MAX_BYTES is set but no longer read") {
			return
		}
	}
	t.Errorf("no advisory for the retired variable; warnings = %q", cfg.Warnings)
}

// A name in the retired table that the sources still read is a contradiction:
// the operator would be told a live setting does nothing.
func TestRetiredEnv_NoRetiredNameIsStillRead(t *testing.T) {
	repo := filepath.Join("..", "..")
	found := envNamesInSources(t, filepath.Join(repo, "cmd"), filepath.Join(repo, "internal"))
	for name := range retiredEnv {
		file, read := found[name]
		if !read {
			continue
		}
		// The lists that keep a retired secret name from being interpolated
		// or logged name it on purpose.
		if retiredNameKeptOnPurpose[name] {
			continue
		}
		t.Errorf("%s is in retiredEnv but still appears as a literal in %s", name, file)
	}
}

// retiredNameKeptOnPurpose: retired names that non-test sources other than the
// table still spell out, and why.
var retiredNameKeptOnPurpose = map[string]bool{
	"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS": true, // retired; still on the infrastructure-secret denylists in case a stale value is set
}

// A retired variable whose value asked for something tighter than what now
// applies stops the load; any other value of it is only advised.
func TestRetiredEnvRefusals_RefusesAValueThatAskedForSomethingTighter(t *testing.T) {
	for _, tc := range []struct {
		name, value   string
		also          []string // other variables of the same environment
		yaml          StorageConfig
		retention     RetentionConfig // the yaml's retention block
		providers     map[string]ProviderConfig
		memory        MemoryConfig
		concurrency   Concurrency
		observability ObservabilityConfig
		auth          AuthConfig
		metrics       MetricsConfig
		refused       bool
	}{
		// Gates: the old read turned the feature on for exactly "1".
		{name: "LOOMCYCLE_SCHEDULER_ENABLED", value: "1"},
		{name: "LOOMCYCLE_SCHEDULER_ENABLED", value: "0", refused: true},
		{name: "LOOMCYCLE_WEBHOOKS_ENABLED", value: "false", refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_ENABLED", value: "", refused: true},
		{name: "LOOMCYCLE_CODE_HOOKS_ENABLED", value: "true", refused: true},
		{name: "LOOMCYCLE_CHANNEL_HOOKS", value: "1"},
		{name: "LOOMCYCLE_BASHBOX_ENABLED", value: "0", refused: true},
		{name: "LOOMCYCLE_BASHBOX_ENABLED", value: "1"},
		{name: "LOOMCYCLE_RESUME_FANOUT", value: "off", refused: true},

		// Caps: lowered or unreadable is refused; raised, equal, or the old
		// "no cap" spelling is advised.
		{name: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: "1024", refused: true},
		{name: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: "1073741824"},
		{name: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: "104857600"},
		{name: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: "1k", refused: true},
		{name: "LOOMCYCLE_GIT_PUSH_MAX_BYTES", value: ""},
		{name: "LOOMCYCLE_CRYPTO_MAX_BYTES", value: "4096", refused: true},
		{name: "LOOMCYCLE_CRYPTO_MAX_BYTES", value: "0"}, // was ignored: the default applied
		{name: "LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN", value: "10", refused: true},
		{name: "LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN", value: "500"},
		{name: "LOOMCYCLE_MEMORY_MAX_VALUE_BYTES", value: "1024", refused: true},
		{name: "LOOMCYCLE_MEMORY_MAX_VALUE_BYTES", value: "0"}, // was "no cap"
		{name: "LOOMCYCLE_MEMORY_MAX_VALUE_BYTES", value: "-1"},
		{name: "LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES", value: "512", refused: true},
		{name: "LOOMCYCLE_MAX_DOCUMENT_ASSET_BYTES", value: "1048576", refused: true},
		{name: "LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES", value: "4096", refused: true},
		{name: "LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES", value: "262144"},
		{name: "LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES", value: "many", refused: true},
		{name: "LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES", value: "1024", refused: true},
		{name: "LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES", value: "1024", refused: true},
		{name: "LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES", value: "100", refused: true},
		{name: "LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS", value: "1", refused: true},
		{name: "LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS", value: "0"},                        // was unlimited
		{name: "LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS", value: "0", refused: true}, // was "a request must name its ttl"
		{name: "LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS", value: "172800"},
		{name: "LOOMCYCLE_GIT_SOURCE_MAX_FILES", value: "10", refused: true},
		{name: "LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS", value: "600"},
		{name: "LOOMCYCLE_CLIENT_TOOL_MAX_CONNS", value: "1", refused: true},
		{name: "LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS", value: "60", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "3600000", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "604800000"},

		// The ceiling is gone, so any ceiling was tighter.
		{name: "LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS", value: "60000", refused: true},
		{name: "LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS", value: "0"},

		// SQL Memory: judged against what the yaml (or its default) gives.
		{name: "LOOMCYCLE_SQLMEM_MAX_ROWS", value: "100", refused: true},
		{name: "LOOMCYCLE_SQLMEM_MAX_ROWS", value: "100", yaml: StorageConfig{SqlMemMaxRows: 100}},
		{name: "LOOMCYCLE_SQLMEM_MAX_ROWS", value: "100", yaml: StorageConfig{SqlMemMaxRows: 50}},
		{name: "LOOMCYCLE_SQLMEM_MAX_ROWS", value: "20000"},
		{name: "LOOMCYCLE_SQLMEM_MAX_ROWS", value: "0"}, // was "the default"
		{name: "LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS", value: "1000", refused: true},
		{name: "LOOMCYCLE_SQLMEM_MAX_RESULT_BYTES", value: "1024", refused: true},
		{name: "LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS", value: "soon", refused: true},
		{name: "LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS", value: "4", refused: true},
		{name: "LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT", value: "2", refused: true}, // default: a quarter of 64
		{name: "LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT", value: "16"},               // exactly the default
		{name: "LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT", value: "2", yaml: StorageConfig{SqlMemMaxOpenTxns: 8}},
		{name: "LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH", value: "2", refused: true},
		{name: "LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES", value: "1048576", refused: true}, // the yaml default is no cap
		{name: "LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES", value: "0"},

		// The filesystem jail: always, as before it was retired.
		{name: "LOOMCYCLE_READ_ROOT", value: "/work/sandbox", refused: true},
		{name: "LOOMCYCLE_WRITE_ROOT", value: "/work/sandbox", refused: true},
		{name: "LOOMCYCLE_BASH_CWD", value: "/work/sandbox", refused: true},
		{name: "LOOMCYCLE_READ_ROOT", value: ""},

		// The usage archiver: only when it was deleting chats and nothing
		// replaces it.
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "prune", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000"}, refused: true},
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "export+prune", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000"}, refused: true},
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "prune", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000"}, retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}},
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "prune"},                                                            // no age: it never ran
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "off", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000"}}, // dormant
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MS", value: "86400000"},                                                           // no mode: dormant

		// The store backend: refused when the variable names another backend
		// than the one this version opens. The env value used to win over the
		// yaml, and an unknown one stopped the boot when the store was opened.
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", refused: true}, // no DSN: SQLite would be opened
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "sqlite", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}, refused: true},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "sqlite", yaml: StorageConfig{PgDSN: "postgres://u@h/db"}, refused: true},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", yaml: StorageConfig{PgDSN: "${LOOMCYCLE_PG_DSN}"}, refused: true}, // a reference is not a DSN
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", yaml: StorageConfig{Backend: "sqlite"}, also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}, refused: true},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "Postgres", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}, refused: true},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "sqlite"},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", yaml: StorageConfig{PgDSN: "postgres://u@h/db"}},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "postgres", yaml: StorageConfig{Backend: "postgres"}}, // the yaml pins it
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: "sqlite", yaml: StorageConfig{Backend: "sqlite"}, also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}},
		{name: "LOOMCYCLE_STORAGE_BACKEND", value: ""}, // was ignored

		// The SQL Memory switch: the old read turned it on for exactly "1" and
		// otherwise left the yaml's answer. Refused when that differs from
		// what this version infers from the store.
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "0", refused: true}, // SQLite: now on
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "", refused: true},
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "true", refused: true}, // was not "1": it stayed off
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "0", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db", "LOOMCYCLE_SQLMEM_PG_DSN=postgres://u@h/aux"}, refused: true},
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}, refused: true},                                       // Postgres, no aux DSN: now off
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1", yaml: StorageConfig{PgDSN: "postgres://u@h/db", SqlMemPgDSN: "${LOOMCYCLE_SQLMEM_PG_DSN}"}, refused: true}, // a reference is not a DSN
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1", yaml: StorageConfig{SqlMemEnabled: boolPtr(false)}, refused: true},                                         // the variable used to win over the yaml
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1"},
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db", "LOOMCYCLE_SQLMEM_PG_DSN=postgres://u@h/aux"}},
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "0", also: []string{"LOOMCYCLE_PG_DSN=postgres://u@h/db"}}, // off then, off now
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "0", yaml: StorageConfig{SqlMemEnabled: boolPtr(false)}},
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "0", yaml: StorageConfig{SqlMemEnabled: boolPtr(true)}}, // the yaml turned it on then too
		// On Postgres with no aux DSN an explicit yaml true stops the boot when
		// SQL Memory is opened, as it did; that is not this rule's to refuse.
		{name: "LOOMCYCLE_SQLMEM_ENABLED", value: "1", yaml: StorageConfig{PgDSN: "postgres://u@h/db", SqlMemEnabled: boolPtr(true)}},

		// The retention switch moved to retention.enabled. The old read ran the
		// sweeper for exactly "1"; refused when the yaml says the opposite.
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "1", refused: true}, // the yaml does not turn it on: retention would stop
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "1", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "1", retention: RetentionConfig{Enabled: true}},
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "0", retention: RetentionConfig{Enabled: true}, refused: true}, // held off, and the yaml starts it
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "", retention: RetentionConfig{Enabled: true}, refused: true},
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "true", retention: RetentionConfig{Enabled: true}, refused: true}, // was not "1": it stayed off
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "0"},
		{name: "LOOMCYCLE_RETENTION_ENABLED", value: "0", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}}, // off then, off now

		// A family mode that moved to the yaml: refused when the family would
		// delete under one and not the other, or export under one only.
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", refused: true}, // the yaml leaves it off: chats would be kept
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "export+prune", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "off", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", retention: RetentionConfig{Defs: RetentionDefs{Mode: "prune"}}, refused: true}, // another family
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "off"},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "delete"}, // never a mode the sweeper acted on
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "", retention: RetentionConfig{Chats: RetentionFamily{Mode: "prune"}}},
		{name: "LOOMCYCLE_RETENTION_DEFS_MODE", value: "prune", refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_MODE", value: "prune", retention: RetentionConfig{Defs: RetentionDefs{Mode: "prune"}}},
		{name: "LOOMCYCLE_RETENTION_MEM_MODE", value: "export+prune", refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_MODE", value: "export+prune", retention: RetentionConfig{Mem: RetentionFamily{Mode: "export+prune"}}},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MODE", value: "prune", refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MODE", value: "prune", retention: RetentionConfig{MemContent: RetentionFamily{Mode: "prune"}}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE", value: "prune", refused: true},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE", value: "prune", retention: RetentionConfig{EmptyDossier: RetentionFamily{Mode: "prune"}}},

		// An age that moved to the yaml: refused when it was a positive number
		// the yaml does not give. The old reads ignored anything else.
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(30 * 24 * time.Hour))}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(90 * 24 * time.Hour))}}},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "0"},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "-5"},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "90d"}, // was ignored
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: ""},
		{name: "LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS", value: "86400000", retention: RetentionConfig{Defs: RetentionDefs{MaxAge: agePtr(HumanDuration(24 * time.Hour))}}},
		{name: "LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS", value: "86400000", retention: RetentionConfig{Mem: RetentionFamily{MaxAge: agePtr(HumanDuration(24 * time.Hour))}}},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS", value: "86400000", retention: RetentionConfig{MemContent: RetentionFamily{MaxAge: agePtr(HumanDuration(24 * time.Hour))}}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS", value: "86400000", retention: RetentionConfig{EmptyDossier: RetentionFamily{MaxAge: agePtr(HumanDuration(24 * time.Hour))}}},
		// These two default to 30 days.
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "2592000000"},
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "86400000", retention: RetentionConfig{UsageDetail: RetentionAge{MaxAge: HumanDuration(24 * time.Hour)}}},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "2592000000"},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", retention: RetentionConfig{Trace: RetentionAge{MaxAge: HumanDuration(24 * time.Hour)}}},

		// Versions kept: any number from zero up was a setting; the default is 5.
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "0", refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "20", refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "5"},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "0", retention: RetentionConfig{Defs: RetentionDefs{KeepLastN: intPtr(0)}}},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "5", retention: RetentionConfig{Defs: RetentionDefs{KeepLastN: intPtr(0)}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "-1"}, // was ignored
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "many"},

		// The export directory: refused unless the yaml names the same one.
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports", refused: true},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports", retention: RetentionConfig{ExportDir: "/srv/other"}, refused: true},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports", retention: RetentionConfig{ExportDir: "/data/exports"}},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: ""},

		// The sweep cadence deletes nothing sooner or later than the ages say.
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "1000"},

		// The A2A switch: the old read served A2A for exactly "1". Refused
		// when it was off and the server card that now turns A2A on is named.
		{name: "LOOMCYCLE_A2A_ENABLED", value: "0", also: []string{"LOOMCYCLE_A2A_SERVER_CARD=public"}, refused: true},
		{name: "LOOMCYCLE_A2A_ENABLED", value: "", also: []string{"LOOMCYCLE_A2A_SERVER_CARD=public"}, refused: true},
		{name: "LOOMCYCLE_A2A_ENABLED", value: "0"},
		{name: "LOOMCYCLE_A2A_ENABLED", value: "0", also: []string{"LOOMCYCLE_A2A_SERVER_CARD=  "}}, // a blank card names nothing
		{name: "LOOMCYCLE_A2A_ENABLED", value: "1", also: []string{"LOOMCYCLE_A2A_SERVER_CARD=public"}},
		{name: "LOOMCYCLE_A2A_ENABLED", value: "1"}, // was on with no card to serve; now off

		// The two audio queue sizes: refused when positive and under the size
		// both queues now get, or unreadable. Zero and a negative number took
		// the default.
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "1", refused: true}, // the default is 2
		{name: "LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT", value: "1", refused: true},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "3", also: []string{"LOOMCYCLE_AUDIO_MAX_INFLIGHT=5"}, refused: true},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "1", also: []string{"LOOMCYCLE_AUDIO_MAX_INFLIGHT=0"}, refused: true}, // zero keeps the default
		{name: "LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT", value: "one", refused: true},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "2"},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "7"},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "0"},
		{name: "LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT", value: "-1"},
		{name: "LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT", value: ""},
		{name: "LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT", value: "1", also: []string{"LOOMCYCLE_AUDIO_MAX_INFLIGHT=1"}},
		{name: "LOOMCYCLE_SPEECH_MAX_INFLIGHT", value: "5", also: []string{"LOOMCYCLE_AUDIO_MAX_INFLIGHT=5"}},

		// The global provider timeouts moved to each provider's options.
		// Refused when lowered below the default and a provider that makes
		// HTTP calls would now wait longer than the variable allowed.
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{
			"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": 5000}},
			"gemini": {Driver: "gemini"},
		}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{
			"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": 5000}},
			"gemini": {Driver: "gemini", Options: map[string]any{"header_timeout_ms": 3000}},
			// These never took the global timeout or make no HTTP call.
			"ollama-local": {Driver: "ollama"}, "code-js": {Driver: "code-js"}, "mock": {Driver: "mock"},
		}},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": "5000"}}}, refused: true}, // a string is not applied
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "60000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                     // the default
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "120000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                    // raised: nothing waits longer
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "0", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                         // was ignored
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "fast", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                      // was ignored
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000"},                                                                                                                                          // no provider to hold to it
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "30000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "30000", providers: map[string]ProviderConfig{"openai": {Driver: "openai", Options: map[string]any{"idle_timeout_ms": 30000}}}},
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "90000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},
		// The local Ollama pair: judged against that one provider, default 600s.
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}, refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"header_timeout_ms": 300000}}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "900000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}}, // no local Ollama declared
		{name: "LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}, refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS", value: "600000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}},

		// The code-agent bounds moved to providers.code-js.options. Refused
		// when lowered below what the yaml gives (120s and 24h unset).
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", providers: map[string]ProviderConfig{"code-js": {Driver: "code-js", Options: map[string]any{"run_timeout_seconds": 30}}}},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", providers: map[string]ProviderConfig{"code-js": {Driver: "code-js", Options: map[string]any{"run_timeout_seconds": 60}}}, refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "120"},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "600"},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "0"},    // was ignored
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "5min"}, // was ignored
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "3600", refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "3600", providers: map[string]ProviderConfig{"code-js": {Driver: "code-js", Options: map[string]any{"max_wall_seconds": 3600}}}},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "86400"},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "604800"},

		// Provider options whose loss tightens nothing: reported only.
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "1"},
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768"},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "99"},

		// Timeout scaling measures and changes no timeout, so no value of its
		// variables tightened anything.
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "off"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "50"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER", value: "2"},

		// The embedder call timeout moved to memory.embedder.timeout_ms
		// (30000 unset). Refused when lowered below what the yaml gives; zero
		// meant "no timeout", which was looser.
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5000", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5000", memory: MemoryConfig{Embedder: EmbedderConfig{TimeoutMs: 5000}}},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5000", memory: MemoryConfig{Embedder: EmbedderConfig{TimeoutMs: 20000}}, refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "30000"},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "120000"},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "0"},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5s"}, // was ignored
		// The batch size and the projection cadence tightened nothing.
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "8"},
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "1h"},

		// The concurrency bounds moved to the yaml `concurrency:` block. Each is
		// refused when it held a caller to less than the yaml now allows.
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", refused: true}, // the yaml has no per-user cap
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", concurrency: Concurrency{MaxConcurrentRunsPerUser: 4}, refused: true},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", concurrency: Concurrency{MaxConcurrentRunsPerUser: 2}},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "8", concurrency: Concurrency{MaxConcurrentRunsPerUser: 2}},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "0", concurrency: Concurrency{MaxConcurrentRunsPerUser: 2}}, // was "no cap"
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "-3"},                                                       // was "no cap"
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "two"},                                                      // was ignored
		// The loader seeds max_queued_runs_per_user with 16; zero is "refuse at once".
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "4", concurrency: Concurrency{MaxQueuedRunsPerUser: 16}, refused: true},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "0", concurrency: Concurrency{MaxQueuedRunsPerUser: 16}, refused: true},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "4", concurrency: Concurrency{MaxQueuedRunsPerUser: 4}},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "16", concurrency: Concurrency{MaxQueuedRunsPerUser: 16}},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "64", concurrency: Concurrency{MaxQueuedRunsPerUser: 16}},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "-1", concurrency: Concurrency{MaxQueuedRunsPerUser: 16}}, // was ignored
		// Zero inherited max_queue_depth, as an absent yaml key does.
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "4", concurrency: Concurrency{MaxQueueDepth: 32}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "4", concurrency: Concurrency{MaxQueueDepth: 32, ProviderQueueDepth: 8}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "4", concurrency: Concurrency{MaxQueueDepth: 32, ProviderQueueDepth: 4}},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "64", concurrency: Concurrency{MaxQueueDepth: 32}},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "0", concurrency: Concurrency{MaxQueueDepth: 32}},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "32", concurrency: Concurrency{MaxQueueDepth: 32}},

		// The per-tenant sampler ratios: refused unless the yaml gives every
		// tenant the variable named the same ratio.
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme=1,beta=0.1", refused: true},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme=1,beta=0.1", observability: ObservabilityConfig{Tracing: TracingConfig{TenantSamplerRatios: map[string]float64{"acme": 1}}}, refused: true},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme=1,beta=0.1", observability: ObservabilityConfig{Tracing: TracingConfig{TenantSamplerRatios: map[string]float64{"acme": 1, "beta": 0.5}}}, refused: true},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: " acme=1, beta=0.1 ,", observability: ObservabilityConfig{Tracing: TracingConfig{TenantSamplerRatios: map[string]float64{"acme": 1, "beta": 0.1, "gamma": 0}}}},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme", refused: true}, // stopped the boot before, too
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: ""},
		// The tenant telemetry host list: with none, a tenant may name any
		// host, so a list the yaml does not carry is refused.
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", refused: true},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", observability: ObservabilityConfig{TenantTelemetry: TenantTelemetryConfig{AllowedHosts: []string{"otel.example.com"}}}},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", observability: ObservabilityConfig{TenantTelemetry: TenantTelemetryConfig{AllowedHosts: []string{"*.grafana.net"}}}}, // the operator wrote a list
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: " , "},                                                                                                                                   // named no host
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: ""},

		// The login page's handoff origins moved to auth.login_origins. Refused
		// when the yaml does not list an origin the variable did: the landing
		// page's sign-in would stop working.
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: "https://landing.example", refused: true},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: "https://landing.example, https://b.example/", auth: AuthConfig{LoginOrigins: []string{"https://landing.example"}}, refused: true},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: "https://landing.example/, ", auth: AuthConfig{LoginOrigins: []string{"https://b.example", " https://landing.example/ "}}},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: " , "},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: ""},

		// The metrics sampler moved to the yaml `metrics:` block. Losing it
		// stops samples being recorded and tightens nothing.
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "1"},
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "0", metrics: MetricsConfig{Enabled: true}},
		{name: "LOOMCYCLE_METRICS_COLLECT_SYSTEM", value: "1"},
		{name: "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", value: "1000"},

		// The debug flags only added log lines.
		{name: "LOOMCYCLE_AUTH_VERBOSE", value: "1"},
		{name: "LOOMCYCLE_CHANNEL_DEBUG", value: "0"},
		{name: "LOOMCYCLE_OLLAMA_DEBUG_THINK", value: "1"},

		// Retired, and no value of it was a tightening.
		{name: "LOOMCYCLE_SSE_KEEPALIVE_MS", value: "0"},
		{name: "LOOMCYCLE_SNAPSHOT_MAX_BYTES", value: "1"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			environ := append([]string{tc.name + "=" + tc.value}, tc.also...)
			got := retiredEnvRefusals(environ, &Config{Storage: tc.yaml, Retention: tc.retention, Providers: tc.providers, Memory: tc.memory, Concurrency: tc.concurrency, Observability: tc.observability, Auth: tc.auth, Metrics: tc.metrics})
			if refused := len(got) > 0; refused != tc.refused {
				t.Fatalf("refused = %v (%q), want %v", refused, got, tc.refused)
			}
			if tc.refused && !strings.HasPrefix(got[0], tc.name+": ") {
				t.Errorf("refusal does not lead with the variable: %q", got[0])
			}
			if adv := retiredEnvWarnings(environ); len(adv) == 0 {
				t.Errorf("no advisory for %s", tc.name)
			}
		})
	}
}

// Every name with a refusal rule is a retired name: a rule on a variable
// that is read would stop the boot of a deployment that sets it correctly.
func TestRetiredEnvRefusals_EveryRuleIsOnARetiredName(t *testing.T) {
	for name := range retiredEnvRefuse {
		if _, ok := retiredEnv[name]; !ok {
			t.Errorf("%s has a refusal rule but is not in retiredEnv", name)
		}
	}
}

func TestLoadLayers_RefusesARetiredVariableThatAskedForSomethingTighter(t *testing.T) {
	t.Setenv("LOOMCYCLE_SCHEDULER_ENABLED", "0")
	t.Setenv("LOOMCYCLE_GIT_PUSH_MAX_BYTES", "4242424")
	t.Setenv("LOOMCYCLE_READ_ROOT", "/work/sandbox-zz")
	t.Setenv("LOOMCYCLE_SQLMEM_MAX_ROWS", "77")
	_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults:\n  provider: mock\n")})
	if err == nil {
		t.Fatal("LoadLayers accepted retired variables that asked for something tighter")
	}
	msg := err.Error()
	for _, want := range []string{
		"LOOMCYCLE_SCHEDULER_ENABLED", "LOOMCYCLE_GIT_PUSH_MAX_BYTES", "LOOMCYCLE_READ_ROOT", "volumes:",
		"LOOMCYCLE_SQLMEM_MAX_ROWS", "storage.sqlmem_max_rows",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %q: %s", want, msg)
		}
	}
	for _, value := range []string{"4242424", "sandbox-zz", "77"} {
		if strings.Contains(msg, value) {
			t.Errorf("error carries a variable's value %q: %s", value, msg)
		}
	}

	// The yaml already asks for the same bound: nothing is loosened.
	t.Setenv("LOOMCYCLE_SCHEDULER_ENABLED", "1")
	t.Setenv("LOOMCYCLE_GIT_PUSH_MAX_BYTES", "")
	t.Setenv("LOOMCYCLE_READ_ROOT", "")
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults:\n  provider: mock\nstorage:\n  sqlmem_max_rows: 77\n")})
	if err != nil {
		t.Fatalf("LoadLayers with the yaml carrying the bound: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Error("the retired variables are no longer advised")
	}
}

// The thin client's own retired variables reach a command that loads no
// config, by name and without their values.
func TestRetiredEnvAdvisories_NamesTheThinClientsRetiredVariables(t *testing.T) {
	got := RetiredEnvAdvisories([]string{
		"LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS=31337",
		"LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS=9",
		"LOOMCYCLE_MCP_UPSTREAM_TOKEN=hunter2", // read today
		"LOOMCYCLE_MCP_UPSTREAM=https://runtime.example",
	})
	if len(got) != 2 ||
		!strings.HasPrefix(got[0], "LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS is set but no longer read: ") ||
		!strings.HasPrefix(got[1], "LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS is set but no longer read: ") {
		t.Fatalf("advisories = %q, want one for each retired thin-client variable", got)
	}
	for _, w := range got {
		if strings.Contains(w, "31337") || strings.Contains(w, "hunter2") {
			t.Errorf("an advisory carries a variable's value: %q", w)
		}
	}
}

// The load checks every family's mode and age from one list. A family added
// to the block and not to that list would take any mode, and prune with no
// age, unchecked.
func TestRetentionFamilies_ListsEveryFamilyOfTheBlock(t *testing.T) {
	rt := reflect.TypeOf(RetentionConfig{})
	want := 0
	for i := 0; i < rt.NumField(); i++ {
		ft := rt.Field(i).Type
		if ft.Kind() != reflect.Struct {
			continue
		}
		if _, ok := ft.FieldByName("Mode"); ok {
			want++
		}
	}
	if want == 0 {
		t.Fatal("found no family with a mode in RetentionConfig")
	}
	if got := len((&RetentionConfig{}).families()); got != want {
		t.Errorf("families() lists %d families, RetentionConfig has %d with a mode", got, want)
	}
}

func agePtr(d HumanDuration) *HumanDuration { return &d }
