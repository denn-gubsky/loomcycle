package builtin

import (
	"strings"
	"testing"
)

// Every agent with the Document or Path tool learns how to cite a document
// in an answer from the description it always sees, not only from a help
// topic it may never open — and the description points at that topic.
func TestToolDescriptions_TeachLoomLinks(t *testing.T) {
	for name, desc := range map[string]string{
		"Document": (&Document{}).Description(),
		"Path":     (&Path{}).Description(),
	} {
		for _, want := range []string{"loom:doc/<document_id>", "?scope=tenant", "Document/citing"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s description does not say %q", name, want)
			}
		}
	}
	if d := (&Document{}).Description(); !strings.Contains(d, "loom:chunk/<chunk_id>") {
		t.Errorf("Document description does not teach the chunk form")
	}
}
