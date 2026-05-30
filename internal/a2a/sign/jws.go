package sign

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
)

// ErrNoSignature is returned by VerifyCard when the card carries no
// signature at all. Callers in tolerant mode treat this as "unsigned";
// callers in strict mode treat it as a verification failure.
var ErrNoSignature = errors.New("a2a sign: card carries no signature")

// es256Protected is the JWS protected header for ES256, the only alg
// RFC G mandates for AgentCards. Marshalled + base64url-encoded into
// each signature's `protected` field.
type es256Protected struct {
	Alg string `json:"alg"`
}

// SignCard computes an ES256 JWS over the JCS canonicalization of card
// (with its Signatures field cleared) and appends the result to
// card.Signatures. The key is an ECDSA P-256 private key. The card is
// mutated in place.
//
// The signing input is base64url(protected) + "." + base64url(JCS(card
// without signatures)) per RFC 7515 §5.1, except the payload is the
// canonical card bytes rather than a separate document — this is the
// "detached-over-canonical-card" convention A2A uses so a verifier can
// re-derive the payload from the card it received.
func SignCard(card *a2asdk.AgentCard, key *ecdsa.PrivateKey) error {
	if card == nil {
		return errors.New("a2a sign: nil card")
	}
	if key == nil {
		return errors.New("a2a sign: nil signing key")
	}

	protectedB64, signingInput, err := signingInputFor(card)
	if err != nil {
		return err
	}
	return signWith(card, key, protectedB64, signingInput)
}

// signWith computes the ECDSA signature over signingInput and appends a
// signature entry to card.Signatures using the given (already-base64url-
// encoded) protected header. Shared by SignCard (bare header) and
// SignCardSelfContained (header with embedded jwk).
func signWith(card *a2asdk.AgentCard, key *ecdsa.PrivateKey, protectedB64 string, signingInput []byte) error {
	digest := sha256.Sum256(signingInput)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return fmt.Errorf("a2a sign: ecdsa: %w", err)
	}
	sig := encodeES256Signature(r, s, key.Curve.Params().BitSize)
	card.Signatures = append(card.Signatures, a2asdk.AgentCardSignature{
		Protected: protectedB64,
		Signature: base64.RawURLEncoding.EncodeToString(sig),
	})
	return nil
}

// VerifyCard verifies the FIRST signature on card against pub. It
// re-derives the signing input from the card's current fields (with
// signatures cleared) and the signature's own protected header, so a
// verifier validates exactly the bytes a signer would have produced for
// the card as received. Returns ErrNoSignature when the card is
// unsigned, or a descriptive error when the signature is present but
// invalid (tampered card, wrong key, malformed encoding).
func VerifyCard(card *a2asdk.AgentCard, pub *ecdsa.PublicKey) error {
	if card == nil {
		return errors.New("a2a sign: nil card")
	}
	if pub == nil {
		return errors.New("a2a sign: nil verification key")
	}
	if len(card.Signatures) == 0 {
		return ErrNoSignature
	}
	candidate := card.Signatures[0]

	// Re-derive the signing input using the signature's own protected
	// header (not a freshly-generated one) so an alg/header the signer
	// chose is honoured byte-for-byte.
	clone := *card
	clone.Signatures = nil
	canon, err := Canonicalize(&clone)
	if err != nil {
		return err
	}
	signingInput := append([]byte(candidate.Protected+"."), canon...)

	sig, err := base64.RawURLEncoding.DecodeString(candidate.Signature)
	if err != nil {
		return fmt.Errorf("a2a sign: decode signature: %w", err)
	}
	r, s, err := decodeES256Signature(sig)
	if err != nil {
		return err
	}

	digest := sha256.Sum256(signingInput)
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return errors.New("a2a sign: signature does not verify (card tampered or wrong key)")
	}
	return nil
}

// signingInputFor returns (base64url(protected), signingInput) for a
// card: the ES256 protected header and the bytes that get hashed.
func signingInputFor(card *a2asdk.AgentCard) (protectedB64 string, signingInput []byte, err error) {
	protected, err := json.Marshal(es256Protected{Alg: "ES256"})
	if err != nil {
		return "", nil, fmt.Errorf("a2a sign: marshal protected header: %w", err)
	}
	protectedB64 = base64.RawURLEncoding.EncodeToString(protected)

	clone := *card
	clone.Signatures = nil
	canon, err := Canonicalize(&clone)
	if err != nil {
		return "", nil, err
	}
	signingInput = append([]byte(protectedB64+"."), canon...)
	return protectedB64, signingInput, nil
}

// encodeES256Signature packs (r, s) into the fixed-width R||S form JWS
// ES256 mandates (RFC 7518 §3.4): each integer left-padded to the curve
// octet length.
func encodeES256Signature(r, s *big.Int, bitSize int) []byte {
	octets := (bitSize + 7) / 8
	out := make([]byte, 2*octets)
	r.FillBytes(out[:octets])
	s.FillBytes(out[octets:])
	return out
}

// decodeES256Signature unpacks the fixed-width R||S JWS signature back
// into (r, s). It requires an even-length input so the split point is
// unambiguous.
func decodeES256Signature(sig []byte) (r, s *big.Int, err error) {
	if len(sig) == 0 || len(sig)%2 != 0 {
		return nil, nil, fmt.Errorf("a2a sign: invalid ES256 signature length %d", len(sig))
	}
	half := len(sig) / 2
	r = new(big.Int).SetBytes(sig[:half])
	s = new(big.Int).SetBytes(sig[half:])
	return r, s, nil
}

// ParseECPrivateKey parses a PEM-encoded ECDSA P-256 private key (PKCS#8
// or SEC1). This is the shape the signing-key env var holds.
func ParseECPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("a2a sign: no PEM block in signing key")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("a2a sign: parse private key: %w", err)
	}
	k, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("a2a sign: signing key is %T, want *ecdsa.PrivateKey", keyAny)
	}
	return k, nil
}
