package config

import (
	"path/filepath"
	"strings"
	"testing"
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
		name, value string
		also        []string // other variables of the same environment
		yaml        StorageConfig
		refused     bool
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
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "prune", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000", "LOOMCYCLE_RETENTION_CHATS_MODE=prune"}},
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "prune"},                                                            // no age: it never ran
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MODE", value: "off", also: []string{"LOOMCYCLE_USAGE_RUN_RETENTION_MS=86400000"}}, // dormant
		{name: "LOOMCYCLE_USAGE_RUN_RETENTION_MS", value: "86400000"},                                                           // no mode: dormant

		// Retired, and no value of it was a tightening.
		{name: "LOOMCYCLE_SSE_KEEPALIVE_MS", value: "0"},
		{name: "LOOMCYCLE_SNAPSHOT_MAX_BYTES", value: "1"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			environ := append([]string{tc.name + "=" + tc.value}, tc.also...)
			got := retiredEnvRefusals(environ, &tc.yaml)
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
