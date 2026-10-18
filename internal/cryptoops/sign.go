package cryptoops

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
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
	if k.isRSAPSS() {
		priv, err := parseRSAPrivate(k, key)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		return rsa.SignPSS(rand.Reader, priv, crypto.SHA256, digest[:], pssOptions())
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
	if k.isRSAPSS() {
		pk, ok := pub.(*rsa.PublicKey)
		if !ok || !publicMatches(k, pk) || len(sig) != pk.Size() {
			return false
		}
		digest := sha256.Sum256(data)
		return rsa.VerifyPSS(pk, crypto.SHA256, digest[:], sig, pssOptions()) == nil
	}
	return false
}

// pssOptions fixes RSASSA-PSS to what JWS PS256 (RFC 7518 §3.5) specifies: a
// salt as long as the SHA-256 hash, and verification demands exactly that.
func pssOptions() *rsa.PSSOptions {
	return &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
}

// PublicKeyOf returns the public half of an asymmetric key: ed25519.PublicKey,
// *ecdsa.PublicKey, *ecdh.PublicKey (X25519) or *rsa.PublicKey. This is the
// only key-derived value safe to show a model.
func PublicKeyOf(k KeyType, key []byte) (crypto.PublicKey, error) {
	if !k.IsSigning() && !k.IsRecipient() {
		return nil, fmt.Errorf("cryptoops: %s has no public key", k)
	}
	if err := ValidateMaterial(k, key); err != nil {
		return nil, err
	}
	switch {
	case k == Ed25519:
		return ed25519.NewKeyFromSeed(key).Public(), nil
	case k == ECDSAP256:
		priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), key)
		if err != nil {
			return nil, err
		}
		return &priv.PublicKey, nil
	case k == X25519:
		priv, err := ecdh.X25519().NewPrivateKey(key)
		if err != nil {
			return nil, err
		}
		return priv.PublicKey(), nil
	}
	priv, err := parseRSAPrivate(k, key)
	if err != nil {
		return nil, err
	}
	return &priv.PublicKey, nil
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

// PublicKeyJWK encodes pub as a JWK (RFC 7517): OKP/Ed25519 or OKP/X25519
// (RFC 8037), EC/P-256, or RSA. kid is included only when non-empty. An RSA
// key's purpose is not in the key, so its JWK carries no "alg"; the caller
// that knows the type may add one.
func PublicKeyJWK(pub crypto.PublicKey, kid string) (map[string]any, error) {
	kt, err := checkPublic(pub)
	if err != nil {
		return nil, err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	var jwk map[string]any
	switch kt {
	case Ed25519:
		jwk = map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64(pub.(ed25519.PublicKey))}
	case X25519:
		jwk = map[string]any{"kty": "OKP", "crv": "X25519", "x": b64(pub.(*ecdh.PublicKey).Bytes())}
	case rsaFamily:
		pk := pub.(*rsa.PublicKey)
		jwk = map[string]any{"kty": "RSA", "n": b64(pk.N.Bytes()), "e": b64(big.NewInt(int64(pk.E)).Bytes())}
	default:
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

// ParsePublicKey reads a PKIX "PUBLIC KEY" PEM or a JWK JSON object holding a
// key that VERIFIES signatures, and returns the signing type it implies: an
// Ed25519 or P-256 key, or an RSA key as rsa-pss-<bits>. An X25519 key is
// refused (it cannot verify). A JWK "alg" or "use" member is ignored, since
// the type comes from kty/crv and the size.
func ParsePublicKey(s string) (KeyType, crypto.PublicKey, error) {
	kt, pub, err := parsePublicAny(s)
	if err != nil {
		return "", nil, err
	}
	switch kt {
	case X25519:
		return "", nil, errors.New("cryptoops: an X25519 key encrypts, it cannot verify a signature")
	case rsaFamily:
		return rsaPSSType(rsaPublicBits(pub.(*rsa.PublicKey))), pub, nil
	}
	return kt, pub, nil
}

// ParseRecipientKey reads a PEM or JWK public key to ENCRYPT to, and returns
// the recipient type it implies: x25519, or an RSA key as rsa-oaep-<bits>. A
// signature key (Ed25519, P-256) is refused.
func ParseRecipientKey(s string) (KeyType, crypto.PublicKey, error) {
	kt, pub, err := parsePublicAny(s)
	if err != nil {
		return "", nil, err
	}
	switch kt {
	case X25519:
		return X25519, pub, nil
	case rsaFamily:
		return rsaOAEPType(rsaPublicBits(pub.(*rsa.PublicKey))), pub, nil
	}
	return "", nil, fmt.Errorf("cryptoops: a %s key verifies signatures, it cannot be encrypted to (want X25519 or RSA)", kt)
}

// parsePublicAny reads any supported public key, RSA as rsaFamily.
func parsePublicAny(s string) (KeyType, crypto.PublicKey, error) {
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
		N   string `json:"n"`
		E   string `json:"e"`
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
	case j.Kty == "OKP" && j.Crv == "X25519":
		x, err := dec(j.X)
		if err != nil || len(x) != x25519KeySize {
			return "", nil, errors.New("cryptoops: X25519 JWK \"x\" must be 32 bytes of base64url")
		}
		pub, err := ecdh.X25519().NewPublicKey(x)
		if err != nil {
			return "", nil, errors.New("cryptoops: X25519 JWK \"x\" is not a valid public key")
		}
		return X25519, pub, nil
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
	case j.Kty == "RSA":
		n, errN := dec(j.N)
		e, errE := dec(j.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 || n[0] == 0 {
			return "", nil, errors.New("cryptoops: RSA JWK \"n\" and \"e\" must be canonical base64url integers")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if rsaPublicBits(pub) == 0 {
			return "", nil, errors.New("cryptoops: RSA JWK must be a 2048, 3072 or 4096-bit key with e=65537")
		}
		return rsaFamily, pub, nil
	}
	return "", nil, fmt.Errorf("cryptoops: unsupported JWK kty=%q crv=%q (want OKP/Ed25519, OKP/X25519, EC/P-256 or RSA)", j.Kty, j.Crv)
}
