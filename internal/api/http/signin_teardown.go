package http

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// subjectCredentials names the kinds of sign-in a person can hold besides the
// credential itself. Every place that ends a person's access — disable,
// erasure, delete, a password reset or change — asks endSubjectCredentials
// for the kinds it ends, so a new kind of sign-in is added in one place
// rather than in each of them (the drift that once left app sign-ins live
// after a reset). TestSignInTeardown_EveryCallerGoesThroughTheHelper holds
// every caller to it.
type subjectCredentials uint8

const (
	// credAppSignIns: app sign-ins (refresh-token families) and the access
	// tokens issued through them.
	credAppSignIns subjectCredentials = 1 << iota
	// credDelegatedTokens: the person's delegated member tokens — hand
	// minted, gate issued and device-login tokens alike (all are member-scope
	// rows retireDelegatedUserTokens retires).
	credDelegatedTokens
	// credWebSessions: every Web UI session. An approved device code that
	// has not been redeemed is bound to the approving session, so it ends
	// with it.
	credWebSessions

	credAll = credAppSignIns | credDelegatedTokens | credWebSessions
)

// endSubjectCredentials ends the kinds of sign-in in set for (tenant,
// subject) and returns the delegated tokens it retired (each recorded in the
// token audit trail).
//
// App sign-ins go before the other tokens: endSubjectAppSignIns says why.
// Each kind is ended whatever the one before answered, so a fault in one
// leaves no other way in working; the errors are reported together, each
// wrapped with the kind it came from.
func (s *Server) endSubjectCredentials(ctx context.Context, tenant, subject string, set subjectCredentials) ([]store.OperatorTokenDefRow, error) {
	var errs []error
	if set&credAppSignIns != 0 {
		if err := s.endSubjectAppSignIns(ctx, tenant, subject); err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", errEndAppSignIns, err))
		}
	}
	var retired []store.OperatorTokenDefRow
	if set&credDelegatedTokens != 0 {
		var err error
		retired, err = s.retireDelegatedUserTokens(ctx, tenant, subject)
		for _, row := range retired {
			s.recordTokenAudit(ctx, "retire", row)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", errRetireTokens, err))
		}
	}
	if set&credWebSessions != 0 {
		if _, err := s.store.UISessionRevokeSubject(ctx, tenant, subject, time.Now().UTC()); err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", errEndSessions, err))
		}
	}
	return retired, errors.Join(errs...)
}

var (
	// errEndAppSignIns, errRetireTokens and errEndSessions let a caller that
	// reports each kind under its own code (the operator PATCH) tell the
	// faults apart.
	errEndAppSignIns = errors.New("end app sign-ins")
	errRetireTokens  = errors.New("retire tokens")
	errEndSessions   = errors.New("end sessions")
)
