package loop_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runScript runs js as the code agent "orch" through the real loop with the
// given user content and tools.
func runScript(t *testing.T, js string, content []loop.PromptContentBlock, ts ...tools.Tool) (loop.RunResult, error) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "orch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	return loop.Run(context.Background(), loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: root, RunTimeout: 5 * time.Second}),
		Model:      "code-js",
		AgentName:  "orch",
		Tools:      ts,
		Dispatcher: tools.NewDispatcher(ts),
		Segments:   []loop.PromptSegment{{Role: "user", Content: content}},
	})
}

// A script reads its run's untrusted blocks as data: raw, in order, with the
// kind each is rendered under, apart from the trusted text. It reads the same
// value on every replay turn — the script below puts it into each of three
// tool calls, so a turn that read anything else would end the run with a
// replay divergence instead of an answer.
func TestCodeJS_InputUntrustedIsTheRunsRawBlocksOnEveryReplayTurn(t *testing.T) {
	js := `
function run(input) {
  var seen = JSON.stringify(input.untrusted);
  mcp__test__echo({ turn: 1, untrusted: seen });
  mcp__test__echo({ turn: 2, untrusted: JSON.stringify(input.untrusted) });
  mcp__test__echo({ turn: 3, untrusted: JSON.stringify(input.untrusted) });
  return { final_text: seen + "|" + input.prompt };
}`
	echo := &echoTool{}
	res, err := runScript(t, js, []loop.PromptContentBlock{
		{Type: "trusted-text", Text: "Score it."},
		{Type: "untrusted-block", Kind: "web_content", Text: "a <b>page</b> & more"},
		{Type: "untrusted-block", Kind: "system", Text: "second"},
	}, echo)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantUntrusted := `[{"kind":"web_content","text":"a <b>page</b> & more"},{"kind":"untrusted","text":"second"}]`
	got, prompt, _ := strings.Cut(res.FinalText, "|")
	if got != wantUntrusted {
		t.Errorf("input.untrusted = %s\nwant %s", got, wantUntrusted)
	}
	// input.prompt is as it was: the trusted text and the fenced copies.
	if !strings.HasPrefix(prompt, "Score it.<web_content>\n") || !strings.Contains(prompt, "‹b>page") {
		t.Errorf("input.prompt changed: %q", prompt)
	}
	if echo.count() != 3 {
		t.Fatalf("the tool was dispatched %d times, want 3", echo.count())
	}
	for i, raw := range echo.calls {
		var in struct{ Untrusted string }
		if err := json.Unmarshal(raw, &in); err != nil || in.Untrusted != wantUntrusted {
			t.Errorf("turn %d read %q (err %v), want the same value as turn 1", i+1, in.Untrusted, err)
		}
	}
}

// A run with no untrusted block still gets an array, so a script reads
// .length without a guard.
func TestCodeJS_InputUntrustedIsAnEmptyArrayWhenTheRunHasNone(t *testing.T) {
	js := `function run(input) { return { final_text: Array.isArray(input.untrusted) + ":" + input.untrusted.length }; }`
	res, err := runScript(t, js, []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalText != "true:0" {
		t.Errorf("input.untrusted is not an empty array: %q", res.FinalText)
	}
}
