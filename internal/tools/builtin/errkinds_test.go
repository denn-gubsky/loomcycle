package builtin

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// classifiedToolFiles are the files of the tools whose failures are classified:
// every failure they return carries a category. Grow the list as more tools
// are migrated; never shrink it.
var classifiedToolFiles = []string{
	// Document
	"document.go", "document_cross_scope.go", "document_deadlink.go", "document_entity.go",
	"document_fact_home.go", "document_fact_index.go", "document_graph.go", "document_image.go", "document_index_text.go",
	"document_labels.go", "document_mermaid.go", "document_ontology.go", "document_ontology_guard.go",
	"document_prune.go", "document_remember.go", "document_subject_proposal.go", "document_subject_refs.go",
	"document_sync.go", "document_verbatim.go", "document_verdict.go",
	// Memory
	"memory.go", "memory_placement.go", "memory_recall_traces.go", "memory_recall_turns.go", "memory_source_span.go",
	// History
	"history.go", "history_content_search.go", "history_page.go", "history_window.go",
	// Path, Channel, Context, Agent, Skill
	"pathtool.go", "channel.go", "context.go", "context_capabilities.go", "context_guide.go", "agent.go", "skill.go",
}

// unclassifiedFailures finds, in one file, every failure built without a
// category: a call to errResult, and a Result literal that sets IsError: true
// but no Error.
func unclassifiedFailures(t *testing.T, fset *token.FileSet, f *ast.File) []string {
	t.Helper()
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "errResult" {
				out = append(out, fset.Position(x.Pos()).String()+": errResult")
			}
		case *ast.CompositeLit:
			if !isResultType(x.Type) {
				return true
			}
			var isErr, hasError bool
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				if key == nil {
					continue
				}
				switch key.Name {
				case "IsError":
					if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
						isErr = true
					}
				case "Error":
					hasError = true
				}
			}
			if isErr && !hasError {
				out = append(out, fset.Position(x.Pos()).String()+": Result{IsError: true} without Error")
			}
		}
		return true
	})
	return out
}

func isResultType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		pkg, _ := x.X.(*ast.Ident)
		return pkg != nil && pkg.Name == "tools" && x.Sel.Name == "Result"
	case *ast.Ident:
		return x.Name == "Result"
	}
	return false
}

// Every failure these tools return carries a category, so the model reads what
// KIND of failure it hit — fix the input, take another path, ask for a grant,
// or try again — instead of a bare message. The helpers in errkinds.go make
// that decision at the call site; an errResult here is a failure nobody
// classified.
func TestClassifiedTools_EveryFailureCarriesACategory(t *testing.T) {
	fset := token.NewFileSet()
	var bad []string
	for _, name := range classifiedToolFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bad = append(bad, unclassifiedFailures(t, fset, f)...)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("unclassified failure: %s — use errValidation / errNotFound / errBusiness / errPermission / errTransient / errFrom", b)
	}
}

// The list above names real files, so a rename cannot silently drop one from
// the check; and every file of a listed tool is on it, so a new file cannot
// slip in unclassified.
func TestClassifiedTools_TheFileListIsComplete(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range classifiedToolFiles {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("listed file %s does not exist", name)
		}
		listed[name] = true
	}
	// Other tools that share a prefix with a classified one.
	notThisTool := map[string]bool{"documentsourcedef.go": true, "memorybackenddef.go": true, "agentdef.go": true, "skilldef.go": true}
	for _, pat := range []string{"document*.go", "memory*.go", "history*.go", "context*.go"} {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if strings.HasSuffix(m, "_test.go") || notThisTool[m] {
				continue
			}
			if !listed[m] {
				t.Errorf("%s belongs to a classified tool but is not in classifiedToolFiles", m)
			}
		}
	}
}

// The helpers carry what they say: the category, retryability, and the next
// step; and errFrom takes its category from the error's TYPE, leaving an
// unknown one unclassified rather than guessing.
func TestErrkinds_CarryTheirCategory(t *testing.T) {
	for _, c := range []struct {
		res       tools.Result
		cat       tools.ErrorCategory
		retryable bool
	}{
		{errValidation("bad", "fix it"), tools.CategoryValidation, false},
		{errNotFound("no such document", "list them first"), tools.CategoryValidation, false},
		{errBusiness("quota", "store less"), tools.CategoryBusiness, false},
		{errPermission("scope not granted", "use user"), tools.CategoryPermission, false},
		{errTransient("lock held", "try again"), tools.CategoryTransient, true},
	} {
		if !c.res.IsError || c.res.Error == nil || c.res.Error.Category != c.cat || c.res.Error.Retryable != c.retryable || c.res.Error.Description == "" {
			t.Errorf("%q: %+v", c.res.Text, c.res.Error)
		}
	}

	quota := errFrom("memory set: quota", fmt.Errorf("set: %w", store.ErrMemoryQuotaExceeded))
	if quota.Error == nil || quota.Error.Category != tools.CategoryBusiness || quota.Text != "memory set: quota" {
		t.Errorf("errFrom(quota) = %+v", quota)
	}
	if unknown := errFrom("boom", errors.New("boom")); !unknown.IsError || unknown.Error != nil {
		t.Errorf("errFrom(unknown) classified a type it does not know: %+v", unknown.Error)
	}
}
