package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestReceiver_OversizedBodyRejectedBeforeSpawn pins the body_size_limit_bytes
// trust-boundary cap: a body larger than the configured limit is rejected
// (400) by the MaxBytesReader-bounded read BEFORE it is buffered, parsed, or
// spawned. Closes a fault-injection coverage gap (the cap was enforced but
// untested).
func TestReceiver_OversizedBodyRejectedBeforeSpawn(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	wh := config.Webhook{
		Enabled:            true,
		Delivery:           "spawn",
		Agent:              "x",
		Auth:               config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		BodySizeLimitBytes: 16, // tiny cap so a normal body overflows it
	}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	big := []byte(`{"goal":"` + strings.Repeat("A", 1024) + `"}`) // well over 16 bytes
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, big))
	w := doPost(rec, "gh", big, h)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if fr.called {
		t.Error("runner invoked for an oversized body; the cap must reject it before spawn")
	}
}
