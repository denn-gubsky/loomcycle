package main

import (
	"strings"
	"testing"
)

// Every chat agent can reach the user's past chats. A chat agent is where a user
// asks "what did we decide last week", and without History the only answer is
// what consolidation happened to distil into Memory. No history_scope is set, so
// the grant is the default: the caller's own chats, nothing wider.
func TestChatBundle_AgentsHoldHistory(t *testing.T) {
	cfg := chatBundleConfig(t)
	found := 0
	for name, agent := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		found++
		held := false
		for _, tl := range agent.Tools {
			if tl == "History" {
				held = true
			}
		}
		if !held {
			t.Errorf("agent %q does not hold History", name)
		}
		if len(agent.HistoryScope) != 0 {
			t.Errorf("agent %q sets history_scope %v; the default (the user's own chats) is the intended grant", name, agent.HistoryScope)
		}
	}
	if found == 0 {
		t.Fatal("no chat/* agents in the bundle — the test is asserting nothing")
	}
}
