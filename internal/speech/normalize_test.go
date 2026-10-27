package speech

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestNormalize_MarkdownBecomesSpeakableText — one case per construct the
// normaliser handles.
func TestNormalize_MarkdownBecomesSpeakableText(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain paragraph", "Hello world.", "Hello world."},
		{"soft line breaks join", "one\ntwo\r\nthree", "one two three"},
		{"paragraphs keep a break", "One.\n\n\nTwo.", "One.\n\nTwo."},
		{"atx heading becomes a sentence", "# Board summary\nText here.", "Board summary.\n\nText here."},
		{"atx closing hashes dropped", "## Results ##", "Results."},
		{"heading punctuation kept", "### Why now?", "Why now?"},
		{"hash inside heading kept", "# Learn C#", "Learn C#."},
		{"no space is not a heading", "#hashtag", "#hashtag"},
		{"setext h1", "Title\n=====\nBody.", "Title.\n\nBody."},
		{"setext h2", "Sub title\n---", "Sub title."},
		{"emphasis", "This is *very* _really_ important.", "This is very really important."},
		{"strong", "**Bold** and __bold__.", "Bold and bold."},
		{"nested emphasis", "***all*** and **bold _inner_ text**", "all and bold inner text"},
		{"strikethrough", "~~old~~ new", "old new"},
		{"inline code keeps text", "Run `go test` now.", "Run go test now."},
		{"double backtick code", "Use ``a ` b`` here.", "Use a ` b here."},
		{"code keeps its asterisks", "Type `a*b*c` exactly.", "Type a*b*c exactly."},
		{"lone asterisk is text", "2 * 3 = 6", "2 * 3 = 6"},
		{"intraword underscore is text", "call snake_case_name now", "call snake_case_name now"},
		{"escaped marker is text", `a \*literal\* star`, "a *literal* star"},
		{"link keeps text", "See [the docs](https://example.com/docs \"Docs\") today.", "See the docs today."},
		{"link with parens in url", "[wiki](https://en.wikipedia.org/wiki/Go_(language)) page", "wiki page"},
		{"reference link", "Read [the guide][g].\n\n[g]: https://example.com", "Read the guide."},
		{"image keeps alt", "![A cat](cat.png) sleeps.", "A cat sleeps."},
		{"emphasis in link text", "[**bold** link](x)", "bold link"},
		{"footnote ref dropped", "Claim[^1] holds.", "Claim holds."},
		{"bare url dropped", "Visit https://example.com/a_b?x=1 for more.", "Visit for more."},
		{"bare url keeps sentence end", "See https://example.com.", "See."},
		{"www url dropped", "Go to www.example.org now", "Go to now"},
		{"autolink dropped", "Mail <https://x.org/a> or <me@example.com>.", "Mail or."},
		{"html tags stripped", "Line<br>next <b>bold</b>er <span class=\"x\">t</span>", "Line next bolder t"},
		{"html comment dropped", "a <!-- hidden\n\nstill hidden --> b", "a b"},
		{"html entities decoded", "Fish &amp; chips &lt;3", "Fish & chips <3"},
		{"less-than is text", "a < b and 3>2", "a < b and 3>2"},
		{"bullet list", "- one\n- two\n* three!", "one. two. three!"},
		{"ordered list", "1. first\n2) second", "first. second."},
		{"task list", "- [ ] todo\n- [x] done", "todo. done."},
		{"nested list", "- a\n    - b\n- c", "a. b. c."},
		{"list continuation", "- first line\n  continues here", "first line continues here."},
		{"list then paragraph", "- a\n- b\n\nAfter.", "a. b.\n\nAfter."},
		{"lone number line is text", "Year\n\n2026.", "Year\n\n2026."},
		{"fenced code", "Before.\n```go\nfmt.Println(\"*x*\")\n```\nAfter.", "Before.\n\nA code block is skipped.\n\nAfter."},
		{"tilde fence", "~~~\ncode\n~~~", "A code block is skipped."},
		{"unclosed fence runs to end", "Text.\n```\ncode\nmore", "Text.\n\nA code block is skipped."},
		{"longer fence closes only on its length", "````\n```\nstill code\n````\nOut.", "A code block is skipped.\n\nOut."},
		{"indented code", "Para.\n\n    x := 1\n\n    y := 2\nAfter.", "Para.\n\nA code block is skipped.\n\nAfter."},
		{"indented line continues a paragraph", "Para\n    still para", "Para still para"},
		{"table", "Intro.\n\n| a | b |\n|---|:-:|\n| 1 | 2 |\n| 3 | 4 |\n\nOutro.", "Intro.\n\nA table is skipped.\n\nOutro."},
		{"pipe text is not a table", "a | b\nnext", "a | b next"},
		{"blockquote", "> Quoted *text*\n> more\n\nAfter.", "Quoted text more\n\nAfter."},
		{"nested blockquote", ">> deep", "deep"},
		{"quoted code fence", "> ```\n> code\n> ```\n> After.", "A code block is skipped.\n\nAfter."},
		{"thematic break", "One\n\n***\n\nTwo", "One\n\nTwo"},
		{"spaced thematic break", "One\n- - -\nTwo", "One\n\nTwo"},
		{"hard line break", "one\\\ntwo  \nthree", "one two three"},
		{"whitespace collapses", "  a \t  b  c  ", "a b c"},
		{"empty", "", ""},
		{"only markup", "**  **\n\n---", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalize(c.in); got != c.want {
				t.Errorf("Normalize(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalize_UkrainianAndStressMarksPassThrough — letters outside ASCII,
// including the combining acute accent used as a stress mark, are untouched.
func TestNormalize_UkrainianAndStressMarksPassThrough(t *testing.T) {
	in := "# Звіт за жо́втень\n\nПрибуток зріс на **12%**, а ви́трати — ні.\n\n- Ґанок\n- Їжак і є́внух"
	want := "Звіт за жо́втень.\n\nПрибуток зріс на 12%, а ви́трати — ні.\n\nҐанок. Їжак і є́внух."
	got := Normalize(in)
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if strings.Count(got, "́") != 3 {
		t.Errorf("stress marks = %d, want 3 kept", strings.Count(got, "́"))
	}
}

// TestNormalize_NotesCanBeTranslated — a caller passes notes in the text's
// language; a field left empty keeps the English default.
func TestNormalize_NotesCanBeTranslated(t *testing.T) {
	in := "```\nx\n```\n\n| a |\n|---|\n| 1 |"
	got := NormalizeWith(in, Notes{CodeBlock: "Блок коду пропущено."})
	want := "Блок коду пропущено.\n\nA table is skipped."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNormalize_UnclosedLinksStayLinear — text full of unclosed link openers
// finishes quickly (the destination scan is bounded).
func TestNormalize_UnclosedLinksStayLinear(t *testing.T) {
	in := strings.Repeat("[a](b", 50000)
	if got := Normalize(in); strings.Contains(got, "[") {
		t.Errorf("brackets left in %q…", got[:min(len(got), 40)])
	}
}

// FuzzNormalize — never panics, and valid UTF-8 in gives valid UTF-8 out with
// no run of blank lines.
func FuzzNormalize(f *testing.F) {
	for _, s := range []string{
		"# h\n\n*a* _b_ `c` [d](e) ![f](g) <h>i</h> https://j.k",
		"```\ncode\n", "| a |\n|---|", "> > q", "- [x] t\n    - n", "Звіт жо́втень",
		"\\", "`", "[", "](", "<", "~~~", "***", "__", "[^", "<!--",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := Normalize(in)
		if utf8.ValidString(in) && !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out of valid input %q: %q", in, out)
		}
		if strings.Contains(out, "\n\n\n") || strings.HasPrefix(out, " ") || strings.HasSuffix(out, " ") {
			t.Fatalf("badly spaced output %q", out)
		}
	})
}
