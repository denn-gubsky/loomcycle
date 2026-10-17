package cryptoops

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
)

// p256SigSize is the IEEE P1363 r||s form JWS ES256 uses: two 32-byte
// big-endian integers. Fixed width (unlike ASN.1 DER) so consumers can split
// it without parsing.
const p256SigSize = 64

func macHash(k KeyType) (func() hash.Hash, error) {
	switch k {
	case HMACSHA256:
		return sha256.New, nil
	case HMACSHA512:
		return sha512.New, nil
	}
	return nil, fmt.Errorf("cryptoops: %s is not a MAC key type", k)
}

// MAC computes HMAC over data with key, after checking the key's shape.
func MAC(k KeyType, key, data []byte) ([]byte, error) {
	if err := ValidateMaterial(k, key); err != nil {
		return nil, err
	}
	return macRaw(k, key, data)
}

// macRaw is MAC without the minimum-length check, so tests can run the RFC
// 4231 vectors (whose keys are shorter than this package accepts).
func macRaw(k KeyType, key, data []byte) ([]byte, error) {
	h, err := macHash(k)
	if err != nil {
		return nil, err
	}
	m := hmac.New(h, key)
	m.Write(data)
	return m.Sum(nil), nil
}

// VerifyMAC reports whether mac is the HMAC of data under key. The comparison
// is constant-time (hmac.Equal) so timing doesn't reveal how many leading
// bytes of a forged MAC were right. Any error is a plain false.
func VerifyMAC(k KeyType, key, data, mac []byte) bool {
	want, err := MAC(k, key, data)
	if err != nil {
		return false
	}
	return hmac.Equal(want, mac)
}

// Sign produces a raw signature: 64 bytes for Ed25519 (RFC 8032, over data
// itself) and a 64-byte r||s for ECDSA P-256 over SHA-256(data). MAC types are
// refused here — they sign only through SignJWS, where the alg is pinned.
func Sign(k KeyType, key, data []byte) ([]byte, error) {
	if !k.IsSigning() {
		return nil, fmt.Errorf("cryptoops: %s cannot produce a raw signature", k)
	}
	if err := ValidateMaterial(k, key); err != nil {
		return nil, err
	}
	if k == Ed25519 {
		return ed25519.Sign(ed25519.NewKeyFromSeed(key), data), nil
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), key)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, p256SigSize)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, nil
}

// Verify checks a raw signature from Sign against pub. pub must be the key
// kind k implies (ed25519.PublicKey or a P-256 *ecdsa.PublicKey); a mismatch
// is false, never a cross-algorithm attempt.
func Verify(k KeyType, pub crypto.PublicKey, data, sig []byte) bool {
	switch k {
	case Ed25519:
		pk, ok := pub.(ed25519.PublicKey)
		if !ok || len(pk) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
			return false
		}
		return ed25519.Verify(pk, data, sig)
	case ECDSAP256:
		pk, ok := pub.(*ecdsa.PublicKey)
		if !ok || pk == nil || pk.Curve != elliptic.P256() || len(sig) != p256SigSize {
			return false
		}
		digest := sha256.Sum256(data)
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pk, digest[:], r, s)
	}
	return false
}

// PublicKeyOf returns the public half of a signing key: ed25519.PublicKey or
// *ecdsa.PublicKey. This is the only key-derived value safe to show a model.
func PublicKeyOf(k KeyType, key []byte) (crypto.PublicKey, error) {
	if !k.IsSigning() {
		return nil, fmt.Errorf("cryptoops: %s has no public key", k)
	}
	if err := ValidateMaterial(k, key); err != nil {
		return nil, err
	}
	if k == Ed25519 {
		return ed25519.NewKeyFromSeed(key).Public(), nil
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), key)
	if err != nil {
		return nil, err
	}
	return &priv.PublicKey, nil
}

// checkPublic narrows pub to a supported kind and reports its KeyType.
func checkPublic(pub crypto.PublicKey) (KeyType, error) {
	switch pk := pub.(type) {
	case ed25519.PublicKey:
		if len(pk) == ed25519.PublicKeySize {
			return Ed25519, nil
		}
	case *ecdsa.PublicKey:
		if pk != nil && pk.Curve == elliptic.P256() {
			return ECDSAP256, nil
		}
	}
	return "", fmt.Errorf("cryptoops: unsupported public key %T (want Ed25519 or ECDSA P-256)", pub)
}

// PublicKeyPEM encodes pub as a PKIX "PUBLIC KEY" PEM block.
func PublicKeyPEM(pub crypto.PublicKey) (string, error) {
	if _, err := checkPublic(pub); err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// PublicKeyJWK encodes pub as a JWK (RFC 7517): OKP/Ed25519 (RFC 8037) or
// EC/P-256. kid is included only when non-empty.
func PublicKeyJWK(pub crypto.PublicKey, kid string) (map[string]any, error) {
	kt, err := checkPublic(pub)
	if err != nil {
		return nil, err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	var jwk map[string]any
	if kt == Ed25519 {
		jwk = map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64(pub.(ed25519.PublicKey))}
	} else {
		raw, err := pub.(*ecdsa.PublicKey).Bytes() // 0x04 || X(32) || Y(32)
		if err != nil {
			return nil, err
		}
		jwk = map[string]any{"kty": "EC", "crv": "P-256", "x": b64(raw[1:33]), "y": b64(raw[33:65])}
	}
	if kid != "" {
		jwk["kid"] = kid
	}
	return jwk, nil
}

// ParsePublicKey reads a PKIX "PUBLIC KEY" PEM or a JWK JSON object and
// returns the key type it implies. Only Ed25519 and P-256 are accepted; a JWK
// "alg" or "use" member is ignored, since the type comes from kty/crv.
func ParsePublicKey(s string) (KeyType, crypto.PublicKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") {
		return parseJWK(s)
	}
	block, rest := pem.Decode([]byte(s))
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return "", nil, errors.New("cryptoops: public key must be a single PKIX PUBLIC KEY PEM block or a JWK object")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", nil, errors.New("cryptoops: PEM does not hold a parseable public key")
	}
	kt, err := checkPublic(pub)
	if err != nil {
		return "", nil, err
	}
	return kt, pub, nil
}

func parseJWK(s string) (KeyType, crypto.PublicKey, error) {
	var j struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		D   string `json:"d"`
	}
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return "", nil, errors.New("cryptoops: JWK is not a valid JSON object")
	}
	if j.D != "" {
		// A private JWK pasted where a public one belongs: refuse rather than
		// quietly accept (and possibly persist) private material.
		return "", nil, errors.New("cryptoops: JWK contains a private key (\"d\"); supply the public key only")
	}
	dec := base64.RawURLEncoding.Strict().DecodeString
	switch {
	case j.Kty == "OKP" && j.Crv == "Ed25519":
		x, err := dec(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return "", nil, errors.New("cryptoops: Ed25519 JWK \"x\" must be 32 bytes of base64url")
		}
		return Ed25519, ed25519.PublicKey(x), nil
	case j.Kty == "EC" && j.Crv == "P-256":
		x, errX := dec(j.X)
		y, errY := dec(j.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			return "", nil, errors.New("cryptoops: P-256 JWK \"x\" and \"y\" must each be 32 bytes of base64url")
		}
		raw := append(append([]byte{4}, x...), y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return "", nil, errors.New("cryptoops: P-256 JWK point is not on the curve")
		}
		return ECDSAP256, pub, nil
	}
	return "", nil, fmt.Errorf("cryptoops: unsupported JWK kty=%q crv=%q (want OKP/Ed25519 or EC/P-256)", j.Kty, j.Crv)
}
