package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestTeamIssue_TSMirrorsEveryKindAndKey: the TS adapter's TeamIssue and
// TeamIssueKind are hand-written mirrors of what verify emits. A key missing
// there is a field a typed client cannot read without a cast — the gap that
// left verify's runnable/issues untyped once already.
//
// Both Go sides are DERIVED, not listed here: the kinds are every teamIssue*
// constant declared in teamdef_issues.go, and the keys are what toMap emits
// for an issue with every field set.
func TestTeamIssue_TSMirrorsEveryKindAndKey(t *testing.T) {
	tsSrc, err := os.ReadFile("../../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("TS adapter not present: %v", err)
	}

	// Kinds.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "teamdef_issues.go", nil, 0)
	if err != nil {
		t.Fatalf("parse teamdef_issues.go: %v", err)
	}
	var kinds []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "teamIssue") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				v, _ := strconv.Unquote(lit.Value)
				kinds = append(kinds, v)
			}
		}
	}
	if len(kinds) < 15 {
		t.Fatalf("only %d issue kinds parsed — the pattern has stopped matching", len(kinds))
	}
	um := regexp.MustCompile(`(?s)export type TeamIssueKind =(.*?);`).FindSubmatch(tsSrc)
	if um == nil {
		t.Fatal("could not find TeamIssueKind in types.ts")
	}
	tsKinds := map[string]bool{}
	for _, k := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(um[1], -1) {
		tsKinds[string(k[1])] = true
	}
	var missing []string
	for _, k := range kinds {
		if !tsKinds[k] {
			missing = append(missing, k)
		}
	}

	// Keys.
	im := regexp.MustCompile(`(?s)export interface TeamIssue \{(.*?)\n\}`).FindSubmatch(tsSrc)
	if im == nil {
		t.Fatal("could not find TeamIssue in types.ts")
	}
	tsKeys := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?m)^\s*([a-z_]+)\??:`).FindAllSubmatch(im[1], -1) {
		tsKeys[string(f[1])] = true
	}
	full := teamIssue{Kind: "k", Severity: severityAdvisory, Path: "p", State: "s", Field: "f",
		Channel: "c", Agent: "a", Skill: "sk", Side: "publish", Detail: "d"}
	for key := range full.toMap() {
		if !tsKeys[key] {
			missing = append(missing, "TeamIssue."+key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("verify emits these, and adapters/ts/src/types.ts does not declare them: %s",
			strings.Join(missing, ", "))
	}
}
