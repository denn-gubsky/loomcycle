package config

import (
	"strings"
	"testing"
)

const minimalQueueYAML = `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
`

// max_queued_runs_per_user defaults to 16, takes the yaml, keeps an explicit 0
// (refuse over the per-user cap), and refuses a negative.
func TestConcurrency_MaxQueuedRunsPerUser(t *testing.T) {
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

	cfg, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_queued_runs_per_user: 3 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxQueuedRunsPerUser != 3 {
		t.Errorf("yaml 3 = %d, want 3", cfg.Concurrency.MaxQueuedRunsPerUser)
	}

	_, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_queued_runs_per_user: -1 }\n")
	if err == nil || !strings.Contains(err.Error(), "max_queued_runs_per_user") {
		t.Errorf("negative value: err = %v, want a refusal naming the key", err)
	}
}

// The queue bound defaults to 32 — room for two users' full per-user queue —
// and a provider queue with no depth of its own inherits it.
func TestConcurrency_MaxQueueDepthDefaultsTo32(t *testing.T) {
	cfg, err := loadYAMLString(t, minimalQueueYAML)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxQueueDepth != 32 {
		t.Errorf("max_queue_depth default = %d, want 32", cfg.Concurrency.MaxQueueDepth)
	}
	if got := cfg.Concurrency.ProviderQueueDepthOrDefault(); got != 32 {
		t.Errorf("provider queue depth default = %d, want the inherited 32", got)
	}
}

// The concurrency block is the only source of the per-user cap, the per-user
// queue and the provider queue depth: the three variables that overrode it are
// not read, and each is reported.
func TestConcurrency_TheYAMLIsTheOnlySource(t *testing.T) {
	cfg, err := loadYAMLString(t, minimalQueueYAML)
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Concurrency; c.MaxConcurrentRunsPerUser != 0 || c.MaxQueuedRunsPerUser != 16 || c.ProviderQueueDepth != 0 {
		t.Errorf("defaults = %+v, want no per-user cap, 16 queued, provider depth inherited", c)
	}
	cfg, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_concurrent_runs_per_user: 4, max_queued_runs_per_user: 2, provider_queue_depth: 7 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Concurrency; c.MaxConcurrentRunsPerUser != 4 || c.MaxQueuedRunsPerUser != 2 || c.ProviderQueueDepthOrDefault() != 7 {
		t.Errorf("yaml = %+v, want 4, 2 and 7", c)
	}

	// The yaml names each key, so the variables are reported and not read.
	t.Setenv("LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", "9")
	t.Setenv("LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", "64")
	t.Setenv("LOOMCYCLE_PROVIDER_QUEUE_DEPTH", "500")
	cfg, err = loadYAMLString(t, minimalQueueYAML+"concurrency: { max_concurrent_runs_per_user: 4, max_queued_runs_per_user: 16, provider_queue_depth: 32 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Concurrency; c.MaxConcurrentRunsPerUser != 4 || c.MaxQueuedRunsPerUser != 16 || c.ProviderQueueDepthOrDefault() != 32 {
		t.Errorf("with the retired variables set = %+v, want the yaml's (4, 16, 32)", c)
	}
	for _, name := range []string{"LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER", "LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER", "LOOMCYCLE_PROVIDER_QUEUE_DEPTH"} {
		requireRetiredAdvisory(t, cfg, name)
	}
}
