package awsauth

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// SigV4Authorization is a parsed SigV4 Authorization header: what a request
// claims about how it was signed. Parsing proves nothing — only the service
// that holds the secret key can check Signature — but it lets a server that
// forwards a client's signed request (the AWS IAM gate) judge what was signed
// before sending it.
type SigV4Authorization struct {
	AccessKeyID   string
	Day           string // the credential scope's date, YYYYMMDD
	Region        string
	Service       string
	SignedHeaders []string // lower-case, as listed
	Signature     string   // lower-case hex
}

// ParseAuthorization reads "AWS4-HMAC-SHA256 Credential=<key>/<day>/<region>/
// <service>/aws4_request, SignedHeaders=<a;b>, Signature=<hex>". Each part
// must appear exactly once.
func ParseAuthorization(h string) (SigV4Authorization, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(h), algorithm+" ")
	if !ok {
		return SigV4Authorization{}, errors.New("sigv4: not an " + algorithm + " authorization")
	}
	parts := map[string]string{}
	for _, p := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || v == "" {
			return SigV4Authorization{}, errors.New("sigv4: malformed authorization")
		}
		if _, dup := parts[k]; dup {
			return SigV4Authorization{}, errors.New("sigv4: repeated authorization part")
		}
		parts[k] = v
	}
	if len(parts) != 3 || parts["Credential"] == "" || parts["SignedHeaders"] == "" || parts["Signature"] == "" {
		return SigV4Authorization{}, errors.New("sigv4: authorization needs exactly Credential, SignedHeaders and Signature")
	}
	scope := strings.Split(parts["Credential"], "/")
	if len(scope) != 5 || scope[0] == "" || len(scope[1]) != len(dayFormat) || scope[2] == "" || scope[3] == "" || scope[4] != "aws4_request" {
		return SigV4Authorization{}, errors.New("sigv4: malformed credential scope")
	}
	sig := parts["Signature"]
	if _, err := hex.DecodeString(sig); err != nil || len(sig) != 64 {
		return SigV4Authorization{}, errors.New("sigv4: malformed signature")
	}
	signed := strings.Split(parts["SignedHeaders"], ";")
	for _, s := range signed {
		if s == "" || s != strings.ToLower(s) {
			return SigV4Authorization{}, errors.New("sigv4: malformed signed headers")
		}
	}
	return SigV4Authorization{
		AccessKeyID: scope[0], Day: scope[1], Region: scope[2], Service: scope[3],
		SignedHeaders: signed, Signature: strings.ToLower(sig),
	}, nil
}

// Signs reports whether name (any case) is among the signed headers.
func (a SigV4Authorization) Signs(name string) bool {
	name = strings.ToLower(name)
	for _, s := range a.SignedHeaders {
		if s == name {
			return true
		}
	}
	return false
}

// ParseAmzDate reads an X-Amz-Date value.
func ParseAmzDate(v string) (time.Time, error) {
	return time.Parse(amzDateFormat, strings.TrimSpace(v))
}
