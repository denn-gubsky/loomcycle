package auth

import (
	"strings"
	"testing"
)

func TestHashToken_DeterministicAndPeppered(t *testing.T) {
	a := HashToken("pepper", "lct_secret")
	b := HashToken("pepper", "lct_secret")
	if a != b {
		t.Fatal("HashToken not deterministic for the same (pepper, token)")
	}
	if HashToken("pepper", "lct_secret") == HashToken("other", "lct_secret") {
		t.Error("different peppers must yield different hashes (pepper not mixed in)")
	}
	if HashToken("pepper", "a") == HashToken("pepper", "b") {
		t.Error("different tokens must yield different hashes")
	}
	if strings.Contains(a, "lct_secret") {
		t.Error("hash must not contain the plaintext")
	}
	if len(a) != 64 {
		t.Errorf("hex SHA-256 must be 64 chars, got %d", len(a))
	}
}

func TestMintToken_FormatAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, sfx, err := MintToken()
		if err != nil {
			t.Fatalf("MintToken: %v", err)
		}
		if !strings.HasPrefix(tok, TokenPrefix) {
			t.Fatalf("token %q lacks prefix %q", tok, TokenPrefix)
		}
		body := strings.TrimPrefix(tok, TokenPrefix)
		if len(sfx) != TokenSuffixLen || !strings.HasPrefix(body, sfx) {
			t.Fatalf("suffix %q is not the first %d chars of body %q", sfx, TokenSuffixLen, body)
		}
		// base58 has no 0/O/I/l.
		if strings.ContainsAny(body, "0OIl") {
			t.Errorf("token body %q contains an ambiguous base58 char", body)
		}
		if seen[tok] {
			t.Fatalf("MintToken produced a duplicate in 1000 draws: %q", tok)
		}
		seen[tok] = true
	}
}

func TestScopeCatalog(t *testing.T) {
	if !ValidScope(ScopeAdmin) || !ValidScope(ScopeRunsCreate) {
		t.Error("catalog scopes must validate")
	}
	if ValidScope("not-a-scope") {
		t.Error("an invented scope must not validate")
	}
	if bad := UnknownScopes([]string{ScopeRunsRead, "bogus"}); len(bad) != 1 || bad[0] != "bogus" {
		t.Errorf("UnknownScopes = %v, want [bogus]", bad)
	}
	// ScopeAdmin is a superuser scope.
	if !HasScope([]string{ScopeAdmin}, ScopeRunsCreate) {
		t.Error("substrate:admin must satisfy any required scope")
	}
	if HasScope([]string{ScopeRunsRead}, ScopeRunsCreate) {
		t.Error("runs:read must NOT satisfy runs:create")
	}
	// memory:read / memory:write were removed as inert dead config — a
	// scope no route enforces must not be grantable (it would be a false
	// limitation). Guards against silent re-introduction without a
	// route that enforces it.
	if ValidScope("memory:read") || ValidScope("memory:write") {
		t.Error("memory:read/write must not be in the catalog (inert — no route enforces them)")
	}
}
