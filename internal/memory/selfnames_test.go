package memory

import (
	"strings"
	"testing"
)

// TestSelfNames_TheShippedTemplateNamesNobody is the test this feature lives or dies by.
//
// The template teaches the form by SHOWING it, and a parser that read its own example
// would claim every unedited profile belongs to "Ada Lovelace" — then refuse to place any
// fact about a real person of that name, and, far worse, quietly convince a reader that
// the identity guard was working when it was matching a sample.
//
// The defence is that the example is indented, so it is a code sample, and the parser
// requires a bullet at column 0.
func TestSelfNames_TheShippedTemplateNamesNobody(t *testing.T) {
	got := ParseSelfNames(UserRootTemplate())
	if len(got) != 0 {
		t.Fatalf("the unedited template declared %v — an untouched profile must name nobody", got)
	}
	// And the template really does contain the directives, or this would pass because the
	// section is missing rather than because the indent works.
	if !strings.Contains(UserRootTemplate(), SelfNameDirective) {
		t.Errorf("the template no longer shows %s — the test above is now vacuous", SelfNameDirective)
	}
}

func TestSelfNames_ReadsWhatTheUserWrote(t *testing.T) {
	md := strings.Join([]string{
		"# User Profile",
		"",
		"## Identity",
		"",
		"- `@name` Ada Lovelace",
		"- `@alias` Ada",
		"- `@alias` ada-lovelace — the git handle",
		"- `@alias`: Countess Lovelace",
		"",
		"## Role and context",
		"",
		"- `name` not a directive, just a field bullet",
		"",
	}, "\n")
	got := ParseSelfNames(md)
	want := []string{"Ada Lovelace", "Ada", "ada-lovelace", "Countess Lovelace"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestSelfNames_SkipsWhatCannotBeAName(t *testing.T) {
	md := strings.Join([]string{
		"- `@name`",            // no value
		"- `@alias` ",          // whitespace only
		"- `@name` A",          // one character matches too much to be identity
		"- `@unknown` Ada",     // not a directive this reads
		"  - `@name` Indented", // a code sample or a nested bullet, never identity
		"- `@alias` Ada",       // the only real one
		"- `@alias` ada",       // a case variant of the same name
	}, "\n")
	got := ParseSelfNames(md)
	if len(got) != 1 || got[0] != "Ada" {
		t.Errorf("got %v, want exactly [Ada]", got)
	}
}

func TestSelfNames_BoundsWhatOneDocumentCanDeclare(t *testing.T) {
	var b strings.Builder
	for i := 0; i < selfNamesMax*3; i++ {
		b.WriteString("- `@alias` name")
		b.WriteString(strings.Repeat("x", i%5+2))
		b.WriteString("\n")
	}
	if got := ParseSelfNames(b.String()); len(got) > selfNamesMax {
		t.Errorf("declared %d names, over the %d bound", len(got), selfNamesMax)
	}
}

// The names are compared case- and whitespace-insensitively, because the extractor spells
// a subject however the conversation did.
func TestSelfNames_MatchIgnoresCaseAndSurroundingSpace(t *testing.T) {
	names := []string{"Ada Lovelace", "Ada"}
	for _, subject := range []string{"Ada Lovelace", "ada lovelace", "  ADA  ", "the Ada"} {
		if !IsSelfSubject(subject, "u_1", names) {
			t.Errorf("subject %q should match a declared name", subject)
		}
	}
	for _, subject := range []string{"Maria", "Ada Byron", "Lovelace"} {
		if IsSelfSubject(subject, "u_1", names) {
			t.Errorf("subject %q is not a declared name and must not match", subject)
		}
	}
}
