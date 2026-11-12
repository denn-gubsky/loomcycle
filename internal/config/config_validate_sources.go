package config

// The blocks validate checks, in its order; see validate.

import (
	"fmt"
	"net/url"
	"strings"
)

// validateMemoryBackends checks the static memory_backends. Part of validate.
func validateMemoryBackends(c *Config) error {
	// Static memory_backends entries skip the MemoryBackendDef tool's
	// validateWebhookDef-equivalent, so structurally validate them at load.
	// Most important: a shared_key_with_prefix backend MUST carry a
	// {tenant_id} token in its prefix_pattern — an empty/token-less prefix
	// resolves to an empty key prefix and collapses every tenant into one
	// keyspace (cross-tenant read+write leak). resolveTenancy is the runtime
	// backstop, but failing loudly at boot is the better operator signal.
	for bname, mb := range c.MemoryBackends {
		if mb.TenancyStrategy.Kind == "shared_key_with_prefix" &&
			!strings.Contains(mb.TenancyStrategy.PrefixPattern, "{tenant_id}") {
			return fmt.Errorf("memory_backends.%s: tenancy_strategy.prefix_pattern %q must contain {tenant_id} for shared_key_with_prefix (an empty or token-less prefix collapses all tenants into one keyspace)", bname, mb.TenancyStrategy.PrefixPattern)
		}
		// RFC CD Part B: a static kind:remote backend is dialed at runtime, so
		// fail fast at load on a missing base_url or the unsupported tenancy
		// (the api_key_env allowlist is enforced at resolve time).
		if mb.Kind == "remote" {
			if mb.Config.BaseURL == "" {
				return fmt.Errorf("memory_backends.%s: kind=remote requires config.base_url", bname)
			}
			if mb.TenancyStrategy.Kind == "shared_key_with_prefix" {
				return fmt.Errorf("memory_backends.%s: kind=remote does not support tenancy_strategy=shared_key_with_prefix (use key_per_tenant)", bname)
			}
		}
	}
	return nil
}

// validateChangeSubscriptions checks the static change subscriptions. Part of
// validate.
func validateChangeSubscriptions(c *Config) error {
	// Static change subscriptions (RFC CD Part C push): a misconfigured callback
	// can never deliver, so fail fast at load.
	changeKindValid := map[string]bool{
		"memory": true, "document": true,
		"memory.set": true, "memory.delete": true, "memory.scope_deleted": true,
		"document.chunk.updated": true, "document.chunk.deleted": true,
	}
	for sname, sub := range c.ChangeSubscriptions {
		if sub.Disabled {
			continue
		}
		if sub.CallbackURL == "" {
			return fmt.Errorf("change_subscriptions.%s: callback_url is required", sname)
		}
		if u, uerr := url.Parse(sub.CallbackURL); uerr != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("change_subscriptions.%s: callback_url %q must be an http(s) URL", sname, sub.CallbackURL)
		}
		if sub.SecretEnv != "" && !EnvNameCredentialSafe(sub.SecretEnv) {
			return fmt.Errorf("change_subscriptions.%s: secret_env %q is not an allowed credential env var "+
				"(must be LOOMCYCLE_-prefixed or a known third-party key, and not an infra secret)", sname, sub.SecretEnv)
		}
		switch sub.Scope {
		case "", "agent", "user", "tenant":
		default:
			return fmt.Errorf("change_subscriptions.%s: scope %q must be agent, user, or tenant", sname, sub.Scope)
		}
		for _, k := range sub.Kinds {
			if !changeKindValid[k] {
				return fmt.Errorf("change_subscriptions.%s: unknown kind %q (want a change type or the family \"memory\"/\"document\")", sname, k)
			}
		}
	}
	return nil
}

// validateDocumentSources checks the static document sources. Part of
// validate.
func validateDocumentSources(c *Config) error {
	// Static document sources (RFC CE): a misconfigured peer can never sync, so
	// fail fast at load. base_url is dialed and api_key_env is SENT to it.
	for dname, ds := range c.DocumentSources {
		if ds.Config.BaseURL == "" {
			return fmt.Errorf("document_sources.%s: config.base_url is required", dname)
		}
		// Require scheme AND host — an empty host (e.g. "http:///v1") is a
		// non-dialable URL. This matches the DocumentSourceDef overlay validator
		// (requireHTTPURL), so a value accepted at boot is also accepted for a
		// runtime fork and vice-versa.
		if u, uerr := url.Parse(ds.Config.BaseURL); uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("document_sources.%s: config.base_url %q must be an http(s) URL with a host", dname, ds.Config.BaseURL)
		}
		if ds.Config.APIKeyEnv != "" && !EnvNameCredentialSafe(ds.Config.APIKeyEnv) {
			return fmt.Errorf("document_sources.%s: config.api_key_env %q is not an allowed credential env var "+
				"(must be LOOMCYCLE_-prefixed or a known third-party key, and not an infra secret)", dname, ds.Config.APIKeyEnv)
		}
		switch ds.TenancyStrategy.Kind {
		case "":
		case "key_per_tenant":
			if ds.TenancyStrategy.EnvPattern != "" && !strings.Contains(ds.TenancyStrategy.EnvPattern, "{tenant_id}") {
				return fmt.Errorf("document_sources.%s: tenancy_strategy.env_pattern %q must contain {tenant_id}", dname, ds.TenancyStrategy.EnvPattern)
			}
		default:
			return fmt.Errorf("document_sources.%s: tenancy_strategy.kind %q must be \"\" or key_per_tenant", dname, ds.TenancyStrategy.Kind)
		}
		if !ValidDocumentSourceTrust(ds.Trust) {
			return fmt.Errorf("document_sources.%s: trust %q must be proposed (the default) or asserted", dname, ds.Trust)
		}
	}
	return nil
}

// validateStaticWebhooks checks the static webhooks and warns on a mapped
// user_tier. Part of validate.
func validateStaticWebhooks(c *Config) error {
	// Static webhooks: a misconfigured delivery target can never fire (F24).
	for wname, wh := range c.Webhooks {
		if err := validateStaticWebhook(wname, wh); err != nil {
			return err
		}
		// user_tier is Def-pinned (webhooks.<name>.user_tier). A payload_mapping
		// targeting "user_tier" is now silently ineffective (the payload can't
		// select the cost tier) — warn so the operator moves it to the def.
		if _, mapped := wh.PayloadMapping["user_tier"]; mapped {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"webhooks.%s: payload_mapping targets \"user_tier\", but user_tier is pinned from the def (set webhooks.%s.user_tier) and the payload value is IGNORED",
				wname, wname))
		}
	}
	return nil
}
