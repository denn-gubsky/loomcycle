package config

import (
	"strings"
	"testing"
)

// `!$.x == 1` parses as the absence of a key spelt `x == 1`, which every body
// meets: the event check it was meant to be passes everything. It, and a path
// whose key holds whitespace, = or a quote, are refused when a definition is
// written — by the yaml load among others — while the checks a snapshot
// restore re-runs still accept them.
func TestCheckWebhookAuthoring_RefusesARuleReadAsAKeyNoBodyHas(t *testing.T) {
	if !ShapeHolds([]string{"$.a", "!$.x == 1"}, map[string]any{"a": 1.0, "x": 1.0}) {
		t.Fatal("precondition: the rule is read as an absence that every body meets")
	}
	ok := Webhook{
		Provider: "github", Events: []string{"x"},
		EventShapes: map[string][]string{"x": {"$.a", "!$.b", `$.c == "d e"`, "$.n==1", "$.l[0].m"}},
		Filters:     []WebhookFilter{{Path: "$.ref", Op: "eq", Value: "main"}},
		DedupKey:    []string{"$.repository.id", "$.after"},
	}
	if err := CheckWebhookAuthoring(ok); err != nil {
		t.Fatalf("a definition with no such rule: %v", err)
	}
	for _, tc := range []struct {
		name string
		w    Webhook
		want string
	}{
		{"! with ==", Webhook{EventShapes: map[string][]string{"x": {"$.a", "!$.x == 1"}}}, "do not combine"},
		{"a spaced shape key", Webhook{EventShapes: map[string][]string{"x": {"$.a b"}}}, `event_shapes["x"]`},
		{"an = in a shape key", Webhook{EventShapes: map[string][]string{"x": {"$.a=1"}}}, "holding whitespace, = or a quote"},
		{"a quoted shape key", Webhook{EventShapes: map[string][]string{"x": {`$."a" == 1`}}}, "holding whitespace, = or a quote"},
		{"a spaced filter key", Webhook{Filters: []WebhookFilter{{Path: "$.re f", Op: "eq", Value: "x"}}}, "filters[0]"},
		{"a quoted dedup key", Webhook{DedupKey: []string{"$.id", "$.a'b"}}, "dedup_key[1]"},
	} {
		if err := CheckWebhookAuthoring(tc.w); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal mentioning %q", tc.name, err, tc.want)
		}
	}

	// The restore path's checks are unchanged.
	stored := Webhook{Provider: "github", Events: []string{"x"}, EventShapes: map[string][]string{"x": {"$.a", "!$.x == 1"}}}
	if err := CheckWebhookEvents(stored); err != nil {
		t.Errorf("a stored definition with the rule is refused on restore: %v", err)
	}

	// The yaml load refuses it.
	static := Webhook{
		Enabled: true, Delivery: "spawn", Agent: "a",
		Auth:     WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_S"},
		Provider: "github", Events: []string{"push"},
		EventShapes: map[string][]string{"push": {"$.ref", "!$.deleted == true"}},
	}
	if err := validateStaticWebhook("gh", static); err == nil || !strings.Contains(err.Error(), "webhooks.gh: event_shapes") {
		t.Errorf("the yaml load accepted the rule: %v", err)
	}
}
