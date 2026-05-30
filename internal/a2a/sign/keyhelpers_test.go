package sign

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
)

// sec1PEM encodes an EC private key in the SEC1 ("EC PRIVATE KEY") PEM
// form. Test-only helper for the key-parser round-trip.
func sec1PEM(k *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// pkcs8PEM encodes an EC private key in the PKCS#8 ("PRIVATE KEY") PEM
// form. Test-only helper for the key-parser round-trip.
func pkcs8PEM(k *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
