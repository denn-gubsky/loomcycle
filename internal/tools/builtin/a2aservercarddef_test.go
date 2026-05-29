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

// a2aServerCardDefFixture builds an A2AServerCardDef tool over in-memory
// SQLite + a stub Config with one yaml template. Returns a ctx with a
// permissive policy (scopes=[any]); per-test code overrides for
// scope-specific cases.
func a2aServerCardDefFixture(t *testing.T) (*A2AServerCardDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{
			"jobs-card": {
				Name:         "jobs-card",
				Description:  "exposes the jobs agents",
				Provider:     config.A2AServerCardProvider{Organization: "Acme"},
				Capabilities: config.A2AServerCardCaps{Streaming: true},
				ExposedAgents: []config.A2AExposedAgent{
					{AgentName: "job-search", SkillID: "search"},
				},
				SecuritySchemes: []config.A2ASecurityScheme{{Kind: "http", Scheme: "bearer"}},
			},
		},
	}
	tool := &A2AServerCardDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "a2a-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "a2a-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestA2AServerCardDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"jobs-card","overlay":{"name":"x","exposed_agents":[{"agent_name":"a"}]}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.A2AServerCards") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_CreateNewName(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-card","overlay":{"name":"adhoc-card","exposed_agents":[{"agent_name":"writer"}]},"description":"manual card"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "adhoc-card" {
		t.Errorf("name = %v, want adhoc-card", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestA2AServerCardDefTool_CreateRefusesMissingName(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	// name field on the envelope is required, AND the definition's name
	// must be non-empty per validateA2AServerCardDef. Here the envelope
	// name is set but the overlay omits the definition name + exposed.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"no-def-name","overlay":{"exposed_agents":[{"agent_name":"a"}]}}`))
	if !res.IsError {
		t.Fatalf("create without definition name should refuse")
	}
	if !strings.Contains(res.Text, "name: required") {
		t.Errorf("refusal should mention name required; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_CreateRefusesEmptyExposedAgents(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"noexp","overlay":{"name":"noexp"}}`))
	if !res.IsError {
		t.Fatalf("create without exposed_agents should refuse")
	}
	if !strings.Contains(res.Text, "exposed_agents") {
		t.Errorf("refusal should mention exposed_agents; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_CreateRefusesBlankExposedAgentName(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"blankexp","overlay":{"name":"blankexp","exposed_agents":[{"skill_id":"x"}]}}`))
	if !res.IsError {
		t.Fatalf("exposed agent with no agent_name should refuse")
	}
	if !strings.Contains(res.Text, "agent_name required") {
		t.Errorf("refusal should name the missing agent_name; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_CreateRefusesBadSecuritySchemeKind(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badsec","overlay":{"name":"badsec","exposed_agents":[{"agent_name":"a"}],"security_schemes":[{"kind":"basic"}]}}`))
	if !res.IsError {
		t.Fatalf("unknown security scheme kind should refuse")
	}
	if !strings.Contains(res.Text, "unknown kind") {
		t.Errorf("refusal should name the bad kind; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_CreateRefusesBadSignKeyEnvShape(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badenv","overlay":{"name":"badenv","exposed_agents":[{"agent_name":"a"}],"sign_with_key_env":"not a var"}}`))
	if !res.IsError {
		t.Fatalf("malformed sign_with_key_env should refuse")
	}
	if !strings.Contains(res.Text, "sign_with_key_env") {
		t.Errorf("refusal should mention sign_with_key_env; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	// First fork bootstraps v1 from yaml, then forks v2 with overlay.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"jobs-card","overlay":{"description":"tenant-acme"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "jobs-card" {
		t.Errorf("name = %v, want jobs-card", out["name"])
	}
	if v := out["version"].(float64); v != 2 {
		t.Errorf("version = %v, want 2 (v1 bootstrap + v2 fork)", v)
	}
	if out["promoted"].(bool) != true {
		t.Errorf("fork default promote = false; want true")
	}
	// The bootstrapped name + exposed_agents survived from the yaml
	// template (overlay only touched description).
	def := out["definition"].(map[string]any)
	if def["name"] != "jobs-card" {
		t.Errorf("forked definition lost template name; got %v", def["name"])
	}
}

func TestA2AServerCardDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"name":"x","exposed_agents":[{"agent_name":"a"}]}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestA2AServerCardDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	ctx = tools.WithA2AServerCardDefPolicy(ctx, tools.A2AServerCardDefPolicyValue{
		Scopes: []string{"named:adhoc-card"},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc-card","overlay":{"name":"adhoc-card","exposed_agents":[{"agent_name":"a"}]}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"name":"other","exposed_agents":[{"agent_name":"a"}]}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestA2AServerCardDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-card","overlay":{"name":"retire-card","exposed_agents":[{"agent_name":"a"}]}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
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

func TestA2AServerCardDefTool_GetByDefID(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"get-card","overlay":{"name":"get-card","exposed_agents":[{"agent_name":"a"}]}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["name"] != "get-card" {
		t.Errorf("get returned wrong name")
	}
}

func TestA2AServerCardDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := a2aServerCardDefFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi-card","overlay":{"name":"multi-card","exposed_agents":[{"agent_name":"a"}]}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi-card"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedA2AServerCardDef_DriftDetection_VsLookupSubstrate pins
// json-tag parity between mergedA2AServerCardDef (substrate-write) and
// lookup.SubstrateA2AServerCardDef (substrate-read). A field added to
// either side without the matching addition fails this test instead of
// silently dropping at the JSON boundary. No exempt set: the two shapes
// are 1:1 (unlike ScheduleDef, A2AServerCardDef has no write-only
// credentials field).
func TestMergedA2AServerCardDef_DriftDetection_VsLookupSubstrate(t *testing.T) {
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedA2AServerCardDef{}))
	substrateTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateA2AServerCardDef{}))

	for tag := range mergedTags {
		if !substrateTags[tag] {
			t.Errorf("mergedA2AServerCardDef has json tag %q but lookup.SubstrateA2AServerCardDef does not — mirror it on the lookup side", tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateA2AServerCardDef has json tag %q but mergedA2AServerCardDef does not — substrate-write is the source-of-truth shape", tag)
		}
	}
}

func a2aBuiltinJSONTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}
