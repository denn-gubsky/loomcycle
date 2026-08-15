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

// documentSourceDefFixture builds a DocumentSourceDef tool over in-memory
// SQLite + a stub Config with one yaml template. The template carries a
// populated `config` block (with a valid base_url — required for a document
// source) so the fork/bootstrap tests can assert the whole definition
// round-trips. RFC CE / mirrors memoryBackendDefFixture.
func documentSourceDefFixture(t *testing.T) (*DocumentSourceDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		DocumentSources: map[string]config.DocumentSource{
			"primary": {
				Config: config.DocumentSourceConfig{
					BaseURL:   "https://docs.example.com",
					APIKeyEnv: "LOOMCYCLE_DOCS_KEY",
				},
			},
		},
	}
	tool := &DocumentSourceDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "document-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithDocumentSourceDefPolicy(ctx, tools.DocumentSourceDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "document-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestDocumentSourceDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"primary","overlay":{"config":{"base_url":"https://a.example.com"}}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.DocumentSources") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestDocumentSourceDefTool_CreateHappyPath(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"local","overlay":{"config":{"base_url":"https://local.example.com"}},"description":"local source"}`))
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

func TestDocumentSourceDefTool_CreateStampsCanonicalName(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	// Overlay name diverges from the key; the stamped name must win.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"canon","overlay":{"name":"divergent","config":{"base_url":"https://c.example.com"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	def := decodeResult(t, res.Text)["definition"].(map[string]any)
	if def["name"] != "canon" {
		t.Errorf("stamped name = %v, want canon (registry key, not overlay)", def["name"])
	}
}

func TestDocumentSourceDefTool_CreateRefusesMissingBaseURL(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"nobase","overlay":{"config":{"api_version":"v1"}}}`))
	if !res.IsError || !strings.Contains(res.Text, "base_url is required") {
		t.Fatalf("create without base_url should be refused; got %s", res.Text)
	}
}

func TestDocumentSourceDefTool_CreateRefusesNonHTTPBaseURL(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badurl","overlay":{"config":{"base_url":"file:///etc/passwd"}}}`))
	if !res.IsError {
		t.Fatalf("non-http base_url should refuse")
	}
	if !strings.Contains(res.Text, "base_url") {
		t.Errorf("refusal should name base_url; got %s", res.Text)
	}
}

func TestDocumentSourceDefTool_CreateRefusesUnknownTenancyKind(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	// shared_key_with_prefix is a valid MemoryBackend tenancy but NOT a
	// document source one — the document proxy has no key-prefix semantics.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badtenant","overlay":{"config":{"base_url":"https://p.example.com"},"tenancy_strategy":{"kind":"shared_key_with_prefix"}}}`))
	if !res.IsError {
		t.Fatalf("unknown tenancy kind should refuse")
	}
	if !strings.Contains(res.Text, "key_per_tenant") {
		t.Errorf("refusal should name the allowed kind; got %s", res.Text)
	}
}

func TestDocumentSourceDefTool_CreateRefusesTenancyPatternWithoutTenantID(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badpattern","overlay":{"config":{"base_url":"https://p.example.com"},"tenancy_strategy":{"kind":"key_per_tenant","env_pattern":"LOOMCYCLE_KEY_STATIC"}}}`))
	if !res.IsError {
		t.Fatalf("key_per_tenant env_pattern without {tenant_id} should refuse")
	}
	if !strings.Contains(res.Text, "{tenant_id}") {
		t.Errorf("refusal should mention {tenant_id}; got %s", res.Text)
	}
}

// TestDocumentSourceDefTool_RefusesUnsafeAPIKeyEnv pins that api_key_env is
// validated at AUTHORING time: its value is SENT to the def-supplied base_url,
// so an infra secret or non-allowlisted var must never be storable. Paired
// with a base_url the author controls, `api_key_env: LOOMCYCLE_AUTH_TOKEN` is a
// one-request exfiltration of the operator bearer.
func TestDocumentSourceDefTool_RefusesUnsafeAPIKeyEnv(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		refuse    bool
		why       string
	}{
		{"operator bearer", "LOOMCYCLE_AUTH_TOKEN", true,
			"loomcycle's own admin credential — allowed by the LOOMCYCLE_ prefix, denied by the infra-secret set"},
		{"postgres dsn", "LOOMCYCLE_PG_DSN", true, "loomcycle's own database credential"},
		{"arbitrary host var", "AWS_SECRET_ACCESS_KEY", true, "not allowlisted"},
		{"path", "PATH", true, "not allowlisted"},
		{"loomcycle-scoped key", "LOOMCYCLE_DOCS_KEY", false, "the intended shape"},
		{"known third-party", "BRAVE_API_KEY", false, "explicitly allowlisted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ctx, cleanup := documentSourceDefFixture(t)
			defer cleanup()
			body := `{"op":"create","name":"d_` + tc.name[:3] + `","overlay":{` +
				`"config":{"base_url":"https://p.example.com","api_key_env":"` + tc.env + `"}}}`
			res, _ := tool.Execute(ctx, json.RawMessage(body))
			if tc.refuse && !res.IsError {
				t.Fatalf("api_key_env=%s was accepted (%s); got %s", tc.env, tc.why, res.Text)
			}
			if !tc.refuse && res.IsError {
				t.Fatalf("api_key_env=%s was refused (%s); got %s", tc.env, tc.why, res.Text)
			}
			if tc.refuse && !strings.Contains(res.Text, "api_key_env") {
				t.Errorf("the refusal does not name the offending field: %s", res.Text)
			}
		})
	}
}

func TestDocumentSourceDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
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
	if cfgBlock["base_url"] != "https://docs.example.com" {
		t.Errorf("fork lost template base_url; got %v", cfgBlock["base_url"])
	}
	if cfgBlock["api_version"] != "v2" {
		t.Errorf("api_version not rotated; got %v", cfgBlock["api_version"])
	}
	if def["name"] != "primary" {
		t.Errorf("fork lost stamped name; got %v", def["name"])
	}
}

func TestDocumentSourceDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	ctx = tools.WithDocumentSourceDefPolicy(ctx, tools.DocumentSourceDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"config":{"base_url":"https://x.example.com"}}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestDocumentSourceDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	ctx = tools.WithDocumentSourceDefPolicy(ctx, tools.DocumentSourceDefPolicyValue{
		Scopes: []string{"named:adhoc"},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"config":{"base_url":"https://a.example.com"}}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"config":{"base_url":"https://o.example.com"}}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestDocumentSourceDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-ds","overlay":{"config":{"base_url":"https://r.example.com"}}}`))
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

func TestDocumentSourceDefTool_GetRoundTrip(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"get-ds","overlay":{"config":{"base_url":"https://g.example.com"}}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["name"] != "get-ds" {
		t.Errorf("get returned wrong name")
	}
}

func TestDocumentSourceDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := documentSourceDefFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi-ds","overlay":{"config":{"base_url":"https://m.example.com"}}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi-ds"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedDocumentSourceDef_DriftDetection_VsLookupSubstrate pins json-tag
// parity between mergedDocumentSourceDef (substrate-write) and
// lookup.SubstrateDocumentSourceDef (substrate-read). RFC CE.
func TestMergedDocumentSourceDef_DriftDetection_VsLookupSubstrate(t *testing.T) {
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedDocumentSourceDef{}))
	substrateTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateDocumentSourceDef{}))

	for tag := range mergedTags {
		if !substrateTags[tag] {
			t.Errorf("mergedDocumentSourceDef has json tag %q but lookup.SubstrateDocumentSourceDef does not — mirror it on the lookup side", tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateDocumentSourceDef has json tag %q but mergedDocumentSourceDef does not — substrate-write is the source-of-truth shape", tag)
		}
	}
}
