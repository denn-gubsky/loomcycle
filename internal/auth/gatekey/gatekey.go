// Package gatekey is an external auth gate's pinned verification key (RFC FA
// §3.5): parsed from the operator's config, compared across gates at load,
// and used by internal/authgate/external to verify a gate's assertions. A
// leaf, so the config loader can validate a key without importing the
// verifier.
package gatekey

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	_ "crypto/sha256" // registers crypto.SHA256 for the thumbprint
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"
)

// minHMACSecret: HS256 is only as strong as its secret.
const minHMACSecret = 32

// Key is a gate's pinned verification key and the one algorithm it verifies.
type Key struct {
	alg    jose.SignatureAlgorithm
	verify any
	kid    string
	thumb  string
}

// Algorithm is the JWS algorithm this key verifies (EdDSA, ES256 or HS256).
func (k Key) Algorithm() string { return string(k.alg) }

// KeyID is the JWK's kid, when it had one.
func (k Key) KeyID() string { return k.kid }

// Fingerprint identifies the key without revealing it: the RFC 7638
// thumbprint of a public key, or a label for an HMAC secret (whose digest
// would be an offline-guessable verifier for the secret). For listings and
// for comparing with what a gate's discovery document says.
func (k Key) Fingerprint() string { return k.thumb }

// Same reports whether k and o verify the same signatures: the same public key
// (by thumbprint), or the same HMAC secret (compared in constant time, so
// no digest of a secret is ever kept to compare by). Config uses it to refuse
// two gates on one key: the gate's name is only a claim, so a shared key would
// let either gate sign as the other.
func (k Key) Same(o Key) bool {
	if k.alg != o.alg {
		return false
	}
	if k.alg == jose.HS256 {
		a, aok := k.verify.([]byte)
		b, bok := o.verify.([]byte)
		return aok && bok && subtle.ConstantTimeCompare(a, b) == 1
	}
	return k.thumb != "" && k.thumb == o.thumb
}

// Symmetric reports whether the key is a shared HMAC secret.
func (k Key) Symmetric() bool { return k.alg == jose.HS256 }

// ParsePublicJWK parses the operator's pinned key: an Ed25519 (kty OKP) or a
// P-256 (kty EC) PUBLIC JWK. A private key is refused — config holds only what
// verifies, and a private key there would let anyone who reads the config
// mint assertions.
func ParsePublicJWK(raw []byte) (Key, error) {
	if hasPrivatePart(raw) {
		return Key{}, errors.New("the JWK holds a private key (\"d\"); register only the public key")
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(raw); err != nil {
		return Key{}, fmt.Errorf("not a JWK: %w", err)
	}
	if !jwk.Valid() || !jwk.IsPublic() {
		return Key{}, errors.New("the JWK is not a valid public key")
	}
	var alg jose.SignatureAlgorithm
	switch pk := jwk.Key.(type) {
	case ed25519.PublicKey:
		alg = jose.EdDSA
	case *ecdsa.PublicKey:
		if pk.Curve != elliptic.P256() {
			return Key{}, errors.New("an EC key must be on P-256 (ES256)")
		}
		alg = jose.ES256
	default:
		return Key{}, errors.New("the key must be Ed25519 (kty OKP) or P-256 (kty EC)")
	}
	if jwk.Algorithm != "" && jwk.Algorithm != string(alg) {
		return Key{}, fmt.Errorf("the JWK says alg %q but the key is for %s", jwk.Algorithm, alg)
	}
	if jwk.Use != "" && jwk.Use != "sig" {
		return Key{}, fmt.Errorf("the JWK's use is %q, not sig", jwk.Use)
	}
	tp, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return Key{}, err
	}
	return Key{alg: alg, verify: jwk.Key, kid: jwk.KeyID, thumb: "sha256:" + base64.RawURLEncoding.EncodeToString(tp)}, nil
}

// hasPrivatePart reports whether a JWK object carries a private member. go-jose
// would parse such a key as a private key and verify with its public half;
// refusing it outright is clearer.
func hasPrivatePart(raw []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for _, k := range []string{"d", "p", "q", "dp", "dq", "qi", "k"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// HMACKey makes a key from a shared secret, for a gate that runs beside
// loomcycle and holds the same secret (from the operator's env). At least 32 bytes: HS256
// is only as strong as its secret.
func HMACKey(secret []byte) (Key, error) {
	if len(secret) < minHMACSecret {
		return Key{}, fmt.Errorf("an HMAC secret must be at least %d bytes, got %d", minHMACSecret, len(secret))
	}
	return Key{alg: jose.HS256, verify: append([]byte(nil), secret...), thumb: "hmac-sha256"}, nil
}

// JOSEAlgorithm is the one algorithm the key verifies, for jwt.ParseSigned.
func (k Key) JOSEAlgorithm() jose.SignatureAlgorithm { return k.alg }

// VerificationKey is what go-jose verifies a signature with: the public key,
// or the HMAC secret. nil for the zero Key.
func (k Key) VerificationKey() any { return k.verify }
