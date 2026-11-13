package promptinject

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPlaceholderFamilies_ExactlySet pins the set of {{<keyword>…}} families
// the single-pass expander recognises. Each family is a reach a system prompt
// gains at every run start, spawn and resume, so adding one must fail this
// test and be argued for, as widening the tool allowlist must.
//
// The families are read from placeholderFamilyPatterns, the list the combined
// regexes are built from, so a family added to the expander cannot escape the
// pin: each pattern must open with `{{` and one keyword, and the set of
// keywords must be exactly the one below. The ${var.…} family is apart: it is
// joined only into combinedWithVarsRe, and only when a caller supplies values.
func TestPlaceholderFamilies_ExactlySet(t *testing.T) {
	keyword := regexp.MustCompile(`^\(\\\\\?\)\\\{\\\{\\s\*([a-z]+)`)
	set := map[string]bool{}
	for i, p := range placeholderFamilyPatterns {
		m := keyword.FindStringSubmatch(p)
		if m == nil {
			t.Fatalf("family pattern %d does not open with an escapable {{<keyword>: %s", i, p)
		}
		set[m[1]] = true
	}
	var got []string
	for k := range set {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"document", "help", "identity", "memory", "tool"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placeholder families changed: got %v, want %v", got, want)
	}
	if want := "(?i)" + strings.Join(placeholderFamilyPatterns, "|"); combinedPlaceholderRe.String() != want {
		t.Fatalf("combinedPlaceholderRe is not built from placeholderFamilyPatterns alone")
	}
	if want := "(?i)" + strings.Join(placeholderFamilyPatterns, "|") + "|" + varPlaceholderPattern; combinedWithVarsRe.String() != want {
		t.Fatalf("combinedWithVarsRe is not placeholderFamilyPatterns plus the variable family")
	}
}

const searchCard = `Document {"op":"search","query":"q"}
Document {"op":"search","query":"q","scope":"tenant","limit":5}`

func TestExpand_HelpRendersTheExamplesInPlace(t *testing.T) {
	prompt := "Calls:\n{{help:Document/search}}\nDone."
	got := Expand(prompt, ExpandInput{HelpExamples: map[string]string{"Document/search": searchCard}})
	want := "Calls:\n" + searchCard + "\nDone."
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Unframed: the samples are meant to be copied, not held at arm's length.
	if strings.Contains(got, "<tool-result") || strings.Contains(got, "<memory") {
		t.Fatalf("help examples were framed as data:\n%s", got)
	}
}

func TestExpand_HelpEscapedPlaceholderStaysLiteral(t *testing.T) {
	got := Expand(`see \{{help:Document/search}}`, ExpandInput{HelpExamples: map[string]string{"Document/search": searchCard}})
	if got != "see {{help:Document/search}}" {
		t.Fatalf("got %q", got)
	}
}

func TestExpand_HelpMissingBodyRendersNothing(t *testing.T) {
	if got := Expand("a{{help:Nope/none}}b", ExpandInput{}); got != "ab" {
		t.Fatalf("got %q, want the placeholder removed", got)
	}
	// A malformed one that got past validation is removed the same way.
	if got := Expand("a{{help:a/b/c}}b", ExpandInput{}); got != "ab" {
		t.Fatalf("got %q, want the malformed placeholder removed", got)
	}
}

// TestExpand_PlaceholderInsideHelpBodyIsNotExpanded: an article's example may
// contain placeholder syntax (an AgentDef example carrying a system prompt
// would). The single pass never rescans what it substituted, so every family
// — {{memory:…}}, {{tool:…}}, {{help:…}} itself, ${var.…} — stays literal.
func TestExpand_PlaceholderInsideHelpBodyIsNotExpanded(t *testing.T) {
	body := `AgentDef {"op":"create","overlay":{"system_prompt":"{{memory:core_blocks}} {{tool:Context.tools}} {{help:Document/search}} ${var.pr}"}}`
	in := ExpandInput{
		HelpExamples: map[string]string{"AgentDef/create": body, "Document/search": searchCard},
		Sections:     map[Variant]string{VariantCoreBlocks: "SECRET-CORE"},
		ToolResults:  map[ToolRef]string{contextTools: "TOOL-INVENTORY"},
		Values:       map[string]string{"var.pr": "VALUE"},
	}
	got := Expand("{{help:AgentDef/create}}", in)
	if !strings.HasPrefix(got, body) {
		t.Fatalf("body was rewritten:\n%s", got)
	}
	for _, leak := range []string{"SECRET-CORE", "TOOL-INVENTORY", "VALUE", `"query":"q"`} {
		// SECRET-CORE may appear in the implicit core_blocks append below the
		// body, never inside it.
		if strings.Contains(strings.SplitN(got, "\n", 2)[0], leak) {
			t.Errorf("placeholder inside a help body expanded to %q:\n%s", leak, got)
		}
	}
}

func TestHelpTopics_MalformedAndEscaped(t *testing.T) {
	s := `{{help:Document/search}} {{ HELP : scopes }} \{{help:Memory/recall}} ` +
		`{{help:}} {{help:Document search}} {{help:Document.search}} {{help:${var.t}}} {{help:../x}} {{help:a/b/c}} {{help:Document/search}}`
	if got, want := HelpTopics(s), []string{"Document/search", "scopes", "Document/search", "Document/search", "Document/search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("HelpTopics = %v, want %v", got, want)
	}
	if got, want := ReferencesHelpTopics(s), []string{"Document/search", "scopes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReferencesHelpTopics = %v, want %v", got, want)
	}
	if got, want := MalformedHelpRefs(s), []string{"", "${var.t", "../x", "a/b/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MalformedHelpRefs = %v, want %v", got, want)
	}
}

// TestHelpPlaceholder_NeverSwallowsAnotherPlaceholder: a help opener with no
// close of its own must not match up to the close of the NEXT placeholder.
// With a topic pattern that admitted `{`, each of these matched as one help
// placeholder: the inner {{identity}} / {{tool:…}} / {{memory:…}} vanished
// with it at render, and validation saw only a "malformed help topic". Now the
// inner placeholder expands as its own family and validation names the
// unclosed help opener.
func TestHelpPlaceholder_NeverSwallowsAnotherPlaceholder(t *testing.T) {
	in := ExpandInput{
		Identity:     &IdentityFacts{DisplayName: "Alice"},
		ToolResults:  map[ToolRef]string{contextTools: "TOOL-INVENTORY"},
		Sections:     map[Variant]string{VariantUserInfo: "PROFILE"},
		HelpExamples: map[string]string{"Document/search": searchCard},
	}
	for prompt, want := range map[string]string{
		"{{help:{{identity}}":           "Alice",
		"{{help:{{tool:Context.tools}}": "TOOL-INVENTORY",
		"{{help:{{memory:user_info}}":   "PROFILE",
	} {
		got := Expand(prompt, in)
		if !strings.Contains(got, want) {
			t.Errorf("%s: the inner placeholder was swallowed; got %q", prompt, got)
		}
		// The unclosed opener is literal text, not part of another match: a
		// help match that reached into {{identity}} was dispatched to the
		// identity family and ate the opener with it.
		if !strings.HasPrefix(got, "{{help:") {
			t.Errorf("%s: the help opener was consumed with the inner placeholder; got %q", prompt, got)
		}
		if len(HelpTopics(prompt)) != 0 {
			t.Errorf("%s: HelpTopics = %v, want none", prompt, HelpTopics(prompt))
		}
		if bad := MalformedHelpRefs(prompt); len(bad) != 1 {
			t.Errorf("%s: MalformedHelpRefs = %v, want the unclosed opener named", prompt, bad)
		}
	}
}

// TestExpand_HelpBudgetDropsWholeLinesThenNothing: past HelpMaxBytes the body
// is cut at a line boundary — never inside a call — with a marker, and a later
// help placeholder renders nothing. The memory family is not affected: the
// budgets are separate.
func TestExpand_HelpBudgetDropsWholeLinesThenNothing(t *testing.T) {
	line := `Document {"op":"search","query":"` + strings.Repeat("x", 989) + `"}` // 1024 bytes
	if len(line) != 1024 {
		t.Fatalf("line is %d bytes", len(line))
	}
	big := strings.TrimSuffix(strings.Repeat(line+"\n", 12), "\n") // 12 calls, > 8 KiB
	in := ExpandInput{
		HelpExamples: map[string]string{"Big/one": big, "Small/two": "Path {\"op\":\"ls\"}"},
		Sections:     map[Variant]string{VariantUserInfo: "PROFILE"},
	}
	got := Expand("{{help:Big/one}}\n--\n{{help:Small/two}}\n{{memory:user_info}}", in)
	first, rest, _ := strings.Cut(got, "\n--\n")
	lines := strings.Split(first, "\n")
	if lines[len(lines)-1] != helpTruncMarker {
		t.Fatalf("no truncation marker:\n%s", first)
	}
	calls := lines[:len(lines)-1]
	// 1024 + 6*1025 = 7174 fits 8192; a seventh joined line (8199) does not.
	if len(calls) != 7 {
		t.Errorf("kept %d calls, want 7 whole ones", len(calls))
	}
	for i, c := range calls {
		if c != line {
			t.Errorf("call %d was cut: %d bytes", i, len(c))
		}
	}
	if strings.Contains(rest, "Path {") {
		t.Errorf("a help placeholder after the spent budget rendered:\n%s", rest)
	}
	if !strings.Contains(rest, "PROFILE") {
		t.Errorf("the memory family lost its section to the help budget:\n%s", rest)
	}
}

func TestExpand_HelpWithinBudgetIsWhole(t *testing.T) {
	body := strings.TrimSuffix(strings.Repeat(`Path {"op":"ls"}`+"\n", 10), "\n")
	got := Expand("{{help:Path/ls}}", ExpandInput{HelpExamples: map[string]string{"Path/ls": body}})
	if got != body {
		t.Fatalf("a body under the budget changed:\n%s", got)
	}
}
