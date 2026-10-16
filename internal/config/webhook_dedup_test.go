package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCheckWebhookDedupKey_RefusesWhatCannotNameAnEvent(t *testing.T) {
	paths := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "$.f" + strings.Repeat("x", i)
		}
		return out
	}
	for _, ok := range []Webhook{
		{},
		{DedupKey: []string{"$.repository.id", "$.ref", "$.after"}},
		{DedupKey: paths(16), DedupTTLSeconds: 90 * 24 * 3600},
		{DedupKey: []string{"$.commits[0].id"}, DedupTTLSeconds: 60},
	} {
		if err := CheckWebhookDedupKey(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, tc := range []struct {
		w    Webhook
		want string
	}{
		{Webhook{DedupKey: paths(17)}, "at most 16"},
		{Webhook{DedupKey: []string{"$..ref"}}, "not a supported JSONPath"},
		{Webhook{DedupKey: []string{"$.commits[*].id"}}, "not a supported JSONPath"},
		{Webhook{DedupKey: []string{"$"}}, "whole body"},
		{Webhook{DedupKey: []string{"$.ref", "$.ref"}}, "listed twice"},
		{Webhook{DedupKey: []string{"$.ref"}, DedupTTLSeconds: -1}, "cannot be negative"},
		{Webhook{DedupKey: []string{"$.ref"}, DedupTTLSeconds: 90*24*3600 + 1}, "90 days"},
		// Past ~292 years in nanoseconds: a Duration of it wraps negative.
		{Webhook{DedupKey: []string{"$.ref"}, DedupTTLSeconds: 9223372037}, "90 days"},
		{Webhook{DedupTTLSeconds: 60}, "without dedup_key"},
	} {
		if err := CheckWebhookDedupKey(tc.w); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want a refusal mentioning %q", tc.w, err, tc.want)
		}
	}
}

func TestWebhookDedupTTL_DefaultsToSevenDays(t *testing.T) {
	if got := (Webhook{DedupKey: []string{"$.ref"}}).DedupTTL(); got != 7*24*time.Hour {
		t.Errorf("default %v, want 7 days", got)
	}
	if got := (Webhook{DedupKey: []string{"$.ref"}, DedupTTLSeconds: 3600}).DedupTTL(); got != time.Hour {
		t.Errorf("%v, want 1h", got)
	}
	// A stored value the check let through before it compared seconds.
	if got := (Webhook{DedupKey: []string{"$.ref"}, DedupTTLSeconds: 9223372037}).DedupTTL(); got != MaxWebhookDedupTTL {
		t.Errorf("an overflowing lifetime is held %v, want the 90-day maximum", got)
	}
}

// The yaml load checks dedup_key with the rest of a static definition.
func TestValidateStaticWebhook_ChecksDedupKey(t *testing.T) {
	var w Webhook
	src := `
enabled: true
delivery: spawn
agent: a
auth: { kind: hmac, header: X-Hub-Signature-256, signing_secret_env: LOOMCYCLE_S }
dedup_key: ["$.repository.id", "$.ref", "$.after"]
dedup_ttl_seconds: 86400
`
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatal(err)
	}
	if err := validateStaticWebhook("gh", w); err != nil {
		t.Fatal(err)
	}
	if len(w.DedupKey) != 3 || w.DedupTTL() != 24*time.Hour {
		t.Errorf("read %v, %v", w.DedupKey, w.DedupTTL())
	}
	w.DedupKey = []string{"$.commits[*].id"}
	if err := validateStaticWebhook("gh", w); err == nil || !strings.Contains(err.Error(), "webhooks.gh: dedup_key[0]") {
		t.Errorf("a bad path loaded: %v", err)
	}
}
