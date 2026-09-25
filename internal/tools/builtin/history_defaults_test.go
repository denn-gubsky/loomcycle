package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An omitted scope follows the grant. It was always `self`, while the default
// grant is `user`, so a call with no scope — which models kept making — was
// refused for every agent on the default. `user` when granted, else `self`.
func TestHistory_OmittedScopeFollowsTheGrant(t *testing.T) {
	h, _ := historyFixture(t)
	for _, c := range []struct {
		granted []string
		want    string
	}{
		{[]string{"user"}, "user"},
		{[]string{"self", "user"}, "user"},
		{[]string{"self"}, "self"},
	} {
		got, err := h.authorizedScope(histCtx(c.granted, "agentA", "u1", "t1"), "")
		if err != nil || got != c.want {
			t.Errorf("granted %v: scope %q, err %v; want %q", c.granted, got, err, c.want)
		}
	}
}

// Inside a run, a markdown transcript is cut to fit a context window and says
// how to read the rest; off-run (an MCP client, the Web UI) it is returned whole.
func TestHistory_MarkdownIsCappedOnlyInsideARun(t *testing.T) {
	h, s := historyFixture(t)
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "tok-user-1")
	run, err := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "tok-user-1", TenantID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a long tool result line. ", 2000) // ~50K chars
	payload, _ := json.Marshal(map[string]any{"type": "tool_result", "text": big})
	if err := s.AppendEvent(bg, run.ID, "tool_result", payload); err != nil {
		t.Fatal(err)
	}
	get := func(ctx context.Context) map[string]any {
		t.Helper()
		res, _ := h.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"markdown"}`, id)))
		if res.IsError {
			t.Fatalf("get: %s", res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	base := histCtx([]string{"self"}, "agentA", "tok-user-1", "t1")

	off := get(base)
	if off["truncated"] != nil || len(off["markdown"].(string)) < 40000 {
		t.Errorf("off-run export was cut (%d chars)", len(off["markdown"].(string)))
	}
	in := get(tools.WithRunID(base, "r_reader"))
	if in["truncated"] != true || len(in["markdown"].(string)) != historyMarkdownInRunCap ||
		!strings.Contains(in["note"].(string), "format=conversation") {
		t.Errorf("in-run export: truncated=%v len=%d note=%v", in["truncated"], len(in["markdown"].(string)), in["note"])
	}
}

// Asked about nothing, documents_summary says what to pass instead of
// returning an empty list that reads as "no documents exist".
func TestDocumentsSummary_NoArgumentsIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, res := docExec(t, d, ctx, `{"op":"documents_summary","scope":"user"}`)
	if !res.IsError || !strings.Contains(res.Text, "pass document_ids") || !strings.Contains(res.Text, "query_documents") {
		t.Errorf("documents_summary with no arguments: %q", res.Text)
	}
}
