package credential

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// force force-refreshes "docs" for tenant t, as a server refused rejected.
func (h *oauthHarness) force(ctx context.Context, rejected string) (string, error) {
	tok, found, err := h.m.ForceRefresh(netguard.WithPinnable(ctx), "t", "", "", "docs", h.f.Resource(), rejected)
	if err == nil && !found {
		return "", errors.New("not found")
	}
	return tok, err
}

func TestOAuthForceRefresh_ReplacesAnUnexpiredRejectedToken(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	first := h.f.LastAccess()
	got, err := h.force(context.Background(), first)
	if err != nil || got == first || got != h.f.LastAccess() {
		t.Fatalf("got a new token %v, err %v", got != first, err)
	}
	if n := h.f.Count("token"); n != 2 {
		t.Errorf("%d token requests, want the connect's and one refresh", n)
	}
}

func TestOAuthForceRefresh_SendsANewerTokenWithoutRefreshing(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	first := h.f.LastAccess()
	// Someone refreshed after the rejected token was issued: the one stored
	// now is newer, and is sent as it is.
	got, err := h.force(context.Background(), "at-an-older-token")
	if err != nil || got != first {
		t.Fatalf("got the stored token %v, err %v", got == first, err)
	}
	if n := h.f.Count("token"); n != 1 {
		t.Errorf("%d token requests, want only the connect's", n)
	}
}

func TestOAuthForceRefresh_ConcurrentRejectionsShareOneRefresh(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	first := h.f.LastAccess()
	h.f.Set(func(f *oauthtest.Server) { f.TokenDelay = 50 * time.Millisecond })
	const n = 16
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], errs[i] = h.force(context.Background(), first)
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || got[i] != h.f.LastAccess() {
			t.Errorf("caller %d: newest %v, err %v", i, got[i] == h.f.LastAccess(), errs[i])
		}
	}
	if c := h.f.Count("token"); c != 2 {
		t.Errorf("%d token requests, want the connect's and ONE refresh", c)
	}
}

func TestOAuthForceRefresh_UnavailableServerDoesNotResendTheRejectedToken(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	first := h.f.LastAccess()
	h.f.Set(func(f *oauthtest.Server) { f.Fail5xx = 1 })
	got, err := h.force(context.Background(), first)
	if !errors.Is(err, ErrOAuthUnavailable) || got == first {
		t.Fatalf("err = %v, resent the rejected token %v", err, got == first)
	}
	// Backing off: no second request, and still not the rejected token.
	got, err = h.force(context.Background(), first)
	if !errors.Is(err, ErrOAuthUnavailable) || got == first {
		t.Fatalf("while backing off: err = %v, resent %v", err, got == first)
	}
	if n := h.f.Count("token"); n != 2 {
		t.Errorf("%d token requests, want the connect's and one attempt", n)
	}
}

// subAttempt substitutes "Bearer $oauth:docs" under a send attempt.
func (h *oauthHarness) subAttempt(a *OAuthAttempt) (string, error) {
	ctx := WithOAuthAttempt(netguard.WithPinnable(context.Background()), a)
	out, _, err := h.m.Substitute(ctx, "t", "", "", "Bearer $oauth:docs", h.f.Resource(), nil)
	return strings.TrimPrefix(out, "Bearer "), err
}

func TestOAuthAttempt_RejectionRefreshesThenSecondMovesToNeedsReauth(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	first := h.f.LastAccess()
	a := NewOAuthAttempt()
	if a.Sent() {
		t.Fatal("an empty attempt reports a send")
	}
	got, err := h.subAttempt(a)
	if err != nil || got != first || !a.Sent() {
		t.Fatalf("first send: %v sent %v", err, a.Sent())
	}
	if err := a.Rejected(context.Background()); err != nil {
		t.Fatalf("first rejection: %v", err)
	}
	second, err := h.subAttempt(a)
	if err != nil || second == first || second != h.f.LastAccess() {
		t.Fatalf("the retry did not send a refreshed token (%v)", err)
	}
	err = a.Rejected(context.Background())
	var re *OAuthReauthError
	if !errors.As(err, &re) || re.Name != "docs" {
		t.Fatalf("second rejection: %v, want an OAuthReauthError", err)
	}
	if strings.Contains(err.Error(), first) || strings.Contains(err.Error(), second) {
		t.Error("the error quotes a token")
	}
	r, _ := h.secretOf(t, id)
	if r.def.Status.State != OAuthNeedsReauth {
		t.Errorf("state %q, want needs_reauth", r.def.Status.State)
	}
	if !strings.Contains(strings.Join(h.sink.actions(), ","), "oauth_needs_reauth") {
		t.Errorf("audit %v, want oauth_needs_reauth", h.sink.actions())
	}
}

// A second rejection of a token that is no longer the stored one (someone
// refreshed since) fails the call but leaves the newer grant alone.
func TestOAuthAttempt_SecondRejectionOfAStaleTokenKeepsTheNewerGrant(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	a := NewOAuthAttempt()
	if _, err := h.subAttempt(a); err != nil {
		t.Fatal(err)
	}
	_ = a.Rejected(context.Background())
	retried, err := h.subAttempt(a)
	if err != nil {
		t.Fatal(err)
	}
	// Another caller refreshes after the retry's token was rejected.
	if _, err := h.force(context.Background(), retried); err != nil {
		t.Fatal(err)
	}
	if err := a.Rejected(context.Background()); !errors.Is(err, ErrOAuthReauthRequired) {
		t.Fatalf("second rejection: %v", err)
	}
	if r, _ := h.secretOf(t, id); r.def.Status.State != OAuthConnected {
		t.Errorf("state %q, want the newer grant kept", r.def.Status.State)
	}
}

// A retry may substitute more than once (a re-initialize, then the request
// itself): each is measured against the token the server refused, so the
// second does not take the first's fresh token for a refused one.
func TestOAuthAttempt_RetrySubstitutionsShareOneRefresh(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	a := NewOAuthAttempt()
	if _, err := h.subAttempt(a); err != nil {
		t.Fatal(err)
	}
	_ = a.Rejected(context.Background())
	reinit, err := h.subAttempt(a)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := h.subAttempt(a)
	if err != nil || retry != reinit {
		t.Fatalf("the retry sent another token than the re-initialize (%v)", err)
	}
	if n := h.f.Count("token"); n != 2 {
		t.Errorf("%d token requests, want the connect's and one refresh", n)
	}
}
