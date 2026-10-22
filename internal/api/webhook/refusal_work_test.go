package webhook

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestWebhook_EveryRefusalHashesTheBodyAlike: a refused delivery answers the
// same 404 whatever the reason (RFC ES #4), and since RFC EU Part C it costs
// the same work too: refusalMACPasses HMAC passes over the body. Before, a
// live HMAC webhook's refusal hashed the body and an unknown name's did not,
// so a large body made the live name's 404 measurably slower; and a missing
// signature header or a bearer webhook hashed nothing, slower than neither.
// The test counts the work, not the time, which would be flaky.
func TestWebhook_EveryRefusalHashesTheBodyAlike(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hmacAuth := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	stripeAuth := config.WebhookAuth{Kind: "hmac", Header: "Stripe-Signature", SigningSecretEnv: "WH_SECRET"}
	defs := map[string]config.Webhook{
		"gh":     {Enabled: true, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"stripe": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: stripeAuth},
		"off":    {Enabled: false, Delivery: "spawn", Agent: "x", Auth: hmacAuth},
		"small":  {Enabled: true, Delivery: "spawn", Agent: "x", Auth: hmacAuth, BodySizeLimitBytes: 1024},
		"bearer": {Enabled: true, Delivery: "spawn", Agent: "x", Auth: config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_BEARER"}},
	}
	env := map[string]string{"WH_SECRET": "shhh", "WH_BEARER": "the-bearer"}
	rec := newTestReceiver(t, defs, &fakeRunner{runID: "r", agentID: "a"}, nil, env, []string{"WH_SECRET", "WH_BEARER"}, now)
	var mu sync.Mutex
	var seen []int
	rec.onRefusalMACs = func(passes int) {
		mu.Lock()
		seen = append(seen, passes)
		mu.Unlock()
	}

	body := bytes.Repeat([]byte("a"), 512<<10) // under the default 1 MiB limit
	hdr := func(k, v string) http.Header {
		h := http.Header{}
		if k != "" {
			h.Set(k, v)
		}
		return h
	}
	for _, c := range []struct {
		name, hook string
		hdr        http.Header
	}{
		{"unknown name", "nope", hdr("", "")},
		{"disabled def", "off", hdr("X-Hub-Signature-256", githubSig("shhh", body))},
		{"body over the def's limit", "small", hdr("X-Hub-Signature-256", githubSig("shhh", body))},
		{"signature mismatch", "gh", hdr("X-Hub-Signature-256", githubSig("not-the-secret", body))},
		{"missing signature header", "gh", hdr("", "")},
		{"malformed signature", "gh", hdr("X-Hub-Signature-256", "sha256=zz")},
		{"stale stripe timestamp", "stripe", hdr("Stripe-Signature", stripeSig("shhh", body, now.Add(-time.Hour)))},
		{"stripe envelope missing v1", "stripe", hdr("Stripe-Signature", "t=1700000000")},
		{"wrong bearer", "bearer", hdr("Authorization", "Bearer not-the-bearer")},
		{"no bearer", "bearer", hdr("", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			mu.Lock()
			seen = nil
			mu.Unlock()
			if w := doPost(rec, c.hook, body, c.hdr); w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 || seen[0] != refusalMACPasses {
				t.Errorf("MAC passes over the body = %v, want one refusal of %d, as every other refusal", seen, refusalMACPasses)
			}
		})
	}
}

// TestVerifySignatureCounted_HMACHashesOncePerSecretWhateverTheHeader: the
// HMAC check computes each secret's MAC before it looks at the header, so its
// work does not depend on what the header holds. Bearer auth hashes nothing;
// the receiver tops that up.
func TestVerifySignatureCounted_HMACHashesOncePerSecretWhateverTheHeader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"a"}`)
	gh := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256"}
	stripe := config.WebhookAuth{Kind: "hmac", Header: "Stripe-Signature"}
	for _, c := range []struct {
		name    string
		auth    config.WebhookAuth
		secrets []string
		header  string
		ok      bool
	}{
		{"valid github, one secret", gh, []string{"s1"}, githubSig("s1", body), true},
		{"valid github under the previous secret", gh, []string{"s1", "s0"}, githubSig("s0", body), true},
		{"wrong github, two secrets", gh, []string{"s1", "s0"}, githubSig("nope", body), false},
		{"missing header", gh, []string{"s1"}, "", false},
		{"bad hex", gh, []string{"s1", "s0"}, "sha256=zz", false},
		{"valid stripe", stripe, []string{"s1"}, stripeSig("s1", body, now), true},
		{"stale stripe", stripe, []string{"s1"}, stripeSig("s1", body, now.Add(-time.Hour)), false},
		{"stripe without v1", stripe, []string{"s1"}, "t=1700000000", false},
		{"stripe with a bad t", stripe, []string{"s1"}, "t=soon, v1=" + strings.Repeat("ab", 32), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			get := func(k string) string {
				if strings.EqualFold(k, c.auth.Header) {
					return c.header
				}
				return ""
			}
			passes, err := verifySignatureCounted(c.auth, c.secrets, body, get, func() time.Time { return now })
			if (err == nil) != c.ok {
				t.Errorf("verdict err = %v, want ok=%v", err, c.ok)
			}
			if passes != len(c.secrets) {
				t.Errorf("passes = %d, want %d (one per secret)", passes, len(c.secrets))
			}
		})
	}
	bearer := config.WebhookAuth{Kind: "bearer"}
	passes, err := verifySignatureCounted(bearer, []string{"tok"}, body, func(string) string { return "Bearer nope" }, time.Now)
	if err == nil || passes != 0 {
		t.Errorf("bearer: passes=%d err=%v, want 0 passes and a mismatch", passes, err)
	}
}
