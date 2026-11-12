package promptinject

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestPlaceholderFamilies_ExactlySet pins the set of {{<keyword>:…}} families
// the single-pass expander recognises. Each family is a reach a system prompt
// gains at every run start, spawn and resume, so adding one must fail this
// test and be argued for, as widening the tool allowlist must.
func TestPlaceholderFamilies_ExactlySet(t *testing.T) {
	candidates := []string{
		"memory", "tool", "document", "identity", "help",
		"file", "url", "env", "exec", "skill", "agent", "var", "include", "secret",
	}
	var got []string
	for _, kw := range candidates {
		// Each family's simplest accepted shape: a bare keyword ({{identity}})
		// or keyword:argument.
		if combinedPlaceholderRe.MatchString("{{"+kw+"}}") ||
			combinedPlaceholderRe.MatchString("{{"+kw+":core_blocks}}") ||
			combinedPlaceholderRe.MatchString("{{"+kw+":Context.tools}}") ||
			combinedPlaceholderRe.MatchString("{{"+kw+":/a/b}}") ||
			combinedPlaceholderRe.MatchString("{{"+kw+":Document/search}}") {
			got = append(got, kw)
		}
	}
	sort.Strings(got)
	want := []string{"document", "help", "identity", "memory", "tool"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placeholder families changed: got %v, want %v", got, want)
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
	if got := Expand("a{{help:Document search}}b", ExpandInput{}); got != "ab" {
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
		`{{help:}} {{help:Document search}} {{help:Document.search}} {{help:${var.t}}} {{help:../x}} {{help:Document/search}}`
	if got, want := HelpTopics(s), []string{"Document/search", "scopes", "Document/search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("HelpTopics = %v, want %v", got, want)
	}
	if got, want := ReferencesHelpTopics(s), []string{"Document/search", "scopes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReferencesHelpTopics = %v, want %v", got, want)
	}
	if got, want := MalformedHelpRefs(s), []string{"", "Document search", "Document.search", "${var.t", "../x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MalformedHelpRefs = %v, want %v", got, want)
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
