// Package webhook implements the RFC H inbound-webhook RECEIVER — the
// security/trust boundary that turns an external HTTP POST into either
// an agent run (delivery=spawn) or a channel publish (delivery=channel).
//
// This package is deliberately self-contained: it re-declares the small
// credential-resolution shape rather than importing the scheduler (which
// would create a cycle, since the scheduler already imports the store +
// runner). The duplication mirrors the scheduler's own re-declaration of
// the ScheduleDef wire shape; parity is the operator's concern, not a
// compile-time one, because the surfaces are intentionally decoupled.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// signatureTolerance is the ±window applied to the timestamp embedded in
// a Stripe-style signature header. A request whose `t=` is more than this
// far from the receiver's clock (in either direction) is rejected as
// stale-or-future — the standard replay-window guard. 5 minutes matches
// Stripe's documented default tolerance.
const signatureTolerance = 5 * time.Minute

// authError is a typed verification failure the server maps to an HTTP
// status. We do NOT leak the reason to the client for signature failures
// (no timing/oracle leak — Decision 9); the reason exists only for the
// structured log line and the verdict the server records internally.
type authError struct {
	// verdict is the internal-only classification (e.g. "rejected_sig",
	// "unresolvable_secret"). Used for logging + the server's status map.
	verdict string
	// secretEnv is populated only for the secret_unresolvable case, so the
	// 503 body can name WHICH env var is missing (the env-var NAME is not a
	// secret; its value is). Never holds a secret value.
	secretEnv string
	// msg is an operator-facing description for the log line only. Never
	// returned in an HTTP body for the signature-mismatch verdicts.
	msg string
}

func (e *authError) Error() string { return e.msg }

// Verdict classifications. These are the internal labels the server logs
// and maps to status codes; they are never echoed verbatim to the client
// for the signature-failure cases.
const (
	verdictAccepted = "accepted"
	// verdictAcceptedReplay is an idempotent re-delivery of an
	// already-accepted (already-signature-verified) request — a 200 ack, NOT
	// an error. Distinct from verdictAccepted so triage can tell a fresh
	// acceptance from a deduped re-send.
	verdictAcceptedReplay = "accepted_replay"
	verdictRejectedSig    = "rejected_sig"
	verdictUnresolved     = "unresolvable_secret"
)

// errSignatureMismatch is the catch-all for every "the request did not
// authenticate" case: bad MAC, tampered body, missing/garbled header,
// out-of-window timestamp, wrong bearer. They all collapse to one verdict
// so the client cannot distinguish them (no oracle). The server maps it
// to 401 with NO body detail.
var errSignatureMismatch = errors.New("signature_mismatch")

// resolveSecret reads an env var through the allowlist gate. Returns the
// secret_unresolvable authError when the var is not allowlisted, unset, or
// empty. The returned value is a secret — callers must never log it.
//
// getenv is injected (rather than calling os.Getenv directly) so tests can
// drive the allowlist/unset/empty branches deterministically without
// mutating process environment.
func resolveSecret(envName string, allowlist map[string]bool, getenv func(string) string) (string, error) {
	if envName == "" {
		return "", &authError{verdict: verdictUnresolved, secretEnv: envName, msg: "auth: no secret env var configured"}
	}
	if !allowlist[envName] {
		return "", &authError{verdict: verdictUnresolved, secretEnv: envName, msg: fmt.Sprintf("auth: env var %q not in allowlist", envName)}
	}
	v := getenv(envName)
	if v == "" {
		return "", &authError{verdict: verdictUnresolved, secretEnv: envName, msg: fmt.Sprintf("auth: env var %q unset or empty", envName)}
	}
	return v, nil
}

// verifySignature authenticates a raw request body against a WebhookDef's
// auth config. It MUST be called with the RAW bytes read off the wire —
// never a re-serialized body — because the HMAC integrity guarantee is
// over exactly those bytes.
//
// Three shapes are supported, dispatched by auth.kind + header:
//
//   - hmac, Stripe envelope (default): header value `t=<unix>, v1=<hexmac>`
//     where the MAC is HMAC-SHA256 of `<t> + "." + <rawbody>`, keyed by the
//     signing secret. The `t` is checked against ±signatureTolerance.
//   - hmac, GitHub envelope: header `X-Hub-Signature-256: sha256=<hexmac>`
//     where the MAC is HMAC-SHA256 of the raw body ONLY (no timestamp). The
//     shape is detected from the `sha256=` prefix.
//   - bearer: the request's Authorization bearer is compared (constant-time)
//     against the configured bearer_token_env value.
//
// The constant-time primitive is crypto/hmac.Equal for the HMAC cases
// (compares the freshly-computed MAC bytes against the decoded header MAC)
// and auth.CompareBearer (sha256-then-subtle.ConstantTimeCompare) for the
// bearer case. We never compare hex strings with == and never use
// bytes.Equal on the MAC.
//
// now is injected so the timestamp-window check is deterministic in tests.
func verifySignature(a config.WebhookAuth, body []byte, headerGet func(string) string, allowlist map[string]bool, getenv func(string) string, now func() time.Time) error {
	kind := strings.ToLower(strings.TrimSpace(a.Kind))
	if kind == "" {
		kind = "hmac"
	}

	switch kind {
	case "bearer":
		want, err := resolveSecret(a.BearerTokenEnv, allowlist, getenv)
		if err != nil {
			return err
		}
		// Two shapes. Default (no auth.header): the standard
		// `Authorization: Bearer <token>`. When auth.header is set, the
		// shared secret is carried RAW in that header instead — e.g.
		// GitLab's `X-Gitlab-Token: <token>` — so compare the raw value.
		// CompareBearer hashes both sides to a fixed length before the
		// constant-time compare, so neither the "Bearer " prefix nor the
		// token length leaks via timing.
		if a.Header != "" {
			got := strings.TrimSpace(headerGet(a.Header))
			if got == "" || !auth.CompareBearer(got, want) {
				return errSignatureMismatch
			}
			return nil
		}
		got := headerGet("Authorization")
		if got == "" || !auth.CompareBearer(got, "Bearer "+want) {
			return errSignatureMismatch
		}
		return nil

	case "hmac":
		secret, err := resolveSecret(a.SigningSecretEnv, allowlist, getenv)
		if err != nil {
			return err
		}
		return verifyHMAC(a, secret, body, headerGet, now)

	default:
		// Unknown auth kind is a config error, not a request error. Treat
		// as unresolvable so the operator sees a 503 rather than silently
		// accepting or returning a misleading 401.
		return &authError{verdict: verdictUnresolved, secretEnv: "", msg: fmt.Sprintf("auth: unknown kind %q", a.Kind)}
	}
}

// verifyHMAC handles the three HMAC envelope shapes. The header name comes
// from a.Header (operator-addressable); when empty we default to the
// Stripe header. The envelope shape (Stripe `t=,v1=` vs GitHub `sha256=`
// vs bare hex) is detected from the header VALUE, not just its name, so an
// operator who points a.Header at a custom name still gets correct parsing.
func verifyHMAC(a config.WebhookAuth, secret string, body []byte, headerGet func(string) string, now func() time.Time) error {
	headerName := a.Header
	if headerName == "" {
		headerName = "X-Loomcycle-Signature"
	}
	raw := strings.TrimSpace(headerGet(headerName))
	if raw == "" {
		// Fall back to probing the GitHub header by its canonical name when
		// the configured header is absent — lets a Def declare GitHub auth
		// purely via header name without a separate envelope flag.
		if alt := strings.TrimSpace(headerGet("X-Hub-Signature-256")); alt != "" {
			raw = alt
		}
	}
	if raw == "" {
		return errSignatureMismatch
	}

	// GitHub envelope: `sha256=<hexmac>` over the raw body only.
	if strings.HasPrefix(raw, "sha256=") {
		wantHex := strings.TrimPrefix(raw, "sha256=")
		return compareHMAC(secret, body, wantHex)
	}

	// Bare-hex envelope (Linear `Linear-Signature`, and many custom HMAC
	// sources): the entire header value is the hex HMAC-SHA256 of the raw
	// body — no prefix, no timestamp. A Stripe envelope always contains ','
	// and '=' so it is never all-hex; this branch is therefore unambiguous
	// and must be checked BEFORE the Stripe parse.
	if isHexString(raw) {
		return compareHMAC(secret, body, raw)
	}

	// Stripe envelope: `t=<unix>, v1=<hexmac>` over `<t>.<rawbody>`.
	var tsStr, macHex string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			tsStr = strings.TrimPrefix(part, "t=")
		case strings.HasPrefix(part, "v1="):
			macHex = strings.TrimPrefix(part, "v1=")
		}
	}
	if tsStr == "" || macHex == "" {
		return errSignatureMismatch
	}
	tsUnix, perr := strconv.ParseInt(tsStr, 10, 64)
	if perr != nil {
		return errSignatureMismatch
	}
	ts := time.Unix(tsUnix, 0)
	skew := now().Sub(ts)
	if skew < 0 {
		skew = -skew
	}
	if skew > signatureTolerance {
		// Stale or future timestamp — replay-window guard. Collapses to
		// the same verdict so the client cannot probe the tolerance.
		return errSignatureMismatch
	}
	signedPayload := append([]byte(tsStr+"."), body...)
	return compareHMAC(secret, signedPayload, macHex)
}

// isHexString reports whether s is a non-empty run of only hex digits — the
// bare-hex signature envelope (e.g. Linear's `Linear-Signature`). Used to
// disambiguate a raw hex MAC from a Stripe `t=,v1=` envelope (which always
// contains non-hex bytes).
func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// compareHMAC computes HMAC-SHA256(secret, payload) and compares it to the
// hex-decoded wantHex using crypto/hmac.Equal (constant-time). Returns
// errSignatureMismatch on any failure (bad hex, length mismatch, MAC
// mismatch) — all collapse to one verdict so no information leaks.
func compareHMAC(secret string, payload []byte, wantHex string) error {
	want, derr := hex.DecodeString(strings.TrimSpace(wantHex))
	if derr != nil {
		return errSignatureMismatch
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	got := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return errSignatureMismatch
	}
	return nil
}
