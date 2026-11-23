package http

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
)

// RFC GW §5.7: for a code-js agent a cell is a recorded tool call. Each
// replay turn of the agent re-runs its code from the top with the recorded
// results, so a cell that appended to a file would append again were it run
// again.
func TestWorkspaceRun_ACodeJSReplayDoesNotRunACellAgain(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  Workspace({op: "open", name: "work"});
  Workspace({op: "run_cell", name: "work", code: "open('log.txt', 'a').write('once\\n')\ncount = 1"});
  Workspace({op: "run_cell", name: "work", code: "count += 1"});
  var r = Workspace({op: "read", name: "work", path: "log.txt"});
  return { final_text: r };
}`
	if err := os.WriteFile(filepath.Join(root, "coder", "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := map[string]config.AgentDef{"coder": {Provider: "code-js", Model: "code-js", Tools: []string{"Workspace"}, Workspace: wsAll}}
	f := newWSRunFixture(t, agents, codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 30 * time.Second}))
	var final string
	cb := f.callbacks()
	inner := cb.OnEvent
	cb.OnEvent = func(ev providers.Event) {
		inner(ev)
		if ev.Type == providers.EventText {
			final += ev.Text
		}
	}
	if err := f.srv.RunOnce(context.Background(), f.input("coder"), cb); err != nil {
		t.Fatal(err)
	}
	if n := len(f.results()); n != 4 {
		t.Errorf("tool results = %d, want 4 (one per op)", n)
	}
	if strings.Count(final, "once") != 1 {
		t.Errorf("final text = %q, want the line written once", final)
	}
}
