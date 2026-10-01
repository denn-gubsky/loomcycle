package webhook

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// BuildEnvAllowlist computes the env-var-NAME allowlist the receiver uses to
// gate secret + credential resolution: the operator knobs plus every name a
// STATIC webhook declares. The rule lives in config.(*Config).WebhookEnvAllowlist
// so the WebhookDef tool can ask whether a user_credentials_from_env re-supply
// will resolve here before it re-enables a def — see there for the rationale.
func BuildEnvAllowlist(cfg *config.Config) map[string]bool {
	return cfg.WebhookEnvAllowlist()
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
