package http

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// authConfiguredUnder is authConfiguredErr against a given config, so a config
// reload can ask what the candidate would decide before it is swapped in.
//
// Principals count whether or not their secret resolved: an inert declaration
// still says the operator meant auth to be on, and reading it as open mode
// would serve every route to anyone because one env var was unset.
func (s *Server) authConfiguredUnder(ctx context.Context, cfg *config.Config) (bool, error) {
	if cfg.Env.AuthToken != "" || cfg.DeclaresPrincipals() {
		return true, nil
	}
	if s.store == nil {
		return false, nil
	}
	n, err := s.store.OperatorTokenDefCountActiveAdmin(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AdminPrincipalResolved reports whether the CURRENT config declares a
// substrate:admin principal that can log in. The token tool's last-admin retire
// guard reads it at retire time, so a reload that dropped the principal is seen.
func (s *Server) AdminPrincipalResolved() bool {
	return s.cfg().AdminPrincipalResolved()
}

// refuseReloadLosingAuth refuses a config reload that would take away what the
// running server authenticates with:
//
//   - open mode. Declared principals are part of the open-mode decision (RFC
//     ES F2), so a reload dropping the last one, with no legacy secret and no
//     admin token, would serve every route unauthenticated — on an address
//     the boot check refused to open, and which nothing checks again.
//   - the last admin credential. An admin principal lets the last admin token
//     be retired, so a reload dropping that principal afterwards would leave
//     nobody able to manage tokens.
func (s *Server) refuseReloadLosingAuth(ctx context.Context, candidate *config.Config) error {
	cur := s.cfg()
	if cur.Env.AuthToken != "" || cur.DeclaresPrincipals() {
		on, err := s.authConfiguredUnder(ctx, candidate)
		if err != nil {
			return fmt.Errorf("cannot tell whether the new config leaves authentication on: %w", err)
		}
		if !on {
			return errors.New("the new config would put the server in open mode: it declares no principals " +
				"and sets no LOOMCYCLE_AUTH_TOKEN, and the store holds no admin operator token; keep a principal, " +
				"or restart to change how the server authenticates")
		}
	}
	if configAdmin(cur) && !configAdmin(candidate) {
		n := 0
		if s.store != nil {
			var err error
			if n, err = s.store.OperatorTokenDefCountActiveAdmin(ctx); err != nil {
				return fmt.Errorf("cannot tell whether the new config leaves an admin credential: %w", err)
			}
		}
		if n == 0 {
			return errors.New("the new config would leave no admin credential: it drops the last substrate:admin " +
				"principal, sets no LOOMCYCLE_AUTH_TOKEN, and the store holds no admin operator token; keep the admin " +
				"principal, or mint an admin token first")
		}
	}
	return nil
}

// configAdmin reports whether the config itself gives the server an admin
// credential: the legacy secret, or an admin principal that can log in.
func configAdmin(c *config.Config) bool {
	return c.Env.AuthToken != "" || c.AdminPrincipalResolved()
}
