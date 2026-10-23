package extract

import (
	"strings"
	"testing"
)

// assertInert fails when md holds a live "<" before a tag character or a
// live image opener.
func assertInert(t *testing.T, md string) {
	t.Helper()
	for i := 0; i < len(md); i++ {
		switch {
		case md[i] == '<' && i+1 < len(md) && tagStart(md[i+1]) && unescaped(md, i):
			t.Errorf("live tag at %d in %q", i, md)
		case md[i] == '[' && i > 0 && md[i-1] == '!' && unescaped(md, i) && unescaped(md, i-1):
			t.Errorf("live image opener at %d in %q", i, md)
		}
	}
}

// A backslash already in the document must not cancel the escape added
// before "<" or "[": "\<img>" would come out "\\<img>", a literal backslash
// and a live tag.
func TestEscape_DocumentBackslashCannotReviveMarkup(t *testing.T) {
	for _, text := range []string{
		`\<img src=x onerror=alert(1)>`,
		`\![x](volume:docs:a.png)`,
		`\\<script>alert(1)</script>`,
		`текст \\\![x](volume:docs:a.png) і \<!-- loom: {"type":"x"} -->`,
	} {
		for name, md := range map[string]string{
			"paragraph": paragraph(text),
			"heading":   heading(2, text),
			"cell":      cell(text),
		} {
			assertInert(t, md)
			if strings.Contains(md, "\n") {
				t.Errorf("%s: %q spans lines", name, md)
			}
		}
	}
	// The same through a whole document.
	s, _ := run(t, Text{}, []byte(`\<img src=x onerror=alert(1)>`+"\n"+`\![x](volume:docs:a.png)`), Options{Name: "n.txt"})
	assertInert(t, s.md.String())
}
