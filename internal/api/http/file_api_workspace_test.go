package http

import (
	"strings"
	"testing"
)

// The File twin refuses a workspace reference in any field, for any op,
// before the run or its inbox is looked at: a workspace belongs to the run
// that opened it (RFC GW P2 §3.2), and a session outside it has none.
func TestFileTwin_RefusesAWorkspaceReference(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	f.capture(t, runID, "acme", "report.pdf", "http", []byte("%PDF-1.7\nhello"), true)
	for _, in := range []map[string]any{
		{"run_id": runID, "op": "stat", "path": "workspace:w:a.txt"},
		{"run_id": runID, "op": "ls", "path": " workspace:w:"},
		{"run_id": runID, "op": "keep", "from": "workspace:w:a.txt", "to": "volume:work:a.txt"},
		{"run_id": runID, "op": "keep", "from": "volume:inbox:documents/report.pdf", "to": "workspace:w:a.pdf"},
		{"op": "render", "document": "d1", "format": "md", "to": "workspace:w:out.md"},
	} {
		_, text, isErr := f.call(t, twinAdmin, in)
		if !isErr || !strings.Contains(text, "workspace references are not available here") {
			t.Errorf("%v = %q, want the workspace refusal", in, text)
		}
	}
}
