package cryptoops

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Size rules for canonical material. HMAC keys may be longer than the minimum
// (RFC 2104 hashes over-long keys), but anything shorter than the hash output
// weakens the MAC, so we refuse it rather than silently accept a weak key.
const (
	aeadKeySize    = 32
	hmacMinKeySize = 32
	seedSize       = 32 // Ed25519 seed and P-256 scalar are both 32 bytes
	// RFC 4226 asks for >= 128-bit seeds, but many services still issue 80-bit
	// ones (16 base32 chars); 10 bytes is the floor that still enrolls them.
	totpMinSeed = 10
	totpGenSeed = 20 // RFC 4226's recommended 160 bits, matching HMAC-SHA1's output
)

// Generate creates fresh canonical material for k from crypto/rand.
func Generate(k KeyType) ([]byte, error) {
	switch k {
	case AES256GCM, XChaCha20Poly1305, HMACSHA256, Ed25519:
		return RandomBytes(32)
	case HMACSHA512:
		return RandomBytes(64)
	case TOTP:
		return RandomBytes(totpGenSeed)
	case ECDSAP256:
		// Not 32 random bytes: a uniformly random 32-byte string can exceed the
		// curve order, so let the stdlib draw a valid scalar.
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("cryptoops: generate %s: %w", k, err)
		}
		return priv.Bytes()
	case X25519:
		priv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("cryptoops: generate %s: %w", k, err)
		}
		return priv.Bytes(), nil
	}
	if bits := k.rsaBits(); bits != 0 {
		// Exactly bits long (GenerateKey sets the top bits) with e = 65537.
		priv, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			return nil, fmt.Errorf("cryptoops: generate %s: %w", k, err)
		}
		return marshalRSA(k, priv)
	}
	return nil, unknownType(k)
}

// ValidateMaterial checks that m has the canonical shape for k. It is a shape
// check, not a strength check: it cannot tell a random key from a predictable one.
func ValidateMaterial(k KeyType, m []byte) error {
	switch k {
	case AES256GCM, XChaCha20Poly1305:
		if len(m) != aeadKeySize {
			return fmt.Errorf("cryptoops: %s key must be exactly %d bytes, got %d", k, aeadKeySize, len(m))
		}
	case HMACSHA256, HMACSHA512:
		if len(m) < hmacMinKeySize {
			return fmt.Errorf("cryptoops: %s key must be at least %d bytes, got %d", k, hmacMinKeySize, len(m))
		}
	case Ed25519:
		if len(m) != ed25519.SeedSize {
			return fmt.Errorf("cryptoops: %s key must be the %d-byte seed, got %d bytes", k, ed25519.SeedSize, len(m))
		}
	case ECDSAP256:
		if len(m) != seedSize {
			return fmt.Errorf("cryptoops: %s key must be the %d-byte private scalar, got %d bytes", k, seedSize, len(m))
		}
		// Rejects zero and values >= the curve order.
		if _, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), m); err != nil {
			return fmt.Errorf("cryptoops: %s key is not a valid P-256 scalar", k)
		}
	case TOTP:
		if len(m) < totpMinSeed {
			return fmt.Errorf("cryptoops: %s seed must be at least %d bytes, got %d", k, totpMinSeed, len(m))
		}
	case X25519:
		if len(m) != x25519KeySize {
			return fmt.Errorf("cryptoops: %s key must be the %d-byte private scalar, got %d bytes", k, x25519KeySize, len(m))
		}
		if _, err := ecdh.X25519().NewPrivateKey(m); err != nil {
			return fmt.Errorf("cryptoops: %s key is not a valid X25519 private key", k)
		}
	default:
		if k.rsaBits() == 0 {
			return unknownType(k)
		}
		if _, err := parseRSAPrivate(k, m); err != nil {
			return err
		}
	}
	return nil
}

// ParseMaterial reads a stored or operator-supplied value into canonical
// material for k, then validates its shape.
//
// encoding is one of "" (auto: base64 for AEAD/HMAC, base32 for TOTP, PEM for
// the asymmetric types — the form each kind is usually handed around in),
// "raw", "base64", "hex", "base32" or "pem". Non-raw values are trimmed of
// surrounding whitespace, which copy-paste routinely adds.
//
// Canonical material is the 32-byte scalar for X25519 (from raw/base64/hex, or
// a PKCS#8 PEM) and PKCS#8 DER for RSA. An RSA key may arrive as a PKCS#8
// "PRIVATE KEY" or PKCS#1 "RSA PRIVATE KEY" PEM, or as base64/hex of either
// DER form; all are normalised to PKCS#8 DER.
func ParseMaterial(k KeyType, value string, encoding string) ([]byte, error) {
	if k.Ops() == nil {
		return nil, unknownType(k)
	}
	if encoding == "" {
		switch {
		case k.IsAEAD(), k.IsMAC():
			encoding = "base64"
		case k == TOTP:
			encoding = "base32"
		default:
			encoding = "pem"
		}
	}
	if encoding != "raw" {
		value = strings.TrimSpace(value)
	}
	var (
		m   []byte
		err error
	)
	switch encoding {
	case "raw":
		m = []byte(value)
	case "base64":
		m, err = decodeBase64Any(value)
	case "hex":
		m, err = hex.DecodeString(value)
	case "base32":
		m, err = decodeBase32Lenient(value)
	case "pem":
		m, err = parsePrivatePEM(k, value)
	default:
		return nil, fmt.Errorf("cryptoops: unknown encoding %q (valid: raw, base64, hex, base32, pem)", encoding)
	}
	if err == nil && encoding != "pem" && k.rsaBits() != 0 {
		m, err = normalizeRSADER(k, m)
	}
	if err != nil {
		// Never echo the value: it is secret material.
		return nil, fmt.Errorf("cryptoops: %s material is not valid %s: %w", k, encoding, err)
	}
	if err := ValidateMaterial(k, m); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeBase64Any accepts standard or URL alphabets, padded or not — operators
// paste both, and the alphabets don't overlap in a way that makes it ambiguous.
func decodeBase64Any(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.Strict().DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64 (standard or URL alphabet)")
}

// decodeBase32Lenient decodes a TOTP seed the way authenticator apps display
// it: grouped with spaces, any case, padding optional.
func decodeBase32Lenient(s string) ([]byte, error) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	s = strings.TrimRight(s, "=")
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, errors.New("not base32")
	}
	return b, nil
}

// parsePrivatePEM extracts canonical material from exactly one PEM private key
// block. Only the algorithm k implies is accepted, so an RSA or P-384 key can
// never be stored under an ecdsa-p256 name, nor a 2048-bit RSA key under a
// 3072-bit one.
func parsePrivatePEM(k KeyType, s string) ([]byte, error) {
	if !k.IsSigning() && !k.IsRecipient() {
		return nil, errors.New("PEM is only accepted for asymmetric key types")
	}
	block, rest := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("trailing data after the PEM block")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		if k != ECDSAP256 {
			return nil, fmt.Errorf("an EC PRIVATE KEY block is not a %s key", k)
		}
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		if k.rsaBits() == 0 {
			return nil, fmt.Errorf("an RSA PRIVATE KEY block is not a %s key", k)
		}
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q (want PRIVATE KEY, EC PRIVATE KEY or RSA PRIVATE KEY)", block.Type)
	}
	if err != nil {
		return nil, errors.New("PEM does not hold a parseable private key")
	}
	switch k {
	case Ed25519:
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM holds a %T, not an Ed25519 key", key)
		}
		return bytes.Clone(priv.Seed()), nil
	case ECDSAP256:
		priv, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM holds a %T, not an ECDSA key", key)
		}
		if priv.Curve != elliptic.P256() {
			return nil, fmt.Errorf("PEM holds an ECDSA key on %s, not P-256", priv.Curve.Params().Name)
		}
		return priv.Bytes()
	case X25519:
		priv, ok := key.(*ecdh.PrivateKey)
		if !ok || priv.Curve() != ecdh.X25519() {
			return nil, fmt.Errorf("PEM holds a %T, not an X25519 key", key)
		}
		return priv.Bytes(), nil
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PEM holds a %T, not an RSA key", key)
	}
	return marshalRSA(k, priv)
}

// deriveSalt is a fixed, public domain-separation salt. The secret input is the
// IKM; per-key separation comes from the info string, so a constant salt is the
// standard HKDF usage here.
var deriveSalt = []byte("loomcycle/crypto/derive/v1")

// Derive turns arbitrary secret bytes (e.g. an ordinary credential's value)
// into canonical material for an AEAD or HMAC type via HKDF-SHA256. The info
// string binds the output to both the type and name, so the same secret yields
// unrelated keys for different uses. Signing and TOTP types are refused: their
// material has structure (a valid scalar, an enrolled seed) that a KDF output
// would not meaningfully carry.
func Derive(k KeyType, secret []byte, name string) ([]byte, error) {
	var n int
	switch k {
	case AES256GCM, XChaCha20Poly1305, HMACSHA256:
		n = 32
	case HMACSHA512:
		n = 64
	default:
		return nil, fmt.Errorf("cryptoops: cannot derive %s material (only AEAD and HMAC types)", k)
	}
	if len(secret) == 0 {
		return nil, errors.New("cryptoops: derive needs a non-empty secret")
	}
	return hkdf.Key(sha256.New, secret, deriveSalt, "loomcycle/crypto/"+string(k)+"/"+name, n)
}

func unknownType(k KeyType) error {
	return fmt.Errorf("cryptoops: unsupported key type %q", string(k))
}
