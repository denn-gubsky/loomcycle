package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// RFC L OSS multi-tenant authorization — token minting + hashing.
//
// A token is a 256-bit CSPRNG secret, base58-encoded, with a fixed
// `lct_` prefix. The hash stored in operator_token_defs is
// SHA-256(pepper ‖ token), hex — indexable for a single-lookup auth hot
// path, compared with the existing constant-time primitive. argon2id is
// deliberately NOT used: a high-entropy random token needs no slow KDF,
// and a salted hash can't be indexed. The pepper (an env-allowlisted
// var) means a stolen DB dump without the pepper yields no usable
// lookup. See rfcs/oss-multi-tenant-authorization.md Decision 2/3.

// TokenPrefix is the fixed, grep-friendly prefix on every minted token.
const TokenPrefix = "lct_"

// TokenSuffixLen is how many leading characters of the encoded body are
// retained (as token_suffix) for log correlation. Never the secret.
const TokenSuffixLen = 6

// HashToken returns hex(SHA-256(pepper ‖ token)). The same function is
// used at mint time (to store) and at auth time (to look up), so the two
// can never drift. An empty pepper is allowed (single-binary dev) but an
// operator running multi-tenant should set one.
func HashToken(pepper, token string) string {
	h := sha256.New()
	h.Write([]byte(pepper))
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

// MintToken generates a fresh token. Returns the full plaintext (shown
// to the operator exactly once, never persisted) and its 6-char suffix
// (safe to store/log for correlation). The substrate is the only minting
// authority — operator-supplied tokens are refused upstream.
func MintToken() (plaintext, suffix string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("mint token: %w", err)
	}
	body := base58Encode(raw[:])
	if len(body) < TokenSuffixLen {
		// Astronomically unlikely (32 random bytes encode to ~43-44
		// base58 chars), but never index out of range.
		return "", "", fmt.Errorf("mint token: encoded body too short")
	}
	return TokenPrefix + body, body[:TokenSuffixLen], nil
}

// base58Alphabet is the Bitcoin alphabet (no 0/O/I/l ambiguity).
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Encode encodes bytes with the Bitcoin base58 alphabet. A tiny
// stdlib-only big-integer-free implementation (the standard
// repeated-division method) — preferred over a dependency for ~25 lines.
func base58Encode(input []byte) string {
	// Count leading zero bytes → leading '1's.
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}
	// Allocate enough space: log(256)/log(58) ≈ 1.37 chars per byte.
	size := (len(input)-zeros)*138/100 + 1
	buf := make([]byte, size)
	high := size - 1
	for i := zeros; i < len(input); i++ {
		carry := int(input[i])
		j := size - 1
		for ; j > high || carry != 0; j-- {
			carry += 256 * int(buf[j])
			buf[j] = byte(carry % 58)
			carry /= 58
		}
		high = j
	}
	// Skip leading zeros in buf.
	k := 0
	for k < size && buf[k] == 0 {
		k++
	}
	out := make([]byte, 0, zeros+(size-k))
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}
	for ; k < size; k++ {
		out = append(out, base58Alphabet[buf[k]])
	}
	return string(out)
}
