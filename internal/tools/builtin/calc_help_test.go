package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Calc help is checked against calc's own tables, so neither can drift
// from the other:
//
//   - A function entry is a `### ` heading whose text is the function's
//     signature exactly as calc.Functions() gives it, e.g.
//     `### round(x, n?)`. It lives in the section article its table section
//     maps to (calcSectionArticle), and carries at least one line
//     "Example: `<expr>` gives `<text>`." — or "gives `error <code>`." for a
//     refusal.
//   - A call example is a ```json fence under `## Examples`, followed by the
//     line "Result: <text>" or "Result: error <code>".
//   - Calc/errors has one `### <code>` entry per calc.ErrorCodes() entry.
//
// Every expected result is compared, as text, with what the tool returns.

// calcSectionArticle maps each calc table section to the one article that
// documents its functions.
var calcSectionArticle = map[string]string{
	calc.SecControl:       "Calc/language",
	calc.SecElementary:    "Calc/elementary",
	calc.SecTrig:          "Calc/elementary",
	calc.SecSpecial:       "Calc/elementary",
	calc.SecIntegers:      "Calc/integers",
	calc.SecBits:          "Calc/integers",
	calc.SecSequences:     "Calc/sequences",
	calc.SecRandom:        "Calc/sequences",
	calc.SecDates:         "Calc/dates",
	calc.SecUnits:         "Calc/units",
	calc.SecStatistics:    "Calc/statistics",
	calc.SecDistributions: "Calc/distributions",
	calc.SecCalculus:      "Calc/calculus",
	calc.SecMatrices:      "Calc/matrices",
	calc.SecComplex:       "Calc/complex",
	calc.SecFinance:       "Calc/finance",
	calc.SecTables:        "Calc/tables",
}

// calcEntryHeading is a `### ` heading that presents a function: a lower-case
// name followed by `(`.
var calcEntryHeading = regexp.MustCompile(`^### ([a-z][a-z0-9_]*)\(`)

var calcInlineExample = regexp.MustCompile("^Example: `(.+)` gives `(.+)`\\.$")

type calcEntry struct {
	topic, heading, name string
	examples             [][2]string // expr, expected
}

// calcEntries parses the function entries of every Calc article.
func calcEntries(t *testing.T, set *help.Set) []calcEntry {
	t.Helper()
	var out []calcEntry
	for _, topic := range calcArticles(t, set) {
		var cur *calcEntry
		flush := func() {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
		}
		for _, ln := range strings.Split(topic.Content, "\n") {
			if strings.HasPrefix(ln, "#") {
				flush()
				if m := calcEntryHeading.FindStringSubmatch(ln); m != nil {
					cur = &calcEntry{topic: topic.Name, heading: strings.TrimPrefix(ln, "### "), name: m[1]}
				}
				continue
			}
			if cur == nil {
				continue
			}
			if m := calcInlineExample.FindStringSubmatch(strings.TrimSpace(ln)); m != nil {
				cur.examples = append(cur.examples, [2]string{m[1], m[2]})
			} else if strings.HasPrefix(strings.TrimSpace(ln), "Example:") {
				t.Errorf("%s, %s: example line not in the form \"Example: `expr` gives `text`.\": %q", topic.Name, cur.heading, ln)
			}
		}
		flush()
	}
	return out
}

func calcArticles(t *testing.T, set *help.Set) []*help.Topic {
	t.Helper()
	art, ok := set.ToolArticle("Calc")
	if !ok {
		t.Fatal("no Calc tool article")
	}
	return append([]*help.Topic{art}, set.SectionsOf("Calc")...)
}

// (a) Every function in the table has an entry in exactly one article — the
// article its section maps to — and every entry names a function in the table.
func TestCalcHelp_EveryFunctionHasOneEntryAndEveryEntryIsAFunction(t *testing.T) {
	set := bundledHelp(t)
	entries := calcEntries(t, set)
	where := map[string][]string{}
	for _, e := range entries {
		where[e.name] = append(where[e.name], e.topic)
	}
	table := map[string]calc.FuncInfo{}
	for _, f := range calc.Functions() {
		table[f.Name] = f
		article, mapped := calcSectionArticle[f.Section]
		if !mapped {
			t.Errorf("calc section %q maps to no article in calcSectionArticle", f.Section)
			continue
		}
		switch got := where[f.Name]; {
		case len(got) == 0:
			t.Errorf("%s has no entry (want `### %s` in %s)", f.Name, f.Signature, article)
		case len(got) > 1:
			t.Errorf("%s has %d entries: %v", f.Name, len(got), got)
		case got[0] != article:
			t.Errorf("%s is documented in %s, but its section %q belongs in %s", f.Name, got[0], f.Section, article)
		}
	}
	for name, topics := range where {
		if _, ok := table[name]; !ok {
			t.Errorf("%v presents %s as a function, but calc has no such function", topics, name)
		}
	}
	for _, e := range entries {
		if len(e.examples) == 0 {
			t.Errorf("%s, %s: no \"Example: `expr` gives `text`.\" line", e.topic, e.heading)
		}
	}
	// Each mapped article exists and is a section of Calc.
	for _, article := range calcSectionArticle {
		if topic, ok := set.Get(article); !ok || topic.Section == "" || topic.Tool != "Calc" {
			t.Errorf("calcSectionArticle names %s, which is not a Calc section article", article)
		}
	}
}

// (b) The signature an entry shows is the table's.
func TestCalcHelp_EntrySignaturesMatchTheTable(t *testing.T) {
	sigs := map[string]string{}
	for _, f := range calc.Functions() {
		sigs[f.Name] = f.Signature
	}
	for _, e := range calcEntries(t, bundledHelp(t)) {
		if want, ok := sigs[e.name]; ok && e.heading != want {
			t.Errorf("%s: entry heading %q, the table's signature is %q", e.topic, e.heading, want)
		}
	}
}

// runCalcExample runs one call through the tool and returns what it gave, in
// the form an expected result is written: the text, or "error <code>".
func runCalcExample(t *testing.T, input json.RawMessage) string {
	t.Helper()
	res, err := (&Calc{}).Execute(context.Background(), input)
	if err != nil {
		return "go error " + err.Error()
	}
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return "unparseable " + res.Text
	}
	if res.IsError {
		return "error " + out.Error
	}
	return out.Text
}

// (c) Every example gives the result its article says, to the digit: each
// call example's "Result:" line and each entry's "Example:" line.
func TestCalcHelp_EveryExampleGivesItsResult(t *testing.T) {
	set := bundledHelp(t)
	n := 0
	for _, topic := range calcArticles(t, set) {
		exs, err := topic.Examples()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(topic.Content, "\n")
		for _, ex := range exs {
			if ex.Tool != "Calc" {
				continue
			}
			want, ok := resultAfterFence(lines, ex.Line)
			if !ok {
				t.Errorf("%s line %d: the example is not followed by a \"Result: …\" line", topic.Name, ex.Line)
				continue
			}
			n++
			if got := runCalcExample(t, ex.Input); got != want {
				t.Errorf("%s line %d: %s\n  article says: %s\n  tool gives:   %s", topic.Name, ex.Line, ex.Input, want, got)
			}
		}
	}
	for _, e := range calcEntries(t, set) {
		for _, ex := range e.examples {
			n++
			in, _ := json.Marshal(map[string]string{"expr": ex[0]})
			if got := runCalcExample(t, in); got != ex[1] {
				t.Errorf("%s, %s: `%s`\n  article says: %s\n  tool gives:   %s", e.topic, e.heading, ex[0], ex[1], got)
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d examples checked; the parser is missing most of them", n)
	}
}

// resultAfterFence finds the "Result: " line after the fence opened at line
// open (1-based): the first non-blank line after the closing fence.
func resultAfterFence(lines []string, open int) (string, bool) {
	i := open // index of the line after the opening fence
	for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
		i++
	}
	for i++; i < len(lines); i++ {
		if s := strings.TrimSpace(lines[i]); s != "" {
			r, ok := strings.CutPrefix(s, "Result: ")
			return r, ok
		}
	}
	return "", false
}

// The `calculations` recipes topic is held to the same rule as the Calc
// articles: every call in it runs through the tool and gives the result the
// topic prints. It is a feature topic, not a Calc article, so its fences name
// the tool (```json tool=Calc).
func TestCalculationsRecipes_EveryCallGivesItsResult(t *testing.T) {
	set := bundledHelp(t)
	topic, ok := set.Get("calculations")
	if !ok || topic.Name != "calculations" {
		t.Fatal("no calculations topic")
	}
	exs, err := topic.Examples()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(topic.Content, "\n")
	n := 0
	for _, ex := range exs {
		if ex.Tool != "Calc" {
			t.Errorf("calculations line %d: example calls %s; every recipe is a Calc call", ex.Line, ex.Tool)
			continue
		}
		want, ok := resultAfterFence(lines, ex.Line)
		if !ok {
			t.Errorf("calculations line %d: the example is not followed by a \"Result: …\" line", ex.Line)
			continue
		}
		n++
		if got := runCalcExample(t, ex.Input); got != want {
			t.Errorf("calculations line %d: %s\n  topic says: %s\n  tool gives: %s", ex.Line, ex.Input, want, got)
		}
	}
	// One call per recipe; fewer means the parser lost some (a fence outside
	// the `## Examples` section is not an example).
	if n < 12 {
		t.Fatalf("only %d recipe calls checked, want at least 12", n)
	}
	if w := len(strings.Fields(topic.Content)); w > 1500 {
		t.Errorf("calculations is %d words, over its 1500-word budget", w)
	}
}

// The recipes are reachable from the words a model asks with, and from the
// Calc article; no alias takes a name another topic answers to.
func TestCalculationsRecipes_AliasesResolveAndCalcPointsThere(t *testing.T) {
	set := bundledHelp(t)
	for _, name := range []string{"calculations", "math", "Calculator", "arithmetic"} {
		if topic, ok := set.Get(name); !ok || topic.Name != "calculations" {
			t.Errorf("Get(%q) does not resolve to calculations", name)
		}
	}
	if topic, ok := set.Get("calc"); !ok || topic.Name != "Calc" {
		t.Error(`Get("calc") must stay the Calc tool article`)
	}
	art, _ := set.ToolArticle("Calc")
	if !strings.Contains(art.Content, "topic=calculations") {
		t.Error("the Calc article does not point to the calculations recipes")
	}
}

// (d) The articles stay within their size budgets.
func TestCalcHelp_ArticlesStayWithinTheirWordBudgets(t *testing.T) {
	for _, topic := range calcArticles(t, bundledHelp(t)) {
		limit := 1500
		if !topic.IsSectionArticle() {
			limit = 1200
		}
		if n := len(strings.Fields(topic.Content)); n > limit {
			t.Errorf("%s is %d words, over its %d-word budget", topic.Name, n, limit)
		}
	}
}

// (d) The description and the `expr` field description reach the model on
// every request; together they stay under 120 words.
func TestCalcDescription_StaysUnderTheWordBudget(t *testing.T) {
	c := &Calc{}
	if n := len(strings.Fields(c.Description())) + len(strings.Fields(calcExprDescription)); n >= 120 {
		t.Errorf("description + expr field = %d words, want under 120", n)
	}
	if !strings.Contains(string(c.InputSchema()), calcExprDescription[:40]) {
		t.Error("the schema's expr description is not calcExprDescription")
	}
}

// (e) Calc/errors has exactly one entry per error code the evaluator returns.
func TestCalcHelp_EveryErrorCodeHasAnEntry(t *testing.T) {
	topic, ok := bundledHelp(t).Get("Calc/errors")
	if !ok {
		t.Fatal("no Calc/errors article")
	}
	seen := map[string]int{}
	for _, ln := range strings.Split(topic.Content, "\n") {
		if code, ok := strings.CutPrefix(ln, "### "); ok {
			seen[strings.TrimSpace(code)]++
		}
	}
	want := map[string]bool{}
	for _, code := range calc.ErrorCodes() {
		want[code] = true
		if seen[code] != 1 {
			t.Errorf("Calc/errors has %d entries for %q, want 1 (`### %s`)", seen[code], code, code)
		}
	}
	var extra []string
	for code := range seen {
		if !want[code] {
			extra = append(extra, code)
		}
	}
	sort.Strings(extra)
	for _, code := range extra {
		t.Errorf("Calc/errors documents %q, which calc.ErrorCodes() does not return", code)
	}
}

// The Calc tool article names every section, and nothing else as one.
func TestCalcHelp_ToolArticleListsEverySection(t *testing.T) {
	set := bundledHelp(t)
	art, _ := set.ToolArticle("Calc")
	for _, s := range set.SectionsOf("Calc") {
		if !strings.Contains(art.Content, fmt.Sprintf("- `%s` — ", s.Name)) {
			t.Errorf("the Calc article does not list section %s", s.Name)
		}
	}
}

// Through the dispatcher with the help loaded, the closed schema refuses an
// argument Calc does not take before the tool runs, and a failed call's help
// pointer names Calc's own article.
func TestCalc_DispatcherRefusesUnknownArgumentsAndPointsAtCalcHelp(t *testing.T) {
	set := bundledHelp(t)
	d := tools.NewDispatcher([]tools.Tool{&Calc{}, &Context{Help: set}})
	res := d.Execute(context.Background(), "Calc", json.RawMessage(`{"expr":"2+2","precision":4}`))
	if !res.IsError || !strings.Contains(res.Text, `unknown argument "precision"`) {
		t.Fatalf("unknown argument: %+v", res)
	}
	res = d.Execute(context.Background(), "Calc", json.RawMessage(`{"expr":"2x"}`))
	if !res.IsError || res.Error == nil || res.Error.CallFormat == nil || res.Error.CallFormat.Tool != "Calc" {
		t.Fatalf("failed call carries no Calc call format: %+v", res.Error)
	}
	cf := res.Error.CallFormat
	if cf.Reference == nil || !strings.Contains(string(cf.Reference.Input), `"topic":"Calc"`) {
		t.Errorf("help reference = %+v, want op=help topic=Calc", cf.Reference)
	}
	if !strings.Contains(string(cf.Example), `"expr"`) {
		t.Errorf("example = %s, want a Calc call", cf.Example)
	}
	res = d.Execute(context.Background(), "Calc", json.RawMessage(`{"expr":"2+2"}`))
	if res.IsError || !strings.Contains(res.Text, `"text":"4"`) {
		t.Errorf("2+2 through the dispatcher: %+v", res)
	}
}
