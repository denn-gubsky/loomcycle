package credential

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

// testKey returns a deterministic, valid 32-byte base64 KEK for tests.
func testKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func mustSealer(t *testing.T, cur, prev string) *Sealer {
	t.Helper()
	s, err := NewSealer(cur, prev)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

var idA = Identity{TenantID: "tnt-a", Scope: "tenant", ScopeID: "", Name: "serper_api_key"}

func TestSealer_RoundTrip(t *testing.T) {
	s := mustSealer(t, testKey(1), "")
	if !s.Enabled() {
		t.Fatal("sealer with a key should be Enabled")
	}
	secret := []byte("sk-super-secret-value-123")
	sealed, err := s.Seal(secret, idA)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed.Ciphertext == "" || sealed.Nonce == "" || sealed.KeyID == "" {
		t.Fatalf("sealed envelope incomplete: %+v", sealed)
	}
	// The plaintext must not appear anywhere in the envelope.
	if bytes.Contains([]byte(sealed.Ciphertext+sealed.Nonce), secret) {
		t.Fatal("plaintext leaked into the sealed envelope")
	}
	got, err := s.Open(sealed, idA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round-trip = %q, want %q", got, secret)
	}
}

func TestSealer_AADBindsIdentity(t *testing.T) {
	s := mustSealer(t, testKey(1), "")
	sealed, err := s.Seal([]byte("v"), idA)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Every single-field divergence in the identity must fail decryption.
	cases := map[string]Identity{
		"other tenant":  {TenantID: "tnt-b", Scope: idA.Scope, ScopeID: idA.ScopeID, Name: idA.Name},
		"other scope":   {TenantID: idA.TenantID, Scope: "user", ScopeID: idA.ScopeID, Name: idA.Name},
		"other scopeid": {TenantID: idA.TenantID, Scope: idA.Scope, ScopeID: "u2", Name: idA.Name},
		"other name":    {TenantID: idA.TenantID, Scope: idA.Scope, ScopeID: idA.ScopeID, Name: "exa_api_key"},
	}
	for label, id := range cases {
		if _, err := s.Open(sealed, id); !errors.Is(err, ErrDecrypt) {
			t.Errorf("Open under %s identity = %v, want ErrDecrypt (a copied row must not decrypt)", label, err)
		}
	}
}

func TestSealer_Disabled(t *testing.T) {
	s := mustSealer(t, "", "")
	if s.Enabled() {
		t.Fatal("sealer with no key should be disabled")
	}
	if _, err := s.Seal([]byte("v"), idA); !errors.Is(err, ErrNoKey) {
		t.Errorf("Seal with no key = %v, want ErrNoKey (fail-closed, never plaintext)", err)
	}
	if _, err := s.Open(Sealed{KeyID: "x", Nonce: "x", Ciphertext: "x"}, idA); !errors.Is(err, ErrNoKey) {
		t.Errorf("Open with no key = %v, want ErrNoKey", err)
	}
}

func TestSealer_Tamper(t *testing.T) {
	s := mustSealer(t, testKey(1), "")
	sealed, _ := s.Seal([]byte("do-not-tamper"), idA)
	raw, _ := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	raw[0] ^= 0xff // flip a byte
	sealed.Ciphertext = base64.StdEncoding.EncodeToString(raw)
	if _, err := s.Open(sealed, idA); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open of tampered ciphertext = %v, want ErrDecrypt", err)
	}
}

func TestSealer_BadKey(t *testing.T) {
	if _, err := NewSealer("not-valid-base64!!!", ""); err == nil {
		t.Error("NewSealer with non-base64 key should error")
	}
	if _, err := NewSealer(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)), ""); err == nil {
		t.Error("NewSealer with a 16-byte key should error (need 32 for AES-256)")
	}
}

func TestSealer_KEKRotation(t *testing.T) {
	old := mustSealer(t, testKey(1), "")
	sealedOld, _ := old.Seal([]byte("rotated-secret"), idA)

	// Rotate: current=key2, previous=key1. Old rows still open; new writes use key2.
	rotated := mustSealer(t, testKey(2), testKey(1))
	got, err := rotated.Open(sealedOld, idA)
	if err != nil || string(got) != "rotated-secret" {
		t.Fatalf("post-rotation Open of an old-key row = (%q,%v), want the secret", got, err)
	}
	if !rotated.NeedsReseal(sealedOld) {
		t.Error("NeedsReseal should be true for a row sealed under the previous key")
	}
	resealed, _ := rotated.Seal([]byte("rotated-secret"), idA)
	if resealed.KeyID == sealedOld.KeyID {
		t.Error("re-seal should use the new current key (different key_id)")
	}
	if rotated.NeedsReseal(resealed) {
		t.Error("a freshly re-sealed row should not need re-sealing")
	}

	// After the grace window ends (only key2 configured), old-key rows no longer open.
	final := mustSealer(t, testKey(2), "")
	if _, err := final.Open(sealedOld, idA); !errors.Is(err, ErrDecrypt) {
		t.Errorf("once the previous key is dropped, old rows = %v, want ErrDecrypt", err)
	}
	if _, err := final.Open(resealed, idA); err != nil {
		t.Errorf("the re-sealed row should still open under the current key: %v", err)
	}
}
