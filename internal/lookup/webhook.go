package lookup

import (
	"context"
	"encoding/json"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// WebhookStore is the subset of store.Store the webhook resolver uses.
// Declared here so tests + callers can mock without depending on the
// full store interface. Mirrors A2AAgentStore for the v1.x RFC H
// WebhookDef substrate.
type WebhookStore interface {
	// RFC N: the substrate lookup carries a tenantID.
	WebhookDefGetActive(ctx context.Context, tenantID, name string) (store.WebhookDefRow, error)

	// RunByIdempotencyKey backs RFC H Decision 10 "Layer 2" durable
	// dedup in the receiver: before spawning, it looks up whether a run
	// already exists for this delivery id (the idempotency key). ok=false
	// means no prior run — proceed with the spawn.
	RunByIdempotencyKey(ctx context.Context, key string) (store.Run, bool, error)

	// ChannelPublish + MemorySet back the WH-5b on_complete hooks (the
	// receiver MIRRORS the scheduler's dispatch rather than importing it).
	// Signatures match store.Store exactly so store.Store satisfies this
	// interface unchanged; the webhook receiver only needs these two of
	// the channel/memory surface.
	ChannelPublish(ctx context.Context, msg store.ChannelMessage, maxMessages int) (id string, dropped int, err error)
	MemorySet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error
}

// Webhook resolves a webhook NAME to its effective config.Webhook within
// the given tenant, walking the lookup chain in precedence order (mirrors
// lookup.Agent):
//
//  1. (tenantID != "") tenant-scoped substrate (webhook_def_active
//     WHERE tenant_id=tenantID)
//  2. static cfg.Webhooks (yaml-defined, the shared operator base)
//  3. shared substrate (tenant_id="")
//
// The inbound receiver passes the URL-derived tenant: the bare-root route
// POST /v1/_webhooks/{name} resolves under "" (so step 1 is skipped and the
// order collapses to static→shared, identical to pre-RFC-N), while
// POST /v1/_webhooks/{tenant}/{name} resolves the named tenant's webhook.
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false).
func Webhook(ctx context.Context, s WebhookStore, cfg *config.Config, tenantID, name string) (config.Webhook, bool) {
	// 1. Tenant-scoped substrate (skipped for the shared "" tenant).
	if tenantID != "" {
		if w, ok := resolveWebhookSubstrate(ctx, s, tenantID, name); ok {
			return w, true
		}
	}
	// 2. Static cfg.Webhooks — the shared operator base.
	if cfg != nil {
		if w, ok := cfg.Webhooks[name]; ok {
			return w, true
		}
	}
	// 3. Shared substrate (tenant_id="").
	return resolveWebhookSubstrate(ctx, s, "", name)
}

// resolveWebhookSubstrate reads the webhook_def_active overlay for one
// tenant pass. Returns (zero, false) on nil store, no active pointer for
// that tenant, or malformed row JSON.
func resolveWebhookSubstrate(ctx context.Context, s WebhookStore, tenantID, name string) (config.Webhook, bool) {
	if s == nil {
		return config.Webhook{}, false
	}
	activeRow, err := s.WebhookDefGetActive(ctx, tenantID, name)
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
	UserCredentials        map[string]string         `json:"user_credentials,omitempty"`
	Metadata               map[string]any            `json:"metadata,omitempty"`
	TenantID               string                    `json:"tenant_id,omitempty"`
	PayloadMapping         map[string]string         `json:"payload_mapping,omitempty"`
	SyncResponse           SubstrateWebhookSyncResp  `json:"sync_response,omitempty"`
	OnComplete             []config.ScheduledRunHook `json:"on_complete,omitempty"`
	// OperatorKeyRestricted (RFC AX) mirrors mergedWebhookDef — captured from the
	// authoring principal, projected onto config.Webhook for the receiver.
	OperatorKeyRestricted bool `json:"operator_key_restricted,omitempty"`
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
		UserCredentials:        s.UserCredentials,
		Metadata:               s.Metadata,
		TenantID:               s.TenantID,
		PayloadMapping:         s.PayloadMapping,
		SyncResponse: config.WebhookSyncResponse{
			Enabled:   s.SyncResponse.Enabled,
			TimeoutMs: s.SyncResponse.TimeoutMs,
		},
		OnComplete:            s.OnComplete,
		OperatorKeyRestricted: s.OperatorKeyRestricted,
	}
}
