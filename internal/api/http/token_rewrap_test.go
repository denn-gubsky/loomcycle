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

// RFC ES F3 — the boot rewrap of unpeppered token hashes.

// bootTokenHashes runs what main.go runs before serving.
func bootTokenHashes(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.RewrapLegacyTokenRows(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
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

// A row minted before any pepper existed is rewrapped under the derived
// pepper before the server serves, still logs in, and that login moves it
// onto the current form.
func TestTokenBoot_UnpepperedRowIsRewrappedAndStillAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	bootTokenHashes(t, s)

	if got, want := storedHash(t, st, "def_old"), wrappedFormFor(t, env.SecretKey, "lct_old"); got != want {
		t.Fatalf("boot left the row on %q, want the wrapped form %q", got, want)
	}
	if p, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok || p.TokenDefID != "def_old" {
		t.Fatalf("rewrapped token refused: ok=%v principal=%+v", ok, p)
	}
	if got, want := storedHash(t, st, "def_old"), hasherFor(t, env).Hash("lct_old"); got != want {
		t.Errorf("login left the row on %q, want the current form %q", got, want)
	}
}

// The forging attack: a database writer adds a row with an unpeppered hash
// of a token they chose. Once the table is on the derived pepper, such a row
// is never rewrapped and never authenticates — not after the next boot
// either, which is what a boot-time allow-list of rows could not promise.
func TestTokenLookup_UnpepperedRowAddedAfterTheRewrapNeverAuthenticates(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_boot", auth.HashToken("", "lct_boot"))
	bootTokenHashes(t, s)

	forged := auth.HashToken("", "lct_forged")
	seedTokenHash(t, st, "def_forged", forged)
	if p, ok := s.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Fatalf("a row added after the rewrap authenticated as %+v", p)
	}

	// The next boot (a restart, or a new replica).
	s2 := &Server{cfgHolder: s.cfgHolder, store: st}
	bootTokenHashes(t, s2)
	if got := storedHash(t, st, "def_forged"); got != forged {
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

// An unpeppered row beside a row already on a derived pepper was not written
// by a server on that pepper (every mint there is tagged), so it is refused
// rather than rewrapped.
func TestTokenLookup_UnpepperedRowBesideATaggedRowIsRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_cur", hasherFor(t, env).Hash("lct_cur"))
	bare := auth.HashToken("", "lct_bare")
	seedTokenHash(t, st, "def_bare", bare)
	bootTokenHashes(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "lct_bare"); ok {
		t.Fatalf("an unpeppered row beside a tagged one authenticated as %+v", p)
	}
	if got := storedHash(t, st, "def_bare"); got != bare {
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
	seedTokenHash(t, st, "def_w", wrappedFormFor(t, pepperKey(1), "lct_w"))
	bootTokenHashes(t, s)

	if p, ok := s.resolvePrincipal(context.Background(), "lct_w"); !ok || p.TokenDefID != "def_w" {
		t.Fatalf("a row wrapped under the previous key refused: ok=%v", ok)
	}
	if got, want := storedHash(t, st, "def_w"), hasherFor(t, env).Hash("lct_w"); got != want {
		t.Errorf("row left on %q, want the current form %q", got, want)
	}
}

// An explicit pepper's rows are untagged and current: the boot leaves them
// alone and they keep logging in.
func TestTokenBoot_ExplicitPepperRowsAreLeftAlone(t *testing.T) {
	env := config.Env{OperatorTokenPepper: "explicit-pepper", SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := auth.HashToken("explicit-pepper", "lct_x")
	seedTokenHash(t, st, "def_x", h)
	bootTokenHashes(t, s)

	if got := storedHash(t, st, "def_x"); got != h {
		t.Fatalf("boot rewrote an explicit-pepper row to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Error("explicit-pepper token refused")
	}
}

// With no pepper at all the boot has nothing to wrap under, and the
// unpeppered rows are the current form.
func TestTokenBoot_NoPepperLeavesRowsAlone(t *testing.T) {
	s, st := derivedPepperServer(t, config.Env{})
	h := auth.HashToken("", "lct_x")
	seedTokenHash(t, st, "def_x", h)
	bootTokenHashes(t, s)

	if got := storedHash(t, st, "def_x"); got != h {
		t.Fatalf("boot rewrote a row with no pepper configured to %q", got)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_x"); !ok {
		t.Error("token refused with no pepper configured")
	}
}

// bootingBesideStore runs `beside` (another replica's boot) once, just before
// this replica's rewrap transaction, so both scanned the same unpeppered rows.
type bootingBesideStore struct {
	store.Store
	beside func()
	fired  bool
}

func (b *bootingBesideStore) OperatorTokenRehashAll(ctx context.Context, ops []store.OperatorTokenRehashOp) (int, error) {
	if !b.fired {
		b.fired = true
		b.beside()
	}
	return b.Store.OperatorTokenRehashAll(ctx, ops)
}

// Two replicas booting at once both find the unpeppered rows and both run
// the rewrap. The row is wrapped once — never a wrap of the wrapped form —
// and the token authenticates on both.
func TestTokenBoot_ConcurrentReplicasWrapEachRowOnce(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	a, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	b := &Server{cfgHolder: a.cfgHolder, store: st}
	a.store = &bootingBesideStore{Store: st, beside: func() { bootTokenHashes(t, b) }}

	rep, err := a.RewrapLegacyTokenRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rewrapped) != 1 || rep.Wrapped != 1 {
		t.Errorf("report = %+v, want the one row rewrapped", rep)
	}
	if got, want := storedHash(t, st, "def_old"), wrappedFormFor(t, env.SecretKey, "lct_old"); got != want {
		t.Fatalf("stored %q, want it wrapped exactly once: %q", got, want)
	}
	a.store = st
	for name, s := range map[string]*Server{"a": a, "b": b} {
		if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
			t.Errorf("replica %s refused the rewrapped token", name)
		}
	}
}

// failingRewrapStore refuses the rewrap transaction.
type failingRewrapStore struct{ store.Store }

func (failingRewrapStore) OperatorTokenRehashAll(context.Context, []store.OperatorTokenRehashOp) (int, error) {
	return 0, errors.New("db unavailable")
}

// A boot whose rewrap fails leaves the rows unpeppered — refused, never
// accepted as they are — and the next boot still rewraps them.
func TestTokenBoot_FailedRewrapIsRetriedAtTheNextBoot(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	seedTokenHash(t, st, "def_old", auth.HashToken("", "lct_old"))
	s.store = failingRewrapStore{st}
	if _, err := s.RewrapLegacyTokenRows(context.Background()); err == nil {
		t.Fatal("a failed rewrap reported success")
	}
	s.store = st
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); ok {
		t.Fatal("an unpeppered row authenticated after a failed rewrap")
	}

	bootTokenHashes(t, s)
	if _, ok := s.resolvePrincipal(context.Background(), "lct_old"); !ok {
		t.Error("the next boot did not rewrap the row")
	}
}

// A row already on the current form, edited by a database writer to an
// unpeppered hash of a token they chose, never authenticates.
func TestTokenLookup_RowEditedToAnUnpepperedHashIsRefused(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	h := hasherFor(t, env)
	seedTokenHash(t, st, "def_cur", h.Hash("lct_cur"))
	bootTokenHashes(t, s)
	if _, err := st.OperatorTokenRehash(context.Background(), "def_cur", h.Hash("lct_cur"), auth.HashToken("", "lct_mine")); err != nil {
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
	seedTokenHash(t, st, "def_cur", h.Hash("lct_cur"))
	seedTokenHash(t, st, "def_w", h.WrappedHash("lct_w"))
	seedTokenHash(t, st, "def_prev", hasherFor(t, config.Env{SecretKey: pepperKey(1)}).Hash("lct_prev"))
	seedTokenHash(t, st, "def_gone", hasherFor(t, config.Env{SecretKey: pepperKey(3)}).Hash("lct_gone"))
	seedTokenHash(t, st, "def_bare", auth.HashToken("", "lct_bare"))

	rep, err := s.RewrapLegacyTokenRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rewrapped) != 0 || rep.Wrapped != 1 || rep.Previous != 1 || rep.OtherKey != 1 ||
		len(rep.Refused) != 1 || rep.Refused[0] != "def_bare" {
		t.Errorf("report = %+v", rep)
	}
	if _, ok := s.resolvePrincipal(context.Background(), "lct_gone"); ok {
		t.Error("a token under a key neither configured key derives authenticated")
	}
}

// A deployment that first boots on the derived pepper with no token rows at
// all (common: only LOOMCYCLE_AUTH_TOKEN or config principals) has no tagged
// row to mark the table as done. An unpeppered row written later must still
// never be rewrapped — a later boot sees the marker the first one left.
func TestTokenBoot_RowWrittenIntoAnEmptyTableAfterTheFirstBootIsNotRewrapped(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	bootTokenHashes(t, s)

	forged := auth.HashToken("", "lct_forged")
	seedTokenHash(t, st, "def_forged", forged)
	s2 := &Server{cfgHolder: s.cfgHolder, store: st}
	rep, err := s2.RewrapLegacyTokenRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := storedHash(t, st, "def_forged"); got != forged {
		t.Fatalf("a row written after the first boot was rewrapped to %q", got)
	}
	if len(rep.Refused) != 1 || rep.Refused[0] != "def_forged" {
		t.Errorf("report = %+v, want the row refused", rep)
	}
	if p, ok := s2.resolvePrincipal(context.Background(), "lct_forged"); ok {
		t.Errorf("a row written after the first boot authenticated as %+v", p)
	}
}
