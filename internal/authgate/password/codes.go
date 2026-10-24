package password

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Invite and reset codes (RFC FA §6.1): 32 CSPRNG bytes, shown once in the
// link and stored only as their SHA-256, so a database read yields no usable
// link. A plain hash is enough here: 256 random bits cannot be guessed from
// it, unlike a password.
const (
	InvitePrefix = "lci_"
	ResetPrefix  = "lcr_"
	codeBytes    = 32
	// maxCodeLen bounds what a redemption endpoint hashes and looks up.
	maxCodeLen = 64
)

// NewCode returns a code with prefix and the hash to store for it.
func NewCode(prefix string) (code, hash string, err error) {
	var b [codeBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	code = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return code, HashCode(code), nil
}

// HashCode is the stored form of a code.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// WellFormedCode reports whether code has prefix and the shape NewCode
// produces. A redemption endpoint checks it before touching the store, so junk
// costs no query.
func WellFormedCode(prefix, code string) bool {
	if len(code) > maxCodeLen || !strings.HasPrefix(code, prefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(code[len(prefix):])
	return err == nil && len(raw) == codeBytes
}
