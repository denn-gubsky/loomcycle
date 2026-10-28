package auth

import "testing"

// A stored authority this build does not know is an error, never read as
// either mode: frozen would hand the token its stored scopes, inherit its
// user's.
func TestTokenInheritsAuthority_OnlyEmptyAndInheritAreKnown(t *testing.T) {
	for stored, want := range map[string]bool{"": false, AuthorityInherit: true} {
		got, err := TokenInheritsAuthority(stored)
		if err != nil || got != want {
			t.Errorf("TokenInheritsAuthority(%q) = %v, %v; want %v", stored, got, err, want)
		}
	}
	// "frozen" is the reported name, never a stored value: a mint stores "".
	for _, stored := range []string{AuthorityFrozen, "Inherit", "inherit ", "all"} {
		if _, err := TokenInheritsAuthority(stored); err == nil {
			t.Errorf("TokenInheritsAuthority(%q) accepted an unknown mode", stored)
		}
	}
}

func TestPrincipalAuthorityMode_NamesTheMode(t *testing.T) {
	if got := (Principal{}).AuthorityMode(); got != AuthorityFrozen {
		t.Errorf("zero principal = %q, want frozen", got)
	}
	if got := (Principal{InheritsAuthority: true}).AuthorityMode(); got != AuthorityInherit {
		t.Errorf("inherit principal = %q, want inherit", got)
	}
}
