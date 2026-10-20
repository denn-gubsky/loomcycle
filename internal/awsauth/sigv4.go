// Package awsauth authenticates outgoing requests to AWS: Signature Version 4
// signing (header and presigned-URL forms), the standard AWS credential chain,
// and the short-term Bedrock API key minted from those credentials (RFC EM).
//
// The signer is loomcycle's own rather than the SDK's: it is small, it is
// tested against AWS's published vectors and against the SDK's signer, and it
// was already in use for AWS KMS before providers needed it. The credential
// chain is the SDK's (chain.go), because that part is where the edge cases
// live (IMDSv2 tokens, rotating container tokens, web identity, profiles).
package awsauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Credentials are one set of AWS credentials. SessionToken is set for
// temporary credentials; Expires is meaningful only when CanExpire.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	CanExpire       bool
	Expires         time.Time
	// Source names where the chain found them (for doctor and errors). Never
	// a secret.
	Source string
}

const (
	amzDateFormat = "20060102T150405Z"
	dayFormat     = "20060102"
	algorithm     = "AWS4-HMAC-SHA256"
)

// SignRequest signs req with SigV4 in the header form, for one region and
// service. payload is the exact request body (nil for none). Host, X-Amz-Date
// and, for temporary credentials, X-Amz-Security-Token are added and signed
// with every other header already on req, so it must run after the caller has
// set its own headers and before the request is sent. A signature is valid for
// about five minutes: sign each attempt, never reuse a signed request.
func SignRequest(req *http.Request, payload []byte, c Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateFormat)
	day := now.Format(dayFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	headers := map[string]string{"host": host}
	for k, vs := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(trimAll(vs), ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	sum := sha256.Sum256(payload)
	canonical := strings.Join([]string{
		req.Method, canonicalPath(req.URL), canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, hex.EncodeToString(sum[:]),
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sig := signature(c.SecretAccessKey, day, region, service, stringToSign(amzDate, scope, canonical))
	req.Header.Set("Authorization", algorithm+" Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// PresignURL returns rawURL with a SigV4 signature in its query string (the
// presigned form), valid for expires. Only the host header is signed and the
// payload is the empty body, which is what a presigned request carries.
// rawURL's own query parameters are kept and signed.
func PresignURL(method, rawURL string, c Credentials, region, service string, expires time.Duration, now time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	now = now.UTC()
	amzDate := now.Format(amzDateFormat)
	day := now.Format(dayFormat)
	scope := day + "/" + region + "/" + service + "/aws4_request"

	q := u.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", c.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	if c.SessionToken != "" {
		q.Set("X-Amz-Security-Token", c.SessionToken)
	}
	cq := canonicalQuery(q)
	empty := sha256.Sum256(nil)
	canonical := strings.Join([]string{
		method, canonicalPath(u), cq, "host:" + u.Host + "\n", "host", hex.EncodeToString(empty[:]),
	}, "\n")
	sig := signature(c.SecretAccessKey, day, region, service, stringToSign(amzDate, scope, canonical))
	u.RawQuery = cq + "&X-Amz-Signature=" + sig
	return u.String(), nil
}

func stringToSign(amzDate, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
}

func signature(secret, day, region, service, toSign string) string {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, toSign))
}

// canonicalPath is the path as sent, URI-encoded once more. Every service but
// S3 computes its canonical URI by encoding the path it received, so a path
// that went out with %3A is signed as %253A. For the plain paths loomcycle
// calls (/, /v1/messages) this is the path unchanged.
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts parameters by name, then value, and encodes both.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is AWS's encoding: every byte but the unreserved set
// (A-Z a-z 0-9 - _ . ~) becomes %XX in upper-case hex.
func uriEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[ch>>4])
		b.WriteByte(hexDigits[ch&0xF])
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// trimAll trims each header value and collapses inner runs of spaces, as the
// canonical form requires.
func trimAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return out
}
