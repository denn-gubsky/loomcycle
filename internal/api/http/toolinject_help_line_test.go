package http

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The injected tool inventory is what a non-stateful local model reads, so the
// instruction to read a tool's call format goes there, once: naming the
// documented tools the run holds, with a real topic to fetch. It is absent when
// the run cannot call Context, and it never names Context itself.
func TestHelpFirstLine_NamesTheDocumentedToolsTheRunHolds(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	ctxTool := &builtin.Context{Help: set}
	got := helpFirstLine([]tools.Tool{&builtin.Path{}, &builtin.Memory{}, &builtin.Read{}, ctxTool})
	for _, want := range []string{
		"Before your first call to Memory, Path, read its call format with Context op=help",
		`"topic":"Memory/get"`, // the first documented op, in the first documented tool's schema order
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Read") || strings.Contains(got, "call to Context") {
		t.Errorf("line names a tool with no article, or Context itself:\n%s", got)
	}
	// It lands in the cached system prompt, so it depends on the tool set, not
	// on the order the run happens to list its tools in.
	if rev := helpFirstLine([]tools.Tool{ctxTool, &builtin.Read{}, &builtin.Memory{}, &builtin.Path{}}); rev != got {
		t.Errorf("line depends on tool order:\n%s\nvs\n%s", got, rev)
	}
	if got := helpFirstLine([]tools.Tool{&builtin.Path{}, &builtin.Memory{}}); got != "" {
		t.Errorf("run without Context got %q, want no line", got)
	}
}
