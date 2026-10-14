// Package sigv1 is loomcycle's one implementation of its HMAC-SHA256 request
// signatures, shared by everything that signs a POST or checks one:
//
//   - the timestamped envelope `t=<unix>,v1=<hex>`, a MAC over `<t>.<body>`,
//     which outbound hook deliveries carry and the inbound webhook receiver
//     accepts;
//   - the bare form `<hex>`, a MAC over the body alone, which change
//     subscriptions carry and the receiver also accepts (Linear-style).
//
// One package so that loomcycle can verify what loomcycle signs: a signer and
// a verifier written twice drift apart (a separator, a whitespace rule) and
// the drift shows up as a receiver that rejects every delivery.
//
// Every comparison is constant-time (hmac.Equal); every malformed input is a
// plain "no" with no reason, so a caller cannot be used as an oracle.
package sigv1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Header is the header loomcycle's signatures travel in.
const Header = "X-Loomcycle-Signature"

// DefaultTolerance is how far an envelope's `t` may be from the verifier's
// clock, either way: the replay window. Stripe's documented default.
const DefaultTolerance = 5 * time.Minute

// mac is HMAC-SHA256(secret, payload).
func mac(secret string, payload []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return m.Sum(nil)
}

// SignBare is the bare form: hex(HMAC-SHA256(secret, body)).
func SignBare(secret string, body []byte) string {
	return hex.EncodeToString(mac(secret, body))
}

// VerifyBare reports whether wantHex (surrounding space ignored) is the bare
// signature of body. Bad hex, a wrong length and a wrong MAC are all false.
func VerifyBare(secret string, body []byte, wantHex string) bool {
	want, err := hex.DecodeString(strings.TrimSpace(wantHex))
	if err != nil {
		return false
	}
	return hmac.Equal(mac(secret, body), want)
}

// Sign is the envelope for body signed at t: `t=<unix>,v1=<hex>`, the MAC
// over `<unix>.<body>`. body must be exactly the bytes sent.
func Sign(secret string, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, signedPayload(ts, body)))
}

// Verify reports whether header is a valid envelope for body: its `t` within
// tolerance of now (inclusive) and its `v1` the MAC of `<t>.<body>`, with `t`
// taken exactly as written. A missing field, an unparsable `t`, a stale or
// future `t`, bad hex or a wrong MAC are all false.
func Verify(secret string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	tsStr, macHex := Fields(header)
	if tsStr == "" || macHex == "" {
		return false
	}
	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	skew := now.Sub(time.Unix(tsUnix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return false
	}
	return VerifyBare(secret, signedPayload(tsStr, body), macHex)
}

// Fields returns the `t=` and `v1=` values of an envelope ("" when absent).
// Parts are comma-separated and may carry spaces around them; a repeated
// field keeps its last value.
func Fields(header string) (ts, macHex string) {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			ts = strings.TrimPrefix(part, "t=")
		case strings.HasPrefix(part, "v1="):
			macHex = strings.TrimPrefix(part, "v1=")
		}
	}
	return ts, macHex
}

func signedPayload(ts string, body []byte) []byte {
	p := make([]byte, 0, len(ts)+1+len(body))
	p = append(p, ts...)
	p = append(p, '.')
	return append(p, body...)
}
