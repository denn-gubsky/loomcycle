package webhook

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// A stored verification secret the server cannot open — sealed under a key
// it no longer holds, or with no key configured — verifies nothing: a
// delivery signed with the real secret is refused, as is an unsigned one,
// with the answer a missing secret gets (503 naming the reference).
func TestReceiver_AnUnopenableStoredSecretRefusesTheDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		kek  string
	}{
		{"another key", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))},
		{"no key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _, put := credentialStore(t)
			put("acme", "hook", "the-secret")
			putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
				"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook"}))
			sealer, err := credential.NewSealer(tc.kek, "")
			if err != nil {
				t.Fatal(err)
			}
			rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: credential.NewEngine(st, sealer)})

			code, body := postWith(rec, "/v1/_webhooks/acme/gh", "the-secret", []byte(`{"n":1}`))
			if code != http.StatusServiceUnavailable || body != `{"error":"unavailable"}`+"\n" {
				t.Errorf("signed with the real secret: %d %s, want the opaque 503", code, body)
			}
			if strings.Contains(body, "the-secret") {
				t.Fatalf("the refusal carries the secret: %s", body)
			}
			if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "", []byte(`{"n":2}`)); code == http.StatusAccepted {
				t.Error("an unsigned delivery was accepted")
			}
		})
	}
}
