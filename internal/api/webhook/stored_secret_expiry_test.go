package webhook

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialtest"
)

// A stored verification secret past its expires_at verifies nothing: a
// delivery signed with it is refused (never accepted as if unsigned), with
// the answer a missing secret gets: 503 naming the reference, never a
// value. An expired PREVIOUS secret is left out while the current one still
// verifies, so a rotation can end by letting the old secret lapse.
func TestReceiver_AnExpiredStoredSecretRefusesTheDelivery(t *testing.T) {
	st, engine, put := credentialStore(t)
	put("acme", "hook", "current-secret")
	put("acme", "hook-old", "old-secret")
	putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
		"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook", "previous_secret": "$cred:hook-old"}))
	rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: engine})
	n := 0
	deliver := func(secret string) (int, string) {
		n++
		return postWith(rec, "/v1/_webhooks/acme/gh", secret, []byte(fmt.Sprintf(`{"n":%d}`, n)))
	}
	if code, _ := deliver("old-secret"); code != http.StatusAccepted {
		t.Fatalf("before any expiry, signed with the previous secret: %d, want 202", code)
	}

	// The previous secret lapses: it no longer verifies; the current does.
	credentialtest.Expire(t, st, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "hook-old"}, time.Now().Add(-time.Minute))
	if code, _ := deliver("old-secret"); code != http.StatusUnauthorized {
		t.Errorf("signed with an expired previous secret: %d, want 401", code)
	}
	if code, _ := deliver("current-secret"); code != http.StatusAccepted {
		t.Errorf("signed with the current secret while the previous one is expired: %d, want 202", code)
	}

	// The current secret lapses: nothing is accepted, signed or not.
	credentialtest.Expire(t, st, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "hook"}, time.Now().Add(-time.Minute))
	code, body := deliver("current-secret")
	if code == http.StatusAccepted {
		t.Fatalf("a delivery signed with an expired secret was accepted: %s", body)
	}
	// The same answer a missing secret gets: the sender is unauthenticated,
	// so it learns which reference failed, not why.
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"secret":"$cred:hook"`) {
		t.Errorf("refusal %d %s: want 503 naming the reference", code, body)
	}
	if strings.Contains(body, "current-secret") {
		t.Fatalf("the refusal carries a secret value: %s", body)
	}
	if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "", []byte(`{"unsigned":true}`)); code == http.StatusAccepted {
		t.Error("an unsigned delivery was accepted once the secret expired")
	}
}
