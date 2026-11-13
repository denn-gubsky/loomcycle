package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// AuthCacheTTLSecondsDefault is how long a replica keeps a resolved token
// when LOOMCYCLE_AUTH_CACHE_TTL_SECONDS is unset.
const AuthCacheTTLSecondsDefault = 30

// applyEnvRuntime reads the settings the boot code used to read for itself,
// each with its own answer to a bad value: some stopped the boot, some fell
// back to the default without a word. They are read once, here, under one
// rule: a value that cannot be honoured fails the load, and the error names
// the variable and never the value. The first bad value, in the order read,
// is the one reported.
//
// offlineTool (LoadOptions.OfflineTool) is the one exception: none of these
// settings is used by a tool that works on the store offline, so for it each
// bad value is recorded in cfg.NotEnforced and the setting is left as if
// unset.
func applyEnvRuntime(cfg *Config, offlineTool bool) error {
	e := &cfg.Env
	var first error
	bad := func(err error) {
		switch {
		case offlineTool:
			cfg.NotEnforced = append(cfg.NotEnforced, "the server refuses to start with this value, and this command reads the variable as unset: "+err.Error())
		case first == nil:
			first = err
		}
	}
	// whole is envWhole with a bad value reported and read as unset.
	whole := func(name string, dflt, lowest int) (n int, ok bool) {
		n, err := envWhole(name, dflt, lowest)
		if err != nil {
			bad(err)
			return dflt, false
		}
		return n, true
	}
	// The spawn_run transport timeout of the MCP surfaces. 0 or unset: none,
	// the run's own budget decides.
	e.MCPSpawnRunTimeoutMS, _ = whole("LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS", 0, 0)
	// How long a rotated operator token keeps working. 0 or unset: the 24h
	// default (OperatorTokenDef.grace); there is no setting for none.
	e.OperatorTokenRotationGraceSeconds, _ = whole("LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS", 0, 0)
	// The per-replica token-resolution cache. 0 turns it off: every request
	// looks its token up, and a revocation is immediate.
	e.AuthCacheTTLSeconds, _ = whole("LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", AuthCacheTTLSecondsDefault, 0)
	// A cap on a delegated user token's lifetime. Unset: no cap. Zero is not
	// "no cap": it is refused, so a cap written as 0 is not read as none.
	e.UserTokenMaxTTLSeconds, _ = whole("LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS", 0, 1)
	// The default run-start rate for a caller whose token carries none. The
	// burst is read only beside a rate: alone it configures nothing. A pair
	// of which either half is bad is no rate at all.
	if strings.TrimSpace(os.Getenv("LOOMCYCLE_RUN_RATE_PER_MINUTE")) != "" {
		rate, rateOK := whole("LOOMCYCLE_RUN_RATE_PER_MINUTE", 0, 1)
		burst, burstOK := 0, false
		if rateOK {
			burst, burstOK = whole("LOOMCYCLE_RUN_RATE_BURST", 0, 0)
		}
		if rateOK && burstOK {
			if err := (auth.RateLimit{PerMinute: rate, Burst: burst}).Validate(); err != nil {
				bad(fmt.Errorf("LOOMCYCLE_RUN_RATE_PER_MINUTE / LOOMCYCLE_RUN_RATE_BURST: %w", err))
			} else {
				e.RunRatePerMinute, e.RunRateBurst = rate, burst
			}
		}
	}
	// Strict token budgets. Any other value is refused: a deployment that
	// asked for enforcement and misspelled it must not run without it.
	switch strings.TrimSpace(os.Getenv("LOOMCYCLE_LIMITS_ENFORCE")) {
	case "":
	case "strict":
		e.LimitsEnforceStrict = true
	default:
		bad(fmt.Errorf("LOOMCYCLE_LIMITS_ENFORCE: not a known value (strict, or unset)"))
	}
	e.MemoryChangesEnabled = os.Getenv("LOOMCYCLE_MEMORY_CHANGES_ENABLED") == "1"
	e.MockEnabled = os.Getenv("LOOMCYCLE_MOCK_ENABLED") == "1"
	e.AnthropicOAuthDevEnabled = os.Getenv("LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED") == "1"
	// The hosts a git-sourced volume may clone from. Unset: the feature is
	// off. An entry that is a URL would never match a host, so it is refused,
	// and the whole list with it.
	var hosts []string
	hostsOK := true
	for _, h := range strings.Split(os.Getenv("LOOMCYCLE_GIT_SOURCE_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			if strings.ContainsAny(h, "/@ ") || strings.Contains(h, "://") {
				bad(fmt.Errorf("LOOMCYCLE_GIT_SOURCE_HOSTS: an entry is not a host name (write github.com, not a URL)"))
				hostsOK = false
				break
			}
			hosts = append(hosts, h)
		}
	}
	if hostsOK {
		e.GitSourceHosts = hosts
	}
	return first
}

// envWhole reads a whole number of at least lowest. Unset or empty gives
// dflt. The error names the variable only.
func envWhole(name string, dflt, lowest int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return dflt, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not a whole number", name)
	}
	if n < lowest {
		return 0, fmt.Errorf("%s: must be %d or more", name, lowest)
	}
	return n, nil
}

// SecretKeysFromEnv returns LOOMCYCLE_SECRET_KEY and
// LOOMCYCLE_SECRET_KEY_PREVIOUS for a command that works without a config or
// a store (`audit verify --export`, `otel user-hash`). A command that loads a
// config reads Env.SecretKey and Env.SecretKeyPrevious instead. Both are
// secrets: never log or print what this returns.
func SecretKeysFromEnv() (current, previous string) {
	return os.Getenv("LOOMCYCLE_SECRET_KEY"), os.Getenv("LOOMCYCLE_SECRET_KEY_PREVIOUS")
}

// PgDSNFromEnv returns LOOMCYCLE_PG_DSN for `loomcycle migrate` run with
// neither --dsn nor --config. With a config, Storage.PgDSN already carries
// it. A secret: never log or print what this returns.
func PgDSNFromEnv() string {
	return os.Getenv("LOOMCYCLE_PG_DSN")
}
