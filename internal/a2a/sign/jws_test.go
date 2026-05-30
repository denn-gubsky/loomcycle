package sign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
)

func testCard() *a2asdk.AgentCard {
	return &a2asdk.AgentCard{
		Name:        "loomcycle-test",
		Description: "a test agent card",
		Version:     "1.0.0",
		Skills: []a2asdk.AgentSkill{
			{ID: "research", Name: "Research", Description: "does research"},
		},
	}
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

// TestSignCard_VerifyRoundTrip asserts a card signed with SignCard
// verifies against the matching public key — the basic happy path the
// served-card signing relies on.
func TestSignCard_VerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	card := testCard()
	if err := SignCard(card, key); err != nil {
		t.Fatalf("SignCard: %v", err)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("got %d signatures, want 1", len(card.Signatures))
	}
	if err := VerifyCard(card, &key.PublicKey); err != nil {
		t.Fatalf("VerifyCard on a freshly-signed card: %v", err)
	}
}

// TestVerifyCard_RejectsTamperedCard asserts that mutating any signed
// field after signing breaks verification — the tamper-evidence property
// the whole signing scheme exists to provide.
func TestVerifyCard_RejectsTamperedCard(t *testing.T) {
	key := testKey(t)
	card := testCard()
	if err := SignCard(card, key); err != nil {
		t.Fatalf("SignCard: %v", err)
	}
	// Tamper: change the description after signing.
	card.Description = "a MALICIOUSLY altered description"
	if err := VerifyCard(card, &key.PublicKey); err == nil {
		t.Fatal("VerifyCard accepted a tampered card; want failure")
	}
}

// TestVerifyCard_RejectsWrongKey asserts a card signed by one key does
// not verify against an unrelated public key.
func TestVerifyCard_RejectsWrongKey(t *testing.T) {
	signer := testKey(t)
	other := testKey(t)
	card := testCard()
	if err := SignCard(card, signer); err != nil {
		t.Fatalf("SignCard: %v", err)
	}
	if err := VerifyCard(card, &other.PublicKey); err == nil {
		t.Fatal("VerifyCard accepted a signature from the wrong key; want failure")
	}
}

// TestVerifyCard_UnsignedCardReturnsErrNoSignature asserts the
// sentinel-error contract the tolerant/strict callers branch on.
func TestVerifyCard_UnsignedCardReturnsErrNoSignature(t *testing.T) {
	key := testKey(t)
	if err := VerifyCard(testCard(), &key.PublicKey); !errors.Is(err, ErrNoSignature) {
		t.Fatalf("VerifyCard on unsigned card = %v, want ErrNoSignature", err)
	}
}

// TestSignCardSelfContained_VerifyRoundTrip asserts the self-contained
// path (public key embedded in the JWS protected header) verifies via
// VerifyCardSelfContained with no separately-distributed key — the path
// the outbound client uses when verify_signed_card=true and the path the
// server uses to sign its served card.
func TestSignCardSelfContained_VerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	card := testCard()
	if err := SignCardSelfContained(card, key); err != nil {
		t.Fatalf("SignCardSelfContained: %v", err)
	}
	if err := VerifyCardSelfContained(card); err != nil {
		t.Fatalf("VerifyCardSelfContained on a freshly-signed card: %v", err)
	}
}

// TestVerifyCardSelfContained_RejectsTamperedCard asserts the embedded-
// key path is still tamper-evident: an attacker cannot edit the card and
// re-derive a passing signature without the private key, even though the
// public key travels with the card.
func TestVerifyCardSelfContained_RejectsTamperedCard(t *testing.T) {
	key := testKey(t)
	card := testCard()
	if err := SignCardSelfContained(card, key); err != nil {
		t.Fatalf("SignCardSelfContained: %v", err)
	}
	card.Name = "spoofed-agent"
	if err := VerifyCardSelfContained(card); err == nil {
		t.Fatal("VerifyCardSelfContained accepted a tampered card; want failure")
	}
}

// TestVerifyCardSelfContained_UnsignedReturnsErrNoSignature asserts the
// unsigned-card sentinel on the self-contained verifier too.
func TestVerifyCardSelfContained_UnsignedReturnsErrNoSignature(t *testing.T) {
	if err := VerifyCardSelfContained(testCard()); !errors.Is(err, ErrNoSignature) {
		t.Fatalf("VerifyCardSelfContained on unsigned card = %v, want ErrNoSignature", err)
	}
}

// TestParseECPrivateKey_RoundTripsSEC1AndPKCS8 asserts the key parser
// accepts both PEM encodings the signing-key env var may hold, and that a
// parsed key actually signs.
func TestParseECPrivateKey_RoundTripsSEC1AndPKCS8(t *testing.T) {
	for _, tc := range []struct {
		name string
		pem  func(*ecdsa.PrivateKey) []byte
	}{
		{"sec1", sec1PEM},
		{"pkcs8", pkcs8PEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := testKey(t)
			parsed, err := ParseECPrivateKey(tc.pem(key))
			if err != nil {
				t.Fatalf("ParseECPrivateKey(%s): %v", tc.name, err)
			}
			card := testCard()
			if err := SignCard(card, parsed); err != nil {
				t.Fatalf("SignCard with parsed key: %v", err)
			}
			if err := VerifyCard(card, &key.PublicKey); err != nil {
				t.Fatalf("VerifyCard against original pub: %v", err)
			}
		})
	}
}

// TestParseECPrivateKey_RejectsNonPEM asserts a clear error for garbage
// input (no key material leaks into the error — it just reports no PEM).
func TestParseECPrivateKey_RejectsNonPEM(t *testing.T) {
	if _, err := ParseECPrivateKey([]byte("not a pem")); err == nil {
		t.Fatal("ParseECPrivateKey accepted non-PEM input; want error")
	}
}
