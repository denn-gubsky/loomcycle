package credential

import (
	"encoding/hex"
	"testing"
)

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

// Pinned the same way (info "loomcycle/password-pepper/v1"): a change would
// make every stored password fail to verify. It must also differ from the
// token pepper derived from the same key.
func TestDerivePasswordPepper_MatchesTheHKDFKnownAnswer(t *testing.T) {
	got, err := DerivePasswordPepper(testKey(1))
	if err != nil {
		t.Fatalf("DerivePasswordPepper: %v", err)
	}
	const want = "bf6bcdc4f993ef254967202967f094c8c46c0223a05bc28a96890116a35f7632"
	if hex.EncodeToString(got) != want {
		t.Fatalf("pepper = %x, want %s", got, want)
	}
	tok, _ := DeriveTokenPepper(testKey(1))
	if tok == hex.EncodeToString(got) {
		t.Error("the password pepper must differ from the token pepper of the same key")
	}
	if p, err := DerivePasswordPepper(" "); err != nil || p != nil {
		t.Errorf("empty key = (%x, %v), want (nil, nil)", p, err)
	}
	if _, err := DerivePasswordPepper("not-base64!"); err == nil {
		t.Error("a malformed key must be an error")
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

// Pinned the same way (info "loomcycle/otel-user/v1", computed outside Go): a
// change would make every user hash already exported to a trace backend stop
// matching the hash an investigation computes for the same user.
func TestDeriveOTelUserKey_MatchesTheHKDFKnownAnswer(t *testing.T) {
	got, err := DeriveOTelUserKey(testKey(1))
	if err != nil {
		t.Fatalf("DeriveOTelUserKey: %v", err)
	}
	const want = "6888a893992739563b4d15a9e417320e7dd2ec2748da497abd27ee9cbae9b2f4"
	if hex.EncodeToString(got) != want {
		t.Fatalf("key = %x, want %s", got, want)
	}
	if audit, _ := DeriveAuditChainKey(testKey(1)); hex.EncodeToString(audit) == want {
		t.Error("the otel user key must differ from the audit chain key of the same master key")
	}
	if k, err := DeriveOTelUserKey(" "); err != nil || k != nil {
		t.Errorf("empty key = (%x, %v), want (nil, nil)", k, err)
	}
	if _, err := DeriveOTelUserKey("not-base64!"); err == nil {
		t.Error("a malformed key must be an error")
	}
}

// The client-tools ticket key (RFC GJ) has its own label: it must differ from
// every other key derived from the same master key, and no key means no
// ticket key.
func TestDeriveClientToolsTicketKey_DistinctFromTheOtherDerivedKeys(t *testing.T) {
	got, err := DeriveClientToolsTicketKey(testKey(1))
	if err != nil || len(got) != 32 {
		t.Fatalf("DeriveClientToolsTicketKey = (%x, %v), want 32 bytes", got, err)
	}
	tok, _ := DeriveTokenPepper(testKey(1))
	pw, _ := DerivePasswordPepper(testKey(1))
	chain, _ := DeriveAuditChainKey(testKey(1))
	otelKey, _ := DeriveOTelUserKey(testKey(1))
	for name, other := range map[string]string{"token pepper": tok, "password pepper": hex.EncodeToString(pw),
		"audit chain": hex.EncodeToString(chain), "otel user": hex.EncodeToString(otelKey)} {
		if other == hex.EncodeToString(got) {
			t.Errorf("the ticket key equals the %s", name)
		}
	}
	if k, err := DeriveClientToolsTicketKey(" "); err != nil || k != nil {
		t.Errorf("empty key = (%x, %v), want (nil, nil)", k, err)
	}
	if _, err := DeriveClientToolsTicketKey("not-base64!"); err == nil {
		t.Error("a malformed key must be an error")
	}
}
