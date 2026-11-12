package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// resumeStepOrder is the order resumePausedRun calls its steps in, with
// "defer" where it queues one of its two cleanups. The phases a resume
// reports at run time (TestResume_PhasesRunInTheirSecurityOrder) sit inside
// these steps; this pins the steps themselves, which the driver's comment
// explains.
var resumeStepOrder = []string{
	"claim", "defer", "endIfOrphaned",
	"restoreRecord", "stampConditions", "loadAgentDef", "replayHistory", "resolveRoute",
	"defaultRunConfig", "buildToolset", "buildPrompt",
	"restoreChildren", "detectFanout", "decideStart", "armReview", "systemSegments",
	"takeSession", "defer", "restampRow", "openRunContext", "registerCancel",
	"goLive", "runIdentity", "recordingEmit", "loopContext", "buildRunOptions", "armResident", "launch",
}

// resumeDriverSteps reads resumePausedRun's body in source order: each
// r.<step>() it calls, and "defer" for each defer statement. What a deferred
// closure calls is the cleanup's, not a step, so closures are not entered.
func resumeDriverSteps(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "resume.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil || fn.Name.Name != "resumePausedRun" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.DeferStmt:
				got = append(got, "defer")
				return false
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "r" {
					got = append(got, sel.Sel.Name)
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("resumePausedRun not found in resume.go: the pin has stopped matching the code it guards")
	}
	return got
}

func TestResume_DriverCallsItsStepsInOrder(t *testing.T) {
	got := resumeDriverSteps(t)
	for _, rule := range []struct{ before, after, why string }{
		{"claim", "endIfOrphaned", "nothing writes to the row before the liveness check and the claim"},
		{"stampConditions", "resolveRoute", "the condition facts are on ctx before the model is asked of the caller's rules"},
		{"replayHistory", "resolveRoute", "an image in the conversation needs a provider that can take it"},
		{"buildToolset", "buildPrompt", "the ACL removes tools before the system prompt is built from them"},
		{"decideStart", "takeSession", "a run refused as idle takes no session"},
		{"takeSession", "restampRow", "the row is restamped only once the run holds its session"},
		{"registerCancel", "goLive", "the run is settled only once it holds its cancel entry"},
		{"buildRunOptions", "armResident", "a resident child's turn cancel replaces the one the options armed"},
		{"armResident", "launch", "the run is launched last"},
	} {
		b, a := slices.Index(got, rule.before), slices.Index(got, rule.after)
		if b < 0 || a < 0 || b > a {
			t.Errorf("%s must run before %s (%s); order was %s", rule.before, rule.after, rule.why, strings.Join(got, " > "))
		}
	}
	if !slices.Equal(got, resumeStepOrder) {
		t.Errorf("resume step order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(resumeStepOrder, " > "))
	}
}
