package lookup_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ---- WebhookDef resolver ----
//
// Mirrors the A2AAgentDef resolver tests (a2a_test.go). Reuses the
// a2aJSONTagsOf + assertTagSetsEqual helpers from that file (same
// lookup_test package).

type stubWebhookStore struct {
	defs map[string]store.WebhookDefRow
}

// WebhookDefGetActive ignores tenantID — these resolver tests exercise the
// precedence/equivalence logic with the shared "" tenant; per-tenant
// isolation is covered by the store contract test.
func (s *stubWebhookStore) WebhookDefGetActive(_ context.Context, _, name string) (store.WebhookDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.WebhookDefRow{}, &store.ErrNotFound{Kind: "webhook_def_active", ID: name}
}

// RunByIdempotencyKey satisfies lookup.WebhookStore (RFC H Decision 10).
// The resolver tests never exercise dedup, so a constant miss suffices.
func (s *stubWebhookStore) RunByIdempotencyKey(_ context.Context, _ string) (store.Run, bool, error) {
	return store.Run{}, false, nil
}

// ChannelPublish + MemorySet satisfy lookup.WebhookStore (WH-5b
// on_complete hooks). The resolver tests never fire hooks, so no-ops
// suffice.
func (s *stubWebhookStore) ChannelPublish(_ context.Context, _ store.ChannelMessage, _ int) (string, int, error) {
	return "", 0, nil
}

func (s *stubWebhookStore) MemorySet(_ context.Context, _ store.MemoryScope, _, _ string, _ json.RawMessage, _ time.Duration) error {
	return nil
}

func TestWebhook_EquivalenceYamlVsSubstrate(t *testing.T) {
	yamlHook := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "intake",
		Auth: config.WebhookAuth{
			Kind:             "hmac",
			Algorithm:        "sha256",
			Header:           "X-Hub-Signature-256",
			SigningSecretEnv: "LOOMCYCLE_WH_SECRET",
			DeliveryIDHeader: "X-Delivery-ID",
		},
		RateLimit:              config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 10},
		BodySizeLimitBytes:     1 << 20,
		UserCredentialsFromEnv: map[string]string{"peer-token": "LOOMCYCLE_PEER_TOKEN"},
		PayloadMapping:         map[string]string{"action": "$.action"},
		SyncResponse:           config.WebhookSyncResponse{Enabled: true, TimeoutMs: 30000},
		OnComplete: []config.ScheduledRunHook{
			{Kind: "channel.publish", Channel: "_system/webhooks"},
		},
	}

	substrateShape := lookup.SubstrateWebhookDef{
		Enabled:  yamlHook.Enabled,
		Delivery: yamlHook.Delivery,
		Agent:    yamlHook.Agent,
		Auth: lookup.SubstrateWebhookAuth{
			Kind:             "hmac",
			Algorithm:        "sha256",
			Header:           "X-Hub-Signature-256",
			SigningSecretEnv: "LOOMCYCLE_WH_SECRET",
			DeliveryIDHeader: "X-Delivery-ID",
		},
		RateLimit:              lookup.SubstrateWebhookRateLimit{RequestsPerMinute: 60, Burst: 10},
		BodySizeLimitBytes:     1 << 20,
		UserCredentialsFromEnv: map[string]string{"peer-token": "LOOMCYCLE_PEER_TOKEN"},
		PayloadMapping:         map[string]string{"action": "$.action"},
		SyncResponse:           lookup.SubstrateWebhookSyncResp{Enabled: true, TimeoutMs: 30000},
		OnComplete: []config.ScheduledRunHook{
			{Kind: "channel.publish", Channel: "_system/webhooks"},
		},
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ss := &stubWebhookStore{
		defs: map[string]store.WebhookDefRow{
			"gh-push": {DefID: "wh_v1", Name: "gh-push", Version: 1, Definition: defJSON, CreatedAt: time.Now()},
		},
	}
	resolved, ok := lookup.Webhook(context.Background(), ss, &config.Config{}, "", "gh-push")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if !reflect.DeepEqual(resolved, yamlHook) {
		t.Errorf("substrate-resolved webhook != yaml webhook:\n got %+v\nwant %+v", resolved, yamlHook)
	}
}

func TestWebhook_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		Webhooks: map[string]config.Webhook{
			"hook": {Delivery: "spawn", Agent: "yaml-agent"},
		},
	}
	ss := &stubWebhookStore{
		defs: map[string]store.WebhookDefRow{
			"hook": {DefID: "wh_v1", Name: "hook", Definition: json.RawMessage(`{"delivery":"spawn","agent":"substrate-agent"}`)},
		},
	}
	got, ok := lookup.Webhook(context.Background(), ss, cfg, "", "hook")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Agent != "yaml-agent" {
		t.Errorf("Agent = %q, want yaml-agent (static must win)", got.Agent)
	}
}

// TestWebhook_DriftDetection pins the SubstrateWebhookDef field set
// against an explicit `want` enumeration. A field added to or removed
// from SubstrateWebhookDef without updating this map fails CI.
//
// The complementary direction (mergedWebhookDef ↔ SubstrateWebhookDef)
// lives in the builtin package (WH-2:
// TestMergedWebhookDef_DriftDetection_VsLookupSubstrate). This `want`
// map is the canonical field-set both the substrate-read shape here and
// the substrate-write `mergedWebhookDef` must match.
func TestWebhook_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"enabled":                   true,
		"delivery":                  true,
		"agent":                     true,
		"channel":                   true,
		"auth":                      true,
		"rate_limit":                true,
		"body_size_limit_bytes":     true,
		"user_credentials_from_env": true,
		"user_credentials":          true, // RFC F fork-time parity (PR 2/2)
		"metadata":                  true, // non-secret agent metadata (PR 2/2)
		"tenant_id":                 true, // tenant the spawned run executes as (RFC N follow-up)
		"payload_mapping":           true,
		"sync_response":             true,
		"on_complete":               true,
		"operator_key_restricted":   true, // RFC AX: captured operator-key restriction (anti-bypass)
	}
	have := a2aJSONTagsOf(reflect.TypeOf(lookup.SubstrateWebhookDef{}))
	assertTagSetsEqual(t, "SubstrateWebhookDef", want, have)
}
