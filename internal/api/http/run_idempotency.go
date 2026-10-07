package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// clientRunKey is what a caller's idempotency_key is stored as in
// runs.idempotency_key. The column's unique index spans the database and is
// shared with the webhook receiver ("webhook:…") and the scheduler
// ("sched:…"), so a caller's key is never stored as sent: it gets its own
// prefix, and the tenant and user it belongs to. Two tenants, or two users of
// one tenant, using the same key hold different rows, and neither can learn
// of or be answered with the other's run. The caller's key goes last because
// it may contain ":"; the tenant and user are escaped so they cannot.
func clientRunKey(tenant, user, key string) string {
	return "run:" + url.QueryEscape(tenant) + ":" + url.QueryEscape(user) + ":" + key
}

// runHoldingClientKey returns the run that holds a caller's stored key, as a
// *runner.DuplicateRunError, or nil when none does.
//
// lostRace is set by the caller that has just been refused by the unique
// index: a run holds the key, but its row may not be readable yet, so the
// lookup is retried briefly before giving up.
func (s *Server) runHoldingClientKey(ctx context.Context, storedKey, tenant, user string, lostRace bool) (*runner.DuplicateRunError, error) {
	attempts := 1
	if lostRace {
		attempts = 20
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %v", runner.ErrInternal, ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		run, found, err := s.store.RunByIdempotencyKey(ctx, storedKey)
		if err != nil {
			return nil, fmt.Errorf("%w: look up idempotency_key: %v", runner.ErrInternal, err)
		}
		if !found {
			continue
		}
		// The stored key already names the tenant and user, so this cannot
		// differ for a key this code wrote. It guards the one row that could
		// share the spelling: a delivery id an earlier release stored bare.
		if run.TenantID != tenant || run.UserID != user {
			return nil, fmt.Errorf("%w: idempotency_key is not available", runner.ErrInvalidArgument)
		}
		return &runner.DuplicateRunError{RunID: run.ID, AgentID: run.AgentID, SessionID: run.SessionID}, nil
	}
	return nil, nil
}

// existingRunPollInterval is how often a caller joined to a run it did not
// start re-reads that run's row.
const existingRunPollInterval = 200 * time.Millisecond

// existingRunResult answers a request whose idempotency_key a run already
// held, from that run's row.
//
// wait is the join: it returns once the run has ended. It does not wait on a
// run that is parked for its next message or held for a review, which would
// never end on its own, and it stops when ctx ends — the caller's timeout or
// departure. In both cases the run is reported as it is, still running. The
// run is never cancelled from here: this caller did not start it.
func (s *Server) existingRunResult(ctx context.Context, dup *runner.DuplicateRunError, wait bool, parentContext *store.ParentContext) connector.SpawnRunResult {
	res := connector.SpawnRunResult{
		AgentID: dup.AgentID, RunID: dup.RunID, SessionID: dup.SessionID,
		Status: string(store.RunRunning), ParentContext: parentContext, Deduplicated: true,
	}
	// The row is read on a ctx that outlives the caller's: a wait that ended
	// with the caller still reports the run as it is now.
	readCtx := context.WithoutCancel(ctx)
	for {
		run, err := s.store.GetRun(readCtx, dup.RunID)
		if err != nil {
			res.Error = "the run holding this idempotency_key could not be read: " + err.Error()
			return res
		}
		res.Status, res.StopReason, res.Error = string(run.Status), run.StopReason, run.ErrorMsg
		if store.IsTerminalRunStatus(run.Status) {
			res.Usage = storeRunToConnector(run).Usage
			var rec runResultRecord
			if len(run.Result) > 0 && json.Unmarshal(run.Result, &rec) == nil {
				res.FinalText = rec.FinalText
			}
			return res
		}
		if !wait {
			return res
		}
		if state, _ := awaited.ForRun(readCtx, s.store, run.ID); state == awaited.Input || state == awaited.Review {
			return res
		}
		select {
		case <-ctx.Done():
			return res
		case <-time.After(existingRunPollInterval):
		}
	}
}
