package config

import (
	"strings"
	"testing"
)

const minimalQueueYAML = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
`

// max_queued_runs_per_user defaults to 16, takes yaml and the env override,
// keeps an explicit 0 (refuse over the per-user cap), and refuses a negative.
func TestConcurrency_MaxQueuedRunsPerUser(t *testing.T) {
	t.Setenv("LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", "")
	cfg, err := loadYAMLString(t, minimalQueueYAML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxQueuedRunsPerUser != 16 {
		t.Errorf("default = %d, want 16", cfg.Concurrency.MaxQueuedRunsPerUser)
	}

	cfg, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_queued_runs_per_user: 0 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxQueuedRunsPerUser != 0 {
		t.Errorf("explicit 0 = %d, want 0 kept", cfg.Concurrency.MaxQueuedRunsPerUser)
	}

	t.Setenv("LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", "3")
	cfg, err = loadYAMLString(t, minimalQueueYAML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxQueuedRunsPerUser != 3 {
		t.Errorf("env override = %d, want 3", cfg.Concurrency.MaxQueuedRunsPerUser)
	}

	t.Setenv("LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", "")
	_, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_queued_runs_per_user: -1 }\n")
	if err == nil || !strings.Contains(err.Error(), "max_queued_runs_per_user") {
		t.Errorf("negative value: err = %v, want a refusal naming the key", err)
	}
}
