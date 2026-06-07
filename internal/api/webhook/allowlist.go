package webhook

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// BuildEnvAllowlist computes the env-var-NAME allowlist the receiver uses to
// gate secret + credential resolution. It is the union of:
//
//   - the explicit operator knobs: cfg.Env.SchedulerEnvAllowlist
//     (LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST) and cfg.Env.WebhooksEnvAllowlist
//     (LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST);
//   - every env-var NAME declared by a STATIC (operator-authored) webhook in
//     cfg.Webhooks: the HMAC signing secret, the bearer token, and every value
//     in user_credentials_from_env.
//
// Static-declared names are auto-trusted because the operator wrote the yaml.
// Requiring them to ALSO appear in the allowlist env var was the F23 trap: a
// static webhook's own signing_secret_env silently never resolved (the
// allowlist stayed at 0 names) and every signed delivery 503'd.
//
// Runtime (webhookdef-authored) defs are deliberately NOT scanned here. Their
// secret/cred env names still need an explicit allowlist entry — except a
// LOOMCYCLE_*-named VERIFY secret, which resolveSecret admits via the namespace
// auto-allow (a verify secret never reaches the agent). This keeps a
// less-trusted authoring path from naming an arbitrary env var as an
// agent-reachable credential source.
func BuildEnvAllowlist(cfg *config.Config) map[string]bool {
	allow := make(map[string]bool)
	if cfg == nil {
		return allow
	}
	add := func(name string) {
		if name != "" {
			allow[name] = true
		}
	}
	for _, name := range cfg.Env.SchedulerEnvAllowlist {
		add(name)
	}
	for _, name := range cfg.Env.WebhooksEnvAllowlist {
		add(name)
	}
	for _, w := range cfg.Webhooks {
		add(w.Auth.SigningSecretEnv)
		add(w.Auth.BearerTokenEnv)
		for _, envName := range w.UserCredentialsFromEnv {
			add(envName)
		}
	}
	return allow
}

// UnresolvableStaticSecrets returns one human-readable warning per STATIC
// webhook that is misconfigured in a way that will fail every delivery:
//
//   - enabled:false (addressable but inert — 404s every delivery);
//   - a verify secret that will not resolve at request time (not allowlisted
//     and not LOOMCYCLE_*-prefixed, or allowlisted-but-unset/empty);
//   - auth.kind requires a secret but none is configured.
//
// Logged once at boot so an operator seeing "env_allowlist=N names" also sees
// exactly which webhook will 503 and why — the discoverability gap that made
// F23 a multi-hour dead end. Pure + getenv-injected for unit-testing; the
// returned slice order is unspecified (map iteration).
func UnresolvableStaticSecrets(cfg *config.Config, allow map[string]bool, getenv func(string) string) []string {
	var warns []string
	if cfg == nil {
		return warns
	}
	for name, w := range cfg.Webhooks {
		if !w.Enabled {
			warns = append(warns, fmt.Sprintf("webhook %q: enabled:false — addressable but inert (every delivery 404s)", name))
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(w.Auth.Kind))
		var envName string
		switch kind {
		case "", "hmac":
			envName = w.Auth.SigningSecretEnv
		case "bearer":
			envName = w.Auth.BearerTokenEnv
		case "none":
			continue // no verification secret needed (gated by allowUnauthenticated at request time)
		default:
			warns = append(warns, fmt.Sprintf("webhook %q: unknown auth.kind %q — every delivery 503s", name, w.Auth.Kind))
			continue
		}
		if envName == "" {
			warns = append(warns, fmt.Sprintf("webhook %q: auth.kind=%s but no secret env configured — every delivery 503s", name, kindOrHMAC(kind)))
			continue
		}
		if !allow[envName] && !config.ExpandEnvAllowed(envName) {
			warns = append(warns, fmt.Sprintf("webhook %q: secret env %q not allowlisted (add it to LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST or use a LOOMCYCLE_*-prefixed name) — every delivery 503s", name, envName))
			continue
		}
		if getenv(envName) == "" {
			warns = append(warns, fmt.Sprintf("webhook %q: secret env %q is allowlisted but unset/empty — every delivery 503s", name, envName))
		}
	}
	return warns
}

func kindOrHMAC(kind string) string {
	if kind == "" {
		return "hmac"
	}
	return kind
}
