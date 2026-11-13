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
