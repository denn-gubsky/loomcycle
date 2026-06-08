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

// webhookDefFixture builds a WebhookDef tool over in-memory SQLite + a
// stub Config with one yaml template (a spawn-delivery hmac webhook).
// Mirrors a2aAgentDefFixture (RFC H WH-2).
func webhookDefFixture(t *testing.T) (*WebhookDef, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Webhooks: map[string]config.Webhook{
			"gh-push": {
				Enabled:  true,
				Delivery: "spawn",
				Agent:    "intake",
				Auth: config.WebhookAuth{
					Kind:             "hmac",
					Algorithm:        "sha256",
					SigningSecretEnv: "LOOMCYCLE_WH_SECRET",
				},
			},
		},
	}
	tool := &WebhookDef{
		Store:               s,
		Cfg:                 cfg,
		MaxDefinitionBytes:  131072,
		MaxDescriptionBytes: 8192,
	}
	ctx := tools.WithAgentName(context.Background(), "webhook-orchestrator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{
		Scopes:   []string{"any"},
		SelfName: "webhook-orchestrator",
	})
	return tool, ctx, func() { _ = s.Close() }
}

func TestWebhookDefTool_CreateRefusedOverStaticName(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"gh-push","overlay":{"delivery":"spawn","agent":"x","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("create over static name should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "static cfg.Webhooks") {
		t.Errorf("refusal should mention static; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateSpawnHmac(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}},"description":"new hook"}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["name"] != "adhoc" {
		t.Errorf("name = %v, want adhoc", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

// TestWebhookDefTool_CreateStampsTenant is the F30 regression: a runtime
// webhook created under a principal whose tenant is "default" must persist
// that tenant in its definition, so the spawn path resolves a dynamic agent
// under the SAME tenant the AgentDef substrate stored it. Before the fix the
// tenant was left "" → `lookup.Webhook` returned TenantID="" → buildRunInput
// spawned under "" → "unknown agent". Asserts through the resolution path the
// webhook receiver actually uses.
func TestWebhookDefTool_CreateStampsTenant(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	// Re-stamp the run identity with a tenant (the legacy token resolves "default").
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "default"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	wd, ok := lookup.Webhook(ctx, tool.Store, tool.Cfg, "adhoc")
	if !ok {
		t.Fatalf("lookup.Webhook(adhoc): not found")
	}
	if wd.TenantID != "default" {
		t.Errorf("resolved TenantID = %q, want %q (F30: un-stamped tenant → unknown agent at spawn)", wd.TenantID, "default")
	}
}

// TestWebhookDefTool_ForkStampsTenant — the fork twin of the F30 regression.
// Forks a DYNAMIC webhook (not in yaml; lookup.Webhook is yaml-first, so a
// forked yaml name would be shadowed by the static def). The parent is created
// under no tenant, so the FORK — not the create — is what must stamp it.
func TestWebhookDefTool_ForkStampsTenant(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	// Create the dynamic parent under the fixture ctx (no tenant → TenantID "").
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}}}`)); res.IsError {
		t.Fatalf("create parent: %s", res.Text)
	}
	// Fork it under a tenant.
	tctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: "default"})
	if res, _ := tool.Execute(tctx, json.RawMessage(`{"op":"fork","name":"adhoc","overlay":{"agent":"intake2"}}`)); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	wd, ok := lookup.Webhook(tctx, tool.Store, tool.Cfg, "adhoc")
	if !ok {
		t.Fatalf("lookup.Webhook(adhoc): not found")
	}
	if wd.TenantID != "default" {
		t.Errorf("forked TenantID = %q, want %q", wd.TenantID, "default")
	}
}

func TestWebhookDefTool_CreateChannelDelivery(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"chan-hook","overlay":{"delivery":"channel","channel":"_system/webhooks","auth":{"kind":"bearer","bearer_token_env":"LOOMCYCLE_TOKEN"}}}`))
	if res.IsError {
		t.Fatalf("create channel delivery: %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesSpawnWithoutAgent(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"noagent","overlay":{"delivery":"spawn","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("spawn without agent should refuse")
	}
	if !strings.Contains(res.Text, "requires agent") {
		t.Errorf("refusal should mention requires agent; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesChannelWithAgent(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"both","overlay":{"delivery":"channel","channel":"c","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("channel with agent should refuse")
	}
	if !strings.Contains(res.Text, "forbids agent") {
		t.Errorf("refusal should mention forbids agent; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesChannelWithCredentials(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"chan-cred","overlay":{"delivery":"channel","channel":"c","auth":{"signing_secret_env":"LOOMCYCLE_S"},"user_credentials_from_env":{"peer":"LOOMCYCLE_PEER_TOKEN"}}}`))
	if !res.IsError {
		t.Fatalf("channel with user_credentials_from_env should refuse (RFC H Decision 11)")
	}
	if !strings.Contains(res.Text, "user_credentials_from_env") {
		t.Errorf("refusal should mention user_credentials_from_env; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesChannelWithCredentialPayloadKey(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"chan-credkey","overlay":{"delivery":"channel","channel":"c","auth":{"signing_secret_env":"LOOMCYCLE_S"},"payload_mapping":{"user_credentials.token":"$.token"}}}`))
	if !res.IsError {
		t.Fatalf("channel with user_credentials.* payload key should refuse (RFC H Decision 11)")
	}
	if !strings.Contains(res.Text, "user_credentials.token") {
		t.Errorf("refusal should name the offending key; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesHmacWithoutSecretEnv(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"nosecret","overlay":{"delivery":"spawn","agent":"a","auth":{"kind":"hmac"}}}`))
	if !res.IsError {
		t.Fatalf("hmac without signing_secret_env should refuse")
	}
	if !strings.Contains(res.Text, "signing_secret_env") {
		t.Errorf("refusal should mention signing_secret_env; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesBearerWithoutTokenEnv(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"notoken","overlay":{"delivery":"spawn","agent":"a","auth":{"kind":"bearer"}}}`))
	if !res.IsError {
		t.Fatalf("bearer without bearer_token_env should refuse")
	}
	if !strings.Contains(res.Text, "bearer_token_env") {
		t.Errorf("refusal should mention bearer_token_env; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesBadEnvVarName(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badenv","overlay":{"delivery":"spawn","agent":"a","auth":{"kind":"hmac","signing_secret_env":"lower-case"}}}`))
	if !res.IsError {
		t.Fatalf("malformed signing_secret_env should refuse")
	}
	if !strings.Contains(res.Text, "valid env-var name") {
		t.Errorf("refusal should mention env-var name; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesUnknownDelivery(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"baddelivery","overlay":{"delivery":"carrier-pigeon","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("unknown delivery should refuse")
	}
	if !strings.Contains(res.Text, "unknown delivery") {
		t.Errorf("refusal should name the bad delivery; got %s", res.Text)
	}
}

func TestWebhookDefTool_CreateRefusesBadSyncTimeout(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"badsync","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"},"sync_response":{"enabled":true,"timeout_ms":120000}}}`))
	if !res.IsError {
		t.Fatalf("out-of-range sync_response.timeout_ms should refuse")
	}
	if !strings.Contains(res.Text, "timeout_ms") {
		t.Errorf("refusal should mention timeout_ms; got %s", res.Text)
	}
}

func TestWebhookDefTool_ForkBootstrapsTemplate(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	// Bootstrap v1 from yaml + fork v2 rotating only the rate limit.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"rate_limit":{"requests_per_minute":120}}}`))
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
	// agent survived from the template; only the rate limit changed.
	if def["agent"] != "intake" {
		t.Errorf("fork lost template agent; got %v", def["agent"])
	}
	rl := def["rate_limit"].(map[string]any)
	if rl["requests_per_minute"].(float64) != 120 {
		t.Errorf("rate limit not rotated; got %v", rl["requests_per_minute"])
	}
}

// TestWebhookDefTool_ForkFlipsDeliveryMode verifies the overlay can flip
// a webhook from spawn to channel without leaving the stale agent that
// would trip the channel-forbids-agent refusal.
func TestWebhookDefTool_ForkFlipsDeliveryMode(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"delivery":"channel","channel":"_system/webhooks"}}`))
	if res.IsError {
		t.Fatalf("fork flip: %s", res.Text)
	}
	def := decodeResult(t, res.Text)["definition"].(map[string]any)
	if _, ok := def["agent"]; ok {
		t.Errorf("agent should be cleared after flipping to channel; got %v", def["agent"])
	}
	if def["channel"] != "_system/webhooks" {
		t.Errorf("channel = %v, want _system/webhooks", def["channel"])
	}
}

func TestWebhookDefTool_NoScopesIsDefaultDeny(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"x","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("empty scopes should default-deny")
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("refusal should mention default-deny; got %s", res.Text)
	}
}

func TestWebhookDefTool_NamedScope(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	ctx = tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{
		Scopes: []string{"named:adhoc"},
	})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"adhoc","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if res.IsError {
		t.Fatalf("named scope should allow matching name; got %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"other","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	if !res.IsError {
		t.Fatalf("named scope should refuse non-matching name")
	}
}

func TestWebhookDefTool_RetireRoundTrip(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"retire-hook","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
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

func TestWebhookDefTool_GetRoundTrip(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"get-hook","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	defID := decodeResult(t, res.Text)["def_id"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	if decodeResult(t, res.Text)["name"] != "get-hook" {
		t.Errorf("get returned wrong name")
	}
}

func TestWebhookDefTool_ListReturnsVersions(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		op := `create`
		if i > 0 {
			op = `fork`
		}
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"`+op+`","name":"multi-hook","overlay":{"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","name":"multi-hook"}`))
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	versions := decodeResult(t, res.Text)["versions"].([]any)
	if len(versions) != 3 {
		t.Errorf("got %d versions, want 3", len(versions))
	}
}

// TestMergedWebhookDef_DriftDetection_VsLookupSubstrate pins json-tag
// parity between mergedWebhookDef (substrate-write) and
// lookup.SubstrateWebhookDef (substrate-read). RFC H WH-2.
func TestMergedWebhookDef_DriftDetection_VsLookupSubstrate(t *testing.T) {
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedWebhookDef{}))
	substrateTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateWebhookDef{}))

	for tag := range mergedTags {
		if !substrateTags[tag] {
			t.Errorf("mergedWebhookDef has json tag %q but lookup.SubstrateWebhookDef does not — mirror it on the lookup side", tag)
		}
	}
	for tag := range substrateTags {
		if !mergedTags[tag] {
			t.Errorf("lookup.SubstrateWebhookDef has json tag %q but mergedWebhookDef does not — substrate-write is the source-of-truth shape", tag)
		}
	}
}
