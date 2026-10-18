package cryptoops

import (
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// jwsAlg is the one JWS alg each key type may use. The token header never
// chooses the algorithm: verify recomputes the expected alg from the key type
// and rejects anything else, which closes "alg":"none" and the classic
// HS256-with-a-public-key confusion.
func jwsAlg(k KeyType) (string, error) {
	switch k {
	case HMACSHA256:
		return "HS256", nil
	case HMACSHA512:
		return "HS512", nil
	case Ed25519:
		return "EdDSA", nil
	case ECDSAP256:
		return "ES256", nil
	}
	return "", fmt.Errorf("cryptoops: %s cannot sign a JWS", k)
}

// jwsHeader marshals with a fixed field order, so the same inputs always yield
// byte-identical tokens.
type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
	Typ string `json:"typ,omitempty"`
}

// SignJWS returns a compact JWS (RFC 7515) over payload with header
// {"alg","kid"}. For a JWT, use SignJWT, which adds "typ":"JWT".
func SignJWS(k KeyType, key []byte, payload []byte, kid string) (string, error) {
	return signJWS(k, key, payload, kid, false)
}

// SignJWT is SignJWS with "typ":"JWT" in the header, for consumers that check
// it. The payload is signed as given; this package does not interpret claims.
func SignJWT(k KeyType, key []byte, payload []byte, kid string) (string, error) {
	return signJWS(k, key, payload, kid, true)
}

func signJWS(k KeyType, key, payload []byte, kid string, typJWT bool) (string, error) {
	alg, err := jwsAlg(k)
	if err != nil {
		return "", err
	}
	h := jwsHeader{Alg: alg, Kid: kid}
	if typJWT {
		h.Typ = "JWT"
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	signingInput := b64(hb) + "." + b64(payload)
	var sig []byte
	if k.IsMAC() {
		sig, err = MAC(k, key, []byte(signingInput))
	} else {
		sig, err = Sign(k, key, []byte(signingInput))
	}
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64(sig), nil
}

// VerifyJWS checks a compact JWS and returns its payload and kid. For MAC
// types pass the secret as key; for signing types pass pub (or, if pub is nil,
// the private key material as key, from which the public key is derived).
//
// Only "alg" (which must equal the key type's alg), "kid" and "typ" are read
// from the header. A "crit" member is rejected, as RFC 7515 §4.1.11 requires
// for extensions the verifier does not implement. Any failure is ok == false
// with no payload, so an unverified payload can never be used by mistake.
func VerifyJWS(k KeyType, key []byte, pub crypto.PublicKey, token string) (payload []byte, kid string, ok bool) {
	wantAlg, err := jwsAlg(k)
	if err != nil {
		return nil, "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, "", false
	}
	dec := base64.RawURLEncoding.Strict().DecodeString
	hb, err := dec(parts[0])
	if err != nil {
		return nil, "", false
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, "", false
	}
	if _, has := h["crit"]; has {
		return nil, "", false
	}
	var alg string
	if raw, has := h["alg"]; !has || json.Unmarshal(raw, &alg) != nil || alg != wantAlg {
		return nil, "", false
	}
	if raw, has := h["kid"]; has {
		if json.Unmarshal(raw, &kid) != nil {
			return nil, "", false
		}
	}
	body, err := dec(parts[1])
	if err != nil {
		return nil, "", false
	}
	sig, err := dec(parts[2])
	if err != nil {
		return nil, "", false
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	if k.IsMAC() {
		if !VerifyMAC(k, key, signingInput, sig) {
			return nil, "", false
		}
		return body, kid, true
	}
	if pub == nil {
		if pub, err = PublicKeyOf(k, key); err != nil {
			return nil, "", false
		}
	}
	if !Verify(k, pub, signingInput, sig) {
		return nil, "", false
	}
	return body, kid, true
}
