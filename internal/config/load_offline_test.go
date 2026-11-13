package config

import (
	"strings"
	"testing"
)

// The offline tools (tokens rewrap, audit verify, migrate) are what an
// operator reaches for when the server will not start. A value the server
// refuses in a variable the tool never uses must not stop the tool as well:
// it loads, and says what it did not enforce.
func TestLoadLayersWith_OfflineToolWarnsWhereTheServerRefuses(t *testing.T) {
	layer := Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string // each must appear in one NotEnforced entry
	}{
		{"a number that is not one", map[string]string{"LOOMCYCLE_AUTH_CACHE_TTL_SECONDS": "zz-soon"}, []string{"LOOMCYCLE_AUTH_CACHE_TTL_SECONDS"}},
		{"a cap written as zero", map[string]string{"LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS": "0"}, []string{"LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS"}},
		{"a rate and a burst that do not parse", map[string]string{"LOOMCYCLE_RUN_RATE_PER_MINUTE": "zz-many", "LOOMCYCLE_RUN_RATE_BURST": "zz-big"}, []string{"LOOMCYCLE_RUN_RATE_PER_MINUTE"}},
		{"a burst out of range", map[string]string{"LOOMCYCLE_RUN_RATE_PER_MINUTE": "60", "LOOMCYCLE_RUN_RATE_BURST": "77000000"}, []string{"LOOMCYCLE_RUN_RATE_BURST"}},
		{"an unknown enforcement mode", map[string]string{"LOOMCYCLE_LIMITS_ENFORCE": "zz-strictt"}, []string{"LOOMCYCLE_LIMITS_ENFORCE"}},
		{"a git host that is a URL", map[string]string{"LOOMCYCLE_GIT_SOURCE_HOSTS": "https://zz-gitea.example.com/x"}, []string{"LOOMCYCLE_GIT_SOURCE_HOSTS"}},
		{"a retired gate kept off", map[string]string{"LOOMCYCLE_SCHEDULER_ENABLED": "0"}, []string{"LOOMCYCLE_SCHEDULER_ENABLED"}},
		{"the retired config-files variable", map[string]string{"LOOMCYCLE_CONFIG_FILES": "/zz-x.yaml"}, []string{"LOOMCYCLE_CONFIG_FILES"}},
		{"several at once", map[string]string{"LOOMCYCLE_SCHEDULER_ENABLED": "0", "LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS": "-77", "LOOMCYCLE_LIMITS_ENFORCE": "zz-on"},
			[]string{"LOOMCYCLE_SCHEDULER_ENABLED", "LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", "LOOMCYCLE_LIMITS_ENFORCE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := LoadLayers(layer); err == nil {
				t.Fatal("the server's load accepted this environment; the case tests nothing")
			}
			cfg, err := LoadLayersWith(LoadOptions{OfflineTool: true}, layer)
			if err != nil {
				t.Fatalf("the offline tool's load failed: %v", err)
			}
			notes := strings.Join(cfg.NotEnforced, "\n")
			for _, want := range tc.want {
				if !strings.Contains(notes, want) {
					t.Errorf("NotEnforced does not name %s:\n%s", want, notes)
				}
			}
			if strings.Contains(notes, "zz-") || strings.Contains(notes, "77") {
				t.Errorf("NotEnforced carries a value:\n%s", notes)
			}
			// What could not be honoured is left at what unset gives.
			e := cfg.Env
			if e.AuthCacheTTLSeconds != AuthCacheTTLSecondsDefault && tc.env["LOOMCYCLE_AUTH_CACHE_TTL_SECONDS"] != "" {
				t.Errorf("AuthCacheTTLSeconds = %d, want the default", e.AuthCacheTTLSeconds)
			}
			if e.UserTokenMaxTTLSeconds != 0 || e.RunRatePerMinute != 0 || e.RunRateBurst != 0 || e.LimitsEnforceStrict || len(e.GitSourceHosts) != 0 || e.MCPSpawnRunTimeoutMS != 0 {
				t.Errorf("a value that could not be honoured was applied anyway: %+v", []any{e.UserTokenMaxTTLSeconds, e.RunRatePerMinute, e.RunRateBurst, e.LimitsEnforceStrict, e.GitSourceHosts, e.MCPSpawnRunTimeoutMS})
			}
		})
	}
}

// Which store is opened is never relaxed: a tool that writes to a store must
// stop where the server would rather than open the other one.
func TestLoadLayersWith_OfflineToolStillRefusesAStoreDisagreement(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	t.Setenv("LOOMCYCLE_STORAGE_BACKEND", "postgres") // no DSN: the store would be SQLite
	t.Setenv("LOOMCYCLE_SCHEDULER_ENABLED", "0")
	_, err := LoadLayersWith(LoadOptions{OfflineTool: true}, Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")})
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_STORAGE_BACKEND") {
		t.Fatalf("err = %v, want the refusal of LOOMCYCLE_STORAGE_BACKEND", err)
	}
	if strings.Contains(err.Error(), "LOOMCYCLE_SCHEDULER_ENABLED") {
		t.Errorf("the error also carries a refusal this load only warns about: %v", err)
	}
}

// The option changes nothing for an environment the server accepts, and the
// plain load never fills NotEnforced.
func TestLoadLayersWith_OfflineToolIsTheSameLoadOtherwise(t *testing.T) {
	layer := Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")}
	t.Setenv("LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", "5")
	t.Setenv("LOOMCYCLE_RUN_RATE_PER_MINUTE", "60")
	strict, err := LoadLayers(layer)
	if err != nil {
		t.Fatal(err)
	}
	offline, err := LoadLayersWith(LoadOptions{OfflineTool: true}, layer)
	if err != nil {
		t.Fatal(err)
	}
	if len(strict.NotEnforced) != 0 || len(offline.NotEnforced) != 0 {
		t.Errorf("NotEnforced = %v / %v, want none", strict.NotEnforced, offline.NotEnforced)
	}
	if offline.Env.AuthCacheTTLSeconds != 5 || offline.Env.RunRatePerMinute != 60 || strict.Env.AuthCacheTTLSeconds != 5 {
		t.Errorf("valid values were not applied: %d %d", offline.Env.AuthCacheTTLSeconds, offline.Env.RunRatePerMinute)
	}
}
