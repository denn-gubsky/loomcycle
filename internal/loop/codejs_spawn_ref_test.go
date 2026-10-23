package loop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A script forwards its run's untrusted input by reference. The recorded
// argument is the reference, which is the same on every re-execution, so the
// second spawn — reached only by replaying the first — is matched and not a
// divergence. The children get the text itself, in the order written, mixed
// with the script's own entries; a bare string "input" is still five
// characters of text.
func TestCodeJS_SpawnForwardsInputByReferenceAcrossReplay(t *testing.T) {
	js := `
function run(input) {
  var a = Agent.spawn({name: "kid", prompt: "judge", untrusted: [{text: "first", kind: "tool_output"}, {ref: "input"}, "last"]});
  var b = Agent.spawn({name: "kid", prompt: "judge", untrusted: "input"});
  var c = Agent.spawn({name: "kid", prompt: "judge", untrusted: [{ref: "input"}]});
  return { final_text: String(a) + "|" + String(b) + "|" + String(c) };
}`
	seen := func(ctx context.Context, _, _, _ string) (string, error) {
		var parts []string
		for _, u := range tools.SpawnUntrusted(ctx) {
			parts = append(parts, u.Kind+":"+u.Text)
		}
		return strings.Join(parts, ","), nil
	}
	res, err := runScript(t, js, []loop.PromptContentBlock{
		{Type: "trusted-text", Text: "go"},
		{Type: "untrusted-block", Kind: "user_input", Text: "evidence <one>"},
		{Type: "untrusted-block", Text: "evidence two"},
	}, &builtin.AgentTool{Run: seen})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	forwarded := "user_input:evidence <one>,untrusted:evidence two"
	want := "tool_output:first," + forwarded + ",:last|:input|" + forwarded
	if res.FinalText != want {
		t.Errorf("the children saw %q\nwant            %q", res.FinalText, want)
	}
}
