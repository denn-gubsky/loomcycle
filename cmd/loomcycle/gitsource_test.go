package main

import (
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestGitSourceConfigFromEnv_OffUnlessHostsAreListed(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"LOOMCYCLE_GIT_SOURCE_HOSTS": "  , "},
		// The other settings alone do not turn it on.
		{"LOOMCYCLE_GIT_SOURCE_MAX_BYTES": "1000"},
	} {
		cfg, err := gitSourceConfigFromEnv(envOf(env), nil)
		if cfg != nil || err != nil {
			t.Errorf("%v: cfg=%v err=%v, want the feature off", env, cfg, err)
		}
	}
}

func TestGitSourceConfigFromEnv_DefaultsAndOverrides(t *testing.T) {
	cfg, err := gitSourceConfigFromEnv(envOf(map[string]string{"LOOMCYCLE_GIT_SOURCE_HOSTS": "GitHub.com, gitea.example:3000"}), nil)
	if err != nil || cfg == nil {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if got := strings.Join(cfg.Hosts, "|"); got != "github.com|gitea.example:3000" {
		t.Errorf("hosts = %q", got)
	}
	if cfg.MaxBytes != 256<<20 || cfg.MaxObjectBytes != 64<<20 || cfg.MaxFileBytes != 5<<20 ||
		cfg.MaxFiles != 100000 || cfg.Timeout != 2*time.Minute || cap(cfg.Slots) != 1 || cfg.MaxHistory != 500 {
		t.Errorf("defaults = %+v (slots %d)", cfg, cap(cfg.Slots))
	}
	if cfg.Transport == nil {
		t.Error("no transport: a clone would have nothing that enforces the network rules")
	}

	cfg, err = gitSourceConfigFromEnv(envOf(map[string]string{
		"LOOMCYCLE_GIT_SOURCE_HOSTS":            "github.com",
		"LOOMCYCLE_GIT_SOURCE_MAX_BYTES":        "1000",
		"LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES": "2000",
		"LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES":   "300",
		"LOOMCYCLE_GIT_SOURCE_MAX_FILES":        "40",
		"LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS":  "5",
		"LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT":   "3",
		"LOOMCYCLE_GIT_SOURCE_MAX_HISTORY":      "50",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBytes != 1000 || cfg.MaxObjectBytes != 2000 || cfg.MaxFileBytes != 300 || cfg.MaxFiles != 40 ||
		cfg.Timeout != 5*time.Second || cap(cfg.Slots) != 3 || cfg.MaxHistory != 50 {
		t.Errorf("overrides = %+v (slots %d)", cfg, cap(cfg.Slots))
	}
}

// A setting that is not a positive number, or a host written as a URL, stops
// the start: an operator who wrote a limit meant one.
func TestGitSourceConfigFromEnv_RefusesWhatItCannotRead(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"a limit that is not a number": {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_BYTES": "256MiB"},
		"a zero limit":                 {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES": "0"},
		"a negative count":             {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com", "LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT": "-1"},
		"a URL for a host":             {"LOOMCYCLE_GIT_SOURCE_HOSTS": "https://github.com"},
		"a path for a host":            {"LOOMCYCLE_GIT_SOURCE_HOSTS": "github.com/acme"},
	} {
		if cfg, err := gitSourceConfigFromEnv(envOf(env), nil); err == nil {
			t.Errorf("%s: accepted as %+v", name, cfg)
		}
	}
}
