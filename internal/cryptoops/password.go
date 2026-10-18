package cryptoops

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing with Argon2id (RFC 9106), stored as a PHC string:
//
//	$argon2id$v=19$m=<KiB>,t=<passes>,p=<threads>$<salt b64>$<hash b64>
//
// A hash is something to keep, never a key: it is not a KeyType and needs no
// key. The parameters are OWASP's first recommendation (m=19 MiB, t=2, p=1).
// A stored hash names its own parameters, so verify bounds them: a hash a
// caller hands in could otherwise ask for gigabytes of memory per call.

const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonHashLen   = 32

	// Bounds on the parameters a hash being verified may name.
	argonMaxMemoryKiB = 64 * 1024
	argonMaxTime      = 10
	argonMaxThreads   = 4
	argonMinSalt      = 8
	argonMaxSalt      = 64
	argonMinHash      = 16
	argonMaxHash      = 64

	// MaxPasswordBytes caps a password: Argon2 accepts any length, but a
	// password longer than this is input abuse, not a password.
	MaxPasswordBytes = 1024
)

// HashPassword returns the Argon2id PHC string of pw, with a fresh random salt.
func HashPassword(pw []byte) (string, error) {
	if len(pw) == 0 || len(pw) > MaxPasswordBytes {
		return "", fmt.Errorf("cryptoops: a password must be 1 to %d bytes", MaxPasswordBytes)
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey(pw, salt, argonTime, argonMemoryKiB, argonThreads, argonHashLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, b64(salt), b64(sum)), nil
}

// VerifyPassword reports whether pw matches the PHC string phc, comparing in
// constant time. A malformed hash, or one naming parameters outside the bounds
// above, is an error rather than false: it says something is wrong with what
// was stored, not with the password.
func VerifyPassword(pw []byte, phc string) (bool, error) {
	if len(pw) > MaxPasswordBytes {
		return false, fmt.Errorf("cryptoops: a password must be at most %d bytes", MaxPasswordBytes)
	}
	p, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	sum := argon2.IDKey(pw, p.salt, p.time, p.memory, p.threads, uint32(len(p.hash)))
	return subtle.ConstantTimeCompare(sum, p.hash) == 1, nil
}

type phcParams struct {
	memory, time uint32
	threads      uint8
	salt, hash   []byte
}

func parsePHC(s string) (phcParams, error) {
	bad := errors.New("cryptoops: not an Argon2id PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$hash)")
	f := strings.Split(s, "$")
	if len(f) != 6 || f[0] != "" || f[1] != "argon2id" || f[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return phcParams{}, bad
	}
	var m, t, par uint32
	if n, err := fmt.Sscanf(f[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil || n != 3 ||
		f[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, par) {
		return phcParams{}, bad
	}
	if m < 8*par || m > argonMaxMemoryKiB || t < 1 || t > argonMaxTime || par < 1 || par > argonMaxThreads {
		return phcParams{}, fmt.Errorf("cryptoops: the hash's parameters are outside the accepted bounds (m ≤ %d KiB, t 1..%d, p 1..%d)",
			argonMaxMemoryKiB, argonMaxTime, argonMaxThreads)
	}
	dec := base64.RawStdEncoding.Strict().DecodeString
	salt, err1 := dec(f[4])
	hash, err2 := dec(f[5])
	if err1 != nil || err2 != nil || len(salt) < argonMinSalt || len(salt) > argonMaxSalt ||
		len(hash) < argonMinHash || len(hash) > argonMaxHash {
		return phcParams{}, bad
	}
	return phcParams{memory: m, time: t, threads: uint8(par), salt: salt, hash: hash}, nil
}
