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

// refuseReloadIntoOpenMode refuses a config reload that would move a running
// server into open mode. Declared principals are part of the open-mode decision
// (RFC ES F2), so a reload dropping the last one, with no legacy secret and no
// admin token, would serve every route unauthenticated — on an address the
// boot check refused to open, and which nothing checks again.
func (s *Server) refuseReloadIntoOpenMode(ctx context.Context, candidate *config.Config) error {
	cur := s.cfg()
	if cur.Env.AuthToken == "" && !cur.DeclaresPrincipals() {
		return nil // the config was not what kept auth on
	}
	on, err := s.authConfiguredUnder(ctx, candidate)
	if err != nil {
		return fmt.Errorf("cannot tell whether the new config leaves authentication on: %w", err)
	}
	if !on {
		return errors.New("the new config would put the server in open mode: it declares no principals " +
			"and sets no LOOMCYCLE_AUTH_TOKEN, and the store holds no admin operator token; keep a principal, " +
			"or restart to change how the server authenticates")
	}
	return nil
}
