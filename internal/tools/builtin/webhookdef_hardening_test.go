package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Every field of a webhook in the operator's config has a place in the
// stored shape a runtime-authored definition is written in. user_tier had
// none, so a definition that set it lost it without a word. The other two
// shapes (the lookup's, the receiver's) are pinned against this one already.
func TestMergedWebhookDef_DriftDetection_VsConfig(t *testing.T) {
	configTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(config.Webhook{}))
	mergedTags := a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedWebhookDef{}))
	for tag := range configTags {
		if !mergedTags[tag] {
			t.Errorf("config.Webhook has json tag %q but mergedWebhookDef does not: a runtime-authored webhook cannot carry it", tag)
		}
	}
	// The stored shape's own extra fields are set by the server.
	serverOnly := map[string]bool{}
	for k := range webhookServerKeys {
		serverOnly[k] = true
	}
	for tag := range mergedTags {
		if !configTags[tag] && !serverOnly[tag] {
			t.Errorf("mergedWebhookDef has json tag %q that config.Webhook does not", tag)
		}
	}
}

func webhookCreate(t *testing.T, tool *WebhookDef, ctx context.Context, name, overlay string) (string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":`+overlay+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return res.Text, res.IsError
}

const webhookBase = `"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}`

// user_tier set on a runtime-authored webhook reaches the definition the
// receiver resolves, and a fork keeps it. A tier the server does not define
// is refused when the definition is written, not on every delivery.
func TestWebhookDefTool_UserTierIsKeptAndChecked(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	tool.Cfg.UserTiers = map[string]config.UserTier{"free": {}, "pro": {}}

	if text, isErr := webhookCreate(t, tool, ctx, "tiered", `{`+webhookBase+`,"user_tier":"pro"}`); isErr {
		t.Fatalf("create: %s", text)
	}
	if w := resolvedWebhook(t, tool, "tiered"); w.UserTier != "pro" {
		t.Fatalf("resolved user_tier = %q, want pro", w.UserTier)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"tiered","overlay":{"metadata":{"x":"y"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if w := resolvedWebhook(t, tool, "tiered"); w.UserTier != "pro" {
		t.Errorf("after a fork that does not name it, user_tier = %q, want pro kept", w.UserTier)
	}
	if text, isErr := webhookCreate(t, tool, ctx, "typo", `{`+webhookBase+`,"user_tier":"proo"}`); !isErr || !strings.Contains(text, "not one of this server's user tiers") {
		t.Errorf("an unknown tier: %s, want it refused", text)
	}
}

// An overlay is read as written. A key no field takes, a key repeated, a key
// matched only by ignoring its case, and a key the server sets are refused,
// where they were dropped or ignored without a word. A hook's keys in their
// documented spelling, which the hook type does not tag, still pass.
func TestWebhookDefTool_AnOverlayIsReadAsWritten(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	for _, tc := range []struct{ name, overlay, want string }{
		{"misspelt", `{` + webhookBase + `,"payload_maping":{"a":"$.a"}}`, `unknown key "payload_maping"`},
		{"from a newer server", `{` + webhookBase + `,"events":["push"]}`, `unknown key "events"`},
		{"case only", `{"enabled":true,"delivery":"spawn","Agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}`, `only by ignoring its case`},
		{"repeated", `{` + webhookBase + `,"agent":"b"}`, `written twice`},
		{"nested", `{"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S","secret":"x"}}`, `unknown key "secret"`},
		{"server-set", `{` + webhookBase + `,"operator_key_restricted":false}`, `"operator_key_restricted" cannot be set`},
		{"server-set", `{` + webhookBase + `,"isolated":false}`, `"isolated" cannot be set`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}

	ok := `{` + webhookBase + `,"on_complete":[{"kind":"channel.publish","channel":"done","payload":{"Any":"Case"}}],"metadata":{"Free":"form"}}`
	if text, isErr := webhookCreate(t, tool, ctx, "hooked", ok); isErr {
		t.Errorf("a definition in documented spelling: %s", text)
	}
}

// body_size_limit_bytes above the server's request limit is refused when it
// is written: a delivery is never read past that limit.
func TestWebhookDefTool_BodySizeLimitIsCappedByTheServer(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	tool.Cfg.Env.MaxRequestBytes = 1 << 20

	if text, isErr := webhookCreate(t, tool, ctx, "big", `{`+webhookBase+`,"body_size_limit_bytes":2097152}`); !isErr || !strings.Contains(text, "above this server's request limit") {
		t.Errorf("a limit above the server's: %s, want it refused", text)
	}
	if text, isErr := webhookCreate(t, tool, ctx, "fits", `{`+webhookBase+`,"body_size_limit_bytes":1048576}`); isErr {
		t.Errorf("a limit at the server's: %s", text)
	}
}
