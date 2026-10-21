package config

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// TokenHasher builds the hasher every operator-token mint and lookup uses
// (RFC ES D2): the explicit LOOMCYCLE_OPERATOR_TOKEN_PEPPER if set, else a
// pepper derived from LOOMCYCLE_SECRET_KEY, else none. A malformed secret key
// is an error rather than a silent fall back to no pepper — the caller fails
// closed.
func (e *Env) TokenHasher() (auth.TokenHasher, error) {
	if e.OperatorTokenPepper != "" {
		return auth.NewTokenHasher(e.OperatorTokenPepper, "", ""), nil
	}
	cur, err := credential.DeriveTokenPepper(e.SecretKey)
	if err != nil {
		return auth.TokenHasher{}, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
	}
	prev, err := credential.DeriveTokenPepper(e.SecretKeyPrevious)
	if err != nil {
		return auth.TokenHasher{}, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
	}
	return auth.NewTokenHasher("", cur, prev), nil
}
