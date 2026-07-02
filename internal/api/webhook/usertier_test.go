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

// TestReceiver_RejectsUnknownDefUserTier: an unknown tier PINNED in the def (an
// operator typo) is rejected with 400 (the spawn-path typo guard), not silently
// dropped to the agent default.
func TestReceiver_RejectsUnknownDefUserTier(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"x"}`)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "responder",
		UserTier:       "nonexistent", // Def-pinned bad tier
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := receiverWithTiers(t, wh, map[string]config.UserTier{"premium": {}}, fr, map[string]string{"WH_SECRET": secret}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown def user_tier; body=%s", w.Code, w.Body.String())
	}
	if fr.called {
		t.Error("runner invoked despite an unknown def user_tier")
	}
}

// TestReceiver_AcceptsDefPinnedUserTier: a tier pinned in the def flows through
// to the run input and spawns.
func TestReceiver_AcceptsDefPinnedUserTier(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"x"}`)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "responder",
		UserTier:       "premium",
		Auth:           config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := receiverWithTiers(t, wh, map[string]config.UserTier{"premium": {}}, fr, map[string]string{"WH_SECRET": secret}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if got := fr.input().UserTier; got != "premium" {
		t.Errorf("UserTier = %q, want premium (from the def pin)", got)
	}
}

// TestReceiver_PayloadCannotSelectUserTier is the regression: a signed sender
// must NOT be able to pick the (cost) tier via the payload. The def pins "basic";
// the payload maps user_tier→"premium"; the run must execute as "basic". Pre-fix,
// buildRunInput took UserTier from proj.Fields → the sender got "premium".
func TestReceiver_PayloadCannotSelectUserTier(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"x","tier":"premium"}`)
	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "responder",
		UserTier: "basic", // operator pin
		Auth:     config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		// A malicious/over-reaching mapping that tries to select the premium tier.
		PayloadMapping: map[string]string{"goal": "$.goal", "user_tier": "$.tier"},
	}
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := receiverWithTiers(t, wh, map[string]config.UserTier{"basic": {}, "premium": {}}, fr, map[string]string{"WH_SECRET": secret}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if got := fr.input().UserTier; got != "basic" {
		t.Errorf("UserTier = %q, want basic — the payload must NOT override the def-pinned tier", got)
	}
}
