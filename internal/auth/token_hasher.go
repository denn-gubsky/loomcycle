package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PepperSource says where the operator-token pepper came from.
type PepperSource string

const (
	// PepperExplicit: LOOMCYCLE_OPERATOR_TOKEN_PEPPER is set.
	PepperExplicit PepperSource = "explicit"
	// PepperDerived: no explicit pepper; one is derived from
	// LOOMCYCLE_SECRET_KEY.
	PepperDerived PepperSource = "derived"
	// PepperNone: neither is set. A token hash is plain SHA-256(token), so
	// anyone who can write the token table can mint a working token.
	PepperNone PepperSource = "none"
)

// derivedTagVersion marks the stored form of a hash made with a DERIVED
// pepper: "p1:<pepper fingerprint>:<hex hash>".
//
// Why a tag at all (RFC ES #6): the hash is one-way, so without one nothing
// can tell a row hashed under the derived pepper from a legacy unpeppered
// row. The tag makes that knowable from the row alone, with no schema change:
// the boot snapshot of rows still on a legacy hash is exact (a row already
// rehashed is never a legacy candidate again, across restarts too), and the
// count of rows left to migrate is a fact rather than a guess. The fingerprint
// names WHICH key derived the pepper, so a row from before a
// LOOMCYCLE_SECRET_KEY rotation is recognisably not current either.
//
// The tag secures nothing on its own — a database writer can copy it. The
// pepper is what they cannot compute.
//
// An explicit pepper keeps the untagged form it has always had, so existing
// explicit-pepper deployments and their rows are unchanged.
const derivedTagVersion = "p1"

// TokenHasher turns a bearer into the token_hash stored for it, and names the
// older forms a row minted before the current pepper may still carry. It is a
// value: build one with NewTokenHasher and pass it where tokens are minted or
// looked up, so every site hashes the same way. The zero value hashes with no
// pepper (PepperNone).
type TokenHasher struct {
	source PepperSource
	pepper string
	tag    string
	legacy []TokenHasher
}

// NewTokenHasher picks the effective pepper: explicit if set, else derived
// (from the current secret key) if set, else none.
//
// Legacy candidates exist only for a derived pepper with legacyFallback on:
// the hash under the pepper derived from the PREVIOUS secret key (so a key
// rotation logs nobody out), then the unpeppered hash (rows minted before a
// pepper existed). An explicit pepper gets none: changing it has always
// invalidated every token, and a deployment that set one should not start
// accepting unpeppered rows.
func NewTokenHasher(explicit, derived, previousDerived string, legacyFallback bool) TokenHasher {
	switch {
	case explicit != "":
		return TokenHasher{source: PepperExplicit, pepper: explicit}
	case derived != "":
		h := TokenHasher{source: PepperDerived, pepper: derived, tag: derivedTag(derived)}
		if legacyFallback {
			if previousDerived != "" && previousDerived != derived {
				h.legacy = append(h.legacy, TokenHasher{source: PepperDerived, pepper: previousDerived, tag: derivedTag(previousDerived)})
			}
			h.legacy = append(h.legacy, TokenHasher{source: PepperNone})
		}
		return h
	default:
		return TokenHasher{source: PepperNone}
	}
}

// derivedTag is the stored-hash prefix for a derived pepper. The fingerprint
// is the first 8 bytes of SHA-256(pepper): it identifies the pepper without
// revealing it, like the key id the credential store keeps beside a sealed
// value.
func derivedTag(pepper string) string {
	sum := sha256.Sum256([]byte(pepper))
	return derivedTagVersion + ":" + hex.EncodeToString(sum[:8]) + ":"
}

// Source reports where the effective pepper came from.
func (h TokenHasher) Source() PepperSource {
	if h.source == "" {
		return PepperNone
	}
	return h.source
}

// Hash is the token_hash a token is stored and looked up under.
func (h TokenHasher) Hash(token string) string {
	return h.tag + HashToken(h.pepper, token)
}

// LegacyHashes are the older stored forms of token, tried on a lookup miss,
// in order. Empty unless the legacy fallback is on.
func (h TokenHasher) LegacyHashes(token string) []string {
	if len(h.legacy) == 0 {
		return nil
	}
	out := make([]string, 0, len(h.legacy))
	for _, l := range h.legacy {
		out = append(out, l.Hash(token))
	}
	return out
}

// LegacyFallback reports whether a lookup miss may try legacy hashes.
func (h TokenHasher) LegacyFallback() bool { return len(h.legacy) > 0 }

// IsCurrent reports whether a stored hash is in the current form. Only a
// derived pepper has a non-current form it can recognise; under an explicit
// pepper or none every stored hash is taken as current.
func (h TokenHasher) IsCurrent(stored string) bool {
	return h.source != PepperDerived || strings.HasPrefix(stored, h.tag)
}
