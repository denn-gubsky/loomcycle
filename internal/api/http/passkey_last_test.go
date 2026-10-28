package http

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// An identity at a gate nobody signs in through (a SCIM gate's, or one of a
// gate no longer configured) is no way in: the last passkey stays.
func TestPasskey_AnIdentityAtANonSignInGateIsNoWayIn(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a := registerPasskey(t, b)
	p := passkeyOnlyUser(t, f, sub, a)
	if err := f.st.UserIdentityCreate(context.Background(), store.UserIdentityRow{TenantID: "acme", Subject: sub, Gate: "okta",
		Issuer: "scim", ExternalSubject: "x1", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if code, out := doPasskeyStepUp(t, p, "DELETE", "/v1/_auth/passkeys/"+b64u.EncodeToString(a.credID), a); code != http.StatusConflict || out["code"] != "last_sign_in_method" {
		t.Fatalf("removing the last passkey beside a SCIM identity: %d %v", code, out)
	}
}

// barrierStore holds every passkey delete until two have arrived, so both
// requests have made their checks before either deletes.
type barrierStore struct {
	store.Store
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (b *barrierStore) wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == 2 {
		close(b.both)
	}
	b.mu.Unlock()
	select {
	case <-b.both:
	case <-time.After(3 * time.Second):
	}
}

func (b *barrierStore) UserPasskeyDelete(ctx context.Context, tenantID, subject string, id []byte) (bool, error) {
	b.wait()
	return b.Store.UserPasskeyDelete(ctx, tenantID, subject, id)
}

func (b *barrierStore) UserPasskeyDeleteKeeping(ctx context.Context, tenantID, subject string, id []byte) (bool, bool, error) {
	b.wait()
	return b.Store.UserPasskeyDeleteKeeping(ctx, tenantID, subject, id)
}

// Two removals at once of a passkey-only account's two passkeys never take
// both: one goes, the other is refused as the last way in.
func TestPasskey_ConcurrentRemovalsKeepTheLastWayIn(t *testing.T) {
	f := newPasskeyFixture(t)
	b, sub := f.passwordUser(t, "alice@example.com")
	a1 := registerPasskey(t, b)
	p := passkeyOnlyUser(t, f, sub, a1)
	code, offer := doPasskeyStepUp(t, p, "POST", "/v1/_auth/passkeys/register/begin", a1)
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, offer)
	}
	a2 := newSoftAuthenticator(t, testRPID, testOrigin)
	if code, out := p.do("POST", "/v1/_auth/passkeys/register/finish", jsonBody(t, a2.create(offer))); code != http.StatusCreated {
		t.Fatalf("register finish: %d %v", code, out)
	}
	paths := []string{"/v1/_auth/passkeys/" + b64u.EncodeToString(a1.credID), "/v1/_auth/passkeys/" + b64u.EncodeToString(a2.credID)}
	keys := []*softAuthenticator{a1, a2}
	bodies := make([]string, 2)
	for i, path := range paths {
		code, out := p.do("DELETE", path, `{}`)
		if code != http.StatusUnauthorized || out["code"] != "second_factor_required" {
			t.Fatalf("step-up offer %d: %d %v", i, code, out)
		}
		bodies[i] = jsonBody(t, map[string]any{"webauthn": keys[i].get(out["webauthn"].(map[string]any))})
	}
	f.srv.store = &barrierStore{Store: f.st, both: make(chan struct{})}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = p.do("DELETE", paths[i], bodies[i])
		}(i)
	}
	wg.Wait()
	deleted, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusNoContent:
			deleted++
		case http.StatusConflict:
			refused++
		}
	}
	if deleted != 1 || refused != 1 {
		t.Fatalf("two removals at once answered %v; want one 204 and one 409", codes)
	}
	if left, _ := f.st.UserPasskeyList(context.Background(), "acme", sub); len(left) != 1 {
		t.Fatalf("%d passkeys left, want 1", len(left))
	}
}
