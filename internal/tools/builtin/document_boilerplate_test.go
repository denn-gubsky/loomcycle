package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// document_boilerplate_test.go — RFC FP Phase C: get_chunk names the paragraphs a
// document class marks as boilerplate, so a linker running outside Go never
// evaluates the RE2 pattern.

// boilerplateOntologyMD: Act declares two kinds of boilerplate; Order inherits them;
// Memo declares none.
const boilerplateOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Act\n@boilerplate (?i)^набирає чинності\n@boilerplate ^Доповнити\n\n" +
	"##### Order\n\n" +
	"#### Memo\n"

// boilerplateBody opens in Cyrillic, so rune and byte offsets differ, and carries
// two boilerplate paragraphs (one indented, one in mixed case) around a real one.
const boilerplateBody = "Банк веде реєстр інцидентів.\n\n" +
	"  НАБИРАЄ ЧИННОСТІ з дня опублікування.\n\n" +
	"Реєстр зберігається п'ять років.\n" +
	"Доповнити не можна.\n\n" +
	"Доповнити пункт 3 словами «щороку».\n"

func boilerplateChunk(t *testing.T, d *Document, ctx context.Context, docType string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":"Акт","type":%q}`, docType))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	body, _ := json.Marshal(boilerplateBody)
	c, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"1","body":%s}`,
		out["document_id"], body))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	return c["id"].(string)
}

// runeSpanOf is the rune span of the first occurrence of s in body.
func runeSpanOf(body, s string) [2]float64 {
	at := strings.Index(body, s)
	start := utf8.RuneCountInString(body[:at])
	return [2]float64{float64(start), float64(start + utf8.RuneCountInString(s))}
}

// TestDocumentBoilerplate_GetChunkNamesTheMatchingParagraphs: in an Order (which
// inherits Act's two patterns), get_chunk returns the rune spans of exactly the
// paragraphs a pattern matches — not the paragraph that only contains the word
// later on, and without the indentation or line breaks around a paragraph.
func TestDocumentBoilerplate_GetChunkNamesTheMatchingParagraphs(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, boilerplateOntologyMD, memrank.OntologyConfirmedStatus)
	id := boilerplateChunk(t, d, ctx, "Order")

	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, id))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	raw, _ := out["boilerplate"].([]any)
	var got [][2]float64
	for _, s := range raw {
		p := s.([]any)
		got = append(got, [2]float64{p[0].(float64), p[1].(float64)})
	}
	want := [][2]float64{
		runeSpanOf(boilerplateBody, "НАБИРАЄ ЧИННОСТІ з дня опублікування."),
		runeSpanOf(boilerplateBody, "Доповнити пункт 3 словами «щороку»."),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("boilerplate = %v, want %v", got, want)
	}
}

// TestDocumentBoilerplate_OnlyAConfirmedClassWithTheDirectiveGoverns: a class that
// declares none, and any class under a draft ontology, carry no boilerplate key.
func TestDocumentBoilerplate_OnlyAConfirmedClassWithTheDirectiveGoverns(t *testing.T) {
	for _, c := range []struct {
		name, status, class string
	}{
		{"memo, confirmed", memrank.OntologyConfirmedStatus, "Memo"},
		{"order, draft", memrank.OntologyDraftStatus, "Order"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			ctx := installTenantOntology(t, d, base, boilerplateOntologyMD, c.status)
			id := boilerplateChunk(t, d, ctx, c.class)
			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, id))
			if strings.Contains(r.Text, `"boilerplate"`) {
				t.Errorf("get_chunk carries boilerplate: %s", r.Text)
			}
		})
	}
}

// TestDocumentBoilerplate_DescribeOntologyShowsItInherited: describe_ontology lists the
// directive on the class that declares it and on the subclass that inherits it, with
// no issue raised.
func TestDocumentBoilerplate_DescribeOntologyShowsItInherited(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, boilerplateOntologyMD, memrank.OntologyConfirmedStatus)
	out, r := docExec(t, d, ctx, `{"op":"describe_ontology","scope":"user"}`)
	if r.IsError {
		t.Fatalf("describe_ontology: %s", r.Text)
	}
	want := `(?:(?i)^набирає чинності)|(?:^Доповнити)`
	for _, raw := range out["classes"].([]any) {
		c := raw.(map[string]any)
		dirs, _ := c["directives"].(map[string]any)
		switch c["name"] {
		case "Act", "Order":
			if dirs["boilerplate"] != want {
				t.Errorf("%s boilerplate = %v, want %q", c["name"], dirs["boilerplate"], want)
			}
			if iss, _ := c["issues"].([]any); len(iss) > 0 {
				t.Errorf("%s issues = %v, want none", c["name"], iss)
			}
		case "Memo":
			if _, ok := dirs["boilerplate"]; ok {
				t.Errorf("Memo carries boilerplate %v", dirs["boilerplate"])
			}
		}
	}
}

// TestParagraphSpans_MatchesAReferenceOnAwkwardBodies: the one-pass walk gives the
// spans a slow, obvious implementation gives — CRLF line ends, trailing and leading
// spaces, blank lines of spaces, astral characters, no final newline.
func TestParagraphSpans_MatchesAReferenceOnAwkwardBodies(t *testing.T) {
	re := regexp.MustCompile(`(?i)^(x|𝔵)`)
	reference := func(body string) [][2]int {
		var out [][2]int
		lines := strings.Split(body, "\n")
		at := 0
		start, end := -1, -1
		flush := func() {
			if start >= 0 && re.MatchString(body[start:end]) {
				out = append(out, [2]int{utf8.RuneCountInString(body[:start]), utf8.RuneCountInString(body[:end])})
			}
			start, end = -1, -1
		}
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				flush()
			} else {
				if start < 0 {
					start = at + len(l) - len(strings.TrimLeft(l, " \t\r"))
				}
				end = at + len(strings.TrimRight(l, " \t\r"))
			}
			at += len(l) + 1
		}
		flush()
		return out
	}
	for _, body := range []string{
		"x one\r\nstill one\r\n\r\ny two  \n   \nX three",
		"  𝔵 astral first\n\nа кирилиця\n𝔵 not a start\n\n𝔵 last",
		"\n\n\nx\n",
		"no match here\n\nnor here",
		"",
	} {
		got, want := paragraphSpans(body, re), reference(body)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("paragraphSpans(%q) = %v, want %v", body, got, want)
		}
	}
}
