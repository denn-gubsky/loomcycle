package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// eventsOf returns one run's stored events of a type, as raw payloads.
func eventsOf(t *testing.T, st store.Store, run store.Run, typ string) []string {
	t.Helper()
	all, err := st.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	var out []string
	for _, e := range all {
		if e.RunID == run.ID && e.Type == typ {
			out = append(out, string(e.Payload))
		}
	}
	return out
}

// A code-js run re-entered from the store reads the input.untrusted it read
// before the pause. The script puts the value into a tool argument, and the
// stored transcript holds that call as it was recorded: a resumed run that
// read anything else would not match it and would end in a replay divergence.
func TestResume_CodeJSRunReadsTheSameUntrustedInput(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "orch"), 0o755); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  Probe({ u: JSON.stringify(input.untrusted) });
  return { final_text: "read " + JSON.stringify(input.untrusted) };
}`
	if err := os.WriteFile(filepath.Join(root, "orch", "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "code-js"
	cfg.Agents = map[string]config.AgentDef{
		"orch": {Provider: "code-js", Model: "code-js", Tools: []string{"Probe"}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 5 * time.Second})
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{&untrustedProbeTool{}}, concurrency.New(8, 8, 5*time.Second), st)

	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "orch", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_resume_untrusted", UserID: "alice", Model: "code-js"})
	if err != nil {
		t.Fatal(err)
	}
	const read = `[{"kind":"web_content","text":"a <b>page</b>"},{"kind":"untrusted","text":"second"}]`
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
		{Type: "trusted-text", Text: "Score it."},
		{Type: "untrusted-block", Kind: "web_content", Text: "a <b>page</b>"},
		{Type: "untrusted-block", Kind: "nonsense", Text: "second"},
	}}})
	arg, _ := json.Marshal(map[string]string{"u": read})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Probe", Input: arg},
	})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{
		Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Probe"}, Text: "ok",
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status == store.RunCompleted {
			break
		}
		if got.Status == store.RunFailed {
			t.Fatalf("the resumed run failed — it did not read the input.untrusted it recorded: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the resumed run did not complete (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	texts := strings.Join(eventsOf(t, st, run, "text"), "\n")
	wantJSON, _ := json.Marshal("read " + read) // as the event payload escapes it
	if !strings.Contains(texts, strings.Trim(string(wantJSON), `"`)) {
		t.Errorf("the resumed run answered %s\nwant it to contain %s", texts, wantJSON)
	}
}
