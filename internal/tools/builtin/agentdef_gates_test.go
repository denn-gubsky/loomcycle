package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// execCreateRefusals is how many refusals AgentDef create makes ITSELF, outside
// gateNewDef: a missing name, gateNewDef's own verdict, the description cap
// (a property of the row, not of the definition), the clash with a team's own
// agent of the same full name (which that agent, passing gateNewDef under
// this name, must not trip over itself), and the two store writes.
const execCreateRefusals = 6

// A team's local agents pass gateNewDef and nothing else of AgentDef create.
// So a gate added to execCreate directly is one a local agent does not pass —
// the drift this pins. It counts the refusals execCreate returns: one more
// means a new check was written there.
func TestAgentDefCreate_GatesLiveInGateNewDef(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "agentdef.go", nil, 0)
	if err != nil {
		t.Fatalf("parse agentdef.go: %v", err)
	}
	var create *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "execCreate" {
			create = fn
		}
	}
	if create == nil {
		t.Fatal("agentdef.go has no execCreate — this test must follow the rename")
	}
	refusals, gated := 0, false
	ast.Inspect(create.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "errResult" {
				refusals++
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "gateNewDef" {
				gated = true
			}
		}
		return true
	})
	if !gated {
		t.Error("execCreate no longer calls gateNewDef: AgentDef create and a team's local agents would pass different gates")
	}
	if refusals != execCreateRefusals {
		t.Errorf("execCreate returns %d refusals, expected %d. A new check on what an agent definition may be belongs in "+
			"gateNewDef, which a team's local agents also pass; one written into execCreate is skipped by them. "+
			"If the new refusal really is about AgentDef create alone, update execCreateRefusals and say why there.",
			refusals, execCreateRefusals)
	}
}
