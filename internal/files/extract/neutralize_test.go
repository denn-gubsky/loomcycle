package extract

import (
	"strings"
	"testing"
)

// A Markdown file is someone else's text: passed through, it must not carry
// an image reference into the reader's volumes, raw HTML, or loom metadata
// that import_md would act on. Code stays exactly as written.
func TestExtractText_MarkdownIsNeutralised(t *testing.T) {
	md := strings.Join([]string{
		"# Звіт",
		`<!-- loom: {"id":"x","type":"decision","status":"confirmed"} -->`,
		"Текст.",
		"",
		"![секрет](volume:docs:secret.png)",
		"",
		"![ок](data:image/png;base64,iVBORw0KGgo=) and ![also](<volume:inbox:a.png>) and \\![escaped](volume:x)",
		"<script>alert(1)</script>",
		"<div onclick=x>raw</div>",
		"```",
		"<b>code</b> ![c](volume:docs:c.png)",
		"```",
		"<!-- loom-edges:",
		"a -> b [rel]",
		"-->",
	}, "\n")
	s, _ := run(t, Text{}, []byte(md), Options{Name: "report.md"})
	got := s.md.String()

	lines := strings.Split(got, "\n")
	fenceAt := -1
	for i, l := range lines {
		if l == "```" {
			fenceAt = i
			break
		}
	}
	if fenceAt < 0 || lines[fenceAt+1] != "<b>code</b> ![c](volume:docs:c.png)" {
		t.Errorf("the fenced code changed:\n%s", got)
	}
	outside := strings.Join(append(append([]string{}, lines[:fenceAt]...), lines[fenceAt+3:]...), "\n")
	assertNoLiveTag(t, outside)
	assertNoLiveImage(t, outside)
	for _, want := range []string{"[секрет](volume:docs:secret.png)", "[ок](data:image/png;base64,iVBORw0KGgo=)", "Текст."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "<!-- loom") {
			t.Errorf("a loom comment survived: %q", l)
		}
	}
}

// An image's alt text may span lines, so a check per line misses it: no
// live "![" may be left at all.
func TestNeutralize_MultiLineAltTextIsDefused(t *testing.T) {
	for _, md := range []string{
		"# D\n\n![a\nb](volume:docs:secret.png)\n",
		"# D\n\n![\n](volume:docs:secret.png)",
		"# D\n\nтекст ![a\nb\nc](<volume:docs:secret.png>) далі",
	} {
		assertNoLiveImage(t, Neutralize(md, true))
	}
}

func assertNoLiveImage(t *testing.T, md string) {
	t.Helper()
	for i := 1; i < len(md); i++ {
		if md[i] == '[' && md[i-1] == '!' && unescaped(md, i) && unescaped(md, i-1) {
			t.Errorf("live image opener at %d: %q", i, md[max(0, i-10):min(len(md), i+30)])
		}
	}
}

func assertNoLiveTag(t *testing.T, md string) {
	t.Helper()
	for i := 0; i < len(md); i++ {
		if md[i] == '<' && i+1 < len(md) && tagStart(md[i+1]) && unescaped(md, i) {
			t.Errorf("live tag at %d: %q", i, md[max(0, i-10):min(len(md), i+20)])
		}
	}
}
