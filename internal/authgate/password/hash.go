// Package password holds the pure logic of the password gate (RFC FA §6,
// NaiveAuthGate): Argon2id hashing with a pepper kept outside the database,
// the NIST SP 800-63B-4 password policy, login-name normalisation, TOTP,
// recovery codes, invite and reset codes, and the throttle arithmetic. It does
// no I/O: the HTTP server reads and writes the store and calls in here.
package password

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. The defaults are the OWASP
// Password Storage Cheat Sheet minimum (m=19 MiB, t=2, p=1).
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultParams is what every new hash uses. A stored hash under other
// parameters still verifies and is re-hashed on the next successful login.
var DefaultParams = Params{MemoryKiB: 19456, Time: 2, Threads: 1}

const (
	saltLen = 16
	keyLen  = 32
	// pepperTag versions the pepper derivation; it prefixes every pepper id.
	pepperTag = "pw1"
	// MaxPasswordBytes bounds the input: NIST asks for at least 64
	// characters, and an unbounded input is free work for an attacker.
	MaxPasswordBytes = 1024
)

// ErrNoPepper: the hasher has no pepper. The gate refuses to run rather than
// store hashes a database reader could attack offline.
var ErrNoPepper = errors.New("password: no pepper configured (set LOOMCYCLE_SECRET_KEY)")

type pepper struct {
	key []byte
	kid string
}

func newPepper(key []byte) pepper {
	sum := sha256.Sum256(key)
	return pepper{key: key, kid: pepperTag + ":" + hex.EncodeToString(sum[:8])}
}

// Hasher hashes and verifies passwords. The digest stored in the PHC string
// is HMAC-SHA256(pepper, argon2id(password, salt)): the Argon2 output never
// reaches the database, so a dump of user_credentials cannot be attacked
// without the pepper, which lives only in process memory (derived from
// LOOMCYCLE_SECRET_KEY). The pepper's id is stored beside the hash so a key
// rotation is detected and the password re-hashed on the next login.
//
// Argon2id at these parameters allocates 19 MiB per call; sem bounds how many
// run at once so a burst of logins cannot exhaust memory.
type Hasher struct {
	cur, prev pepper
	params    Params
	sem       chan struct{}
}

// maxConcurrentHashes bounds the memory Argon2 can hold at once (19 MiB
// each). Callers beyond it wait (or give up when their context ends).
const maxConcurrentHashes = 8

// NewHasher builds a hasher over the current pepper and, during a key
// rotation, the previous one. A nil current pepper is ErrNoPepper.
func NewHasher(current, previous []byte) (*Hasher, error) {
	if len(current) == 0 {
		return nil, ErrNoPepper
	}
	h := &Hasher{cur: newPepper(current), params: DefaultParams, sem: make(chan struct{}, maxConcurrentHashes)}
	if len(previous) > 0 && !hmac.Equal(previous, current) {
		h.prev = newPepper(previous)
	}
	return h, nil
}

// PepperKID is the id of the pepper new hashes are keyed by.
func (h *Hasher) PepperKID() string { return h.cur.kid }

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

func (h *Hasher) digest(ctx context.Context, p pepper, password string, salt []byte, prm Params) ([]byte, error) {
	if err := h.acquire(ctx); err != nil {
		return nil, err
	}
	raw := argon2.IDKey([]byte(password), salt, prm.Time, prm.MemoryKiB, prm.Threads, keyLen)
	h.release()
	mac := hmac.New(sha256.New, p.key)
	mac.Write(raw)
	return mac.Sum(nil), nil
}

// Hash returns the PHC string for password under the current pepper and the
// pepper's id. password must already be normalised (Normalize).
func (h *Hasher) Hash(ctx context.Context, password string) (phc, kid string, err error) {
	if len(password) > MaxPasswordBytes {
		return "", "", ErrTooLong
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	d, err := h.digest(ctx, h.cur, password, salt, h.params)
	if err != nil {
		return "", "", err
	}
	return encodePHC(h.params, salt, d), h.cur.kid, nil
}

// Verify reports whether password matches the stored phc keyed by kid, and
// whether a match should be re-hashed (other parameters or an older pepper).
// An unparseable hash or an unknown pepper id is a mismatch, never an error a
// caller could turn into an oracle. The compare is constant-time.
func (h *Hasher) Verify(ctx context.Context, password, phc, kid string) (ok, rehash bool, err error) {
	p, known := h.pepperFor(kid)
	prm, salt, want, parsed := decodePHC(phc)
	if !known || !parsed || len(password) > MaxPasswordBytes {
		// Spend the same work as a real verify so a corrupt or foreign row
		// answers in the same time as a wrong password.
		return false, false, h.Dummy(ctx, password)
	}
	got, err := h.digest(ctx, p, password, salt, prm)
	if err != nil {
		return false, false, err
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, prm != h.params || p.kid != h.cur.kid, nil
}

// Dummy spends one verify's work on nothing: the login path calls it for an
// unknown login, a locked account or a user without a password, so none of
// them answers faster than a wrong password does.
func (h *Hasher) Dummy(ctx context.Context, password string) error {
	if len(password) > MaxPasswordBytes {
		password = password[:MaxPasswordBytes]
	}
	var salt [saltLen]byte
	_, err := h.digest(ctx, h.cur, password, salt[:], h.params)
	return err
}

func (h *Hasher) pepperFor(kid string) (pepper, bool) {
	switch {
	case kid == h.cur.kid:
		return h.cur, true
	case h.prev.kid != "" && kid == h.prev.kid:
		return h.prev, true
	}
	return pepper{}, false
}

// encodePHC renders the PHC string format argon2 tools use. The digest is
// peppered, so the string does not verify with a plain argon2 tool — by
// design.
func encodePHC(p Params, salt, digest []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(digest))
}

// Bounds on parameters read back from a stored hash: a tampered row must not
// be able to make a login allocate gigabytes or spin for minutes.
const (
	maxMemoryKiB = 256 * 1024
	maxTime      = 16
	maxThreads   = 16
)

func decodePHC(phc string) (Params, []byte, []byte, bool) {
	parts := strings.Split(phc, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, digest
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, false
	}
	var p Params
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Params{}, nil, nil, false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Params{}, nil, nil, false
		}
		switch k {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > maxThreads {
				return Params{}, nil, nil, false
			}
			p.Threads = uint8(n)
		default:
			return Params{}, nil, nil, false
		}
	}
	if p.MemoryKiB < 8 || p.MemoryKiB > maxMemoryKiB || p.Time < 1 || p.Time > maxTime || p.Threads < 1 {
		return Params{}, nil, nil, false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Params{}, nil, nil, false
	}
	digest, err := b64.DecodeString(parts[5])
	if err != nil || len(digest) != sha256.Size {
		return Params{}, nil, nil, false
	}
	return p, salt, digest, true
}

// MAC is HMAC-SHA256 under a sub-key of the current pepper, hex-encoded: the
// stored form of a recovery code, so a database read cannot brute-force a
// code's 50 bits offline. purpose separates the sub-keys.
func (h *Hasher) MAC(purpose, msg string) string { return macUnder(h.cur, purpose, msg) }

// MACs is msg's MAC under the current pepper and, during a key rotation, the
// previous one: a code stored before the rotation is still found.
func (h *Hasher) MACs(purpose, msg string) []string {
	out := []string{macUnder(h.cur, purpose, msg)}
	if h.prev.kid != "" {
		out = append(out, macUnder(h.prev, purpose, msg))
	}
	return out
}

func macUnder(p pepper, purpose, msg string) string {
	sub := hmac.New(sha256.New, p.key)
	sub.Write([]byte("loomcycle/password/" + purpose))
	mac := hmac.New(sha256.New, sub.Sum(nil))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
