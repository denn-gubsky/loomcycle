package config

import "testing"

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

// The retired switch is not read: =0 does not turn SQL Memory off on SQLite
// (the yaml key does), and the operator is told.
func TestSQLMem_RetiredVariableIsNotRead(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	t.Setenv("LOOMCYCLE_SQLMEM_ENABLED", "0")
	cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Storage.SQLMemOn() {
		t.Error("SQLMemOn = false, want true")
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_SQLMEM_ENABLED")
}

// The retired variable no longer selects the backend: with no DSN the store
// stays SQLite, and the operator is told the line does nothing.
func TestStorageBackend_RetiredVariableIsNotRead(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_DSN", "")
	t.Setenv("LOOMCYCLE_STORAGE_BACKEND", "postgres")
	cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Backend != "sqlite" {
		t.Errorf("storage.backend = %q, want sqlite", cfg.Storage.Backend)
	}
	requireRetiredAdvisory(t, cfg, "LOOMCYCLE_STORAGE_BACKEND")
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
