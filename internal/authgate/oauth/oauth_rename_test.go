package oauth

import "testing"

// A user renaming their login keeps the same subject, and the renamable
// fields never reach the claims an identity map reads.
func TestOAuth_LoginRenameKeepsTheSubjectAndMutableFieldsAreNotClaims(t *testing.T) {
	g, err := New(Config{Name: "gh", Preset: PresetGitHub, ClientID: "c", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := g.assertion(profile{id: "4242", login: "alice", name: "Alice"})
	a2, _ := g.assertion(profile{id: "4242", login: "mallory-was-alice", name: "Mallory"})
	if a1.Subject != a2.Subject {
		t.Fatalf("a rename changed the subject: %q vs %q", a1.Subject, a2.Subject)
	}
	for _, k := range []string{"login", "name", "username", "preferred_username"} {
		if _, ok := a2.Claims[k]; ok {
			t.Fatalf("claims carry the renamable %q: %v", k, a2.Claims)
		}
	}
	if a2.DisplayName != "Mallory" {
		t.Fatalf("display name = %q", a2.DisplayName)
	}
}
