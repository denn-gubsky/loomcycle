package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const defOld = "def_00000000000000a1"

var fingerprintRE = regexp.MustCompile(`--def-id (def_[0-9a-f]{16})@([0-9a-f]+)`)

// reviewFingerprint is the `<def_id>@<fingerprint>` the review prints for
// defID, the form --def-id takes.
func reviewFingerprint(t *testing.T, out, defID string) string {
	t.Helper()
	for _, m := range fingerprintRE.FindAllStringSubmatch(out, -1) {
		if m[1] == defID {
			return m[1] + "@" + m[2]
		}
	}
	t.Fatalf("the review prints no --def-id %s@<fingerprint>:\n%s", defID, out)
	return ""
}

func review(t *testing.T, cfgPath string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("review rc = %d, stderr: %s", rc, errb.String())
	}
	return out.String()
}

// Item 1: a retired or expired unpeppered row is not a rewrap candidate. A
// writer who inserts one already retired (as the review advises for a row you
// never minted), has it wrapped, then clears retired_at would otherwise hold
// a working admin token. A row still inside a rotation grace window is live.
func TestTokensRewrap_DeadRowsAreListedApartAndNeverRewrapped(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	ctx := context.Background()
	for id, mutate := range map[string]func(*store.OperatorTokenDefRow){
		"def_00000000000000d1": func(r *store.OperatorTokenDefRow) { r.RetiredAt = time.Now().Add(-time.Hour) },
		"def_00000000000000d2": func(r *store.OperatorTokenDefRow) { r.ExpiresAt = time.Now().Add(-time.Hour) },
		"def_00000000000000d3": func(r *store.OperatorTokenDefRow) { r.RetiredAt = time.Now().Add(time.Hour) },
	} {
		r := store.OperatorTokenDefRow{DefID: id, Name: id, TenantID: "default", Subject: id,
			TokenHash: auth.HashToken("", "lct_"+id), AllowedScopes: []string{auth.ScopeAdmin}}
		mutate(&r)
		if _, err := st.OperatorTokenDefCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	out := review(t, cfgPath)
	if !strings.Contains(out, "not rewrappable") {
		t.Errorf("the review does not list the dead rows apart:\n%s", out)
	}
	var o, e bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--all-listed", reviewDigest(t, out)}, &o, &e); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, e.String())
	}
	for _, id := range []string{"def_00000000000000d1", "def_00000000000000d2"} {
		if got := rowHash(t, st, id); got != auth.HashToken("", "lct_"+id) {
			t.Errorf("dead row %s was rewrapped to %q", id, got)
		}
	}
	if got, want := rowHash(t, st, "def_00000000000000d3"), wrapped(t, "lct_def_00000000000000d3"); got != want {
		t.Errorf("a row inside its rotation grace window was not rewrapped: %q", got)
	}
	o.Reset()
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", "def_00000000000000d1"}, &o, &e); rc != 2 {
		t.Errorf("naming a dead row: rc = %d, want 2", rc)
	}
}

// Item 2: --def-id pins the reviewed row. A bare def_id is refused, and a
// row whose hash a writer swapped after the review no longer matches its
// fingerprint.
func TestTokensRewrap_DefIDMustCarryTheReviewedFingerprint(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", defOld}, &out, &errb); rc != 2 {
		t.Fatalf("a bare --def-id: rc = %d, want 2", rc)
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Fatalf("a bare --def-id rewrote the row to %q", got)
	}

	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	if _, err := st.OperatorTokenRehash(context.Background(), defOld, before, auth.HashToken("", "lct_mine")); err != nil {
		t.Fatal(err)
	}
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named}, &out, &errb); rc != 2 {
		t.Fatalf("a swapped hash: rc = %d, want 2", rc)
	}
	if got := tokensStoredHash(t, st); got != auth.HashToken("", "lct_mine") {
		t.Errorf("the swapped row was rewritten to %q", got)
	}
}

// Item 2: the result re-prints each wrapped row's review block.
func TestTokensRewrap_ResultReprintsTheWrappedRows(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if got, want := tokensStoredHash(t, st), wrapped(t, "lct_old"); got != want {
		t.Fatalf("stored %q, want %q", got, want)
	}
	for _, want := range []string{`name: "old"`, `"runs:create"`, strings.TrimPrefix(named, defOld+"@")} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("result lacks %q:\n%s", want, out.String())
		}
	}
}

// Item 3: run with a LOOMCYCLE_SECRET_KEY other than the server's, the
// command would wrap under the wrong pepper and kill the tokens. With rows
// under another key and none under this one, it refuses and prints this
// key's pepper fingerprint.
func TestTokensRewrap_RefusesWhatLooksLikeTheWrongSecretKey(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	serverKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	env := config.Env{SecretKey: serverKey}
	h, err := env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_00000000000000e9", Name: "minted", TenantID: "default", Subject: "m",
		TokenHash: h.Hash("lct_minted"), AllowedScopes: []string{"runs:create"},
	}); err != nil {
		t.Fatal(err)
	}
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2; stdout: %s", rc, out.String())
	}
	mine, _ := (&config.Env{SecretKey: testSecretKey()}).TokenHasher()
	if !strings.Contains(out.String()+errb.String(), mine.Fingerprint()) {
		t.Errorf("the refusal does not print this key's pepper fingerprint %s:\n%s%s", mine.Fingerprint(), out.String(), errb.String())
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Errorf("the refused run rewrote the row to %q", got)
	}
}

// Item 5: a def_id not of the minted shape is suspicious; non-ASCII in the
// name, tenant or subject is warned about (lookalikes) but not refused.
func TestTokensRewrap_LookalikesAreFlagged(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_0000000000000\u3164a0", Name: "x", TenantID: "default", Subject: "x",
		TokenHash: auth.HashToken("", "lct_blank"), AllowedScopes: []string{"runs:create"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_00000000000000c1", Name: "\u0430dmin", TenantID: "default", Subject: "x",
		TokenHash: auth.HashToken("", "lct_cyr"), AllowedScopes: []string{"runs:create"},
	}); err != nil {
		t.Fatal(err)
	}
	out := review(t, cfgPath)
	if !strings.Contains(out, "SUSPICIOUS") {
		t.Errorf("a blank-rendering def_id was not flagged:\n%s", out)
	}
	if !strings.Contains(out, "non-ASCII") {
		t.Errorf("a lookalike name was not warned about:\n%s", out)
	}
	if !strings.Contains(out, reviewFingerprint(t, out, "def_00000000000000c1")) {
		t.Error("a non-ASCII name made its row unnamable")
	}
}

// Item 7: --preset is accepted, as the server's own flag is.
func TestTokensRewrap_AcceptsThePresetFlag(t *testing.T) {
	_, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--preset", "base", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
}
