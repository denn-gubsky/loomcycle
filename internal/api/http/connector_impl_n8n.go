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
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ListChannels returns the operator-declared channels joined with
// runtime stats (count + oldest/newest visible_at). Mirrors what
// handleListChannels writes to the HTTP wire — same code path
// minus the JSON-encoder framing.
// formatVisibleAt renders a message's visible_at for the channel listing,
// returning "" for anything that is not a real delivery time.
//
// A HELD message carries the reserved sentinel instant (RFC CY), and printing
// "2200-01-01" in an operator's channel list reads as a bug rather than as
// "this channel is holding". The `hold` flag on the descriptor is what says
// that; the timestamp says nothing, which is the truth — a held message has no
// scheduled arrival.
func formatVisibleAt(t time.Time) string {
	if t.IsZero() || store.IsChannelReservedVisibleAt(t) {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) ListChannels(ctx context.Context) (connector.ListChannelsResponse, error) {
	stats, err := s.store.ChannelStats(ctx)
	if err != nil {
		return connector.ListChannelsResponse{}, err
	}
	ix := newStatsIndex(stats)

	// An operator's yaml hooks are the operator's: a tenant operator sees that
	// a channel is hooked and by which hooks, never an inline webhook's URL
	// or headers (a URL may carry a token; a header may hold an expanded
	// ${LOOMCYCLE_*} secret).
	tenantID, all := s.principalTenantScope(ctx, "")
	out := make([]connector.ChannelDescriptor, 0, len(s.cfg().Channels))
	for name, ch := range s.cfg().Channels {
		yamlHooks := ch.Hooks
		if !all {
			yamlHooks = hooks.WithoutEndpoints(ch.Hooks)
		}
		desc := connector.ChannelDescriptor{
			Name:        name,
			Description: ch.Description,
			Scope:       ch.Scope,
			Semantic:    ch.Semantic,
			Publisher:   ch.Publisher,
			Period:      ch.Period,
			DefaultTTL:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
			Hold:        ch.Hold,
			Hooks:       yamlHooks,
			Source:      "yaml",
		}
		// A yaml channel's messages are keyed by the writer's tenant (and a
		// global one's also by the operator layer every tenant reads): a
		// tenant sees what it reads, an admin every tenant's summed.
		st, ok := ix.forReader(name, tenantID, store.MemoryScope(ch.Scope))
		if all {
			st, ok = ix.acrossTenants(name)
		}
		if ok {
			attachStats(&desc, st)
		}
		out = append(out, desc)
	}
	// Merge runtime-substrate channels (v0.11.5). yaml-declared names
	// take precedence — a runtime row sharing a name with yaml is a
	// no-op here because the CRUD layer refuses to create one in the
	// first place; defensively skip on read anyway.
	runtimeRows, runtimeErr := s.store.ChannelsList(ctx)
	if runtimeErr != nil {
		return connector.ListChannelsResponse{}, runtimeErr
	}
	// ChannelsList returns every tenant's rows; tenant operators see only
	// their own channels, admin sees all.
	for _, r := range runtimeRows {
		if !all && r.TenantID != tenantID {
			continue
		}
		if _, yaml := s.cfg().Channels[r.Name]; yaml {
			continue
		}
		desc := rowToBareDescriptor(r)
		if st, ok := ix.runtimeStats(r, all); ok {
			attachStats(&desc, st)
		}
		out = append(out, desc)
	}
	// Surface orphaned message rows for channels NOT in the
	// declared yaml OR runtime substrate — same forensic shape as the
	// HTTP handler.
	runtimeNames := map[string]bool{}
	for _, r := range runtimeRows {
		runtimeNames[r.Name] = true
	}
	// A tenant sees only its own keyspace's orphans: another tenant's channel
	// names are not its to read.
	orphans := map[string]bool{}
	for _, st := range stats {
		name := st.Channel
		if _, declared := s.cfg().Channels[name]; declared || runtimeNames[name] || orphans[name] {
			continue
		}
		if !all && st.TenantID != tenantID {
			continue
		}
		orphans[name] = true
		desc := connector.ChannelDescriptor{Name: name, Source: "orphan"}
		if all {
			st, _ = ix.acrossTenants(name)
		}
		attachStats(&desc, st)
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
			// RFC L/N tenant isolation: a tenant principal sees only its own
			// tenant's run transitions. Filtered here (after the bus read, where
			// the runstate event still carries TenantID) so it also covers the
			// cluster-backplane path. Left off (TenantScoped=false) for the
			// gRPC/MCP adapters — behaviour unchanged for them.
			if req.TenantScoped && evt.TenantID != req.TenantID {
				continue
			}
			if req.Agent != "" && evt.Agent != req.Agent {
				continue
			}
			// A run with no parent context cannot belong to a walk, so an
			// unstamped event is filtered out rather than passed through — a
			// walk view that also showed unrelated runs would not be a walk
			// view.
			if !walkIDMatches(req.WalkID, evt.ParentContext) {
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

// walkIDMatches reports whether an event belongs to the filtered walk.
//
// An empty filter matches everything — the whole-user stream is unchanged for
// every caller that does not ask for a walk. A run with NO parent context
// cannot belong to any walk, so it is EXCLUDED rather than passed through: a
// walk view that also showed unrelated runs would not be a walk view.
func walkIDMatches(want string, pc *store.ParentContext) bool {
	if want == "" {
		return true
	}
	return pc != nil && pc.WalkID == want
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
		ParentContext: e.ParentContext,
	}
}

// statsIndex is ChannelStats by (message keyspace, channel). Stats are
// aggregated per tenant keyspace; joining them by channel name alone gave
// one tenant another tenant's counts for a channel of the same name.
type statsIndex map[string]store.ChannelStats

func newStatsIndex(stats []store.ChannelStats) statsIndex {
	ix := make(statsIndex, len(stats))
	for _, st := range stats {
		ix[st.TenantID+"\x00"+st.Channel] = st
	}
	return ix
}

// forKeyspace is one channel's stats in one keyspace.
func (ix statsIndex) forKeyspace(name, keyspace string) (store.ChannelStats, bool) {
	st, ok := ix[keyspace+"\x00"+name]
	return st, ok
}

// forReader is one channel's stats as a reader in tenant sees them: its own
// keyspace, and for a global channel the operator layer every tenant reads.
func (ix statsIndex) forReader(name, tenant string, scope store.MemoryScope) (store.ChannelStats, bool) {
	own, shared := store.ChannelReadTenants(tenant, scope)
	var parts []store.ChannelStats
	for _, k := range []string{own, shared} {
		if st, ok := ix.forKeyspace(name, k); ok {
			parts = append(parts, st)
		}
		if shared == own {
			break
		}
	}
	return sumStats(parts)
}

// acrossTenants is one channel's stats summed over every keyspace, for a
// caller who sees them all.
func (ix statsIndex) acrossTenants(name string) (store.ChannelStats, bool) {
	var parts []store.ChannelStats
	for _, st := range ix {
		if st.Channel == name {
			parts = append(parts, st)
		}
	}
	return sumStats(parts)
}

// sumStats folds several keyspaces' stats for one channel into one.
func sumStats(parts []store.ChannelStats) (store.ChannelStats, bool) {
	if len(parts) == 0 {
		return store.ChannelStats{}, false
	}
	out := parts[0]
	out.TenantID = ""
	for _, st := range parts[1:] {
		out.MessageCount += st.MessageCount
		out.Held += st.Held
		out.AwaitingHooks += st.AwaitingHooks
		if !st.OldestVisibleAt.IsZero() && (out.OldestVisibleAt.IsZero() || st.OldestVisibleAt.Before(out.OldestVisibleAt)) {
			out.OldestVisibleAt = st.OldestVisibleAt
		}
		if st.NewestVisibleAt.After(out.NewestVisibleAt) {
			out.NewestVisibleAt = st.NewestVisibleAt
		}
	}
	return out, true
}

// runtimeStats is a runtime channel's stats for the lister: an admin sees a
// global channel's every layer, anyone else what its owner's readers see.
func (ix statsIndex) runtimeStats(r store.ChannelRow, all bool) (store.ChannelStats, bool) {
	if all && store.MemoryScope(r.Scope) == store.MemoryScopeGlobal {
		return ix.acrossTenants(r.Name)
	}
	return ix.forReader(r.Name, r.TenantID, store.MemoryScope(r.Scope))
}
