package http

import (
	"context"
	"log"
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
