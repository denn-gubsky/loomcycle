package memory

import (
	"strings"
	"testing"
)

// TestParseEntryBodies_LockedIsAFlagOnEveryEntryKind (RFC FO 3.1a): @locked, in either
// spelling, marks a class, relation kind, scheme, concept and instance as locked, with
// no issue — and on an instance it is not read as a statement of a kind named "locked".
func TestParseEntryBodies_LockedIsAFlagOnEveryEntryKind(t *testing.T) {
	for _, body := range []string{"@locked\nWhat it is.", "- `@locked` — legal hold\nWhat it is."} {
		c := ParseClassBody(body)
		r := ParseRelationBody("supervises", body)
		s := ParseSchemeBody("topics", body)
		k := ParseConceptBody("topics", "Risk", []string{"Risk"}, body)
		in := ParseInstanceBody("Acme", "@a organization\n"+body)
		if !c.Locked || !r.Locked || !s.Locked || !k.Locked || !in.Locked {
			t.Errorf("%q: locked class=%v relation=%v scheme=%v concept=%v instance=%v", body, c.Locked, r.Locked, s.Locked, k.Locked, in.Locked)
		}
		if issues := strings.Join(append(append(append(append(append([]string{}, c.Issues...), r.Issues...), s.Issues...), k.Issues...), in.Issues...), "; "); issues != "" {
			t.Errorf("%q: issues %q, want none", body, issues)
		}
		if len(in.Relations) != 0 {
			t.Errorf("%q: the instance read @locked as a relation: %+v", body, in.Relations)
		}
	}
}

// TestParseEntryBodies_LockedTakesNoValue: a value is refused rather than read, so
// `@locked false` never locks, and is reported.
func TestParseEntryBodies_LockedTakesNoValue(t *testing.T) {
	c := ParseClassBody("@locked false")
	k := ParseConceptBody("topics", "Risk", []string{"Risk"}, "@locked no")
	if c.Locked || k.Locked {
		t.Errorf("a valued @locked locked: class=%v concept=%v", c.Locked, k.Locked)
	}
	for _, issues := range [][]string{c.Issues, k.Issues} {
		if !strings.Contains(strings.Join(issues, "; "), "@locked takes no value") {
			t.Errorf("issues %q do not report the value", issues)
		}
	}
}

// TestDeclaresLocked_SeesEitherSpellingAndNotAQuotedLine: the check a proposal body is
// held to reads the line as the reader would, so a line propose_entity quoted is not
// a lock.
func TestDeclaresLocked_SeesEitherSpellingAndNotAQuotedLine(t *testing.T) {
	for body, want := range map[string]bool{
		"@locked": true, "- `@locked`": true, "text\n@locked\nmore": true,
		"> @locked": false, "the word @locked in prose": false, "": false,
	} {
		if got := DeclaresLocked(body); got != want {
			t.Errorf("DeclaresLocked(%q) = %v, want %v", body, got, want)
		}
	}
}

// TestBodyLocked_ReadsTheFlagAsTheParserDoes: a valued @locked is no lock, as the entry
// parsers read it; the plan judges an edited body with this.
func TestBodyLocked_ReadsTheFlagAsTheParserDoes(t *testing.T) {
	for body, want := range map[string]bool{
		"@locked": true, "- `@locked` — legal hold": true, "@locked false": false, "> @locked": false, "x": false,
	} {
		if got := BodyLocked(body); got != want {
			t.Errorf("BodyLocked(%q) = %v, want %v", body, got, want)
		}
	}
}
