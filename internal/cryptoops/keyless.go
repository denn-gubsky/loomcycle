package cryptoops

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // TOTP is defined over HMAC-SHA1 (RFC 6238); HMAC-SHA1 is not broken by SHA-1 collisions
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// TOTPCode computes the RFC 6238 time-based one-time password for seed at t,
// using HMAC-SHA1 — the RFC default and the only algorithm authenticator apps
// reliably support. digits is 6..8 (0 means 6); period is in seconds (0 means
// 30). validUntil is when this code's time step ends.
func TOTPCode(seed []byte, t time.Time, digits, period int) (code string, validUntil time.Time, err error) {
	if digits == 0 {
		digits = 6
	}
	if period == 0 {
		period = 30
	}
	if digits < 6 || digits > 8 {
		return "", time.Time{}, fmt.Errorf("cryptoops: totp digits must be 6..8, got %d", digits)
	}
	if period < 1 {
		return "", time.Time{}, fmt.Errorf("cryptoops: totp period must be positive, got %d", period)
	}
	if err := ValidateMaterial(TOTP, seed); err != nil {
		return "", time.Time{}, err
	}
	unix := t.Unix()
	if unix < 0 {
		return "", time.Time{}, errors.New("cryptoops: totp time is before the Unix epoch")
	}
	counter := uint64(unix) / uint64(period)

	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, seed)
	m.Write(msg[:])
	sum := m.Sum(nil)
	// RFC 4226 §5.3 dynamic truncation.
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	code = fmt.Sprintf("%0*d", digits, bin%mod)
	validUntil = time.Unix(int64((counter+1)*uint64(period)), 0).UTC()
	return code, validUntil, nil
}

// Hash computes a keyless digest. alg is one of sha256, sha512, sha3-256,
// sha3-512; MD5 and SHA-1 are deliberately absent so an agent can't pick a
// broken hash for an integrity check.
func Hash(alg string, data []byte) ([]byte, error) {
	switch alg {
	case "sha256":
		s := sha256.Sum256(data)
		return s[:], nil
	case "sha512":
		s := sha512.Sum512(data)
		return s[:], nil
	case "sha3-256":
		s := sha3.Sum256(data)
		return s[:], nil
	case "sha3-512":
		s := sha3.Sum512(data)
		return s[:], nil
	}
	return nil, fmt.Errorf("cryptoops: unknown hash %q (valid: sha256, sha512, sha3-256, sha3-512)", alg)
}

// maxRandom caps one request so a model can't ask for an unbounded buffer.
const maxRandom = 4096

// RandomBytes returns n bytes from crypto/rand, 1 <= n <= 4096.
func RandomBytes(n int) ([]byte, error) {
	if n < 1 || n > maxRandom {
		return nil, fmt.Errorf("cryptoops: random length must be 1..%d, got %d", maxRandom, n)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// RandomToken returns n random bytes (1..4096) as unpadded base64url — safe in
// URLs, headers and file names without escaping.
func RandomToken(n int) (string, error) {
	b, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// UUIDv4 returns a random RFC 9562 version-4 UUID in canonical lowercase form.
func UUIDv4() (string, error) {
	b, err := RandomBytes(16)
	if err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
