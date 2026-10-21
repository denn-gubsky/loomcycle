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
//   - authentication. Declared principals are part of the open-mode decision
//     (RFC ES F2). When the running config turns auth on by itself (a legacy
//     secret or principals), the new one must too. Admin tokens in the store
//     do not count here: one can be retired right after the reload — by
//     another replica whose config still has an admin principal, or inside
//     this reload's own check-to-swap window — and this replica would then
//     serve every route unauthenticated, on an address the boot check refused
//     to open, with nothing to notice (no open-mode warning runs once boot
//     saw auth on). The cost: a reload cannot drop every principal of a
//     server that relies on admin tokens alone; that takes a restart.
//   - the last admin credential. An admin principal lets the last admin token
//     be retired, so a reload dropping that principal afterwards would leave
//     nobody able to manage tokens.
func (s *Server) refuseReloadLosingAuth(ctx context.Context, candidate *config.Config) error {
	cur := s.cfg()
	if configAuthenticates(cur) && !configAuthenticates(candidate) {
		return errors.New("the new config declares no principals and sets no LOOMCYCLE_AUTH_TOKEN, so the " +
			"server would fall into open mode as soon as no admin operator token is left (tokens do not count " +
			"here: one can be retired at any moment). Keep a principal, set LOOMCYCLE_AUTH_TOKEN, or restart " +
			"to change how the server authenticates")
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

// configAuthenticates reports whether the config by itself turns
// authentication on, without asking the store.
func configAuthenticates(c *config.Config) bool {
	return c.Env.AuthToken != "" || c.DeclaresPrincipals()
}

// configAdmin reports whether the config itself gives the server an admin
// credential: the legacy secret, or an admin principal that can log in.
func configAdmin(c *config.Config) bool {
	return c.Env.AuthToken != "" || c.AdminPrincipalResolved()
}

// AuthPostureWarnings names, for the boot log, a server whose config turns
// authentication on with principals but where no request can get in, or
// nobody can administer it. Both are legal configs and both fail closed, so
// boot carries on; the operator just has to be told, because the symptom is
// "every request is 401" or "no one can mint a token", far from the cause.
// "Can get in" counts any non-retired token row; an expired one would still
// read as a way in, which only errs towards saying less.
func (s *Server) AuthPostureWarnings(ctx context.Context) []string {
	cfg := s.cfg()
	if cfg.Env.AuthToken != "" || !cfg.DeclaresPrincipals() {
		// The legacy secret is a login and an admin; with no principals the
		// server is open or rests on admin tokens, both handled elsewhere.
		return nil
	}
	if s.store == nil {
		return nil
	}
	var out []string
	if len(cfg.ResolvedPrincipals) == 0 {
		names, err := s.store.OperatorTokenDefListNames(ctx)
		if err != nil {
			return []string{fmt.Sprintf("could not list operator tokens to check that anyone can log in: %v", err)}
		}
		anyToken := false
		for _, n := range names {
			anyToken = anyToken || n.HasCurrent
		}
		if !anyToken {
			out = append(out, "no credential can authenticate: every declared principal is inert (its token_env "+
				"is empty), LOOMCYCLE_AUTH_TOKEN is not set and the store holds no operator token, so every "+
				"request will get 401. Set a principal's token_env, or set LOOMCYCLE_AUTH_TOKEN")
		}
	}
	if !cfg.AdminPrincipalResolved() {
		n, err := s.store.OperatorTokenDefCountActiveAdmin(ctx)
		if err != nil {
			return append(out, fmt.Sprintf("could not count admin operator tokens: %v", err))
		}
		if n == 0 {
			out = append(out, "no admin credential: no principal that can log in holds substrate:admin, "+
				"LOOMCYCLE_AUTH_TOKEN is not set and the store holds no admin operator token, so nobody can "+
				"mint tokens or use the admin routes. Add a substrate:admin principal or set LOOMCYCLE_AUTH_TOKEN")
		}
	}
	return out
}
