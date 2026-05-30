package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// WebhookStore is the subset of store.Store the webhook resolver uses.
// Declared here so tests + callers can mock without depending on the
// full store interface. Mirrors A2AAgentStore for the v1.x RFC H
// WebhookDef substrate.
type WebhookStore interface {
	WebhookDefGetActive(ctx context.Context, name string) (store.WebhookDefRow, error)
}

// Webhook resolves a webhook NAME to its effective config.Webhook by
// walking the lookup chain in precedence order:
//
//  1. static cfg.Webhooks (yaml-defined, pre-validated at boot)
//  2. webhook_def_active + webhook_defs (substrate path)
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false) — defensive against
// future-field churn or hand-edited rows.
//
// Mirrors lookup.A2AAgent for the v1.x RFC H WebhookDef substrate.
func Webhook(ctx context.Context, s WebhookStore, cfg *config.Config, name string) (config.Webhook, bool) {
	if cfg != nil {
		if w, ok := cfg.Webhooks[name]; ok {
			return w, true
		}
	}
	if s == nil {
		return config.Webhook{}, false
	}
	activeRow, err := s.WebhookDefGetActive(ctx, name)
	if err != nil {
		return config.Webhook{}, false
	}
	var wd SubstrateWebhookDef
	if uerr := json.Unmarshal(activeRow.Definition, &wd); uerr != nil {
		return config.Webhook{}, false
	}
	return wd.ToConfigDef(), true
}

// SubstrateWebhookDef mirrors the JSON shape `WebhookDef` create/fork
// persists in `webhook_defs.definition` (snake_case JSON tags via the
// `mergedWebhookDef` adapter in internal/tools/builtin/webhookdef.go).
// The runtime consumer (`config.Webhook`) carries yaml tags for the
// operator-yaml path; this adapter is the substrate-read seam.
//
// Kept in sync with `mergedWebhookDef`; the builtin-package drift test
// TestMergedWebhookDef_DriftDetection_VsLookupSubstrate pins
// merged↔substrate parity. The complementary assertion here
// (webhook_test.go TestWebhook_DriftDetection) pins this shape against
// an explicit expected field-set, mirroring the A2AAgentDef resolver.
type SubstrateWebhookDef struct {
	Enabled                bool                      `json:"enabled,omitempty"`
	Delivery               string                    `json:"delivery,omitempty"`
	Agent                  string                    `json:"agent,omitempty"`
	Channel                string                    `json:"channel,omitempty"`
	Auth                   SubstrateWebhookAuth      `json:"auth,omitempty"`
	RateLimit              SubstrateWebhookRateLimit `json:"rate_limit,omitempty"`
	BodySizeLimitBytes     int                       `json:"body_size_limit_bytes,omitempty"`
	UserCredentialsFromEnv map[string]string         `json:"user_credentials_from_env,omitempty"`
	PayloadMapping         map[string]string         `json:"payload_mapping,omitempty"`
	SyncResponse           SubstrateWebhookSyncResp  `json:"sync_response,omitempty"`
	OnComplete             []config.ScheduledRunHook `json:"on_complete,omitempty"`
}

// SubstrateWebhookAuth mirrors config.WebhookAuth.
type SubstrateWebhookAuth struct {
	Kind             string `json:"kind,omitempty"`
	Algorithm        string `json:"algorithm,omitempty"`
	Header           string `json:"header,omitempty"`
	SigningSecretEnv string `json:"signing_secret_env,omitempty"`
	DeliveryIDHeader string `json:"delivery_id_header,omitempty"`
	BearerTokenEnv   string `json:"bearer_token_env,omitempty"`
}

// SubstrateWebhookRateLimit mirrors config.WebhookRateLimit.
type SubstrateWebhookRateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	Burst             int `json:"burst,omitempty"`
}

// SubstrateWebhookSyncResp mirrors config.WebhookSyncResponse.
type SubstrateWebhookSyncResp struct {
	Enabled   bool `json:"enabled,omitempty"`
	TimeoutMs int  `json:"timeout_ms,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.Webhook for
// the runtime to consume. Pure data shuffling.
//
// OnComplete is config.ScheduledRunHook on BOTH sides (the substrate
// shape reuses ScheduleDef's hook type per RFC H), so the slice is
// copied by reference — no field-by-field projection needed.
func (s SubstrateWebhookDef) ToConfigDef() config.Webhook {
	return config.Webhook{
		Enabled:  s.Enabled,
		Delivery: s.Delivery,
		Agent:    s.Agent,
		Channel:  s.Channel,
		Auth: config.WebhookAuth{
			Kind:             s.Auth.Kind,
			Algorithm:        s.Auth.Algorithm,
			Header:           s.Auth.Header,
			SigningSecretEnv: s.Auth.SigningSecretEnv,
			DeliveryIDHeader: s.Auth.DeliveryIDHeader,
			BearerTokenEnv:   s.Auth.BearerTokenEnv,
		},
		RateLimit: config.WebhookRateLimit{
			RequestsPerMinute: s.RateLimit.RequestsPerMinute,
			Burst:             s.RateLimit.Burst,
		},
		BodySizeLimitBytes:     s.BodySizeLimitBytes,
		UserCredentialsFromEnv: s.UserCredentialsFromEnv,
		PayloadMapping:         s.PayloadMapping,
		SyncResponse: config.WebhookSyncResponse{
			Enabled:   s.SyncResponse.Enabled,
			TimeoutMs: s.SyncResponse.TimeoutMs,
		},
		OnComplete: s.OnComplete,
	}
}
