package http

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/denn-gubsky/loomcycle/internal/authgate/external"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const extAud = "https://lc.example"

// extGate is a test external gate: its private key and the config entry
// that pins the public half.
type extGate struct {
	name string
	priv ed25519.PrivateKey
	cfg  config.AuthGate
}

var extJTI atomic.Int64

func newExtGate(t *testing.T, name string, m config.AuthIdentityMap, ext config.AuthExternalGate) *extGate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(jose.JSONWebKey{Key: pub})
	k, err := external.ParsePublicJWK(raw)
	if err != nil {
		t.Fatal(err)
	}
	ext.Key = k
	if ext.TokenTTL == 0 {
		ext.TokenTTL = 12 * time.Hour
	}
	if ext.Removal.UnbindAfter == 0 {
		ext.Removal.UnbindAfter = 7 * lcDay
	}
	if m.Provision == "" {
		m.Provision = config.ProvisionAuto
	}
	if m.NewUser.AccessMode == "" {
		m.NewUser = config.AuthNewUser{AccessMode: "isolated", Groups: []string{"everyone", "new-users"}, Status: "active", TokenTTL: 12 * time.Hour}
	}
	return &extGate{name: name, priv: priv, cfg: config.AuthGate{Name: name, Kind: config.AuthGateKindExternal, Label: name, Identity: &m, External: &ext}}
}

// sign makes a JWT under the gate's key; mutate adjusts the registered claims.
func (g *extGate) sign(t *testing.T, use string, extra map[string]any, mutate func(*jwt.Claims)) string {
	t.Helper()
	return signWith(t, g.priv, g.name, use, extra, mutate)
}

func signWith(t *testing.T, priv ed25519.PrivateKey, iss, use string, extra map[string]any, mutate func(*jwt.Claims)) string {
	t.Helper()
	now := time.Now()
	c := jwt.Claims{Issuer: iss, Audience: jwt.Audience{extAud}, Subject: "12345", ID: fmt.Sprintf("j-%d", extJTI.Add(1)),
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(2 * time.Minute))}
	if mutate != nil {
		mutate(&c)
	}
	if extra == nil {
		extra = map[string]any{}
	}
	extra["lc_use"] = use
	sg, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: priv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jwt.Signed(sg).Claims(c).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// extFixture is a gate fixture with external gates configured and the public
// URL their assertions are addressed to.
func extFixture(t *testing.T, gates ...*extGate) *gateFixture {
	t.Helper()
	f := newGateFixture(t)
	cfg := f.srv.cfg()
	cfg.Env.PublicURL = extAud
	for _, g := range gates {
		cfg.ResolvedAuth.Gates = append(cfg.ResolvedAuth.Gates, g.cfg)
	}
	return f
}

// post sends an unauthenticated JSON body (gate routes carry no bearer).
func (f *gateFixture) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(f.ts.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (f *gateFixture) assert(t *testing.T, gate, jwtStr string) (int, map[string]any) {
	t.Helper()
	return f.post(t, "/v1/_auth/assert", map[string]string{"gate": gate, "assertion": jwtStr})
}

func acmeMap() config.AuthIdentityMap { return config.AuthIdentityMap{Tenant: "acme"} }

func TestExternalAssert_IssuesAShortLivedTokenForTheProvisionedUser(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g)
	code, body := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"name": "Ann"}, nil))
	if code != http.StatusOK {
		t.Fatalf("assert: %d %v", code, body)
	}
	if body["subject"] != "tg:12345" || body["tenant"] != "acme" {
		t.Fatalf("body = %v", body)
	}
	exp, _ := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
	if d := time.Until(exp); d <= 0 || d > 12*time.Hour+time.Minute {
		t.Fatalf("issued token lives %s", d)
	}
	c, me := f.call(t, body["token"].(string), "GET", "/v1/_me", "")
	if c != http.StatusOK || me["subject"] != "tg:12345" {
		t.Fatalf("/v1/_me with the issued token: %d %v", c, me)
	}
	u, err := f.st.UserGet(context.Background(), "acme", "tg:12345")
	if err != nil || u.AccessMode != "isolated" || u.Status != "active" {
		t.Fatalf("user = %+v %v", u, err)
	}
}

// RFC FA §8: an assertion signed by gate A is refused for gate B.
func TestExternalAssert_AnAssertionSignedByGateAIsRefusedForGateB(t *testing.T) {
	a := newExtGate(t, "gate-a", acmeMap(), config.AuthExternalGate{})
	b := newExtGate(t, "gate-b", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, a, b)
	for _, iss := range []string{"gate-a", "gate-b"} {
		code, _ := f.assert(t, "gate-b", signWith(t, a.priv, iss, "assert", nil, nil))
		if code != http.StatusUnauthorized {
			t.Fatalf("A's key naming %s, posted for B: %d, want 401", iss, code)
		}
	}
	if users, _ := f.st.UserList(context.Background(), "acme"); slices.ContainsFunc(users, func(u store.UserRow) bool { return strings.HasPrefix(u.Subject, "gate-") }) {
		t.Fatal("a refused assertion provisioned a user")
	}
}

// secondServer is another replica on the same store.
func secondServer(t *testing.T, f *gateFixture) *httptest.Server {
	return serverOn(t, f, f.st)
}

// serverOn is a replica of f's server on st (f's store, or a wrapper of it).
func serverOn(t *testing.T, f *gateFixture, st store.Store) *httptest.Server {
	t.Helper()
	cfg := *f.srv.cfg()
	s2 := &Server{cfgHolder: config.NewHolder(&cfg), cancelReg: cancel.NewRegistry(), sessionLocks: runner.NewSessionLockMap(),
		hookDispatcher: hooks.NewDispatcher(hooks.NewSet(), nil), sem: concurrency.New(8, 16, 30000), store: st}
	ts := httptest.NewServer(s2.Mux())
	t.Cleanup(ts.Close)
	return ts
}

// userGetFails is a store whose UserGet fails once armed.
type userGetFails struct {
	store.Store
	armed atomic.Bool
}

func (u *userGetFails) UserGet(ctx context.Context, tenant, subject string) (store.UserRow, error) {
	if u.armed.Load() {
		return store.UserRow{}, fmt.Errorf("db down")
	}
	return u.Store.UserGet(ctx, tenant, subject)
}

// The removal policy holds when a credential is USED, not only when the
// sweeper runs: an issued token stops at once when its user is disabled —
// even if nothing retired the token (a removal that failed part-way, a
// status written behind the policy's back) and even on a replica whose token
// cache still holds it. A status that cannot be read refuses (fail closed).
func TestExternalAssert_AnIssuedTokenStopsTheMomentItsUserIsNotActive(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g)
	ctx := context.Background()
	_, body := f.assert(t, "tg", g.sign(t, "assert", nil, nil))
	tok := body["token"].(string)
	wrapped := &userGetFails{Store: f.st}
	ts2 := serverOn(t, f, wrapped)
	me := func(ts *httptest.Server) int {
		code, _ := bearerCall(t, ts, tok, "GET", "/v1/_me", "")
		return code
	}
	// Warm both replicas' token caches.
	if me(f.ts) != http.StatusOK || me(ts2) != http.StatusOK {
		t.Fatal("the issued token does not work")
	}
	// Disabled with no retire, no session revoke, no sweeper.
	if err := f.st.UserUpdate(ctx, "acme", "tg:12345", store.UserPatch{Status: strPtr("disabled")}); err != nil {
		t.Fatal(err)
	}
	if me(f.ts) != http.StatusUnauthorized || me(ts2) != http.StatusUnauthorized {
		t.Fatal("a disabled user's issued token still authenticated")
	}
	if err := f.st.UserUpdate(ctx, "acme", "tg:12345", store.UserPatch{Status: strPtr("active")}); err != nil {
		t.Fatal(err)
	}
	if me(ts2) != http.StatusOK {
		t.Fatal("re-enabled user's token refused")
	}
	wrapped.armed.Store(true)
	if me(ts2) != http.StatusUnauthorized {
		t.Fatal("a token whose user status could not be read authenticated (fail open)")
	}
	wrapped.armed.Store(false)
	// A revoke on replica 1 holds on replica 2's next request, cache or not.
	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "tg", "request": g.sign(t, "revoke", nil, nil)}); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if me(ts2) != http.StatusUnauthorized {
		t.Fatal("a revoke did not hold on the other replica's next request")
	}
}

// RFC FA §8: a second use of a jti is refused — on the same replica and on
// another replica sharing the store.
func TestExternalAssert_ASecondUseOfAJTIIsRefusedOnEveryReplica(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g)
	tok := g.sign(t, "assert", nil, nil)
	if code, body := f.assert(t, "tg", tok); code != http.StatusOK {
		t.Fatalf("first use: %d %v", code, body)
	}
	if code, _ := f.assert(t, "tg", tok); code != http.StatusUnauthorized {
		t.Fatalf("second use on the same replica: %d", code)
	}
	ts2 := secondServer(t, f)
	raw, _ := json.Marshal(map[string]string{"gate": "tg", "assertion": tok})
	resp, err := http.Post(ts2.URL+"/v1/_auth/assert", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay on another replica: %d, want 401", resp.StatusCode)
	}
}

// RFC FA §8: exp more than five minutes out, an expired assertion and a
// wrong aud are refused.
func TestExternalAssert_RefusesALongLifetimeAnExpiryAndAWrongAudience(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g)
	for name, m := range map[string]func(*jwt.Claims){
		"lifetime over 5 min": func(c *jwt.Claims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(6 * time.Minute)) },
		"expired": func(c *jwt.Claims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-3 * time.Minute))
			c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Second))
		},
		"wrong aud": func(c *jwt.Claims) { c.Audience = jwt.Audience{"https://other.example"} },
	} {
		if code, _ := f.assert(t, "tg", g.sign(t, "assert", nil, m)); code != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", name, code)
		}
	}
}

// RFC FA §8: an external gate cannot assert a tenant outside its identity
// map, and an untrusted one cannot raise access_mode or add groups.
func TestExternalAssert_TheIdentityMapNotTheGateDecidesTenantAndReach(t *testing.T) {
	m := config.AuthIdentityMap{TenantClaim: "tenant", AllowedTenants: map[string]string{"acme": "acme"}}
	g := newExtGate(t, "tg", m, config.AuthExternalGate{})
	f := extFixture(t, g)
	ctx := context.Background()
	if _, err := f.st.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "admins", ExternalID: "tg:admins"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"tenant": "other"}, nil)); code != http.StatusUnauthorized {
		t.Fatalf("a tenant outside the map: %d, want 401", code)
	}
	if users, _ := f.st.UserList(ctx, "other"); len(users) != 0 {
		t.Fatalf("a user was created in the unlisted tenant: %+v", users)
	}
	code, body := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"tenant": "acme", "access_mode": "tenant", "groups": []string{"admins"}}, nil))
	if code != http.StatusOK {
		t.Fatalf("assert: %d %v", code, body)
	}
	u, _ := f.st.UserGet(ctx, "acme", "tg:12345")
	if u.AccessMode != "isolated" {
		t.Fatalf("an untrusted gate raised access_mode to %q", u.AccessMode)
	}
	if gs, _ := f.st.GroupsOfSubject(ctx, "acme", "tg:12345"); slices.Contains(gs, "admins") {
		t.Fatalf("an untrusted gate added a group: %v", gs)
	}
}

// A trusted gate syncs its groups; an assertion that does not mention groups
// syncs nothing (a bridge that could not look memberships up strips none);
// an empty list removes them.
func TestExternalAssert_GroupsSyncOnlyWhenTheGateMentionsThem(t *testing.T) {
	m := acmeMap()
	m.Trusted, m.SyncGroups, m.GroupsProvision = true, true, true
	g := newExtGate(t, "tg", m, config.AuthExternalGate{})
	f := extFixture(t, g)
	ctx := context.Background()
	groups := func() []string {
		gs, _ := f.st.GroupsOfSubject(ctx, "acme", "tg:12345")
		return gs
	}
	if code, b := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"groups": []string{"tg-chat-1"}}, nil)); code != http.StatusOK {
		t.Fatalf("assert: %d %v", code, b)
	}
	if !slices.Contains(groups(), "tg-chat-1") {
		t.Fatalf("groups = %v, want tg-chat-1", groups())
	}
	f.assert(t, "tg", g.sign(t, "assert", nil, nil))
	if !slices.Contains(groups(), "tg-chat-1") {
		t.Fatal("an assertion without a groups claim removed the gate's group")
	}
	f.assert(t, "tg", g.sign(t, "assert", map[string]any{"groups": []string{}}, nil))
	if slices.Contains(groups(), "tg-chat-1") {
		t.Fatal("an empty groups claim kept the gate's group")
	}
}

// The issued token is held to the gate's ceiling; an assertion may narrow it
// but never widen it.
func TestExternalAssert_TheTokenRequestOnlyNarrowsTheGatesCeiling(t *testing.T) {
	ext := config.AuthExternalGate{TokenTTL: time.Hour}
	ext.TokenRestrictions.Agents = []string{"chat"}
	g := newExtGate(t, "tg", acmeMap(), ext)
	f := extFixture(t, g)
	if code, _ := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"token": map[string]any{"agents": []string{"admin"}}}, nil)); code != http.StatusBadRequest {
		t.Fatalf("asking for an agent outside the ceiling: %d, want 400", code)
	}
	code, body := f.assert(t, "tg", g.sign(t, "assert", map[string]any{"token": map[string]any{"ttl_seconds": 7200}}, nil))
	if code != http.StatusOK {
		t.Fatalf("assert: %d %v", code, body)
	}
	exp, _ := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
	if time.Until(exp) > time.Hour+time.Minute {
		t.Fatalf("a token request widened the gate's ttl: %s", time.Until(exp))
	}
}

// A pending user (the identity map's approval queue) gets no token.
func TestExternalAssert_APendingUserGetsNoToken(t *testing.T) {
	m := acmeMap()
	m.NewUser = config.AuthNewUser{AccessMode: "isolated", Groups: []string{}, Status: store.UserStatusPending, TokenTTL: time.Hour}
	g := newExtGate(t, "tg", m, config.AuthExternalGate{})
	f := extFixture(t, g)
	if code, body := f.assert(t, "tg", g.sign(t, "assert", nil, nil)); code != http.StatusForbidden || body["code"] != "awaiting_approval" {
		t.Fatalf("pending: %d %v", code, body)
	}
}

// Refusals are throttled per source: a stream of junk is held back.
func TestExternalAssert_RefusalsAreThrottledPerSource(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g)
	var last int
	for i := 0; i < 25; i++ {
		last, _ = f.assert(t, "tg", "not-a-jwt")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after 25 refused assertions: %d, want 429", last)
	}
	// A valid assertion from the throttled source waits too.
	if code, _ := f.assert(t, "tg", g.sign(t, "assert", nil, nil)); code != http.StatusTooManyRequests {
		t.Fatalf("valid assertion from a throttled source: %d", code)
	}
}

// RFC FA §8: revoke and sync not signed by that gate are refused; a signed
// revoke disables the user at once and starts its removal.
func TestExternalRevoke_OnlyTheGatesOwnSignatureRemovesItsUser(t *testing.T) {
	g := newExtGate(t, "tg", acmeMap(), config.AuthExternalGate{})
	other := newExtGate(t, "other", acmeMap(), config.AuthExternalGate{})
	f := extFixture(t, g, other)
	ctx := context.Background()
	_, body := f.assert(t, "tg", g.sign(t, "assert", nil, nil))
	tok := body["token"].(string)

	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "tg", "request": signWith(t, other.priv, "tg", "revoke", nil, nil)}); code != http.StatusUnauthorized {
		t.Fatalf("a revoke signed by another gate's key: %d", code)
	}
	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "tg", "request": g.sign(t, "assert", nil, nil)}); code != http.StatusUnauthorized {
		t.Fatalf("an assert JWT posted as a revoke: %d", code)
	}
	csub := g.sign(t, "sync", map[string]any{"users": []map[string]any{{"sub": "x"}}}, func(c *jwt.Claims) { c.Subject = "" })
	if code, _ := f.post(t, "/v1/_auth/sync", map[string]string{"gate": "other", "request": csub}); code != http.StatusUnauthorized {
		t.Fatalf("a sync signed by tg posted for other: %d", code)
	}
	if u, _ := f.st.UserGet(ctx, "acme", "tg:12345"); u.Status != "active" {
		t.Fatal("a refused revoke disabled the user")
	}

	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "tg", "request": g.sign(t, "revoke", nil, nil)}); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if u, _ := f.st.UserGet(ctx, "acme", "tg:12345"); u.Status != "disabled" {
		t.Fatalf("revoked user status %q", u.Status)
	}
	if code, _ := f.call(t, tok, "GET", "/v1/_me", ""); code != http.StatusUnauthorized {
		t.Fatalf("the issued token survived the revoke: %d", code)
	}
	if r, err := f.st.AuthRemovalGet(ctx, "acme", "tg:12345"); err != nil || r.Gate != "tg" || r.Reason != store.RemovalReasonRevoked {
		t.Fatalf("removal = %+v %v", r, err)
	}
	// A revoked user cannot assert its way back in (reactivate: operator).
	if code, _ := f.assert(t, "tg", g.sign(t, "assert", nil, nil)); code != http.StatusUnauthorized {
		t.Fatalf("a revoked user asserted again: %d", code)
	}
	// A sub the gate never linked is the same 204: no oracle.
	if code, _ := f.post(t, "/v1/_auth/revoke", map[string]string{"gate": "tg", "request": g.sign(t, "revoke", nil, func(c *jwt.Claims) { c.Subject = "nobody" })}); code != http.StatusNoContent {
		t.Fatalf("revoke of an unknown sub: %d", code)
	}
}

// An authoritative gate's complete sync removes exactly the users it no
// longer lists; repeating it changes nothing; an empty complete sync is
// refused.
func TestExternalSync_AnAuthoritativeCompleteSyncRemovesOnlyTheMissing(t *testing.T) {
	m := acmeMap()
	m.AuthoritativeForUsers = true
	g := newExtGate(t, "tg", m, config.AuthExternalGate{})
	f := extFixture(t, g)
	ctx := context.Background()
	sync := func(users []string, complete bool) (int, map[string]any) {
		list := []map[string]any{}
		for _, u := range users {
			list = append(list, map[string]any{"sub": u})
		}
		return f.post(t, "/v1/_auth/sync", map[string]string{"gate": "tg", "request": g.sign(t, "sync",
			map[string]any{"users": list, "complete": complete}, func(c *jwt.Claims) { c.Subject = "" })})
	}
	if code, body := sync([]string{"1", "2", "3"}, true); code != http.StatusOK || body["created"] != float64(3) {
		t.Fatalf("first sync: %d %v", code, body)
	}
	if code, body := sync([]string{"1", "2", "3"}, true); code != http.StatusOK || body["created"] != float64(0) || body["removed"] != float64(0) {
		t.Fatalf("repeat: %d %v", code, body)
	}
	if code, body := sync([]string{"1", "3"}, true); code != http.StatusOK || body["removed"] != float64(1) {
		t.Fatalf("drop 2: %d %v", code, body)
	}
	for sub, want := range map[string]string{"tg:1": "active", "tg:2": "disabled", "tg:3": "active"} {
		if u, _ := f.st.UserGet(ctx, "acme", sub); u.Status != want {
			t.Fatalf("%s is %q, want %q", sub, u.Status, want)
		}
	}
	if code, _ := sync(nil, true); code != http.StatusBadRequest {
		t.Fatalf("an empty complete sync: %d, want 400", code)
	}
	// Not complete: nobody is removed.
	if code, body := sync([]string{"1"}, false); code != http.StatusOK || body["removed"] != float64(0) {
		t.Fatalf("partial sync: %d %v", code, body)
	}
}

// browserFor is a cookie-keeping client that does not follow redirects.
func browserFor(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// beginLogin starts a browser login and returns the login_state the gate got.
func beginLogin(t *testing.T, f *gateFixture, hc *http.Client, gate, returnTo string) (int, string) {
	t.Helper()
	u := f.ts.URL + "/v1/_auth/login/" + gate
	if returnTo != "" {
		u += "?return_to=" + url.QueryEscape(returnTo)
	}
	resp, err := hc.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return resp.StatusCode, ""
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// A refused start goes back to the login page with an error code, as for
	// every redirect gate; the caller reads it as the state's place.
	if loc.Path == "/ui/login" {
		return resp.StatusCode, "error:" + loc.Query().Get("error")
	}
	state := loc.Query().Get("login_state")
	complete, err := url.Parse(loc.Query().Get("complete"))
	if err != nil || loc.Query().Get("aud") != extAud || !strings.HasSuffix(complete.Path, "/v1/_auth/complete/"+gate) ||
		complete.Query().Get("state") != state {
		t.Fatalf("callback query = %v", loc.Query())
	}
	return resp.StatusCode, state
}

// completeLogin is the browser coming back from the gate: the complete URL
// loomcycle gave the gate (carrying the login state) plus the assertion.
func completeLogin(t *testing.T, f *gateFixture, hc *http.Client, gate, state, assertion string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", f.ts.URL+"/v1/_auth/complete/"+gate+"?state="+url.QueryEscape(state)+
		"&assertion="+url.QueryEscape(assertion), nil)
	req.Header.Set("User-Agent", testUA)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// refusedToLoginPage reports a sign-in refused the way every redirect gate
// refuses one: a 303 to the login page with an error code, and no session.
func refusedToLoginPage(resp *http.Response) bool {
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusSeeOther || loc.Path != "/ui/login" || loc.Query().Get("error") == "" {
		return false
	}
	return !slices.ContainsFunc(resp.Cookies(), func(c *http.Cookie) bool {
		return (c.Name == sessionCookiePlain || c.Name == sessionCookieSecure) && c.Value != ""
	})
}

// RFC FA §8: a login_state from another browser is refused; the browser that
// began the login signs in and lands on its return_to.
func TestExternalLogin_TheLoginStateIsBoundToTheBrowserThatBeganIt(t *testing.T) {
	g := newExtGate(t, "hr", acmeMap(), config.AuthExternalGate{CallbackURL: "https://gate.example/login",
		AllowedReturnTo: []string{"https://app.example/welcome"}})
	f := extFixture(t, g)
	alice, mallory := browserFor(t), browserFor(t)
	code, state := beginLogin(t, f, alice, "hr", "https://app.example/welcome")
	if code != http.StatusSeeOther || state == "" {
		t.Fatalf("begin: %d %q", code, state)
	}
	assertion := g.sign(t, "login", map[string]any{"login_state": state}, nil)
	// Mallory holds a login cookie of her own (she began a login too): it is
	// not the one alice's state is bound to.
	if code, _ := beginLogin(t, f, mallory, "hr", ""); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if resp := completeLogin(t, f, mallory, "hr", state, assertion); !refusedToLoginPage(resp) {
		t.Fatalf("another browser completing the login: %d → %q, want a refusal to the login page with no session",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	// The state is spent by the attempt, so even the right browser needs a
	// new login — a stolen assertion is worth one try, to nobody.
	code, state = beginLogin(t, f, alice, "hr", "https://app.example/welcome")
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	resp := completeLogin(t, f, alice, "hr", state, g.sign(t, "login", map[string]any{"login_state": state}, nil))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://app.example/welcome" {
		t.Fatalf("complete: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !slices.ContainsFunc(resp.Cookies(), func(c *http.Cookie) bool {
		return (c.Name == sessionCookiePlain || c.Name == sessionCookieSecure) && c.Value != ""
	}) {
		t.Fatal("no session cookie after the login")
	}
	req, _ := http.NewRequest("GET", f.ts.URL+"/v1/_me", nil)
	req.Header.Set("User-Agent", testUA)
	me, err := alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var who map[string]any
	_ = json.NewDecoder(me.Body).Decode(&who)
	me.Body.Close()
	if me.StatusCode != http.StatusOK || who["subject"] != "hr:12345" {
		t.Fatalf("/v1/_me through the gate session: %d %v", me.StatusCode, who)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("the complete page lets the assertion leak through Referer")
	}
}

// RFC FA §8: return_to outside allowed_return_to is refused before anything
// is stored or the browser leaves.
func TestExternalLogin_ReturnToOutsideTheAllowlistIsRefused(t *testing.T) {
	g := newExtGate(t, "hr", acmeMap(), config.AuthExternalGate{CallbackURL: "https://gate.example/login",
		AllowedReturnTo: []string{"https://app.example/welcome"}})
	f := extFixture(t, g)
	for _, rt := range []string{"https://evil.example/", "https://app.example/welcome?x=1", "//evil.example", "/v1/_users"} {
		if code, state := beginLogin(t, f, browserFor(t), "hr", rt); code != http.StatusSeeOther || state != "error:return_to_refused" {
			t.Fatalf("return_to %q: %d %q, want 303 to the login page with return_to_refused", rt, code, state)
		}
	}
	// An assert-only gate has no browser login at all.
	b := newExtGate(t, "bridge", acmeMap(), config.AuthExternalGate{})
	f2 := extFixture(t, b)
	if code, _ := beginLogin(t, f2, browserFor(t), "bridge", ""); code != http.StatusNotFound {
		t.Fatalf("login through an assert-only gate: %d", code)
	}
}

// GET /v1/_authgate lists the gates for an admin, with the external gate's
// key fingerprint (never key material) and, on ?probe=1, what its discovery
// document says.
func TestAuthgateList_ShowsGatesAndProbesExternalOnes(t *testing.T) {
	g := newExtGate(t, "hr", acmeMap(), config.AuthExternalGate{})
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/loomcycle-authgate" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "hr", "kind": "external", "fingerprint": g.cfg.External.Key.Fingerprint(), "capabilities": []string{"assert"}})
	}))
	defer disc.Close()
	g.cfg.External.DiscoveryURL = disc.URL
	f := extFixture(t, g)
	code, body := f.call(t, bAdmin, "GET", "/v1/_authgate?probe=1", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"name_matches":true`) || !strings.Contains(string(raw), `"key_matches":true`) ||
		!strings.Contains(string(raw), g.cfg.External.Key.Fingerprint()) {
		t.Fatalf("list = %s", raw)
	}
	if code, _ := f.call(t, bAcmeOp, "GET", "/v1/_authgate", ""); code != http.StatusForbidden {
		t.Fatalf("a tenant operator listed the gates: %d", code)
	}
}
