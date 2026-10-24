package password

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Login names are unique per gate across every tenant (D18), so two that a
// person would read as the same must be the same string: NFKC, then
// full Unicode case folding, then NFKC again (folding can leave a string
// unnormalised).
const (
	MinLoginLength = 2
	MaxLoginBytes  = 254 // an e-mail address is at most 254 octets
)

// ErrInvalidLogin: empty, too long, or holding whitespace, control or
// invisible formatting characters.
var ErrInvalidLogin = errors.New("login must be 2 to 254 characters with no spaces or control characters")

// NormalizeLogin returns the canonical form of a login name, or
// ErrInvalidLogin. E-mail-shaped names are allowed; nothing about the
// characters beyond printability is assumed.
func NormalizeLogin(login string) (string, error) {
	if !utf8.ValidString(login) {
		return "", ErrInvalidLogin
	}
	s := norm.NFKC.String(cases.Fold().String(norm.NFKC.String(strings.TrimSpace(login))))
	if len(s) > MaxLoginBytes || utf8.RuneCountInString(s) < MinLoginLength {
		return "", ErrInvalidLogin
	}
	for _, r := range s {
		// IsGraphic excludes controls and the format characters (zero-width
		// joiners, bidi overrides) that make two logins look alike.
		if unicode.IsSpace(r) || !unicode.IsGraphic(r) {
			return "", ErrInvalidLogin
		}
	}
	return s, nil
}

var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewSubject mints the subject of a user the gate creates: `<gate>:<random>`
// (D5). The login is an attribute of the user, not its key, so a rename never
// moves the user's data. 80 random bits.
func NewSubject(gate string) (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return gate + ":" + lowerBase32.EncodeToString(b[:]), nil
}

// NewID returns a random identifier for an invite or a reset row (not a
// secret: the code is what grants).
func NewID(prefix string) (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + lowerBase32.EncodeToString(b[:]), nil
}
