package http

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
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
