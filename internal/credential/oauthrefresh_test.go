package credential

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// sub substitutes "Bearer $oauth:<name>" for tenant t's run (agent and user
// as given) toward the fake's resource.
func (h *oauthHarness) sub(ctx context.Context, agent, user, name string) (string, error) {
	out, un, err := h.m.Substitute(netguard.WithPinnable(ctx), "t", agent, user, "Bearer $oauth:"+name, h.f.Resource(), nil)
	if err == nil && len(un) > 0 {
		return "", errors.New("unresolved: " + strings.Join(un, ","))
	}
	return strings.TrimPrefix(out, "Bearer "), err
}

func TestOAuthSubstitute_ReturnsCachedTokenAboveThreshold(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	first := h.f.LastAccess()
	h.clock.advance(239 * time.Second) // 61 s left of 300: above a minute
	got, err := h.sub(context.Background(), "", "", "docs")
	if err != nil || got != first {
		t.Fatalf("got %.6s… %v, want the cached token", got, err)
	}
	if n := h.f.Count("token"); n != 1 { // the connect's own
		t.Errorf("%d token requests, want only the connect's", n)
	}
}

func TestOAuthSubstitute_RefreshesWithinMax60sOr10Percent(t *testing.T) {
	for _, tc := range []struct {
		ttl            int64
		cached, missed time.Duration
	}{
		{300, 239 * time.Second, 241 * time.Second},    // a minute
		{1200, 1079 * time.Second, 1081 * time.Second}, // a tenth: two minutes
	} {
		h := newOAuthHarness(t)
		h.f.Set(func(f *oauthtest.Server) { f.AccessTTL = tc.ttl })
		id := oauthID("docs")
		h.connect(t, id)
		first := h.f.LastAccess()
		start := h.clock.now()
		h.clock.advance(tc.cached)
		if got, err := h.sub(context.Background(), "", "", "docs"); err != nil || got != first {
			t.Errorf("ttl %d at %v: refreshed (%v)", tc.ttl, tc.cached, err)
		}
		h.clock.t = start.Add(tc.missed)
		got, err := h.sub(context.Background(), "", "", "docs")
		if err != nil || got == first || got != h.f.LastAccess() {
			t.Errorf("ttl %d at %v: not refreshed (%v)", tc.ttl, tc.missed, err)
		}
	}
}

func TestOAuthRefresh_PersistsRotatedRefreshTokenBeforeReturningAccess(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.clock.advance(290 * time.Second)
	var registered []string
	out, _, err := h.m.Substitute(netguard.WithPinnable(context.Background()), "t", "", "", "$oauth:docs", h.f.Resource(),
		func(v string) {
			registered = append(registered, v)
			// When the token is handed out, the rotated grant is stored.
			_, sec := h.secretOf(t, id)
			var latestRT string
			h.f.Set(func(f *oauthtest.Server) { latestRT = f.IssuedRefresh[len(f.IssuedRefresh)-1] })
			if sec.RefreshToken != latestRT || sec.AccessToken != v {
				t.Error("the access token was handed out before the rotated grant was stored")
			}
		})
	if err != nil || out != h.f.LastAccess() || len(registered) != 1 || registered[0] != out {
		t.Fatalf("out %.6s… registered %d err %v", out, len(registered), err)
	}
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.LastRefreshAt == nil || m.OAuth.Status.AccessExpiresAt == nil || m.OAuth.Status.State != OAuthConnected {
		t.Errorf("status %+v", m.OAuth.Status)
	}
}

// roundTripHook calls before ahead of each request it sends.
type roundTripHook struct {
	base   http.RoundTripper
	before func()
}

func (r roundTripHook) RoundTrip(req *http.Request) (*http.Response, error) {
	r.before()
	return r.base.RoundTrip(req)
}

// failingStore fails the conditional writes it is told to.
type failingStore struct {
	Store
	failPutIf atomic.Int32 // fail the next n
}

func (s *failingStore) CredentialDefPutIf(ctx context.Context, row credentialdef.Row, prev json.RawMessage) (bool, error) {
	if s.failPutIf.Load() > 0 {
		s.failPutIf.Add(-1)
		return false, errors.New("injected store failure")
	}
	return s.Store.CredentialDefPutIf(ctx, row, prev)
}

func TestOAuthRefresh_PersistFailureSetsNeedsReauth(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs := &failingStore{Store: st}
	h := newOAuthHarnessOn(t, fs, oauthtest.New(t), "replica-a")
	id := oauthID("docs")
	h.connect(t, id)
	h.clock.advance(290 * time.Second)
	// The lease write lands; the write of the rotated grant fails.
	var calls atomic.Int32
	h.m.client.http.Transport = roundTripHook{base: h.m.client.http.Transport, before: func() {
		if calls.Add(1) == 1 {
			fs.failPutIf.Store(1)
		}
	}}
	_, err = h.sub(context.Background(), "", "", "docs")
	if !errors.Is(err, ErrOAuthReauthRequired) {
		t.Fatalf("err = %v, want ErrOAuthReauthRequired", err)
	}
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.State != OAuthNeedsReauth {
		t.Errorf("state %q, want needs_reauth", m.OAuth.Status.State)
	}
	if !slices.Contains(h.sink.actions(), "oauth_needs_reauth") {
		t.Errorf("actions %v", h.sink.actions())
	}
}

func TestOAuthRefresh_InvalidGrantSetsNeedsReauthAndAudits(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	// The grant is revoked at the server behind the runtime's back.
	_, sec := h.secretOf(t, id)
	if err := (&oauthClient{http: h.f.Client()}).revoke(context.Background(), testFakeCfg(h), sec); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(290 * time.Second)
	_, err := h.sub(context.Background(), "", "", "docs")
	var re *OAuthReauthError
	if !errors.As(err, &re) || re.Reason != "invalid_grant" || re.Name != "docs" {
		t.Fatalf("err = %v, want a reauth error for invalid_grant", err)
	}
	if strings.Contains(err.Error(), sec.RefreshToken) {
		t.Error("the error quotes the refresh token")
	}
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.State != OAuthNeedsReauth || m.OAuth.Status.LastError != "invalid_grant" {
		t.Errorf("status %+v", m.OAuth.Status)
	}
	_, after := h.secretOf(t, id)
	if after.RefreshToken != "" || after.AccessToken != "" || after.ClientID == "" {
		t.Error("the grant was kept, or the registration dropped")
	}
	if !slices.Contains(h.sink.actions(), "oauth_needs_reauth") {
		t.Errorf("actions %v", h.sink.actions())
	}
	// From now on it refuses at once, without asking the server.
	before := h.f.Count("token")
	if _, err := h.sub(context.Background(), "", "", "docs"); !errors.Is(err, ErrOAuthReauthRequired) {
		t.Errorf("err = %v", err)
	}
	if h.f.Count("token") != before {
		t.Error("a needs_reauth credential asked the server")
	}
}

func testFakeCfg(h *oauthHarness) OAuthConfig {
	return OAuthConfig{Resource: h.f.Resource(), Client: OAuthClientConfig{Registration: OAuthRegistrationDynamic, TokenEndpointAuth: OAuthAuthNone}}
}

func TestOAuthRefresh_5xxKeepsStateAndBacksOff(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	first := h.f.LastAccess()
	// Within the threshold, still valid: a 5xx sends the token held.
	h.f.Set(func(f *oauthtest.Server) { f.Fail5xx = 1 })
	h.clock.advance(250 * time.Second)
	if got, err := h.sub(context.Background(), "", "", "docs"); err != nil || got != first {
		t.Fatalf("a 5xx within validity: %.6s… %v, want the held token", got, err)
	}
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.State != OAuthConnected {
		t.Errorf("state %q after a 5xx", m.OAuth.Status.State)
	}
	if r, _ := h.e.getOAuth(context.Background(), id); r.def.Lease != nil {
		t.Error("the lease was kept after a 5xx")
	}
	// Backing off: no request until it passes.
	n := h.f.Count("token")
	if _, err := h.sub(context.Background(), "", "", "docs"); err != nil {
		t.Fatal(err)
	}
	if h.f.Count("token") != n {
		t.Error("a refresh was tried while backing off")
	}
	// Expired and backing off: a transient refusal, not a reauth.
	h.f.Set(func(f *oauthtest.Server) { f.Fail5xx = 1 })
	h.clock.advance(3 * time.Second)
	h.clock.advance(60 * time.Second) // past expiry and past the 2 s backoff
	if _, err := h.sub(context.Background(), "", "", "docs"); !errors.Is(err, ErrOAuthUnavailable) {
		t.Fatalf("err = %v, want ErrOAuthUnavailable", err)
	}
	h.clock.advance(5 * time.Second)
	got, err := h.sub(context.Background(), "", "", "docs")
	if err != nil || got != h.f.LastAccess() || got == first {
		t.Errorf("after the backoff: %v", err)
	}
}

func TestOAuthRefresh_SendsResourceParameter(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.RequireResource = false })
	id := oauthID("docs")
	h.connect(t, id)
	h.clock.advance(290 * time.Second)
	if _, err := h.sub(context.Background(), "", "", "docs"); err != nil {
		t.Fatal(err)
	}
	var form map[string][]string
	h.f.Set(func(f *oauthtest.Server) { form = f.TokenForms[len(f.TokenForms)-1] })
	if form["grant_type"][0] != "refresh_token" || form["resource"][0] != h.f.Resource() {
		t.Errorf("refresh form %v, want resource", form)
	}
}

func TestOAuthRefresh_ConcurrentCallersMakeOneTokenCall(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.f.Set(func(f *oauthtest.Server) { f.TokenDelay = 50 * time.Millisecond })
	h.clock.advance(290 * time.Second)
	before := h.f.Count("token")
	var wg sync.WaitGroup
	toks := make([]string, 50)
	errs := make([]error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toks[i], errs[i] = h.sub(context.Background(), "", "", "docs")
		}()
	}
	wg.Wait()
	if n := h.f.Count("token") - before; n != 1 {
		t.Errorf("%d token requests, want exactly 1", n)
	}
	for i := range toks {
		if errs[i] != nil || toks[i] != h.f.LastAccess() {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
}

// Two replicas over one store: the one that takes the lease refreshes, the
// other waits for its write instead of spending the refresh token again.
func TestOAuthRefresh_TwoManagersOneStoreMakeOneTokenCall(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := oauthtest.New(t)
	a := newOAuthHarnessOn(t, st, f, "replica-a")
	b := newOAuthHarnessOn(t, st, f, "replica-b")
	id := oauthID("docs")
	a.connect(t, id)
	f.Set(func(f *oauthtest.Server) { f.TokenDelay = 100 * time.Millisecond })
	// Past expiry: the waiting replica has nothing valid to send meanwhile.
	// One clock reading for both replicas: a lease is judged on each one's
	// own clock.
	later := a.clock.now().Add(301 * time.Second)
	for _, h := range []*oauthHarness{a, b} {
		h.clock.t = later
		h.m.leasePoll = 5 * time.Millisecond
		h.m.wait = sleepCtx // real time: the lease must not lapse on a fake clock
	}
	before := f.Count("token")
	var wg sync.WaitGroup
	var errA, errB error
	var tokA, tokB string
	wg.Add(2)
	go func() { defer wg.Done(); tokA, errA = a.sub(context.Background(), "", "", "docs") }()
	go func() { defer wg.Done(); tokB, errB = b.sub(context.Background(), "", "", "docs") }()
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("a: %v, b: %v", errA, errB)
	}
	if n := f.Count("token") - before; n != 1 {
		t.Errorf("%d token requests, want exactly 1", n)
	}
	if tokA != tokB || tokA != f.LastAccess() {
		t.Error("the replicas sent different tokens")
	}
}

func TestOAuthRefresh_CancelledCallerDoesNotLoseRotation(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.f.Set(func(f *oauthtest.Server) { f.TokenDelay = 150 * time.Millisecond })
	h.clock.advance(290 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.sub(ctx, "", "", "docs"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	// The flight goes on and stores the rotated grant.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, sec := h.secretOf(t, id)
		var latestRT string
		h.f.Set(func(f *oauthtest.Server) { latestRT = f.IssuedRefresh[len(f.IssuedRefresh)-1] })
		if sec.RefreshToken == latestRT && sec.AccessToken == h.f.LastAccess() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated grant was not stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := h.f.Count("token")
	if got, err := h.sub(context.Background(), "", "", "docs"); err != nil || got != h.f.LastAccess() {
		t.Fatalf("after: %v", err)
	}
	if h.f.Count("token") != n {
		t.Error("the stored grant was refreshed again")
	}
}

func TestOAuthRefresh_ExpiredLeaseIsTakenOver(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.clock.advance(301 * time.Second)
	r, _ := h.e.getOAuth(context.Background(), id)
	def := r.def
	def.Lease = &oauthLease{Holder: "replica-gone", Purpose: leaseRefresh, Until: h.clock.now().Add(-time.Second)}
	if _, ok, err := h.e.casOAuth(context.Background(), r, def, nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, err := h.sub(context.Background(), "", "", "docs")
	if err != nil || got != h.f.LastAccess() {
		t.Fatalf("an expired lease was not taken over: %v", err)
	}
}

func TestOAuthSubstitute_RestrictedToAllowedURLs(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	before := h.f.Count("token")
	_, _, err := h.m.Substitute(netguard.WithPinnable(context.Background()), "t", "", "", "$oauth:docs", "https://elsewhere.example.test/mcp", nil)
	if !errors.Is(err, ErrURLNotAllowed) {
		t.Errorf("err = %v, want ErrURLNotAllowed", err)
	}
	if h.f.Count("token") != before {
		t.Error("the server was asked for a request it may not see")
	}
}

func TestOAuthSubstitute_RequiresPinnableContext(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	_, _, err := h.m.Substitute(context.Background(), "t", "", "", "$oauth:docs", h.f.Resource(), nil)
	if !errors.Is(err, ErrNotPinnable) {
		t.Errorf("err = %v, want ErrNotPinnable", err)
	}
}

func TestOAuthSubstitute_UserScopeShadowsTenant(t *testing.T) {
	h := newOAuthHarness(t)
	h.connect(t, oauthID("docs"))
	tenantTok := h.f.LastAccess()
	h.connect(t, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "docs"})
	aliceTok := h.f.LastAccess()
	if got, err := h.sub(context.Background(), "", "alice", "docs"); err != nil || got != aliceTok {
		t.Errorf("alice: %v", err)
	}
	if got, err := h.sub(context.Background(), "", "bob", "docs"); err != nil || got != tenantTok {
		t.Errorf("bob: %v", err)
	}
	// An unknown name is unresolved, not an error; a $cred: one is refused.
	if _, un, err := h.m.Substitute(netguard.WithPinnable(context.Background()), "t", "", "", "$oauth:absent", h.f.Resource(), nil); err != nil || len(un) != 1 {
		t.Errorf("absent: %v %v", un, err)
	}
	if _, err := h.e.PutInline(context.Background(), oauthID("plain"), "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sub(context.Background(), "", "", "plain"); !errors.Is(err, ErrNotOAuth) {
		t.Errorf("an ordinary credential as $oauth: %v", err)
	}
}

func TestOAuthSubstitute_PendingConnectIsNotConnected(t *testing.T) {
	h := newOAuthHarness(t)
	h.create(t, oauthID("docs"))
	if _, err := h.sub(context.Background(), "", "", "docs"); !errors.Is(err, ErrOAuthNotConnected) || !HeldButUnusable(err) {
		t.Errorf("err = %v, want ErrOAuthNotConnected", err)
	}
}
