package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memoryBackendDefFixture builds a MemoryBackendDef tool over in-memory
// SQLite + a stub Config with one yaml template (a mem9 backend). RFC I
// MR-3a / mirrors webhookDefFixture.
func memoryBackendDefFixture(t *testing.T) (*MemoryBackendDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		MemoryBackends: map[string]config.MemoryBackend{
			"primary": {
				Kind: "mem9",
				Config: config.MemoryBackendConfig{
					BaseURL:   "https://mem9.example.com",
					APIKeyEnv: "LOOMCYCLE_MEM9_KEY",
				},
			},
		},
	}
	tool := &MemoryBackendDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "memory-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "memory-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestMemoryBackendDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"primary","overlay":{"kind":"inprocess"}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.MemoryBackends") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateInprocess(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"local","overlay":{"kind":"inprocess"},"description":"local backend"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "local" {
		t.Errorf("name = %v, want local", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestMemoryBackendDefTool_CreateMem9(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"remote","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"}}}`))
	if res.IsError {
		t.Fatalf("create mem9: %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateStampsCanonicalName(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	// Overlay name diverges from the key; the stamped name must win.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"canon","overlay":{"name":"divergent","kind":"inprocess"}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def := decodeResult(t, res.Text)["definition"].(map[string]any)
	if def["name"] != "canon" {
		t.Errorf("stamped name = %v, want canon (registry key, not overlay)", def["name"])
	}
}

func TestMemoryBackendDefTool_CreateRefusesMem9WithoutBaseURL(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"nobase","overlay":{"kind":"mem9","config":{"api_key_env":"LOOMCYCLE_M_KEY"}}}`))
	if !res.IsError {
		t.Fatalf("mem9 without base_url should refuse")
	}
	if !strings.Contains(res.Text, "config.base_url") {
		t.Errorf("refusal should mention config.base_url; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateRefusesMem9WithoutAPIKeyEnv(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"nokey","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com"}}}`))
	if !res.IsError {
		t.Fatalf("mem9 without api_key_env should refuse")
	}
	if !strings.Contains(res.Text, "config.api_key_env") {
		t.Errorf("refusal should mention config.api_key_env; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateRefusesBadAPIKeyEnvName(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badenv","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"lower-case"}}}`))
	if !res.IsError {
		t.Fatalf("malformed api_key_env should refuse")
	}
	if !strings.Contains(res.Text, "valid env-var name") {
		t.Errorf("refusal should mention env-var name; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateRefusesUnknownKind(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badkind","overlay":{"kind":"redis"}}`))
	if !res.IsError {
		t.Fatalf("unknown kind should refuse")
	}
	if !strings.Contains(res.Text, "unknown kind") {
		t.Errorf("refusal should name the bad kind; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateRefusesTenancyPatternWithoutTenantID(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badtenant","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_KEY_STATIC"}}}`))
	if !res.IsError {
		t.Fatalf("key_per_tenant env_pattern without {tenant_id} should refuse")
	}
	if !strings.Contains(res.Text, "{tenant_id}") {
		t.Errorf("refusal should mention {tenant_id}; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_CreateRefusesUnknownFallback(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badfallback","overlay":{"kind":"inprocess","fallback_on_error":"mem9"}}`))
	if !res.IsError {
		t.Fatalf("fallback_on_error=mem9 should refuse")
	}
	if !strings.Contains(res.Text, "fallback_on_error") {
		t.Errorf("refusal should mention fallback_on_error; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	// Bootstrap v1 from yaml + fork v2 rotating only the api_version.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"primary","overlay":{"config":{"api_version":"v2"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if v := out["version"].(float64); v != 2 {
		t.Errorf("version = %v, want 2 (v1 bootstrap + v2 fork)", v)
	}
	if out["promoted"].(bool) != true {
		t.Errorf("fork default promote = false; want true")
	}
	def := out["definition"].(map[string]any)
	// base_url survived from the template; only the api_version changed.
	cfgBlock := def["config"].(map[string]any)
	if cfgBlock["base_url"] != "https://mem9.example.com" {
		t.Errorf("fork lost template base_url; got %v", cfgBlock["base_url"])
	}
	if cfgBlock["api_version"] != "v2" {
		t.Errorf("api_version not rotated; got %v", cfgBlock["api_version"])
	}
	if def["name"] != "primary" {
		t.Errorf("fork lost stamped name; got %v", def["name"])
	}
}

func TestMemoryBackendDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"kind":"inprocess"}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestMemoryBackendDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	ctx = tools.WithMemoryBackendDefPolicy(ctx, tools.MemoryBackendDefPolicyValue{
		Scopes: []string{"named:adhoc"},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"kind":"inprocess"}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"kind":"inprocess"}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestMemoryBackendDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-be","overlay":{"kind":"inprocess"}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":true}`))
	if res.IsError {
		t.Fatalf("retire: %s", res.Text)
	}
	if decodeResult(t, res.Text)["retired"].(bool) != true {
		t.Errorf("retired = false, want true")
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"retire","def_id":"`+defID+`","retired":false}`))
	if res.IsError {
		t.Fatalf("un-retire: %s", res.Text)
	}
	if decodeResult(t, res.Text)["retired"].(bool) != false {
		t.Errorf("retired = true, want false")
	}
}

func TestMemoryBackendDefTool_GetRoundTrip(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"get-be","overlay":{"kind":"inprocess"}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["name"] != "get-be" {
		t.Errorf("get returned wrong name")
	}
}

func TestMemoryBackendDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := memoryBackendDefFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi-be","overlay":{"kind":"inprocess"}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi-be"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedMemoryBackendDef_DriftDetection_VsLookupSubstrate pins
// json-tag parity between mergedMemoryBackendDef (substrate-write) and
// lookup.SubstrateMemoryBackendDef (substrate-read). RFC I MR-3a.
func TestMergedMemoryBackendDef_DriftDetection_VsLookupSubstrate(t *testing.T) {
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedMemoryBackendDef{}))
	substrateTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateMemoryBackendDef{}))

	for tag := range mergedTags {
		if !substrateTags[tag] {
			t.Errorf("mergedMemoryBackendDef has json tag %q but lookup.SubstrateMemoryBackendDef does not — mirror it on the lookup side", tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateMemoryBackendDef has json tag %q but mergedMemoryBackendDef does not — substrate-write is the source-of-truth shape", tag)
		}
	}
}
