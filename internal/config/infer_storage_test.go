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
