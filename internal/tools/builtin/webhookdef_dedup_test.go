package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// dedup_key and its lifetime are checked when a definition is written.
func TestWebhookDefTool_DedupKeyIsCheckedWhenWritten(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	auth := `"auth":{"signing_secret_env":"LOOMCYCLE_S"}`

	ok := `{` + eventsBase + `,` + auth + `,"dedup_key":["$.repository.id","$.ref","$.after"],"dedup_ttl_seconds":86400}`
	if text, isErr := webhookCreate(t, tool, ctx, "w", ok); isErr {
		t.Fatal(text)
	}
	w := resolvedWebhook(t, tool, "w")
	if !reflect.DeepEqual(w.DedupKey, []string{"$.repository.id", "$.ref", "$.after"}) || w.DedupTTLSeconds != 86400 {
		t.Errorf("stored %v, %d", w.DedupKey, w.DedupTTLSeconds)
	}

	for _, tc := range []struct{ name, overlay, want string }{
		{"bad path", `{` + eventsBase + `,` + auth + `,"dedup_key":["$.commits[*].id"]}`, `dedup_key[0]`},
		{"the whole body", `{` + eventsBase + `,` + auth + `,"dedup_key":["$"]}`, `whole body`},
		{"a lifetime past 90 days", `{` + eventsBase + `,` + auth + `,"dedup_key":["$.ref"],"dedup_ttl_seconds":7776001}`, `90 days`},
		{"a lifetime without a key", `{` + eventsBase + `,` + auth + `,"dedup_ttl_seconds":60}`, `without dedup_key`},
		{"a key that is not a list", `{` + eventsBase + `,` + auth + `,"dedup_key":"$.ref"}`, `dedup_key`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w2", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}
}

// A fork replaces the list whole; [] removes it and its lifetime; a fork that
// leaves both out keeps the parent's.
func TestWebhookDefTool_AForkReplacesDedupKey(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	base := `{` + eventsBase + `,"auth":{"signing_secret_env":"LOOMCYCLE_S"},"dedup_key":["$.ref","$.after"],"dedup_ttl_seconds":3600}`
	if text, isErr := webhookCreate(t, tool, ctx, "w", base); isErr {
		t.Fatal(text)
	}
	fork := func(overlay string) (string, bool) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"w","overlay":`+overlay+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return res.Text, res.IsError
	}
	if text, isErr := fork(`{"agent":"a"}`); isErr {
		t.Fatal(text)
	}
	if w := resolvedWebhook(t, tool, "w"); len(w.DedupKey) != 2 || w.DedupTTLSeconds != 3600 {
		t.Fatalf("a fork without dedup_key kept %v, %d; want the parent's", w.DedupKey, w.DedupTTLSeconds)
	}
	if text, isErr := fork(`{"dedup_key":["$.after"]}`); isErr {
		t.Fatal(text)
	}
	if w := resolvedWebhook(t, tool, "w"); !reflect.DeepEqual(w.DedupKey, []string{"$.after"}) || w.DedupTTLSeconds != 3600 {
		t.Fatalf("after a fork naming one path: %v, %d", w.DedupKey, w.DedupTTLSeconds)
	}
	if text, isErr := fork(`{"dedup_key":[]}`); isErr {
		t.Fatalf("removing dedup_key was refused: %s", text)
	}
	if w := resolvedWebhook(t, tool, "w"); len(w.DedupKey) != 0 || w.DedupTTLSeconds != 0 {
		t.Fatalf("after dedup_key: []: %v, %d; want neither", w.DedupKey, w.DedupTTLSeconds)
	}
	if text, isErr := fork(`{"dedup_ttl_seconds":60}`); !isErr || !strings.Contains(text, "without dedup_key") {
		t.Errorf("a lifetime forked onto a definition without a key: %s", text)
	}
}

// A fork of a static definition that leaves dedup_key out keeps it and its
// lifetime.
func TestWebhookDefTool_AForkOfAStaticDefinitionKeepsItsDedupKey(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	static := tool.Cfg.Webhooks["gh-push"]
	static.DedupKey = []string{"$.ref", "$.after"}
	static.DedupTTLSeconds = 3600
	tool.Cfg.Webhooks["gh-push"] = static

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"rate_limit":{"requests_per_minute":10}}}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %s", err, res.Text)
	}
	row, err := tool.Store.WebhookDefGetActive(context.Background(), "", "gh-push")
	if err != nil {
		t.Fatal(err)
	}
	var w mergedWebhookDef
	if err := json.Unmarshal(row.Definition, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.DedupKey, static.DedupKey) || w.DedupTTLSeconds != 3600 {
		t.Errorf("the fork's dedup_key: %v, %d; want the static definition's", w.DedupKey, w.DedupTTLSeconds)
	}
}
