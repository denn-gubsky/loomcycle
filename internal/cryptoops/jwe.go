package cryptoops

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JWE compact serialization (RFC 7516) for the three things a JWE is used for
// here: a symmetric key (alg "dir"), an X25519 recipient (alg "ECDH-ES",
// RFC 8037) and an RSA recipient (alg "RSA-OAEP-256"). The content encryption
// is always A256GCM. As with JWS, the algorithm comes from the key type, never
// from the token: decrypt computes the one alg its key allows and refuses any
// token naming another, plus any zip or crit header, so a token cannot steer
// how it is opened.

const (
	jweEnc       = "A256GCM"
	jweCEKSize   = 32
	jweIVSize    = 12
	jweTagSize   = 16
	jweAlgDir    = "dir"
	jweAlgECDHES = "ECDH-ES"
	jweAlgRSA    = "RSA-OAEP-256"
)

// jweAlg is the one JWE alg each key type may use.
func jweAlg(k KeyType) (string, error) {
	switch {
	case k == AES256GCM:
		return jweAlgDir, nil
	case k == X25519:
		return jweAlgECDHES, nil
	case k.isRSAOAEP():
		return jweAlgRSA, nil
	}
	return "", fmt.Errorf("cryptoops: %s has no JWE algorithm (JWE needs aes-256-gcm, x25519 or rsa-oaep-*)", k)
}

// jweHeader marshals with a fixed field order, so the same inputs give the
// same header bytes.
type jweHeader struct {
	Alg string         `json:"alg"`
	Enc string         `json:"enc"`
	Kid string         `json:"kid,omitempty"`
	Epk map[string]any `json:"epk,omitempty"`
}

// EncryptJWEDirect seals plaintext under a 32-byte aes-256-gcm key as a JWE
// with alg "dir": the key itself is the content encryption key.
func EncryptJWEDirect(key, plaintext []byte, kid string) (string, error) {
	if err := ValidateMaterial(AES256GCM, key); err != nil {
		return "", err
	}
	return sealJWE(jweHeader{Alg: jweAlgDir, Enc: jweEnc, Kid: kid}, nil, key, plaintext)
}

// EncryptJWETo seals plaintext to a recipient's public key: an X25519 key
// (ECDH-ES, an ephemeral key per token) or an RSA key (RSA-OAEP-256 wrapping a
// random content key). k is the recipient type, which pub must match exactly.
func EncryptJWETo(k KeyType, pub crypto.PublicKey, plaintext []byte, kid string) (string, error) {
	if !k.IsRecipient() || !publicMatches(k, pub) {
		return "", fmt.Errorf("cryptoops: cannot encrypt to a %T as %s", pub, k)
	}
	if k == X25519 {
		eph, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		z, err := eph.ECDH(pub.(*ecdh.PublicKey))
		if err != nil {
			return "", errors.New("cryptoops: X25519 key agreement failed")
		}
		cek := concatKDF(z, jweEnc, jweCEKSize)
		epk := map[string]any{"kty": "OKP", "crv": "X25519", "x": base64.RawURLEncoding.EncodeToString(eph.PublicKey().Bytes())}
		return sealJWE(jweHeader{Alg: jweAlgECDHES, Enc: jweEnc, Kid: kid, Epk: epk}, nil, cek, plaintext)
	}
	cek := make([]byte, jweCEKSize)
	if _, err := rand.Read(cek); err != nil {
		return "", err
	}
	ek, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), cek, nil)
	if err != nil {
		return "", errors.New("cryptoops: RSA-OAEP key wrap failed")
	}
	return sealJWE(jweHeader{Alg: jweAlgRSA, Enc: jweEnc, Kid: kid}, ek, cek, plaintext)
}

func sealJWE(h jweHeader, encryptedKey, cek, plaintext []byte) (string, error) {
	hb, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	protected := b64(hb)
	gcm, err := newJWEGCM(cek)
	if err != nil {
		return "", err
	}
	iv := make([]byte, jweIVSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, plaintext, []byte(protected))
	ct, tag := sealed[:len(sealed)-jweTagSize], sealed[len(sealed)-jweTagSize:]
	return protected + "." + b64(encryptedKey) + "." + b64(iv) + "." + b64(ct) + "." + b64(tag), nil
}

// IsJWE reports whether s has the shape of a compact JWE (five dot-separated
// parts, the first a JSON object). It says nothing about whether it decrypts.
func IsJWE(s string) bool {
	parts := strings.Split(s, ".")
	return len(parts) == 5 && strings.HasPrefix(parts[0], "eyJ")
}

// JWEKid returns the kid a compact JWE's header names, or "". It is read
// before decryption, so it is only a hint for choosing a key version: a token
// whose header was altered fails the AEAD tag, which covers the header.
func JWEKid(token string) string {
	h, _, ok := jweParseHeader(token)
	if !ok {
		return ""
	}
	return h.Kid
}

// DecryptJWE opens a compact JWE with held key material for k (a 32-byte
// aes-256-gcm key, an X25519 private scalar, or an RSA-OAEP PKCS#8 key) and
// returns the plaintext and the header's kid. Any failure is ok == false with
// no detail, so the function is no oracle for which part was wrong.
func DecryptJWE(k KeyType, key []byte, token string) (plaintext []byte, kid string, ok bool) {
	wantAlg, err := jweAlg(k)
	if err != nil {
		return nil, "", false
	}
	h, parts, ok := jweParseHeader(token)
	if !ok || h.Alg != wantAlg || h.Enc != jweEnc {
		return nil, "", false
	}
	dec := base64.RawURLEncoding.Strict().DecodeString
	ek, err1 := dec(parts[1])
	iv, err2 := dec(parts[2])
	ct, err3 := dec(parts[3])
	tag, err4 := dec(parts[4])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || len(iv) != jweIVSize || len(tag) != jweTagSize {
		return nil, "", false
	}
	var cek []byte
	switch h.Alg {
	case jweAlgDir:
		if len(ek) != 0 || ValidateMaterial(AES256GCM, key) != nil {
			return nil, "", false
		}
		cek = key
	case jweAlgECDHES:
		if len(ek) != 0 {
			return nil, "", false
		}
		priv, err := ecdh.X25519().NewPrivateKey(key)
		if err != nil {
			return nil, "", false
		}
		epk, ok := parseEPK(h.Epk)
		if !ok {
			return nil, "", false
		}
		z, err := priv.ECDH(epk) // refuses a low-order point (all-zero secret)
		if err != nil {
			return nil, "", false
		}
		cek = concatKDF(z, jweEnc, jweCEKSize)
	case jweAlgRSA:
		priv, err := parseRSAPrivate(k, key)
		if err != nil || len(ek) != priv.Size() {
			return nil, "", false
		}
		// RFC 7516 §11.5: on an unwrap failure carry on with a random key, so
		// the time taken does not say whether the padding was valid.
		cek, err = rsa.DecryptOAEP(sha256.New(), nil, priv, ek, nil)
		if err != nil || len(cek) != jweCEKSize {
			cek = make([]byte, jweCEKSize)
			_, _ = rand.Read(cek)
		}
	default:
		return nil, "", false
	}
	gcm, err := newJWEGCM(cek)
	if err != nil {
		return nil, "", false
	}
	pt, err := gcm.Open(nil, iv, append(ct, tag...), []byte(parts[0]))
	if err != nil {
		return nil, "", false
	}
	return pt, h.Kid, true
}

// jweParseHeader splits a compact JWE and reads its protected header, refusing
// a header with zip or crit and any non-canonical encoding.
func jweParseHeader(token string) (jweHeader, []string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return jweHeader{}, nil, false
	}
	hb, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return jweHeader{}, nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(hb, &raw); err != nil {
		return jweHeader{}, nil, false
	}
	if _, has := raw["zip"]; has {
		return jweHeader{}, nil, false
	}
	if _, has := raw["crit"]; has {
		return jweHeader{}, nil, false
	}
	var h jweHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return jweHeader{}, nil, false
	}
	return h, parts, true
}

func parseEPK(epk map[string]any) (*ecdh.PublicKey, bool) {
	kty, _ := epk["kty"].(string)
	crv, _ := epk["crv"].(string)
	x, _ := epk["x"].(string)
	if kty != "OKP" || crv != "X25519" {
		return nil, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(x)
	if err != nil || len(b) != x25519KeySize {
		return nil, false
	}
	pub, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		return nil, false
	}
	return pub, true
}

func newJWEGCM(cek []byte) (cipher.AEAD, error) {
	if len(cek) != jweCEKSize {
		return nil, errors.New("cryptoops: JWE content key must be 32 bytes")
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// concatKDF is the single-step KDF of NIST SP 800-56A §5.8.1 as JWA ECDH-ES
// uses it (RFC 7518 §4.6.2), with SHA-256, empty PartyUInfo and PartyVInfo,
// and algorithmID the "enc" value (key agreement in direct mode).
func concatKDF(z []byte, algorithmID string, keyLen int) []byte {
	return concatKDFParties(z, algorithmID, nil, nil, keyLen)
}

// concatKDFParties is concatKDF with PartyUInfo / PartyVInfo, which this
// package always leaves empty; it exists so the RFC 7518 Appendix C vector
// (which names both parties) checks the construction.
func concatKDFParties(z []byte, algorithmID string, apu, apv []byte, keyLen int) []byte {
	var other []byte
	other = appendLenPrefixed(other, []byte(algorithmID))
	other = appendLenPrefixed(other, apu)
	other = appendLenPrefixed(other, apv)
	other = binary.BigEndian.AppendUint32(other, uint32(keyLen*8))
	var out []byte
	for counter := uint32(1); len(out) < keyLen; counter++ {
		h := sha256.New()
		_ = binary.Write(h, binary.BigEndian, counter)
		h.Write(z)
		h.Write(other)
		out = h.Sum(out)
	}
	return out[:keyLen]
}

func appendLenPrefixed(b, data []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(data)))
	return append(b, data...)
}
