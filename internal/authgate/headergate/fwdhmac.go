package headergate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
)

// The forward-auth-hmac preset's headers. A forward-auth proxy (Traefik
// ForwardAuth, nginx auth_request, oauth2-proxy, a custom edge) that already
// authenticated the person sets both on every request it forwards:
//
//	X-Loomcycle-Identity:           {"sub":"alice","email":"alice@acme.com","groups":["eng"]}
//	X-Loomcycle-Identity-Signature: t=1792000000,n=<nonce>,v1=<hex HMAC-SHA256>
//
// where v1 = HMAC-SHA256(secret, "loomcycle-identity-v1\n" + t + "\n" + n +
// "\n" + <the X-Loomcycle-Identity value, byte for byte>), t is unix seconds
// and n is a fresh random value per request (16–128 of [A-Za-z0-9_-]).
const (
	IdentityHeader          = "X-Loomcycle-Identity"
	IdentitySignatureHeader = "X-Loomcycle-Identity-Signature"
)

// DefaultHMACWindow is how far t may be from this server's clock.
const DefaultHMACWindow = 5 * time.Minute

const hmacDomain = "loomcycle-identity-v1"

var hmacNonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// ForwardAuthHMAC verifies the forward-auth-hmac preset's header pair.
type ForwardAuthHMAC struct {
	// Secret returns the shared secret (read per request, so a rotated
	// secret takes effect without a restart when its source allows).
	Secret func(ctx context.Context) ([]byte, error)
	// Window bounds |now - t|; DefaultHMACWindow when zero.
	Window time.Duration
	// Replay remembers accepted nonces for the window, on this replica.
	Replay *replay.Cache
	// Shared, when set, records each accepted nonce (as a SHA-256 hash)
	// for every replica, under SharedScope: a header captured between the
	// proxy and one replica is refused by the others too. A record that
	// cannot be written refuses the header.
	Shared      replay.Shared
	SharedScope string
	Now         func() time.Time
}

// SignForwardAuth computes the signature header value for identity, as a
// proxy (or a test) does.
func SignForwardAuth(secret []byte, identity string, t time.Time, nonce string) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",n=" + nonce + ",v1=" + hex.EncodeToString(forwardAuthMAC(secret, ts, nonce, identity))
}

func forwardAuthMAC(secret []byte, ts, nonce, identity string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(hmacDomain + "\n" + ts + "\n" + nonce + "\n" + identity))
	return m.Sum(nil)
}

// Verify checks one identity header and its signature.
func (f *ForwardAuthHMAC) Verify(ctx context.Context, identity, signature string) (Identity, error) {
	if identity == "" || signature == "" {
		return Identity{}, invalid("forward-auth: identity or signature header missing")
	}
	if len(identity) > maxAssertionBytes || len(signature) > 512 {
		return Identity{}, invalid("forward-auth: oversized header")
	}
	if f.Secret == nil || f.Replay == nil {
		return Identity{}, errors.New("forward-auth: gate is not configured")
	}
	ts, nonce, mac, err := parseForwardAuthSignature(signature)
	if err != nil {
		return Identity{}, err
	}
	secret, err := f.Secret(ctx)
	if err != nil || len(secret) == 0 {
		// Fail closed: no secret, no identity. Not the caller's fault, so
		// not ErrInvalidCredential.
		return Identity{}, fmt.Errorf("forward-auth: secret unavailable")
	}
	// The MAC first: nothing unauthenticated reaches the replay cache, so
	// junk cannot fill it.
	if !hmac.Equal(mac, forwardAuthMAC(secret, ts, nonce, identity)) {
		return Identity{}, invalid("forward-auth: bad signature")
	}
	sec, _ := strconv.ParseInt(ts, 10, 64)
	t := time.Unix(sec, 0)
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	window := f.Window
	if window <= 0 {
		window = DefaultHMACWindow
	}
	if d := now.Sub(t); d >= window || d <= -window {
		return Identity{}, invalid("forward-auth: timestamp outside the window")
	}
	// Remembered past the last moment t is inside the window, judged by the
	// same clock read as the window. The MAC is already checked, so only an
	// authenticated nonce is recorded.
	expires := replay.EntryExpiry(t, window)
	if err := f.Replay.Admit(nonce, expires, now); err != nil {
		return Identity{}, invalid("forward-auth: nonce %v", err)
	}
	if f.Shared != nil {
		sum := sha256.Sum256([]byte(nonce))
		ok, err := f.Shared.UseOnce(ctx, f.SharedScope, hex.EncodeToString(sum[:]), expires, now)
		if err != nil {
			// Fail closed, and not as the caller's fault: unrecorded, the
			// header could be replayed to another replica.
			return Identity{}, fmt.Errorf("forward-auth: recording the nonce: %w", err)
		}
		if !ok {
			return Identity{}, invalid("forward-auth: nonce %v", replay.ErrReplayed)
		}
	}
	claims, err := decodeClaims([]byte(identity))
	if err != nil {
		return Identity{}, err
	}
	sub := stringClaim(claims, "sub")
	if sub == "" {
		return Identity{}, invalid("forward-auth: identity names no sub")
	}
	return Identity{
		Subject:       sub,
		Email:         stringClaim(claims, "email"),
		EmailVerified: boolClaim(claims, "email_verified"),
		Name:          stringClaim(claims, "name"),
		Groups:        stringsClaim(claims, "groups"),
		Tenant:        stringClaim(claims, "tenant"),
		// The header is per request: it is valid for this request only.
		ExpiresAt: now,
		AMR:       []string{"forward-auth"},
		Claims:    claims,
	}, nil
}

// parseForwardAuthSignature reads "t=…,n=…,v1=…" (in any order, each once).
func parseForwardAuthSignature(v string) (ts, nonce string, mac []byte, err error) {
	seen := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return "", "", nil, invalid("forward-auth: malformed signature header")
		}
		if _, dup := seen[k]; dup {
			return "", "", nil, invalid("forward-auth: malformed signature header")
		}
		seen[k] = val
	}
	ts, nonce = seen["t"], seen["n"]
	if len(seen) != 3 || ts == "" || nonce == "" || seen["v1"] == "" {
		return "", "", nil, invalid("forward-auth: malformed signature header")
	}
	if _, perr := strconv.ParseUint(ts, 10, 63); perr != nil || len(ts) > 12 {
		return "", "", nil, invalid("forward-auth: malformed timestamp")
	}
	if !hmacNonceRE.MatchString(nonce) {
		return "", "", nil, invalid("forward-auth: malformed nonce")
	}
	mac, herr := hex.DecodeString(seen["v1"])
	if herr != nil || len(mac) != sha256.Size {
		return "", "", nil, invalid("forward-auth: malformed signature")
	}
	return ts, nonce, mac, nil
}
