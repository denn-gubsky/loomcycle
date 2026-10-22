package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Item 5: a def_id that is not the minted shape ("def_" + 16 lowercase hex)
// is suspicious — this catches the blank-rendering and lookalike characters
// unicode.IsPrint lets through.
func TestSuspiciousTokenRow_DefIDOutsideTheMintedShape(t *testing.T) {
	if SuspiciousTokenRow(store.OperatorTokenDefRow{DefID: "def_0123456789abcdef"}) {
		t.Fatal("a minted def_id was flagged")
	}
	for _, r := range []rune{0x3164, 0x115F, 0xFFA0, 0x2800, 0x0336, 0xFE0F, 0x0430, 0x00A0} {
		id := "def_0123456789abcd" + string(r) + "f"
		if !SuspiciousTokenRow(store.OperatorTokenDefRow{DefID: id}) {
			t.Errorf("def_id with U+%04X not flagged", r)
		}
	}
	for _, id := range []string{"def_0123456789ABCDEF", "def_0123", "tok_0123456789abcdef", "def_0123456789abcdef0"} {
		if !SuspiciousTokenRow(store.OperatorTokenDefRow{DefID: id}) {
			t.Errorf("def_id %q not flagged", id)
		}
	}
}

// Item 1: a retired or expired unpeppered row is never a candidate. A writer
// inserts one already retired, the operator confirms the listed set, the
// writer clears retired_at: the row must still be unpeppered, so the
// attacker's token does not log in. A row inside a rotation grace window
// (retired_at in the future) is live and is a candidate.
func TestTokenRewrap_DeadRowsAreNotCandidatesAndStayDead(t *testing.T) {
	env := config.Env{SecretKey: pepperKey(1)}
	s, st := derivedPepperServer(t, env)
	ctx := context.Background()
	seed := func(id, token string, retired, expires time.Time) {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: id, Name: id, TenantID: "default", Subject: id, TokenHash: auth.HashToken("", token),
			AllowedScopes: []string{auth.ScopeAdmin}, RetiredAt: retired, ExpiresAt: expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	seed("def_00000000000000d1", "lct_retired", past, time.Time{})
	seed("def_00000000000000d2", "lct_expired", time.Time{}, past)
	seed("def_00000000000000d3", "lct_grace", future, time.Time{})

	rep, err := s.ReportTokenHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Untagged) != 1 || rep.Untagged[0] != "def_00000000000000d3" || rep.UntaggedDead != 2 {
		t.Errorf("boot report = %+v, want only the grace row listed and 2 dead counted", rep)
	}
	res := func() UntaggedTokenRows {
		r, err := ListUntaggedTokenRows(ctx, st, hasherFor(t, env))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()
	if len(res.Live) != 1 || len(res.Dead) != 2 {
		t.Fatalf("live %d, dead %d; want 1 and 2", len(res.Live), len(res.Dead))
	}
	rewrapTokenHashes(t, s)
	if err := st.OperatorTokenDefSetRetiredAt(ctx, "def_00000000000000d1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if p, ok := s.resolvePrincipal(ctx, "lct_retired"); ok {
		t.Errorf("a row retired at review time, un-retired after the rewrap, authenticated as %+v", p)
	}
	if _, ok := s.resolvePrincipal(ctx, "lct_grace"); !ok {
		t.Error("the row inside its grace window was not rewrapped")
	}
}

// Item 3: rows under another key and none under this one look like the
// wrong LOOMCYCLE_SECRET_KEY.
func TestTokenHashReport_LikelyWrongKey(t *testing.T) {
	for _, tc := range []struct {
		rep  TokenHashReport
		want bool
	}{
		{TokenHashReport{OtherKey: 2}, true},
		{TokenHashReport{OtherKey: 2, Untagged: []string{"def_0000000000000001"}}, true},
		{TokenHashReport{OtherKey: 2, Current: 1}, false},
		{TokenHashReport{OtherKey: 2, Wrapped: 1}, false},
		{TokenHashReport{Untagged: []string{"def_0000000000000001"}}, false},
	} {
		if got := tc.rep.LikelyWrongKey(); got != tc.want {
			t.Errorf("%+v: LikelyWrongKey = %v, want %v", tc.rep, got, tc.want)
		}
	}
}

// Item 2: the fingerprint pins every field the review shows that matters,
// the hash included.
func TestTokenRowFingerprint_ChangesWithEveryPinnedField(t *testing.T) {
	base := store.OperatorTokenDefRow{DefID: "def_0000000000000001", TokenHash: "h", TenantID: "t", Subject: "s", AllowedScopes: []string{"runs:create"}}
	fp := TokenRowFingerprint(base)
	if len(fp) != 16 {
		t.Fatalf("fingerprint %q, want 16 hex characters", fp)
	}
	for name, mutate := range map[string]func(*store.OperatorTokenDefRow){
		"def_id":     func(r *store.OperatorTokenDefRow) { r.DefID = "def_0000000000000002" },
		"token_hash": func(r *store.OperatorTokenDefRow) { r.TokenHash = "h2" },
		"tenant":     func(r *store.OperatorTokenDefRow) { r.TenantID = "t2" },
		"subject":    func(r *store.OperatorTokenDefRow) { r.Subject = "s2" },
		"scopes":     func(r *store.OperatorTokenDefRow) { r.AllowedScopes = []string{auth.ScopeAdmin} },
		"retired_at": func(r *store.OperatorTokenDefRow) { r.RetiredAt = time.Unix(1, 0) },
		"expires_at": func(r *store.OperatorTokenDefRow) { r.ExpiresAt = time.Unix(1, 0) },
	} {
		r := base
		mutate(&r)
		if TokenRowFingerprint(r) == fp {
			t.Errorf("changing %s kept the fingerprint", name)
		}
	}
}
