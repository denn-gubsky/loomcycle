package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC ES F3/C2 — unpeppered token hashes: reported at boot, rewrapped only by
// the operator's `loomcycle tokens rewrap`.

// bootTokenHashes runs what main.go runs before serving.
func bootTokenHashes(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.ReportTokenHashes(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
}

// rewrapTokenHashes runs what the operator's two steps of `loomcycle tokens
// rewrap` run against s's store and pepper: list the unpeppered rows, then
// confirm every listed row by the set's digest. Returns the rows listed and
// how many were rewrapped.
func rewrapTokenHashes(t *testing.T, s *Server) ([]store.OperatorTokenDefRow, int) {
	t.Helper()
	h, err := s.hasher()
	if err != nil {
		t.Fatal(err)
	}
	res, err := ListUntaggedTokenRows(context.Background(), s.store, h)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	picked, err := SelectUntaggedTokenRows(res.Live, nil, UntaggedTokenSetDigest(res.Live))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	moved, _, err := RewrapTokenRows(context.Background(), s.store, h, picked)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	return res.Live, len(moved)
}

// wrappedFormFor spells out the stored wrapped form independently of the
// code under test, so the format is pinned: "w1:<first 8 bytes of
// SHA-256(pepper), hex>:<hex HMAC-SHA256(pepper, "w1" 0x00 + the stored
// unpeppered hash as its 64-char hex text)>".
func wrappedFormFor(t *testing.T, secretKey, token string) string {
	t.Helper()
	pepper, err := credential.DeriveTokenPepper(secretKey)
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256([]byte(pepper))
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte("w1\x00" + auth.HashToken("", token)))
	return "w1:" + hex.EncodeToString(fp[:8]) + ":" + hex.EncodeToString(mac.Sum(nil))
}

// Boot no longer rewraps (C2): an unpeppered row found at boot is reported by
// def_id, left as it is, and refused at login.
func TestTokenBoot_UnpepperedRowIsLeftAloneAndRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	bare := auth.HashToken("", "lct_old")
	seedTokenHash(t, st, "def_00000000000000a1", bare)
	rep, err := s.ReportTokenHashes(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Untagged) != 1 || rep.Untagged[0] != "def_00000000000000a1" {
		t.Errorf("report = %+v, want def_00000000000000a1 listed as untagged", rep)
	}
	if got := storedHash(t, st, "def_00000000000000a1"); got != bare {
		t.Fatalf("boot rewrote the unpeppered row to %q", got)
	}
	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Fatalf("an unpeppered row authenticated before the rewrap command ran: %+v", p)
	}
}

// The residual C2 closes: whatever a database writer removes — here the
// marker a v1.119.0 boot wrote, on a table with no tagged rows — a restart
// does not rewrap a row they inserted.
func TestTokenBoot_RestartNeverRewrapsAnInsertedRow(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	bootTokenHashes(t, s)
	forged := auth.HashToken("", "lct_forged")
	seedTokenHash(t, st, "def_00000000000000f0", forged)
	if _, err := st.CredentialDefDelete(context.Background(), "", "system", "", "operator-token-rewrap"); err != nil {
		t.Fatal(err)
	}

	s2 := &Server{cfgHolder: s.cfgHolder, store: st}
	bootTokenHashes(t, s2)
	if got := storedHash(t, st, "def_00000000000000f0"); got != forged {
		t.Fatalf("a restart rewrapped an inserted row to %q", got)
	}
	if p, ok := s2.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Fatalf("an inserted unpeppered row authenticated after a restart: %+v", p)
	}
}

// The command wraps a row minted before any pepper existed while a server is
// running; that server then logs the token in without a restart, and the
// login moves the row onto the current form.
func TestTokenRewrap_RowAuthenticatesOnARunningServerAfterTheCommand(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	bootTokenHashes(t, s)
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Fatal("the unpeppered row authenticated before the rewrap")
	}

	cmd := &Server{cfgHolder: s.cfgHolder, store: st}
	rows, n := rewrapTokenHashes(t, cmd)
	if n != 1 || len(rows) != 1 || rows[0].DefID != "def_00000000000000a1" {
		t.Errorf("listed %+v, rewrapped %d; want def_00000000000000a1 rewrapped", rows, n)
	}
	if got, want := storedHash(t, st, "def_00000000000000a1"), wrappedFormFor(t, env.SecretKey, "lct_old"); got != want {
		t.Fatalf("the command left the row on %q, want the wrapped form %q", got, want)
	}
	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_00000000000000a1" {
		t.Fatalf("the running server refused the rewrapped token: ok=%v principal=%+v", ok, p)
	}
	if got, want := storedHash(t, st, "def_00000000000000a1"), hasherFor(t, env).Hash("lct_old"); got != want {
		t.Errorf("login left the row on %q, want the current form %q", got, want)
	}
}

// The review step lists the unpeppered rows and writes nothing.
func TestTokenRewrap_ListingChangesNothing(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	bare := auth.HashToken("", "lct_old")
	seedTokenHash(t, st, "def_00000000000000a1", bare)
	bootTokenHashes(t, s)

	res, err := ListUntaggedTokenRows(context.Background(), st, hasherFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Live) != 1 || res.Live[0].DefID != "def_00000000000000a1" || len(res.Untagged) != 1 {
		t.Errorf("listing = %+v, want def_00000000000000a1", res)
	}
	if got := storedHash(t, st, "def_00000000000000a1"); got != bare {
		t.Fatalf("listing rewrote the row to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Error("the row authenticated after a listing")
	}
}

// Only the rows the operator named are rewrapped; an unpeppered row nobody
// named — a database writer's insert — stays refused.
func TestTokenRewrap_OnlyNamedRowsAreRewrapped(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	forged := auth.HashToken("", "lct_forged")
	seedTokenHash(t, st, "def_00000000000000f0", forged)
	bootTokenHashes(t, s)
	h := hasherFor(t, env)

	res, err := ListUntaggedTokenRows(context.Background(), st, h)
	if err != nil {
		t.Fatal(err)
	}
	var old store.OperatorTokenDefRow
	for _, r := range res.Live {
		if r.DefID == "def_00000000000000a1" {
			old = r
		}
	}
	picked, err := SelectUntaggedTokenRows(res.Live, []string{old.DefID + "@" + TokenRowFingerprint(old)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if moved, _, err := RewrapTokenRows(context.Background(), st, h, picked); err != nil || len(moved) != 1 {
		t.Fatalf("rewrap = (%d, %v), want 1", len(moved), err)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the named row was refused")
	}
	if got := storedHash(t, st, "def_00000000000000f0"); got != forged {
		t.Errorf("an unnamed row was rewritten to %q", got)
	}
	if p, ok := s.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Errorf("an unnamed row authenticated as %+v", p)
	}
}

// A selection that does not match the current rows is refused outright: a
// named def_id that is not unpeppered now, or a digest of a set that has
// since gained a row or had a hash changed. Names and a digest together are
// refused too.
func TestSelectUntaggedTokenRows_RefusesWhatWasNotReviewed(t *testing.T) {
	row := func(id, token string) store.OperatorTokenDefRow {
		return store.OperatorTokenDefRow{DefID: id, TokenHash: auth.HashToken("", token)}
	}
	reviewed := []store.OperatorTokenDefRow{row("def_000000000000000a", "a"), row("def_000000000000000b", "b")}
	digest := UntaggedTokenSetDigest(reviewed)

	if got, err := SelectUntaggedTokenRows([]store.OperatorTokenDefRow{reviewed[1], reviewed[0]}, nil, digest); err != nil || len(got) != 2 {
		t.Errorf("the reviewed set in another order = (%d rows, %v), want both", len(got), err)
	}
	for name, now := range map[string][]store.OperatorTokenDefRow{
		"row added":    append(append([]store.OperatorTokenDefRow(nil), reviewed...), row("def_00000000000000cc", "x")),
		"hash changed": {reviewed[0], row("def_000000000000000b", "mine")},
		"row gone":     {reviewed[0]},
	} {
		if _, err := SelectUntaggedTokenRows(now, nil, digest); err == nil {
			t.Errorf("%s: the stale digest was accepted", name)
		}
	}
	named := func(r store.OperatorTokenDefRow) string { return r.DefID + "@" + TokenRowFingerprint(r) }
	if got, err := SelectUntaggedTokenRows(reviewed, []string{named(reviewed[0])}, ""); err != nil || len(got) != 1 {
		t.Errorf("a reviewed row named with its fingerprint = (%d, %v)", len(got), err)
	}
	if _, err := SelectUntaggedTokenRows(reviewed, []string{named(reviewed[0]), "def_00000000000000dd@0000000000000000"}, ""); err == nil {
		t.Error("a def_id that is not an unpeppered row was accepted")
	}
	if _, err := SelectUntaggedTokenRows(reviewed, []string{"def_000000000000000a"}, ""); err == nil {
		t.Error("a def_id without its fingerprint was accepted")
	}
	swapped := []store.OperatorTokenDefRow{row("def_000000000000000a", "mine"), reviewed[1]}
	if _, err := SelectUntaggedTokenRows(swapped, []string{named(reviewed[0])}, ""); err == nil {
		t.Error("a row whose hash changed since the review was accepted under its old fingerprint")
	}
	if _, err := SelectUntaggedTokenRows(reviewed, []string{named(reviewed[0])}, digest); err == nil {
		t.Error("names and a digest together were accepted")
	}
	if _, err := SelectUntaggedTokenRows(reviewed, nil, ""); err == nil {
		t.Error("an empty selection was accepted")
	}
}

// Under an explicit pepper the untagged rows are current, and with no pepper
// there is nothing to wrap under: the command refuses and writes nothing.
func TestTokenRewrap_RefusedWithoutADerivedPepper(t *testing.T) {
	for name, env := range map[string]config.Env{
		"explicit": {OperatorTokenPepper: "explicit-pepper", SecretKey: pepperKey(1)},
		"none":     {},
	} {
		t.Run(name, func(t *testing.T) {
			_, st := derivedPepperServer(t, env)
			h := auth.HashToken(env.OperatorTokenPepper, "lct_x")
			seedTokenHash(t, st, "def_00000000000000cc", h)
			if _, err := ListUntaggedTokenRows(context.Background(), st, hasherFor(t, env)); err == nil {
				t.Error("the listing ran without a derived pepper")
			}
			rows := []store.OperatorTokenDefRow{{DefID: "def_00000000000000cc", TokenHash: h}}
			if _, _, err := RewrapTokenRows(context.Background(), st, hasherFor(t, env), rows); err == nil {
				t.Fatal("the rewrap ran without a derived pepper")
			}
			if got := storedHash(t, st, "def_00000000000000cc"); got != h {
				t.Errorf("the refused rewrap rewrote the row to %q", got)
			}
		})
	}
}

// The forging attack: a database writer adds a row with an unpeppered hash
// of a token they chose after the operator's rewrap. It never authenticates,
// and no restart rewraps it.
func TestTokenLookup_UnpepperedRowAddedAfterTheRewrapNeverAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000b0", auth.HashToken("", "lct_boot"))
	rewrapTokenHashes(t, s)
	bootTokenHashes(t, s)

	forged := auth.HashToken("", "lct_forged")
	seedTokenHash(t, st, "def_00000000000000f0", forged)
	if p, ok := s.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Fatalf("a row added after the rewrap authenticated as %+v", p)
	}

	// The next boot (a restart, or a new replica).
	s2 := &Server{cfgHolder: s.cfgHolder, store: st}
	bootTokenHashes(t, s2)
	if got := storedHash(t, st, "def_00000000000000f0"); got != forged {
		t.Errorf("the next boot rewrote the added row to %q", got)
	}
	if p, ok := s2.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Errorf("a row added after the rewrap authenticated after the next boot as %+v", p)
	}
	// The genuine row is unaffected.
	if _, ok := s2.resolvePrincipal(context.Background(), "lct_boot"); !ok {
		t.Error("the rewrapped row was refused after the next boot")
	}
}

// An unpeppered row beside a row already on a derived pepper is refused at
// login, and boot leaves it as it is.
func TestTokenLookup_UnpepperedRowBesideATaggedRowIsRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000c0", hasherFor(t, env).Hash("lct_cur"))
	bare := auth.HashToken("", "lct_bare")
	seedTokenHash(t, st, "def_00000000000000ba", bare)
	bootTokenHashes(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "lct_bare"); ok {
		t.Fatalf("an unpeppered row beside a tagged one authenticated as %+v", p)
	}
	if got := storedHash(t, st, "def_00000000000000ba"); got != bare {
		t.Errorf("the refused row was rewritten to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_cur"); !ok {
		t.Error("the current token was refused")
	}
}

// A row rewrapped under a key that has since been rotated (old key moved to
// LOOMCYCLE_SECRET_KEY_PREVIOUS) still logs in, and moves onto the current
// key's form.
func TestTokenLookup_WrappedRowUnderPreviousSecretKeyAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000e0", wrappedFormFor(t, pepperKey(1), "lct_w"))
	bootTokenHashes(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "lct_w"); !ok || p.TokenDefID != "def_00000000000000e0" {
		t.Fatalf("a row wrapped under the previous key refused: ok=%v", ok)
	}
	if got, want := storedHash(t, st, "def_00000000000000e0"), hasherFor(t, env).Hash("lct_w"); got != want {
		t.Errorf("row left on %q, want the current form %q", got, want)
	}
}

// An explicit pepper's rows are untagged and current: the boot leaves them
// alone and they keep logging in.
func TestTokenBoot_ExplicitPepperRowsAreLeftAlone(t *testing.T) {
	env := config.Env{OperatorTokenPepper: "explicit-pepper", SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := auth.HashToken("explicit-pepper", "lct_x")
	seedTokenHash(t, st, "def_00000000000000cc", h)
	bootTokenHashes(t, s)

	if got := storedHash(t, st, "def_00000000000000cc"); got != h {
		t.Fatalf("boot rewrote an explicit-pepper row to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Error("explicit-pepper token refused")
	}
}

// With no pepper at all the unpeppered rows are the current form.
func TestTokenBoot_NoPepperLeavesRowsAlone(t *testing.T) {
	s, st := derivedPepperServer(t, config.Env{})
	h := auth.HashToken("", "lct_x")
	seedTokenHash(t, st, "def_00000000000000cc", h)
	bootTokenHashes(t, s)

	if got := storedHash(t, st, "def_00000000000000cc"); got != h {
		t.Fatalf("boot rewrote a row with no pepper configured to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Error("token refused with no pepper configured")
	}
}

// runningBesideStore runs `beside` (a second rewrap, or anything else
// writing the table) once, just before this rewrap's transaction, so both
// scanned the same unpeppered rows.
type runningBesideStore struct {
	store.Store
	beside func()
	fired  bool
}

func (b *runningBesideStore) OperatorTokenRehashAll(ctx context.Context, ops []store.OperatorTokenRehashOp) ([]bool, error) {
	if !b.fired {
		b.fired = true
		b.beside()
	}
	return b.Store.OperatorTokenRehashAll(ctx, ops)
}

// Two rewraps at once (run from two shells, or beside a running server) both
// confirmed the same unpeppered rows. The row is wrapped once — never a wrap of the
// wrapped form — and the token authenticates.
func TestTokenRewrap_ConcurrentRunsWrapEachRowOnce(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	srv, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	bootTokenHashes(t, srv)
	h := hasherFor(t, env)
	res, err := ListUntaggedTokenRows(context.Background(), st, h)
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Live
	var other []store.OperatorTokenDefRow
	racing := &runningBesideStore{Store: st, beside: func() {
		var err error
		if other, _, err = RewrapTokenRows(context.Background(), st, h, rows); err != nil {
			t.Error(err)
		}
	}}

	moved, notMoved, err := RewrapTokenRows(context.Background(), racing, h, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved)+len(other) != 1 {
		t.Errorf("rewrapped %d and %d, want the one row rewrapped once between them", len(moved), len(other))
	}
	// The losing run reports the row as not moved by it.
	if len(notMoved) != 1 || notMoved[0].DefID != "def_00000000000000a1" {
		t.Errorf("not moved = %+v, want the row the other run took", notMoved)
	}
	if got, want := storedHash(t, st, "def_00000000000000a1"), wrappedFormFor(t, env.SecretKey, "lct_old"); got != want {
		t.Fatalf("stored %q, want it wrapped exactly once: %q", got, want)
	}
	if _, ok := srv.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the running server refused the rewrapped token")
	}
}

// failingRewrapStore refuses the rewrap transaction.
type failingRewrapStore struct{ store.Store }

func (failingRewrapStore) OperatorTokenRehashAll(context.Context, []store.OperatorTokenRehashOp) ([]bool, error) {
	return nil, errors.New("db unavailable")
}

// A failed rewrap reports the error and leaves the rows unpeppered — refused,
// never accepted as they are — and running it again rewraps them.
func TestTokenRewrap_FailedRunCanBeRetried(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000a1", auth.HashToken("", "lct_old"))
	bootTokenHashes(t, s)
	res, err := ListUntaggedTokenRows(context.Background(), st, hasherFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RewrapTokenRows(context.Background(), failingRewrapStore{st}, hasherFor(t, env), res.Live); err == nil {
		t.Fatal("a failed rewrap reported success")
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Fatal("an unpeppered row authenticated after a failed rewrap")
	}

	rewrapTokenHashes(t, s)
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("running the rewrap again did not rewrap the row")
	}
}

// A row already on the current form, edited by a database writer to an
// unpeppered hash of a token they chose, never authenticates.
func TestTokenLookup_RowEditedToAnUnpepperedHashIsRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := hasherFor(t, env)
	seedTokenHash(t, st, "def_00000000000000c0", h.Hash("lct_cur"))
	bootTokenHashes(t, s)
	if _, err := st.OperatorTokenRehash(context.Background(), "def_00000000000000c0", h.Hash("lct_cur"), auth.HashToken("", "lct_mine")); err != nil {
		t.Fatal(err)
	}
	bootTokenHashes(t, s)
	if p, ok := s.resolvePrincipal(context.Background(), "lct_mine"); ok {
		t.Errorf("a row edited to an unpeppered hash authenticated as %+v", p)
	}
}

// The boot report names what an operator needs to act on.
func TestTokenBoot_ReportCountsEachForm(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := hasherFor(t, env)
	seedTokenHash(t, st, "def_00000000000000c0", h.Hash("lct_cur"))
	seedTokenHash(t, st, "def_00000000000000e0", h.WrappedHash("lct_w"))
	seedTokenHash(t, st, "def_00000000000000d0", hasherFor(t, config.Env{SecretKey: pepperKey(1)}).Hash("lct_prev"))
	seedTokenHash(t, st, "def_00000000000000dd", hasherFor(t, config.Env{SecretKey: pepperKey(3)}).Hash("lct_gone"))
	seedTokenHash(t, st, "def_00000000000000ba", auth.HashToken("", "lct_bare"))

	rep, err := s.ReportTokenHashes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Wrapped != 1 || rep.Previous != 1 || rep.OtherKey != 1 ||
		len(rep.Untagged) != 1 || rep.Untagged[0] != "def_00000000000000ba" {
		t.Errorf("report = %+v", rep)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_gone"); ok {
		t.Error("a token under a key neither configured key derives authenticated")
	}
}

func seedAdminTokenHash(t *testing.T, st store.Store, defID, hash string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: defID, Name: defID, TenantID: "default", Subject: defID,
		TokenHash: hash, AllowedScopes: []string{auth.ScopeAdmin},
	}); err != nil {
		t.Fatalf("seed %s: %v", defID, err)
	}
}

// An admin-scoped row that cannot authenticate — unpeppered and not yet
// rewrapped, or under a key neither configured key derives — must not switch
// the shared secret off: that would lock a deployment whose only admin token
// it was out of admin altogether, and let a database writer do the same on
// purpose.
func TestLegacyFallback_UnusableAdminRowsLeaveTheSharedSecretWorking(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1), AuthToken: "legacy-shared-secret"}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_00000000000000c0", hasherFor(t, env).Hash("lct_cur"))
	seedAdminTokenHash(t, st, "def_00000000000000ab", auth.HashToken("", "lct_bare"))
	seedAdminTokenHash(t, st, "def_00000000000000ad", hasherFor(t, config.Env{SecretKey: pepperKey(3)}).Hash("lct_gone"))
	bootTokenHashes(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "legacy-shared-secret"); !ok || !p.Legacy {
		t.Fatalf("an admin row that cannot authenticate disabled LOOMCYCLE_AUTH_TOKEN: ok=%v", ok)
	}
}

// The other side: an admin row that CAN authenticate — wrapped, or under the
// previous key — still switches the shared secret off.
func TestLegacyFallback_UsableAdminRowsDisableTheSharedSecret(t *testing.T) {
	for name, seed := range map[string]func(*testing.T, store.Store){
		"wrapped": func(t *testing.T, st store.Store) {
			seedAdminTokenHash(t, st, "def_00000000000000aa", hasherFor(t, config.Env{SecretKey: pepperKey(2)}).WrappedHash("lct_admin"))
		},
		"previous key": func(t *testing.T, st store.Store) {
			seedAdminTokenHash(t, st, "def_00000000000000aa", hasherFor(t, config.Env{SecretKey: pepperKey(1)}).Hash("lct_admin"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := config.Env{SecretKey: pepperKey(2), SecretKeyPrevious: pepperKey(1), AuthToken: "legacy-shared-secret"}
			s, st := derivedPepperServer(t, env)
			seed(t, st)
			bootTokenHashes(t, s)
			if _, ok := s.resolvePrincipal(context.Background(), "legacy-shared-secret"); ok {
				t.Error("a usable admin token left LOOMCYCLE_AUTH_TOKEN enabled")
			}
		})
	}
}

// A row whose fields hold control or non-printable characters (or invalid
// UTF-8) is suspicious and refused by either way of naming it; an ordinary
// row, non-ASCII letters included, is not.
func TestSelectUntaggedTokenRows_RefusesSuspiciousRows(t *testing.T) {
	plain := store.OperatorTokenDefRow{DefID: "def_0000000000000001", Name: "café", TenantID: "acme", Subject: "Zoë", AllowedScopes: []string{"runs:create"}, TokenHash: auth.HashToken("", "ok")}
	if SuspiciousTokenRow(plain) {
		t.Fatal("an ordinary row was flagged")
	}
	for name, mutate := range map[string]func(*store.OperatorTokenDefRow){
		"escape in name":    func(r *store.OperatorTokenDefRow) { r.Name = "x\x1b[2K" },
		"newline in def_id": func(r *store.OperatorTokenDefRow) { r.DefID = "def_00000000000000cc\n" },
		"bidi in tenant":    func(r *store.OperatorTokenDefRow) { r.TenantID = "acme‮" },
		"bell in subject":   func(r *store.OperatorTokenDefRow) { r.Subject = "s\x07" },
		"tab in a scope":    func(r *store.OperatorTokenDefRow) { r.AllowedScopes = []string{"runs:create\t"} },
		"invalid utf-8":     func(r *store.OperatorTokenDefRow) { r.Name = "\xff" },
	} {
		r := plain
		r.DefID = "def_0000000000000002"
		r.TokenHash = auth.HashToken("", "bad")
		mutate(&r)
		if !SuspiciousTokenRow(r) {
			t.Errorf("%s: not flagged", name)
		}
		rows := []store.OperatorTokenDefRow{plain, r}
		if _, err := SelectUntaggedTokenRows(rows, nil, UntaggedTokenSetDigest(rows)); err == nil {
			t.Errorf("%s: --all-listed accepted a set holding it", name)
		}
		if _, err := SelectUntaggedTokenRows(rows, []string{r.DefID + "@" + TokenRowFingerprint(r)}, ""); err == nil {
			t.Errorf("%s: naming it by def_id was accepted", name)
		}
		if got, err := SelectUntaggedTokenRows(rows, []string{plain.DefID + "@" + TokenRowFingerprint(plain)}, ""); err != nil || len(got) != 1 {
			t.Errorf("%s: naming only the ordinary row = (%d, %v)", name, len(got), err)
		}
	}
}

// The names listing has one row per (tenant, name), so a per-user name minted
// in two tenants appears twice. The review lists rows by name, so it must
// still list each row once: a row listed twice would be counted twice, and
// the digest of the set would cover it twice.
func TestTokenRewrap_NameInTwoTenantsListsEachRowOnce(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	_, st := derivedPepperServer(t, env)
	for _, r := range []struct{ defID, tenant string }{
		{"def_00000000000000f1", "t1"},
		{"def_00000000000000f2", "t2"},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: r.defID, Name: "u-alice", TenantID: r.tenant, Subject: "alice",
			TokenHash: auth.HashToken("", "lct_"+r.defID), AllowedScopes: []string{auth.ScopeRunsRead},
		}); err != nil {
			t.Fatalf("seed %s: %v", r.defID, err)
		}
	}
	res, err := ListUntaggedTokenRows(context.Background(), st, hasherFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Live) != 2 || len(res.Untagged) != 2 ||
		res.Untagged[0] != "def_00000000000000f1" || res.Untagged[1] != "def_00000000000000f2" {
		t.Errorf("listing = live %d, untagged %v; want each of the two rows once", len(res.Live), res.Untagged)
	}
}
