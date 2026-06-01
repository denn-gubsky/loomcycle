package codejs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// echoTool is a minimal real tool registered in a real Dispatcher. It records
// each dispatch so the test can prove the JS tool calls flowed through the
// loop's Dispatcher.Execute (NOT a provider-internal dispatcher).
type echoTool struct {
	mu    sync.Mutex
	calls []json.RawMessage
}

func (e *echoTool) Name() string                 { return "mcp__test__echo" }
func (e *echoTool) Description() string          { return "echoes its input" }
func (e *echoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (e *echoTool) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	e.mu.Lock()
	e.calls = append(e.calls, raw)
	e.mu.Unlock()
	return tools.Result{Text: `{"echoed": true}`}, nil
}

func (e *echoTool) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// The end-to-end proof: the REAL agent loop (internal/loop) drives the code-js
// provider through two JS tool calls, each dispatched via the loop's own
// Dispatcher.Execute, with the result returned synchronously into the JS. This
// exercises the full loop-driven suspend/resume handshake against the actual
// loop — not the in-package drive harness — closing the load-bearing path.
func TestCodeJS_RealLoop_DispatchesThroughLoop(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "echoer")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	js := `
function run(input) {
  var a = mcp__test__echo({ n: 1, who: input.metadata.user_id });
  var b = mcp__test__echo({ n: 2 });
  return { final_text: "echoed=" + a.echoed + "," + b.echoed };
}`
	if err := os.WriteFile(filepath.Join(agentDir, "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 5 * time.Second})
	echo := &echoTool{}
	disp := tools.NewDispatcher([]tools.Tool{echo})

	res, err := loop.Run(context.Background(), loop.RunOptions{
		Provider:   prov,
		Model:      "code-js",
		AgentName:  "echoer",
		Tools:      []tools.Tool{echo},
		Dispatcher: disp,
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", res.StopReason)
	}
	if echo.count() != 2 {
		t.Fatalf("echo tool dispatched %d times via the loop, want 2", echo.count())
	}
	if !strings.Contains(res.FinalText, "echoed=true,true") {
		t.Errorf("final text = %q, want it to contain echoed=true,true", res.FinalText)
	}
}
