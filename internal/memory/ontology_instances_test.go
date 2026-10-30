package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseInstanceBody_ClassNestingAliasesAndRelations.
func TestParseInstanceBody_ClassNestingAliasesAndRelations(t *testing.T) {
	in := ParseInstanceBody("Compliance Officer", "@a Post\n@identity\n@alt CO\n@nesting manages\n@reports_to Head of Compliance\n- `@notation` X\n")
	if in.Class != "Post" || in.Nesting != "manages" || !reflect.DeepEqual(in.Alt, []string{"CO"}) {
		t.Errorf("parsed = %#v", in)
	}
	if !reflect.DeepEqual(in.Relations, []InstanceRelation{{Kind: "reports_to", Target: "Head of Compliance"}}) {
		t.Errorf("relations = %#v", in.Relations)
	}
	if !strings.Contains(strings.Join(in.Issues, " "), "@notation is not an instance directive") {
		t.Errorf("a scheme directive should be reported: %v", in.Issues)
	}
	ghost := ParseInstanceBody("Ghost", "@reports_to\n- `name` text\n")
	joined := strings.Join(ghost.Issues, "\n")
	for _, want := range []string{"no @a class", "names no instance", "carries no fields"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing issue %q in:\n%s", want, joined)
		}
	}
}
