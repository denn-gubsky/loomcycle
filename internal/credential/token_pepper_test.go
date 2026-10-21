package credential

import "testing"

// The pepper is pinned to a value computed outside Go (RFC 5869 HKDF-SHA256,
// zero salt, info "loomcycle/operator-token-pepper/v1"). A change to the
// derivation would silently log out every token hashed under it.
func TestDeriveTokenPepper_MatchesTheHKDFKnownAnswer(t *testing.T) {
	got, err := DeriveTokenPepper(testKey(1))
	if err != nil {
		t.Fatalf("DeriveTokenPepper: %v", err)
	}
	const want = "8728c23d16d4421c6d7cc0bf61a06399ad62d6df8dce7a1af27bbe9b28b90b3d"
	if got != want {
		t.Fatalf("pepper = %s, want %s", got, want)
	}
	other, _ := DeriveTokenPepper(testKey(2))
	if other == got {
		t.Error("different keys must derive different peppers")
	}
}

func TestDeriveTokenPepper_EmptyKeyDerivesNothingAndBadKeyErrs(t *testing.T) {
	if got, err := DeriveTokenPepper("  "); err != nil || got != "" {
		t.Errorf("empty key = (%q, %v), want (\"\", nil)", got, err)
	}
	if _, err := DeriveTokenPepper("not-base64!"); err == nil {
		t.Error("a malformed key must be an error, not a silent empty pepper")
	}
}
