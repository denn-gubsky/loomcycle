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

	// Asymmetric encryption ("recipient") types: anyone holding the public key
	// can encrypt to them, only the private key decrypts.
	X25519      KeyType = "x25519"
	RSAOAEP2048 KeyType = "rsa-oaep-2048"
	RSAOAEP3072 KeyType = "rsa-oaep-3072"
	RSAOAEP4096 KeyType = "rsa-oaep-4096"

	// RSA signature types. The purpose is part of the type: one RSA key either
	// signs (PSS) or decrypts (OAEP), never both, so a decryption oracle can
	// never be turned into a signing oracle or the reverse.
	RSAPSS2048 KeyType = "rsa-pss-2048"
	RSAPSS3072 KeyType = "rsa-pss-3072"
	RSAPSS4096 KeyType = "rsa-pss-4096"
)

// allKeyTypes is the valid set in a stable order, used for error messages so
// an operator who typos a type sees every accepted spelling.
var allKeyTypes = []KeyType{
	AES256GCM, XChaCha20Poly1305, HMACSHA256, HMACSHA512, Ed25519, ECDSAP256, TOTP,
	X25519, RSAOAEP2048, RSAOAEP3072, RSAOAEP4096, RSAPSS2048, RSAPSS3072, RSAPSS4096,
}

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
	case k.IsRecipient():
		return []string{OpEncrypt, OpDecrypt, OpPublicKey}
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

// IsSigning reports whether k is an asymmetric signature type: Ed25519, ECDSA
// P-256 or RSA-PSS. Its material is not always a 32-byte seed — an RSA-PSS key
// is PKCS#8 DER — so callers must not assume a shape from this predicate.
func (k KeyType) IsSigning() bool { return k == Ed25519 || k == ECDSAP256 || k.isRSAPSS() }

// IsRecipient reports whether k is an asymmetric encryption type (X25519 or
// RSA-OAEP): it encrypts to its public key and decrypts with its private key,
// always as a compact JWE — never through Seal/Open, which are AEAD-only.
func (k KeyType) IsRecipient() bool { return k == X25519 || k.isRSAOAEP() }

func (k KeyType) isRSAOAEP() bool { return k == RSAOAEP2048 || k == RSAOAEP3072 || k == RSAOAEP4096 }

func (k KeyType) isRSAPSS() bool { return k == RSAPSS2048 || k == RSAPSS3072 || k == RSAPSS4096 }

// rsaBits is the exact modulus size an RSA type fixes, or 0 for any other type.
func (k KeyType) rsaBits() int {
	switch k {
	case RSAOAEP2048, RSAPSS2048:
		return 2048
	case RSAOAEP3072, RSAPSS3072:
		return 3072
	case RSAOAEP4096, RSAPSS4096:
		return 4096
	}
	return 0
}

// rsaPSSType and rsaOAEPType name the RSA type of a modulus size, or "" for a
// size this package does not accept.
func rsaPSSType(bits int) KeyType {
	return map[int]KeyType{2048: RSAPSS2048, 3072: RSAPSS3072, 4096: RSAPSS4096}[bits]
}

func rsaOAEPType(bits int) KeyType {
	return map[int]KeyType{2048: RSAOAEP2048, 3072: RSAOAEP3072, 4096: RSAOAEP4096}[bits]
}
