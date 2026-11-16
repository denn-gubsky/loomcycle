package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// RFC ES #6 / D2 — the derived token pepper and the older-form lookups.

func pepperKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// derivedPepperServer is a Server with NO explicit pepper and the given
// secret keys, so the effective pepper is derived. No token cache: every
// resolve goes to the store, which is what these tests observe.
func derivedPepperServer(t *testing.T, env config.Env) (*Server, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Env: env}
	s := &Server{
		cfgHolder:      config.NewHolder(cfg),
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookDispatcher: hooks.NewDispatcher(hooks.NewSet(), nil),
		sem:            concurrency.New(8, 16, 30000),
		store:          st,
	}
	return s, st
}

func hasherFor(t *testing.T, env config.Env) auth.TokenHasher {
	t.Helper()
	h, err := env.TokenHasher()
	if err != nil {
		t.Fatalf("TokenHasher: %v", err)
	}
	return h
}

func seedTokenHash(t *testing.T, st store.Store, defID, hash string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: defID, Name: defID, TenantID: "acme", Subject: defID,
		TokenHash: hash, AllowedScopes: []string{auth.ScopeRunsCreate},
	}); err != nil {
		t.Fatalf("seed %s: %v", defID, err)
	}
}

func storedHash(t *testing.T, st store.Store, defID string) string {
	t.Helper()
	row, err := st.OperatorTokenDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get %s: %v", defID, err)
	}
	return row.TokenHash
}

// A LOOMCYCLE_SECRET_KEY rotation (old key moved to _PREVIOUS) logs nobody
// out: a token hashed under the pepper the old key derived still logs in,
// and is moved onto the new one.
func TestTokenLookup_PreviousSecretKeyRowStillAuthenticates(t *testing.T) {
	oldEnv := config.Env{SecretKey: pepperKey(1)}
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000d0", hasherFor(t, oldEnv).Hash("lct_prev"))
	bootTokenHashes(t, s)

	p, ok := s.resolvePrincipal(context.Background(), "lct_prev")
	if !ok || p.TokenDefID != "def_00000000000000d0" {
		t.Fatalf("token hashed under the previous key's pepper refused: ok=%v", ok)
	}
	if got, want := storedHash(t, st, "def_00000000000000d0"), hasherFor(t, env).Hash("lct_prev"); got != want {
		t.Errorf("row not rehashed onto the current key: %q, want %q", got, want)
	}
}

// Every mint uses the effective pepper: a user token minted with no explicit
// pepper is stored under the derived hash and resolves.
func TestTokenMint_UserTokenStoredUnderTheDerivedPepper(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	bootTokenHashes(t, s)
	seedUser(t, st, "acme", "alice", "isolated", "active")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleMintUserToken(rec, bodyReq("POST", "/v1/_users/alice/tokens", "", acme, "subject", "alice"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint status %d: %s", rec.Code, rec.Body.String())
	}
	var resp mintUserTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	plaintext := resp.Token
	row, err := st.OperatorTokenDefGetByTokenHash(context.Background(), hasherFor(t, env).Hash(plaintext))
	if err != nil {
		t.Fatalf("minted token not stored under the derived hash: %v", err)
	}
	var nf *store.ErrNotFound
	if _, err := st.OperatorTokenDefGetByTokenHash(context.Background(), auth.HashToken("", plaintext)); !errors.As(err, &nf) {
		t.Errorf("minted token also stored unpeppered: %v", err)
	}
	if p, ok := s.resolvePrincipal(context.Background(), plaintext); !ok || p.TokenDefID != row.DefID {
		t.Errorf("minted token does not resolve: ok=%v", ok)
	}
}

// racingTokenStore runs `during` once, right after the lookup of hash `at`,
// to put a concurrent first login (another request or replica finishing the
// rehash) at a chosen point of this request's lookup.
type racingTokenStore struct {
	store.Store
	at     string
	during func()
	fired  bool
}

func (r *racingTokenStore) OperatorTokenDefGetByTokenHash(ctx context.Context, h string) (store.OperatorTokenDefRow, error) {
	row, err := r.Store.OperatorTokenDefGetByTokenHash(ctx, h)
	if h == r.at && !r.fired {
		r.fired = true
		r.during()
	}
	return row, err
}

// loggedTokenLookupFailure reports whether out holds the token lookup's own
// failure lines. The capture is process-wide, so it also holds what other
// tests' leftover goroutines log ("UpdateHeartbeat failed" from a closed
// store failed this check on CI); matching any "failed" was a flake.
func loggedTokenLookupFailure(out string) bool {
	return strings.Contains(out, "token rehash onto the current pepper failed") ||
		strings.Contains(out, "token-substrate lookup error")
}

// Two first logins of one rewrapped token at once. The other one rehashes the
// row between this request's miss on the current hash and its wrapped-form
// lookup, so the wrapped form no longer matches. This request must still
// authenticate, and must not leave a refusal in the cache for the next one.
func TestTokenLookup_ConcurrentFirstLoginRehashedMidLookupAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	rewrapTokenHashes(t, s)
	bootTokenHashes(t, s)
	old := hasherFor(t, env).WrappedHash("lct_old")
	cur := hasherFor(t, env).Hash("lct_old")
	s.store = &racingTokenStore{Store: st, at: cur, during: func() {
		if _, err := st.OperatorTokenRehash(context.Background(), "def_00000000000000a1", old, cur); err != nil {
			t.Error(err)
		}
	}}
	s.tokenCache = newTokenCache(30 * time.Second)
	logs, restore := captureLog(t)
	defer restore()

	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_00000000000000a1" {
		t.Fatalf("racing first login refused: ok=%v", ok)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the next login was refused (a negative was cached)")
	}
	if loggedTokenLookupFailure(logs()) {
		t.Errorf("the race was logged as a failure: %s", logs())
	}
}

// The other ordering: this request finds the row under its wrapped form, and
// the other login finishes the rehash before this one's compare-and-set,
// which then loses.
func TestTokenLookup_ConcurrentFirstLoginLosingTheRehashAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	rewrapTokenHashes(t, s)
	bootTokenHashes(t, s)
	old := hasherFor(t, env).WrappedHash("lct_old")
	cur := hasherFor(t, env).Hash("lct_old")
	s.store = &racingTokenStore{Store: st, at: old, during: func() {
		if _, err := st.OperatorTokenRehash(context.Background(), "def_00000000000000a1", old, cur); err != nil {
			t.Error(err)
		}
	}}
	s.tokenCache = newTokenCache(30 * time.Second)
	logs, restore := captureLog(t)
	defer restore()

	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_00000000000000a1" {
		t.Fatalf("racing first login refused: ok=%v", ok)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the next login was refused (a negative was cached)")
	}
	if loggedTokenLookupFailure(logs()) {
		t.Errorf("the race was logged as a failure: %s", logs())
	}
}

// A refusal reached through the older-form lookups is not cached: it may be
// the losing side of a concurrent first login, and a cached refusal would
// lock a valid token out for the whole TTL.
func TestTokenLookup_NegativeAfterOlderFormLookupIsNotCached(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	rewrapTokenHashes(t, s)
	bootTokenHashes(t, s)
	s.tokenCache = newTokenCache(30 * time.Second)

	if _, ok := s.resolvePrincipal(context.Background(), "lct_unknown"); ok {
		t.Fatal("an unknown bearer authenticated")
	}
	if _, _, cached := s.tokenCache.get(hasherFor(t, env).Hash("lct_unknown")); cached {
		t.Error("a refusal that went through the older-form lookups was cached")
	}
}

// RFC GN: LOOMCYCLE_OPERATOR_TOKEN_PEPPER is legacy. Set beside
// LOOMCYCLE_SECRET_KEY, a token its pepper hashed still logs in, and that login
// moves the row onto the derived pepper, so the variable can then go.
func TestTokenLookup_LegacyPepperRowLogsInAndMovesOntoTheDerivedPepper(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), OperatorTokenPepper: "pep"}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000e0", auth.HashToken("pep", "lct_old"))
	bootTokenHashes(t, s)

	p, ok := s.resolvePrincipal(context.Background(), "lct_old")
	if !ok || p.TokenDefID != "def_00000000000000e0" {
		t.Fatalf("token hashed under the legacy pepper refused: ok=%v", ok)
	}
	derivedOnly := config.Env{SecretKey: pepperKey(1)}
	if got, want := storedHash(t, st, "def_00000000000000e0"), hasherFor(t, derivedOnly).Hash("lct_old"); got != want {
		t.Fatalf("row not moved onto the derived pepper: %q, want %q", got, want)
	}

	// The variable removed: the moved token keeps working.
	s2, _ := derivedPepperServer(t, derivedOnly)
	s2.store = st
	bootTokenHashes(t, s2)
	if _, ok := s2.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("a token moved at login must log in once LOOMCYCLE_OPERATOR_TOKEN_PEPPER is gone")
	}
}

// The legacy pepper does not bring back an unpeppered row: the lookup computes
// the legacy hash from the presented token, and SHA-256(token) is not it.
func TestTokenLookup_LegacyPepperDoesNotRescueAnUnpepperedRow(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), OperatorTokenPepper: "pep"}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000e1", auth.HashToken("", "lct_bare"))
	bootTokenHashes(t, s)
	if _, ok := s.resolvePrincipal(context.Background(), "lct_bare"); ok {
		t.Fatal("an unpeppered row must stay refused with a legacy pepper set")
	}
	if got := storedHash(t, st, "def_00000000000000e1"); got != auth.HashToken("", "lct_bare") {
		t.Errorf("the unpeppered row was rewritten: %q", got)
	}
}

// Boot counts the rows still under the legacy pepper, so the operator knows
// when the variable can go; the rewrap refuses meanwhile, since wrapping a
// legacy row as an unpeppered one would end its token.
func TestReportTokenHashes_CountsRowsLeftUnderTheLegacyPepper(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), OperatorTokenPepper: "pep"}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000e2", auth.HashToken("pep", "lct_left"))
	seedTokenHash(t, st, "def_00000000000000e3", hasherFor(t, env).Hash("lct_new"))
	rep, err := s.ReportTokenHashes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.LegacyPepper || len(rep.Untagged) != 1 || rep.Untagged[0] != "def_00000000000000e2" || rep.Current != 1 {
		t.Errorf("report = %+v, want the legacy pepper flagged, one row left under it and one current", rep)
	}
	if _, err := ListUntaggedTokenRows(context.Background(), st, hasherFor(t, env)); err == nil {
		t.Error("the rewrap review must refuse while LOOMCYCLE_OPERATOR_TOKEN_PEPPER is set")
	}
	row, _ := st.OperatorTokenDefGet(context.Background(), "def_00000000000000e2")
	if _, _, err := RewrapTokenRows(context.Background(), st, hasherFor(t, env), []store.OperatorTokenDefRow{row}); err == nil {
		t.Error("the rewrap must refuse while LOOMCYCLE_OPERATOR_TOKEN_PEPPER is set")
	}
}

// While the legacy pepper is kept, an admin row still under it can log in, so
// it switches LOOMCYCLE_AUTH_TOKEN off as it did under the explicit pepper:
// the upgrade must not turn the shared secret back on.
func TestLegacyFallback_AdminRowUnderTheLegacyPepperKeepsTheSharedSecretOff(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), OperatorTokenPepper: "pep", AuthToken: "legacy-shared-secret"}
	s, st := derivedPepperServer(t, env)
	seedAdminTokenHash(t, st, "def_00000000000000e4", auth.HashToken("pep", "lct_admin"))
	bootTokenHashes(t, s)
	if _, ok := s.resolvePrincipal(context.Background(), "legacy-shared-secret"); ok {
		t.Error("an admin token under the legacy pepper left LOOMCYCLE_AUTH_TOKEN enabled")
	}
}
