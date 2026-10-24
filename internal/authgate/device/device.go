// Package device holds the pure logic of RFC FA's DeviceGate, the RFC 8628
// device authorization grant: the codes, their stored form, and the error
// vocabulary. It does no I/O; the HTTP server keeps the rows and the CLI
// polls.
//
// DeviceGate is not an identity source. A CLI or the MCP thin client asks for
// a code pair, shows the person the short user code, and polls with the long
// device code; the person signs in to the Web UI through any interactive gate,
// types the user code and confirms. The approval records who confirmed, and
// the poll that follows receives a token issued to that person.
package device

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
	"time"
)

// Lifetimes and pacing (RFC 8628 §3.2, §3.5).
const (
	// CodeTTL is how long a code pair can be approved and redeemed.
	CodeTTL = 10 * time.Minute
	// DefaultInterval is the minimum spacing between two polls of one device
	// code that the server asks for.
	DefaultInterval = 5 * time.Second
	// SlowDownStep is what every poll that comes too early adds to the
	// interval, for that poll and all later ones (RFC 8628 §3.5).
	SlowDownStep = 5 * time.Second
	// MaxInterval stops a misbehaving client from pushing its own interval
	// to a value that outlives the code.
	MaxInterval = 60 * time.Second
)

// GrantType is the token request's grant_type (RFC 8628 §3.4).
const GrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Error codes of the token endpoint (RFC 8628 §3.5, RFC 6749 §5.2).
const (
	ErrAuthorizationPending = "authorization_pending"
	ErrSlowDown             = "slow_down"
	ErrAccessDenied         = "access_denied"
	ErrExpiredToken         = "expired_token"
	ErrInvalidGrant         = "invalid_grant"
	ErrInvalidRequest       = "invalid_request"
	ErrInvalidClient        = "invalid_client"
	ErrInvalidScope         = "invalid_scope"
	ErrUnsupportedGrantType = "unsupported_grant_type"
)

// The device code: 32 CSPRNG bytes. It never leaves the polling client, and
// the server keeps only its SHA-256, so a database read yields no code that
// polls. 256 random bits make guessing one hopeless; a plain hash is enough.
const (
	DeviceCodePrefix = "lcd_"
	deviceCodeBytes  = 32
	maxDeviceCodeLen = 64
)

// NewDeviceCode returns a device code and the hash to store for it.
func NewDeviceCode() (code, hash string, err error) {
	var b [deviceCodeBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	code = DeviceCodePrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return code, HashDeviceCode(code), nil
}

// HashDeviceCode is the stored form of a device code.
func HashDeviceCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// WellFormedDeviceCode reports whether code has the shape NewDeviceCode
// produces, so junk costs the token endpoint no query.
func WellFormedDeviceCode(code string) bool {
	if len(code) > maxDeviceCodeLen || !strings.HasPrefix(code, DeviceCodePrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(code[len(DeviceCodePrefix):])
	return err == nil && len(raw) == deviceCodeBytes
}

// The user code is what a person reads off a terminal and types into a
// browser, so it is short, upper-case and drawn from letters nobody confuses:
// RFC 8628 §6.1's consonant alphabet (no vowels, so no words), without L,
// which reads as 1 or I in many terminal fonts. Eight of the 19 letters is
// 19^8 ≈ 1.7·10^10 (just under 34 bits). With a ten-minute life and the approval page's
// per-person and per-source attempt limits, a guesser gets a few dozen tries
// at a space of ten billion.
const (
	UserCodeAlphabet = "BCDFGHJKMNPQRSTVWXZ"
	UserCodeLen      = 8
)

// NewUserCode returns a fresh user code in its canonical form (no separator).
// Each letter is drawn uniformly (crypto/rand.Int rejection-samples).
func NewUserCode() (string, error) {
	n := big.NewInt(int64(len(UserCodeAlphabet)))
	var b strings.Builder
	for i := 0; i < UserCodeLen; i++ {
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b.WriteByte(UserCodeAlphabet[k.Int64()])
	}
	return b.String(), nil
}

// NormalizeUserCode turns what a person typed into the canonical code: case is
// ignored, and spaces, hyphens and other separators are dropped. It refuses
// (false) anything that is not exactly UserCodeLen letters of the alphabet
// after that, rather than guessing what an ambiguous character meant — a
// guess would make two codes collide.
func NormalizeUserCode(in string) (string, bool) {
	if len(in) > 4*UserCodeLen {
		return "", false
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '\t':
			continue
		case r < 128 && strings.IndexByte(UserCodeAlphabet, byte(r)) >= 0:
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	if b.Len() != UserCodeLen {
		return "", false
	}
	return b.String(), true
}

// FormatUserCode is how a code is shown: two groups of four, "BCDF-GHJK".
func FormatUserCode(canonical string) string {
	if len(canonical) != UserCodeLen {
		return canonical
	}
	return canonical[:4] + "-" + canonical[4:]
}

// UserCodeHashLabel domain-separates the user code's stored form. A user code
// is low-entropy, so the server stores a keyed hash of it (the token hasher,
// whose key is outside the database) rather than a plain digest a database
// reader could reverse by enumeration.
const UserCodeHashLabel = "loomcycle/device-user-code/v1:"

// NextInterval is the interval after a poll: unchanged when the poll came on
// time, one SlowDownStep longer (capped) when it came early.
func NextInterval(cur time.Duration, early bool) time.Duration {
	if cur <= 0 {
		cur = DefaultInterval
	}
	if !early {
		return cur
	}
	cur += SlowDownStep
	if cur > MaxInterval {
		cur = MaxInterval
	}
	return cur
}

// ClientID bounds what a client may call itself: it is shown to the person
// on the approval page, so it is a short printable label, not free text.
const maxClientIDLen = 64

// ValidClientID reports whether id is an acceptable client_id: 1–64 of
// letters, digits, '-', '_', '.' and ':'.
func ValidClientID(id string) bool {
	if id == "" || len(id) > maxClientIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}
