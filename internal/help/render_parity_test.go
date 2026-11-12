package help

import (
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
)

// TestRenderExamples_AgreesWithLoadedTopics holds the two readers of the
// embedded corpus to one answer for every bundled topic: the {{help:<topic>}}
// renderer resolves a name to the same article LoadSet serves under it (tool
// article before topic, operation articles under <Tool>/<op>), and renders
// exactly the calls Topic.Examples returns — same count, same tool, same
// order. A topic LoadSet has with no examples is ErrNoExamples to the renderer.
func TestRenderExamples_AgreesWithLoadedTopics(t *testing.T) {
	set, err := LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	rendered := 0
	for _, topic := range set.All() {
		exs, err := topic.Examples()
		if err != nil {
			t.Fatalf("%s: Examples: %v", topic.Name, err)
		}
		got, rerr := builtin.RenderExamples(topic.Name)
		if len(exs) == 0 {
			if !errors.Is(rerr, builtin.ErrNoExamples) {
				t.Errorf("%s: no examples loaded, renderer err = %v, want ErrNoExamples", topic.Name, rerr)
			}
			continue
		}
		if rerr != nil {
			t.Errorf("%s: renderer: %v", topic.Name, rerr)
			continue
		}
		lines := strings.Split(got, "\n")
		if len(lines) != len(exs) {
			t.Errorf("%s: rendered %d lines, loaded %d examples", topic.Name, len(lines), len(exs))
			continue
		}
		for i, ex := range exs {
			if !strings.HasPrefix(lines[i], ex.Tool+" {") {
				t.Errorf("%s: line %d = %q, want a %s call", topic.Name, i, lines[i], ex.Tool)
			}
		}
		rendered++
	}
	if rendered < 100 {
		t.Fatalf("only %d topics rendered examples; the corpus walk is not reaching the articles", rendered)
	}
}
