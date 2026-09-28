package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// NewChannelHookWorker builds the worker that decides the messages of
// channels that carry hooks, from the server's own collaborators: its store,
// its hook dispatcher (so a channel hook's webhook dials and credentials
// follow the same rules as a run's), the channel writer for decision records,
// and the channel bus. It also wires the worker's counters into /metrics.
// owner names this replica on the worker's leases. interruption is the tool a
// hold asks a person through (the same instance agents use); nil leaves holds
// unanswerable.
func (s *Server) NewChannelHookWorker(owner string, sched *channels.Scheduler, interruption tools.Tool) *channelhooks.Worker {
	env := s.cfg().Env
	var writer channels.Writer
	if w, ok := s.systemPublisher.(channels.Writer); ok {
		writer = w
	}
	w := channelhooks.New(channelhooks.Config{
		Store:        s.store,
		Dispatcher:   s.hookDispatcher,
		Lookup:       builtin.HookDefLookup(s.store),
		Defs:         s.ChannelHookDef,
		Writer:       writer,
		Bus:          s.channelBus,
		Scheduler:    sched,
		Owner:        owner,
		Concurrency:  env.ChannelHooksConcurrency,
		PerChannel:   env.ChannelHooksPerChannel,
		MaxWait:      env.ChannelHooksMaxWait,
		MaxBodyBytes: env.ChannelsMaxValueBytes,
		Interruption: interruption,
		Runs:         channelHookRuns{s},
	})
	s.channelHookStats = w.Stats
	return w
}

// channelHookRuns opens the runs a channel hook's asks are filed under. A hook
// run belongs to the tenant whose definition carries the hook, and its user
// is the system: the tenant's operators answer it (they may answer any run in
// their tenant), never the publisher, and never another tenant.
type channelHookRuns struct{ s *Server }

// hookRunUser is a hook run's user: no person, the system.
const hookRunUser = channels.SystemPublisherUserID

func (r channelHookRuns) Open(ctx context.Context, tenant, agent, runID string) (context.Context, string, error) {
	s := r.s
	if s.store == nil {
		return nil, "", fmt.Errorf("hook runs need a store")
	}
	if runID != "" {
		if run, err := s.store.GetRun(ctx, runID); err != nil || run.Status != store.RunRunning {
			runID = "" // gone, or already ended (swept while its worker was away): open a new one
		}
	}
	if runID == "" {
		_, id, err := s.openOrCreateSessionAndRun(ctx, "", agent, tenant, hookRunUser, store.RunIdentity{AgentID: agent, UserID: hookRunUser, TenantID: tenant})
		if err != nil {
			return nil, "", fmt.Errorf("open hook run: %w", err)
		}
		runID = id
	}
	rctx := tools.WithRunID(ctx, runID)
	rctx = tools.WithRunIdentity(rctx, tools.RunIdentityValue{TenantID: tenant, UserID: hookRunUser, AgentID: agent})
	// What happens under the run is its transcript: the pending ask, its
	// answer, and the decisions the hooks made.
	id := runID
	rctx = tools.WithEventEmitter(rctx, func(ev providers.Event) {
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if err := s.store.AppendEvent(context.WithoutCancel(ctx), id, string(ev.Type), payload); err != nil {
			log.Printf("channel hooks: record %s on run %s: %v", ev.Type, id, err)
		}
	})
	return rctx, runID, nil
}

func (r channelHookRuns) Finish(runID string, status store.RunStatus, reason string) {
	if r.s.store == nil {
		return
	}
	if err := r.s.store.FinishRun(context.Background(), runID, status, reason, store.Usage{}, ""); err != nil {
		log.Printf("channel hooks: finish hook run %s: %v", runID, err)
	}
}
