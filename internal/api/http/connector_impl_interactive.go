package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// connector_impl_interactive.go — RFC AI. The interactive-session operations
// (steer + re-attach) lifted onto the Connector so gRPC dispatches through the
// SAME in-process steer registry / store the HTTP handlers own (one process →
// a gRPC steer reaches an HTTP-started run and vice versa). Mirrors the v0.33.0
// CompactRun lift; the HTTP handlers (handleRunInput / handleRunStream) call
// these too, so there's one implementation per operation.

// SteerRun implements connector.Connector. It is the transport-agnostic core of
// POST /v1/runs/{run_id}/input: tenant-ownership gate + steerReg.Push. `source`
// is resolved by the caller at its auth boundary (HTTP: cookie→webui else api;
// gRPC: always api) — never trusted from the wire. Cross-replica routing is
// inherited from steerReg.Push (a local miss delegates to the cluster
// coordinator).
func (s *Server) SteerRun(ctx context.Context, runID, text, source string) (bool, error) {
	if s.steerReg == nil {
		return false, connector.ErrSteeringUnavailable
	}
	text = strings.TrimSpace(text)
	if text == "" {
		// Callers validate too (HTTP 422 / gRPC InvalidArgument); this is a
		// defensive floor so an empty turn never reaches the loop.
		return false, connector.ErrRunNotInFlight
	}
	// Tenant-ownership gate: a cross-tenant (or unknown) run is folded into an
	// opaque ErrRunNotInFlight — run_ids are not secret, so the gate must not
	// become an existence oracle. Mirrors handleRunInput's gate exactly.
	entry, ok := s.steerReg.Get(runID)
	if !ok {
		return false, connector.ErrRunNotInFlight
	}
	if entry.SessionID != "" && s.store != nil {
		if sess, err := s.store.GetSession(ctx, entry.SessionID); err == nil {
			if !sessionOwnershipOK(ctx, sess) {
				return false, connector.ErrRunNotInFlight
			}
		}
	}
	delivered, err := s.steerReg.Push(ctx, runID, steer.Message{
		Text: text, Source: source, EnqueuedAt: time.Now(),
	})
	switch {
	case errors.Is(err, steer.ErrQueueFull):
		return false, connector.ErrSteerQueueFull
	case errors.Is(err, steer.ErrRunNotFound):
		return false, connector.ErrRunNotInFlight
	case err != nil:
		return false, err
	}
	return delivered, nil
}

// StreamRunEvents implements connector.Connector — the transport-agnostic core
// of GET /v1/runs/{run_id}/stream: tenant-ownership gate + the shared
// streamRunEvents tail engine (which replays the operator's own turns per RFC
// AI S1). It needs only the store (no steerReg). The HTTP handler emits its own
// side-channel `agent` frame; a gRPC StreamRun handler synthesizes an `agent`
// Event before calling this.
func (s *Server) StreamRunEvents(ctx context.Context, runID string, fromSeq int64, visit connector.RunEventVisitor) error {
	if s.store == nil {
		return connector.ErrSteeringUnavailable
	}
	// Tenant-ownership gate via the tenant-scoped accessor: a cross-tenant or
	// missing run both fold into the opaque ErrRunNotInFlight (no existence
	// oracle). Super-admin / legacy / open see all.
	if _, err := s.tenantStore(ctx).GetRun(ctx, runID); err != nil {
		return connector.ErrRunNotInFlight
	}
	return s.streamRunEvents(ctx, runID, fromSeq, func(ev providers.Event) error {
		return visit(ev)
	})
}

// CancelTurn implements connector.Connector — the transport-agnostic core of
// POST /v1/runs/{run_id}/cancel (RFC BH). It stops the current turn of a LIVE
// interactive run and parks it at awaiting_input; it is NOT whole-run cancel.
// Both handleCancelTurn (HTTP) and the gRPC CancelTurn RPC call this, so the
// session-ownership gate + cross-replica owner-routing live in one place.
//
// Two paths (RFC BH P3a):
//   - Live on THIS replica (steer registry hit): the P1 local flow — ownership
//     gate + IsArmed + fire the local token.
//   - Not live here (steer registry miss): gate ownership via the SHARED store
//     (a cross-tenant / unknown run folds into the opaque ErrRunNotInFlight) and
//     route to the owning replica by runs.replica_id via the registry's cluster
//     fallback. Single-process (no coordinator) folds back to ErrRunNotInFlight,
//     byte-identical to P1's steer-miss.
func (s *Server) CancelTurn(ctx context.Context, runID, reason string) (bool, bool, error) {
	if s.turnCancelReg == nil || s.steerReg == nil {
		return false, false, connector.ErrTurnCancelUnavailable
	}
	reason = strings.TrimSpace(reason)

	// Local fast path: the run is live on THIS replica. Gate on its session's
	// tenant (mirrors SteerRun), then fire the local armed token.
	if entry, ok := s.steerReg.Get(runID); ok {
		if entry.SessionID != "" && s.store != nil {
			if sess, err := s.store.GetSession(ctx, entry.SessionID); err == nil {
				if !sessionOwnershipOK(ctx, sess) {
					return false, false, connector.ErrRunNotInFlight
				}
			}
		}
		// Only an armed run (interactive + in-flight) is turn-cancellable.
		if !s.turnCancelReg.IsArmed(runID) {
			// Distinguish a non-interactive run for a clearer error (best-effort;
			// the caller already passed the ownership gate above).
			if s.store != nil {
				if run, err := s.tenantStore(ctx).GetRun(ctx, runID); err == nil && !run.Interactive {
					return false, false, connector.ErrTurnNotInteractive
				}
			}
			return false, false, connector.ErrTurnNotMidTurn
		}
		// Fire the local armed token. A false return means the token vanished
		// between IsArmed and here (turn just ended / a concurrent cancel won).
		if !s.turnCancelReg.CancelLocal(runID, reason) {
			return false, false, connector.ErrTurnNotMidTurn
		}
		return true, true, nil
	}

	// Cross-replica fallback: the run is not live here. Gate ownership via the
	// shared store (a cross-tenant / unknown run folds into the opaque
	// ErrRunNotInFlight), then route the cancel to the run's owning replica.
	if s.store == nil {
		return false, false, connector.ErrRunNotInFlight
	}
	if _, err := s.tenantStore(ctx).GetRun(ctx, runID); err != nil {
		return false, false, connector.ErrRunNotInFlight
	}
	fired, err := s.turnCancelReg.Cancel(ctx, runID, reason)
	if err != nil {
		return false, false, err
	}
	if !fired {
		// Not armed on any reachable replica (parked / just ended / owner gone),
		// or single-process with no coordinator wired. "No in-flight turn."
		return false, false, connector.ErrRunNotInFlight
	}
	return true, true, nil
}

// ResolveInterrupt implements connector.Connector — the transport-agnostic core
// of POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve (RFC BH). It
// resolves a pending interruption as an answer (disposition "" / "answer") or a
// decline (disposition "declined": carries no answer, skips option validation,
// writes the terminal "declined" status). Both handleResolveInterrupt (HTTP) and
// the gRPC ResolveInterrupt RPC call this, so the tenant-ownership gate + the
// validation + persist + wake + system-publish live in one place.
//
// `resolvedBy` is resolved at the caller's auth boundary (HTTP cookie→webui else
// api; gRPC api) — never wire-trusted. Returns the terminal status. Errors are
// wrapped via connector.WithMessage so each transport classifies on the sentinel
// while surfacing the verbatim (byte-identical, for HTTP) message.
//
// Distinct from InterruptionResolve (the MCP-facing method), which keeps its own
// shipped wording + "mcp" attribution default and lacks the decline disposition.
func (s *Server) ResolveInterrupt(ctx context.Context, runID, interruptID, kind, answer, resolvedBy, disposition string) (string, error) {
	if s.store == nil {
		return "", connector.WithMessage(connector.ErrInterruptStoreUnavailable, "interrupts require persistence (Store not configured)")
	}
	if kind == "" {
		kind = store.InterruptKindQuestion
	}
	if kind != store.InterruptKindQuestion {
		return "", connector.WithMessage(connector.ErrInterruptUnsupportedKind,
			fmt.Sprintf("unsupported kind %q (v0.8.16 supports: question)", kind))
	}
	if resolvedBy == "" {
		// Defensive floor — HTTP/gRPC resolve this at their auth boundary and
		// pass a non-empty value; keep the ledger attributed if one slips through.
		resolvedBy = store.InterruptResolvedByAPI
	}

	// Disposition gate (RFC BH P2). Only "" / "answer" / "declined" are valid.
	switch disposition {
	case "", dispositionAnswer, dispositionDeclined:
		// ok
	default:
		return "", connector.WithMessage(connector.ErrInterruptUnsupportedDisposition,
			fmt.Sprintf("unsupported disposition %q (valid: answer, declined)", disposition))
	}
	declined := disposition == dispositionDeclined
	if declined && answer != "" {
		return "", connector.WithMessage(connector.ErrInterruptDeclineWithAnswer,
			"declined disposition must not carry an answer")
	}

	// Validate the stored row's ownership + options + expiry.
	row, err := s.store.InterruptGet(ctx, interruptID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return "", connector.WithMessage(connector.ErrInterruptNotFound, "interrupt not found")
	}
	if err != nil {
		return "", err
	}
	if row.RunID != runID {
		// The URL/path run_id must match the stored row's run_id — blocks
		// retargeting a resolve at another run's interrupt WITHIN a tenant.
		return "", connector.WithMessage(connector.ErrInterruptNotFound, "interrupt does not belong to that run")
	}
	// Tenant-ownership gate (RFC L/N): the run this interrupt belongs to must be
	// in the caller's tenant — else a resolve steers ANOTHER tenant's paused run.
	// tenantStore folds a cross-tenant/missing run into an opaque ErrNotFound.
	if _, err := s.tenantStore(ctx).GetRun(ctx, row.RunID); err != nil {
		if errors.As(err, &nf) {
			return "", connector.WithMessage(connector.ErrInterruptNotFound, "interrupt not found")
		}
		return "", err
	}
	if row.Status != store.InterruptStatusPending {
		if row.Status == store.InterruptStatusTimedOut && !row.ExpiresAt.IsZero() && row.ExpiresAt.Before(time.Now()) {
			return "", connector.WithMessage(connector.ErrInterruptExpired, "interrupt expired")
		}
		return "", connector.WithMessage(connector.ErrInterruptAlreadyTerminal,
			fmt.Sprintf("interrupt already %s", row.Status))
	}
	if !row.ExpiresAt.IsZero() && row.ExpiresAt.Before(time.Now()) {
		return "", connector.WithMessage(connector.ErrInterruptExpired, "interrupt expired")
	}

	// A decline (RFC BH P2) carries no answer, so it SKIPS option-list validation
	// entirely. The answer path is unchanged.
	if !declined {
		if len(row.Options) > 0 {
			var opts []string
			if err := json.Unmarshal(row.Options, &opts); err == nil && len(opts) > 0 {
				ok := false
				for _, o := range opts {
					if o == answer {
						ok = true
						break
					}
				}
				if !ok {
					return "", connector.WithMessage(connector.ErrInterruptInvalidAnswer,
						fmt.Sprintf("answer %q is not one of the declared options: %v", answer, opts))
				}
			}
		} else if answer == "" {
			return "", connector.WithMessage(connector.ErrInterruptInvalidAnswer,
				"answer is required for free-text interrupts")
		}
	}

	// Persist the terminal state. A decline uses InterruptFinish (the answer-less
	// terminal writer) → status=declined; an answer uses InterruptResolve →
	// status=resolved. Both reject an already-terminal row with
	// ErrInterruptAlreadyTerminal.
	finalStatus := store.InterruptStatusResolved
	var persistErr error
	if declined {
		finalStatus = store.InterruptStatusDeclined
		persistErr = s.store.InterruptFinish(ctx, interruptID, store.InterruptStatusDeclined, resolvedBy)
	} else {
		persistErr = s.store.InterruptResolve(ctx, interruptID, answer, resolvedBy, nil)
	}
	if persistErr != nil {
		if errors.Is(persistErr, store.ErrInterruptAlreadyTerminal) {
			return "", connector.WithMessage(connector.ErrInterruptAlreadyTerminal,
				"interrupt already resolved, timed out, or cancelled")
		}
		return "", persistErr
	}

	// Wake the blocked tool (best-effort — no waiter is a no-op).
	if s.interruptionBus != nil {
		s.interruptionBus.Notify("intr:" + interruptID)
	}

	// Publish the external _system/interrupts/resolved signal so non-run Channel
	// subscribers see the terminal state. Best-effort.
	if s.systemPublisher != nil && row.UserID != "" {
		payload, _ := json.Marshal(map[string]any{
			"interrupt_id": interruptID,
			"run_id":       runID,
			"kind":         row.Kind,
			"status":       finalStatus,
			"answer":       answer,
			"resolved_by":  resolvedBy,
		})
		_, _ = s.systemPublisher.PublishNow(
			ctx,
			"_system/interrupts/resolved",
			store.MemoryScopeUser, row.UserID,
			payload, channels.SystemPublisherUserID, 0, 0,
		)
	}

	return finalStatus, nil
}
