package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// receiverWithTiers builds a Receiver whose cfg carries the given webhooks
// AND user_tiers (newTestReceiver only wires webhooks), so the spawn-path
// user_tier validation can be exercised.
func receiverWithTiers(t *testing.T, wh config.Webhook, tiers map[string]config.UserTier, fr *fakeRunner, env map[string]string, now time.Time) *Receiver {
	t.Helper()
	cfg := &config.Config{
		Webhooks:  map[string]config.Webhook{"gh": wh},
		UserTiers: tiers,
	}
	return New(Deps{
		Cfg:          cfg,
		Runner:       fr,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(now),
		Getenv:       mapGetenv(env),
	})
}

// TestReceiver_RejectsUnknownUserTierFromPayload pins the spawn-path tier
// guard: a payload-projected user_tier not in cfg.UserTiers is rejected with
// 400 (parity with the HTTP handler) instead of being silently dropped to the
// agent default. Regression-grade: pre-fix deliverSpawn never validated it,
// so the run was spawned (202) under the agent default.
func TestReceiver_RejectsUnknownUserTierFromPayload(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"x","tier":"nonexistent"}`)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "responder",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal", "user_tier": "$.tier"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := receiverWithTiers(t, wh, map[string]config.UserTier{"premium": {}}, fr, map[string]string{"WH_SECRET": secret}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown user_tier; body=%s", w.Code, w.Body.String())
	}
	if fr.called {
		t.Error("runner was invoked for a delivery with an unknown user_tier; must be rejected before spawn")
	}
}

// TestReceiver_AcceptsKnownUserTierFromPayload confirms a configured tier
// still spawns and flows through to the run input.
func TestReceiver_AcceptsKnownUserTierFromPayload(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"x","tier":"premium"}`)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "responder",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal", "user_tier": "$.tier"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := receiverWithTiers(t, wh, map[string]config.UserTier{"premium": {}}, fr, map[string]string{"WH_SECRET": secret}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !fr.called {
		t.Fatal("runner was not invoked for a valid tier")
	}
	if got := fr.input().UserTier; got != "premium" {
		t.Errorf("UserTier = %q, want premium", got)
	}
}
