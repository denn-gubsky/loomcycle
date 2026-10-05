package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// skillDefCreateRefusals is how many refusals SkillDef create makes ITSELF,
// outside gateNewSkill: a missing name, the name grammar and the allowlist
// (checked again ahead of the static-name refusal, so that refusal never
// answers a caller without the grant), the static-name refusal (create's
// alone: a team's own skill shadows no static skill), a bad overlay,
// gateNewSkill's own verdict, the description cap (a property of the row, not
// of the skill), and the two store writes.
const skillDefCreateRefusals = 9

// A team's local skills pass gateNewSkill and nothing else of SkillDef create,
// so a gate added to execCreate directly is one a local skill does not pass.
// It counts the refusals execCreate returns: one more means a new check was
// written there.
func TestSkillDefCreate_GatesLiveInGateNewSkill(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "skilldef.go", nil, 0)
	if err != nil {
		t.Fatalf("parse skilldef.go: %v", err)
	}
	var create *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "execCreate" {
			create = fn
		}
	}
	if create == nil {
		t.Fatal("skilldef.go has no execCreate — this test must follow the rename")
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
			if fun.Sel.Name == "gateNewSkill" {
				gated = true
			}
		}
		return true
	})
	if !gated {
		t.Error("execCreate no longer calls gateNewSkill: SkillDef create and a team's local skills would pass different gates")
	}
	if refusals != skillDefCreateRefusals {
		t.Errorf("execCreate returns %d refusals, expected %d. A new check on what a skill definition may be belongs in "+
			"gateNewSkill, which a team's local skills also pass; one written into execCreate is skipped by them. "+
			"If the new refusal really is about SkillDef create alone, update skillDefCreateRefusals and say why there.",
			refusals, skillDefCreateRefusals)
	}
}
