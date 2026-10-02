package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A list of chats is a page too: a row count does not bound it, because a
// chat's description can be any length and a caller may ask for 500 rows.
// Inside a run the rows are fitted to a quarter of the model's window. These
// run on a 16K model with nothing configured — unfixed, every row came back.

const rowsBudgetWindow = 16384

func rowsBudgetCtx(scopes []string, agent, user, tenant string) context.Context {
	return tools.WithEffectiveContextWindow(
		tools.WithRunID(histCtx(scopes, agent, user, tenant), "r_reader"), rowsBudgetWindow)
}

// rowsResult runs req and returns the raw result fields, so a size is measured
// on exactly the bytes the model receives.
func rowsResult(t *testing.T, h *History, ctx context.Context, req string) map[string]json.RawMessage {
	t.Helper()
	res, err := h.Execute(ctx, json.RawMessage(req))
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %s", req, err, res.Text)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func rowCount(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	return len(rows)
}

func TestHistoryList_InRunPagesFitAQuarterOfTheEffectiveWindow(t *testing.T) {
	h, s := historyFixture(t)
	for i := 0; i < 10; i++ {
		id := seedChat(t, s, "t1", "agentA", "alice")
		desc := fmt.Sprintf("chat %d ", i) + strings.Repeat("d", 3000)
		if err := s.SetSessionMeta(context.Background(), id, store.SessionMetaPatch{Description: &desc}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := rowsBudgetCtx([]string{"self"}, "agentA", "alice", "t1")

	seen, offset := 0, 0
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		out := rowsResult(t, h, ctx, fmt.Sprintf(`{"op":"list","scope":"self","offset":%d}`, offset))
		n := rowCount(t, out["chats"])
		if len(out["chats"]) > rowsBudgetWindow || n == 0 {
			t.Fatalf("page at offset %d: %d chats in %d characters, want 1+ within %d", offset, n, len(out["chats"]), rowsBudgetWindow)
		}
		seen += n
		if string(out["has_more"]) != "true" {
			break
		}
		if pages == 0 && string(out["truncated"]) != "true" {
			t.Error("a page shortened to fit does not say so")
		}
		if err := json.Unmarshal(out["next_offset"], &offset); err != nil {
			t.Fatal(err)
		}
	}
	if seen != 10 {
		t.Errorf("pages covered %d chats, want all 10", seen)
	}
}

func TestHistoryList_InRunCutsAChatTooBigAlone(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")
	desc := "the launch plan " + strings.Repeat("d", 50000)
	if err := s.SetSessionMeta(context.Background(), id, store.SessionMetaPatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	out := rowsResult(t, h, rowsBudgetCtx([]string{"self"}, "agentA", "alice", "t1"), `{"op":"list","scope":"self"}`)
	if len(out["chats"]) > rowsBudgetWindow || rowCount(t, out["chats"]) != 1 ||
		string(out["truncated"]) != "true" || !strings.Contains(string(out["chats"]), "the launch plan") {
		t.Errorf("one oversized chat: %d characters, truncated=%s; want it cut to %d with its start kept",
			len(out["chats"]), out["truncated"], rowsBudgetWindow)
	}
}

func TestHistoryRelated_InRunFitsAQuarterOfTheEffectiveWindow(t *testing.T) {
	h, s := historyFixture(t)
	emb := eval.NewDeterministicEmbedder(64)
	h.Embedder = emb
	for i := 0; i < 5; i++ {
		id := seedChat(t, s, "t1", "agentA", "alice")
		desc := fmt.Sprintf("postgres chat %d ", i) + strings.Repeat("d", 5000)
		if err := s.SetSessionMeta(context.Background(), id, store.SessionMetaPatch{Description: &desc}); err != nil {
			t.Fatal(err)
		}
		seedEmbedding(t, s, emb, id, "postgres migration schema upgrade")
	}
	out := rowsResult(t, h, rowsBudgetCtx([]string{"self"}, "agentA", "alice", "t1"),
		`{"op":"related","scope":"self","query":"postgres schema migration"}`)
	n := rowCount(t, out["related"])
	if len(out["related"]) > rowsBudgetWindow || n == 0 || n == 5 || string(out["truncated"]) != "true" {
		t.Errorf("related: %d chats in %d characters (truncated=%s), want fewer than 5 within %d",
			n, len(out["related"]), out["truncated"], rowsBudgetWindow)
	}
}

func TestHistorySearch_InRunContentMatchesFitAQuarterOfTheEffectiveWindow(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade")
	h.Embedder = emb
	ctx := rowsBudgetCtx([]string{"user"}, "chat", "u1", "acme")
	for i := 0; i < 4; i++ {
		seedTurn(t, s, "acme", "u1", seedChat(t, s, "acme", "chat", "u1"), "postgres upgrade "+strings.Repeat("x", 6000), emb)
	}

	size := func(out map[string]json.RawMessage) int { return len(out["chats"]) + len(out["matched_turns"]) }
	out := rowsResult(t, h, ctx, `{"op":"search","scope":"user","match":"content","query":"postgres upgrade"}`)
	n := rowCount(t, out["chats"])
	if size(out) > rowsBudgetWindow || n == 0 || n == 4 || string(out["truncated"]) != "true" ||
		rowCount(t, out["matched_turns"]) != n {
		t.Errorf("content search: %d chats in %d characters (truncated=%s), want fewer than 4 within %d, index-aligned",
			n, size(out), out["truncated"], rowsBudgetWindow)
	}
}

func TestHistorySearch_InRunCutsAMatchedTurnTooBigAlone(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade")
	h.Embedder = emb
	seedTurn(t, s, "acme", "u1", seedChat(t, s, "acme", "chat", "u1"), "postgres upgrade "+strings.Repeat("x", 50000), emb)

	out := rowsResult(t, h, rowsBudgetCtx([]string{"user"}, "chat", "u1", "acme"),
		`{"op":"search","scope":"user","match":"content","query":"postgres upgrade"}`)
	if n := len(out["chats"]) + len(out["matched_turns"]); n > rowsBudgetWindow || string(out["truncated"]) != "true" ||
		!strings.Contains(string(out["matched_turns"]), "postgres upgrade") {
		t.Errorf("one oversized matched turn: %d characters (truncated=%s), want it cut to %d with its start kept",
			n, out["truncated"], rowsBudgetWindow)
	}
}
