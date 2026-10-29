package memory

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParseConceptBody_ReadsTheConceptDirectives.
func TestParseConceptBody_ReadsTheConceptDirectives(t *testing.T) {
	c := ParseConceptBody("file-plan", "Regulatory Reporting", []string{"Compliance Management", "Regulatory Reporting"},
		"Returns filed with the regulator.\n@notation RR\n@alt звітність\n- `@alt` reporting\n@exact_match topics/reporting\n@retention 5y\n@acl compliance\n")
	want := OntologyConcept{
		Scheme: "file-plan", Name: "Regulatory Reporting",
		Path: []string{"Compliance Management", "Regulatory Reporting"}, Parent: "Compliance Management",
		Notation: "RR", Alt: []string{"звітність", "reporting"}, ExactMatch: "topics/reporting",
		Retention: "5y", ACL: "compliance",
		Tag:     "file-plan/compliance-management/regulatory-reporting",
		Meaning: "Returns filed with the regulator.",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("parsed:\n got %#v\nwant %#v", c, want)
	}
}

// TestParseConceptBody_BadDeclarationsAreReportedNotGuessed.
func TestParseConceptBody_BadDeclarationsAreReportedNotGuessed(t *testing.T) {
	c := ParseConceptBody("file-plan", "x", []string{"x"}, "@retention forever\n@identity\n@bogus\n- `title` text\n")
	if c.Retention != "" {
		t.Errorf("an unusable retention must not be kept: %q", c.Retention)
	}
	joined := strings.Join(c.Issues, "\n")
	for _, want := range []string{"@retention", "@identity is not a concept directive", "unknown directive @bogus", "carries no fields"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing issue %q in:\n%s", want, joined)
		}
	}
	s := ParseSchemeBody("topics", "@ordered\n@render\n@inverse x\n")
	if !s.Ordered || !s.Render || len(s.Issues) != 1 {
		t.Errorf("scheme = %#v", s)
	}
}

// TestMirrorTag_OneNormalisationForEveryReader.
//
// The tag a concept is mirrored onto and the tag a document is classified with must be
// the same string, so the normalisation is pinned: lowercase, words joined by '-', and
// a '/' inside a title never fabricates a level of nesting.
func TestMirrorTag_OneNormalisationForEveryReader(t *testing.T) {
	cases := map[string]struct {
		scheme string
		path   []string
	}{
		"topics/risk-management/outsourcing": {"topics", []string{"Risk Management", "outsourcing"}},
		"file-plan/a-b-c":                    {"File Plan", []string{"A / B  c"}},
		"rank/закон":                         {"rank", []string{"Закон"}},
	}
	for want, c := range cases {
		if got := MirrorTag(c.scheme, c.path); got != want {
			t.Errorf("MirrorTag(%q, %v) = %q, want %q", c.scheme, c.path, got, want)
		}
	}
}

// TestParseRetention_Units.
func TestParseRetention_Units(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{"90d": 90 * day, "6w": 42 * day, "18mo": 540 * day, "10y": 3650 * day, "72h": 72 * time.Hour} {
		if got, ok := ParseRetention(in); !ok || got != want {
			t.Errorf("ParseRetention(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "0d", "-1y", "forever", "10m3", "y"} {
		if _, ok := ParseRetention(bad); ok {
			t.Errorf("ParseRetention(%q) should fail", bad)
		}
	}
}

// TestRenderSchemes_OnlyRenderMarkedSchemes.
func TestRenderSchemes_OnlyRenderMarkedSchemes(t *testing.T) {
	topics := OntologyScheme{Name: "topics", Render: true, Concepts: []OntologyConcept{
		{Name: "risk-management", Path: []string{"risk-management"}, Notation: "RM"},
		{Name: "outsourcing", Path: []string{"risk-management", "outsourcing"}, Alt: []string{"аутсорсинг"}},
	}}
	plan := OntologyScheme{Name: "file-plan", Concepts: []OntologyConcept{{Name: "x", Path: []string{"x"}}}}
	if RenderSchemes([]OntologyScheme{plan}) != "" || RenderSchemes(nil) != "" {
		t.Error("a scheme not marked @render must not reach the prompt")
	}
	got := RenderSchemes([]OntologyScheme{topics, plan})
	for _, want := range []string{"# Concept schemes", "**topics**", "- risk-management (RM)", "  - outsourcing — also: аутсорсинг"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "file-plan") {
		t.Errorf("an unrendered scheme leaked:\n%s", got)
	}
}
