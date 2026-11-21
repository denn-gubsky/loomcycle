package ocrsplice

import (
	"strings"
	"testing"
)

// A transcript replaces its page between the offsets, the other pages stay,
// the header sits before the first page and the transcript's headings go
// two levels down.
func TestSplice_ReplacesOnlyTheTranscribedPages(t *testing.T) {
	md := "# Doc\n\n## Page 1\n\ntyped\n\n## Page 2\n\n"
	starts := map[int]int{1: strings.Index(md, "\n\n## Page 1"), 2: strings.Index(md, "\n\n## Page 2")}
	got := Splice(md, starts, map[int]string{2: "# Scan\n\nread"}, "<!-- h -->")
	want := "# Doc\n\n<!-- h -->\n\n## Page 1\n\ntyped\n\n## Page 2\n\n### Scan\n\nread"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// Offsets that run backwards or past the end (a worker's answer is not
// trusted) splice without a panic.
func TestSplice_DisorderedOffsetsDoNotPanic(t *testing.T) {
	md := "# Doc\n\nabc"
	for _, starts := range []map[int]int{{1: 9, 2: 3}, {1: -4, 2: 1 << 30}} {
		_ = Splice(md, starts, map[int]string{1: "x", 2: "y"}, "<!-- h -->")
	}
}

func TestImageStarts_IsAfterTheTitle(t *testing.T) {
	if got := ImageStarts("# a.png\n\n![](x)"); got[1] != 7 {
		t.Errorf("got %v", got)
	}
	if ImageStarts("# a.png") != nil {
		t.Error("a title alone has a page start")
	}
}
