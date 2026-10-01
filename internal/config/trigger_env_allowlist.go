package config

// The env-var-NAME allowlists the trigger fire paths read
// user_credentials_from_env through. They live here, not beside each fire
// path, because the ScheduleDef and WebhookDef tools must answer the same
// question the fire path does — "will this name actually resolve?" — before
// they re-enable a def restored without its credentials. One implementation
// per trigger keeps the tool and the fire path from drifting apart.

// SchedulerCredentialEnvAllowlist is the set of env-var names a scheduled run
// may read through user_credentials_from_env: exactly
// LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST. Empty by default, so a default install
// resolves no env credential for a schedule at all.
func (c *Config) SchedulerCredentialEnvAllowlist() map[string]bool {
	allow := make(map[string]bool)
	if c == nil {
		return allow
	}
	for _, name := range c.Env.SchedulerEnvAllowlist {
		if name != "" {
			allow[name] = true
		}
	}
	return allow
}

// WebhookEnvAllowlist is the env-var-NAME allowlist the webhook receiver uses
// to gate secret + credential resolution. It is the union of:
//
//   - the explicit operator knobs: Env.SchedulerEnvAllowlist
//     (LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST) and Env.WebhooksEnvAllowlist
//     (LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST);
//   - every env-var NAME declared by a STATIC (operator-authored) webhook in
//     Webhooks: the HMAC signing secret, the bearer token, and every value
//     in user_credentials_from_env.
//
// Static-declared names are auto-trusted because the operator wrote the yaml.
// Requiring them to ALSO appear in the allowlist env var was the F23 trap: a
// static webhook's own signing_secret_env silently never resolved (the
// allowlist stayed at 0 names) and every signed delivery 503'd.
//
// Runtime (webhookdef-authored) defs are deliberately NOT scanned here. Their
// secret/cred env names still need an explicit allowlist entry — except a
// LOOMCYCLE_*-named VERIFY secret, which the receiver admits via the namespace
// auto-allow (a verify secret never reaches the agent). This keeps a
// less-trusted authoring path from naming an arbitrary env var as an
// agent-reachable credential source.
func (c *Config) WebhookEnvAllowlist() map[string]bool {
	allow := c.SchedulerCredentialEnvAllowlist()
	if c == nil {
		return allow
	}
	add := func(name string) {
		if name != "" {
			allow[name] = true
		}
	}
	for _, name := range c.Env.WebhooksEnvAllowlist {
		add(name)
	}
	for _, w := range c.Webhooks {
		add(w.Auth.SigningSecretEnv)
		add(w.Auth.BearerTokenEnv)
		for _, envName := range w.UserCredentialsFromEnv {
			add(envName)
		}
	}
	return allow
}
