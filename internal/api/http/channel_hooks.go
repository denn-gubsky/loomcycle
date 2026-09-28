package http

import (
	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// NewChannelHookWorker builds the worker that decides the messages of
// channels that carry hooks, from the server's own collaborators: its store,
// its hook dispatcher (so a channel hook's webhook dials and credentials
// follow the same rules as a run's), the channel writer for decision records,
// and the channel bus. It also wires the worker's counters into /metrics.
// owner names this replica on the worker's leases.
func (s *Server) NewChannelHookWorker(owner string, sched *channels.Scheduler) *channelhooks.Worker {
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
	})
	s.channelHookStats = w.Stats
	return w
}
