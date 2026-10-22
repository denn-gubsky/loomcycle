package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func seedTokenRow(t *testing.T, st store.Store, defID, token string, scopes ...string) {
	t.Helper()
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: defID, Name: defID + "-name", TenantID: "acme", Subject: defID + "-sub",
		TokenHash: auth.HashToken("", token), AllowedScopes: scopes,
	}); err != nil {
		t.Fatal(err)
	}
}

func rowHash(t *testing.T, st store.Store, defID string) string {
	t.Helper()
	row, err := st.OperatorTokenDefGet(context.Background(), defID)
	if err != nil {
		t.Fatal(err)
	}
	return row.TokenHash
}

var digestRE = regexp.MustCompile(`--all-listed ([0-9a-f]{64})`)

func reviewDigest(t *testing.T, out string) string {
	t.Helper()
	m := digestRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the listing prints no --all-listed digest:\n%s", out)
	}
	return m[1]
}

func wrapped(t *testing.T, token string) string {
	t.Helper()
	env := config.Env{SecretKey: testSecretKey()}
	h, err := env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	return h.WrappedHash(token)
}

// With no confirmation the command is a review: it lists each unpeppered row
// with what it would grant, flags an admin row, warns, and writes nothing.
func TestTokensRewrap_WithoutConfirmationListsAndWritesNothing(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	seedTokenRow(t, st, "def_00000000000000ac", "lct_adm", auth.ScopeAdmin)
	before, beforeAdm := tokensStoredHash(t, st), rowHash(t, st, "def_00000000000000ac")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if tokensStoredHash(t, st) != before || rowHash(t, st, "def_00000000000000ac") != beforeAdm {
		t.Fatal("the command wrote without the operator naming any row")
	}
	for _, want := range []string{
		"def_00000000000000a1", "old", "default", "runs:create",
		"def_00000000000000ac", "def_00000000000000ac-name", "acme", "def_00000000000000ac-sub", "ADMIN",
		"you never minted", "nothing was changed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing lacks %q:\n%s", want, out.String())
		}
	}
	reviewDigest(t, out.String())
}

// --def-id rewraps exactly the named rows; an unpeppered row nobody named
// stays as it is.
func TestTokensRewrap_DefIDRewrapsOnlyTheNamedRows(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	seedTokenRow(t, st, "def_00000000000000f0", "lct_forged", auth.ScopeAdmin)
	forged := rowHash(t, st, "def_00000000000000f0")
	var out, errb bytes.Buffer
	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if got, want := tokensStoredHash(t, st), wrapped(t, "lct_old"); got != want {
		t.Errorf("the named row is on %q, want %q", got, want)
	}
	if got := rowHash(t, st, "def_00000000000000f0"); got != forged {
		t.Errorf("a row nobody named was rewritten to %q", got)
	}
}

// A named def_id that is not an unpeppered row now refuses the whole run.
func TestTokensRewrap_DefIDNotUnpepperedRefusesAndWritesNothing(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named, "--def-id", "def_00000000000000dd@0000000000000000"}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2; stderr: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "not a live unpeppered") || !strings.Contains(errb.String(), "def_00000000000000dd") {
		t.Errorf("stderr does not name the row:\n%s", errb.String())
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Errorf("a refused run rewrote def_00000000000000a1 to %q", got)
	}
}

// --all-listed rewraps the set the review listed, all of it.
func TestTokensRewrap_AllListedRewrapsTheReviewedSet(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	seedTokenRow(t, st, "def_0000000000000022", "lct_two", "runs:create")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("review rc = %d", rc)
	}
	digest := reviewDigest(t, out.String())
	out.Reset()
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--all-listed", digest}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if got, want := tokensStoredHash(t, st), wrapped(t, "lct_old"); got != want {
		t.Errorf("def_00000000000000a1 is on %q, want %q", got, want)
	}
	if got, want := rowHash(t, st, "def_0000000000000022"), wrapped(t, "lct_two"); got != want {
		t.Errorf("def_0000000000000022 is on %q, want %q", got, want)
	}
}

// A row added, or a listed row's hash changed, after the review: the digest
// no longer matches and nothing is written.
func TestTokensRewrap_AllListedRefusedWhenTheSetChangedSinceTheReview(t *testing.T) {
	for name, change := range map[string]func(*testing.T, store.Store){
		"row added": func(t *testing.T, st store.Store) {
			seedTokenRow(t, st, "def_00000000000000f0", "lct_forged", auth.ScopeAdmin)
		},
		"hash changed": func(t *testing.T, st store.Store) {
			if _, err := st.OperatorTokenRehash(context.Background(), "def_00000000000000a1", auth.HashToken("", "lct_old"), auth.HashToken("", "lct_mine")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
			var out, errb bytes.Buffer
			if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
				t.Fatalf("review rc = %d", rc)
			}
			digest := reviewDigest(t, out.String())
			change(t, st)
			before := tokensStoredHash(t, st)
			if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--all-listed", digest}, &out, &errb); rc != 2 {
				t.Fatalf("rc = %d, want 2; stderr: %s", rc, errb.String())
			}
			if got := tokensStoredHash(t, st); got != before {
				t.Errorf("a refused run rewrote def_00000000000000a1 to %q", got)
			}
			if name == "row added" && rowHash(t, st, "def_00000000000000f0") != auth.HashToken("", "lct_forged") {
				t.Error("a refused run rewrote the added row")
			}
		})
	}
}

func TestTokensRewrap_DefIDAndAllListedTogetherIsAUsageError(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", defOld + "@0000000000000000", "--all-listed", strings.Repeat("0", 64)}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Errorf("a usage error rewrote the row to %q", got)
	}
}

// A refused digest does not print the new one: copying it from the error
// would confirm the changed set without reviewing it.
func TestTokensRewrap_StaleDigestRefusalDoesNotPrintTheNewDigest(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("review rc = %d", rc)
	}
	digest := reviewDigest(t, out.String())
	seedTokenRow(t, st, "def_00000000000000f0", "lct_forged", auth.ScopeAdmin)
	out.Reset()
	errb.Reset()
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--all-listed", digest}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(out.String() + errb.String()) {
		t.Errorf("the refusal printed a digest:\n%s%s", out.String(), errb.String())
	}
}

// A forged row whose fields carry terminal escapes or newlines must not be
// able to hide itself, or fake another row, in the review: every field from
// the database is printed escaped, the row is flagged, it cannot be
// rewrapped either way, and the row count sits right before the digest.
func TestTokensRewrap_ControlCharactersInARowAreEscapedFlaggedAndRefused(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	evil := "x\x1b[1A\x1b[2K\r\n  def_fake\n      name: ok"
	evilRow, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_evil\x1b[8m", Name: evil, TenantID: "acme‮", Subject: "s\x07",
		TokenHash: auth.HashToken("", "lct_evil"), AllowedScopes: []string{auth.ScopeAdmin + "\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	evilHash := auth.HashToken("", "lct_evil")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("review rc = %d, stderr: %s", rc, errb.String())
	}
	listing := out.String()
	for _, raw := range []string{"\x1b", "\r", "\x07", "‮", "\n  def_fake"} {
		if strings.Contains(listing, raw) {
			t.Errorf("the listing printed %q raw:\n%s", raw, listing)
		}
	}
	if !strings.Contains(listing, "SUSPICIOUS") {
		t.Errorf("the row was not flagged:\n%s", listing)
	}
	lines := strings.Split(listing, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "  loomcycle tokens rewrap --all-listed ") {
			if i == 0 || !strings.Contains(lines[i-1], "2 row(s)") {
				t.Errorf("the line before the digest does not carry the row count: %q", lines[max(i-1, 0)])
			}
		}
	}

	for name, args := range map[string][]string{
		"all-listed": {"--all-listed", reviewDigest(t, listing)},
		"def-id":     {"--def-id", "def_evil\x1b[8m@" + lchttp.TokenRowFingerprint(evilRow)},
	} {
		out.Reset()
		errb.Reset()
		if rc := runTokens(append([]string{"rewrap", "--config", cfgPath}, args...), &out, &errb); rc != 2 {
			t.Errorf("%s: rc = %d, want 2; stderr: %s", name, rc, errb.String())
		}
		if strings.Contains(errb.String(), "\x1b") {
			t.Errorf("%s: the refusal printed an escape raw: %q", name, errb.String())
		}
		if rowHash(t, st, "def_evil\x1b[8m") != evilHash {
			t.Errorf("%s: the suspicious row was rewrapped", name)
		}
	}
	if got := tokensStoredHash(t, st); got != auth.HashToken("", "lct_old") {
		t.Errorf("a refused run rewrote def_00000000000000a1 to %q", got)
	}
}
