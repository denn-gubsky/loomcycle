package webhook

import (
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// TestWebhook_BadSignatureIsIndistinguishableFromUnknownName: an
// unauthenticated caller cannot tell whether a webhook name exists. An unknown
// name, a disabled def and every authentication failure on a live def (a
// signature mismatch, a missing signature header, a wrong bearer) answer the
// same status, headers and body bytes; only the delivery ring keeps the real
// verdict, for the operator.
func TestWebhook_BadSignatureIsIndistinguishableFromUnknownName(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hmacAuth := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	defs := map[string]config.Webhook{
		"gh":     {Enabled: true, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"off":    {Enabled: false, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"bearer": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_BEARER"}},
	}
	env := map[string]string{"WH_SECRET": "shhh", "WH_BEARER": "the-bearer"}
	fr := &fakeRunner{runID: "r", agentID: "a"}
	rec := newTestReceiver(t, defs, fr, nil, env, []string{"WH_SECRET", "WH_BEARER"}, now)

	body := []byte(`{"goal":"a"}`)
	badSig := http.Header{}
	badSig.Set("X-Hub-Signature-256", githubSig("not-the-secret", body))
	wrongBearer := http.Header{}
	wrongBearer.Set("Authorization", "Bearer not-the-bearer")

	baseline := doPost(rec, "nope", body, http.Header{})
	if baseline.Code != http.StatusNotFound {
		t.Fatalf("unknown name: status = %d, want 404", baseline.Code)
	}
	for _, c := range []struct {
		name, hook string
		hdr        http.Header
		verdict    string // "" = no ring for this def
	}{
		{"disabled def", "off", badSig, ""},
		{"signature mismatch", "gh", badSig, verdictRejectedSig},
		{"missing signature header", "gh", http.Header{}, verdictRejectedSig},
		{"wrong bearer", "bearer", wrongBearer, verdictRejectedSig},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := doPost(rec, c.hook, body, c.hdr)
			if w.Code != baseline.Code {
				t.Errorf("status = %d, want %d (as an unknown name)", w.Code, baseline.Code)
			}
			if !reflect.DeepEqual(w.Header(), baseline.Header()) {
				t.Errorf("headers = %v, want %v (as an unknown name)", w.Header(), baseline.Header())
			}
			if !bytes.Equal(w.Body.Bytes(), baseline.Body.Bytes()) {
				t.Errorf("body = %q, want %q (as an unknown name)", w.Body.String(), baseline.Body.String())
			}
			if c.verdict == "" {
				return
			}
			recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, c.hook), 1)
			if !ok || len(recs) == 0 || recs[0].Verdict != c.verdict {
				t.Errorf("delivery ring = %+v, want the real verdict %q kept for the operator", recs, c.verdict)
			}
		})
	}
	if fr.wasCalled() {
		t.Fatal("a run started on an unauthenticated delivery")
	}
}

// TestWebhook_UnresolvableSecretDoesNotNameTheSecret: a def whose secret
// cannot be resolved stays 503 (so a sender retries once the operator fixes
// it), but the unauthenticated caller learns neither the env var nor the
// credential reference; the log keeps them for the operator. An auth.kind=none
// def the operator has not enabled is the same config-side failure and
// answers the same body.
func TestWebhook_UnresolvableSecretDoesNotNameTheSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	for _, c := range []struct {
		name, ref string
		auth      config.WebhookAuth
	}{
		{"env var not allowlisted", "WH_SECRET", config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}},
		{"stored credential missing", "$cred:hook", config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", Secret: "$cred:hook"}},
		{"unauthenticated mode not enabled", "LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED", config.WebhookAuth{Kind: "none"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged strings.Builder
			rec := newTestReceiver(t, map[string]config.Webhook{"gh": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: c.auth}},
				&fakeRunner{runID: "r", agentID: "a"}, nil, map[string]string{"WH_SECRET": "shhh"}, nil, now)
			rec.logf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }

			h := http.Header{}
			h.Set("X-Hub-Signature-256", githubSig("shhh", body))
			w := doPost(rec, "gh", body, h)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if got, want := strings.TrimSpace(w.Body.String()), `{"error":"unavailable"}`; got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
			if !strings.Contains(logged.String(), c.ref) {
				t.Errorf("log %q does not name %s for the operator", logged.String(), c.ref)
			}
			recs, ok := rec.recentSnapshot(webhookKey(lookup.WebhookOwner{Static: true}, "gh"), 1)
			if !ok || len(recs) == 0 || recs[0].Verdict != verdictUnresolved {
				t.Errorf("delivery ring = %+v, want verdict %q", recs, verdictUnresolved)
			}
		})
	}
}
