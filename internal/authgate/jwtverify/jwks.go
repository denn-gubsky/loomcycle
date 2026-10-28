package jwtverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// JWKS defaults.
const (
	// DefaultJWKSMaxAge is how long a fetched key set is used before it is
	// fetched again on the next verification.
	DefaultJWKSMaxAge = time.Hour
	// DefaultJWKSMinRefresh is the least time between two fetches. A token
	// naming a kid the set lacks triggers a fetch (the provider rotated), but
	// at most this often: a stream of tokens with made-up kids must not
	// become a stream of requests to the provider.
	DefaultJWKSMinRefresh = time.Minute
	// maxJWKSBytes bounds a key set response.
	maxJWKSBytes = 1 << 20
	// maxJWKSKeys bounds how many keys a set may hold.
	maxJWKSKeys = 100
)

// RemoteJWKS is a provider's key set at a URL (an OIDC jwks_uri, Cloudflare
// Access's /cdn-cgi/access/certs), cached by kid.
type RemoteJWKS struct {
	URL    string
	Client *http.Client
	// MaxAge and MinRefresh default to DefaultJWKSMaxAge and
	// DefaultJWKSMinRefresh.
	MaxAge     time.Duration
	MinRefresh time.Duration
	Now        func() time.Time

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time
	triedAt   time.Time
	fetches   int
}

// Fetches reports how many times the set was fetched (for tests and the
// doctor).
func (r *RemoteJWKS) Fetches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fetches
}

func (r *RemoteJWKS) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Key returns the key for kid, fetching the set when it is older than MaxAge
// or lacks kid — no more often than MinRefresh. A failed fetch keeps the keys
// already held.
func (r *RemoteJWKS) Key(ctx context.Context, kid string, alg jose.SignatureAlgorithm) (jose.JSONWebKey, error) {
	maxAge, minRefresh := r.MaxAge, r.MinRefresh
	if maxAge <= 0 {
		maxAge = DefaultJWKSMaxAge
	}
	if minRefresh <= 0 {
		minRefresh = DefaultJWKSMinRefresh
	}
	// Held across the fetch on purpose: concurrent verifications that all
	// miss wait for one fetch rather than each making their own.
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	k, found := pickKey(r.keys, kid, alg)
	stale := r.fetchedAt.IsZero() || now.Sub(r.fetchedAt) >= maxAge
	if found && !stale {
		return k, nil
	}
	if !r.triedAt.IsZero() && now.Sub(r.triedAt) < minRefresh {
		if found {
			return k, nil
		}
		return jose.JSONWebKey{}, fmt.Errorf("%w %q (key set refresh rate-limited)", ErrUnknownKey, kid)
	}
	r.triedAt = now
	r.fetches++
	keys, err := r.fetch(ctx)
	if err != nil {
		if found {
			return k, nil
		}
		return jose.JSONWebKey{}, fmt.Errorf("%w: %w", ErrKeySetUnavailable, err)
	}
	r.keys, r.fetchedAt = keys, now
	if k, ok := pickKey(r.keys, kid, alg); ok {
		return k, nil
	}
	return jose.JSONWebKey{}, fmt.Errorf("%w %q", ErrUnknownKey, kid)
}

func (r *RemoteJWKS) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET key set: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("key set too large")
	}
	return ParseKeySet(body)
}

// ParseKeySet reads a JWKS document, keeping only public signing keys. A
// symmetric key (kty oct) is dropped: a provider's published set is public by
// definition, so a shared secret in it is either a mistake or an attack.
func ParseKeySet(body []byte) ([]jose.JSONWebKey, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	if len(raw.Keys) > maxJWKSKeys {
		return nil, fmt.Errorf("key set: more than %d keys", maxJWKSKeys)
	}
	var out []jose.JSONWebKey
	for _, k := range raw.Keys {
		var jwk jose.JSONWebKey
		// One unreadable key (an algorithm this build does not know) does
		// not take the others down with it.
		if err := jwk.UnmarshalJSON(k); err != nil {
			continue
		}
		if !jwk.IsPublic() || !jwk.Valid() || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		out = append(out, jwk)
	}
	return out, nil
}

// pickKey chooses the key for kid. A token without a kid is accepted only
// when exactly one key could have signed it.
func pickKey(keys []jose.JSONWebKey, kid string, alg jose.SignatureAlgorithm) (jose.JSONWebKey, bool) {
	var match []jose.JSONWebKey
	for _, k := range keys {
		if k.Algorithm != "" && k.Algorithm != string(alg) {
			continue
		}
		if kid != "" {
			if k.KeyID == kid {
				return k, true
			}
			continue
		}
		match = append(match, k)
	}
	if kid == "" && len(match) == 1 {
		return match[0], true
	}
	return jose.JSONWebKey{}, false
}

// StaticKeys is a fixed key set (an external gate's registered key).
type StaticKeys []jose.JSONWebKey

// Key picks from the fixed set.
func (s StaticKeys) Key(_ context.Context, kid string, alg jose.SignatureAlgorithm) (jose.JSONWebKey, error) {
	if k, ok := pickKey(s, kid, alg); ok {
		return k, nil
	}
	return jose.JSONWebKey{}, fmt.Errorf("%w %q", ErrUnknownKey, kid)
}
