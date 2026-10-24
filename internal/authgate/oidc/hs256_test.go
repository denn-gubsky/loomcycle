package oidc

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/denn-gubsky/loomcycle/internal/authgate/oidc/oidctest"
)

// hs256WithPublicKey forges the classic algorithm-confusion token: HS256 whose
// HMAC key is the provider's published RSA public key.
func hs256WithPublicKey(p *oidctest.Provider) string {
	pub := &p.Signing.Priv.(*rsa.PrivateKey).PublicKey
	der, _ := x509.MarshalPKIXPublicKey(pub)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: jose.JSONWebKey{Key: der, KeyID: p.Signing.ID}}, nil)
	if err != nil {
		panic(err)
	}
	body, _ := json.Marshal(map[string]any{"iss": p.Issuer, "aud": p.ClientID, "sub": "u-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
	obj, _ := s.Sign(body)
	out, _ := obj.CompactSerialize()
	return out
}
