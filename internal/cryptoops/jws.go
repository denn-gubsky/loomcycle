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
	case RSAPSS2048, RSAPSS3072, RSAPSS4096:
		return "PS256", nil
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
	return signJWSWith(k, payload, kid, typJWT, func(signingInput []byte) ([]byte, error) {
		if k.IsMAC() {
			return MAC(k, key, signingInput)
		}
		return Sign(k, key, signingInput)
	})
}

// SignJWSWith is SignJWS for a key held elsewhere (an external KMS): sign
// returns the signature over the signing input — for a signing type the raw
// form Sign returns (r||s for ES256), for a MAC type the MAC.
func SignJWSWith(k KeyType, payload []byte, kid string, sign func(signingInput []byte) ([]byte, error)) (string, error) {
	return signJWSWith(k, payload, kid, false, sign)
}

func signJWSWith(k KeyType, payload []byte, kid string, typJWT bool, sign func([]byte) ([]byte, error)) (string, error) {
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
	sig, err := sign([]byte(signingInput))
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
	signingInput, body, sig, kid, ok := ParseJWS(k, token)
	if !ok {
		return nil, "", false
	}
	if k.IsMAC() {
		if !VerifyMAC(k, key, signingInput, sig) {
			return nil, "", false
		}
		return body, kid, true
	}
	if pub == nil {
		var err error
		if pub, err = PublicKeyOf(k, key); err != nil {
			return nil, "", false
		}
	}
	if !Verify(k, pub, signingInput, sig) {
		return nil, "", false
	}
	return body, kid, true
}

// ParseJWS reads a compact JWS made with a key of type k WITHOUT verifying it:
// the signing input, the payload, the signature and the kid. It applies
// VerifyJWS's header rules (alg must be k's, no crit). The payload is
// unverified; it is for a caller that checks sig itself (against a key held
// in an external KMS) and uses the payload only once that check passed.
func ParseJWS(k KeyType, token string) (signingInput, payload, sig []byte, kid string, ok bool) {
	wantAlg, err := jwsAlg(k)
	if err != nil {
		return nil, nil, nil, "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, nil, "", false
	}
	dec := base64.RawURLEncoding.Strict().DecodeString
	hb, err := dec(parts[0])
	if err != nil {
		return nil, nil, nil, "", false
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, nil, nil, "", false
	}
	if _, has := h["crit"]; has {
		return nil, nil, nil, "", false
	}
	var alg string
	if raw, has := h["alg"]; !has || json.Unmarshal(raw, &alg) != nil || alg != wantAlg {
		return nil, nil, nil, "", false
	}
	if raw, has := h["kid"]; has {
		if json.Unmarshal(raw, &kid) != nil {
			return nil, nil, nil, "", false
		}
	}
	body, err := dec(parts[1])
	if err != nil {
		return nil, nil, nil, "", false
	}
	sig, err = dec(parts[2])
	if err != nil {
		return nil, nil, nil, "", false
	}
	return []byte(parts[0] + "." + parts[1]), body, sig, kid, true
}
