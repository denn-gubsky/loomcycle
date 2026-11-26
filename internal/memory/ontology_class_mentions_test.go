package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestRewriteClassMentions_RewritesWhatTheReaderTakesAsTheClass: every mention a reader
// takes as the class is rewritten, in each spelling the readers accept, and what the
// reader then parses names the new class in the same place; prose, examples, other
// directives and a concept field's scheme keep their text.
func TestRewriteClassMentions_RewritesWhatTheReaderTakesAsTheClass(t *testing.T) {
	from := []string{"Bulletin", "Notice"}
	cases := []struct {
		kind, body, want string
		n                int
	}{
		{MentionKindInstance, "Head office.\n@a bulletin\n@alt Bulletin", "Head office.\n@a Circular\n@alt Bulletin", 1},
		{MentionKindInstance, "- `@a`: Bulletin — the class", "- `@a`: Circular — the class", 1},
		{MentionKindScheme, "@projects Memo, Bulletin ,notice\n@domain Bulletin", "@projects Memo, Circular ,Circular\n@domain Bulletin", 2},
		{MentionKindRelation, "@domain Person | Bulletin\n@range bulletin, Memo\n@inverse Bulletin", "@domain Person | Circular\n@range Circular, Memo\n@inverse Bulletin", 2},
		{MentionKindRelation, "  - `@range` = Bulletin", "  - `@range` = Circular", 1},
		{MentionKindClass, "- `issuer` ref( Bulletin ) @required\n- `rank` concept(Bulletin)\n- `note` Bulletin text", "- `issuer` ref( Circular ) @required\n- `rank` concept(Bulletin)\n- `note` Bulletin text", 1},
		// Examples are not declarations: a fenced or indented bare line is left.
		{MentionKindInstance, "```\n@a Bulletin\n```\n    @a Bulletin\n\t@a Bulletin", "```\n@a Bulletin\n```\n    @a Bulletin\n\t@a Bulletin", 0},
		// The minimal pair: a class not renamed, and a directive of another kind's section.
		{MentionKindInstance, "@a Memo", "@a Memo", 0},
		{MentionKindRelation, "@projects Bulletin", "@projects Bulletin", 0},
	}
	for _, c := range cases {
		got, n := RewriteClassMentions(c.kind, c.body, from, "Circular")
		if got != c.want || n != c.n {
			t.Errorf("%s %q:\n got %q (%d)\nwant %q (%d)", c.kind, c.body, got, n, c.want, c.n)
		}
		if _, again := RewriteClassMentions(c.kind, got, from, "Circular"); again != 0 {
			t.Errorf("%s %q still mentions the class after the rewrite: %q", c.kind, c.body, got)
		}
	}
}

// TestRewriteClassMentions_ReadersSeeTheNewName: what each reader parses from the
// rewritten body is what it parsed before, with the old names replaced.
func TestRewriteClassMentions_ReadersSeeTheNewName(t *testing.T) {
	from := []string{"Bulletin"}
	swap := func(names []string) []string {
		out := make([]string, len(names))
		for i, n := range names {
			if strings.EqualFold(n, "Bulletin") {
				n = "Circular"
			}
			out[i] = n
		}
		return out
	}
	rel := "@domain Bulletin|Person\n@range Bulletin"
	after, _ := RewriteClassMentions(MentionKindRelation, rel, from, "Circular")
	b, a := ParseRelationBody("issued_by", rel), ParseRelationBody("issued_by", after)
	if !reflect.DeepEqual(swap(b.Domain), a.Domain) || !reflect.DeepEqual(swap(b.Range), a.Range) {
		t.Errorf("relation: %v %v → %v %v", b.Domain, b.Range, a.Domain, a.Range)
	}
	sch := "@projects Bulletin, Memo"
	after, _ = RewriteClassMentions(MentionKindScheme, sch, from, "Circular")
	if b, a := ParseSchemeBody("topics", sch), ParseSchemeBody("topics", after); !reflect.DeepEqual(swap(b.Projects), a.Projects) {
		t.Errorf("scheme: %v → %v", b.Projects, a.Projects)
	}
	inst := "- `@a` Bulletin"
	after, _ = RewriteClassMentions(MentionKindInstance, inst, from, "Circular")
	if a := ParseInstanceBody("Head office", after); a.Class != "Circular" {
		t.Errorf("instance class = %q", a.Class)
	}
	cls := "- `issuer` ref(Bulletin)"
	after, _ = RewriteClassMentions(MentionKindClass, cls, from, "Circular")
	if specs := ParseOntologyFieldSpecs(after); len(specs) != 1 || specs[0].Range != "Circular" || specs[0].Type != FieldRef {
		t.Errorf("field = %+v", specs)
	}
}
