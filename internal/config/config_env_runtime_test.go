package config

import (
	"strings"
	"testing"
)

// The settings the boot code used to read for itself are fields of Env, read
// at load. A value that cannot be honoured fails the load, naming the variable
// and never the value; before, some of these stopped the boot and some fell
// back to the default without a word.
func TestLoadLayers_RuntimeEnvBadValueFailsTheLoadNamingTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		also        map[string]string
	}{
		// Fell back silently before.
		{name: "LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", value: "zz-soon"},
		{name: "LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", value: "-77"},
		{name: "LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS", value: "zz-1h"},
		{name: "LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS", value: "-77"},
		{name: "LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", value: "zz-off"},
		{name: "LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", value: "-77"},
		// Stopped the boot before, after the store was open.
		{name: "LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS", value: "0"},
		{name: "LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS", value: "zz-day"},
		{name: "LOOMCYCLE_RUN_RATE_PER_MINUTE", value: "zz-many"},
		{name: "LOOMCYCLE_RUN_RATE_PER_MINUTE", value: "0"},
		{name: "LOOMCYCLE_RUN_RATE_BURST", value: "zz-big", also: map[string]string{"LOOMCYCLE_RUN_RATE_PER_MINUTE": "60"}},
		{name: "LOOMCYCLE_LIMITS_ENFORCE", value: "zz-strictt"},
		{name: "LOOMCYCLE_GIT_SOURCE_HOSTS", value: "github.com,https://zz-gitea.internal/x"},
		{name: "LOOMCYCLE_GIT_SOURCE_HOSTS", value: "zz-github.com/acme"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			for k, v := range tc.also {
				t.Setenv(k, v)
			}
			t.Setenv(tc.name, tc.value)
			_, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")})
			if err == nil {
				t.Fatalf("loaded with %s set to a value it cannot honour", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error does not name the variable: %v", err)
			}
			if strings.Contains(err.Error(), "zz-") || strings.Contains(err.Error(), "77") {
				t.Errorf("error carries the value: %v", err)
			}
		})
	}
}

// What each variable gives when it is unset and when it is set well.
func TestLoadLayers_RuntimeEnvReadsEachSetting(t *testing.T) {
	load := func(t *testing.T) Env {
		t.Helper()
		cfg, err := LoadLayers(Layer{Name: "t.yaml", Data: []byte("defaults: {provider: mock}\n")})
		if err != nil {
			t.Fatalf("LoadLayers: %v", err)
		}
		return cfg.Env
	}
	names := []string{
		"LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", "LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS",
		"LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", "LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS",
		"LOOMCYCLE_RUN_RATE_PER_MINUTE", "LOOMCYCLE_RUN_RATE_BURST", "LOOMCYCLE_LIMITS_ENFORCE",
		"LOOMCYCLE_MEMORY_CHANGES_ENABLED", "LOOMCYCLE_MOCK_ENABLED",
		"LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED", "LOOMCYCLE_GIT_SOURCE_HOSTS",
	}
	for _, n := range names {
		t.Setenv(n, "")
	}
	e := load(t)
	if e.MCPSpawnRunTimeoutMS != 0 || e.OperatorTokenRotationGraceSeconds != 0 || e.UserTokenMaxTTLSeconds != 0 ||
		e.RunRatePerMinute != 0 || e.RunRateBurst != 0 || e.LimitsEnforceStrict || e.MemoryChangesEnabled ||
		e.MockEnabled || e.AnthropicOAuthDevEnabled || len(e.GitSourceHosts) != 0 {
		t.Errorf("unset: a setting is not at its off value: %+v", []any{e.MCPSpawnRunTimeoutMS, e.OperatorTokenRotationGraceSeconds,
			e.UserTokenMaxTTLSeconds, e.RunRatePerMinute, e.RunRateBurst, e.LimitsEnforceStrict, e.MemoryChangesEnabled,
			e.MockEnabled, e.AnthropicOAuthDevEnabled, e.GitSourceHosts})
	}
	if e.AuthCacheTTLSeconds != AuthCacheTTLSecondsDefault {
		t.Errorf("unset: AuthCacheTTLSeconds = %d, want the default %d", e.AuthCacheTTLSeconds, AuthCacheTTLSecondsDefault)
	}

	t.Setenv("LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", "120000")
	t.Setenv("LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS", "3600")
	t.Setenv("LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", "0")
	t.Setenv("LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS", "86400")
	t.Setenv("LOOMCYCLE_RUN_RATE_PER_MINUTE", "60")
	t.Setenv("LOOMCYCLE_RUN_RATE_BURST", "10")
	t.Setenv("LOOMCYCLE_LIMITS_ENFORCE", "strict")
	t.Setenv("LOOMCYCLE_MEMORY_CHANGES_ENABLED", "1")
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "1")
	t.Setenv("LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED", "1")
	t.Setenv("LOOMCYCLE_GIT_SOURCE_HOSTS", " GitHub.com , gitea.internal:3000 ,")
	e = load(t)
	if e.MCPSpawnRunTimeoutMS != 120000 || e.OperatorTokenRotationGraceSeconds != 3600 || e.AuthCacheTTLSeconds != 0 ||
		e.UserTokenMaxTTLSeconds != 86400 || e.RunRatePerMinute != 60 || e.RunRateBurst != 10 || !e.LimitsEnforceStrict ||
		!e.MemoryChangesEnabled || !e.MockEnabled || !e.AnthropicOAuthDevEnabled {
		t.Errorf("set: a setting was not read: %+v", []any{e.MCPSpawnRunTimeoutMS, e.OperatorTokenRotationGraceSeconds,
			e.AuthCacheTTLSeconds, e.UserTokenMaxTTLSeconds, e.RunRatePerMinute, e.RunRateBurst, e.LimitsEnforceStrict,
			e.MemoryChangesEnabled, e.MockEnabled, e.AnthropicOAuthDevEnabled})
	}
	if got, want := e.GitSourceHosts, []string{"github.com", "gitea.internal:3000"}; !equalStrings(got, want) {
		t.Errorf("GitSourceHosts = %v, want %v", got, want)
	}

	// A burst with no rate configures nothing and is not read, as before.
	t.Setenv("LOOMCYCLE_RUN_RATE_PER_MINUTE", "")
	t.Setenv("LOOMCYCLE_RUN_RATE_BURST", "zz-big")
	if e = load(t); e.RunRatePerMinute != 0 || e.RunRateBurst != 0 {
		t.Errorf("burst alone: rate = %d/%d, want none", e.RunRatePerMinute, e.RunRateBurst)
	}
	// A value of only whitespace is an empty value, and an empty value is
	// unset, as for every other variable of the environment. The two that
	// stopped the boot on it before the reads moved here no longer do: a
	// blank line in an env file is not a cap or a rate anyone asked for.
	for _, n := range names {
		t.Setenv(n, "")
	}
	t.Setenv("LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS", "  ")
	t.Setenv("LOOMCYCLE_RUN_RATE_PER_MINUTE", " \t")
	if e = load(t); e.UserTokenMaxTTLSeconds != 0 || e.RunRatePerMinute != 0 {
		t.Errorf("whitespace only: cap = %d, rate = %d, want both unset", e.UserTokenMaxTTLSeconds, e.RunRatePerMinute)
	}
	// A switch is on for exactly 1.
	t.Setenv("LOOMCYCLE_MOCK_ENABLED", "true")
	if e = load(t); e.MockEnabled {
		t.Error("LOOMCYCLE_MOCK_ENABLED=true turned the mock providers on; only 1 does")
	}
}
