package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// fixedClock returns a now() that always reports t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// mapGetenv builds a getenv from a map.
func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// githubSig computes a GitHub-style `sha256=<hexmac>` over the raw body.
func githubSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// stripeSig computes a Stripe-style `t=<unix>, v1=<hexmac>` over `<t>.<body>`.
func stripeSig(secret string, body []byte, ts time.Time) string {
	tsStr := fmt.Sprintf("%d", ts.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(append([]byte(tsStr+"."), body...))
	return fmt.Sprintf("t=%s, v1=%s", tsStr, hex.EncodeToString(mac.Sum(nil)))
}

func headerGetter(h http.Header) func(string) string {
	return func(k string) string { return h.Get(k) }
}

func TestVerifySignature_GitHubStyleValid_Accepts(t *testing.T) {
	secret := "shhh"
	body := []byte(`{"action":"opened"}`)
	allow := map[string]bool{"WH_SECRET": true}
	env := mapGetenv(map[string]string{"WH_SECRET": secret})

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))

	a := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	if err := verifySignature(a, body, headerGetter(h), allow, env, time.Now); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestVerifySignature_TamperedBody_Rejects(t *testing.T) {
	secret := "shhh"
	signed := []byte(`{"action":"opened"}`)
	tampered := []byte(`{"action":"closed"}`)
	allow := map[string]bool{"WH_SECRET": true}
	env := mapGetenv(map[string]string{"WH_SECRET": secret})

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, signed))

	a := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	err := verifySignature(a, tampered, headerGetter(h), allow, env, time.Now)
	if !errors.Is(err, errSignatureMismatch) {
		t.Fatalf("want errSignatureMismatch, got %v", err)
	}
}

func TestVerifySignature_StripeStyleValid_Accepts(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_700_000_000, 0)
	allow := map[string]bool{"WH_SECRET": true}
	env := mapGetenv(map[string]string{"WH_SECRET": secret})

	h := http.Header{}
	h.Set("X-Loomcycle-Signature", stripeSig(secret, body, now))

	a := config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "WH_SECRET"}
	if err := verifySignature(a, body, headerGetter(h), allow, env, fixedClock(now)); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestVerifySignature_OutOfWindowTimestamp_Rejects(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"id":"evt_1"}`)
	signedAt := time.Unix(1_700_000_000, 0)
	// Verify 10 minutes later — beyond the ±5m tolerance.
	checkAt := signedAt.Add(10 * time.Minute)
	allow := map[string]bool{"WH_SECRET": true}
	env := mapGetenv(map[string]string{"WH_SECRET": secret})

	h := http.Header{}
	h.Set("X-Loomcycle-Signature", stripeSig(secret, body, signedAt))

	a := config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "WH_SECRET"}
	err := verifySignature(a, body, headerGetter(h), allow, env, fixedClock(checkAt))
	if !errors.Is(err, errSignatureMismatch) {
		t.Fatalf("want errSignatureMismatch (stale ts), got %v", err)
	}
}

func TestVerifySignature_BearerValid_Accepts(t *testing.T) {
	token := "bearer-token-xyz"
	allow := map[string]bool{"WH_BEARER": true}
	env := mapGetenv(map[string]string{"WH_BEARER": token})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)

	a := config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_BEARER"}
	if err := verifySignature(a, []byte(`{}`), headerGetter(h), allow, env, time.Now); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestVerifySignature_BearerMissing_Rejects(t *testing.T) {
	allow := map[string]bool{"WH_BEARER": true}
	env := mapGetenv(map[string]string{"WH_BEARER": "bearer-token-xyz"})

	h := http.Header{} // no Authorization header

	a := config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_BEARER"}
	err := verifySignature(a, []byte(`{}`), headerGetter(h), allow, env, time.Now)
	if !errors.Is(err, errSignatureMismatch) {
		t.Fatalf("want errSignatureMismatch, got %v", err)
	}
}

func TestVerifySignature_SecretNotInAllowlist_Unresolvable(t *testing.T) {
	body := []byte(`{}`)
	allow := map[string]bool{} // WH_SECRET deliberately absent
	env := mapGetenv(map[string]string{"WH_SECRET": "shhh"})

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig("shhh", body))

	a := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	err := verifySignature(a, body, headerGetter(h), allow, env, time.Now)
	var ae *authError
	if !errors.As(err, &ae) || ae.verdict != verdictUnresolved {
		t.Fatalf("want unresolvable authError, got %v", err)
	}
	if ae.secretEnv != "WH_SECRET" {
		t.Fatalf("want secretEnv=WH_SECRET, got %q", ae.secretEnv)
	}
}

func TestVerifySignature_SecretUnsetInEnv_Unresolvable(t *testing.T) {
	body := []byte(`{}`)
	allow := map[string]bool{"WH_SECRET": true}
	env := mapGetenv(map[string]string{}) // allowlisted but unset

	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig("shhh", body))

	a := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	err := verifySignature(a, body, headerGetter(h), allow, env, time.Now)
	var ae *authError
	if !errors.As(err, &ae) || ae.verdict != verdictUnresolved {
		t.Fatalf("want unresolvable authError, got %v", err)
	}
}
