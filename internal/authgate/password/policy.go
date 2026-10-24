package password

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// builtinBreached is SecLists' Passwords/Common-Credentials/10k-most-common.txt
// (MIT, Daniel Miessler; see breached/LICENSE and NOTICE), the "top-10k" the
// RFC asks every deployment to refuse whatever its own list says.
//
//go:embed breached/10k-most-common.txt
var builtinBreached string

// NIST SP 800-63B-4 lengths: 15 when the password is the only factor, 8 when
// a second factor is required.
const (
	MinLengthSoleFactor = 15
	MinLengthWithMFA    = 8
	// MaxLength is in characters; MaxPasswordBytes bounds the encoding.
	MaxLength = 256
)

// Policy refusals. Each is safe to show the person choosing the password; none
// echoes the password.
var (
	ErrTooShort   = errors.New("password: too short")
	ErrTooLong    = errors.New("password: too long")
	ErrBreached   = errors.New("password: appears in a list of breached or common passwords")
	ErrContextual = errors.New("password: must not be your login or e-mail")
	ErrInvalidUTF = errors.New("password: not valid text")
)

// PolicyError carries the minimum length with ErrTooShort for the UI.
type PolicyError struct {
	Err       error
	MinLength int
}

func (e *PolicyError) Error() string {
	if errors.Is(e.Err, ErrTooShort) {
		return fmt.Sprintf("%v: use at least %d characters", e.Err, e.MinLength)
	}
	return e.Err.Error()
}

func (e *PolicyError) Unwrap() error { return e.Err }

// Policy is the password policy (RFC FA §6): length only, no composition
// rules, no expiry, a breached-list check, paste allowed (nothing here could
// tell), Unicode normalised to NFKC so the same password typed on two keyboards
// is one password.
type Policy struct {
	denied map[string]struct{}
}

// NewPolicy builds the policy over the built-in list plus extra, an operator's
// denylist (one password per line, `#` comments; nil for none). Entries are
// compared case-insensitively after NFKC, so "Password1" in the list refuses
// "password1" too.
func NewPolicy(extra io.Reader) (*Policy, error) {
	p := &Policy{denied: make(map[string]struct{}, 10240)}
	if err := p.add(strings.NewReader(builtinBreached)); err != nil {
		return nil, err
	}
	if extra != nil {
		if err := p.add(extra); err != nil {
			return nil, fmt.Errorf("password denylist: %w", err)
		}
	}
	return p, nil
}

func (p *Policy) add(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxPasswordBytes*4)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p.denied[denyKey(line)] = struct{}{}
	}
	return sc.Err()
}

// Size is how many distinct entries the policy refuses (for a boot log line —
// never the entries themselves).
func (p *Policy) Size() int { return len(p.denied) }

func denyKey(s string) string { return cases.Fold().String(norm.NFKC.String(s)) }

// Normalize returns the form a password is checked and hashed in: NFKC.
// Nothing else is changed — no trimming: a space is a character the person
// typed.
func Normalize(pw string) (string, error) {
	if !utf8.ValidString(pw) {
		return "", ErrInvalidUTF
	}
	return norm.NFKC.String(pw), nil
}

// Check applies the policy to a normalised password. withMFA lowers the
// minimum length; contextual are values the password must not equal (the
// login and the e-mail), compared case-insensitively.
func (p *Policy) Check(pw string, withMFA bool, contextual ...string) error {
	min := MinLengthSoleFactor
	if withMFA {
		min = MinLengthWithMFA
	}
	n := utf8.RuneCountInString(pw)
	switch {
	case len(pw) > MaxPasswordBytes || n > MaxLength:
		return &PolicyError{Err: ErrTooLong}
	case n < min:
		return &PolicyError{Err: ErrTooShort, MinLength: min}
	}
	key := denyKey(pw)
	if _, bad := p.denied[key]; bad {
		return &PolicyError{Err: ErrBreached}
	}
	for _, c := range contextual {
		if c != "" && denyKey(c) == key {
			return &PolicyError{Err: ErrContextual}
		}
	}
	return nil
}
