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

// RFC ES #6 / D2 — the derived token pepper and the legacy-hash fallback.

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

func snapshotLegacy(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.SnapshotLegacyTokenRows(context.Background()); err != nil {
		t.Fatalf("SnapshotLegacyTokenRows: %v", err)
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

// A token minted before any pepper existed still logs in once the secret key
// derives one — and that login moves its row onto the current hash, so the
// unpeppered form stops working for it.
func TestTokenLookup_LegacyUnpepperedRowRehashedOnFirstLogin(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	snapshotLegacy(t, s)

	p, ok := s.resolvePrincipal(context.Background(), "lct_old")
	if !ok || p.TokenDefID != "def_old" {
		t.Fatalf("legacy token refused: ok=%v principal=%+v", ok, p)
	}
	want := hasherFor(t, env).Hash("lct_old")
	if got := storedHash(t, st, "def_old"); got != want {
		t.Fatalf("row not rehashed: stored %q, want the derived-pepper hash %q", got, want)
	}
	// The next login is an ordinary hit on the current hash.
	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_old" {
		t.Errorf("second login refused: ok=%v", ok)
	}
	// And the unpeppered form is no longer a way in for this row: a writer
	// who puts it back is not trusted, because the row left the boot set.
	if _, err := st.OperatorTokenRehash(context.Background(), "def_old", want, auth.HashToken("", "lct_old")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Error("a row rehashed back to the unpeppered form after verification authenticated")
	}
}

// The forging attack the pepper exists to stop: a database writer inserts a
// row whose unpeppered hash they computed for a token they chose. A row that
// was not there at boot never matches a legacy hash.
func TestTokenLookup_RowInsertedAfterBootWithUnpepperedHashIsRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_boot", auth.HashToken("", "lct_boot"))
	snapshotLegacy(t, s)

	seedTokenHash(t, st, "def_forged", auth.HashToken("", "lct_forged"))
	logs, restore := captureLog(t)
	defer restore()
	if p, ok := s.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Fatalf("a row inserted after boot with an unpeppered hash authenticated as %+v", p)
	}
	if !strings.Contains(logs(), `token_def="def_forged" matched a legacy token hash`) {
		t.Errorf("the refused row was not logged: %q", logs())
	}
	if got := storedHash(t, st, "def_forged"); got != auth.HashToken("", "lct_forged") {
		t.Errorf("a refused row was rewritten: %q", got)
	}
}

// A LOOMCYCLE_SECRET_KEY rotation (old key moved to _PREVIOUS) logs nobody
// out: a token hashed under the pepper the old key derived still logs in,
// and is moved onto the new one.
func TestTokenLookup_PreviousSecretKeyRowStillAuthenticates(t *testing.T) {
	oldEnv := config.Env{SecretKey: pepperKey(1)}
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_prev", hasherFor(t, oldEnv).Hash("lct_prev"))
	snapshotLegacy(t, s)

	p, ok := s.resolvePrincipal(context.Background(), "lct_prev")
	if !ok || p.TokenDefID != "def_prev" {
		t.Fatalf("token hashed under the previous key's pepper refused: ok=%v", ok)
	}
	if got, want := storedHash(t, st, "def_prev"), hasherFor(t, env).Hash("lct_prev"); got != want {
		t.Errorf("row not rehashed onto the current key: %q, want %q", got, want)
	}
}

// LOOMCYCLE_TOKEN_LEGACY_HASH=off ends the fallback: a row still on the
// unpeppered hash no longer logs in, even one that existed at boot.
func TestTokenLookup_LegacyFallbackOffRefusesLegacyRow(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), TokenLegacyHashOff: true}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	snapshotLegacy(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Fatalf("legacy row authenticated with the fallback off: %+v", p)
	}
}

// A boot-time row already on the current hash is verified by its first
// login and leaves the legacy set, so an edit that swaps in an unpeppered
// hash afterwards is not honoured.
func TestTokenLookup_CurrentRowLeavesTheLegacySetOnLogin(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := hasherFor(t, env)
	seedTokenHash(t, st, "def_cur", h.Hash("lct_cur"))
	seedTokenHash(t, st, "def_legacy", auth.HashToken("", "lct_legacy"))
	snap, err := s.SnapshotLegacyTokenRows(context.Background())
	if err != nil || len(snap.IDs) != 1 || snap.IDs[0] != "def_legacy" || snap.Untagged != 1 {
		t.Fatalf("snapshot = (%+v, %v), want exactly the one legacy row", snap, err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_cur"); !ok {
		t.Fatal("current token refused")
	}
	if _, err := st.OperatorTokenRehash(context.Background(), "def_cur", h.Hash("lct_cur"), auth.HashToken("", "lct_cur")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_cur"); ok {
		t.Error("a current row edited to an unpeppered hash after boot authenticated")
	}
}

// Every mint uses the effective pepper: a user token minted with no explicit
// pepper is stored under the derived hash and resolves.
func TestTokenMint_UserTokenStoredUnderTheDerivedPepper(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	snapshotLegacy(t, s)
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

// Two first logins of one legacy token at once. The other one rehashes the
// row between this request's miss on the current hash and its legacy
// lookups, so the legacy hash no longer matches. This request must still
// authenticate, and must not leave a refusal in the cache for the next one.
func TestTokenLookup_ConcurrentFirstLoginRehashedMidLookupAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	old := auth.HashToken("", "lct_old")
	seedTokenHash(t, st, "def_old", old)
	snapshotLegacy(t, s)
	cur := hasherFor(t, env).Hash("lct_old")
	s.store = &racingTokenStore{Store: st, at: cur, during: func() {
		if _, err := st.OperatorTokenRehash(context.Background(), "def_old", old, cur); err != nil {
			t.Error(err)
		}
		s.legacyTokens.forget("def_old")
	}}
	s.tokenCache = newTokenCache(30 * time.Second)
	logs, restore := captureLog(t)
	defer restore()

	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_old" {
		t.Fatalf("racing first login refused: ok=%v", ok)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the next login was refused (a negative was cached)")
	}
	if strings.Contains(logs(), "legacy token hash") {
		t.Errorf("the race was logged as a forged row: %s", logs())
	}
}

// The other ordering: this request finds the row under its legacy hash, and
// the other login finishes the rehash (and drops the row from the boot set)
// before this one checks the set.
func TestTokenLookup_ConcurrentFirstLoginLeavingTheSetMidLookupAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	old := auth.HashToken("", "lct_old")
	seedTokenHash(t, st, "def_old", old)
	snapshotLegacy(t, s)
	cur := hasherFor(t, env).Hash("lct_old")
	s.store = &racingTokenStore{Store: st, at: old, during: func() {
		if _, err := st.OperatorTokenRehash(context.Background(), "def_old", old, cur); err != nil {
			t.Error(err)
		}
		s.legacyTokens.forget("def_old")
	}}
	s.tokenCache = newTokenCache(30 * time.Second)
	logs, restore := captureLog(t)
	defer restore()

	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_old" {
		t.Fatalf("racing first login refused: ok=%v", ok)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the next login was refused (a negative was cached)")
	}
	if strings.Contains(logs(), "legacy token hash") {
		t.Errorf("the race was logged as a forged row: %s", logs())
	}
}

// LOOMCYCLE_TOKEN_LEGACY_HASH=off refuses only the unpeppered form. A later
// LOOMCYCLE_SECRET_KEY rotation with _PREVIOUS set must not log every token
// out — admin included.
func TestTokenLookup_LegacyFallbackOffStillAcceptsPreviousSecretKeyRow(t *testing.T) {
	oldEnv := config.Env{SecretKey: pepperKey(1)}
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1), TokenLegacyHashOff: true}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_prev", hasherFor(t, oldEnv).Hash("lct_prev"))
	seedTokenHash(t, st, "def_bare", auth.HashToken("", "lct_bare"))
	snapshotLegacy(t, s)

	if _, ok := s.resolvePrincipal(context.Background(), "lct_prev"); !ok {
		t.Fatal("a token under the previous key's pepper was refused with the fallback off")
	}
	if got, want := storedHash(t, st, "def_prev"), hasherFor(t, env).Hash("lct_prev"); got != want {
		t.Errorf("previous-key row not rehashed: %q, want %q", got, want)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_bare"); ok {
		t.Error("an unpeppered row authenticated with the fallback off")
	}
}

// A refusal reached through the legacy lookups is not cached: it may be the
// losing side of a concurrent first login, and a cached refusal would lock
// a valid token out for the whole TTL.
func TestTokenLookup_NegativeAfterLegacyLookupIsNotCached(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	snapshotLegacy(t, s)
	s.tokenCache = newTokenCache(30 * time.Second)

	if _, ok := s.resolvePrincipal(context.Background(), "lct_unknown"); ok {
		t.Fatal("an unknown bearer authenticated")
	}
	if _, _, cached := s.tokenCache.get(hasherFor(t, env).Hash("lct_unknown")); cached {
		t.Error("a refusal that went through the legacy lookups was cached")
	}
}

// With the unpeppered form off, an unpeppered row is reported at boot but
// kept out of the set: it can never match, and an entry would keep the
// legacy lookups (and uncached refusals) running for every unknown bearer.
func TestSnapshotLegacyTokenRows_UnpepperedOffReportsButDoesNotTrackUntaggedRows(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1), TokenLegacyHashOff: true}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_bare", auth.HashToken("", "lct_bare"))
	snap, err := s.SnapshotLegacyTokenRows(context.Background())
	if err != nil || len(snap.IDs) != 1 || snap.Untagged != 1 {
		t.Fatalf("snapshot = (%+v, %v), want the one unpeppered row reported", snap, err)
	}
	if !s.legacyTokens.empty() {
		t.Error("an unpeppered row was tracked with the unpeppered form off")
	}
}
