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
	// The retention sweeper ran: the family variables had an effect.
	ran := []string{"LOOMCYCLE_RETENTION_ENABLED=1"}
	codeJS := func(options map[string]any) map[string]ProviderConfig {
		return map[string]ProviderConfig{"code-js": {Driver: "code-js", Options: options}}
	}
	for _, tc := range []struct {
		name, value string
		also        []string // other variables of the same environment
		yaml        StorageConfig
		retention   RetentionConfig // the yaml's retention block
		providers   map[string]ProviderConfig
		concurrency Concurrency
		named       []string // yaml keys the config names
		refused     bool
		says        string // text the refusal must carry
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

		// The internal-chat age is fixed at 3 days: any other age changes when
		// those chats are deleted, in either direction. Zero inherited the
		// chat age (retention.chats.max_age now), and an unset chat age was
		// "no minimum".
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "3600000", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "604800000", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "259200000"},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "-1"}, // was ignored: the 3-day default applied
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "soon", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "0", refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "0", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(7 * 24 * time.Hour))}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "0", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(time.Hour))}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "0", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(0))}}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS", value: "0", retention: RetentionConfig{Chats: RetentionFamily{MaxAge: agePtr(HumanDuration(3 * 24 * time.Hour))}}},

		// Switches of work that changes what is kept: the old "off" is refused,
		// every other value left the work running.
		{name: "LOOMCYCLE_USAGE_SWEEPER", value: "0", refused: true},
		{name: "LOOMCYCLE_USAGE_SWEEPER", value: "1"},
		{name: "LOOMCYCLE_USAGE_SWEEPER", value: "off"}, // the old read turned it off for exactly "0"
		{name: "LOOMCYCLE_USAGE_SWEEPER", value: ""},
		{name: "LOOMCYCLE_HEARTBEAT_SWEEPER", value: "0", refused: true},
		{name: "LOOMCYCLE_HEARTBEAT_SWEEPER", value: "1"},
		{name: "LOOMCYCLE_HEARTBEAT_SWEEPER", value: "false"},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: "0", refused: true},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: "false", refused: true},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: " No ", refused: true},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: "OFF", refused: true},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: "1"},
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: "maybe"}, // was ignored: the default (on) applied
		{name: "LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS", value: ""},

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

		// A setting that moved to a yaml key is refused when the variable gave
		// something other than the default and the yaml does not name the key.
		// It is only advised when it gave the default, or when the yaml names
		// the key with any value: the operator has written what they want.
		//
		// The retention family variables did nothing unless the sweeper ran,
		// which took LOOMCYCLE_RETENTION_ENABLED=1.
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "export+prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", also: ran, named: []string{"retention.chats.mode"}},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", also: ran, named: []string{"retention.defs.mode"}, refused: true}, // another family
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune"},                                                                   // retention never ran
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "prune", also: []string{"LOOMCYCLE_RETENTION_ENABLED=true"}},               // nor here: the switch was exactly "1"
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "off", also: ran},
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "delete", also: ran}, // never a mode the sweeper acted on
		{name: "LOOMCYCLE_RETENTION_CHATS_MODE", value: "", also: ran},
		{name: "LOOMCYCLE_RETENTION_DEFS_MODE", value: "prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_MODE", value: "prune", also: ran, named: []string{"retention.defs.mode"}},
		{name: "LOOMCYCLE_RETENTION_MEM_MODE", value: "export+prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_MODE", value: "export+prune", also: ran, named: []string{"retention.mem.mode"}},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MODE", value: "prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MODE", value: "export+prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MODE", value: "prune", also: ran, named: []string{"retention.mem_content.mode"}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE", value: "prune", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE", value: "prune", also: ran, named: []string{"retention.empty_dossier.mode"}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE", value: "prune"},

		// An age: a positive number was a setting, and the default was none.
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", also: ran, named: []string{"retention.chats.max_age"}},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000", also: ran, named: []string{"retention.chats.mode"}, refused: true},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "7776000000"}, // retention never ran
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "0", also: ran},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "-5", also: ran},
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "90d", also: ran}, // was ignored
		{name: "LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", value: "", also: ran},
		{name: "LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS", value: "86400000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS", value: "86400000", also: ran, named: []string{"retention.defs.max_age"}},
		{name: "LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS", value: "86400000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS", value: "86400000", also: ran, named: []string{"retention.mem.max_age"}},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS", value: "86400000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS", value: "86400000", also: ran, named: []string{"retention.mem_content.max_age"}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS", value: "86400000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS", value: "86400000", also: ran, named: []string{"retention.empty_dossier.max_age"}},
		{name: "LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS", value: "86400000"},

		// The usage rollup always runs, so its age needs no switch. 30 days is
		// the default.
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "86400000", refused: true},
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "7776000000", refused: true}, // kept longer: also a setting
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "2592000000"},
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "86400000", named: []string{"retention.usage_detail.max_age"}},
		{name: "LOOMCYCLE_USAGE_DETAIL_RETENTION_MS", value: "0"},
		// The trace age did nothing unless the trace index was on.
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", also: []string{"LOOMCYCLE_MEMORY_TRACE_INDEX=1"}, refused: true},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", also: []string{"LOOMCYCLE_MEMORY_TRACE_INDEX=TRUE"}, refused: true},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000"},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", also: []string{"LOOMCYCLE_MEMORY_TRACE_INDEX=0"}},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "2592000000", also: []string{"LOOMCYCLE_MEMORY_TRACE_INDEX=1"}},
		{name: "LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS", value: "86400000", also: []string{"LOOMCYCLE_MEMORY_TRACE_INDEX=1"}, named: []string{"retention.trace.max_age"}},

		// Versions kept: any number from zero up was a setting; the default is 5.
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "0", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "20", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "5", also: ran},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "0", also: ran, named: []string{"retention.defs.keep_last_n"}},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "0"},
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "-1", also: ran}, // was ignored
		{name: "LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N", value: "many", also: ran},

		// The export directory and the sweep cadence.
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports", also: ran, named: []string{"retention.export_dir"}},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "/data/exports"},
		{name: "LOOMCYCLE_RETENTION_EXPORT_DIR", value: "", also: ran},
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "86400000", also: ran, refused: true},
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "86400000", also: ran, named: []string{"retention.interval"}},
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "3600000", also: ran}, // the default hour
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "1000"},
		{name: "LOOMCYCLE_RETENTION_INTERVAL_MS", value: "hourly", also: ran}, // was ignored

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

		// The global provider timeouts moved to each provider's options, raised
		// as well as lowered. They are judged over the providers this
		// deployment can use: declared, with their key variable set (or none
		// to set), and not ollama-local, code-js or a mock, which never took
		// the global pair. The yaml names the option when it gives a positive
		// number, the only form the runtime applies.
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true, says: "openai"},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "120000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true, says: "openai"}, // raised
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{
			"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": 5000}},
			"gemini": {Driver: "gemini"},
		}, refused: true, says: "gemini"},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{
			"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": 5000}},
			"gemini": {Driver: "gemini", Options: map[string]any{"header_timeout_ms": 90000}}, // any value: the operator chose it
			// These never took the global timeout or make no HTTP call.
			"ollama-local": {Driver: "ollama"}, "code-js": {Driver: "code-js"}, "mock": {Driver: "mock"},
		}},
		// A keyed provider whose key variable is not set is not registered, so
		// nothing calls it and its option is not asked for.
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", also: []string{"OPENAI_API_KEY=k"}, providers: map[string]ProviderConfig{
			"openai":    {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY", Options: map[string]any{"header_timeout_ms": 5000}},
			"anthropic": {Driver: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
			"deepseek":  {Driver: "deepseek", APIKeyEnv: "DEEPSEEK_API_KEY"},
		}},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", also: []string{"OPENAI_API_KEY=k", "ANTHROPIC_API_KEY=k"}, providers: map[string]ProviderConfig{
			"openai":    {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY", Options: map[string]any{"header_timeout_ms": 5000}},
			"anthropic": {Driver: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
			"deepseek":  {Driver: "deepseek", APIKeyEnv: "DEEPSEEK_API_KEY"},
		}, refused: true, says: "these providers: anthropic ("},
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000", providers: map[string]ProviderConfig{"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": "5000"}}}, refused: true}, // a string is not applied
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "60000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                     // the default
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "0", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                         // was ignored
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "fast", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                                      // was ignored
		{name: "LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS", value: "5000"},                                                                                                                                          // no provider to hold to it
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "30000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}, refused: true},                                                       // raised
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"openai": {Driver: "openai", Options: map[string]any{"header_timeout_ms": 300000}}}, refused: true}, // the other option
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "30000", providers: map[string]ProviderConfig{"openai": {Driver: "openai", Options: map[string]any{"idle_timeout_ms": 30000}}}},
		{name: "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", value: "90000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},
		// The local Ollama pair: that one provider, default 600s, and only
		// while it is declared and not switched off.
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}, refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "900000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}, refused: true}, // raised
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "900000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"header_timeout_ms": 300000}}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "900000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"header_timeout_ms": 0}}}, refused: true}, // a zero is not applied
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "600000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"openai": {Driver: "openai"}}},                                                   // no local Ollama declared
		{name: "LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS", value: "300000", also: []string{"OLLAMA_BASE_URL=disabled"}, providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}}, // switched off
		{name: "LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS", value: "300000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}, refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS", value: "600000", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama"}}},

		// The code-agent options moved to providers.code-js.options (120s, 24h
		// and false unset), and did nothing with no code-js provider declared.
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", providers: codeJS(nil), refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "600", providers: codeJS(nil), refused: true}, // raised
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", providers: codeJS(map[string]any{"run_timeout_seconds": 60})},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30", providers: codeJS(map[string]any{"max_wall_seconds": 60}), refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "30"}, // no code-js provider
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "120", providers: codeJS(nil)},
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "0", providers: codeJS(nil)},    // was ignored
		{name: "LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS", value: "5min", providers: codeJS(nil)}, // was ignored
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "3600", providers: codeJS(nil), refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "604800", providers: codeJS(nil), refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "3600", providers: codeJS(map[string]any{"max_wall_seconds": 3600})},
		{name: "LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS", value: "86400", providers: codeJS(nil)},
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "1", providers: codeJS(nil), refused: true},
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "1", providers: codeJS(map[string]any{"deterministic": false})},
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "1"},
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "0", providers: codeJS(nil)},
		{name: "LOOMCYCLE_CODE_AGENTS_DETERMINISTIC", value: "true", providers: codeJS(nil)}, // the old read was on for exactly "1"

		// The hosted Ollama context window and the local GPU offload moved to
		// provider options. Refused when the variable was a positive number
		// and the yaml does not name the option: the window would shrink to
		// the model's own, or the offload fall back to the CPU, with nothing
		// but slower or truncated answers to show for it.
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", refused: true},
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", providers: map[string]ProviderConfig{"ollama": {Driver: "ollama"}}, refused: true},
		// A yaml option always won over the variable, so the variable beside
		// another yaml value changed nothing before and changes nothing now. A yaml zero won too.
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", providers: map[string]ProviderConfig{"ollama": {Driver: "ollama", Options: map[string]any{"num_ctx": 8192}}}},
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", providers: map[string]ProviderConfig{"ollama": {Driver: "ollama", Options: map[string]any{"num_ctx": 0}}}},
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"num_ctx": 32768}}}, refused: true}, // the other provider's option
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32768", providers: map[string]ProviderConfig{"ollama": {Driver: "ollama", Options: map[string]any{"num_ctx": 32768}}}},
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "0"},   // was ignored
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "-1"},  // was ignored
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: "32k"}, // was ignored
		{name: "LOOMCYCLE_OLLAMA_NUM_CTX", value: ""},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "99", refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "99", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"num_ctx": 32768}}}, refused: true},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "99", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"num_gpu": 20}}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "99", providers: map[string]ProviderConfig{"ollama-local": {Driver: "ollama", Options: map[string]any{"num_gpu": 99}}}},
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "0"},   // was ignored: Ollama decided
		{name: "LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU", value: "all"}, // was ignored

		// Timeout scaling: measure is the default, so off is a setting.
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "off", refused: true},
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: " OFF ", refused: true},
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "on", refused: true}, // stopped the boot before, too
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "off", named: []string{"timeout_scaling.mode"}},
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "measure"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: "Measure"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING", value: ""},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "50", refused: true},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "50", named: []string{"timeout_scaling.reference.decode_tps"}},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "100"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "100.0"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "fast"}, // was ignored, with a warning
		{name: "LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS", value: "0"},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER", value: "2", refused: true},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER", value: "2", named: []string{"timeout_scaling.max_multiplier"}},
		{name: "LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER", value: "8"},

		// The embedder call timeout and batch size moved to memory.embedder
		// (30000 and 100 unset). Any whole number was read; zero and below
		// meant "no timeout" and "one text per call".
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5000", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "120000", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "0", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5000", named: []string{"memory.embedder.timeout_ms"}},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "30000"},
		{name: "LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS", value: "5s"}, // was ignored
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "8", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "0", refused: true},
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "8", named: []string{"memory.embedder.batch_size"}},
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "100"},
		{name: "LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE", value: "many"},
		// A positive projection interval started the sweeper (and turned the
		// projection on); none, or zero, is the default.
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "1h", refused: true},
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "hourly", refused: true}, // stopped the boot before, too
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "1h", named: []string{"memory.projection.interval", "memory.projection.enabled"}},
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "1h", named: []string{"memory.projection.interval"}, refused: true, says: "memory.projection.enabled"}, // a cadence for a projection left off
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: "0"},
		{name: "LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", value: " "},

		// The concurrency bounds moved to the yaml `concurrency:` block. A
		// per-user cap of zero (or below) is "no cap", the default.
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", refused: true},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", named: []string{"concurrency.max_concurrent_runs_per_user"}},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "2", named: []string{"concurrency.max_concurrent_runs"}, refused: true}, // another key
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "0"},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "-3"},
		{name: "LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", value: "two"}, // was ignored
		// The loader seeds max_queued_runs_per_user with 16; zero is "refuse at once".
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "4", refused: true},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "0", refused: true},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "64", refused: true},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "4", named: []string{"concurrency.max_queued_runs_per_user"}},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "16"},
		{name: "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", value: "-1"}, // was ignored
		// Zero inherited max_queue_depth, as an absent yaml key does, so that
		// depth is the default.
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "4", concurrency: Concurrency{MaxQueueDepth: 32}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "64", concurrency: Concurrency{MaxQueueDepth: 32}, refused: true},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "4", concurrency: Concurrency{MaxQueueDepth: 32, ProviderQueueDepth: 8}, named: []string{"concurrency.provider_queue_depth"}},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "0", concurrency: Concurrency{MaxQueueDepth: 32}},
		{name: "LOOMCYCLE_PROVIDER_QUEUE_DEPTH", value: "32", concurrency: Concurrency{MaxQueueDepth: 32}},

		// The per-tenant sampler ratios: any pair was a setting.
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme=1,beta=0.1", refused: true},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme", refused: true}, // stopped the boot before, too
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: "acme=1,beta=0.1", named: []string{"observability.tracing.tenant_sampler_ratios"}},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: " , "},
		{name: "LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS", value: ""},
		// The tenant telemetry host list did nothing unless tenant telemetry
		// was on.
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", also: []string{"LOOMCYCLE_TENANT_TELEMETRY=1"}, refused: true},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", also: []string{"LOOMCYCLE_TENANT_TELEMETRY= true "}, refused: true},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", also: []string{"LOOMCYCLE_TENANT_TELEMETRY=1"}, named: []string{"observability.tenant_telemetry.allowed_hosts"}},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com"},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "otel.example.com", also: []string{"LOOMCYCLE_TENANT_TELEMETRY=0"}},
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: " , ", also: []string{"LOOMCYCLE_TENANT_TELEMETRY=1"}}, // named no host
		{name: "LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS", value: "", also: []string{"LOOMCYCLE_TENANT_TELEMETRY=1"}},

		// The login page's handoff origins moved to auth.login_origins.
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: "https://landing.example", refused: true},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: "https://landing.example", named: []string{"auth.login_origins"}},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: " , "},
		{name: "LOOMCYCLE_UI_LOGIN_ORIGINS", value: ""},

		// The metrics sampler moved to the yaml `metrics:` block. Its two
		// tunings did nothing unless the sampler was on.
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "1", refused: true},
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "1", named: []string{"metrics.enabled"}},
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "0"},
		{name: "LOOMCYCLE_METRICS_ENABLED", value: "true"}, // the old read was on for exactly "1"
		{name: "LOOMCYCLE_METRICS_COLLECT_SYSTEM", value: "1", also: []string{"LOOMCYCLE_METRICS_ENABLED=1"}, refused: true},
		{name: "LOOMCYCLE_METRICS_COLLECT_SYSTEM", value: "1", also: []string{"LOOMCYCLE_METRICS_ENABLED=1"}, named: []string{"metrics.collect_system"}},
		{name: "LOOMCYCLE_METRICS_COLLECT_SYSTEM", value: "1"},
		{name: "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", value: "1000", also: []string{"LOOMCYCLE_METRICS_ENABLED=1"}, refused: true},
		{name: "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", value: "1000", also: []string{"LOOMCYCLE_METRICS_ENABLED=1"}, named: []string{"metrics.sample_interval"}},
		{name: "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", value: "5000", also: []string{"LOOMCYCLE_METRICS_ENABLED=1"}},
		{name: "LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS", value: "1000"},

		// The debug flags only added log lines.
		{name: "LOOMCYCLE_AUTH_VERBOSE", value: "1"},
		{name: "LOOMCYCLE_CHANNEL_DEBUG", value: "0"},
		{name: "LOOMCYCLE_OLLAMA_DEBUG_THINK", value: "1"},

		// The dead-link collection deletes data. Its old "off" and "dry run"
		// are refused unless the variable that replaced both says the same or
		// less; an interval longer than the fixed hour is refused while the
		// collection deletes, a shorter one never.
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "0", refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "-1", refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "0", also: []string{"LOOMCYCLE_DEADLINK_GC=dry-run"}, refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "0", also: []string{"LOOMCYCLE_DEADLINK_GC=on"}, refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "0", also: []string{"LOOMCYCLE_DEADLINK_GC=off"}},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "0", also: []string{"LOOMCYCLE_DEADLINK_GC= off "}},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "3600000"},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "600000"},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "7200000", refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "7200000", also: []string{"LOOMCYCLE_DEADLINK_GC=on"}, refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "7200000", also: []string{"LOOMCYCLE_DEADLINK_GC=off"}},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "7200000", also: []string{"LOOMCYCLE_DEADLINK_GC=dry-run"}},
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: "hourly"}, // was ignored: the default hour applied
		{name: "LOOMCYCLE_DEADLINK_GC_MS", value: ""},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "1", refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "1", also: []string{"LOOMCYCLE_DEADLINK_GC=on"}, refused: true},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "1", also: []string{"LOOMCYCLE_DEADLINK_GC=dry-run"}},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "1", also: []string{"LOOMCYCLE_DEADLINK_GC=off"}},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "0"},
		{name: "LOOMCYCLE_DEADLINK_GC_DRY_RUN", value: "true"}, // the old read was a dry run for exactly "1"

		// Retired, and no value of it was a tightening.
		{name: "LOOMCYCLE_SSE_KEEPALIVE_MS", value: "0"},
		{name: "LOOMCYCLE_SNAPSHOT_MAX_BYTES", value: "1"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			environ := append([]string{tc.name + "=" + tc.value}, tc.also...)
			cfg := &Config{Storage: tc.yaml, Retention: tc.retention, Providers: tc.providers, Concurrency: tc.concurrency, yamlKeys: map[string]bool{}}
			for _, key := range tc.named {
				cfg.yamlKeys[key] = true
			}
			// The entry of the variable under test: a companion variable in
			// `also` may be refused on its own account.
			entry := ""
			for _, e := range retiredEnvRefusals(environ, cfg) {
				if strings.HasPrefix(e, tc.name+": ") {
					entry = e
				}
			}
			if refused := entry != ""; refused != tc.refused {
				t.Fatalf("refused = %v (%q), want %v", refused, entry, tc.refused)
			}
			if !strings.Contains(entry, tc.says) {
				t.Errorf("refusal does not say %q: %q", tc.says, entry)
			}
			if tc.refused && tc.name == "LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS" && !strings.Contains(entry, "kept 3 days") {
				t.Errorf("refusal does not say how long internal chats are kept: %q", entry)
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
	for name := range retiredEnvMoved {
		if _, ok := retiredEnv[name]; !ok {
			t.Errorf("%s has a moved-setting rule but is not in retiredEnv", name)
		}
	}
}

// A variable whose advisory says to set a yaml key is a setting that moved,
// and has a rule: the uniform one, or one of its own in retiredEnvRefuse (the
// SQL Memory bounds, the retention switch, the two Ollama options, the hooks
// lists, the usage archiver). One with neither would be dropped in silence.
// The names below are the ones only advised on purpose.
func TestRetiredEnvMoved_EveryMovedVariableHasOneRule(t *testing.T) {
	advisedOnly := map[string]string{
		// Not read by the release before this one either: nothing is lost.
		"LOOMCYCLE_INTERRUPTION_DEFAULT_TIMEOUT_MS":    "already dead",
		"LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS":        "already dead",
		"LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN":   "already dead",
		"LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS": "already dead",
		// A collection cadence: no value of it kept or lost data.
		"LOOMCYCLE_SQLMEM_GC_INTERVAL_MS": "a cadence",
		// The archiver's age and directory are judged with its mode.
		"LOOMCYCLE_USAGE_RUN_RETENTION_MS": "part of LOOMCYCLE_USAGE_RUN_RETENTION_MODE's rule",
		"LOOMCYCLE_USAGE_EXPORT_DIR":       "the archiver exported nowhere else",
	}
	for name, instead := range retiredEnv {
		_, moved := retiredEnvMoved[name]
		_, own := retiredEnvRefuse[name]
		if moved && own {
			t.Errorf("%s has a rule in both retiredEnvMoved and retiredEnvRefuse", name)
		}
		if !strings.Contains(instead, "in the yaml") || moved || own {
			continue
		}
		if _, ok := advisedOnly[name]; !ok {
			t.Errorf("%s moved to the yaml (%q) and has no rule: set to something other than the default it would be dropped with only an advisory", name, instead)
		}
	}
	// The advisory and the refusal name the same key.
	for name, m := range retiredEnvMoved {
		for _, key := range strings.Split(m.key, " and ") {
			if !strings.Contains(retiredEnv[name], key) {
				t.Errorf("%s: the advisory %q does not name the rule's key %s", name, retiredEnv[name], key)
			}
		}
	}
}

// A raised provider timeout is lost as silently as a lowered one. The refusal
// names the yaml key and the providers that still lack the option, and counts
// only providers this deployment can use: a built-in provider with no key is
// not registered, so no option is asked of it.
func TestLoadLayers_RefusesARaisedProviderTimeoutNamingTheProviders(t *testing.T) {
	const yaml = `defaults: {provider: mock}
providers:
  anthropic: {driver: anthropic, api_key_env: ANTHROPIC_API_KEY}
  openai: {driver: openai, api_key_env: OPENAI_API_KEY}
  gemini: {driver: gemini, api_key_env: GEMINI_API_KEY}
  ollama-local: {driver: ollama}
  code-js: {driver: code-js}
`
	t.Setenv("ANTHROPIC_API_KEY", "k-anthropic")
	t.Setenv("OPENAI_API_KEY", "k-openai")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS", "300000")
	_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(yaml)})
	if err == nil {
		t.Fatal("LoadLayers accepted a raised provider timeout the yaml does not carry")
	}
	msg := err.Error()
	for _, want := range []string{"LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS: ", "providers.<id>.options.idle_timeout_ms", "these providers: anthropic, openai ("} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not say %q: %s", want, msg)
		}
	}
	for _, not := range []string{"gemini", "ollama-local", "code-js", "300000", "k-anthropic", "k-openai"} {
		if strings.Contains(msg, not) {
			t.Errorf("error carries %q: %s", not, msg)
		}
	}

	// The option on each of those providers, with any value: advised only.
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte(yaml)}, Layer{Name: "site.yaml", Data: []byte(`providers:
  anthropic: {options: {idle_timeout_ms: 300000}}
  openai: {options: {idle_timeout_ms: 120000}}
`)})
	if err != nil {
		t.Fatalf("LoadLayers with the option on each usable provider: %v", err)
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS")
}

// A retention family variable was read by a sweeper that ran only for
// LOOMCYCLE_RETENTION_ENABLED=1. Left over in a deployment that never ran
// retention, it changed nothing and stops nothing.
func TestLoadLayers_AdvisesAStrayRetentionVariableWithRetentionOff(t *testing.T) {
	t.Setenv("LOOMCYCLE_RETENTION_CHATS_MODE", "prune")
	t.Setenv("LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS", "86400000")
	t.Setenv("LOOMCYCLE_RETENTION_EXPORT_DIR", "/data/exports")
	cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")})
	if err != nil {
		t.Fatalf("LoadLayers refused retention variables of a sweeper that never ran: %v", err)
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_RETENTION_CHATS_MODE")
	if cfg.Env.RetentionEnabled || cfg.Env.RetentionChatsMode != "" {
		t.Errorf("retention = enabled %v, chats %q; want off", cfg.Env.RetentionEnabled, cfg.Env.RetentionChatsMode)
	}

	// The sweeper ran: the same variables are a setting the yaml lacks.
	t.Setenv("LOOMCYCLE_RETENTION_ENABLED", "1")
	_, err = LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\nretention: {enabled: true}\n")})
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_RETENTION_CHATS_MODE: ") || !strings.Contains(err.Error(), "retention.chats.mode") {
		t.Errorf("err = %v, want a refusal naming the variable and retention.chats.mode", err)
	}
}

// The sampler no longer starts from the variable. With no `metrics:` key the
// activity page would be empty and nothing would say why.
func TestLoadLayers_RefusesTheMetricsSwitchWithNoMetricsBlock(t *testing.T) {
	t.Setenv("LOOMCYCLE_METRICS_ENABLED", "1")
	_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")})
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_METRICS_ENABLED: ") || !strings.Contains(err.Error(), "metrics.enabled") {
		t.Fatalf("err = %v, want a refusal naming the variable and metrics.enabled", err)
	}
	// Naming the key, even to turn the sampler off, is the operator's answer.
	for _, block := range []string{"metrics: {enabled: true}", "metrics: {enabled: false}"} {
		cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n" + block + "\n")})
		if err != nil {
			t.Errorf("%s: %v", block, err)
			continue
		}
		requireRetiredAdvisory(t, cfg, "LOOMCYCLE_METRICS_ENABLED")
	}
	// One file on disk takes the other parse path.
	if _, err := loadYAMLString(t, "defaults: {provider: mock}\nmetrics: {enabled: true}\n"); err != nil {
		t.Errorf("one file naming metrics.enabled: %v", err)
	}
	if _, err := loadYAMLString(t, "defaults: {provider: mock}\nmetrics:\n  enabled:\n"); err == nil {
		t.Error("a null metrics.enabled counted as naming the key")
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

// The refusal's leading sentence lists what a refused variable can stand
// for. A variable that decided which data is kept is refused for that
// reason, and an operator reading only the lead must find it there.
func TestLoadLayers_RefusalLeadNamesDataKeptOrDeletedDifferently(t *testing.T) {
	t.Setenv("LOOMCYCLE_USAGE_SWEEPER", "0")
	_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults:\n  provider: mock\n")})
	if err == nil {
		t.Fatal("LoadLayers accepted a retired switch that kept per-call usage rows")
	}
	lead, _, _ := strings.Cut(err.Error(), "LOOMCYCLE_USAGE_SWEEPER")
	if !strings.Contains(lead, "data kept or deleted differently") {
		t.Errorf("the lead does not name the data case: %q", lead)
	}
}
