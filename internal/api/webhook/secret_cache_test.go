package webhook

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func newTestClock() *testClock { return &testClock{t: time.Now()} }

// pastSecretTTL moves the clock past storedSecretTTL, so the next delivery
// reads its stored secret again.
func (c *testClock) pastSecretTTL() { c.advance(storedSecretTTL + time.Second) }

// countingCreds is a CredentialResolver that counts its reads.
type countingCreds struct {
	mu     sync.Mutex
	reads  int
	values map[string]string // tenant/name → value
}

func (c *countingCreds) ResolveToCompare(_ context.Context, tenantID, name string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	v, ok := c.values[tenantID+"/"+name]
	return v, ok, nil
}

func (c *countingCreds) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// Every delivery to a webhook whose secret is a stored credential read and
// decrypted the credential before its signature was checked — so anyone could
// make the server read and decrypt it at will by posting unsigned bodies. The
// resolved secret is kept in memory for storedSecretTTL: a burst of posts
// costs one read, and a rotated or deleted secret takes effect once it lapses.
func TestReceiver_StoredSecretIsReadOncePerTTL(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
		"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook"}))
	creds := &countingCreds{values: map[string]string{"acme/hook": "the-secret"}}
	clock := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: creds, Now: now})

	for i := 0; i < 20; i++ {
		if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "forged", []byte(fmt.Sprintf(`{"n":%d}`, i))); code != http.StatusUnauthorized {
			t.Fatalf("an unsigned post: %d, want 401", code)
		}
	}
	if n := creds.count(); n != 1 {
		t.Fatalf("20 unsigned posts read the credential %d times, want 1", n)
	}
	if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "the-secret", []byte(`{"ok":1}`)); code != http.StatusAccepted {
		t.Fatalf("signed with the cached secret: %d, want 202", code)
	}

	// Rotated: the old value is still compared until the cache lapses, then
	// the new one is read.
	creds.mu.Lock()
	creds.values["acme/hook"] = "rotated"
	creds.mu.Unlock()
	advance(storedSecretTTL + time.Second)
	if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "rotated", []byte(`{"ok":2}`)); code != http.StatusAccepted {
		t.Errorf("signed with the rotated secret after the cache lapsed: %d, want 202", code)
	}
	if code, _ := postWith(rec, "/v1/_webhooks/acme/gh", "the-secret", []byte(`{"ok":3}`)); code != http.StatusUnauthorized {
		t.Errorf("signed with the old secret after the cache lapsed: %d, want 401", code)
	}
	if n := creds.count(); n != 2 {
		t.Errorf("the credential was read %d times, want 2", n)
	}

	// Another tenant's credential of the same name is its own entry.
	if _, _, err := rec.creds.ResolveToCompare(context.Background(), "other", "hook"); err != nil {
		t.Fatal(err)
	}
	if n := creds.count(); n != 3 {
		t.Errorf("another tenant's credential was answered from acme's entry (reads %d, want 3)", n)
	}
}
