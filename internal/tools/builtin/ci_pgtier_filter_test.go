package builtin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCIFilter_CoversEveryPostgresTierTest enforces what the CI workflow asks for
// in a comment.
//
// The postgres-tier tests in this package cannot run under a plain
// `go test ./...`: the workflow step that owns the sqlmem aux database runs them
// through an explicit `-run` regex, because two packages cannot hold that one
// database concurrently. The step says so itself — "Add every new postgres-tier
// test in this package here, or it is decoration rather than a gate."
//
// That is a maintenance rule with no enforcement, and this repo has now produced
// several bugs from exactly that shape. Worse, the failure is SILENT in the most
// misleading way: a test omitted from the filter still passes locally, still passes
// in CI, and simply never executes its assertions there — indistinguishable from a
// test that runs. The pgvector contract sat dark for exactly this reason.
//
// A test is "postgres-tier" if its body CALLS os.Getenv on the aux DSN, which is
// how each one skips when that database is absent. Matching a bare mention of the
// name instead is not enough — the first version of this test flagged ITSELF,
// because it names the variable as a constant.
//
// A test can also reach that database through a helper (pgDocumentOrSkip and its
// kin) that reads the DSN and skips. The first version looked only at test bodies,
// so the postgres halves of helper-based tests were invisible to it and sat dark in
// CI while it passed. The helpers are derived, not listed: every function whose
// body reads the DSN, then every function that calls one of those, to a fixpoint.
func TestCIFilter_CoversEveryPostgresTierTest(t *testing.T) {
	// Assembled rather than written whole so this file does not contain the
	// literal it searches for, which is what made the first version self-flag.
	gate := `os.Getenv("` + "LOOMCYCLE_TEST_SQLMEM" + `_PG_DSN")`

	wf, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Skipf("cannot read the workflow (not a source checkout?): %v", err)
	}
	// Pull the -run regex out of the step that sets the aux DSN.
	m := regexp.MustCompile(`-run '([^']+)' \./internal/tools/builtin`).FindSubmatch(wf)
	if m == nil {
		t.Fatalf("could not find the `-run ... ./internal/tools/builtin` filter in ci.yml — " +
			"if the step was renamed or the tests were un-gated, update this test deliberately")
	}
	filter, err := regexp.Compile("^(" + string(m[1]) + ")")
	if err != nil {
		t.Fatalf("the CI -run filter is not a valid regex: %v", err)
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	type fn struct{ name, path, body string }
	// Methods are included so a helper on a fixture type is followed too.
	funcRe := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	var funcs []fn
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		for _, fm := range funcRe.FindAllStringSubmatchIndex(text, -1) {
			// Body ends at the first `}` in column 0. Using "the next func" instead
			// swallows the FOLLOWING test's doc comment, which produced a false
			// positive the first time this audit was run by hand.
			body := text[fm[1]:]
			if end := strings.Index(body, "\n}\n"); end > 0 {
				body = body[:end]
			}
			funcs = append(funcs, fn{name: text[fm[2]:fm[3]], path: path, body: body})
		}
	}

	// reaches[name]: the function reads the DSN itself, or calls one that does.
	// Calls are read as any `ident(` in the body; a name used that way in a
	// comment over-counts, which can only make this check stricter.
	callRe := regexp.MustCompile(`\b(\w+)\(`)
	calls := make([][]string, len(funcs))
	reaches := map[string]bool{}
	for i, f := range funcs {
		for _, m := range callRe.FindAllStringSubmatch(f.body, -1) {
			calls[i] = append(calls[i], m[1])
		}
		if strings.Contains(f.body, gate) {
			reaches[f.name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i, f := range funcs {
			if reaches[f.name] {
				continue
			}
			for _, c := range calls[i] {
				if reaches[c] {
					reaches[f.name], changed = true, true
					break
				}
			}
		}
	}

	var found, uncovered int
	for _, f := range funcs {
		if !reaches[f.name] || !strings.HasPrefix(f.name, "Test") ||
			!strings.HasPrefix(f.body, "t *testing.T)") {
			continue
		}
		found++
		if !filter.MatchString(f.name) {
			uncovered++
			t.Errorf("%s (%s) reaches %s (directly or through a helper) but is NOT matched "+
				"by the CI -run filter, so it never executes in CI — add it to the filter in "+
				".github/workflows/ci.yml", f.name, f.path, gate)
		}
	}
	// Guard the guard: if the scan matched nothing, this test would pass while
	// checking nothing — the same failure mode it exists to prevent.
	if found == 0 {
		t.Fatal("found no postgres-tier tests at all; the scan is broken, so this test " +
			"is not actually enforcing the filter")
	}
	t.Logf("%d postgres-tier tests, %d uncovered", found, uncovered)
}
