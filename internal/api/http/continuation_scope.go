package http

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// sessionNoUserScope reports whether a continuation of sessionID must resolve
// $cred: for its agent and tenant only, never in the session user's own scope.
//
// A continuation runs as the session's user, whoever sends it. When a trigger
// started the session — a webhook, whose user can come from the delivery; a
// schedule written for someone else — that user was never shown to be someone
// the trigger may act as, and its run carried CredentialsNoUserScope. A
// continuation must not get back what the trigger's run was denied, so any
// run of the session that carried the bit carries it to the continuation
// (each continuation then carries it on, so the last run alone would also
// do; any run is the reading that cannot be widened by a run row gone
// missing). Runs that cannot be read withhold the scope: the bit cannot be
// shown absent.
func (s *Server) sessionNoUserScope(ctx context.Context, sessionID string) bool {
	if s.store == nil {
		return false // no store, no row: nothing a trigger could have recorded
	}
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil {
		log.Printf("continuation: read session %s's runs for its credential scope: %v; withholding the user's scope", sessionID, err)
		return true
	}
	for _, r := range runs {
		if r.CredentialsNoUserScope {
			return true
		}
	}
	return false
}

// actingForAnother reports whether the caller on ctx is someone other than
// the user (tenantID, userID) a session's or run's work is done as.
//
// Anyone a tenant lets continue a session, or send a turn to a live run, may
// do so — the work still runs as that session's user — but must not get the
// user's stored credentials through it: what they type would then be acted on
// with the user's own keys. Such a caller's turns resolve $cred: for the agent
// and tenant only. An admin with another subject is another user.
//
// Two callers are not judged here and keep the session user's scope:
//   - no principal (open mode): nobody is authenticated, so nobody is "other";
//   - the legacy LOOMCYCLE_AUTH_TOKEN: it is how an integration (a backend
//     serving its own users) speaks for them, naming the user on each request,
//     so a continuation it sends is the session user's own.
func actingForAnother(ctx context.Context, tenantID, userID string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy {
		return false
	}
	return p.TenantID != tenantID || p.Subject != userID
}

// userScopeLatch returns the live run's UserScopeWithheld, creating it on
// first use. Both the run's identity and its steer queue take it from here,
// in either order, so they share one; releaseUserScopeLatch drops it when the
// run's steering ends (or a sub-run's setup is torn down).
func (s *Server) userScopeLatch(runID string) *atomic.Bool {
	v, _ := s.userScopeLatches.LoadOrStore(runID, new(atomic.Bool))
	return v.(*atomic.Bool)
}

func (s *Server) releaseUserScopeLatch(runID string) { s.userScopeLatches.Delete(runID) }

// withholdForOtherSender records, before another user's turn reaches runID,
// that the run may no longer use its user's own scope: on its row (which the
// run reads back when it takes the turn, and a resume reads) and on the latch
// of a run live on this replica. A row that cannot be read or written refuses
// the turn: sending it unrecorded would hand the user's keys to its sender.
func (s *Server) withholdForOtherSender(ctx context.Context, runID string) error {
	if _, ok := auth.PrincipalFromContext(ctx); !ok || s.store == nil {
		return nil
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("%w: read run %s for its credential scope: %v", runner.ErrInternal, runID, err)
	}
	if !actingForAnother(ctx, run.TenantID, run.UserID) || run.CredentialsNoUserScope {
		return nil
	}
	if err := s.store.WithholdRunUserScope(ctx, runID); err != nil {
		return fmt.Errorf("%w: record run %s's credential scope: %v", runner.ErrInternal, runID, err)
	}
	if v, ok := s.userScopeLatches.Load(runID); ok {
		v.(*atomic.Bool).Store(true)
	}
	return nil
}

// adoptWithheldUserScope is the run's side of withholdForOtherSender, called
// as it takes an operator turn: the turn's sender may have been someone else,
// on this replica or another, so the row decides. A row that cannot be read
// withholds the scope.
func (s *Server) adoptWithheldUserScope(ctx context.Context, runID string, latch *atomic.Bool) {
	if latch == nil || latch.Load() || s.store == nil {
		return
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		log.Printf("steer: read run %s for its credential scope: %v; withholding the user's scope", runID, err)
		latch.Store(true)
		return
	}
	if run.CredentialsNoUserScope {
		latch.Store(true)
	}
}
