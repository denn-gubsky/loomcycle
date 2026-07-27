package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// internalAgentFixture wires a History tool over a config that declares
// `memory/extractor` internal, and seeds one ordinary chat plus one extractor
// chat under the same user. Both are titled so `search` can reach them.
func internalAgentFixture(t *testing.T) (*History, string, string) {
	t.Helper()
	h, s := historyFixture(t)
	h.Cfg = &config.Config{Agents: map[string]config.AgentDef{
		"chat":             {},
		"memory/extractor": {Internal: true},
	}}
	ctx := context.Background()
	human := seedChat(t, s, "t1", "chat", "alice")
	maintenance := seedChat(t, s, "t1", "memory/extractor", "alice")
	for _, id := range []string{human, maintenance} {
		if err := s.SetSessionMeta(ctx, id, store.SessionMetaPatch{Title: strptr("berlin deploy")}); err != nil {
			t.Fatalf("SetSessionMeta: %v", err)
		}
	}
	return h, human, maintenance
}

func historyChatIDs(t *testing.T, h *History, req string) []string {
	t.Helper()
	res, _ := h.Execute(histCtx([]string{"user"}, "chat", "alice", "t1"), json.RawMessage(req))
	return chatIDs(t, res)
}

// TestHistory_ListHidesInternalAgentChats is the user-visible half of the
// self-consuming-consolidator fix. A consolidation pass spawns one
// `memory/extractor` child per chat it reads, and every child is a session — on
// the live store 7 of the last 8 chats were extractor sessions out of 95 total.
// A person's chat list must not be mostly the runtime's own bookkeeping.
func TestHistory_ListHidesInternalAgentChats(t *testing.T) {
	h, human, maintenance := internalAgentFixture(t)

	ids := historyChatIDs(t, h, `{"op":"list","scope":"user"}`)
	if !contains(ids, human) {
		t.Errorf("list dropped the human chat %s: %v", human, ids)
	}
	if contains(ids, maintenance) {
		t.Errorf("list returned the internal agent's chat %s: %v", maintenance, ids)
	}
}

// TestHistory_SearchHidesInternalAgentChats. search shares filterForScope with
// list, but "shares a helper" is the kind of thing that silently stops being
// true — and a search hit is how an operator would most easily stumble into a
// maintenance chat, since an extractor transcript CONTAINS the chat it read and
// therefore matches the same words.
func TestHistory_SearchHidesInternalAgentChats(t *testing.T) {
	h, human, maintenance := internalAgentFixture(t)

	ids := historyChatIDs(t, h, `{"op":"search","scope":"user","query":"berlin"}`)
	if !contains(ids, human) {
		t.Errorf("search dropped the human chat %s: %v", human, ids)
	}
	if contains(ids, maintenance) {
		t.Errorf("search returned the internal agent's chat %s: %v", maintenance, ids)
	}
}

// TestHistory_IncludeInternalSurfacesMaintenanceChats. Hiding them by default
// is right; hiding them with no way back is not. An operator debugging a bad
// consolidation pass needs to read exactly these chats, and there is nothing
// sensitive in them — the same posture as include_archived.
func TestHistory_IncludeInternalSurfacesMaintenanceChats(t *testing.T) {
	h, human, maintenance := internalAgentFixture(t)

	ids := historyChatIDs(t, h, `{"op":"list","scope":"user","include_internal":true}`)
	if !contains(ids, maintenance) {
		t.Errorf("include_internal did not surface the maintenance chat %s: %v", maintenance, ids)
	}
	if !contains(ids, human) {
		t.Errorf("include_internal dropped the human chat %s: %v", human, ids)
	}

	// And the by-id read is NOT gated on it: a listing with the opt-in is only
	// useful if the reads that follow work without repeating it.
	req := `{"op":"get","scope":"user","session_id":"` + maintenance + `"}`
	res, _ := h.Execute(histCtx([]string{"user"}, "chat", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Errorf("get on a maintenance chat refused: %s — an operator who listed it must be able to open it", res.Text)
	}
}

// TestHistory_ListWithoutConfigHidesNothing pins the nil-Cfg path. Every unit
// test and any caller that constructs a bare History gets nil, and that must
// degrade to the pre-feature behaviour rather than to a panic or — worse — an
// empty exclusion set that silently filters nothing while looking wired.
func TestHistory_ListWithoutConfigHidesNothing(t *testing.T) {
	h, s := historyFixture(t)
	human := seedChat(t, s, "t1", "chat", "alice")
	maintenance := seedChat(t, s, "t1", "memory/extractor", "alice")

	ids := historyChatIDs(t, h, `{"op":"list","scope":"user"}`)
	if !contains(ids, human) || !contains(ids, maintenance) {
		t.Errorf("a History with no Cfg must filter nothing; got %v, want both %s and %s", ids, human, maintenance)
	}
}

// TestWithInternalAgents_UnionsAndDeduplicates covers the helper both tools
// share: the caller's own name must survive alongside the declared set, and a
// caller that IS internal must not appear twice (a duplicate in a NOT IN list is
// harmless in SQL but makes the placeholder run longer than the caller expects).
func TestWithInternalAgents_UnionsAndDeduplicates(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentDef{
		"memory/consolidator": {Internal: true},
		"memory/extractor":    {Internal: true},
		"chat":                {},
	}}
	got := withInternalAgents(cfg, "memory/consolidator", "", "custom")
	want := []string{"custom", "memory/consolidator", "memory/extractor"}
	if len(got) != len(want) {
		t.Fatalf("withInternalAgents = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("withInternalAgents = %v, want %v (sorted, deduped, empties dropped)", got, want)
		}
	}
	if n := len(withInternalAgents(nil, "solo")); n != 1 {
		t.Errorf("nil config yielded %d names, want just the caller's own", n)
	}
}
