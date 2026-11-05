package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// modelResolutionCallers classifies every function in this package that
// resolves a model (RFC FY Part E). "inference" leads to a model call and
// asks the caller's `use tier:`/`use model:` (through resolveAgentDef,
// resolveAgent or resolveInference, which ask unless ctx is report-only);
// "report" only says where a run would land and marks its ctx
// withReportOnlyResolution (or calls Cascade, which asks nothing);
// "resolver" is the asking machinery itself. A new caller fails
// TestModelUse_EveryResolutionIsClassified until it is classified here.
var modelResolutionCallers = map[string]string{
	"resolveAgent":              "resolver",
	"resolveAgentDef":           "resolver",
	"resolveInference":          "resolver",
	"RunOnce":                   "inference",
	"prepareSubRunValues":       "inference",
	"resumePausedRun":           "inference",
	"resolveAfterQueue":         "inference",
	"queueMoveFn":               "inference",
	"reResolveOnOperatorTurnFn": "inference",
	"fallbackForRun":            "inference",
	"compactRunWithSource":      "inference",
	"RecapSession":              "inference",
	"computeReplayCompaction":   "inference",
	"ResolveOCRModel":           "inference",
	"resolveGatewayRequest":     "inference",
	"resolveVisionModel":        "inference",
	"ResolveAgentProvider":      "report",
	"agentContextWindow":        "report",
	"effectiveCtxFor":           "report",
	"keyableProvidersFor":       "report",
	"providerForModel":          "report",
	"buildConfig":               "report",
	"handleRouting":             "report",
}

func TestModelUse_EveryResolutionIsClassified(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	asking := map[string]bool{}        // calls a resolution that asks unless marked
	marksReport := map[string]bool{}   // calls withReportOnlyResolution
	directResolve := map[string]bool{} // calls s.resolver.Resolve itself, asking nothing
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "withReportOnlyResolution" {
					marksReport[fn.Name.Name] = true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "resolveAgentDef", "resolveAgent", "resolveInference":
					asking[fn.Name.Name] = true
				case "Resolve", "Cascade":
					// s.resolver.Resolve / .Cascade only (not the search resolver).
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "resolver" {
						return true
					}
					if sel.Sel.Name == "Resolve" {
						directResolve[fn.Name.Name] = true
					}
				default:
					return true
				}
				found[fn.Name.Name] = true
				return true
			})
		}
	}
	if len(found) < 10 {
		t.Fatalf("only %d resolution callers found: the scan is broken", len(found))
	}
	var unclassified []string
	for fn := range found {
		if _, ok := modelResolutionCallers[fn]; !ok {
			unclassified = append(unclassified, fn)
		}
	}
	sort.Strings(unclassified)
	for _, fn := range unclassified {
		t.Errorf("%s resolves a model and is not classified in modelResolutionCallers (inference asks the caller's use rules; report marks its ctx withReportOnlyResolution)", fn)
	}
	for fn, class := range modelResolutionCallers {
		switch {
		case class == "report" && asking[fn] && !marksReport[fn]:
			t.Errorf("%s is classified report but resolves without withReportOnlyResolution, so it asks the caller's use rules", fn)
		case class == "inference" && directResolve[fn]:
			t.Errorf("%s leads to a model call but calls s.resolver.Resolve directly, which asks no use rule; go through resolveInference", fn)
		case class == "inference" && marksReport[fn]:
			t.Errorf("%s is classified inference but marks its resolution report-only, so it asks nothing", fn)
		}
		if !found[fn] {
			t.Errorf("modelResolutionCallers lists %s, which resolves no model", fn)
		}
	}
}
