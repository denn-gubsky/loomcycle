package config

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGlobMatch_StarIsAnyRunAndQuestionIsOneCharacter(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"refs/heads/*", "refs/heads/main", true},
		{"refs/heads/*", "refs/heads/feature/x", true},
		{"refs/heads/*", "refs/tags/v1", false},
		{"refs/heads/*", "refs/heads/", true},
		{"main", "main", true},
		{"main", "main2", false},
		{"*", "", true},
		{"*", "anything / at all", true},
		{"", "", true},
		{"", "x", false},
		{"?", "é", true}, // one character, not one byte
		{"?", "", false},
		{"?", "ab", false},
		{"a?c", "abc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "acb", false},
		{"*ab*ab", "abab", true},
		{"*ab*ab", "ab", false}, // the last piece may not reuse the middle's characters
		{"ab*ba", "aba", false},
		{"ab*ba", "abba", true},
		{"a*a", "a", false},
		{"*a?c*", "zzabczz", true},
		{"*?", "", false},
		{"*?", "x", true},
		{"*[skip ci]*", "fix: thing [skip ci]", true}, // [ is itself
		{"*[skip ci]*", "fix: thing s", false},
		{`a\*`, `a\anything`, true}, // \ is itself, and escapes nothing
		{`a\*`, `a*`, false},
		{"release-?.*", "release-1.2", true},
		{"release-?.*", "release-12.2", false},
	} {
		if got := GlobMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("GlobMatch(%q, %q) = %t, want %t", tc.pattern, tc.s, got, tc.want)
		}
	}
}

// globReference is the textbook recursive matcher, for comparison.
func globReference(p, s []rune) bool {
	if len(p) == 0 {
		return len(s) == 0
	}
	switch p[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if globReference(p[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return len(s) > 0 && globReference(p[1:], s[1:])
	}
	return len(s) > 0 && s[0] == p[0] && globReference(p[1:], s[1:])
}

// The piecewise matcher agrees with the recursive one on every pattern and
// string drawn from a small alphabet — the one the pieces could disagree on.
func TestGlobMatch_AgreesWithTheReferenceMatcher(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pick := func(alphabet []rune, n int) string {
		out := make([]rune, n)
		for i := range out {
			out[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(out)
	}
	matched := 0
	for i := 0; i < 50000; i++ {
		p := pick([]rune("ab*?é"), rng.Intn(7))
		s := pick([]rune("abé"), rng.Intn(8))
		want := globReference([]rune(p), []rune(s))
		if got := GlobMatch(p, s); got != want {
			t.Fatalf("GlobMatch(%q, %q) = %t, the reference says %t", p, s, got, want)
		}
		if want {
			matched++
		}
	}
	if matched < 1000 {
		t.Fatalf("only %d of the draws matched: the comparison checks almost nothing", matched)
	}
}

func decodeDoc(t *testing.T, body string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Comparison is typed, and a filter that cannot be evaluated — a path that
// is missing, null, or of another type than the value — misses, ne included.
func TestWebhookFilters_ComparisonIsTypedAndAMissIsAMiss(t *testing.T) {
	doc := decodeDoc(t, `{"ref":"refs/heads/main","deleted":false,"forced":null,"size":3,"id_str":"3",
		"repository":{"full_name":"acme/app","id":42},"commits":[{"id":"c1"}]}`)
	meta := map[string]any{"repo": "acme/app", "repos": []any{"acme/web", "acme/app"}, "count": 3}
	for _, tc := range []struct {
		name string
		f    WebhookFilter
		want bool
	}{
		{"eq string", WebhookFilter{Path: "$.ref", Op: "eq", Value: "refs/heads/main"}, true},
		{"eq bool", WebhookFilter{Path: "$.deleted", Op: "eq", Value: false}, true},
		{"eq a yaml int", WebhookFilter{Path: "$.size", Op: "eq", Value: 3}, true},
		{"eq a json number", WebhookFilter{Path: "$.size", Op: "eq", Value: 3.0}, true},
		{"eq a number against a string", WebhookFilter{Path: "$.id_str", Op: "eq", Value: 3}, false},
		{"eq a string against a number", WebhookFilter{Path: "$.size", Op: "eq", Value: "3"}, false},
		{"eq an object", WebhookFilter{Path: "$.repository", Op: "eq", Value: "acme/app"}, false},
		{"eq through an index", WebhookFilter{Path: "$.commits[0].id", Op: "eq", Value: "c1"}, true},
		{"ne", WebhookFilter{Path: "$.ref", Op: "ne", Value: "refs/heads/dev"}, true},
		{"ne equal", WebhookFilter{Path: "$.ref", Op: "ne", Value: "refs/heads/main"}, false},
		{"ne missing", WebhookFilter{Path: "$.nope", Op: "ne", Value: "x"}, false},
		{"ne null", WebhookFilter{Path: "$.forced", Op: "ne", Value: true}, false},
		{"ne another type", WebhookFilter{Path: "$.size", Op: "ne", Value: "x"}, false},
		{"glob", WebhookFilter{Path: "$.ref", Op: "glob", Value: "refs/heads/*"}, true},
		{"glob a tag", WebhookFilter{Path: "$.ref", Op: "glob", Value: "refs/tags/*"}, false},
		{"glob a number", WebhookFilter{Path: "$.size", Op: "glob", Value: "*"}, false},
		{"in", WebhookFilter{Path: "$.repository.id", Op: "in", Value: []any{"42", 41, 42}}, true},
		{"in, only of another type", WebhookFilter{Path: "$.repository.id", Op: "in", Value: []any{"42"}}, false},
		{"value_from", WebhookFilter{Path: "$.repository.full_name", Op: "eq", ValueFrom: "metadata.repo"}, true},
		{"value_from a list", WebhookFilter{Path: "$.repository.full_name", Op: "in", ValueFrom: "metadata.repos"}, true},
		{"value_from a yaml int", WebhookFilter{Path: "$.size", Op: "eq", ValueFrom: "metadata.count"}, true},
		{"value_from a key the metadata lost", WebhookFilter{Path: "$.ref", Op: "ne", ValueFrom: "metadata.gone"}, false},
		{"a path that does not parse", WebhookFilter{Path: "$..ref", Op: "ne", Value: "x"}, false},
	} {
		w := Webhook{Metadata: meta, Filters: []WebhookFilter{tc.f}}
		if got := w.FirstFailingFilter(doc) < 0; got != tc.want {
			t.Errorf("%s: holds = %t, want %t", tc.name, got, tc.want)
		}
	}
	// Every filter must hold; the first that misses is reported.
	w := Webhook{Filters: []WebhookFilter{
		{Path: "$.ref", Op: "glob", Value: "refs/heads/*"},
		{Path: "$.deleted", Op: "eq", Value: true},
		{Path: "$.nope", Op: "eq", Value: "x"},
	}}
	if got := w.FirstFailingFilter(doc); got != 1 {
		t.Errorf("first failing filter = %d, want 1", got)
	}
	if got := (Webhook{}).FirstFailingFilter(doc); got != -1 {
		t.Errorf("no filters: %d, want -1", got)
	}
}

func TestCheckWebhookFilters_RefusesWhatCannotBeEvaluated(t *testing.T) {
	meta := map[string]any{"repo": "acme/app", "obj": map[string]any{"a": 1}}
	ok := []WebhookFilter{
		{Path: "$.ref", Op: "glob", Value: "refs/heads/*"},
		{Path: "$.deleted", Op: "eq", Value: false},
		{Path: "$.size", Op: "ne", Value: 3},
		{Path: "$.id", Op: "in", Value: []any{1, "1", true}},
		{Path: "$.repository.full_name", Op: "eq", ValueFrom: "metadata.repo"},
	}
	if err := CheckWebhookFilters(Webhook{Metadata: meta, Filters: ok}); err != nil {
		t.Fatalf("valid filters refused: %v", err)
	}
	tooMany := make([]WebhookFilter, maxWebhookFilters+1)
	for i := range tooMany {
		tooMany[i] = WebhookFilter{Path: "$.a", Op: "eq", Value: "x"}
	}
	longIn := make([]any, maxWebhookFilterIn+1)
	for i := range longIn {
		longIn[i] = "x"
	}
	for _, tc := range []struct {
		name    string
		filters []WebhookFilter
		want    string
	}{
		{"too many", tooMany, "at most 32"},
		{"bad path", []WebhookFilter{{Path: "$.a[*]", Op: "eq", Value: "x"}}, "not a supported JSONPath"},
		{"no path", []WebhookFilter{{Op: "eq", Value: "x"}}, "not a supported JSONPath"},
		{"whole body", []WebhookFilter{{Path: "$", Op: "eq", Value: "x"}}, "names the whole body"},
		{"bad op", []WebhookFilter{{Path: "$.a", Op: "matches", Value: "x"}}, `op "matches" is not one of`},
		{"both", []WebhookFilter{{Path: "$.a", Op: "eq", Value: "x", ValueFrom: "metadata.repo"}}, "not both"},
		{"neither", []WebhookFilter{{Path: "$.a", Op: "eq"}}, "needs a value"},
		{"value_from elsewhere", []WebhookFilter{{Path: "$.a", Op: "eq", ValueFrom: "body.ref"}}, "is not metadata.<key>"},
		{"value_from no key", []WebhookFilter{{Path: "$.a", Op: "eq", ValueFrom: "metadata."}}, "is not metadata.<key>"},
		{"value_from absent", []WebhookFilter{{Path: "$.a", Op: "eq", ValueFrom: "metadata.nope"}}, `has no key "nope"`},
		{"value_from an object", []WebhookFilter{{Path: "$.a", Op: "eq", ValueFrom: "metadata.obj"}}, `value_from "metadata.obj": a value is a string`},
		{"glob a number", []WebhookFilter{{Path: "$.a", Op: "glob", Value: 3}}, "glob takes a string"},
		{"glob too long", []WebhookFilter{{Path: "$.a", Op: "glob", Value: strings.Repeat("*", maxWebhookGlobPattern+1)}}, "at most 256 bytes"},
		{"in a scalar", []WebhookFilter{{Path: "$.a", Op: "in", Value: "x"}}, "in takes a list"},
		{"in empty", []WebhookFilter{{Path: "$.a", Op: "in", Value: []any{}}}, "1 to 64"},
		{"in too long", []WebhookFilter{{Path: "$.a", Op: "in", Value: longIn}}, "1 to 64"},
		{"in a list holding a list", []WebhookFilter{{Path: "$.a", Op: "in", Value: []any{"x", []any{"y"}}}}, "in[1]"},
		{"eq a list", []WebhookFilter{{Path: "$.a", Op: "eq", Value: []any{"x"}}}, "not a list"},
		{"eq an object", []WebhookFilter{{Path: "$.a", Op: "eq", Value: map[string]any{}}}, "not an object"},
		{"a long string", []WebhookFilter{{Path: "$.a", Op: "eq", Value: strings.Repeat("x", maxWebhookFilterString+1)}}, "at most 1024 bytes"},
	} {
		err := CheckWebhookFilters(Webhook{Metadata: meta, Filters: tc.filters})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestCheckWebhookMappingPaths(t *testing.T) {
	if err := CheckWebhookMappingPaths(map[string]string{"goal": "$.head_commit.message", "user_id": "$.sender.login"}); err != nil {
		t.Fatal(err)
	}
	err := CheckWebhookMappingPaths(map[string]string{"goal": "$.a", "run_metadata.x": "$.items[*].id"})
	if err == nil || !strings.Contains(err.Error(), `payload_mapping["run_metadata.x"]`) {
		t.Fatalf("%v, want the unparseable target named", err)
	}
}

// The help article's yaml: a yaml load gives ints and []any where a JSON
// overlay gives float64s, and both compare with a decoded body.
func TestWebhookFilters_FromYAML(t *testing.T) {
	var w Webhook
	src := `
provider: github
events: [push]
metadata: { repo: acme/app, sizes: [1, 2] }
filters:
  - { path: "$.ref",                  op: glob, value: "refs/heads/*" }
  - { path: "$.deleted",              op: eq,   value: false }
  - { path: "$.repository.full_name", op: eq,   value_from: metadata.repo }
  - { path: "$.size",                 op: in,   value_from: metadata.sizes }
`
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatal(err)
	}
	if err := CheckWebhookFilters(w); err != nil {
		t.Fatal(err)
	}
	push := `{"ref":"refs/heads/main","deleted":false,"size":2,"repository":{"full_name":"acme/app"}}`
	if got := w.FirstFailingFilter(decodeDoc(t, push)); got != -1 {
		t.Errorf("a branch push missed filter %d", got)
	}
	for body, want := range map[string]int{
		`{"ref":"refs/tags/v1","deleted":false,"size":2,"repository":{"full_name":"acme/app"}}`:      0,
		`{"ref":"refs/heads/main","deleted":true,"size":2,"repository":{"full_name":"acme/app"}}`:    1,
		`{"ref":"refs/heads/main","deleted":false,"size":2,"repository":{"full_name":"acme/web"}}`:   2,
		`{"ref":"refs/heads/main","deleted":false,"size":3,"repository":{"full_name":"acme/app"}}`:   3,
		`{"ref":"refs/heads/main","deleted":false,"size":"2","repository":{"full_name":"acme/app"}}`: 3,
	} {
		if got := w.FirstFailingFilter(decodeDoc(t, body)); got != want {
			t.Errorf("%s: missed %d, want %d", body, got, want)
		}
	}
}
