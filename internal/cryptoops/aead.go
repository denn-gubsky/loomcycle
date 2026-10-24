package cryptoops

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strconv"

	"golang.org/x/crypto/chacha20poly1305"
)

// ErrDecrypt is the single, opaque failure of Open: wrong key, wrong AAD,
// tampered or truncated input and malformed arguments all look identical, so a
// caller probing with crafted inputs learns nothing about which check failed.
var ErrDecrypt = errors.New("cryptoops: did not decrypt")

// newAEAD builds the cipher for k. The key length is checked here rather than
// left to the cipher constructors, because aes.NewCipher would happily accept a
// 16- or 24-byte key and quietly give AES-128/192 under an aes-256-gcm name.
func newAEAD(k KeyType, key []byte) (cipher.AEAD, error) {
	if !k.IsAEAD() {
		return nil, unknownType(k)
	}
	if err := ValidateMaterial(k, key); err != nil {
		return nil, err
	}
	if k == XChaCha20Poly1305 {
		return chacha20poly1305.NewX(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext under key, authenticating aad, and returns
// nonce||ciphertext||tag with a fresh random nonce (12 bytes for AES-256-GCM,
// 24 for XChaCha20-Poly1305). A random 96-bit GCM nonce is safe for well under
// 2^32 messages per key; callers expecting more should prefer XChaCha20.
func Seal(k KeyType, key, plaintext, aad []byte) ([]byte, error) {
	a, err := newAEAD(k, key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, plaintext, aad), nil
}

// sealWithNonce is Seal with a caller-chosen nonce. It exists only so tests can
// check published known-answer vectors; production code must never choose a
// nonce, which is why it is unexported.
func sealWithNonce(k KeyType, key, nonce, plaintext, aad []byte) ([]byte, error) {
	a, err := newAEAD(k, key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != a.NonceSize() {
		return nil, errors.New("cryptoops: nonce must be " + strconv.Itoa(a.NonceSize()) + " bytes")
	}
	return a.Seal(append([]byte(nil), nonce...), nonce, plaintext, aad), nil
}

// Open reverses Seal. Every failure returns ErrDecrypt and nothing else.
func Open(k KeyType, key, sealed, aad []byte) ([]byte, error) {
	a, err := newAEAD(k, key)
	if err != nil {
		return nil, ErrDecrypt
	}
	ns := a.NonceSize()
	if len(sealed) < ns+a.Overhead() {
		return nil, ErrDecrypt
	}
	pt, err := a.Open(nil, sealed[:ns], sealed[ns:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// aadPrefix versions the AAD layout; changing the layout means a new prefix,
// so old ciphertexts keep opening under the old rule.
const aadPrefix = "loomcycle/crypto/v1"

// AAD builds the associated data that binds a ciphertext to where its key lives
// (tenant, source, key name and version) plus an optional caller context. The
// tool and the operator API both use this one function, so a value sealed by
// one opens with the other — and a ciphertext moved to another tenant or key
// fails to open.
//
// Fields are joined with 0x1f (ASCII unit separator). context is last, so a
// 0x1f inside it cannot shift any other field; tenant, source and keyName are
// expected to be validated identifiers that never contain 0x1f.
func AAD(tenant, source, keyName string, version int, context string) []byte {
	const sep = 0x1f
	b := append([]byte(nil), aadPrefix...)
	b = append(b, sep)
	b = append(b, tenant...)
	b = append(b, sep)
	b = append(b, source...)
	b = append(b, sep)
	b = append(b, keyName...)
	b = append(b, sep)
	b = strconv.AppendInt(b, int64(version), 10)
	b = append(b, sep)
	b = append(b, context...)
	return b
}
