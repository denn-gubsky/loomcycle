package auth

import (
	"crypto/hmac"
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

// The stored forms of a hash made with a DERIVED pepper carry a tag:
//
//	p1:<pepper fingerprint>:<hex HMAC-SHA256(pepper, token)>
//	w1:<pepper fingerprint>:<hex HMAC-SHA256(pepper, "w1" 0x00 unpeppered hash)>
//
// p1 is what every mint writes. w1 is a row minted before any pepper existed
// (its token_hash the unpeppered hex SHA-256(token), HashToken("", token)),
// wrapped by `loomcycle tokens rewrap` without its token: the MAC is over that 64-character hex
// text, exactly as it was stored (RFC ES F3). A lookup recomputes it from the
// presented token. Either form needs the pepper to compute, so a database
// writer can forge neither.
//
// The w1 message is domain-separated ("w1" and a NUL before the hash) so the
// two forms never share a MAC: without it, presenting a token's unpeppered
// hash AS a bearer would give a p1 MAC equal to that token's w1 MAC, and only
// the differing prefix would keep the two apart.
//
// Why a tag at all (RFC ES #6): the hash is one-way, so without one nothing
// can tell a row hashed under the derived pepper from a legacy unpeppered
// row. The tag makes that knowable from the row alone, with no schema
// change. The fingerprint (first 8 bytes of SHA-256(pepper), identifying it
// without revealing it, like the key id the credential store keeps beside a
// sealed value) names WHICH key derived the pepper, so a row from before a
// LOOMCYCLE_SECRET_KEY rotation is recognisably not current either.
//
// The tag secures nothing on its own — a database writer can copy it. The
// pepper is what they cannot compute.
//
// An explicit pepper keeps the untagged form it has always had, so existing
// explicit-pepper deployments and their rows are unchanged.
const (
	currentTagVersion = "p1"
	wrappedTagVersion = "w1"
)

// TokenHashForm is what a stored token_hash is, read against a hasher.
type TokenHashForm int

const (
	// FormCurrent is the form Hash produces. Under an explicit pepper or
	// none, every stored hash reads as current.
	FormCurrent TokenHashForm = iota
	// FormWrapped is an unpeppered hash wrapped under the current pepper.
	FormWrapped
	// FormPrevious is either tagged form under the pepper
	// LOOMCYCLE_SECRET_KEY_PREVIOUS derives.
	FormPrevious
	// FormOtherKey is a tagged form under a pepper neither key derives. It
	// can no longer authenticate.
	FormOtherKey
	// FormUntagged, under a derived pepper, is an unpeppered hash (or one
	// under an explicit pepper since removed).
	FormUntagged
)

// derivedKey is one derived pepper and its fingerprint.
type derivedKey struct {
	pepper string
	fp     string
}

func newDerivedKey(pepper string) derivedKey {
	sum := sha256.Sum256([]byte(pepper))
	return derivedKey{pepper: pepper, fp: hex.EncodeToString(sum[:8])}
}

func (k derivedKey) tagged(version, msg string) string {
	mac := hmac.New(sha256.New, []byte(k.pepper))
	mac.Write([]byte(msg))
	return version + ":" + k.fp + ":" + hex.EncodeToString(mac.Sum(nil))
}

func (k derivedKey) hash(token string) string { return k.tagged(currentTagVersion, token) }

func (k derivedKey) wrap(unpeppered string) string {
	return k.tagged(wrappedTagVersion, wrappedTagVersion+"\x00"+unpeppered)
}

func (k derivedKey) has(stored, version string) bool {
	return strings.HasPrefix(stored, version+":"+k.fp+":")
}

// TokenHasher turns a bearer into the token_hash stored for it, and names the
// older forms a row may still carry under a derived pepper. It is a value:
// build one with NewTokenHasher and pass it where tokens are minted or looked
// up, so every site hashes the same way. The zero value hashes with no pepper
// (PepperNone).
type TokenHasher struct {
	source   PepperSource
	explicit string
	// cur and prev are set only for a derived pepper; prev.pepper is empty
	// when there is no previous key.
	cur, prev derivedKey
}

// NewTokenHasher picks the effective pepper: explicit if set, else derived
// (from the current secret key) if set, else none.
//
// Only a derived pepper has older forms: the wrapped form of a row minted
// before any pepper existed, and both forms under the pepper the PREVIOUS
// secret key derives, so a key rotation logs nobody out. An explicit pepper
// has none: changing it has always invalidated every token.
func NewTokenHasher(explicit, derived, previousDerived string) TokenHasher {
	switch {
	case explicit != "":
		return TokenHasher{source: PepperExplicit, explicit: explicit}
	case derived != "":
		h := TokenHasher{source: PepperDerived, cur: newDerivedKey(derived)}
		if previousDerived != "" && previousDerived != derived {
			h.prev = newDerivedKey(previousDerived)
		}
		return h
	default:
		return TokenHasher{source: PepperNone}
	}
}

// Source reports where the effective pepper came from.
func (h TokenHasher) Source() PepperSource {
	if h.source == "" {
		return PepperNone
	}
	return h.source
}

func (h TokenHasher) derived() bool { return h.source == PepperDerived }

// Hash is the token_hash a token is minted under and first looked up by.
func (h TokenHasher) Hash(token string) string {
	if h.derived() {
		return h.cur.hash(token)
	}
	return HashToken(h.explicit, token)
}

// Wrap is the wrapped form of a stored unpeppered hash under the current
// pepper — what the rewrap command writes. Empty unless the pepper is derived.
func (h TokenHasher) Wrap(unpeppered string) string {
	if !h.derived() {
		return ""
	}
	return h.cur.wrap(unpeppered)
}

// WrappedHash is the wrapped form token's row carries if it was rewrapped
// under the current pepper. Empty unless the pepper is derived.
func (h TokenHasher) WrappedHash(token string) string {
	return h.Wrap(HashToken("", token))
}

// HasPrevious reports whether a previous secret key's pepper is configured.
func (h TokenHasher) HasPrevious() bool { return h.derived() && h.prev.pepper != "" }

// PreviousHashes are the forms token's row may carry under the previous
// secret key's pepper: minted there, then rewrapped there. Nil without one.
func (h TokenHasher) PreviousHashes(token string) []string {
	if !h.HasPrevious() {
		return nil
	}
	return []string{h.prev.hash(token), h.prev.wrap(HashToken("", token))}
}

// UsablePrefixes describes, as prefixes, the stored hashes a lookup through
// this hasher can match: under a derived pepper, either tagged form under the
// current or previous key (include); otherwise any untagged hash (exclude
// the tagged forms). A stored hash outside them cannot authenticate.
func (h TokenHasher) UsablePrefixes() (include, exclude []string) {
	if !h.derived() {
		return nil, []string{currentTagVersion + ":", wrappedTagVersion + ":"}
	}
	keys := []derivedKey{h.cur}
	if h.HasPrevious() {
		keys = append(keys, h.prev)
	}
	for _, k := range keys {
		for _, v := range []string{currentTagVersion, wrappedTagVersion} {
			include = append(include, v+":"+k.fp+":")
		}
	}
	return include, nil
}

// Form reports what a stored hash is, read against this hasher.
func (h TokenHasher) Form(stored string) TokenHashForm {
	switch {
	case !h.derived(), h.cur.has(stored, currentTagVersion):
		return FormCurrent
	case h.cur.has(stored, wrappedTagVersion):
		return FormWrapped
	case h.HasPrevious() && (h.prev.has(stored, currentTagVersion) || h.prev.has(stored, wrappedTagVersion)):
		return FormPrevious
	case strings.HasPrefix(stored, currentTagVersion+":"), strings.HasPrefix(stored, wrappedTagVersion+":"):
		return FormOtherKey
	default:
		return FormUntagged
	}
}
