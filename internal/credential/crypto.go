// Package credential implements loomcycle's per-tenant secure credential store
// (RFC AR CredentialDef). This file is the crypto core: envelope encryption of
// secret values with a per-tenant key derived from one deployment master key.
//
// loomcycle otherwise keeps secret *values* out of the DB (it stores
// ${LOOMCYCLE_*} references and resolves them from host env at use time, F32).
// CredentialDef is the first primitive that must hold secret *bytes* at rest, so
// it brings its own encryption:
//
//   - KEK (key-encryption key): one deployment master key, LOOMCYCLE_SECRET_KEY,
//     a base64-encoded 32-byte value. Never stored; lives only in process env.
//   - DEK (data-encryption key): derived per tenant via HKDF-SHA256(KEK,
//     info=tenant) at use time — no DEK table, no wrapping (mirrors SQL-Memory's
//     per-scope HMAC-derived password).
//   - AES-256-GCM with a random 96-bit nonce. The GCM AAD binds the ciphertext
//     to its credential row (key_id|tenant|scope|scope_id|name), so a raw row
//     copied to another tenant/name fails authentication — defense-in-depth
//     beneath the app-layer tenant scoping.
//
// Each ciphertext records the KEK *fingerprint* (key_id) that sealed it, so a
// KEK rotation (set LOOMCYCLE_SECRET_KEY=new, LOOMCYCLE_SECRET_KEY_PREVIOUS=old)
// still opens old rows — no version bookkeeping, no schema change; a lazy
// re-encrypt-on-write sweep migrates them (see NeedsReseal).
//
// Fail-closed: with no KEK configured, Seal/Open return ErrNoKey — the inline
// backend is disabled and the runtime NEVER falls back to storing plaintext.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// hkdfSalt is a fixed, non-secret domain-separation salt. The secret is the
// KEK; per-tenant separation comes from the HKDF info string. A constant salt
// is standard for HKDF when the input key material is already high-entropy.
var hkdfSalt = []byte("loomcycle/credentialdef/hkdf/v1")

var (
	// ErrNoKey is returned when no KEK is configured — the inline backend is
	// disabled. Callers surface this as "set LOOMCYCLE_SECRET_KEY to store
	// inline credentials" rather than ever writing plaintext.
	ErrNoKey = errors.New("credential: inline backend disabled (LOOMCYCLE_SECRET_KEY not set)")
	// ErrDecrypt is a deliberately opaque failure for a wrong key, tampered
	// ciphertext, an unknown key fingerprint (rotated out), or a row whose
	// identity (tenant/scope/name) doesn't match the AAD it was sealed with.
	// Never distinguishes the cause (no oracle).
	ErrDecrypt = errors.New("credential: decrypt failed (wrong key, tampered data, or mismatched identity)")
)

// Sealer performs envelope encryption of credential secret values. Construct
// via NewSealer; it is immutable and safe for concurrent use.
type Sealer struct {
	keks      map[string][]byte // key_id (KEK fingerprint) → 32-byte KEK
	currentID string            // fingerprint of the KEK new writes use
}

// keyID is a short, non-secret fingerprint of a KEK — the first 8 bytes of its
// SHA-256, hex-encoded. Like a GPG key fingerprint: identifies which key sealed
// a ciphertext without revealing the key (preimage-resistant, truncated).
func keyID(kek []byte) string {
	sum := sha256.Sum256(kek)
	return hex.EncodeToString(sum[:8])
}

// NewSealer builds a Sealer from the current and optional previous master keys,
// each a base64-encoded 32-byte value (generate with `openssl rand -base64 32`).
// An empty currentB64 yields a disabled Sealer (Enabled()==false; Seal/Open
// return ErrNoKey) — the fail-closed posture. previousB64 (optional) lets a KEK
// rotation still open rows sealed under the prior key.
func NewSealer(currentB64, previousB64 string) (*Sealer, error) {
	s := &Sealer{keks: map[string][]byte{}}
	if strings.TrimSpace(currentB64) == "" {
		return s, nil // disabled — no key configured
	}
	cur, err := decodeKey(currentB64)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
	}
	s.currentID = keyID(cur)
	s.keks[s.currentID] = cur
	if strings.TrimSpace(previousB64) != "" {
		prev, err := decodeKey(previousB64)
		if err != nil {
			return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
		}
		// If previous == current (mid-rotation misconfig) the map just holds one
		// entry; harmless.
		s.keks[keyID(prev)] = prev
	}
	return s, nil
}

func decodeKey(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("must decode to 32 bytes for AES-256 (got %d)", len(raw))
	}
	return raw, nil
}

// Enabled reports whether inline encryption is available (a KEK is configured).
func (s *Sealer) Enabled() bool { return len(s.keks) > 0 }

// Identity binds a sealed value to the credential row it belongs to. It becomes
// the GCM AAD, so decryption only succeeds when the row's identity matches what
// was sealed — a ciphertext copied to another tenant/scope/name won't open.
type Identity struct {
	TenantID string
	Scope    string
	ScopeID  string
	Name     string
}

// aad renders (key_id + row identity) as authenticated (not encrypted)
// associated data. Field separation via the ASCII unit separator (0x1f), which
// the credential name/scope charset excludes, so distinct tuples can't collide.
func (id Identity) aad(kid string) []byte {
	return fmt.Appendf(nil, "%s\x1f%s\x1f%s\x1f%s\x1f%s",
		kid, id.TenantID, id.Scope, id.ScopeID, id.Name)
}

// Sealed is the persisted envelope (stored as JSON in the credential row's
// definition.value). It carries no secret without the KEK.
type Sealed struct {
	KeyID      string `json:"key_id"`     // KEK fingerprint that sealed this
	Nonce      string `json:"nonce"`      // base64, 96-bit GCM nonce
	Ciphertext string `json:"ciphertext"` // base64, GCM output (ct||tag)
}

// dek derives the per-tenant data-encryption key from the KEK identified by kid.
func (s *Sealer) dek(kid, tenantID string) ([]byte, error) {
	kek, ok := s.keks[kid]
	if !ok {
		return nil, ErrNoKey
	}
	return hkdf.Key(sha256.New, kek, hkdfSalt, "credentialdef:v1:tenant:"+tenantID, 32)
}

func (s *Sealer) gcm(kid, tenantID string) (cipher.AEAD, error) {
	dek, err := s.dek(kid, tenantID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext for the given credential identity under the current
// KEK. Returns ErrNoKey when no KEK is configured (fail-closed).
func (s *Sealer) Seal(plaintext []byte, id Identity) (Sealed, error) {
	if !s.Enabled() {
		return Sealed{}, ErrNoKey
	}
	gcm, err := s.gcm(s.currentID, id.TenantID)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, id.aad(s.currentID))
	return Sealed{
		KeyID:      s.currentID,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// Open decrypts a sealed value for the given credential identity. It returns
// ErrDecrypt (opaque) for any failure — wrong/unknown key, tampered bytes, or
// an identity that doesn't match the AAD the value was sealed with.
func (s *Sealer) Open(sealed Sealed, id Identity) ([]byte, error) {
	if !s.Enabled() {
		return nil, ErrNoKey
	}
	gcm, err := s.gcm(sealed.KeyID, id.TenantID)
	if err != nil {
		// Unknown key fingerprint (rotated out) — treat as undecryptable.
		return nil, ErrDecrypt
	}
	nonce, err := base64.StdEncoding.DecodeString(sealed.Nonce)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return nil, ErrDecrypt
	}
	ct, err := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return nil, ErrDecrypt
	}
	pt, err := gcm.Open(nil, nonce, ct, id.aad(sealed.KeyID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// NeedsReseal reports whether a sealed value was written under a non-current KEK
// and should be re-sealed under the current one (lazy re-encrypt-on-write during
// a KEK rotation).
func (s *Sealer) NeedsReseal(sealed Sealed) bool {
	return s.Enabled() && sealed.KeyID != s.currentID
}
