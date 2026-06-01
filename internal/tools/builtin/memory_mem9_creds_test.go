package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests cover the RFC I MR-4 credential resolution + tenancy logic
// that lives in the builtin layer (so the mem9 package stays free of an
// internal/tools dependency — the CredentialResolver is injected). They
// exercise the resolver/tenancy functions directly, not over HTTP.

// TestMem9Cred_PerRunCredentialPreferred pins resolution order step 1:
// an RFC-F per-run credential keyed by the api_key_env NAME wins over the
// env fallback.
func TestMem9Cred_PerRunCredentialPreferred(t *testing.T) {
	m := &Memory{EnvAllowlist: map[string]bool{"LOOMCYCLE_MEM9_KEY": true}}
	t.Setenv("LOOMCYCLE_MEM9_KEY", "env-value")

	def := config.MemoryBackend{Config: config.MemoryBackendConfig{APIKeyEnv: "LOOMCYCLE_MEM9_KEY"}}
	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, "")

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserID:          "alice",
		UserCredentials: map[string]string{"LOOMCYCLE_MEM9_KEY": "per-run-value"},
	})
	got, err := resolver(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "per-run-value" {
		t.Errorf("resolved key = %q, want the per-run credential", got)
	}
}

// TestMem9Cred_EnvFallbackWhenAllowlisted pins step 2: no per-run cred →
// the allowlisted env var supplies the key.
func TestMem9Cred_EnvFallbackWhenAllowlisted(t *testing.T) {
	m := &Memory{EnvAllowlist: map[string]bool{"LOOMCYCLE_MEM9_KEY": true}}
	t.Setenv("LOOMCYCLE_MEM9_KEY", "env-value")

	def := config.MemoryBackend{Config: config.MemoryBackendConfig{APIKeyEnv: "LOOMCYCLE_MEM9_KEY"}}
	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, "")

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "alice"})
	got, err := resolver(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "env-value" {
		t.Errorf("resolved key = %q, want env-value", got)
	}
}

// TestMem9Cred_NonAllowlistedEnvIsTypedError pins the security gate: an
// env var NOT on the allowlist yields an error (never a silent
// unauthenticated call). The error references the env-var NAME, never a
// value.
func TestMem9Cred_NonAllowlistedEnvIsTypedError(t *testing.T) {
	m := &Memory{EnvAllowlist: map[string]bool{}} // empty → nothing allowed
	t.Setenv("LOOMCYCLE_MEM9_KEY", "env-value")

	def := config.MemoryBackend{Config: config.MemoryBackendConfig{APIKeyEnv: "LOOMCYCLE_MEM9_KEY"}}
	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, "")

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "alice"})
	_, err := resolver(ctx)
	if err == nil {
		t.Fatal("resolve with non-allowlisted env: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowlist") {
		t.Errorf("error = %v, want an allowlist refusal", err)
	}
	if strings.Contains(err.Error(), "env-value") {
		t.Errorf("error leaked the secret VALUE: %v", err)
	}
}

// TestMem9Cred_PerTenantEnvPattern pins that key_per_tenant resolves the
// per-tenant env var via env_pattern with {tenant_id} substituted from
// the run's user_id.
func TestMem9Cred_PerTenantEnvPattern(t *testing.T) {
	m := &Memory{EnvAllowlist: map[string]bool{"LOOMCYCLE_MEM9_TENANT_alice_API_KEY": true}}
	t.Setenv("LOOMCYCLE_MEM9_TENANT_alice_API_KEY", "alice-key")

	def := config.MemoryBackend{
		Config:          config.MemoryBackendConfig{APIKeyEnv: "LOOMCYCLE_MEM9_FALLBACK"},
		TenancyStrategy: config.MemoryBackendTenancy{Kind: "key_per_tenant", EnvPattern: "LOOMCYCLE_MEM9_TENANT_{tenant_id}_API_KEY"},
	}
	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, "")

	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "alice"})
	got, err := resolver(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "alice-key" {
		t.Errorf("resolved key = %q, want the per-tenant alice-key", got)
	}
}

// TestMem9Tenancy_SharedPrefixSubstitutesTenant pins that
// shared_key_with_prefix substitutes {tenant_id} into the key prefix.
func TestMem9Tenancy_SharedPrefixSubstitutesTenant(t *testing.T) {
	ts := config.MemoryBackendTenancy{Kind: "shared_key_with_prefix", PrefixPattern: "tenant-{tenant_id}::"}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "bob"})

	tenancy, tenant, err := resolveTenancy(ctx, ts)
	if err != nil {
		t.Fatalf("resolveTenancy: %v", err)
	}
	if tenancy.KeyPrefix != "tenant-bob::" {
		t.Errorf("KeyPrefix = %q, want tenant-bob::", tenancy.KeyPrefix)
	}
	if tenant != "bob" {
		t.Errorf("tenant = %q, want bob", tenant)
	}
}

// TestMem9Tenancy_SharedPrefixNoTenantIsError pins the isolation guard:
// shared_key_with_prefix on a run with no tenant must error (an empty
// prefix would collapse all tenants into one keyspace).
func TestMem9Tenancy_SharedPrefixNoTenantIsError(t *testing.T) {
	ts := config.MemoryBackendTenancy{Kind: "shared_key_with_prefix", PrefixPattern: "tenant-{tenant_id}::"}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{}) // no UserID

	_, _, err := resolveTenancy(ctx, ts)
	if err == nil {
		t.Fatal("resolveTenancy with no tenant: want error, got nil")
	}
}
