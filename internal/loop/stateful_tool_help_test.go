package loop

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The stateful tool list shortens each description to one line of 240 bytes.
// The per-run help instruction is appended to the description, so the cut used
// to remove it every time: measured live, a stateful run never saw where a
// tool's call format was. A long description is still shortened; the
// instruction survives whole.
func TestStatefulSystem_ToolListKeepsTheHelpInstruction(t *testing.T) {
	help := `Before your first call to Memory, read the call format for the operation you need: call Context with {"op":"help","topic":"Memory/get"} (put your operation in place of get). It gives the exact arguments and an example.`
	long := strings.Repeat("Persistent storage with many operations and caveats. ", 20)
	spec := providers.ToolSpec{Name: "Memory", Description: long + "\n\n" + help, Help: help}

	var b strings.Builder
	for _, c := range buildStatefulSystem(nil, []providers.ToolSpec{spec}, nil, false) {
		b.WriteString(c.Text)
	}
	text := b.String()
	if !strings.Contains(text, help) {
		t.Errorf("stateful tool list lost the help instruction:\n%s", text)
	}
	if strings.Count(text, "Persistent storage with many operations") > 5 {
		t.Error("the long description was not shortened")
	}
}
