package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Every RunStatus declared in store.go is either one of the two live states or
// in TerminalRunStatuses. Parsed from source, not restated: a list here would
// be a second copy, and the eleven hand-written copies of "the terminal
// statuses" this list replaced are exactly how a new status goes unhandled.
func TestRunStatus_EveryDeclaredStatusIsLiveOrTerminal(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "store.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{"RunConfigured": true, "RunRunning": true}
	terminal := map[RunStatus]bool{}
	for _, s := range TerminalRunStatuses {
		terminal[s] = true
	}
	values := map[string]RunStatus{
		"RunConfigured": RunConfigured, "RunRunning": RunRunning, "RunCompleted": RunCompleted,
		"RunFailed": RunFailed, "RunCancelled": RunCancelled, "RunRejected": RunRejected,
	}
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != "RunStatus" {
			return true
		}
		for _, name := range vs.Names {
			found++
			if live[name.Name] {
				continue
			}
			v, known := values[name.Name]
			if !known {
				t.Errorf("RunStatus %s is new: decide whether it is terminal, add it to TerminalRunStatuses (or to the live set here) and to this test's value map", name.Name)
				continue
			}
			if !terminal[v] {
				t.Errorf("RunStatus %s is neither live nor in TerminalRunStatuses", name.Name)
			}
		}
		return true
	})
	if found < 6 {
		t.Fatalf("parsed only %d RunStatus constants — the pattern stopped matching", found)
	}
}
