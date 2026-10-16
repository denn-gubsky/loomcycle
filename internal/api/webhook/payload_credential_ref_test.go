package webhook

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Only a definition's author may name a stored credential. A credential value
// the payload supplies that is, or holds, a reference ($cred:, $ghapp:) would
// be resolved where the run uses it — the sender choosing which of the
// tenant's secrets goes out — so the delivery is refused as a bad mapping,
// and the dry-run says it would be.
func TestReceiver_APayloadCredentialNamingAStoredOneIsRefused(t *testing.T) {
	secret := "shhh"
	now := time.Unix(1_700_000_000, 0)
	wh := config.Webhook{
		Enabled:  true,
		Delivery: "spawn",
		Agent:    "researcher",
		Auth:     config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		PayloadMapping: map[string]string{
			"goal":                 "$.goal",
			"user_credentials.TOK": "$.tok",
		},
	}
	for _, tok := range []string{"$cred:tenant_secret", "Bearer $ghapp:the_app", "x$cred:y"} {
		fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
		rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)
		body, _ := json.Marshal(map[string]string{"goal": "g", "tok": tok})
		h := http.Header{}
		h.Set("X-Hub-Signature-256", githubSig(secret, body))

		w := doPost(rec, "gh", body, h)
		if w.Code != http.StatusBadRequest || fr.called {
			t.Errorf("tok %q: status %d, runner called %t; want 400 and no run", tok, w.Code, fr.called)
		}
		var resp testResult
		tw := triageDo(triageServer(t, rec, triageTokens), http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, body, h)
		if err := json.Unmarshal(tw.Body.Bytes(), &resp); err != nil {
			t.Fatalf("tok %q: dry-run %d %s", tok, tw.Code, tw.Body.String())
		}
		if resp.WouldAccept || resp.Verdict != "rejected_mapping" {
			t.Errorf("tok %q: dry-run would_accept=%t verdict=%q, want a rejected mapping", tok, resp.WouldAccept, resp.Verdict)
		}
	}

	// A plain per-delivery token is still the payload's to supply.
	fr := &fakeRunner{runID: "run-1", agentID: "agent-1"}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, fr, nil, map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, now)
	body := []byte(`{"goal":"g","tok":"plain-token"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted || fr.input().UserCredentials["TOK"] != "plain-token" {
		t.Errorf("a plain token: status %d, TOK %q", w.Code, fr.input().UserCredentials["TOK"])
	}
}
