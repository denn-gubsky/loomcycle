// Package kms forwards crypto operations to an external key management
// service — Vault Transit, AWS KMS or Google Cloud KMS — so the runtime can use
// a key whose material it never holds. loomcycle stores only a Ref (where the
// key lives and which credentials reach it) and the key's loomcycle type; each
// operation is a call to the KMS.
//
// Like internal/cryptoops this package is policy-free: no loomcycle imports, no
// logging and no package-level mutable state. Who may use a key is decided a
// layer up. Two rules run through every client here:
//
//   - The algorithm is fixed by the key type given to New, never by a caller.
//   - An error never carries a KMS response body, a token or key material. It
//     names the KMS, the operation and at most an HTTP status, and unwraps to
//     one of the sentinel errors below so the caller can pick a category.
package kms

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind names the KMS a Ref points at. The values are persisted with the
// credential, so add new ones, never rename.
type Kind string

const (
	KindVault Kind = "vault"
	KindAWS   Kind = "aws_kms"
	KindGCP   Kind = "gcp_kms"
)

// Key types, using loomcycle's names (the same strings internal/cryptoops
// persists). The algorithm of each is fixed: RSA-PSS signs SHA-256 digests
// (salt length = hash length), RSA-OAEP uses SHA-256 for hash and MGF1, ECDSA
// signs SHA-256(data) and returns a raw 64-byte r||s, Ed25519 signs data as is.
const (
	AES256GCM   = "aes-256-gcm"
	HMACSHA256  = "hmac-sha256"
	HMACSHA512  = "hmac-sha512"
	ECDSAP256   = "ecdsa-p256"
	Ed25519     = "ed25519"
	RSAPSS2048  = "rsa-pss-2048"
	RSAPSS3072  = "rsa-pss-3072"
	RSAPSS4096  = "rsa-pss-4096"
	RSAOAEP2048 = "rsa-oaep-2048"
	RSAOAEP3072 = "rsa-oaep-3072"
	RSAOAEP4096 = "rsa-oaep-4096"
)

// Operation names, matching internal/cryptoops' Op* strings so a grant can be
// checked against KeyInfo.Ops directly.
const (
	OpEncrypt   = "encrypt"
	OpDecrypt   = "decrypt"
	OpRewrap    = "rewrap"
	OpMAC       = "mac"
	OpVerifyMAC = "verify_mac"
	OpSign      = "sign"
	OpVerify    = "verify"
	OpPublicKey = "public_key"
)

// Sentinel errors. Every error a Client returns wraps exactly one of these
// (plus, for a network failure, the transport error).
var (
	// ErrUnsupported: the operation is not possible for this key type on this
	// KMS (e.g. Sign on an AES key, AAD with RSA-OAEP, Ed25519 on AWS).
	ErrUnsupported = errors.New("operation not supported for this key")
	// ErrDenied: the KMS refused the credentials or the permission, or the
	// credentials could not be turned into an auth token.
	ErrDenied = errors.New("permission denied")
	// ErrUnavailable: network failure, timeout, throttling, 5xx or a
	// malformed response. Transient; a retry may succeed.
	ErrUnavailable = errors.New("service unavailable")
	// ErrDecrypt: the ciphertext did not decrypt under this key (wrong key,
	// wrong AAD, tampered or malformed). Deliberately opaque.
	ErrDecrypt = errors.New("decryption failed")
	// ErrMismatch: Describe found a key whose type is not the one asked for.
	ErrMismatch = errors.New("key type mismatch")
	// ErrNotFound: the KMS says the key (or key version) does not exist.
	ErrNotFound = errors.New("key not found")
	// ErrRejected: the KMS rejected the request for another reason (key
	// disabled, input too large, malformed request).
	ErrRejected = errors.New("request rejected")
)

// Secrets resolves a "$cred:<name>" reference to its secret value. The caller
// supplies it (it is the tenant-scoped credential store); this package calls
// it on each use, never caches its result beyond one request, except that the
// GCP client caches the OAuth2 access token it mints from the key.
type Secrets func(ctx context.Context, credRef string) (string, error)

// Options configures New. Only HTTPClient and Secrets are meant for production
// callers; the endpoint overrides exist for tests.
type Options struct {
	// HTTPClient sends every request. Required for Vault, whose address is
	// the Ref author's: pass an SSRF-guarded client. For AWS and Google (fixed
	// public endpoints) nil means a client with a 30s timeout. New copies it
	// and disables redirects (a redirect would carry the auth header to
	// wherever it points).
	HTTPClient *http.Client
	// Secrets resolves the Ref's credential references. Required.
	Secrets Secrets
	// Now is the clock, for SigV4 dates and token expiry. nil means time.Now.
	Now func() time.Time
	// AllowHTTP admits a plain http:// Vault address. Off by default.
	AllowHTTP bool

	// Test overrides. AWSEndpoint replaces https://kms.<region>.amazonaws.com;
	// GCPEndpoint replaces https://cloudkms.googleapis.com; GCPTokenURL
	// replaces https://oauth2.googleapis.com/token.
	AWSEndpoint, GCPEndpoint, GCPTokenURL string
}

// KeyInfo is the KMS's own view of a key, mapped onto loomcycle's names, for a
// create-time check that a Ref points at the key it claims to.
type KeyInfo struct {
	// KeyType is the loomcycle type the KMS key maps to, or "" when it maps to
	// none this package supports.
	KeyType string `json:"key_type"`
	// Native is the KMS's own name for the key type (e.g. "aes256-gcm96",
	// "SYMMETRIC_DEFAULT", "EC_SIGN_P256_SHA256"). Non-secret.
	Native string `json:"native"`
	// Version is the current key version; 0 when the KMS exposes none (AWS).
	Version int `json:"version"`
	// Enabled is false when the KMS reports the key (or its current version)
	// as disabled or pending deletion.
	Enabled bool `json:"enabled"`
	// Ops are the operations this package offers for KeyType.
	Ops []string `json:"ops"`
}

// Client forwards crypto operations on one KMS key. Implementations are safe
// for concurrent use. A Client caches auth tokens and public keys, so callers
// should keep one per (Ref, key type) rather than build one per call.
//
// version arguments name a key version to use; 0 means "the current one" for
// PublicKey and "any usable version, newest first" for Verify and VerifyMAC.
// Returned versions are 0 where the KMS has no versions (AWS) or does not say
// which one it used (a GCP symmetric decrypt).
type Client interface {
	Encrypt(ctx context.Context, plaintext, aad []byte) (body []byte, version int, err error)
	Decrypt(ctx context.Context, body, aad []byte) (plaintext []byte, version int, err error)
	Rewrap(ctx context.Context, body, aad []byte) (newBody []byte, version int, err error)
	// DecryptOAEP decrypts a bare RSA-OAEP ciphertext (no KMS framing, as a
	// JWE's encrypted key carries it) with key version version, 0 meaning the
	// current one. RSA-OAEP keys only.
	DecryptOAEP(ctx context.Context, ct []byte, version int) (plaintext []byte, usedVersion int, err error)
	MAC(ctx context.Context, data []byte) (mac []byte, version int, err error)
	VerifyMAC(ctx context.Context, data, mac []byte, version int) (valid bool, usedVersion int, err error)
	Sign(ctx context.Context, data []byte) (sig []byte, version int, err error)
	Verify(ctx context.Context, data, sig []byte, version int) (valid bool, usedVersion int, err error)
	PublicKey(ctx context.Context, version int) (pub crypto.PublicKey, usedVersion int, err error)
	// Describe reports the KMS's view of the key. When the key's type is not
	// the one New was given, it returns the info together with ErrMismatch.
	Describe(ctx context.Context) (KeyInfo, error)
}

const (
	defaultTimeout = 30 * time.Second
	maxResponse    = 1 << 20
	// maxVersionTries bounds how many key versions a verify or an RSA decrypt
	// walks, so an attacker-supplied input can't fan out into unbounded calls.
	maxVersionTries = 16
)

// New returns a Client for the key ref points at, typed as keyType. It checks
// the Ref's shape and that this KMS supports keyType; it makes no network call
// (use Describe for that).
func New(ref Ref, keyType string, opts Options) (Client, error) {
	if err := ref.Validate(opts.AllowHTTP); err != nil {
		return nil, err
	}
	if !knownType(keyType) {
		return nil, fmt.Errorf("kms: unknown key type %q", keyType)
	}
	if !kindSupports(ref.Kind, keyType) {
		return nil, fmt.Errorf("kms: %s does not support key type %s: %w", ref.Kind, keyType, ErrUnsupported)
	}
	if opts.Secrets == nil {
		return nil, errors.New("kms: Options.Secrets is required")
	}
	// A Vault address is the Ref author's, so it is dialled only through the
	// caller's client, which carries the SSRF guard. There is no default for
	// it: a default client would reach any address the name resolves to.
	if ref.Kind == KindVault && opts.HTTPClient == nil {
		return nil, errors.New("kms: a Vault key needs Options.HTTPClient (an SSRF-guarded client)")
	}
	// Cloud KMS encrypts only with a symmetric key's primary version, so a
	// pin could not be honoured: refuse it rather than ignore it.
	if ref.Kind == KindGCP && isAEAD(keyType) && strings.Contains(ref.Resource, "/cryptoKeyVersions/") {
		return nil, fmt.Errorf("kms: a %s key cannot be pinned to a version (Cloud KMS encrypts with the primary): %w", keyType, ErrUnsupported)
	}
	b := base{kind: ref.Kind, keyType: keyType, secrets: opts.Secrets, now: opts.Now, http: noRedirect(opts.HTTPClient)}
	if b.now == nil {
		b.now = time.Now
	}
	switch ref.Kind {
	case KindVault:
		return newVault(b, ref), nil
	case KindAWS:
		return newAWS(b, ref, opts.AWSEndpoint), nil
	case KindGCP:
		return newGCP(b, ref, opts.GCPEndpoint, opts.GCPTokenURL), nil
	}
	return nil, fmt.Errorf("kms: unknown kind %q", ref.Kind)
}

// noRedirect copies c (or a default) with redirects disabled: Vault's token
// and GCP's bearer travel in headers a redirect would forward. The copy shares
// c's Transport, so an SSRF guard on it still applies.
func noRedirect(c *http.Client) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	} else {
		cp.Timeout = defaultTimeout
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

func knownType(t string) bool {
	switch t {
	case AES256GCM, HMACSHA256, HMACSHA512, ECDSAP256, Ed25519,
		RSAPSS2048, RSAPSS3072, RSAPSS4096, RSAOAEP2048, RSAOAEP3072, RSAOAEP4096:
		return true
	}
	return false
}

// kindSupports: every type works on Vault; AWS and GCP lack Ed25519 here.
func kindSupports(k Kind, t string) bool {
	return t != Ed25519 || k == KindVault
}

func isAEAD(t string) bool { return t == AES256GCM }
func isMAC(t string) bool  { return t == HMACSHA256 || t == HMACSHA512 }
func isRSAOAEP(t string) bool {
	return t == RSAOAEP2048 || t == RSAOAEP3072 || t == RSAOAEP4096
}
func isRSAPSS(t string) bool {
	return t == RSAPSS2048 || t == RSAPSS3072 || t == RSAPSS4096
}
func isSigning(t string) bool    { return t == ECDSAP256 || t == Ed25519 || isRSAPSS(t) }
func isEncrypting(t string) bool { return isAEAD(t) || isRSAOAEP(t) }
func hasPublicKey(t string) bool { return isSigning(t) || isRSAOAEP(t) }

// rsaBits is the modulus size an RSA key type names, or 0.
func rsaBits(t string) int {
	switch t {
	case RSAPSS2048, RSAOAEP2048:
		return 2048
	case RSAPSS3072, RSAOAEP3072:
		return 3072
	case RSAPSS4096, RSAOAEP4096:
		return 4096
	}
	return 0
}

// opsFor lists the operations offered for key type t.
func opsFor(t string) []string {
	switch {
	case isAEAD(t):
		return []string{OpEncrypt, OpDecrypt, OpRewrap}
	case isRSAOAEP(t):
		return []string{OpEncrypt, OpDecrypt, OpRewrap, OpPublicKey}
	case isMAC(t):
		return []string{OpMAC, OpVerifyMAC}
	case isSigning(t):
		return []string{OpSign, OpVerify, OpPublicKey}
	}
	return nil
}

// base is what every client shares.
type base struct {
	kind    Kind
	keyType string
	secrets Secrets
	now     func() time.Time
	http    *http.Client
}

// need refuses op unless the key type allows it.
func (b *base) need(op string, ok bool) error {
	if ok {
		return nil
	}
	return &opError{kind: b.kind, op: op, sentinel: ErrUnsupported, detail: "not available for " + b.keyType}
}

// noAAD refuses associated data where the KMS cannot bind it (RSA-OAEP: none
// of the three accepts AAD or a context for asymmetric encryption, and
// dropping it silently would weaken what the caller believes it asked for).
func (b *base) noAAD(op string, aad []byte) error {
	if len(aad) == 0 {
		return nil
	}
	return &opError{kind: b.kind, op: op, sentinel: ErrUnsupported, detail: "associated data cannot be bound to an RSA-OAEP ciphertext"}
}

// secret resolves a credential reference. The resolver's error is wrapped as
// is (it is the caller's own); a resolved empty value is refused.
func (b *base) secret(ctx context.Context, ref string) (string, error) {
	v, err := b.secrets(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("kms: resolve %s: %w", ref, err)
	}
	if v == "" {
		return "", fmt.Errorf("kms: %s resolved to an empty value: %w", ref, ErrDenied)
	}
	return v, nil
}

// describeResult finishes a Describe: fill Ops and flag a type mismatch.
func (b *base) describeResult(info KeyInfo) (KeyInfo, error) {
	info.Ops = opsFor(info.KeyType)
	if info.KeyType != b.keyType {
		got := info.KeyType
		if got == "" {
			got = "an unsupported type"
		}
		return info, &opError{kind: b.kind, op: "describe", sentinel: ErrMismatch,
			detail: fmt.Sprintf("the key is %s (%s), not %s", got, info.Native, b.keyType)}
	}
	return info, nil
}

// opError is every error a Client returns. Its message holds only fixed text,
// the HTTP status and (for network failures) the transport error — never a
// response body.
type opError struct {
	kind     Kind
	op       string
	status   int
	sentinel error
	detail   string
	cause    error
}

func (e *opError) Error() string {
	s := fmt.Sprintf("%s %s: %s", displayName(e.kind), e.op, e.sentinel.Error())
	if e.detail != "" {
		s += ": " + e.detail
	}
	if e.status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.status)
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *opError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.sentinel, e.cause}
	}
	return []error{e.sentinel}
}

func displayName(k Kind) string {
	switch k {
	case KindVault:
		return "vault"
	case KindAWS:
		return "aws kms"
	case KindGCP:
		return "google cloud kms"
	}
	return string(k)
}

func errf(k Kind, op string, sentinel error, status int, detail string) error {
	return &opError{kind: k, op: op, status: status, sentinel: sentinel, detail: detail}
}

// classifyStatus maps a non-2xx status to a sentinel. decryptish marks
// decrypt and verify calls, where a 400 means "this input does not open".
func classifyStatus(status int, decryptish bool) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrDenied
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return ErrUnavailable
	case status >= 300 && status < 400:
		// A refused redirect (a Vault standby pointing at the active node, a
		// proxy): the request did not reach a KMS that would answer it.
		return ErrUnavailable
	case status == http.StatusBadRequest && decryptish:
		return ErrDecrypt
	}
	return ErrRejected
}
