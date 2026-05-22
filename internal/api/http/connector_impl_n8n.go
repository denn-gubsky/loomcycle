// connector_impl_n8n.go — Connector method bodies for the v0.9.x
// n8n RFC Phase 0 additions (ListChannels + StreamUserRunStates).
// Same canonical-business-logic pattern as the rest of
// connector_impl*.go: the HTTP handlers are thin REST wrappers; the
// real work lives here so MCP / gRPC dispatch the same code.
package http

import (
	"context"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
)

// ListChannels returns the operator-declared channels joined with
// runtime stats (count + oldest/newest visible_at). Mirrors what
// handleListChannels writes to the HTTP wire — same code path
// minus the JSON-encoder framing.
func (s *Server) ListChannels(ctx context.Context) (connector.ListChannelsResponse, error) {
	stats, err := s.store.ChannelStats(ctx)
	if err != nil {
		return connector.ListChannelsResponse{}, err
	}
	statsByName := make(map[string]struct {
		Count           int64
		OldestVisibleAt time.Time
		NewestVisibleAt time.Time
	}, len(stats))
	for _, st := range stats {
		statsByName[st.Channel] = struct {
			Count           int64
			OldestVisibleAt time.Time
			NewestVisibleAt time.Time
		}{st.MessageCount, st.OldestVisibleAt, st.NewestVisibleAt}
	}

	out := make([]connector.ChannelDescriptor, 0, len(s.cfg.Channels))
	for name, ch := range s.cfg.Channels {
		desc := connector.ChannelDescriptor{
			Name:        name,
			Scope:       ch.Scope,
			Semantic:    ch.Semantic,
			Publisher:   ch.Publisher,
			Period:      ch.Period,
			DefaultTTL:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
		}
		if st, ok := statsByName[name]; ok {
			desc.MessageCount = st.Count
			if !st.OldestVisibleAt.IsZero() {
				desc.OldestVisibleAt = st.OldestVisibleAt.UTC().Format(time.RFC3339)
			}
			if !st.NewestVisibleAt.IsZero() {
				desc.NewestVisibleAt = st.NewestVisibleAt.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, desc)
	}
	// Surface orphaned message rows for channels NOT in the
	// declared yaml — same forensic shape as the HTTP handler.
	for name, st := range statsByName {
		if _, declared := s.cfg.Channels[name]; declared {
			continue
		}
		desc := connector.ChannelDescriptor{Name: name, MessageCount: st.Count}
		if !st.OldestVisibleAt.IsZero() {
			desc.OldestVisibleAt = st.OldestVisibleAt.UTC().Format(time.RFC3339)
		}
		if !st.NewestVisibleAt.IsZero() {
			desc.NewestVisibleAt = st.NewestVisibleAt.UTC().Format(time.RFC3339)
		}
		out = append(out, desc)
	}
	// Deterministic order — easier on transports that snapshot the
	// response for caching or comparison.
	sortChannelDescriptors(out)
	return connector.ListChannelsResponse{Channels: out}, nil
}

func sortChannelDescriptors(out []connector.ChannelDescriptor) {
	// In-place insertion sort by Name. Channel counts are typically
	// small (~10s); avoiding the sort package keeps the call free of
	// heap allocations on a hot listing path.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Name > out[j].Name; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
}

// StreamUserRunStates subscribes to the runstate.Bus and calls visit
// for every event matching the filter. Exits cleanly when ctx
// cancels (returning nil) or when visit returns ErrStopStreaming.
// Non-sentinel visit errors propagate.
//
// Each call holds one bus subscription for its lifetime — slow
// visitors that don't drain in time cause drops on the subscription's
// buffer (logged as DroppedEvents at unsubscribe).
func (s *Server) StreamUserRunStates(ctx context.Context, req connector.StreamUserRunStatesRequest, visit connector.RunStateVisitor) error {
	if s.runStateBus == nil {
		return connector.ErrRunStateStreamUnavailable
	}

	statusSet := make(map[string]bool, len(req.Statuses))
	for _, st := range req.Statuses {
		if st != "" {
			statusSet[st] = true
		}
	}

	sub := s.runStateBus.Subscribe(req.UserID)
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-sub.C:
			if !ok {
				return nil
			}
			if req.Agent != "" && evt.Agent != req.Agent {
				continue
			}
			if len(statusSet) > 0 && !statusSet[evt.Status] {
				continue
			}
			if err := visit(runStateEventToConnector(evt)); err != nil {
				if errors.Is(err, connector.ErrStopStreaming) {
					return nil
				}
				return err
			}
		}
	}
}

func runStateEventToConnector(e runstate.RunStateEvent) connector.RunStateEvent {
	ts := ""
	if !e.TS.IsZero() {
		ts = e.TS.UTC().Format(time.RFC3339)
	}
	return connector.RunStateEvent{
		RunID:         e.RunID,
		AgentID:       e.AgentID,
		Agent:         e.Agent,
		UserID:        e.UserID,
		ParentAgentID: e.ParentAgentID,
		Status:        e.Status,
		StopReason:    e.StopReason,
		Error:         e.Error,
		TS:            ts,
	}
}
