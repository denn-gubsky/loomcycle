package config

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// TokenHasher builds the hasher every operator-token mint and lookup uses
// (RFC ES D2): a pepper derived from LOOMCYCLE_SECRET_KEY if set, else the
// explicit LOOMCYCLE_OPERATOR_TOKEN_PEPPER, else none. A malformed secret key
// is an error rather than a silent fall back to no pepper — the caller fails
// closed.
//
// LOOMCYCLE_OPERATOR_TOKEN_PEPPER is legacy (RFC GN). With
// LOOMCYCLE_SECRET_KEY also set, it is kept for lookups only: its rows log in
// and move onto the derived pepper at their next login, and the variable can
// go once none is left (the boot log counts them). Alone, it still hashes as
// before, so an instance without a secret key keeps working until it sets one.
func (e *Env) TokenHasher() (auth.TokenHasher, error) {
	cur, err := credential.DeriveTokenPepper(e.SecretKey)
	if err != nil {
		return auth.TokenHasher{}, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
	}
	if cur == "" && e.OperatorTokenPepper != "" {
		return auth.NewTokenHasher(e.OperatorTokenPepper, "", ""), nil
	}
	prev, err := credential.DeriveTokenPepper(e.SecretKeyPrevious)
	if err != nil {
		return auth.TokenHasher{}, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
	}
	return auth.NewTokenHasher("", cur, prev).WithLegacy(e.OperatorTokenPepper), nil
}
