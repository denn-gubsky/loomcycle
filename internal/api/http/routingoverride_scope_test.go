package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// The routing override applies to the RUN'S OWN TURN and to nothing else.
//
// resolveAgentDef has seven callers and they are not one kind. Four serve the
// run's turn; three are AUXILIARY model calls the runtime makes on the run's
// behalf — a compaction summary, a session recap, a replay compaction. Those
// must not inherit the override: a run steered onto a frontier model must not
// silently drag its summaries there too, and `compaction.model` already exists
// so summaries can run CHEAPER than the conversation. Overriding it by accident
// would be a cost decision nobody made.
//
// Asserted structurally rather than behaviourally because the risk is a future
// edit, not today's behaviour: someone threading the override "for consistency"
// into the next caller would be making that cost decision silently. This test
// is where they find out they are making it.
func TestRoutingOverrideScope_AuxiliaryCallsDoNotRoute(t *testing.T) {
	// The auxiliary callers, by name, with why each is excluded.
	auxiliary := map[string]string{
		"compactRunWithSource":    "a compaction summary — compaction.model is its knob, and it exists to be CHEAPER",
		"RecapSession":            "a session recap, not a turn of the conversation",
		"computeReplayCompaction": "a replay-time summary, with no live run to speak for",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	seen := map[string]bool{}
	var leaked []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				why, isAux := auxiliary[fn.Name.Name]
				if !isAux {
					return true
				}
				seen[fn.Name.Name] = true
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					lit, ok := m.(*ast.CompositeLit)
					if !ok {
						return true
					}
					if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "routingOverride" {
						leaked = append(leaked, fn.Name.Name+" ("+why+") at "+fset.Position(lit.Pos()).String())
					}
					return true
				})
				return true
			})
		}
	}

	// Non-vacuity: a renamed or deleted caller must fail here rather than
	// silently stop being checked.
	var missing []string
	for name := range auxiliary {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these auxiliary callers no longer exist under these names, so this guard is "+
			"no longer guarding them: %s", strings.Join(missing, ", "))
	}

	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Errorf("an auxiliary model call builds a routing override:\n  %s\n\n"+
			"If that is intended, it is a COST decision — say so explicitly and move the "+
			"caller out of this list.", strings.Join(leaked, "\n  "))
	}
}

// RFC DC §7.3. An untrusted trigger payload may not choose the model, exactly
// as it may not choose user_tier (#623) — safety here is a property of
// (parameter x caller), not of the parameter alone.
//
// The webhook, A2A and schedule receivers build their RunInput from the DEF, so
// the guarantee is structural: no runner.RunInput they construct sets a routing
// field. Asserted over their source for that reason — a behavioural test would
// only cover the payload shape someone thought to try, and the risk is the
// field someone adds later "so webhooks can pick a model too".
func TestRoutingOverrideScope_UntrustedTriggersCannotRoute(t *testing.T) {
	routing := map[string]bool{"Model": true, "Provider": true, "Tier": true, "Effort": true}

	checked := 0
	var leaked []string
	for _, dir := range []string{"../webhook", "../a2a", "../../scheduler", "../../a2a"} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok || selectorName(lit.Type) != "runner.RunInput" {
						return true
					}
					checked++
					for _, el := range lit.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if id, ok := kv.Key.(*ast.Ident); ok && routing[id.Name] {
							leaked = append(leaked, dir+" sets "+id.Name+" at "+fset.Position(kv.Pos()).String())
						}
					}
					return true
				})
			}
		}
	}

	// Non-vacuity: if no trigger package builds a RunInput any more, this guard
	// has stopped guarding and should say so rather than pass.
	if checked == 0 {
		t.Fatal("found no runner.RunInput literal in any trigger package; this guard is no " +
			"longer checking anything")
	}
	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Errorf("an untrusted trigger sets a per-run routing field:\n  %s\n\n"+
			"A signed webhook sender, an A2A peer or a schedule payload must not be able to "+
			"choose which model (and so which vendor) serves the run.", strings.Join(leaked, "\n  "))
	}
}
