package headergate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ALBHeader carries the ALB's signed user claims (authenticate-oidc /
// authenticate-cognito). The ALB's x-amzn-oidc-identity and
// x-amzn-oidc-accesstoken headers are not signed and are never read.
const ALBHeader = "X-Amzn-Oidc-Data"

var (
	albSignerRE = regexp.MustCompile(`^arn:aws(-[a-z]+)*:elasticloadbalancing:([a-z]{2}(-[a-z]+)+-[0-9]):[0-9]{12}:loadbalancer/app/[A-Za-z0-9-]{1,32}/[0-9a-f]{1,32}$`)
	albKidRE    = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// ALBRegionFromSigner is the region in an application load balancer ARN, ""
// when s is not one.
func ALBRegionFromSigner(s string) string {
	m := albSignerRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[2]
}

// ALB verifies the ES256 token an AWS Application Load Balancer puts in
// x-amzn-oidc-data after it authenticated the user.
//
// The ALBeast lesson: the regional key endpoint serves keys for EVERY load
// balancer in the region, so a valid signature proves only that some ALB —
// possibly one an attacker created in their own account, configured with
// their own IdP — signed it. The signer header names which ALB; it must equal
// this deployment's.
type ALB struct {
	// SignerARN is this deployment's load balancer ARN.
	SignerARN string
	// Issuer, when set, must equal the token's iss (the IdP the ALB is
	// configured with).
	Issuer string
	// ClientID, when set, must equal the token's client (the ALB's OIDC
	// client id).
	ClientID string
	Keys     *ALBKeys
	Now      func() time.Time
}

type albHeader struct {
	Alg    string `json:"alg"`
	Kid    string `json:"kid"`
	Signer string `json:"signer"`
	Iss    string `json:"iss"`
	Client string `json:"client"`
}

// Verify checks one x-amzn-oidc-data value.
func (a *ALB) Verify(ctx context.Context, token string) (Identity, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxAssertionBytes {
		return Identity{}, invalid("aws alb: empty or oversized token")
	}
	if a.Keys == nil || ALBRegionFromSigner(a.SignerARN) == "" || a.Keys.Region != ALBRegionFromSigner(a.SignerARN) {
		return Identity{}, fmt.Errorf("aws alb: gate is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, invalid("aws alb: not a compact JWS")
	}
	rawHeader, err1 := albSegment(parts[0])
	rawPayload, err2 := albSegment(parts[1])
	sig, err3 := albSegment(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Identity{}, invalid("aws alb: malformed token")
	}
	var h albHeader
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return Identity{}, invalid("aws alb: malformed header")
	}
	// Everything checkable without a key is checked first, so a token for
	// another load balancer never causes a key fetch.
	if h.Alg != "ES256" {
		return Identity{}, invalid("aws alb: algorithm %q refused", h.Alg)
	}
	if h.Signer != a.SignerARN {
		return Identity{}, invalid("aws alb: token signed by another load balancer")
	}
	if a.Issuer != "" && h.Iss != a.Issuer {
		return Identity{}, invalid("aws alb: wrong issuer")
	}
	if a.ClientID != "" && h.Client != a.ClientID {
		return Identity{}, invalid("aws alb: wrong client")
	}
	if !albKidRE.MatchString(h.Kid) {
		return Identity{}, invalid("aws alb: malformed kid")
	}
	headerClaims, err := decodeClaims(rawHeader)
	if err != nil {
		return Identity{}, err
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	exp, err := checkTimes(headerClaims, now)
	if err != nil {
		return Identity{}, err
	}
	key, err := a.Keys.Key(ctx, h.Kid)
	if err != nil {
		return Identity{}, invalid("aws alb: %v", err)
	}
	if len(sig) != 64 {
		return Identity{}, invalid("aws alb: malformed signature")
	}
	// The ALB signs the segments as it sent them — padding included — so
	// the signing input is the raw text, not a re-encoding.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return Identity{}, invalid("aws alb: bad signature")
	}
	claims, err := decodeClaims(rawPayload)
	if err != nil {
		return Identity{}, err
	}
	if _, present := claims["exp"]; present {
		if pexp, err := checkTimes(claims, now); err != nil {
			return Identity{}, err
		} else if pexp.Before(exp) {
			exp = pexp
		}
	}
	sub := stringClaim(claims, "sub")
	if sub == "" {
		return Identity{}, invalid("aws alb: token names no subject")
	}
	return Identity{
		Issuer:        h.Iss,
		Subject:       sub,
		Email:         stringClaim(claims, "email"),
		EmailVerified: boolClaim(claims, "email_verified"),
		Name:          stringClaim(claims, "name"),
		Groups:        stringsClaim(claims, "groups"),
		ExpiresAt:     exp,
		AMR:           []string{"aws-alb"},
		Claims:        claims,
	}, nil
}

// albSegment decodes one segment. AWS documents that the ALB's token is
// base64url WITH padding, which strict JWT libraries refuse; the padding is
// accepted here and nowhere else.
func albSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// ALBKeys fetches and caches an ALB region's public keys, one per kid, from
// https://public-keys.auth.elb.<region>.amazonaws.com/<kid>. A kid's key never
// changes, so a fetched key is kept. A kid nobody has seen costs a fetch, and
// anyone can put a random kid in a token, so unknown-kid fetches are
// rate-limited (a token bucket) and a kid the endpoint did not know is
// remembered as unknown for a while.
type ALBKeys struct {
	Region string
	// HTTP is the client keys are fetched with; nil uses a 5-second-timeout
	// client that follows no redirects.
	HTTP *http.Client
	// Burst and Every shape the unknown-kid budget: Burst fetches at once, then
	// one per Every. Defaults 3 and 10 s.
	Burst int
	Every time.Duration
	Now   func() time.Time

	// keyURL builds the fetch URL; tests point it at a local server. The
	// operator cannot: the host is AWS's, fixed by the region.
	keyURL func(region, kid string) string

	mu      sync.Mutex
	keys    map[string]*ecdsa.PublicKey
	unknown map[string]time.Time
	tokens  float64
	last    time.Time
	fetchMu sync.Mutex
}

var (
	// errKeyBudget: an unknown kid arrived while the fetch budget was spent.
	errKeyBudget = errors.New("unknown key id and the key-fetch budget is spent")
	// errNoSuchKey: the key endpoint does not know the kid.
	errNoSuchKey = errors.New("unknown key id")
)

const (
	albMaxKeys     = 64
	albUnknownTTL  = 5 * time.Minute
	albMaxKeyBytes = 8 << 10
)

// ALBKeyURL is AWS's regional key endpoint for one key id (loomcycle doctor
// probes the endpoint with an empty kid).
func ALBKeyURL(region, kid string) string {
	return "https://public-keys.auth.elb." + region + ".amazonaws.com/" + kid
}

func (k *ALBKeys) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// Key returns the public key for kid.
func (k *ALBKeys) Key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	if !albKidRE.MatchString(kid) {
		return nil, errors.New("malformed kid")
	}
	if key, done, err := k.cached(kid); done {
		return key, err
	}
	// One fetch at a time; verifications with a known kid never wait here.
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()
	if key, done, err := k.cached(kid); done {
		return key, err
	}
	if !k.spend() {
		return nil, errKeyBudget
	}
	key, err := k.fetch(ctx, kid)
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		// Only AWS saying "no such key" marks the kid unknown; a timeout or
		// a cancelled request says nothing about it.
		if !errors.Is(err, errNoSuchKey) {
			return nil, err
		}
		if k.unknown == nil {
			k.unknown = make(map[string]time.Time)
		}
		if len(k.unknown) >= albMaxKeys {
			clear(k.unknown)
		}
		k.unknown[kid] = k.now().Add(albUnknownTTL)
		return nil, err
	}
	if k.keys == nil {
		k.keys = make(map[string]*ecdsa.PublicKey)
	}
	if len(k.keys) >= albMaxKeys {
		// A key can always be fetched again; bound the map instead.
		for old := range k.keys {
			delete(k.keys, old)
			break
		}
	}
	k.keys[kid] = key
	return key, nil
}

func (k *ALBKeys) cached(kid string) (*ecdsa.PublicKey, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[kid]; ok {
		return key, true, nil
	}
	if until, ok := k.unknown[kid]; ok && k.now().Before(until) {
		return nil, true, errNoSuchKey
	}
	return nil, false, nil
}

// spend takes one unknown-kid fetch from the bucket.
func (k *ALBKeys) spend() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	burst, every := k.Burst, k.Every
	if burst <= 0 {
		burst = 3
	}
	if every <= 0 {
		every = 10 * time.Second
	}
	now := k.now()
	if k.last.IsZero() {
		k.tokens = float64(burst)
	} else {
		k.tokens += float64(now.Sub(k.last)) / float64(every)
		if k.tokens > float64(burst) {
			k.tokens = float64(burst)
		}
	}
	k.last = now
	if k.tokens < 1 {
		return false
	}
	k.tokens--
	return true
}

func (k *ALBKeys) fetch(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	build := k.keyURL
	if build == nil {
		build = ALBKeyURL
	}
	client := k.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, build(k.Region, kid), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the ALB key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		// S3-backed: a missing object answers 403 as often as 404.
		return nil, errNoSuchKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the ALB key: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, albMaxKeyBytes+1))
	if err != nil || len(raw) > albMaxKeyBytes {
		return nil, errors.New("fetching the ALB key: unreadable or oversized answer")
	}
	return parseALBKey(raw)
}

// parseALBKey reads the endpoint's PEM "PUBLIC KEY", which must be P-256.
func parseALBKey(raw []byte) (*ecdsa.PublicKey, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("ALB key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ALB key: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("ALB key is not a P-256 key")
	}
	return ec, nil
}
