package kms

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// send performs req under the default timeout and returns the status and a
// bounded body. A transport failure comes back as an ErrUnavailable opError;
// a non-2xx status is returned for the caller to classify (only the caller
// knows whether a 400 means "does not decrypt").
func (b *base) send(ctx context.Context, op string, req *http.Request) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, err := b.http.Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, &opError{kind: b.kind, op: op, sentinel: ErrUnavailable, cause: scrubURLError(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return 0, nil, &opError{kind: b.kind, op: op, sentinel: ErrUnavailable, detail: "reading the response failed", status: resp.StatusCode}
	}
	if len(body) > maxResponse {
		return 0, nil, &opError{kind: b.kind, op: op, sentinel: ErrUnavailable, detail: "response too large", status: resp.StatusCode}
	}
	return resp.StatusCode, body, nil
}

// scrubURLError drops the URL from a *url.Error-shaped message. The URLs this
// package builds carry no secrets, but a test override or a future query
// parameter might; the operation name already says what was being called.
func scrubURLError(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok && u.Unwrap() != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return u.Unwrap()
	}
	return err
}

// newJSONRequest builds a request with a JSON body (nil body for GET).
func newJSONRequest(method, url string, body []byte, contentType string) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// --- ECDSA signature encodings ---------------------------------------------

// p256SigSize is the raw r||s form loomcycle uses for ECDSA P-256 (IEEE P1363,
// as JWS ES256): two 32-byte big-endian integers.
const p256SigSize = 64

type ecdsaDER struct{ R, S *big.Int }

// derToRaw converts an ASN.1 DER ECDSA-Sig-Value (what AWS, GCP and Vault's
// default marshaling return) to loomcycle's 64-byte r||s.
func derToRaw(der []byte) ([]byte, error) {
	var sig ecdsaDER
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil || len(rest) != 0 || sig.R == nil || sig.S == nil {
		return nil, errors.New("malformed ECDSA signature")
	}
	if sig.R.Sign() <= 0 || sig.S.Sign() <= 0 || sig.R.BitLen() > 256 || sig.S.BitLen() > 256 {
		return nil, errors.New("ECDSA signature out of range for P-256")
	}
	out := make([]byte, p256SigSize)
	sig.R.FillBytes(out[:32])
	sig.S.FillBytes(out[32:])
	return out, nil
}

// rawToDER converts a 64-byte r||s to ASN.1 DER for a KMS that verifies DER.
func rawToDER(raw []byte) ([]byte, error) {
	if len(raw) != p256SigSize {
		return nil, errors.New("ECDSA P-256 signature must be 64 bytes (r||s)")
	}
	r := new(big.Int).SetBytes(raw[:32])
	s := new(big.Int).SetBytes(raw[32:])
	if r.Sign() == 0 || s.Sign() == 0 {
		return nil, errors.New("ECDSA signature has a zero component")
	}
	return asn1.Marshal(ecdsaDER{R: r, S: s})
}

// --- public keys -------------------------------------------------------------

// parsePublicPEM reads a PKIX "PUBLIC KEY" PEM block.
func parsePublicPEM(s string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(s)))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PUBLIC KEY PEM block")
	}
	return x509.ParsePKIXPublicKey(block.Bytes)
}

// checkPublic reports whether pub is the kind of key keyType implies — curve,
// algorithm and RSA modulus size — so a KMS can't hand back a different key.
func checkPublic(keyType string, pub crypto.PublicKey) bool {
	switch pk := pub.(type) {
	case *ecdsa.PublicKey:
		return keyType == ECDSAP256 && pk != nil && pk.Curve == elliptic.P256()
	case ed25519.PublicKey:
		return keyType == Ed25519 && len(pk) == ed25519.PublicKeySize
	case *rsa.PublicKey:
		return pk != nil && rsaBits(keyType) != 0 && pk.N.BitLen() == rsaBits(keyType)
	}
	return false
}

// verifyLocal checks a loomcycle-format signature against a public key. It is
// used where the public key is all verification needs (and a KMS round trip
// per version would only add latency).
func verifyLocal(keyType string, pub crypto.PublicKey, data, sig []byte) bool {
	if !checkPublic(keyType, pub) {
		return false
	}
	switch {
	case keyType == Ed25519:
		return len(sig) == ed25519.SignatureSize && ed25519.Verify(pub.(ed25519.PublicKey), data, sig)
	case keyType == ECDSAP256:
		if len(sig) != p256SigSize {
			return false
		}
		d := sha256.Sum256(data)
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub.(*ecdsa.PublicKey), d[:], r, s)
	case isRSAPSS(keyType):
		d := sha256.Sum256(data)
		// Auto accepts any salt length: AWS and GCP use the hash length; a
		// Vault too old to honour salt_length uses the maximum.
		return rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, d[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto}) == nil
	}
	return false
}
