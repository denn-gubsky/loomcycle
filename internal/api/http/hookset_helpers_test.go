package http

import (
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

var testHookSets sync.Map // *Server → *hooks.Set

// testHooks is a fixed hook chain a test server fires for any run that carries
// no hooks of its own — a stand-in for the hooks an AgentDef would carry, for
// tests about what a hook does rather than how a run came to have it.
func (s *Server) testHooks() *hooks.Set {
	if v, ok := testHookSets.Load(s); ok {
		return v.(*hooks.Set)
	}
	return s.resetTestHooks()
}

// resetTestHooks replaces the chain with an empty one.
func (s *Server) resetTestHooks() *hooks.Set {
	set := hooks.NewSet()
	testHookSets.Store(s, set)
	var allow []string
	if s.cfgHolder != nil {
		allow = s.cfgHolder.Load().Hooks.PrivateHostAllowlist
	}
	d := hooks.NewDispatcherWithPrivateHosts(set, nil, allow)
	d.SetCodeRunner(s.codeHooks)
	s.hookDispatcher = d
	return set
}
