package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A spawn that asks for the object gets it from a real child run: the ids are
// the run's, the answer has no header, and the usage is what that run used.
func TestSubAgent_ResultObjectIsTheChildRunsOwnOutcome(t *testing.T) {
	parent, children, st := spawnParentChildStore(t, "a_parent_object", `{"name":"child","prompt":"hi","result":"object"}`, 1)
	child := children[0]
	var obj struct {
		AgentID    string `json:"agent_id"`
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
		StopReason string `json:"stop_reason"`
		FinalText  string `json:"final_text"`
		Usage      struct {
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
			Provider     string `json:"provider"`
		} `json:"usage"`
	}
	text := parentToolResult(t, st, parent, "tu_spawn")
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("the spawn result is not a JSON object (%v): %s", err, text)
	}
	if obj.AgentID != child.AgentID || obj.RunID != child.ID {
		t.Errorf("object names %s / %s, want the child run %s / %s", obj.AgentID, obj.RunID, child.AgentID, child.ID)
	}
	if obj.Status != "completed" || obj.StopReason != "end_turn" || obj.FinalText != "child answer" {
		t.Errorf("object = %+v, want the completed child's bare answer", obj)
	}
	// The child's row is what a second lookup would have read; the object
	// must agree with it, so that lookup is no longer needed.
	row, err := st.GetRun(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Usage.InputTokens != row.InputTokens || obj.Usage.OutputTokens != row.OutputTokens || row.OutputTokens == 0 || obj.Usage.Provider != row.Provider {
		t.Errorf("object usage = %+v, row = %d in / %d out on %q; want them equal and non-zero",
			obj.Usage, row.InputTokens, row.OutputTokens, row.Provider)
	}
}

// The outcome a spawn asked for is its own child's. A run that child starts
// in turn must not report into it.
func TestSubAgent_ResultObjectIsNotOverwrittenByAGrandchild(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"parent":     {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent"},
		"child":      {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "child"},
		"grandchild": {Model: "stub-model", Tools: []string{}, SystemPrompt: "grandchild"},
	}
	call := func(id, input string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Agent", Input: json.RawMessage(input)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	say := func(text string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventText, Text: text},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}
	prov := &scriptedProvider{defaultS: say("parent done"), scripts: [][]providers.Event{
		call("tu_spawn", `{"name":"child","prompt":"p","result":"object"}`),
		call("tu_inner", `{"name":"grandchild","prompt":"p"}`),
		say("the grandchild's answer"),
		say("the child's answer"),
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "object.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","agent_id":"a_parent_grand","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	parent, err := st.GetRunByAgentID(context.Background(), "a_parent_grand")
	if err != nil {
		t.Fatal(err)
	}
	children, err := st.ListRunsByParentAgentID(context.Background(), "a_parent_grand")
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %v, %v", children, err)
	}
	var obj struct {
		RunID     string `json:"run_id"`
		FinalText string `json:"final_text"`
	}
	text := parentToolResult(t, st, parent, "tu_spawn")
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("not an object (%v): %s", err, text)
	}
	if obj.RunID != children[0].ID || obj.FinalText != "the child's answer" {
		t.Errorf("object = %+v, want the child's own run %s and answer — not its grandchild's", obj, children[0].ID)
	}
}
