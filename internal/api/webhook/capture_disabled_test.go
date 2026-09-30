package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A webhook a snapshot restored without its literal credentials carries a
// capture_disabled marker. The receiver treats it as disabled — the same
// opaque 404 an unknown name gets — even when the def says enabled:true and
// the delivery is correctly signed: nothing runs without the credentials the
// def was authored with.
func TestReceiver_CaptureDisabledMarker_404sEvenWhenEnabled(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"go"}`)
	wh := config.Webhook{
		Enabled: true, Delivery: "spawn", Agent: "x",
		Auth:            config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		CaptureDisabled: &config.WebhookCaptureDisabled{StrippedCredentials: []string{"jobs"}},
	}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := doPost(rec, "gh", body, h)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a capture-disabled def; body=%s", w.Code, w.Body.String())
	}
	if fr.wasCalled() {
		t.Fatal("runner invoked for a capture-disabled def")
	}

	// The same def without the marker accepts the same signed delivery, so
	// the 404 above is the marker's doing.
	wh.CaptureDisabled = nil
	fr = &fakeRunner{runID: "r", agentID: "a"}
	rec = newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("unmarked control: status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
}
