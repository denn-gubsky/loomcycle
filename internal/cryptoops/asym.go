package cryptoops

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
)

const (
	x25519KeySize = 32
	// rsaExponent is the only public exponent accepted. Everything in common
	// use is 65537; refusing the rest (3 in particular) keeps a whole class of
	// small-exponent mistakes out without costing interop.
	rsaExponent = 65537
)

// parseRSAPrivate reads canonical RSA material (PKCS#8 DER) and checks it is
// exactly what k names: an RSA key of k's modulus size with e = 65537 that
// passes the stdlib's consistency check.
func parseRSAPrivate(k KeyType, der []byte) (*rsa.PrivateKey, error) {
	bits := k.rsaBits()
	if bits == 0 {
		return nil, unknownType(k)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("cryptoops: %s key must be a PKCS#8 RSA private key", k)
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("cryptoops: %s key holds a %T, not an RSA key", k, key)
	}
	if err := checkRSAPrivate(k, priv); err != nil {
		return nil, err
	}
	return priv, nil
}

func checkRSAPrivate(k KeyType, priv *rsa.PrivateKey) error {
	if got := rsaPublicBits(&priv.PublicKey); got != k.rsaBits() {
		if priv.E != rsaExponent {
			return fmt.Errorf("cryptoops: %s key must use public exponent %d", k, rsaExponent)
		}
		return fmt.Errorf("cryptoops: %s key must have a %d-bit modulus, got %d", k, k.rsaBits(), priv.N.BitLen())
	}
	if err := priv.Validate(); err != nil {
		return fmt.Errorf("cryptoops: %s key is not a consistent RSA key", k)
	}
	return nil
}

// normalizeRSADER turns PKCS#8 or PKCS#1 DER into canonical PKCS#8 DER for k,
// so a stored RSA key has one shape whichever form it was handed in.
func normalizeRSADER(k KeyType, der []byte) ([]byte, error) {
	var priv *rsa.PrivateKey
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		p, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("DER holds a %T, not an RSA key", key)
		}
		priv = p
	} else if p, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		priv = p
	} else {
		return nil, errors.New("DER is not a PKCS#8 or PKCS#1 RSA private key")
	}
	return marshalRSA(k, priv)
}

func marshalRSA(k KeyType, priv *rsa.PrivateKey) ([]byte, error) {
	if err := checkRSAPrivate(k, priv); err != nil {
		return nil, err
	}
	return x509.MarshalPKCS8PrivateKey(priv)
}

// rsaPublicBits is the modulus size of an RSA public key this package accepts
// (2048, 3072 or 4096 bits with e = 65537), or 0 for any other.
func rsaPublicBits(pk *rsa.PublicKey) int {
	if pk == nil || pk.N == nil || pk.E != rsaExponent {
		return 0
	}
	switch b := pk.N.BitLen(); b {
	case 2048, 3072, 4096:
		return b
	}
	return 0
}

// rsaFamily stands for "an acceptable RSA public key" where the key alone
// cannot say whether it verifies (PSS) or encrypts (OAEP). It is never a
// stored type and ParseKeyType does not accept it.
const rsaFamily KeyType = "rsa"

// checkPublic narrows pub to a supported kind and reports its type, or
// rsaFamily for an RSA key.
func checkPublic(pub crypto.PublicKey) (KeyType, error) {
	switch pk := pub.(type) {
	case ed25519.PublicKey:
		if len(pk) == ed25519.PublicKeySize {
			return Ed25519, nil
		}
	case *ecdsa.PublicKey:
		if pk != nil && pk.Curve == elliptic.P256() {
			return ECDSAP256, nil
		}
	case *ecdh.PublicKey:
		if pk != nil && pk.Curve() == ecdh.X25519() {
			return X25519, nil
		}
	case *rsa.PublicKey:
		if rsaPublicBits(pk) != 0 {
			return rsaFamily, nil
		}
	}
	return "", fmt.Errorf("cryptoops: unsupported public key %T (want Ed25519, ECDSA P-256, X25519, or RSA 2048/3072/4096 with e=65537)", pub)
}

// publicMatches reports whether pub is a public key of exactly type k — for an
// RSA type, of exactly k's modulus size.
func publicMatches(k KeyType, pub crypto.PublicKey) bool {
	kt, err := checkPublic(pub)
	if err != nil {
		return false
	}
	if kt == rsaFamily {
		return k.rsaBits() != 0 && rsaPublicBits(pub.(*rsa.PublicKey)) == k.rsaBits()
	}
	return kt == k
}
