package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

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
	seedTokenRow(t, st, "def_adm", "lct_adm", auth.ScopeAdmin)
	before, beforeAdm := tokensStoredHash(t, st), rowHash(t, st, "def_adm")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if tokensStoredHash(t, st) != before || rowHash(t, st, "def_adm") != beforeAdm {
		t.Fatal("the command wrote without the operator naming any row")
	}
	for _, want := range []string{
		"def_old", "old", "default", "runs:create",
		"def_adm", "def_adm-name", "acme", "def_adm-sub", "ADMIN",
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
	seedTokenRow(t, st, "def_forged", "lct_forged", auth.ScopeAdmin)
	forged := rowHash(t, st, "def_forged")
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", "def_old"}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if got, want := tokensStoredHash(t, st), wrapped(t, "lct_old"); got != want {
		t.Errorf("the named row is on %q, want %q", got, want)
	}
	if got := rowHash(t, st, "def_forged"); got != forged {
		t.Errorf("a row nobody named was rewritten to %q", got)
	}
}

// A named def_id that is not an unpeppered row now refuses the whole run.
func TestTokensRewrap_DefIDNotUnpepperedRefusesAndWritesNothing(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", "def_old", "--def-id", "def_gone"}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2; stderr: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "def_gone") {
		t.Errorf("stderr does not name the row:\n%s", errb.String())
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Errorf("a refused run rewrote def_old to %q", got)
	}
}

// --all-listed rewraps the set the review listed, all of it.
func TestTokensRewrap_AllListedRewrapsTheReviewedSet(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	seedTokenRow(t, st, "def_two", "lct_two", "runs:create")
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
		t.Errorf("def_old is on %q, want %q", got, want)
	}
	if got, want := rowHash(t, st, "def_two"), wrapped(t, "lct_two"); got != want {
		t.Errorf("def_two is on %q, want %q", got, want)
	}
}

// A row added, or a listed row's hash changed, after the review: the digest
// no longer matches and nothing is written.
func TestTokensRewrap_AllListedRefusedWhenTheSetChangedSinceTheReview(t *testing.T) {
	for name, change := range map[string]func(*testing.T, store.Store){
		"row added": func(t *testing.T, st store.Store) {
			seedTokenRow(t, st, "def_forged", "lct_forged", auth.ScopeAdmin)
		},
		"hash changed": func(t *testing.T, st store.Store) {
			if _, err := st.OperatorTokenRehash(context.Background(), "def_old", auth.HashToken("", "lct_old"), auth.HashToken("", "lct_mine")); err != nil {
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
				t.Errorf("a refused run rewrote def_old to %q", got)
			}
			if name == "row added" && rowHash(t, st, "def_forged") != auth.HashToken("", "lct_forged") {
				t.Error("a refused run rewrote the added row")
			}
		})
	}
}

func TestTokensRewrap_DefIDAndAllListedTogetherIsAUsageError(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", "def_old", "--all-listed", strings.Repeat("0", 64)}, &out, &errb); rc != 2 {
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
	seedTokenRow(t, st, "def_forged", "lct_forged", auth.ScopeAdmin)
	out.Reset()
	errb.Reset()
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--all-listed", digest}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if regexp.MustCompile(`[0-9a-f]{64}`).MatchString(out.String() + errb.String()) {
		t.Errorf("the refusal printed a digest:\n%s%s", out.String(), errb.String())
	}
}
