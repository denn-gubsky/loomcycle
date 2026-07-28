package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func textMsg(role, text string) providers.Message {
	return providers.Message{Role: role, Content: []providers.ContentBlock{{Type: "text", Text: text}}}
}

// TestConversationFromMessages_DropsToolTraffic is the v1.36.5 lesson enforced one
// layer down. Handed loomcycle's own scaffolding, the extractor extracted the
// scaffolding — so the bridge banks dialogue and nothing else.
//
// Filtering is by block TYPE, never by pattern: a user turn that happens to
// contain JSON is content and must survive verbatim.
func TestConversationFromMessages_DropsToolTraffic(t *testing.T) {
	msgs := []providers.Message{
		textMsg("user", "I run ROCm on an AMD card"),
		{Role: "assistant", Content: []providers.ContentBlock{
			{Type: "text", Text: "Noted."},
			{Type: "tool_use", ToolName: "Bash", ToolInput: json.RawMessage(`{"cmd":"rocminfo"}`)},
		}},
		// A pure tool_result turn: no dialogue at all → dropped entirely.
		{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", Text: "gfx1100"}}},
		// A user turn that legitimately contains JSON — content, not plumbing.
		textMsg("user", `my config is {"num_ctx": 131072}`),
		// Thinking is run mechanics.
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "thinking", Text: "hmm"}}},
	}

	got := ConversationFromMessages(msgs)
	if len(got) != 3 {
		t.Fatalf("got %d turns, want 3 (two user turns + one assistant): %+v", len(got), got)
	}
	if got[0].Content != "I run ROCm on an AMD card" || got[0].Role != "user" {
		t.Errorf("turn 0 = %+v", got[0])
	}
	// The assistant turn keeps its text and loses the tool_use beside it.
	if got[1].Content != "Noted." {
		t.Errorf("turn 1 = %q, want just the text half of a mixed turn", got[1].Content)
	}
	if got[2].Content != `my config is {"num_ctx": 131072}` {
		t.Errorf("a user turn containing JSON was altered: %q", got[2].Content)
	}
	blob, _ := json.Marshal(got)
	for _, banned := range []string{"rocminfo", "gfx1100", "tool_use", "tool_result", "hmm"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("banked payload leaked run mechanics %q: %s", banned, blob)
		}
	}
}

// TestConversationFromMessages_ToolOnlySpanIsEmpty: a span of pure tool traffic
// yields nothing, which BankSpan then treats as a legitimate nothing-to-bank
// rather than enqueueing an empty conversation for the extractor to puzzle over.
func TestConversationFromMessages_ToolOnlySpanIsEmpty(t *testing.T) {
	msgs := []providers.Message{
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolName: "Read"}}},
		{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", Text: "file contents"}}},
	}
	if got := ConversationFromMessages(msgs); len(got) != 0 {
		t.Errorf("got %d turns from a tool-only span, want 0: %+v", len(got), got)
	}
}

// TestBankSpan_RefusesWithoutAWritableScope: an agent with no resolvable user
// scope must get a NAMED refusal, not a silent no-op. Consolidation drains user
// scopes only, so banking anywhere else would enqueue a row nothing ever reads —
// and "banked and silently forgotten" is worse than "not banked".
func TestBankSpan_RefusesWithoutAWritableScope(t *testing.T) {
	_, err := BankSpan(context.Background(), nil, BankSpanRequest{
		Messages: []LayerMessage{{Role: "user", Content: "x"}},
	})
	if err == nil {
		t.Fatal("banking with no store succeeded")
	}
	// And with a store but no scope id, the refusal names the scope.
	_, err = BankSpan(context.Background(), stubPendingStore{}, BankSpanRequest{
		Messages: []LayerMessage{{Role: "user", Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("err = %v, want a refusal naming the missing scope", err)
	}
}

// TestBankSpan_EmptySpanIsNotAnError: nothing to bank is a normal outcome (the
// converter dropped a tool-only span), distinguishable from a failure because it
// returns neither an id nor an error.
func TestBankSpan_EmptySpanIsNotAnError(t *testing.T) {
	id, err := BankSpan(context.Background(), stubPendingStore{}, BankSpanRequest{ScopeID: "u1"})
	if err != nil || id != "" {
		t.Errorf("got id=%q err=%v, want both empty", id, err)
	}
}

// TestBankSpan_OversizedSpanIsRefusedNotTruncated: the guard REFUSES and names the
// size. Truncating would silently drop part of a conversation, which is the exact
// failure mode several releases went into eliminating, and it would be invisible.
func TestBankSpan_OversizedSpanIsRefusedNotTruncated(t *testing.T) {
	st := &recordingPendingStore{}
	huge := strings.Repeat("x", BankSpanMaxBytes+1)
	id, err := BankSpan(context.Background(), st, BankSpanRequest{
		ScopeID:  "u1",
		Messages: []LayerMessage{{Role: "user", Content: huge}},
	})
	if err == nil {
		t.Fatal("an oversized span was accepted")
	}
	if id != "" || st.calls != 0 {
		t.Errorf("an oversized span reached the store (id=%q calls=%d) — it must be refused before the write, not truncated into one", id, st.calls)
	}
	if !strings.Contains(err.Error(), "unaffected") {
		t.Errorf("err = %v; it should say the compaction itself is unaffected, since that is the operator's first question", err)
	}
}

// TestBankSpan_StampsCompactionOriginAndProvenance: the row must carry the origin
// that makes a consolidated fact traceable to a compaction, plus the run/session
// that become its origin link.
func TestBankSpan_StampsCompactionOriginAndProvenance(t *testing.T) {
	st := &recordingPendingStore{}
	id, err := BankSpan(context.Background(), st, BankSpanRequest{
		TenantID: "acme", Scope: "user", ScopeID: "u1",
		RunID: "run-1", SessionID: "sess-1",
		Messages: []LayerMessage{{Role: "user", Content: "I use ROCm"}},
		Metadata: map[string]string{"source": "compaction"},
	})
	if err != nil {
		t.Fatalf("BankSpan: %v", err)
	}
	if id == "" || st.calls != 1 {
		t.Fatalf("id=%q calls=%d, want an id and exactly one enqueue", id, st.calls)
	}
	row := st.last
	if row.Origin != "compaction" {
		t.Errorf("origin = %q, want compaction — otherwise the fact is indistinguishable from a scheduled pass", row.Origin)
	}
	if row.TenantID != "acme" || row.ScopeID != "u1" || row.SourceRunID != "run-1" || row.SourceSessionID != "sess-1" {
		t.Errorf("row = %+v, want the tenant/scope/run/session threaded through", row)
	}
	if row.ID != id {
		t.Errorf("returned id %q does not match the enqueued row %q", id, row.ID)
	}
	// The payload must be the shape the consolidator parses.
	var p PendingPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("payload is not a PendingPayload: %v (%s)", err, row.Payload)
	}
	if len(p.Messages) != 1 || p.Messages[0].Content != "I use ROCm" || p.Metadata["source"] != "compaction" {
		t.Errorf("payload = %+v", p)
	}
}

// The stubs embed store.Store (nil) so only the one method BankSpan calls is
// defined — implementing the full interface in a test would be pages of noise,
// and any accidental call to another method panics loudly rather than silently
// returning a zero value.
type stubPendingStore struct{ store.Store }

func (stubPendingStore) MemoryPendingEnqueue(context.Context, store.MemoryPendingRow) error {
	return nil
}

type recordingPendingStore struct {
	store.Store
	calls int
	last  store.MemoryPendingRow
}

func (r *recordingPendingStore) MemoryPendingEnqueue(_ context.Context, row store.MemoryPendingRow) error {
	r.calls++
	r.last = row
	return nil
}
