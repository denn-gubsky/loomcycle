package config

import (
	"os"
	"strings"
	"testing"
)

// The backend is inferred from the DSN when the yaml does not name one, and an
// explicit yaml value is never overridden by the inference.
func TestStorageBackend_InferredFromDSNUnlessYAMLNamesOne(t *testing.T) {
	const dsn = "postgres://u@h/db"
	for _, tc := range []struct {
		name   string
		yaml   string
		envDSN string
		want   string
	}{
		{"no DSN anywhere is sqlite", "", "", "sqlite"},
		{"env DSN is postgres", "", dsn, "postgres"},
		{"yaml DSN is postgres", "storage:\n  pg_dsn: " + dsn + "\n", "", "postgres"},
		{"explicit yaml sqlite wins over an env DSN", "storage:\n  backend: sqlite\n", dsn, "sqlite"},
		{"explicit yaml sqlite wins over a yaml DSN", "storage:\n  backend: sqlite\n  pg_dsn: " + dsn + "\n", "", "sqlite"},
		{"explicit yaml postgres without a DSN stays postgres", "storage:\n  backend: postgres\n", "", "postgres"},
		{"an unexpanded yaml reference is not a DSN", "storage:\n  pg_dsn: ${LOOMCYCLE_PG_DSN}\n", "", "sqlite"},
		{"an unexpanded yaml reference with the env DSN is postgres", "storage:\n  pg_dsn: ${LOOMCYCLE_PG_DSN}\n", dsn, "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", tc.envDSN)
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Storage.Backend != tc.want {
				t.Errorf("storage.backend = %q, want %q", cfg.Storage.Backend, tc.want)
			}
		})
	}
}

// SQL Memory follows the store when the yaml does not say: on with SQLite, on
// with Postgres only when its own DSN is set. An explicit yaml value wins in
// both directions.
func TestSQLMem_InferredFromStoreUnlessYAMLSays(t *testing.T) {
	const pg = "postgres://u@h/db"
	const aux = "postgres://u@h/aux"
	for _, tc := range []struct {
		name          string
		yaml          string
		pgDSN, auxDSN string
		want          bool
	}{
		{"sqlite store", "", "", "", true},
		{"sqlite store ignores a missing aux DSN", "storage:\n  backend: sqlite\n", pg, "", true},
		{"postgres store without the aux DSN", "", pg, "", false},
		{"postgres store with the aux DSN in env", "", pg, aux, true},
		{"postgres store with the aux DSN in yaml", "storage:\n  sqlmem_pg_dsn: " + aux + "\n", pg, "", true},
		{"postgres store with an unexpanded aux reference", "storage:\n  sqlmem_pg_dsn: ${LOOMCYCLE_SQLMEM_PG_DSN}\n", pg, "", false},
		{"yaml false turns it off on sqlite", "storage:\n  sqlmem_enabled: false\n", "", "", false},
		{"yaml false turns it off on postgres with the aux DSN", "storage:\n  sqlmem_enabled: false\n", pg, aux, false},
		{"yaml true on postgres without the aux DSN stays on", "storage:\n  sqlmem_enabled: true\n", pg, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", tc.pgDSN)
			t.Setenv("LOOMCYCLE_SQLMEM_PG_DSN", tc.auxDSN)
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Storage.SQLMemOn(); got != tc.want {
				t.Errorf("SQLMemOn = %v, want %v", got, tc.want)
			}
		})
	}
}

// The retired switch is not read. A value that kept SQL Memory off where it
// is now on, or asked for it where it now stays off, stops the load; a value
// that agrees with what the store gives is only advised.
func TestLoadLayers_RefusesARetiredSQLMemSwitchThatDisagreesWithTheStore(t *testing.T) {
	const pg = "postgres://u:hunter2@h/db"
	for _, tc := range []struct {
		name, value, pgDSN, want string
	}{
		{"off on sqlite", "0", "", "storage.sqlmem_enabled: false"},
		{"on with postgres and no aux DSN", "1", pg, "LOOMCYCLE_SQLMEM_PG_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", tc.pgDSN)
			t.Setenv("LOOMCYCLE_SQLMEM_PG_DSN", "")
			t.Setenv("LOOMCYCLE_SQLMEM_ENABLED", tc.value)
			_, err := loadYAMLString(t, "defaults: {provider: mock}\n")
			if err == nil {
				t.Fatal("the load accepted a retired SQL Memory switch that disagrees with the store")
			}
			msg := err.Error()
			for _, want := range []string{"LOOMCYCLE_SQLMEM_ENABLED", tc.want} {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not name %q: %s", want, msg)
				}
			}
			if strings.Contains(msg, "hunter2") || strings.Contains(msg, "@h/db") {
				t.Errorf("error carries the DSN: %s", msg)
			}
		})
	}

	for _, tc := range []struct {
		name, value, yaml string
		want              bool
	}{
		{"on on sqlite", "1", "", true},
		{"off with the yaml turning it off", "0", "storage:\n  sqlmem_enabled: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", "")
			t.Setenv("LOOMCYCLE_SQLMEM_ENABLED", tc.value)
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Storage.SQLMemOn(); got != tc.want {
				t.Errorf("SQLMemOn = %v, want %v", got, tc.want)
			}
			requireRetiredAdvisory(t, cfg, "LOOMCYCLE_SQLMEM_ENABLED")
		})
	}
}

// The retired variable no longer selects the backend. Naming another backend
// than the one the DSN gives stops the load instead of opening the wrong
// store; naming the same one, or pinning it in the yaml, is only advised.
func TestLoadLayers_RefusesARetiredStorageBackendThatNamesAnotherStore(t *testing.T) {
	const dsn = "postgres://u:hunter2@h/db"
	for _, tc := range []struct {
		name, value, dsn string
	}{
		{"postgres with no DSN", "postgres", ""},
		{"sqlite with a DSN", "sqlite", dsn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", tc.dsn)
			t.Setenv("LOOMCYCLE_STORAGE_BACKEND", tc.value)
			_, err := loadYAMLString(t, "defaults: {provider: mock}\n")
			if err == nil {
				t.Fatal("the load accepted a retired backend that names another store")
			}
			msg := err.Error()
			for _, want := range []string{"LOOMCYCLE_STORAGE_BACKEND", "storage.backend"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not name %q: %s", want, msg)
				}
			}
			if strings.Contains(msg, "hunter2") || strings.Contains(msg, "@h/db") {
				t.Errorf("error carries the DSN: %s", msg)
			}
		})
	}

	for _, tc := range []struct {
		name, value, dsn, yaml, want string
	}{
		{"sqlite with no DSN", "sqlite", "", "", "sqlite"},
		{"postgres with a DSN", "postgres", dsn, "", "postgres"},
		{"postgres pinned in the yaml", "postgres", "", "storage:\n  backend: postgres\n", "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_PG_DSN", tc.dsn)
			t.Setenv("LOOMCYCLE_STORAGE_BACKEND", tc.value)
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Storage.Backend != tc.want {
				t.Errorf("storage.backend = %q, want %q", cfg.Storage.Backend, tc.want)
			}
			requireRetiredAdvisory(t, cfg, "LOOMCYCLE_STORAGE_BACKEND")
		})
	}
}

func requireRetiredAdvisory(t *testing.T, cfg *Config, name string) {
	t.Helper()
	for _, w := range cfg.Warnings {
		if len(w) > len(name) && w[:len(name)] == name {
			return
		}
	}
	t.Errorf("no advisory for %s; warnings = %q", name, cfg.Warnings)
}

// The retention sweeper starts on its switch alone. A family mode staged
// without the switch deletes nothing: with no age set, a chats mode of prune
// would take every finished chat on the first sweep.
func TestRetentionEnabled_NeedsTheSwitchWhateverTheFamilyModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		set         bool
		value, mode string
		want        bool
	}{
		{name: "nothing set"},
		{name: "a family mode alone", mode: "prune"},
		{name: "a family mode with the switch off", set: true, value: "0", mode: "export+prune"},
		{name: "the switch alone", set: true, value: "1", want: true},
		{name: "the switch and a family mode", set: true, value: "1", mode: "prune", want: true},
		{name: "a word that is not 1", set: true, value: "true", mode: "prune"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_RETENTION_ENABLED", tc.value)
			if !tc.set {
				if err := os.Unsetenv("LOOMCYCLE_RETENTION_ENABLED"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("LOOMCYCLE_RETENTION_CHATS_MODE", tc.mode)
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Env.RetentionEnabled != tc.want {
				t.Errorf("RetentionEnabled = %v, want %v", cfg.Env.RetentionEnabled, tc.want)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w, "LOOMCYCLE_RETENTION_ENABLED") {
					t.Errorf("the switch is reported as retired: %s", w)
				}
			}
		})
	}
}

// The retired switch kept A2A off whatever card was named. Set to anything
// but 1 with a card named, the load stops instead of serving A2A.
func TestLoadLayers_RefusesARetiredA2ASwitchThatKeptA2AOff(t *testing.T) {
	t.Setenv("LOOMCYCLE_A2A_SERVER_CARD", "public")
	t.Setenv("LOOMCYCLE_A2A_ENABLED", "0")
	_, err := loadYAMLString(t, "defaults: {provider: mock}\n")
	if err == nil {
		t.Fatal("the load accepted a retired A2A switch that kept A2A off")
	}
	for _, want := range []string{"LOOMCYCLE_A2A_ENABLED", "unset LOOMCYCLE_A2A_SERVER_CARD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %s", want, err)
		}
	}
}

// A2A is on exactly when the active server card is named. Declaring cards in
// the yaml does not turn it on, and neither does the retired switch.
func TestA2AEnabled_FollowsTheServerCardName(t *testing.T) {
	const yamlCard = "a2a_server_cards:\n  public:\n    name: Public\n"
	for _, tc := range []struct {
		name          string
		yaml          string
		card, retired string
		want          bool
	}{
		{"no card", "", "", "", false},
		{"card named", "", "public", "", true},
		{"cards declared in yaml but none named", yamlCard, "", "", false},
		{"cards declared in yaml and one named", yamlCard, "public", "", true},
		{"retired switch without a card", "", "", "1", false},
		{"retired switch on with a card", "", "public", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_A2A_SERVER_CARD", tc.card)
			if tc.retired != "" {
				t.Setenv("LOOMCYCLE_A2A_ENABLED", tc.retired)
			}
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n"+tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Env.A2AServerEnabled != tc.want {
				t.Errorf("A2AServerEnabled = %v, want %v", cfg.Env.A2AServerEnabled, tc.want)
			}
			if tc.retired != "" {
				requireRetiredAdvisory(t, cfg, "LOOMCYCLE_A2A_ENABLED")
			}
		})
	}
}
