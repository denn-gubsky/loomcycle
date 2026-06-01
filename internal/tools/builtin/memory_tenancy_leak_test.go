package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestResolveTenancy_SharedPrefixEmptyOrTokenlessRejected pins the HIGH
// cross-tenant leak fix at the runtime backstop: shared_key_with_prefix
// with an empty OR token-less prefix_pattern must error, never resolve to an
// empty KeyPrefix (which collapses every tenant into one Mem9 keyspace).
//
// Regression-grade: pre-fix the empty/token-less branch fell through and
// returned mem9.Tenancy{KeyPrefix: ""} with no error.
func TestResolveTenancy_SharedPrefixEmptyOrTokenlessRejected(t *testing.T) {
	// RFC L: tenancy keys on the authoritative TenantID (not UserID).
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "alice"})
	for _, pat := range []string{"", "static-no-token::", "tenant-::"} {
		ts := config.MemoryBackendTenancy{Kind: "shared_key_with_prefix", PrefixPattern: pat}
		tenancy, _, err := resolveTenancy(ctx, ts)
		if err == nil {
			t.Errorf("prefix_pattern %q: resolveTenancy returned KeyPrefix=%q with no error; want a refusal (empty prefix = cross-tenant leak)", pat, tenancy.KeyPrefix)
		}
	}
	// Sanity: a valid token-bearing pattern still resolves.
	ts := config.MemoryBackendTenancy{Kind: "shared_key_with_prefix", PrefixPattern: "tenant-{tenant_id}::"}
	tenancy, _, err := resolveTenancy(ctx, ts)
	if err != nil || tenancy.KeyPrefix != "tenant-alice::" {
		t.Fatalf("valid pattern: got (%q, %v), want (tenant-alice::, nil)", tenancy.KeyPrefix, err)
	}
}

// TestMemoryBackendDefTool_RefusesEmptySharedPrefix pins the authoring-time
// guard: a MemoryBackendDef with shared_key_with_prefix + empty
// prefix_pattern must be refused at create (pre-fix the validator's
// `PrefixPattern != "" &&` precondition let an empty pattern pass).
func TestMemoryBackendDefTool_RefusesEmptySharedPrefix(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"leaky","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"},"tenancy_strategy":{"kind":"shared_key_with_prefix"}}}`))
	if !res.IsError {
		t.Fatal("shared_key_with_prefix with empty prefix_pattern should be refused")
	}
	if !strings.Contains(res.Text, "{tenant_id}") {
		t.Errorf("refusal should mention {tenant_id}; got %s", res.Text)
	}
}

// TestMemoryBackendDefTool_RefusesNonHTTPBaseURL pins the SSRF defense-in-
// depth: a mem9 base_url that is not an http(s) URL with a host is refused
// at create (base_url is model-authorable via a fork overlay, and the Mem9
// client sends the allowlisted X-API-Key to whatever host it names).
func TestMemoryBackendDefTool_RefusesNonHTTPBaseURL(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	for _, bad := range []string{"file:///etc/passwd", "ftp://x/", "not a url", "https://"} {
		body := `{"op":"create","name":"bad","overlay":{"kind":"mem9","config":{"base_url":"` + bad + `","api_key_env":"LOOMCYCLE_M_KEY"}}}`
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if !res.IsError {
			t.Errorf("base_url %q should be refused", bad)
		}
	}
}
