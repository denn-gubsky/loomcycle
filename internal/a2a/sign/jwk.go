package sign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
)

// ecJWK is the minimal JWK shape for an EC public key (RFC 7517), as
// embedded in a JWS protected header's `jwk` parameter. We support only
// the P-256 curve ES256 uses.
type ecJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// PublicJWK renders an ECDSA P-256 public key as a JWK for embedding in
// the JWS protected header. This lets a signed card be self-contained:
// the verifier extracts the key from the signature rather than needing a
// separately-distributed key. (Trust in a self-described key is exactly
// as strong as trust in the transport that delivered the card — TLS to
// the operator-registered peer URL; A2A-6 leaves a stronger PKI binding
// to a later slice.)
func PublicJWK(pub *ecdsa.PublicKey) (json.RawMessage, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, errors.New("a2a sign: public key is not P-256")
	}
	size := (pub.Curve.Params().BitSize + 7) / 8
	xb := make([]byte, size)
	yb := make([]byte, size)
	pub.X.FillBytes(xb)
	pub.Y.FillBytes(yb)
	return json.Marshal(ecJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(xb),
		Y:   base64.RawURLEncoding.EncodeToString(yb),
	})
}

// publicKeyFromJWK parses an embedded EC P-256 JWK back into a public
// key. Rejects any other key type/curve so a verifier never accepts a
// key shape it cannot actually validate.
func publicKeyFromJWK(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	var jwk ecJWK
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, fmt.Errorf("a2a sign: parse jwk: %w", err)
	}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return nil, fmt.Errorf("a2a sign: unsupported jwk kty=%q crv=%q (want EC/P-256)", jwk.Kty, jwk.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("a2a sign: decode jwk x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("a2a sign: decode jwk y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}

// es256ProtectedWithJWK is the protected header when the public key is
// embedded for self-contained verification.
type es256ProtectedWithJWK struct {
	Alg string          `json:"alg"`
	JWK json.RawMessage `json:"jwk,omitempty"`
}

// SignCardSelfContained signs card like SignCard but embeds the matching
// public key as a `jwk` protected-header param, so a verifier can call
// VerifyCardSelfContained without a separately-distributed key.
func SignCardSelfContained(card *a2asdk.AgentCard, key *ecdsa.PrivateKey) error {
	if card == nil {
		return errors.New("a2a sign: nil card")
	}
	if key == nil {
		return errors.New("a2a sign: nil signing key")
	}
	jwk, err := PublicJWK(&key.PublicKey)
	if err != nil {
		return err
	}
	protected, err := json.Marshal(es256ProtectedWithJWK{Alg: "ES256", JWK: jwk})
	if err != nil {
		return fmt.Errorf("a2a sign: marshal protected header: %w", err)
	}
	protectedB64 := base64.RawURLEncoding.EncodeToString(protected)

	clone := *card
	clone.Signatures = nil
	canon, err := Canonicalize(&clone)
	if err != nil {
		return err
	}
	return signWith(card, key, protectedB64, append([]byte(protectedB64+"."), canon...))
}

// VerifyCardSelfContained verifies the first signature on card using the
// public key embedded in that signature's protected header (the `jwk`
// param written by SignCardSelfContained). Returns ErrNoSignature for an
// unsigned card; a descriptive error when the signature is present but
// the embedded key is missing/malformed or the signature does not
// verify.
//
// This is the verification path the OUTBOUND client uses when an
// A2AAgentDef sets verify_signed_card=true: it proves the card was
// signed by whoever holds the private key matching the embedded public
// key, and that the card bytes were not tampered after signing.
func VerifyCardSelfContained(card *a2asdk.AgentCard) error {
	if card == nil {
		return errors.New("a2a sign: nil card")
	}
	if len(card.Signatures) == 0 {
		return ErrNoSignature
	}
	candidate := card.Signatures[0]

	protectedJSON, err := base64.RawURLEncoding.DecodeString(candidate.Protected)
	if err != nil {
		return fmt.Errorf("a2a sign: decode protected header: %w", err)
	}
	var hdr es256ProtectedWithJWK
	if err := json.Unmarshal(protectedJSON, &hdr); err != nil {
		return fmt.Errorf("a2a sign: parse protected header: %w", err)
	}
	if hdr.Alg != "ES256" {
		return fmt.Errorf("a2a sign: unsupported alg %q (want ES256)", hdr.Alg)
	}
	if len(hdr.JWK) == 0 {
		return errors.New("a2a sign: signature carries no embedded jwk to verify against")
	}
	pub, err := publicKeyFromJWK(hdr.JWK)
	if err != nil {
		return err
	}
	return VerifyCard(card, pub)
}
