// Package cryptoops is the pure algorithm layer behind the Crypto tool: it
// encrypts, decrypts, signs, verifies, MACs and hashes with key material the
// runtime holds, so an agent can use a key without the model ever seeing it.
//
// Everything here is deliberately policy-free: no loomcycle imports, no I/O, no
// logging and no package-level mutable state. Which key a caller may use, who
// owns it and where it is stored are decided a layer up; this package only
// answers "given these bytes, do the math correctly and fail closed".
//
// Two choices run through the whole package:
//
//   - The algorithm is fixed by the KeyType, never by caller input. A key minted
//     as hmac-sha256 cannot be talked into Ed25519, and a JWS header cannot pick
//     its own alg (the "alg confusion" / "none" attacks).
//   - Nonces are always generated here from crypto/rand. Callers never supply
//     one, because a reused AEAD nonce is a catastrophic, silent failure.
package cryptoops

import (
	"fmt"
	"slices"
	"strings"
)

// KeyType names an algorithm together with the shape of its key material. It
// is stored with every key, so the string values are a persisted contract —
// add new values, never rename existing ones.
type KeyType string

const (
	AES256GCM         KeyType = "aes-256-gcm"
	XChaCha20Poly1305 KeyType = "xchacha20-poly1305"
	HMACSHA256        KeyType = "hmac-sha256"
	HMACSHA512        KeyType = "hmac-sha512"
	Ed25519           KeyType = "ed25519"
	ECDSAP256         KeyType = "ecdsa-p256"
	TOTP              KeyType = "totp"
)

// allKeyTypes is the valid set in a stable order, used for error messages so
// an operator who typos a type sees every accepted spelling.
var allKeyTypes = []KeyType{AES256GCM, XChaCha20Poly1305, HMACSHA256, HMACSHA512, Ed25519, ECDSAP256, TOTP}

// ParseKeyType maps a user-supplied name to a KeyType. Surrounding space and
// case are forgiven (they carry no meaning); anything else unknown is an error
// that lists the valid set.
func ParseKeyType(s string) (KeyType, error) {
	k := KeyType(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(allKeyTypes, k) {
		return k, nil
	}
	names := make([]string, len(allKeyTypes))
	for i, t := range allKeyTypes {
		names[i] = string(t)
	}
	return "", fmt.Errorf("cryptoops: unknown key type %q (valid: %s)", s, strings.Join(names, ", "))
}

// Op names, shared with the tool layer so a grant can be checked against the
// same strings the dispatcher uses.
const (
	OpEncrypt   = "encrypt"
	OpDecrypt   = "decrypt"
	OpRewrap    = "rewrap"
	OpMAC       = "mac"
	OpVerifyMAC = "verify_mac"
	OpSign      = "sign"
	OpVerify    = "verify"
	OpPublicKey = "public_key"
	OpTOTP      = "totp"
)

// Ops lists the operations this key type supports. MAC types include sign and
// verify because they back HS256/HS512 JWS; raw Sign/Verify still refuse them.
// The result is a fresh slice the caller may modify.
func (k KeyType) Ops() []string {
	switch {
	case k.IsAEAD():
		return []string{OpEncrypt, OpDecrypt, OpRewrap}
	case k.IsMAC():
		return []string{OpMAC, OpVerifyMAC, OpSign, OpVerify}
	case k.IsSigning():
		return []string{OpSign, OpVerify, OpPublicKey}
	case k == TOTP:
		return []string{OpTOTP}
	}
	return nil
}

// Allows reports whether op is one of k's supported operations.
func (k KeyType) Allows(op string) bool {
	return slices.Contains(k.Ops(), op)
}

// IsAEAD reports whether k is an authenticated-encryption type.
func (k KeyType) IsAEAD() bool { return k == AES256GCM || k == XChaCha20Poly1305 }

// IsMAC reports whether k is an HMAC type.
func (k KeyType) IsMAC() bool { return k == HMACSHA256 || k == HMACSHA512 }

// IsSigning reports whether k is an asymmetric signature type.
func (k KeyType) IsSigning() bool { return k == Ed25519 || k == ECDSAP256 }
