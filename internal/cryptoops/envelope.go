package cryptoops

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Envelope sources: which kind of credential holds the key that sealed a value,
// and for a crypto key which scope holds it. The scope is part of the source
// (and so of the AAD) so a decrypt opens the key of the scope that sealed,
// never a same-named key another scope holds.
const (
	SourceKeyTenant  = "kt" // a crypto-key credential in tenant scope (versioned)
	SourceKeyUser    = "ku" // a crypto-key credential in user scope
	SourceKeyAgent   = "ka" // a crypto-key credential in agent scope
	SourceCredential = "c"  // an ordinary credential, key derived from its value
	SourceRun        = "r"  // a per-run credential, key derived from its value
)

// KeySource is the envelope source of a crypto key held in scope ("tenant",
// "user" or "agent"); "" for any other scope.
func KeySource(scope string) string {
	switch scope {
	case "tenant":
		return SourceKeyTenant
	case "user":
		return SourceKeyUser
	case "agent":
		return SourceKeyAgent
	}
	return ""
}

// KeyScope is the scope a crypto-key source names; "" for a non-key source.
func KeyScope(source string) string {
	switch source {
	case SourceKeyTenant:
		return "tenant"
	case SourceKeyUser:
		return "user"
	case SourceKeyAgent:
		return "agent"
	}
	return ""
}

const envelopePrefix = "lcx1"

// maxVersionDigits bounds the version field so parsing can never overflow an
// int on any platform (9 digits < 2^31).
const maxVersionDigits = 9

var envelopeNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Envelope is the self-describing text form of a sealed value:
//
//	lcx1:<source>:<name>:<version>:<base64url-no-padding(nonce||ct||tag)>
//
// It names the key (not the key material) so a later decrypt can find the
// right key and version without the caller tracking it. The tool seals with
// AAD(tenant, Source, Name, Version, context), so editing the header to point
// at a different key or version makes Open fail.
type Envelope struct {
	Source  string
	Name    string
	Version int
	Body    []byte
}

// String renders the envelope. It does not validate; ParseEnvelope does.
func (e Envelope) String() string {
	return envelopePrefix + ":" + e.Source + ":" + e.Name + ":" + strconv.Itoa(e.Version) + ":" +
		base64.RawURLEncoding.EncodeToString(e.Body)
}

// ParseEnvelope parses s strictly: the exact prefix, a known source, a name
// matching ^[A-Za-z0-9_-]{1,128}$, a canonical non-negative decimal version
// (no sign, no leading zeros), and a non-empty unpadded base64url body. Being
// strict keeps there exactly one spelling of each envelope, so two strings
// that differ never decrypt to the same thing by accident.
func ParseEnvelope(s string) (Envelope, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 5 {
		return Envelope{}, errors.New("cryptoops: envelope must have exactly 5 colon-separated fields")
	}
	if parts[0] != envelopePrefix {
		return Envelope{}, errors.New("cryptoops: not an lcx1 envelope")
	}
	src := parts[1]
	if KeyScope(src) == "" && src != SourceCredential && src != SourceRun {
		return Envelope{}, errors.New("cryptoops: envelope source must be kt, ku, ka, c or r")
	}
	name := parts[2]
	if !envelopeNameRE.MatchString(name) {
		return Envelope{}, errors.New("cryptoops: envelope key name must match ^[A-Za-z0-9_-]{1,128}$")
	}
	ver, err := parseCanonicalUint(parts[3])
	if err != nil {
		return Envelope{}, err
	}
	if parts[4] == "" {
		return Envelope{}, errors.New("cryptoops: envelope body is empty")
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return Envelope{}, errors.New("cryptoops: envelope body is not unpadded base64url")
	}
	return Envelope{Source: src, Name: name, Version: ver, Body: body}, nil
}

func parseCanonicalUint(s string) (int, error) {
	bad := errors.New("cryptoops: envelope version must be a non-negative decimal integer")
	if s == "" || len(s) > maxVersionDigits || (len(s) > 1 && s[0] == '0') {
		return 0, bad
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, bad
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, bad
	}
	return n, nil
}
