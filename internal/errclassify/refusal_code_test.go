package errclassify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// coded stands in for the HTTP package's configured-run refusal, which is
// unexported: a code, a message, and optionally the error it stands for.
type coded struct {
	code  string
	cause error
}

func (c coded) Error() string     { return "refused: " + c.code }
func (c coded) ErrorCode() string { return c.code }
func (c coded) Unwrap() error     { return c.cause }

// waited stands in for the rate limiter's refusal: it names its own wait.
type waited struct{ d time.Duration }

func (w waited) Error() string             { return "rate limited" }
func (w waited) RetryDelay() time.Duration { return w.d }

// The draft cap and the rate limit share HTTP 429, and only the rate limit
// clears by waiting. The status cannot tell them apart, so the classification
// must come from the code — and a wrong "retryable" on the cap is a retry
// loop that never ends.
func TestCategoryOf_CodedRefusalsClassifyByTheirCode(t *testing.T) {
	for code, want := range map[string]struct {
		cat       tools.ErrorCategory
		retryable bool
	}{
		"configured_run_cap":         {tools.CategoryBusiness, false},
		"rate_limited":               {tools.CategoryTransient, true},
		"agent_not_allowed":          {tools.CategoryPermission, false},
		"override_not_allowed":       {tools.CategoryPermission, false},
		"agent_id_in_use":            {tools.CategoryBusiness, false},
		"run_not_configured":         {tools.CategoryBusiness, false},
		"retention_none_unsupported": {tools.CategoryBusiness, false},
		"no_store":                   {tools.CategoryBusiness, false},
		"unknown_run":                {tools.CategoryValidation, false},
		"invalid_draft":              {tools.CategoryValidation, false},
		"empty_patch":                {tools.CategoryValidation, false},
	} {
		got, ok := CategoryOf(coded{code: code})
		if !ok || got.Category != want.cat || got.Retryable != want.retryable {
			t.Errorf("%s = %v/%s retryable=%v, want %s retryable=%v", code, ok, got.Category, got.Retryable, want.cat, want.retryable)
		}
	}
	if _, ok := CategoryOf(coded{code: "internal"}); ok {
		t.Error("an internal refusal was classified; it has no caller-actionable next step")
	}
}

// A retryable refusal that names its own wait carries that wait, not the
// class default; a non-retryable one never carries a wait, even if some error
// in its chain names one.
func TestCategoryOf_OwnWaitOnlyOnARetryableRefusal(t *testing.T) {
	got, _ := CategoryOf(coded{code: "rate_limited", cause: waited{3 * time.Second}})
	if got.RetryAfter == nil || *got.RetryAfter != 3*time.Second {
		t.Errorf("rate_limited RetryAfter = %v, want the refusal's own 3s", got.RetryAfter)
	}
	got, _ = CategoryOf(coded{code: "configured_run_cap", cause: waited{3 * time.Second}})
	if got.RetryAfter != nil {
		t.Errorf("configured_run_cap carries RetryAfter = %v; waiting does not free a draft", *got.RetryAfter)
	}
}

// Every code a configured-run refusal is built with is classified, or listed
// here with why not. Read from the source rather than restated, so a refusal
// added with a new code fails here instead of reaching MCP unlabelled and
// gRPC without a category.
func TestCategoryOf_EveryDraftRefusalCodeIsClassified(t *testing.T) {
	unclassified := map[string]string{
		"internal": "a server fault; no caller-actionable next step",
	}
	codes := draftRefusalCodes(t, "../api/http")
	if len(codes) == 0 {
		t.Fatal("found no draftRefusal codes — the AST scan is broken, so this guard proves nothing")
	}
	for _, code := range codes {
		_, ok := CategoryOf(coded{code: code})
		_, exempt := unclassified[code]
		switch {
		case ok && exempt:
			t.Errorf("%s is classified and also listed as unclassified", code)
		case !ok && !exempt:
			t.Errorf("draftRefusal code %q has no classification in byRefusalCode and no recorded reason", code)
		}
	}
}

// draftRefusalCodes returns the string-literal codes passed to draftRefusal
// (its second argument) in dir's non-test files.
func draftRefusalCodes(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "draftRefusal" {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: draftRefusal with a non-literal code; this guard cannot read it", fset.Position(call.Pos()))
					return true
				}
				code, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				if !seen[code] {
					seen[code] = true
					out = append(out, code)
				}
				return true
			})
		}
	}
	return out
}
